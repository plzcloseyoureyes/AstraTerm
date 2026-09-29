package rdp

import (
	"context"
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/rdp/guac"
)

// fakeGuacd is a scripted guacd.
type fakeGuacd struct {
	ln      net.Listener
	mu      sync.Mutex
	connect map[string]string // parameter values received in "connect"
	size    []string
	fromCli []guac.Instruction // instructions received after "ready"
	argv    map[string]string
	script  func(c net.Conn, r *guac.Reader, f *fakeGuacd)
	// joined: per "select" argument ("rdp" or "$connection"), the connect values and later client instructions.
	joined     map[string]map[string]string
	joinScript func(c net.Conn, r *guac.Reader, f *fakeGuacd) // serves joining users (select $id) when set
	accepted   atomic.Int32                                   // connections Termstead opened to this guacd
}

var fakeArgs = []string{"hostname", "port", "username", "password", "domain", "security", "ignore-cert", "disable-copy",
	"disable-paste", "enable-drive", "drive-path", "create-drive-path", "drive-name", "resize-method", "client-name",
	"read-only"}

func newFakeGuacd(t *testing.T, script func(c net.Conn, r *guac.Reader, f *fakeGuacd)) *fakeGuacd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGuacd{ln: ln, connect: map[string]string{}, argv: map[string]string{}, script: script,
		joined: map[string]map[string]string{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeGuacd) serve(c net.Conn) {
	defer c.Close()
	r := guac.NewReader(c, guac.CodePoints)
	in, err := r.Read()
	if err != nil || in.Opcode != "select" {
		return
	}
	sel := in.Arg(0)
	_, _ = c.Write(guac.New("args", append([]string{guac.Version150}, fakeArgs...)...).Encode(guac.CodePoints))
	for {
		in, err := r.Read()
		if err != nil {
			return
		}
		if in.Opcode == "size" {
			f.mu.Lock()
			f.size = in.Args
			f.mu.Unlock()
		}
		if in.Opcode == "connect" {
			f.mu.Lock()
			values := map[string]string{}
			for i, name := range fakeArgs {
				values[name] = in.Arg(i + 1)
			}
			f.joined[sel] = values
			if !strings.HasPrefix(sel, "$") {
				f.connect = values
			}
			f.mu.Unlock()
			break
		}
	}
	_, _ = c.Write(guac.New("ready", "$fake-connection").Encode(guac.CodePoints))
	if strings.HasPrefix(sel, "$") && f.joinScript != nil {
		f.joinScript(c, r, f)
		return
	}
	f.script(c, r, f)
}

func (f *fakeGuacd) received() []guac.Instruction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]guac.Instruction(nil), f.fromCli...)
}

// readArgv reads argv streams until one "end" per requested name, recording values.
func (f *fakeGuacd) readArgv(r *guac.Reader, n int) {
	names := map[string]string{}
	values := map[string]string{}
	for ended := 0; ended < n; {
		in, err := r.Read()
		if err != nil {
			return
		}
		switch in.Opcode {
		case "argv":
			names[in.Arg(0)] = in.Arg(2)
		case "blob":
			b, _ := base64.StdEncoding.DecodeString(in.Arg(1))
			values[in.Arg(0)] += string(b)
		case "end":
			f.mu.Lock()
			f.argv[names[in.Arg(0)]] = values[in.Arg(0)]
			f.mu.Unlock()
			ended++
		}
	}
}

// browserTunnel opens /ws/guac as guacamole-common-js does and returns the socket.
func (c *testClient) browserTunnel(t *testing.T, sessionID, token string) *websocket.Conn {
	t.Helper()
	q := url.Values{"token": {token}, "width": {"1024"}, "height": {"768"}, "dpi": {"96"},
		"audio": {"audio/L16"}, "image": {"image/png", "image/jpeg"}, "timezone": {"Europe/Paris"}}
	ws, resp, err := c.ws("/ws/guac/"+sessionID+"?"+q.Encode(), "guacamole")
	if err != nil {
		t.Fatalf("dial tunnel: %v (%v)", err, resp)
	}
	if ws.Subprotocol() != "guacamole" {
		t.Fatalf("subprotocol %q not echoed", ws.Subprotocol())
	}
	return ws
}

// readInstructions reads browser frames until stop returns true, parsing them with UTF-16 lengths and answering
// every "sync" like guacamole-common-js does (guacd drops clients that stop answering).
func readInstructions(t *testing.T, ws *websocket.Conn, timeout time.Duration, stop func(guac.Instruction) bool) ([]guac.Instruction, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var all []guac.Instruction
	var frames []string
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("tunnel read: %v (got %d instructions: %v)", err, len(all), opcodes(all))
		}
		frames = append(frames, string(data))
		ins, err := guac.ParseAll(string(data), guac.UTF16Units)
		if err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		for _, in := range ins {
			all = append(all, in)
			if in.Opcode == "sync" {
				_ = ws.Write(ctx, websocket.MessageText, guac.New("sync", in.Arg(0)).Encode(guac.UTF16Units))
			}
			if stop(in) {
				return all, frames
			}
		}
	}
}

