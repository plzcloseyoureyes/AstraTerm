package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

// X.224 (ISO 8073 class 0 over TPKT, RFC 1006) framing of the RDP connection initiation ([MS-RDPBCGR] 2.2.1.1 /
// 2.2.1.2) and the pre-connection PDU ([MS-RDPEPS] 2.2.1).

// Security protocols of RDP_NEG_REQ / RDP_NEG_RSP.
const (
	protoRDP      = 0x00000000 // standard RDP security (no TLS)
	protoSSL      = 0x00000001 // TLS
	protoHybrid   = 0x00000002 // CredSSP (NLA)
	protoRDSTLS   = 0x00000004
	protoHybridEx = 0x00000008 // CredSSP + Early User Authorization Result PDU
	protoRDSAAD   = 0x00000010
)

const (
	tpktHeaderLen = 4
	// maxTPKT bounds the Connection Confirm (a few dozen bytes in practice).
	maxTPKT = 4096

	x224TypeCR = 0xE0 // Connection Request
	x224TypeCC = 0xD0 // Connection Confirm

	negTypeReq     = 0x01
	negTypeRsp     = 0x02
	negTypeFailure = 0x03
)

// readTPKT reads one TPKT packet (header included).
func readTPKT(r io.Reader) ([]byte, error) {
	var hdr [tpktHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 3 {
		return nil, fmt.Errorf("unexpected TPKT version %d (is this an RDP server?)", hdr[0])
	}
	n := int(binary.BigEndian.Uint16(hdr[2:4]))
	if n < tpktHeaderLen+3 || n > maxTPKT {
		return nil, fmt.Errorf("invalid TPKT length %d", n)
	}
	pkt := make([]byte, n)
	copy(pkt, hdr[:])
	if _, err := io.ReadFull(r, pkt[tpktHeaderLen:]); err != nil {
		return nil, err
	}
	return pkt, nil
}

// x224Request is the parsed client Connection Request.
type x224Request struct {
	requested uint32 // requested protocols (RDP_NEG_REQ), protoRDP when absent
	hasNeg    bool
}

// parseConnectionRequest validates a TPKT-framed X.224 Connection Request as sent by the client.
func parseConnectionRequest(b []byte) (x224Request, error) {
	var req x224Request
	if len(b) < tpktHeaderLen+7 {
		return req, errors.New("X.224 connection request too short")
	}
	if b[0] != 3 || int(binary.BigEndian.Uint16(b[2:4])) != len(b) {
		return req, errors.New("X.224 connection request has an invalid TPKT header")
	}
	li := int(b[4])
	if li < 6 || tpktHeaderLen+1+li > len(b) {
		return req, errors.New("X.224 connection request has an invalid length indicator")
	}
	if b[5]&0xF0 != x224TypeCR {
		return req, errors.New("not an X.224 connection request")
	}
	// Variable part: optional routing token / cookie terminated by CRLF, then an optional 8-byte RDP_NEG_REQ (and an
	// optional RDP_NEG_CORRELATION_INFO).
	v := b[tpktHeaderLen+7 : tpktHeaderLen+1+li]
	if len(v) >= 2 && v[0] == 'C' { // "Cookie: mstshash=…\r\n" or a routing token "Cookie: msts=…\r\n"
		for i := 0; i+1 < len(v); i++ {
			if v[i] == '\r' && v[i+1] == '\n' {
				v = v[i+2:]
				break
			}
		}
	}
	if len(v) >= 8 && v[0] == negTypeReq && binary.LittleEndian.Uint16(v[2:4]) == 8 {
		req.requested = binary.LittleEndian.Uint32(v[4:8])
		req.hasNeg = true
	}
	return req, nil
}

// x224Confirm is the parsed server Connection Confirm.
type x224Confirm struct {
	selected    uint32 // selected protocol (RDP_NEG_RSP), protoRDP when absent
	failure     uint32 // RDP_NEG_FAILURE code
	failed      bool
	negotiation bool // the server answered with RDP_NEG_RSP / RDP_NEG_FAILURE
}

// parseConnectionConfirm parses a TPKT-framed X.224 Connection Confirm.
func parseConnectionConfirm(b []byte) (x224Confirm, error) {
	var cc x224Confirm
	if len(b) < tpktHeaderLen+7 {
		return cc, errors.New("X.224 connection confirm too short")
	}
	li := int(b[4])
	if li < 6 || tpktHeaderLen+1+li > len(b) {
		return cc, errors.New("X.224 connection confirm has an invalid length indicator")
	}
	if b[5]&0xF0 != x224TypeCC {
		return cc, fmt.Errorf("unexpected X.224 TPDU 0x%02x (expected a connection confirm)", b[5])
	}
	neg := b[tpktHeaderLen+7 : tpktHeaderLen+1+li]
	if len(neg) >= 8 && binary.LittleEndian.Uint16(neg[2:4]) == 8 {
		switch neg[0] {
		case negTypeRsp:
			cc.selected, cc.negotiation = binary.LittleEndian.Uint32(neg[4:8]), true
		case negTypeFailure:
			cc.failure, cc.failed, cc.negotiation = binary.LittleEndian.Uint32(neg[4:8]), true, true
		}
	}
	return cc, nil
}

// negotiationFailureText explains an RDP_NEG_FAILURE code.
func negotiationFailureText(code uint32) string {
	switch code {
	case 0x01:
		return "the server requires TLS security (SSL_REQUIRED_BY_SERVER)"
	case 0x02:
		return "the server does not allow TLS security (SSL_NOT_ALLOWED_BY_SERVER)"
	case 0x03:
		return "the server has no TLS certificate (SSL_CERT_NOT_ON_SERVER)"
	case 0x04:
		return "inconsistent security flags (INCONSISTENT_FLAGS)"
	case 0x05:
		return "the server requires Network Level Authentication (HYBRID_REQUIRED_BY_SERVER)"
	case 0x06:
		return "the server requires TLS with user authentication (SSL_WITH_USER_AUTH_REQUIRED_BY_SERVER)"
	}
	return fmt.Sprintf("security negotiation failed (code %d)", code)
}

// protocolName names a selected security protocol.
func protocolName(p uint32) string {
	switch {
	case p&protoHybridEx != 0:
		return "NLA (CredSSP, HYBRID_EX)"
	case p&protoHybrid != 0:
		return "NLA (CredSSP)"
	case p&protoRDSAAD != 0:
		return "RDSAAD"
	case p&protoRDSTLS != 0:
		return "RDSTLS"
	case p&protoSSL != 0:
		return "TLS"
	}
	return "standard RDP security"
}

// needsTLS12 reports whether the selected protocol runs CredSSP, which some servers only accept over TLS ≤ 1.2.
func needsTLS12(selected uint32) bool { return selected&(protoHybrid|protoHybridEx) != 0 }

// preconnectionPDU encodes an RDP_PRECONNECTION_PDU_V2 ([MS-RDPEPS] 2.2.1.2): the pre-connection blob (Hyper-V
// console: the VM id) sent before the X.224 Connection Request.
func preconnectionPDU(id uint32, blob string) []byte {
	u := utf16.Encode([]rune(blob))
	cch := len(u) + 1 // null terminator
	size := 16 + 2 + 2*cch
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b[0:4], uint32(size))
	binary.LittleEndian.PutUint32(b[4:8], 0)  // flags
	binary.LittleEndian.PutUint32(b[8:12], 2) // version: TYPE_ID_V2
	binary.LittleEndian.PutUint32(b[12:16], id)
	binary.LittleEndian.PutUint16(b[16:18], uint16(cch))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[18+2*i:], c)
	}
	return b
}
