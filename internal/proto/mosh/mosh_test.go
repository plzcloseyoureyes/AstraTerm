package mosh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mosh "github.com/unixshells/mosh-go"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// realOutput is what mosh-server 1.4.0 prints on `mosh-server new` (captured from the test container).
const realOutput = "MOSH CONNECT 23011 xkCPKTj0erPOW0scew3EwA\n\nmosh-server (mosh 1.4.0) [build mosh 1.4.0]\nCopyright 2012 Keith Winstein <mosh-devel@mit.edu>\nLicense GPLv3+: GNU GPL version 3 or later <http://gnu.org/licenses/gpl.html>.\nThis is free software: you are free to change and redistribute it.\nThere is NO WARRANTY, to the extent permitted by law.\n\n[mosh-server detached, pid = 27]\n"

func TestParseMoshConnect(t *testing.T) {
	port, key, err := parseMoshConnect(realOutput)
	if err != nil || port != 23011 || key != "xkCPKTj0erPOW0scew3EwA" {
		t.Fatalf("parse = %d %q %v", port, key, err)
	}
	if _, _, err := parseMoshConnect("Some banner\r\nMOSH CONNECT 60001 gjrM8s2v9kR7pQwXyZ0abc\r\n"); err != nil {
		t.Fatalf("CRLF output: %v", err)
	}
	for _, bad := range []string{"", "mosh-server: command not found\n", "MOSH CONNECT 99999 xkCPKTj0erPOW0scew3EwA\n", "MOSH CONNECT 1 short\n"} {
		if _, _, err := parseMoshConnect(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestBuildServerCommand(t *testing.T) {
	base, err := buildServerCommand(model.Options{}, "LANG=en_US.UTF-8")
	if err != nil || base != "mosh-server new -s -c 256 -l LANG=en_US.UTF-8" {
		t.Fatalf("base command = %q, %v", base, err)
	}
	withPorts, err := buildServerCommand(model.Options{"moshPorts": "60000:61000", "moshServer": "/opt/homebrew/bin/mosh-server"}, "LC_ALL=C.UTF-8")
	if err != nil || withPorts != "/opt/homebrew/bin/mosh-server new -s -c 256 -l LC_ALL=C.UTF-8 -p 60000:61000" {
		t.Fatalf("command = %q, %v", withPorts, err)
	}
	// Environment and a remote command are quoted for the remote shell.
	withCmd, err := buildServerCommand(model.Options{"env": map[string]any{"B": "x'y", "A": "1 2"}, "remoteCommand": "tmux new -A -s main; echo 'done'"}, "LANG=en_US.UTF-8")
	want := `env 'A=1 2' 'B=x'\''y' mosh-server new -s -c 256 -l LANG=en_US.UTF-8 -- /bin/sh -c 'tmux new -A -s main; echo '\''done'\'''`
	if err != nil || withCmd != want {
		t.Fatalf("command = %s (%v)\nwant      %s", withCmd, err, want)
	}
	if _, err := buildServerCommand(model.Options{"env": map[string]any{"BAD NAME": "x"}}, "LANG=en_US.UTF-8"); err == nil {
		t.Error("invalid env name accepted")
	}
	// Anything that could reach the remote shell unchecked is refused.
	for _, o := range []model.Options{
		{"moshPorts": "99999:1; rm -rf /"},
		{"moshPorts": "61000:60000"},
		{"moshServer": "mosh-server; id"},
		{"moshServer": "$(id)"},
		{"moshServer": "a b"},
	} {
		if cmd, err := buildServerCommand(o, "LANG=en_US.UTF-8"); err == nil {
			t.Errorf("options %v accepted: %q", o, cmd)
		}
	}
}

func TestValidPortRange(t *testing.T) {
	cases := map[string]bool{"60000": true, "60000:61000": true, "1:65535": true, "0": false, "70000": false, "61000:60000": false, "abc": false, "60000:": false}
	for in, want := range cases {
		if got := validPortRange(in); got != want {
			t.Errorf("validPortRange(%q) = %v, want %v", in, got, want)
		}
	}
}

type fakeExec struct {
	calls   []string
	respond func(cmd string) (string, string, int)
}

func (f *fakeExec) Exec(_ context.Context, cmd string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, cmd)
	out, errOut, code := f.respond(cmd)
	return []byte(out), []byte(errOut), code, nil
}

func TestBootstrapLocaleFallback(t *testing.T) {
	ex := &fakeExec{respond: func(cmd string) (string, string, int) {
		if strings.Contains(cmd, "LANG=en_US.UTF-8") {
			return "", "mosh-server needs a UTF-8 native locale to run.\n\nUnfortunately, the local environment ([no charset variables]) specifies\nthe character set \"US-ASCII\",\n", 1
		}
		return realOutput, "", 0
	}}
	port, key, err := bootstrap(context.Background(), ex, model.Options{})
	if err != nil || port != 23011 || key == "" {
		t.Fatalf("bootstrap = %d %q %v", port, key, err)
	}
	if len(ex.calls) != 2 || !strings.Contains(ex.calls[1], "LC_ALL=C.UTF-8") {
		t.Fatalf("calls = %q", ex.calls)
	}
}

func TestBootstrapErrors(t *testing.T) {
	notInstalled := &fakeExec{respond: func(string) (string, string, int) { return "", "bash: mosh-server: command not found\n", 127 }}
	if _, _, err := bootstrap(context.Background(), notInstalled, model.Options{}); err == nil || !term.IsPermanent(err) || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("not installed: %v", err)
	}
	failing := &fakeExec{respond: func(string) (string, string, int) { return "", "mosh-server: bind: Address already in use\n", 1 }}
	if _, _, err := bootstrap(context.Background(), failing, model.Options{}); err == nil || !strings.Contains(err.Error(), "Address already in use") {
		t.Fatalf("server error: %v", err)
	}
	bad := &fakeExec{respond: func(string) (string, string, int) { return realOutput, "", 0 }}
	if _, _, err := bootstrap(context.Background(), bad, model.Options{"moshPorts": "x"}); err == nil || !term.IsPermanent(err) {
		t.Fatalf("invalid ports: %v", err)
	}
}

// udpListener returns a UDP socket for a fake server and a client socket connected to it.
func udpListener(t *testing.T) (srv *net.UDPConn, cl *net.UDPConn) {
	t.Helper()
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	cl, err = net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); cl.Close() })
	return srv, cl
}

