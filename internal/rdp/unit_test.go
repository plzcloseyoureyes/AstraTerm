package rdp

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

func TestTicketStore(t *testing.T) {
	s := newTicketStore()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	tok := s.issue(&ticket{sessionID: "s1", userID: "u1", engine: engineIronRDP})
	if len(tok) < 40 {
		t.Fatalf("token %q", tok)
	}
	if _, ok := s.consume(tok, "s1", "u1", engineGuacd); ok {
		t.Fatal("wrong engine accepted")
	}
	if _, ok := s.consume(tok, "s1", "u1", engineIronRDP); ok {
		t.Fatal("token presented with the wrong engine must be burnt")
	}
	tok = s.issue(&ticket{sessionID: "s1", userID: "u1", engine: engineIronRDP})
	if _, ok := s.consume(tok, "s1", "u2", engineIronRDP); ok {
		t.Fatal("other user accepted")
	}
	tok = s.issue(&ticket{sessionID: "s1", userID: "u1", engine: engineIronRDP})
	if tk, ok := s.consume(tok, "s1", "u1", engineIronRDP); !ok || tk.sessionID != "s1" {
		t.Fatal("valid ticket refused")
	}
	if _, ok := s.consume(tok, "s1", "u1", engineIronRDP); ok {
		t.Fatal("ticket reused")
	}
	// Expiry.
	tok = s.issue(&ticket{sessionID: "s1", userID: "u1", engine: engineIronRDP})
	now = now.Add(ticketTTL)
	if _, ok := s.consume(tok, "s1", "u1", engineIronRDP); ok {
		t.Fatal("expired ticket accepted")
	}
	// Per-user bound and session revocation.
	for i := 0; i < maxTicketsPerUser+10; i++ {
		s.issue(&ticket{sessionID: "s2", userID: "u9", engine: engineGuacd})
	}
	if n := s.len(); n != maxTicketsPerUser {
		t.Fatalf("stored %d tickets", n)
	}
	s.revokeSession("s2")
	if n := s.len(); n != 0 {
		t.Fatalf("after revoke %d", n)
	}
	if _, ok := s.consume("", "s1", "u1", engineIronRDP); ok {
		t.Fatal("empty token")
	}
}

func TestParseOptionsAndEngine(t *testing.T) {
	o := parseOptions(model.Options{})
	if o.Security != secAny || o.ColorDepth != 32 || o.ResizeMethod != "display-update" || o.DriveName != defaultDrive ||
		o.fixedSize() || !o.wantsCredSSP() || o.Engine != "" {
		t.Fatalf("defaults %+v", o)
	}
	o = parseOptions(model.Options{"security": "NLA-EXT", "width": 100, "height": 900, "colorDepth": 12, "dpi": 1000,
		"resizeMethod": "weird", "rdpEngine": "GUACD", "gatewayPort": 99999, "driveName": "Share"})
	if o.Security != secNLA || o.fixedSize() || o.ColorDepth != 32 || o.DPI != 0 || o.ResizeMethod != "display-update" ||
		o.Engine != engineGuacd || o.GatewayPort != 0 || o.DriveName != "Share" {
		t.Fatalf("sanitized %+v", o)
	}
	o = parseOptions(model.Options{"width": "1920", "height": 1080.0, "security": "tls"})
	if !o.fixedSize() || o.Width != 1920 || o.Height != 1080 || o.wantsCredSSP() {
		t.Fatalf("fixed %+v", o)
	}
	for _, c := range []struct {
		req, opt, def, want string
	}{
		{"", "", "", engineIronRDP},
		{"", "", engineGuacd, engineGuacd},
		{"", engineIronRDP, engineGuacd, engineIronRDP},
		{engineGuacd, engineIronRDP, "", engineGuacd},
		{"bogus", "", "", engineIronRDP},
	} {
		if got := chooseEngine(c.req, rdpOptions{Engine: c.opt}, globalSettings{DefaultEngine: c.def}); got != c.want {
			t.Errorf("chooseEngine(%q,%q,%q) = %q", c.req, c.opt, c.def, got)
		}
	}
}