func opcodes(ins []guac.Instruction) []string {
	var out []string
	for _, in := range ins {
		out = append(out, in.Opcode)
	}
	return out
}

func TestGuacTunnelFakeGuacd(t *testing.T) {
	gd := newFakeGuacd(t, func(c net.Conn, r *guac.Reader, f *fakeGuacd) {
		// guacd needs the password it was not given.
		_, _ = c.Write(guac.New("required", "password").Encode(guac.CodePoints))
		f.readArgv(r, 1)
		out := []guac.Instruction{
			guac.New("ack", "56", "OK", "0"), // for the tunnel's own argv stream: never reaches the browser
			guac.New("size", "0", "1024", "768"),
			guac.New("clipboard", "5", "text/plain"), // dropped: clipboard disabled
			guac.New("blob", "5", "SGVsbG8="),
			guac.New("end", "5"),
			guac.New("file", "6", "application/pdf", "report 😀.pdf"),
			guac.New("sync", "1000", "0"),
		}
		var b []byte
		for _, in := range out {
			b = in.Append(b, guac.CodePoints)
		}
		_, _ = c.Write(b)
		for {
			in, err := r.Read()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.fromCli = append(f.fromCli, in)
			f.mu.Unlock()
			if in.Opcode == "disconnect" {
				return
			}
		}
	})
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = gd.ln.Addr().String() })
	admin := env.setup()
	conn := env.createConnection(admin.user, "127.0.0.1", 3389, "alice", nil,
		model.Options{"rdpEngine": "guacd", "ignoreCert": true, "disableClipboard": true, "enableDrive": true})
	pa := admin.answerPrompts(func(p model.Prompt) model.PromptResponse {
		return model.PromptResponse{Accept: true, Values: []string{"prompted-pw"}, Save: true}
	})
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, map[string]any{"width": 1024, "height": 768})
	if tr.Engine != engineGuacd || tr.Password != "" || tr.EnableCredssp {
		t.Fatalf("guacd ticket %+v", tr)
	}
	ws := admin.browserTunnel(t, rs.ID, tr.Token)
	defer ws.CloseNow()

	// The internal-opcode UUID comes first.
	first, _ := readInstructions(t, ws, 10*time.Second, func(guac.Instruction) bool { return true })
	if first[0].Opcode != guac.InternalOpcode || len(first[0].Arg(0)) != 36 {
		t.Fatalf("first instruction %+v", first[0])
	}
	// Pings are echoed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte("0.,4.ping,13.1700000000000;")); err != nil {
		t.Fatal(err)
	}
	ins, frames := readInstructions(t, ws, 20*time.Second, func(in guac.Instruction) bool { return in.Opcode == "sync" })
	sawPong := false
	for _, in := range ins {
		switch in.Opcode {
		case guac.InternalOpcode:
			sawPong = sawPong || (in.Arg(0) == "ping" && in.Arg(1) == "1700000000000")
		case "clipboard", "blob", "end", "required", "ack":
			t.Fatalf("instruction %q reached the browser", in.Opcode)
		case "file":
			if in.Arg(2) != "report 😀.pdf" {
				t.Fatalf("file name %q", in.Arg(2))
			}
		}
	}
	if !sawPong {
		t.Fatalf("ping not echoed: %v", opcodes(ins))
	}
	if !strings.Contains(strings.Join(frames, ""), "13.report 😀.pdf") {
		t.Fatal("file name not re-encoded with UTF-16 lengths")
	}

	// Handshake values: credentials from the vault / prompt, never from the browser.
	gd.mu.Lock()
	cp := map[string]string{}
	for k, v := range gd.connect {
		cp[k] = v
	}
	argv := gd.argv["password"]
	size := strings.Join(gd.size, "x")
	gd.mu.Unlock()
	if cp["hostname"] != "127.0.0.1" || cp["port"] != "3389" || cp["username"] != "alice" || cp["password"] != "" ||
		cp["ignore-cert"] != "true" || cp["disable-copy"] != "true" || cp["disable-paste"] != "true" ||
		cp["enable-drive"] != "true" || cp["drive-path"] != "/tmp/termstead/drives/"+admin.user.ID || cp["security"] != "any" ||
		cp["resize-method"] != "display-update" || cp["client-name"] != "Termstead" {
		t.Fatalf("connect params %v", cp)
	}
	if argv != "prompted-pw" || size != "1024x768x96" {
		t.Fatalf("argv %q size %q", argv, size)
	}
	if prompts := pa.seen(); len(prompts) != 1 || prompts[0].Kind != model.PromptPassword || prompts[0].Fields[0].Echo {
		t.Fatalf("prompts %+v", prompts)
	}
	waitFor(t, "connected state", func() bool { st, _ := env.sessionState(rs.ID); return st == model.StateConnected })
	// The prompted password was saved once the desktop drew its first frame.
	waitFor(t, "saved password", func() bool {
		c, _ := env.d.Store.Connections.Get(context.Background(), conn.ID)
		return strings.Join(c.SecretKeys, ",") == "password"
	})

	// Browser instructions: key events pass, clipboard is dropped by policy.
	if err := ws.Write(ctx, websocket.MessageText, []byte("3.key,2.65,1.1;9.clipboard,1.1,10.text/plain;4.blob,1.1,4.SGk=;3.end,1.1;")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "key forwarded", func() bool {
		for _, in := range gd.received() {
			if in.Opcode == "key" && in.Arg(0) == "65" {
				return true
			}
		}
		return false
	})
	_ = ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "disconnect forwarded", func() bool {
		for _, in := range gd.received() {
			if in.Opcode == "disconnect" {
				return true
			}
		}
		return false
	})
	for _, in := range gd.received() {
		if in.Opcode == "clipboard" || in.Opcode == "blob" {
			t.Fatalf("policy-dropped %q reached guacd", in.Opcode)
		}
	}
	waitFor(t, "disconnected state", func() bool {
		st, _ := env.sessionState(rs.ID)
		return st == model.StateDisconnected && env.core.Sessions.Get(rs.ID).Info().Clients == 0
	})
}