// TestSSPShutdownOnClose: Close sends the shutdown state (new_num 2^64-1), which the library's server-side parser
// (the same wire format as mosh-server) accepts.
func TestSSPShutdownOnClose(t *testing.T) {
	const key = "xkCPKTj0erPOW0scew3EwA"
	srv, cl := udpListener(t)
	ocb, _ := newOCB(key)
	c := newSSPClient(cl, ocb, 80, 24)
	time.Sleep(30 * time.Millisecond)
	c.Close()

	srvOCB, _ := newOCB(key)
	tr := mosh.NewTransport(srvOCB, true)
	buf := make([]byte, 65536)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	for tr.LastRecvNewNum() != math.MaxUint64 {
		n, err := srv.Read(buf)
		if err != nil {
			t.Fatalf("no shutdown datagram (last new_num %d): %v", tr.LastRecvNewNum(), err)
		}
		tr.Recv(append([]byte(nil), buf[:n]...))
	}
	// Under another key nothing is accepted.
	other, _ := newOCB("AAAAAAAAAAAAAAAAAAAAAA")
	tr2 := mosh.NewTransport(other, true)
	c2 := newSSPClient(cl, ocb, 80, 24)
	c2.Close()
	_ = srv.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		n, err := srv.Read(buf)
		if err != nil {
			break
		}
		tr2.Recv(append([]byte(nil), buf[:n]...))
	}
	if tr2.LastRecvNewNum() != 0 {
		t.Fatal("datagram accepted under a different key")
	}
}

// echoRW is the "terminal" of an in-process mosh-go server: what the client types is echoed back as output.
type echoRW struct {
	ch   chan []byte
	once sync.Once
	done chan struct{}
}

func newEchoRW() *echoRW { return &echoRW{ch: make(chan []byte, 64), done: make(chan struct{})} }
func (e *echoRW) Read(p []byte) (int, error) {
	select {
	case b := <-e.ch:
		return copy(p, b), nil
	case <-e.done:
		return 0, io.EOF
	}
}
func (e *echoRW) Write(p []byte) (int, error) {
	select {
	case e.ch <- append([]byte(nil), p...):
	case <-e.done:
	}
	return len(p), nil
}
func (e *echoRW) Close() error { e.once.Do(func() { close(e.done) }); return nil }

// TestSSPInteropWithGoServer runs the client against mosh-go's server (wire compatible with mosh-server) in process.
func TestSSPInteropWithGoServer(t *testing.T) {
	srv, err := mosh.NewServer("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var size [2]uint16
	rw := newEchoRW()
	go func() {
		_ = srv.ServeRW(rw, func(cols, rows uint16) {
			mu.Lock()
			size = [2]uint16{cols, rows}
			mu.Unlock()
		})
	}()
	// mosh-go's Server.Close panics in ServeRW mode (v0.5.2); ending the "terminal" is enough for a test.
	defer rw.Close()

	b, err := dialBuiltin(context.Background(), nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: srv.Port()}, srv.KeyBase64(), 100, 30)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer b.Close()
	if _, err := b.Write([]byte("hello mosh")); err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	for !bytes.Contains(out, []byte("hello mosh")) && time.Now().Before(deadline) {
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		out = append(out, buf[:n]...)
	}
	if !bytes.Contains(out, []byte("hello mosh")) {
		t.Fatalf("echo not rendered: %q", out)
	}
	mu.Lock()
	got := size
	mu.Unlock()
	if got != [2]uint16{100, 30} {
		t.Fatalf("server saw size %v, want 100x30", got)
	}
}

