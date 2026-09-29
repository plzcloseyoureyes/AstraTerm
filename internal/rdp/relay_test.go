package rdp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexterm/nexterm/internal/model"
)

// ---- a fake RDP server: X.224 negotiation, then TLS, then an echo service ------------------------------------------

type fakeRDP struct {
	ln       net.Listener
	cert     tls.Certificate
	selected uint32
	failure  uint32 // RDP_NEG_FAILURE code instead of a response
	gotCR    chan []byte
	gotPCB   chan []byte
	pcb      bool
}

func selfSigned(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newFakeRDP starts a fake server; configure adjusts it before it accepts connections.
func newFakeRDP(t *testing.T, selected uint32, configure ...func(*fakeRDP)) *fakeRDP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRDP{ln: ln, cert: selfSigned(t, "fake-rdp"), selected: selected, gotCR: make(chan []byte, 8), gotPCB: make(chan []byte, 8)}
	for _, c := range configure {
		c(f)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeRDP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeRDP) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if f.pcb {
		var hdr [4]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		rest := make([]byte, binary.LittleEndian.Uint32(hdr[:])-4)
		if _, err := io.ReadFull(c, rest); err != nil {
			return
		}
		f.gotPCB <- append(hdr[:], rest...)
	}
	cr, err := readTPKT(c)
	if err != nil {
		return
	}
	f.gotCR <- cr
	cc := []byte{3, 0, 0, 19, 14, 0xD0, 0, 0, 0x12, 0x34, 0, negTypeRsp, 0, 8, 0, 0, 0, 0, 0}
	if f.failure != 0 {
		cc[11] = negTypeFailure
		binary.LittleEndian.PutUint32(cc[15:], f.failure)
	} else {
		binary.LittleEndian.PutUint32(cc[15:], f.selected)
	}
	if _, err := c.Write(cc); err != nil || f.failure != 0 {
		return
	}
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}})
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	_, _ = io.Copy(tc, tc) // echo
}

