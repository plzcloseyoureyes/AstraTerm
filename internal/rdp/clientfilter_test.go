package rdp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"
)

// realConnectInitial is the MCS Connect-Initial IronRDP 0.7 sent to xrdp (client build 0, channel drdynvc).
func realConnectInitial(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/ironrdp_connect_initial.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// connectInitialWithChannels replaces the Client Network Data of the real PDU with the given channels.
func connectInitialWithChannels(t *testing.T, channels ...string) []byte {
	t.Helper()
	pdu := realConnectInitial(t)
	core := findCoreData(pdu[9:]) + 9
	net := core
	for { // walk to the Client Network Data block
		if binary.LittleEndian.Uint16(pdu[net:]) == csNetType {
			break
		}
		net += int(binary.LittleEndian.Uint16(pdu[net+2:]))
	}
	oldLen := int(binary.LittleEndian.Uint16(pdu[net+2:]))
	block := make([]byte, 8, 8+12*len(channels))
	binary.LittleEndian.PutUint16(block, csNetType)
	binary.LittleEndian.PutUint16(block[2:], uint16(8+12*len(channels)))
	binary.LittleEndian.PutUint32(block[4:], uint32(len(channels)))
	for _, c := range channels {
		def := make([]byte, 12)
		copy(def, c)
		binary.LittleEndian.PutUint32(def[8:], 0x80800000)
		block = append(block, def...)
	}
	out := append(append(append([]byte(nil), pdu[:net]...), block...), pdu[net+oldLen:]...)
	binary.BigEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// clientInfoPDU builds a Client Info PDU (TLS security: basic security header with SEC_INFO_PKT, unencrypted).
func clientInfoPDU(user, password string, flags uint32) []byte {
	utf16 := func(s string) []byte {
		b := make([]byte, 0, 2*len(s))
		for _, r := range s {
			b = append(b, byte(r), 0)
		}
		return b
	}
	u, p := utf16(user), utf16(password)
	info := make([]byte, 18)
	binary.LittleEndian.PutUint32(info[4:], flags)
	binary.LittleEndian.PutUint16(info[10:], uint16(len(u)))
	binary.LittleEndian.PutUint16(info[12:], uint16(len(p)))
	info = append(info, 0, 0) // domain
	info = append(append(info, u...), 0, 0)
	info = append(append(info, p...), 0, 0)
	info = append(info, 0, 0, 0, 0) // alternate shell, working dir
	body := append([]byte{0x40, 0x00, 0x00, 0x00}, info...)
	per := []byte{byte(len(body))}
	if len(body) >= 0x80 {
		per = []byte{0x80 | byte(len(body)>>8), byte(len(body))}
	}
	pdu := []byte{3, 0, 0, 0, 0x02, 0xF0, 0x80, mcsSendDataRequest << 2, 0x00, 0x04, 0x03, 0xEB, 0x70}
	pdu = append(append(pdu, per...), body...)
	binary.BigEndian.PutUint16(pdu[2:], uint16(len(pdu)))
	return pdu
}

func infoFlags(t *testing.T, pdu []byte) uint32 {
	t.Helper()
	info, ok := clientInfoPacket(pdu)
	if !ok {
		t.Fatal("not a client info PDU")
	}
	return binary.LittleEndian.Uint32(info[4:])
}

func clientFilterAll(t *testing.T, selected uint32, autologon bool, blocked []string, input []byte, chunk int) ([]byte, *clientFilter) {
	t.Helper()
	var out bytes.Buffer
	f := newClientFilter(&out, selected, autologon, blocked)
	for len(input) > 0 {
		n := len(input)
		if chunk > 0 {
			n = min(chunk, n)
		}
		if _, err := f.Write(input[:n]); err != nil {
			t.Fatal(err)
		}
		input = input[n:]
	}
	return out.Bytes(), f
}

func TestClientFilterRaisesClientBuild(t *testing.T) {
	ci := realConnectInitial(t)
	core := findCoreData(ci[9:]) + 9
	if build := binary.LittleEndian.Uint32(ci[core+csCoreBuildOffset:]); build != 0 {
		t.Fatalf("vector build %d", build)
	}
	for _, chunk := range []int{0, 1, 5} {
		out, f := clientFilterAll(t, protoSSL, false, nil, ci, chunk)
		if !f.patchedBuild || len(out) != len(ci) {
			t.Fatalf("chunk %d: patched %v, %d bytes", chunk, f.patchedBuild, len(out))
		}
		if build := binary.LittleEndian.Uint32(out[core+csCoreBuildOffset:]); build != reportedClientBuild {
			t.Fatalf("build %d", build)
		}
		// Nothing else changed.
		diff := 0
		for i := range out {
			if out[i] != ci[i] {
				diff++
			}
		}
		if diff > 4 {
			t.Fatalf("%d bytes changed", diff)
		}
	}
	// A modern build is left alone.
	modern := append([]byte(nil), ci...)
	binary.LittleEndian.PutUint32(modern[core+csCoreBuildOffset:], 19041)
	out, f := clientFilterAll(t, protoSSL, false, nil, modern, 0)
	if f.patchedBuild || !bytes.Equal(out, modern) {
		t.Fatal("a recent client build must not change")
	}
}

func TestClientFilterDisablesBlockedChannels(t *testing.T) {
	ci := connectInitialWithChannels(t, "cliprdr", "rdpdr", "rdpsnd", "drdynvc")
	out, f := clientFilterAll(t, protoSSL, false, []string{"cliprdr", "rdpsnd"}, ci, 3)
	if !slices.Equal(f.disabled, []string{"cliprdr", "rdpsnd"}) || len(out) != len(ci) {
		t.Fatalf("disabled %v", f.disabled)
	}
	var names []string
	core := findCoreData(out[9:]) + 9
	for j := core; j+4 <= len(out); {
		typ := binary.LittleEndian.Uint16(out[j:])
		l := int(binary.LittleEndian.Uint16(out[j+2:]))
		if typ&0xC000 != 0xC000 || l < 4 {
			break
		}
		if typ == csNetType {
			for k := range int(binary.LittleEndian.Uint32(out[j+4:])) {
				names = append(names, cString(out[j+8+12*k:j+16+12*k]))
			}
		}
		j += l
	}
	if !slices.Equal(names, []string{"nxoff0", "rdpdr", "nxoff1", "drdynvc"}) {
		t.Fatalf("channels %v", names)
	}
	// The policy of a connection.
	if got := channelPolicy(rdpOptions{DisableClipboard: true, EnableAudio: true, EnablePrinting: true}); !slices.Equal(got, []string{"cliprdr"}) {
		t.Fatalf("policy %v", got)
	}
	if got := channelPolicy(rdpOptions{EnableDrive: true}); !slices.Equal(got, []string{"rdpsnd"}) {
		t.Fatalf("policy %v", got)
	}
}

func TestClientFilterAutologon(t *testing.T) {
	ci := realConnectInitial(t)
	info := clientInfoPDU("ubuntu", "secret", 0x0000_0033)
	after := clientInfoPDU("x", "y", 0) // anything after the Client Info PDU passes untouched
	for _, tc := range []struct {
		name      string
		autologon bool
		pdu       []byte
		want      uint32
	}{
		{"credentials", true, info, 0x33 | infoAutologon},
		{"not requested", false, info, 0x33},
		{"no password", true, clientInfoPDU("ubuntu", "", 0x33), 0x33},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, f := clientFilterAll(t, protoSSL, tc.autologon, nil, concat(ci, tc.pdu, after), 7)
			rest := out[len(ci):]
			if got := infoFlags(t, rest[:len(tc.pdu)]); got != tc.want {
				t.Fatalf("flags %#x, want %#x", got, tc.want)
			}
			if !bytes.Equal(rest[len(tc.pdu):], after) || f.state != filterPassthrough {
				t.Fatal("PDUs after the Client Info PDU must pass unchanged")
			}
		})
	}
}

