package rdp

import (
	"encoding/binary"
	"io"
)

// clientFilter sits between the IronRDP web client and the RDP server (the client → server direction of the relay)
// and adjusts the connection sequence only:
//
//   - clientBuild of the Client Core Data ([MS-RDPBCGR] 2.2.1.3.2): IronRDP reports its crate version (0.0.0 → build
//     0). xrdp treats every client build ≤ 419 as a pre-XP client that cannot resize and ignores all display control
//     requests, so the remote desktop never follows the tab. The filter reports build 2600 (FreeRDP's default)
//     instead of such values.
//   - INFO_AUTOLOGON of the Client Info PDU (2.2.1.11.1.1): IronRDP never sets it, so servers using TLS security (xrdp,
//     Windows without NLA) ignore the password Termstead handed to the client and show their own logon screen. When the
//     ticket carried a user name and a password, the filter sets the flag, as mstsc does with saved credentials.
//   - the static virtual channels of the Client Network Data (2.2.1.3.4): channels the connection's policy disables
//     (clipboard, printer / drive redirection, sound) are renamed so the server never attaches its clipboard, device
//     redirection or audio service to them — the policy holds even against a modified web client, which only honours
//     it voluntarily.
//
// After the Client Info PDU, or anything it does not expect, the filter forwards everything unchanged.
type clientFilter struct {
	out       io.Writer
	state     int
	pending   []byte
	obuf      []byte
	autologon bool
	blocked   map[string]bool // static channel names to disable
	tpkts     int

	patchedBuild bool
	patchedLogon bool
	disabled     []string // channels renamed
}

const (
	mcsConnectInitial0     = 0x7F // BER application tag 101: 7F 65
	mcsConnectInitial1     = 0x65
	mcsSendDataRequest     = 25
	csCoreType             = 0xC001
	csNetType              = 0xC003
	csCoreMinLength        = 132 // mandatory part of TS_UD_CS_CORE
	channelDefSize         = 12  // CHANNEL_DEF: name[8] + options
	csCoreBuildOffset      = 20  // header, version, desktop size, color depth, SAS sequence, keyboard layout
	minResizableBuild      = 420 // xrdp: builds ≤ 419 "can't resize"
	reportedClientBuild    = 2600
	secInfoPkt             = 0x0040
	secEncrypt             = 0x0008
	infoAutologon          = 0x00000008
	maxConnectSequencePDUs = 32 // TPKTs inspected before giving up on finding the Client Info PDU
)

// channelPolicy lists the static virtual channels a connection's options disable for the IronRDP client.
func channelPolicy(opts rdpOptions) []string {
	var blocked []string
	if opts.DisableClipboard {
		blocked = append(blocked, "cliprdr")
	}
	if !opts.EnablePrinting && !opts.EnableDrive {
		blocked = append(blocked, "rdpdr")
	}
	if !opts.EnableAudio {
		blocked = append(blocked, "rdpsnd")
	}
	return blocked
}

func newClientFilter(out io.Writer, selected uint32, autologon bool, blockedChannels []string) *clientFilter {
	f := &clientFilter{out: out, state: filterRDP, autologon: autologon, blocked: map[string]bool{}}
	for _, c := range blockedChannels {
		f.blocked[c] = true
	}
	if selected&(protoHybrid|protoHybridEx) != 0 {
		f.state = filterCredSSP
	}
	return f
}