// ironRDPRequest builds the RDCleanPath request the IronRDP web client sends.
func ironRDPRequest(t *testing.T, token, dest, pcb string) []byte {
	t.Helper()
	cookie := []byte("Cookie: mstshash=alice\r\n")
	body := append([]byte{0xE0, 0, 0, 0, 0, 0}, cookie...)
	body = append(body, negTypeReq, 0, 8, 0)
	body = binary.LittleEndian.AppendUint32(body, protoSSL|protoHybrid|protoHybridEx)
	cr := append([]byte{3, 0, 0, 0, byte(len(body))}, body...)
	binary.BigEndian.PutUint16(cr[2:4], uint16(len(cr)))
	b, err := (&cleanPathPDU{Version: cleanPathVersion, Destination: dest, ProxyAuth: token, PreconnectionBlob: pcb,
		X224ConnectionPDU: cr}).encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type ticketResp = ticketResponse

func (c *testClient) ticket(sessionID string, body any) ticketResp {
	c.env.t.Helper()
	var tr ticketResp
	c.must("POST", "/api/sessions/"+sessionID+"/rdp-ticket", body, &tr)
	return tr
}

// relayHandshake runs the RDCleanPath exchange over /ws/rdp and returns the socket and the decoded answer.
func (c *testClient) relayHandshake(sessionID string, req []byte) (*websocket.Conn, *cleanPathPDU) {
	c.env.t.Helper()
	ws, _, err := c.ws("/ws/rdp/" + sessionID)
	if err != nil {
		c.env.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, req); err != nil {
		c.env.t.Fatal(err)
	}
	typ, data, err := ws.Read(ctx)
	if err != nil {
		c.env.t.Fatalf("read RDCleanPath answer: %v", err)
	}
	if typ != websocket.MessageBinary {
		c.env.t.Fatalf("answer type %v", typ)
	}
	p, err := decodeCleanPath(data)
	if err != nil {
		c.env.t.Fatal(err)
	}
	return ws, p
}

func TestRelayEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	srv := newFakeRDP(t, protoSSL)
	conn := env.createConnection(admin.user, "127.0.0.1", srv.port(), "alice", map[string]string{"password": "s3cret"}, model.Options{})
	pa := admin.answerPrompts(func(p model.Prompt) model.PromptResponse {
		return model.PromptResponse{Accept: p.Kind == model.PromptHostKey, Save: true}
	})
	rs := admin.openSession(conn.ID)

	tr := admin.ticket(rs.ID, map[string]any{"width": 1024, "height": 700, "dpi": 120})
	if tr.Engine != engineIronRDP || tr.Token == "" || tr.Password != "s3cret" || tr.Username != "alice" ||
		tr.Destination != "127.0.0.1:"+strconv.Itoa(srv.port()) || tr.Width != 1024 || tr.Height != 700 || tr.DPI != 120 ||
		!tr.EnableCredssp || !tr.Clipboard || tr.FixedSize {
		t.Fatalf("ticket %+v", tr)
	}

	ws, p := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, "ignored.example:3389", ""))
	defer ws.CloseNow()
	if p.Error.ErrorCode != 0 || len(p.ServerCertChain) != 1 || !bytes.Equal(p.ServerCertChain[0], srv.cert.Certificate[0]) ||
		p.ServerAddr != "127.0.0.1:"+strconv.Itoa(srv.port()) {
		t.Fatalf("answer %+v", p)
	}
	cc, err := parseConnectionConfirm(p.X224ConnectionPDU)
	if err != nil || cc.selected != protoSSL {
		t.Fatalf("confirm %+v %v", cc, err)
	}
	cr := <-srv.gotCR
	if req, _ := parseConnectionRequest(cr); req.requested != protoSSL|protoHybrid|protoHybridEx {
		t.Fatalf("server got request %x", cr)
	}

	// The unknown certificate was confirmed through the prompt broker and pinned as an rdp-tls known host.
	prompts := pa.seen()
	if len(prompts) != 1 || prompts[0].HostKey == nil || prompts[0].HostKey.KeyType != rdpKeyType ||
		prompts[0].HostKey.Fingerprint != certFingerprint(srv.cert.Certificate[0]) || prompts[0].SessionID != rs.ID {
		t.Fatalf("prompts %+v", prompts)
	}
	known, _ := env.d.Store.KnownHosts.Find(context.Background(), "127.0.0.1", srv.port())
	if len(known) != 1 || known[0].KeyType != rdpKeyType || known[0].Fingerprint != certFingerprint(srv.cert.Certificate[0]) {
		t.Fatalf("known hosts %+v", known)
	}

	// Plaintext pump (the fake server echoes).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("rdp-bytes")); err != nil {
		t.Fatal(err)
	}
	var echoed []byte
	for len(echoed) < len("rdp-bytes") {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		echoed = append(echoed, data...)
	}
	if string(echoed) != "rdp-bytes" {
		t.Fatalf("echo %q", echoed)
	}
	if st, msg := env.sessionState(rs.ID); st != model.StateAuthenticating || !strings.Contains(msg, "TLS") {
		t.Fatalf("state %s %q", st, msg)
	}
	if n := env.core.Sessions.Get(rs.ID).Info().Clients; n != 1 {
		t.Fatalf("clients %d", n)
	}
	var info model.RuntimeSession
	admin.must("POST", "/api/sessions/"+rs.ID+"/rdp-state", map[string]string{"state": "connected"}, &info)
	if info.State != model.StateConnected || info.ConnectedAt == nil {
		t.Fatalf("state after report %+v", info)
	}

	// The token was single-use.
	ws2, p2 := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, "", ""))
	ws2.CloseNow()
	if p2.Error.ErrorCode != cleanPathGeneralError || p2.Error.HTTPStatusCode != 403 {
		t.Fatalf("reused token answer %+v", p2)
	}

	ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "viewer released", func() bool {
		st, _ := env.sessionState(rs.ID)
		return env.core.Sessions.Get(rs.ID).Info().Clients == 0 && st != model.StateConnected
	})

	// Second connection: the pinned certificate is accepted without a prompt.
	tr = admin.ticket(rs.ID, nil)
	ws3, p3 := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, "", ""))
	ws3.CloseNow()
	if p3.Error.ErrorCode != 0 || len(pa.seen()) != 1 {
		t.Fatalf("second connection %+v, prompts %d", p3.Error, len(pa.seen()))
	}

	// Closing the session ends the relay and forgets tickets.
	tr = admin.ticket(rs.ID, nil)
	admin.must("DELETE", "/api/sessions/"+rs.ID, nil, nil)
	if env.h.tickets.len() != 0 {
		t.Fatalf("tickets left: %d", env.h.tickets.len())
	}
}