func TestGuacParams(t *testing.T) {
	h := &handler{}
	tk := &ticket{
		userID:    "user1",
		sessionID: "sess1",
		conn:      &model.Connection{Username: `CORP\alice`, Options: model.Options{"preconnectionBlob": "vm-1"}},
		secrets:   map[string]string{"password": "pw", "gatewayPassword": "gwpw"},
	}
	opts := parseOptions(model.Options{"enableDrive": true, "recording": true, "gatewayHost": "gw.example", "gatewayPort": 8443,
		"width": 1600, "height": 900, "enableAudio": true, "disableClipboard": true, "security": "nla", "resizeMethod": "none",
		"preconnectionId": 7})
	p := h.guacParams(tk, opts, globalSettings{GuacdDataPath: "/data/guac/"}, "127.0.0.1", 40000, true)
	want := map[string]string{
		"hostname": "127.0.0.1", "port": "40000", "username": "alice", "domain": "CORP", "password": "pw",
		"security": "nla", "ignore-cert": "true", "enable-drive": "true", "drive-path": "/data/guac/drives/user1",
		"create-drive-path": "true", "width": "1600", "height": "900",
		"disable-audio": "", "disable-copy": "true", "disable-paste": "true", "gateway-hostname": "gw.example",
		"gateway-port": "8443", "gateway-password": "gwpw", "gateway-username": "alice", "gateway-domain": "CORP",
		"preconnection-blob": "vm-1", "preconnection-id": "7",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	if _, ok := p["resize-method"]; ok {
		t.Error("resize-method set although resizing is disabled")
	}
	// NexTerm records guacd sessions itself (guacrecord.go): guacd must not write recordings of its own.
	if _, ok := p["recording-path"]; ok {
		t.Errorf("recording-path %q passed to guacd", p["recording-path"])
	}
	// A data path that is not absolute (or escapes) falls back to the default.
	p = h.guacParams(tk, opts, globalSettings{GuacdDataPath: "../etc"}, "h", 1, false)
	if p["drive-path"] != defaultGuacdDataPath+"/drives/user1" || p["ignore-cert"] != "" {
		t.Errorf("fallback %q %q", p["drive-path"], p["ignore-cert"])
	}
}

func TestRDPFile(t *testing.T) {
	b := string(buildRDPFile(rdpFileParams{Host: "fe80::1", Port: 3390, Username: "bob", Domain: "D",
		Opts: parseOptions(model.Options{"security": "tls", "initialProgram": "calc.exe\r\nevil:s:1", "gatewayHost": "gw",
			"gatewayPort": 443, "enableDrive": true, "ignoreCert": true})}))
	for _, want := range []string{"full address:s:[fe80::1]:3390\r\n", `username:s:D\bob`, "enablecredsspsupport:i:0",
		"alternate shell:s:calc.exeevil:s:1\r\n", "gatewayhostname:s:gw\r\n", "drivestoredirect:s:*", "authentication level:i:0",
		"screen mode id:i:2", "dynamic resolution:i:1"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
	if strings.Count(b, "\n") != strings.Count(b, "\r\n") {
		t.Error("bare LF in file")
	}
	if got := rdpFileName(`a/b:c*?"<>|`, "h"); got != "a_b_c______.rdp" {
		t.Errorf("file name %q", got)
	}
	if got := rdpFileName("  ", "host.example"); got != "host.example.rdp" {
		t.Errorf("file name %q", got)
	}
}

func TestSocketErrorMapping(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{&net.DNSError{Err: "no such host", Name: "x"}, wsaHostNotFound},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, wsaConnRefused},
		{errors.New("ssh: rejected: connect failed (Connection refused)"), wsaConnRefused},
		{context.DeadlineExceeded, wsaTimedOut},
		{errors.New("i/o timeout"), wsaTimedOut},
		{&net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, wsaHostUnreachable},
		{errors.New("something else"), 0},
	} {
		if code, _ := socketError(c.err); code != c.code {
			t.Errorf("socketError(%v) = %d, want %d", c.err, code, c.code)
		}
	}
}

func TestForwarder(t *testing.T) {
	// Echo server standing in for the RDP host.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	env := newTestEnv(t)
	port := ln.Addr().(*net.TCPAddr).Port
	conn := &model.Connection{Protocol: model.ProtoRDP, Host: "127.0.0.1", Port: port, Options: model.Options{}}
	f, err := env.h.startForwarder(context.Background(), "127.0.0.1", &model.User{ID: "u"}, conn, nil, nil, 2, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.Close()
	// Idle forwarders close themselves.
	select {
	case <-f.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("idle forwarder did not close")
	}
	if forwardBindIP("host.docker.internal") != "127.0.0.1" || forwardBindIP("127.0.0.2") != "127.0.0.2" ||
		forwardBindIP("203.0.113.9") != "127.0.0.1" {
		t.Fatal("forwardBindIP")
	}
}

func TestCleanMessageAndHelpers(t *testing.T) {
	if got := cleanMessage("  a\nb  "); got != "a b" {
		t.Fatalf("cleanMessage %q", got)
	}
	if got := clipRunes("héllo", 2); got != "hé" {
		t.Fatalf("clipRunes %q", got)
	}
	if !validHostPort("127.0.0.1:4822") || !validHostPort("[::1]:4822") || validHostPort("host") || validHostPort("h:0") ||
		validHostPort("h:70000") {
		t.Fatal("validHostPort")
	}
	if !validTimezone("Europe/Paris") || validTimezone("../etc/passwd") || validTimezone("") {
		t.Fatal("validTimezone")
	}
	if got := mimetypes([]string{"audio/L16;rate=44100,channels=2", "bad type", "image/png"}); len(got) != 2 {
		t.Fatalf("mimetypes %v", got)
	}
	if u := newUUID(); len(u) != 36 || u[14] != '4' {
		t.Fatalf("uuid %q", u)
	}
}