// Write consumes client bytes and forwards them (adjusted) to the server.
func (f *clientFilter) Write(p []byte) (int, error) {
	if f.state == filterPassthrough && len(f.pending) == 0 {
		if _, err := f.out.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	f.pending = append(f.pending, p...)
	f.obuf = f.obuf[:0]
	consumed := 0
	for consumed < len(f.pending) {
		n := f.next(f.pending[consumed:])
		if n == 0 {
			break
		}
		consumed += n
	}
	f.pending = append(f.pending[:0], f.pending[consumed:]...)
	if len(f.obuf) > 0 {
		if _, err := f.out.Write(f.obuf); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (f *clientFilter) next(b []byte) int {
	switch f.state {
	case filterPassthrough:
		f.obuf = append(f.obuf, b...)
		return len(b)
	case filterCredSSP:
		if b[0] != 0x30 {
			f.state = filterRDP
			return f.next(b)
		}
		n, ok := derSize(b)
		switch {
		case !ok || n > maxCredSSPMessage:
			return f.passthrough(b)
		case n == 0 || len(b) < n:
			return 0
		}
		f.obuf = append(f.obuf, b[:n]...)
		return n
	}
	if b[0] != fpActionX224 { // fast-path input: the connection sequence is over
		return f.passthrough(b)
	}
	if len(b) < 4 {
		return 0
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n < 4 {
		return f.passthrough(b)
	}
	if len(b) < n {
		return 0
	}
	start := len(f.obuf)
	f.obuf = append(f.obuf, b[:n]...)
	f.tpkts++
	if f.patch(f.obuf[start:]) || f.tpkts > maxConnectSequencePDUs {
		f.state = filterPassthrough
	}
	return n
}

func (f *clientFilter) passthrough(b []byte) int {
	f.state = filterPassthrough
	f.obuf = append(f.obuf, b...)
	return len(b)
}

// patch adjusts a client TPKT in place; it returns true once the Client Info PDU went by (nothing left to do).
func (f *clientFilter) patch(pdu []byte) bool {
	if len(pdu) < 9 || pdu[4] != 0x02 || pdu[5] != 0xF0 || pdu[6] != 0x80 {
		return false
	}
	switch {
	case pdu[7] == mcsConnectInitial0 && pdu[8] == mcsConnectInitial1:
		build, disabled := patchConnectInitial(pdu[9:], f.blocked)
		f.patchedBuild = build || f.patchedBuild
		f.disabled = append(f.disabled, disabled...)
		return false
	case pdu[7]>>2 == mcsSendDataRequest:
		info, ok := clientInfoPacket(pdu)
		if !ok {
			return false
		}
		if f.autologon && len(info) >= 14 {
			cbUser := binary.LittleEndian.Uint16(info[10:])
			cbPass := binary.LittleEndian.Uint16(info[12:])
			flags := binary.LittleEndian.Uint32(info[4:])
			if cbUser > 0 && cbPass > 0 && flags&infoAutologon == 0 {
				binary.LittleEndian.PutUint32(info[4:], flags|infoAutologon)
				f.patchedLogon = true
			}
		}
		return true
	}
	return false
}

// patchConnectInitial adjusts the client data blocks of the GCC Conference Create Request (the user data of the MCS
// Connect-Initial) in place: it raises a clientBuild servers consider ancient (Client Core Data) and renames the
// blocked static channels (Client Network Data). It reports whether the build changed and the channels it disabled.
func patchConnectInitial(b []byte, blocked map[string]bool) (buildPatched bool, disabled []string) {
	i := findCoreData(b)
	if i < 0 {
		return false, nil
	}
	if build := binary.LittleEndian.Uint32(b[i+csCoreBuildOffset:]); build < minResizableBuild {
		binary.LittleEndian.PutUint32(b[i+csCoreBuildOffset:], reportedClientBuild)
		buildPatched = true
	}
	// The data blocks follow each other: TS_UD_HEADER (type, length) + body.
	for j := i; j+4 <= len(b); {
		typ := binary.LittleEndian.Uint16(b[j:])
		length := int(binary.LittleEndian.Uint16(b[j+2:]))
		if typ&0xC000 != 0xC000 || length < 4 || j+length > len(b) {
			break
		}
		if typ == csNetType && length >= 8 {
			count := int(binary.LittleEndian.Uint32(b[j+4:]))
			if count >= 0 && count <= 31 && 8+count*channelDefSize <= length {
				for k := range count {
					def := b[j+8+k*channelDefSize:]
					name := cString(def[:8])
					if blocked[name] {
						copy(def[:8], disabledChannelName(len(disabled)))
						disabled = append(disabled, name)
					}
				}
			}
		}
		j += length
	}
	return buildPatched, disabled
}

// findCoreData returns the offset of the Client Core Data block (the first client data block), or -1.
func findCoreData(b []byte) int {
	for i := 0; i+csCoreBuildOffset+4 <= len(b); i++ {
		if b[i] != byte(csCoreType&0xFF) || b[i+1] != byte(csCoreType>>8) {
			continue
		}
		length := int(binary.LittleEndian.Uint16(b[i+2:]))
		version := binary.LittleEndian.Uint32(b[i+4:])
		if length >= csCoreMinLength && i+length <= len(b) && version >= 0x00080001 && version <= 0x000800FF {
			return i
		}
	}
	return -1
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// disabledChannelName is the (unique, meaningless to servers) name a disabled channel gets: "nxoff0" …
func disabledChannelName(i int) []byte {
	n := make([]byte, 8)
	copy(n, "nxoff")
	n[5] = '0' + byte(i%10)
	return n
}

// clientInfoPacket returns the TS_INFO_PACKET of a Client Info PDU (MCS Send Data Request whose basic security header
// carries SEC_INFO_PKT, unencrypted: TLS / CredSSP connections).
func clientInfoPacket(pdu []byte) ([]byte, bool) {
	const mcsStart = 7
	i := mcsStart + 6 // choice, initiator (2), channelId (2), priority/segmentation (1)
	if len(pdu) <= i {
		return nil, false
	}
	l := int(pdu[i])
	i++
	if l&0x80 != 0 {
		if l&0x40 != 0 || i >= len(pdu) {
			return nil, false
		}
		l = (l&0x3F)<<8 | int(pdu[i])
		i++
	}
	if i+l != len(pdu) || l < 4+18 {
		return nil, false
	}
	secFlags := binary.LittleEndian.Uint16(pdu[i:])
	if secFlags&secInfoPkt == 0 || secFlags&secEncrypt != 0 {
		return nil, false
	}
	return pdu[i+4:], true
}