func TestRelayCertificateMismatchAndRejection(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	srv := newFakeRDP(t, protoHybrid)
	// A different certificate is pinned for this host.
	other := selfSigned(t, "other")
	if err := env.d.Store.KnownHosts.Add(context.Background(), &model.KnownHost{Host: "127.0.0.1", Port: srv.port(),
		KeyType: rdpKeyType, PublicKey: "x", Fingerprint: certFingerprint(other.Certificate[0])}); err != nil {
		t.Fatal(err)
	}
	conn := env.createConnection(admin.user, "127.0.0.1", srv.port(), "alice", map[string]string{"password": "pw"}, model.Options{})
	pa := admin.answerPrompts(func(p model.Prompt) model.PromptResponse { return model.PromptResponse{Accept: false} })
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, nil)
	ws, p := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, "", ""))
	ws.CloseNow()
	if p.Error.ErrorCode != cleanPathGeneralError || p.Error.TLSAlertCode != 42 {
		t.Fatalf("answer %+v", p.Error)
	}
	prompts := pa.seen()
	if len(prompts) != 1 || prompts[0].HostKey.Status != model.HostKeyMismatch || !prompts[0].AllowSave {
		t.Fatalf("prompts %+v", prompts)
	}
	st, msg := env.sessionState(rs.ID)
	if st != model.StateError || !strings.Contains(msg, "changed") {
		t.Fatalf("state %s %q", st, msg)
	}
}

func TestRelayIgnoreCertAndNegotiationFailure(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	srv := newFakeRDP(t, protoSSL)
	conn := env.createConnection(admin.user, "127.0.0.1", srv.port(), "", nil, model.Options{"ignoreCert": true})
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, nil)
	// No username: no credential prompt, CredSSP off (the server shows its logon screen).
	if tr.EnableCredssp || tr.Username != "" || tr.Password != "" {
		t.Fatalf("ticket %+v", tr)
	}
	ws, p := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, "", ""))
	ws.CloseNow()
	if p.Error.ErrorCode != 0 || len(p.ServerCertChain) != 1 {
		t.Fatalf("ignoreCert answer %+v", p.Error)
	}

	// Negotiation failure: the confirm is handed to the client with the negotiation error code.
	bad := newFakeRDP(t, 0, func(f *fakeRDP) { f.failure = 5 }) // HYBRID_REQUIRED_BY_SERVER
	conn2 := env.createConnection(admin.user, "127.0.0.1", bad.port(), "", nil, model.Options{"ignoreCert": true})
	rs2 := admin.openSession(conn2.ID)
	tr2 := admin.ticket(rs2.ID, nil)
	ws2, p2 := admin.relayHandshake(rs2.ID, ironRDPRequest(t, tr2.Token, "", ""))
	ws2.CloseNow()
	if p2.Error.ErrorCode != cleanPathNegotiationError || len(p2.X224ConnectionPDU) == 0 {
		t.Fatalf("negotiation answer %+v", p2)
	}
	if st, msg := env.sessionState(rs2.ID); st != model.StateError || !strings.Contains(msg, "Network Level Authentication") {
		t.Fatalf("state %s %q", st, msg)
	}

	// Connection refused → WSA 10061.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	conn3 := env.createConnection(admin.user, "127.0.0.1", port, "", nil, model.Options{})
	rs3 := admin.openSession(conn3.ID)
	tr3 := admin.ticket(rs3.ID, nil)
	ws3, p3 := admin.relayHandshake(rs3.ID, ironRDPRequest(t, tr3.Token, "", ""))
	ws3.CloseNow()
	if p3.Error.WSALastError != wsaConnRefused {
		t.Fatalf("refused answer %+v", p3.Error)
	}
}

func TestRelayRejectsBadRequests(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	srv := newFakeRDP(t, protoSSL)
	conn := env.createConnection(admin.user, "127.0.0.1", srv.port(), "", nil, model.Options{"ignoreCert": true})
	a, b := admin.openSession(conn.ID), admin.openSession(conn.ID)

	// Wrong version.
	tr := admin.ticket(a.ID, nil)
	req, _ := (&cleanPathPDU{Version: 3389, ProxyAuth: tr.Token, X224ConnectionPDU: probeConnectionRequest(protoSSL)}).encode()
	ws, p := admin.relayHandshake(a.ID, req)
	ws.CloseNow()
	if p.Error.HTTPStatusCode != 400 {
		t.Fatalf("version answer %+v", p.Error)
	}
	// A ticket of session a presented on session b is refused (and burnt).
	tr = admin.ticket(a.ID, nil)
	ws, p = admin.relayHandshake(b.ID, ironRDPRequest(t, tr.Token, "", ""))
	ws.CloseNow()
	if p.Error.HTTPStatusCode != 403 {
		t.Fatalf("cross-session answer %+v", p.Error)
	}
	ws, p = admin.relayHandshake(a.ID, ironRDPRequest(t, tr.Token, "", ""))
	ws.CloseNow()
	if p.Error.HTTPStatusCode != 403 {
		t.Fatalf("burnt token answer %+v", p.Error)
	}
	// Garbage instead of DER.
	ws, _, err := admin.ws("/ws/rdp/" + a.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageBinary, []byte("GET / HTTP/1.1"))
	_, data, err := ws.Read(ctx)
	ws.CloseNow()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := decodeCleanPath(data); err != nil || p.Error.HTTPStatusCode != 400 {
		t.Fatalf("garbage answer %+v %v", p, err)
	}

	// Another user cannot use the session at all.
	bob := env.createUser(admin, "bob")
	if st, _ := bob.errorCode("POST", "/api/sessions/"+a.ID+"/rdp-ticket", nil); st != 404 {
		t.Fatalf("bob ticket: %d", st)
	}
	if _, resp, err := bob.ws("/ws/rdp/" + a.ID); err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("bob relay: %v", err)
	}
	// Unknown session.
	if st, _ := admin.errorCode("POST", "/api/sessions/"+model.NewID()+"/rdp-ticket", nil); st != 404 {
		t.Fatalf("unknown session: %d", st)
	}
}