func TestClientFilterCredSSPThenConnectInitial(t *testing.T) {
	ci := realConnectInitial(t)
	tsreq := append([]byte{0x30, 0x81, 0x90}, make([]byte, 0x90)...)
	in := concat(tsreq, []byte{0x30, 0x02, 0xA0, 0x00}, ci, clientInfoPDU("u", "p", 0))
	out, f := clientFilterAll(t, protoHybridEx, true, nil, in, 11)
	if !f.patchedBuild || !f.patchedLogon || len(out) != len(in) || !bytes.Equal(out[:len(tsreq)+4], in[:len(tsreq)+4]) {
		t.Fatalf("patched build %v logon %v", f.patchedBuild, f.patchedLogon)
	}
}

func TestClientFilterPassesUnknownTraffic(t *testing.T) {
	// Fast-path input right away: pass-through.
	in := []byte{0x04, 0x05, 0x01, 0x02, 0x03, 0x04}
	out, f := clientFilterAll(t, protoSSL, true, []string{"cliprdr"}, in, 2)
	if !bytes.Equal(out, in) || f.state != filterPassthrough {
		t.Fatal("unknown traffic changed")
	}
}

func FuzzClientFilter(f *testing.F) {
	f.Add([]byte{0x03, 0x00, 0x00, 0x0B, 0x02, 0xF0, 0x80, 0x7F, 0x65, 0x01, 0x02}, uint8(1))
	f.Add(clientInfoPDU("a", "b", 0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		var out bytes.Buffer
		fl := newClientFilter(&out, []uint32{protoSSL, protoHybrid}[int(mode)%2], mode&2 != 0, []string{"cliprdr", "rdpdr"})
		for i := 0; i < len(data); i += 1 + int(mode)%7 {
			_, _ = fl.Write(data[i:min(len(data), i+1+int(mode)%7)])
		}
		// The client filter never changes lengths: everything consumed was forwarded.
		if out.Len()+len(fl.pending) != len(data) {
			t.Fatalf("forwarded %d + pending %d != %d", out.Len(), len(fl.pending), len(data))
		}
	})
}