func TestSSPHostStateRules(t *testing.T) {
	_, cl := udpListener(t)
	ocb, _ := newOCB("xkCPKTj0erPOW0scew3EwA")
	c := newSSPClient(cl, ocb, 80, 24)
	defer c.Close()
	host := func(s string) []byte {
		return mosh.MarshalHostMessage([]mosh.HostInstruction{{Hoststring: []byte(s), EchoAckNum: -1}})
	}

	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, OldNum: 0, NewNum: 1, Diff: host("A")})
	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, OldNum: 2, NewNum: 3, Diff: host("C")}) // base not on screen
	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, OldNum: 0, NewNum: 2, Diff: host("X")}) // base older than screen
	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, OldNum: 1, NewNum: 2, Diff: host("B")})
	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, OldNum: 1, NewNum: 2, Diff: host("B")}) // duplicate
	c.mu.Lock()
	out, recv, force := string(c.out), c.recvNum, c.forceAck
	c.mu.Unlock()
	if out != "AB" || recv != 2 || !force {
		t.Fatalf("out=%q recv=%d forceAck=%v", out, recv, force)
	}
	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, OldNum: 2, NewNum: math.MaxUint64, Diff: host("bye")})
	if !c.Ended() {
		t.Fatal("shutdown state not detected")
	}
	buf := make([]byte, 16)
	n, _ := c.Read(buf)
	if string(buf[:n]) != "ABbye" {
		t.Fatalf("read %q", buf[:n])
	}
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("after shutdown: %v, want EOF", err)
	}
}

// TestSSPAckAdvancesBase: acknowledged input leaves the send buffer, and later states use the acknowledged state as
// diff base and throwaway number (the fix for the 1024-state server quench).
func TestSSPAckAdvancesBase(t *testing.T) {
	_, cl := udpListener(t)
	ocb, _ := newOCB("xkCPKTj0erPOW0scew3EwA")
	c := newSSPClient(cl, ocb, 80, 24)
	defer c.Close()
	_ = c.Send([]byte("ls"))
	now := time.Now().Add(time.Hour) // the tick loop is also running; drive decisions explicitly
	c.tick(now)
	c.mu.Lock()
	newest := c.sent[len(c.sent)-1].num
	c.mu.Unlock()
	c.apply(&mosh.TransportInstruction{ProtocolVersion: 2, AckNum: newest})
	c.mu.Lock()
	base, pending, keys := c.baseNum, len(c.actions), c.keyBytes
	c.mu.Unlock()
	if base != newest || pending != 0 || keys != 0 {
		t.Fatalf("base=%d (want %d) pending actions=%d keyBytes=%d", base, newest, pending, keys)
	}
	_ = c.Send([]byte("x"))
	dgs := c.tick(now.Add(time.Second))
	if len(dgs) == 0 {
		t.Fatal("no datagram for new input")
	}
	srvOCB, _ := newOCB("xkCPKTj0erPOW0scew3EwA")
	var nonce [12]byte
	copy(nonce[4:], dgs[0][:8])
	plain := srvOCB.Decrypt(nonce[:], dgs[0][8:])
	frag, err := mosh.UnmarshalFragment(plain[4:])
	if err != nil {
		t.Fatal(err)
	}
	data, err := inflate(frag.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var ti mosh.TransportInstruction
	if err := ti.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	if ti.OldNum != newest || ti.ThrowawayNum != newest || ti.NewNum <= newest {
		t.Fatalf("instruction old=%d throwaway=%d new=%d, want old=throwaway=%d", ti.OldNum, ti.ThrowawayNum, ti.NewNum, newest)
	}
	instrs, _ := mosh.UnmarshalHostMessage(nil)
	_ = instrs
}

func TestUDPTargetPrefersSSHAddress(t *testing.T) {
	ip, err := udpTarget(context.Background(), "example.invalid", fakeAddr("192.0.2.7:22"))
	if err != nil || ip.String() != "192.0.2.7" {
		t.Fatalf("got %v %v", ip, err)
	}
	ip, err = udpTarget(context.Background(), "2001:db8::1", nil)
	if err != nil || ip.String() != "2001:db8::1" {
		t.Fatalf("IPv6 literal: %v %v", ip, err)
	}
	if _, err := udpTarget(context.Background(), "nexterm-no-such-host.invalid", nil); err == nil {
		t.Fatal("unresolvable host accepted")
	}
}

func TestOpenValidation(t *testing.T) {
	_, err := open(context.Background(), nil, nil, term.OpenRequest{Connection: &model.Connection{Protocol: model.ProtoMosh}})
	if err == nil || !term.IsPermanent(err) {
		t.Fatalf("missing host: %v", err)
	}
	if !errors.Is(errLocale, errLocale) {
		t.Fatal("sanity")
	}
}