func TestPCBForwarded(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	srv := newFakeRDP(t, protoHybrid, func(f *fakeRDP) { f.pcb = true })
	conn := env.createConnection(admin.user, "127.0.0.1", srv.port(), "alice", map[string]string{"password": "pw"},
		model.Options{"security": "vmconnect", "ignoreCert": true})
	rs := admin.openSession(conn.ID)
	// The VM id is required …
	if st, code := admin.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", nil); st != 400 || code != "vm_id_required" {
		t.Fatalf("no vm id: %d %s", st, code)
	}
	// … and can be supplied with the ticket request.
	tr := admin.ticket(rs.ID, map[string]string{"preconnectionBlob": "5A1B2C3D-VM"})
	if tr.PreConnectionBlob != "5A1B2C3D-VM" || !tr.EnableCredssp {
		t.Fatalf("ticket %+v", tr)
	}
	ws, p := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, "", "ignored-by-relay"))
	ws.CloseNow()
	if p.Error.ErrorCode != 0 {
		t.Fatalf("answer %+v", p.Error)
	}
	pcb := <-srv.gotPCB
	if !bytes.Equal(pcb, preconnectionPDU(0, "5A1B2C3D-VM")) {
		t.Fatalf("pcb %x", pcb)
	}
}

// TestRelayTestEnvXRDP runs the relay against the shared test environment's xrdp (NEXTERM_TESTENV=1) through the
// X.224 and TLS stages, with the certificate confirmed through the prompt broker.
func TestRelayTestEnvXRDP(t *testing.T) {
	if !testEnvEnabled() {
		t.Skip("NEXTERM_TESTENV=1 not set")
	}
	env := newTestEnv(t)
	admin := env.setup()
	conn := env.createConnection(admin.user, "127.0.0.1", 22089, "ubuntu", map[string]string{"password": "ubuntu"},
		model.Options{"security": "tls"})
	pa := admin.answerPrompts(func(p model.Prompt) model.PromptResponse { return model.PromptResponse{Accept: true} })
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, map[string]any{"width": 1280, "height": 800})
	if tr.EnableCredssp {
		t.Fatalf("tls security must disable CredSSP: %+v", tr)
	}
	ws, p := admin.relayHandshake(rs.ID, ironRDPRequest(t, tr.Token, tr.Destination, ""))
	defer ws.CloseNow()
	if p.Error.ErrorCode != 0 {
		st, msg := env.sessionState(rs.ID)
		t.Fatalf("xrdp answer %+v (state %s %q)", p.Error, st, msg)
	}
	cc, err := parseConnectionConfirm(p.X224ConnectionPDU)
	if err != nil || cc.selected&protoSSL == 0 {
		t.Fatalf("xrdp selected %+v %v", cc, err)
	}
	if len(p.ServerCertChain) == 0 {
		t.Fatal("no certificate chain")
	}
	cert, err := x509.ParseCertificate(p.ServerCertChain[0])
	if err != nil {
		t.Fatal(err)
	}
	if prompts := pa.seen(); len(prompts) != 1 || prompts[0].HostKey.Fingerprint != certFingerprint(cert.Raw) {
		t.Fatalf("prompts %+v", prompts)
	}
	if st, msg := env.sessionState(rs.ID); st != model.StateAuthenticating {
		t.Fatalf("state %s %q", st, msg)
	}
	t.Logf("xrdp: %s, certificate %q, server %s", protocolName(cc.selected), cert.Subject.CommonName, p.ServerAddr)
}
