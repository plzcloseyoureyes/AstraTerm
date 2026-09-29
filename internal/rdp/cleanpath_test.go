package rdp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// der builds a TLV (short or long form length).
func der(tag byte, content ...[]byte) []byte {
	body := bytes.Join(content, nil)
	out := []byte{tag}
	switch n := len(body); {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	default:
		out = append(out, 0x82, byte(n>>8), byte(n))
	}
	return append(out, body...)
}

func TestCleanPathResponseDER(t *testing.T) {
	x224 := []byte{3, 0, 0, 19, 14, 0xD0, 0, 0, 0x12, 0x34, 0, 2, 0, 8, 0, 1, 0, 0, 0}
	certA, certB := bytes.Repeat([]byte{0xAA}, 200), []byte{0xBB, 0xCC}
	got, err := cleanPathResponse(x224, [][]byte{certA, certB}, "10.0.0.5:3389")
	if err != nil {
		t.Fatal(err)
	}
	// Every field is [n] EXPLICIT (constructed context-specific tag 0xA0|n) around its universal encoding, strings are
	// UTF8String (0x0C) — the encoding of IronRDP's RDCleanPathPdu (der crate, tag_mode = "EXPLICIT").
	want := der(0x30,
		der(0xA0, []byte{0x02, 0x02, 0x0D, 0x3E}),
		der(0xA6, der(0x04, x224)),
		der(0xA7, der(0x30, der(0x04, certA), der(0x04, certB))),
		der(0xA9, der(0x0C, []byte("10.0.0.5:3389"))),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("response DER\n got %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	back, err := decodeCleanPath(got)
	if err != nil {
		t.Fatal(err)
	}
	if back.Version != cleanPathVersion || !bytes.Equal(back.X224ConnectionPDU, x224) || len(back.ServerCertChain) != 2 ||
		back.ServerAddr != "10.0.0.5:3389" || back.Error.ErrorCode != 0 {
		t.Fatalf("round trip %+v", back)
	}
}

func TestCleanPathErrorDER(t *testing.T) {
	got, err := cleanPathFailure{HTTPStatus: 502}.encode()
	if err != nil {
		t.Fatal(err)
	}
	want := der(0x30,
		der(0xA0, []byte{0x02, 0x02, 0x0D, 0x3E}),
		der(0xA1, der(0x30,
			der(0xA0, []byte{0x02, 0x01, 0x01}),
			der(0xA1, []byte{0x02, 0x02, 0x01, 0xF6}),
		)),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("error DER\n got %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	// WSA / TLS codes; the negotiation error carries the server's confirm.
	got, _ = cleanPathFailure{WSAError: wsaConnRefused}.encode()
	p, err := decodeCleanPath(got)
	if err != nil || p.Error.ErrorCode != cleanPathGeneralError || p.Error.WSALastError != 10061 || p.Error.HTTPStatusCode != 0 {
		t.Fatalf("wsa %+v %v", p, err)
	}
	cc := []byte{3, 0, 0, 19, 14, 0xD0, 0, 0, 0, 0, 0, 3, 0, 8, 0, 5, 0, 0, 0}
	got, _ = cleanPathFailure{Code: cleanPathNegotiationError, X224: cc, TLSAlert: 0}.encode()
	p, err = decodeCleanPath(got)
	if err != nil || p.Error.ErrorCode != cleanPathNegotiationError || !bytes.Equal(p.X224ConnectionPDU, cc) {
		t.Fatalf("negotiation %+v %v", p, err)
	}
	got, _ = cleanPathFailure{TLSAlert: 42}.encode()
	if p, _ = decodeCleanPath(got); p.Error.TLSAlertCode != 42 {
		t.Fatalf("tls alert %+v", p)
	}
}

func TestCleanPathRequestDecode(t *testing.T) {
	// A request as IronRDP sends it: version, destination, proxy_auth, preconnection blob, X.224 request.
	cr := probeConnectionRequest(protoSSL | protoHybrid)
	req := der(0x30,
		der(0xA0, []byte{0x02, 0x02, 0x0D, 0x3E}),
		der(0xA2, der(0x0C, []byte("win.example:3389"))),
		der(0xA3, der(0x0C, []byte("token-value"))),
		der(0xA5, der(0x0C, []byte("vm-guid"))),
		der(0xA6, der(0x04, cr)),
	)
	n, err := derLength(req[:3])
	if err != nil || n != len(req) {
		t.Fatalf("derLength %d %v (want %d)", n, err, len(req))
	}
	p, err := decodeCleanPath(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 3390 || p.Destination != "win.example:3389" || p.ProxyAuth != "token-value" || p.PreconnectionBlob != "vm-guid" ||
		!bytes.Equal(p.X224ConnectionPDU, cr) {
		t.Fatalf("request %+v", p)
	}
	// Unknown trailing fields ([8] OCSP, future additions) are tolerated.
	withExtra := der(0x30, req[2:], der(0xA8, der(0x0C, []byte("x"))))
	if _, err := decodeCleanPath(withExtra); err != nil {
		t.Fatalf("extra field: %v", err)
	}
	if _, err := decodeCleanPath(append(append([]byte(nil), req...), 0)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := decodeCleanPath([]byte{0x30, 0x03, 0x02, 0x01}); err == nil {
		t.Fatal("truncated PDU accepted")
	}
}

func TestDERLength(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want int
		err  bool
	}{
		{[]byte{0x30}, 0, false},
		{[]byte{0x30, 0x05}, 7, false},
		{[]byte{0x30, 0x81}, 0, false},
		{[]byte{0x30, 0x81, 0x80}, 131, false},
		{[]byte{0x30, 0x82, 0x01, 0x00}, 260, false},
		{[]byte{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff}, 0, true}, // too large
		{[]byte{0x30, 0x80}, 0, true},                         // indefinite length
		{[]byte{0x04, 0x01}, 0, true},                         // not a SEQUENCE
	} {
		got, err := derLength(c.in)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("derLength(%x) = %d, %v", c.in, got, err)
		}
	}
}

func TestX224(t *testing.T) {
	// Connection request with a cookie, as IronRDP sends it.
	cookie := []byte("Cookie: mstshash=alice\r\n")
	body := append([]byte{0xE0, 0, 0, 0, 0, 0}, cookie...)
	body = append(body, negTypeReq, 0, 8, 0)
	body = binary.LittleEndian.AppendUint32(body, protoSSL|protoHybridEx)
	cr := append([]byte{3, 0, 0, 0, byte(len(body))}, body...)
	binary.BigEndian.PutUint16(cr[2:4], uint16(len(cr)))
	req, err := parseConnectionRequest(cr)
	if err != nil || !req.hasNeg || req.requested != protoSSL|protoHybridEx {
		t.Fatalf("request %+v %v", req, err)
	}
	if _, err := parseConnectionRequest(cr[:len(cr)-1]); err == nil {
		t.Fatal("bad TPKT length accepted")
	}
	pr, err := parseConnectionRequest(probeConnectionRequest(protoSSL))
	if err != nil || pr.requested != protoSSL {
		t.Fatalf("probe request %+v %v", pr, err)
	}

	// Confirms.
	rsp := []byte{3, 0, 0, 19, 14, 0xD0, 0, 0, 0x12, 0x34, 0, negTypeRsp, 0x1f, 8, 0, 2, 0, 0, 0}
	cc, err := parseConnectionConfirm(rsp)
	if err != nil || !cc.negotiation || cc.failed || cc.selected != protoHybrid || !needsTLS12(cc.selected) {
		t.Fatalf("confirm %+v %v", cc, err)
	}
	fail := []byte{3, 0, 0, 19, 14, 0xD0, 0, 0, 0, 0, 0, negTypeFailure, 0, 8, 0, 5, 0, 0, 0}
	cc, err = parseConnectionConfirm(fail)
	if err != nil || !cc.failed || cc.failure != 5 || !strings.Contains(negotiationFailureText(cc.failure), "Network Level") {
		t.Fatalf("failure %+v %v", cc, err)
	}
	legacy := []byte{3, 0, 0, 11, 6, 0xD0, 0, 0, 0, 0, 0}
	cc, err = parseConnectionConfirm(legacy)
	if err != nil || cc.negotiation {
		t.Fatalf("legacy %+v %v", cc, err)
	}
	if _, err := parseConnectionConfirm([]byte{3, 0, 0, 11, 6, 0xF0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("data TPDU accepted as confirm")
	}

	// TPKT framing.
	pkt, err := readTPKT(bytes.NewReader(append(append([]byte(nil), rsp...), 0xFF)))
	if err != nil || !bytes.Equal(pkt, rsp) {
		t.Fatalf("readTPKT %x %v", pkt, err)
	}
	if _, err := readTPKT(bytes.NewReader([]byte{0x16, 3, 1, 0, 5})); err == nil {
		t.Fatal("TLS record accepted as TPKT")
	}
	if _, err := readTPKT(bytes.NewReader(rsp[:10])); err != io.ErrUnexpectedEOF {
		t.Fatalf("short TPKT: %v", err)
	}
}

func TestPreconnectionPDU(t *testing.T) {
	b := preconnectionPDU(7, "AB")
	// cbSize(4) flags(4) version(4) id(4) cchPCB(2) "A\0B\0\0\0"
	want := []byte{24, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 7, 0, 0, 0, 3, 0, 'A', 0, 'B', 0, 0, 0}
	if !bytes.Equal(b, want) {
		t.Fatalf("pcb %v", b)
	}
	if protocolName(protoHybridEx|protoHybrid) == "" || protocolName(protoSSL) != "TLS" || protocolName(0) != "standard RDP security" {
		t.Fatal("protocolName")
	}
}