func TestGuacTunnelBadTicketAndGuacdDown(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = "127.0.0.1:1" })
	admin := env.setup()
	conn := env.createConnection(admin.user, "127.0.0.1", 3389, "", nil, model.Options{"rdpEngine": "guacd", "ignoreCert": true})
	rs := admin.openSession(conn.ID)
	ws := admin.browserTunnel(t, rs.ID, "not-a-ticket")
	ins, _ := readInstructions(t, ws, 10*time.Second, func(in guac.Instruction) bool { return in.Opcode == "error" })
	last := ins[len(ins)-1]
	if last.Arg(1) != "769" || !strings.Contains(last.Arg(0), "ticket") {
		t.Fatalf("error %+v", last)
	}
	ws.CloseNow()

	tr := admin.ticket(rs.ID, nil)
	ws = admin.browserTunnel(t, rs.ID, tr.Token)
	defer ws.CloseNow()
	ins, _ = readInstructions(t, ws, 10*time.Second, func(in guac.Instruction) bool { return in.Opcode == "error" })
	last = ins[len(ins)-1]
	if last.Arg(1) != "519" || !strings.Contains(last.Arg(0), "guacd") {
		t.Fatalf("guacd down %+v", last)
	}
	if st, msg := env.sessionState(rs.ID); st != model.StateError || !strings.Contains(msg, "guacd") {
		t.Fatalf("state %s %q", st, msg)
	}
}

// TestGuacTunnelTestEnv connects through the shared test environment's guacd to its xrdp (TERMSTEAD_TESTENV=1) and
// expects display instructions.
func TestGuacTunnelTestEnv(t *testing.T) {
	if !testEnvEnabled() {
		t.Skip("TERMSTEAD_TESTENV=1 not set")
	}
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = "127.0.0.1:22822" })
	admin := env.setup()
	// "rdp" is the xrdp container's name on the test network: only guacd can resolve it.
	conn := env.createConnection(admin.user, "rdp", 3389, "ubuntu", map[string]string{"password": "ubuntu"},
		model.Options{"rdpEngine": "guacd", "ignoreCert": true, "security": "any"})
	admin.answerPrompts(func(p model.Prompt) model.PromptResponse { return model.PromptResponse{Accept: true} })
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, map[string]any{"width": 1024, "height": 768})
	ws := admin.browserTunnel(t, rs.ID, tr.Token)
	defer ws.CloseNow()
	seen := map[string]int{}
	ins, _ := readInstructions(t, ws, 45*time.Second, func(in guac.Instruction) bool {
		seen[in.Opcode]++
		if in.Opcode == "error" {
			return true
		}
		return seen["sync"] >= 3 && seen["size"] > 0 && (seen["img"] > 0 || seen["blob"] > 0 || seen["rect"] > 0 || seen["copy"] > 0)
	})
	if seen["error"] > 0 {
		t.Fatalf("guacd error: %+v", ins[len(ins)-1])
	}
	waitFor(t, "connected", func() bool { st, _ := env.sessionState(rs.ID); return st == model.StateConnected })
	t.Logf("received %d instructions: %v", len(ins), seen)
}
