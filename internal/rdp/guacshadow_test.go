package rdp

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/rdp/guac"
)

// TestGuacShadowAndRecording: an administrator joins a user's guacd session read-only, and the session (options
// recording) is recorded by AstraTerm.
func TestGuacShadowAndRecording(t *testing.T) {
	var (
		mu         sync.Mutex
		ownerIn    []guac.Instruction
		shadowIn   []guac.Instruction
		ownerConns = map[net.Conn]bool{}
	)
	display := func(c net.Conn) {
		var b []byte
		for _, in := range []guac.Instruction{
			guac.New("size", "0", "800", "600"),
			guac.New("clipboard", "3", "text/plain"), // the owner gets it (clipboard allowed); never recorded
			guac.New("blob", "3", "c2VjcmV0"),
			guac.New("end", "3"),
			guac.New("rect", "0", "0", "0", "10", "10"),
			guac.New("sync", "100", "0"),
		} {
			b = in.Append(b, guac.CodePoints)
		}
		_, _ = c.Write(b)
	}
	collect := func(c net.Conn, r *guac.Reader, dst *[]guac.Instruction) {
		for {
			in, err := r.Read()
			if err != nil {
				return
			}
			mu.Lock()
			*dst = append(*dst, in)
			mu.Unlock()
			if in.Opcode == "disconnect" {
				return
			}
		}
	}
	gd := newFakeGuacd(t, func(c net.Conn, r *guac.Reader, f *fakeGuacd) {
		mu.Lock()
		ownerConns[c] = true
		mu.Unlock()
		display(c)
		collect(c, r, &ownerIn)
	})
	gd.joinScript = func(c net.Conn, r *guac.Reader, f *fakeGuacd) {
		display(c)
		collect(c, r, &shadowIn)
	}
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = gd.ln.Addr().String() })
	admin := env.setup()
	bob := env.createUser(admin, "bob")
	conn := env.createConnection(bob.user, "10.0.0.5", 3389, "bob", map[string]string{"password": "pw"},
		model.Options{"rdpEngine": "guacd", "ignoreCert": true, "recording": true})
	rs := bob.openSession(conn.ID)

	// Nothing to join before the owner is connected.
	if st, code := admin.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", map[string]any{"shadow": true}); st != 409 || code != "shadow_unavailable" {
		t.Fatalf("shadow before connect: %d %s", st, code)
	}

	tr := bob.ticket(rs.ID, map[string]any{"width": 800, "height": 600})
	if !tr.Recording || tr.ReadOnly {
		t.Fatalf("owner ticket %+v", tr)
	}
	owner := bob.browserTunnel(t, rs.ID, tr.Token)
	defer owner.CloseNow()
	ins, _ := readInstructions(t, owner, 10*time.Second, func(in guac.Instruction) bool { return in.Opcode == "sync" })
	if !strings.Contains(strings.Join(opcodes(ins), ","), "clipboard") {
		t.Fatalf("owner instructions %v", opcodes(ins))
	}
	waitFor(t, "join registered", func() bool { _, ok := env.h.joins.get(rs.ID); return ok })

	// Regular users can neither view other users' sessions nor ask for shadow tickets.
	carol := env.createUser(admin, "carol")
	if st, _ := carol.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", map[string]any{"shadow": true}); st != 404 {
		t.Fatalf("carol shadow: %d", st)
	}
	// Without "shadow" an administrator gets nothing either (as before).
	if st, _ := admin.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", nil); st != 404 {
		t.Fatalf("admin plain ticket: %d", st)
	}

	st := admin.ticket(rs.ID, map[string]any{"shadow": true})
	if st.Engine != engineGuacd || !st.ReadOnly || st.Password != "" || st.Width != 800 || st.Height != 600 {
		t.Fatalf("shadow ticket %+v", st)
	}
	// The shadow ticket is bound to the administrator: the owner cannot use it.
	if ws := bob.browserTunnel(t, rs.ID, st.Token); ws != nil {
		ins, _ := readInstructions(t, ws, 10*time.Second, func(in guac.Instruction) bool { return in.Opcode == "error" })
		ws.CloseNow()
		if ins[len(ins)-1].Arg(1) != "769" {
			t.Fatalf("owner with the shadow ticket: %+v", ins[len(ins)-1])
		}
	}
	st = admin.ticket(rs.ID, map[string]any{"shadow": true})
	view := admin.browserTunnel(t, rs.ID, st.Token)
	defer view.CloseNow()
	ins, _ = readInstructions(t, view, 10*time.Second, func(in guac.Instruction) bool { return in.Opcode == "sync" })
	for _, in := range ins {
		if in.Opcode == "clipboard" || in.Opcode == "blob" || in.Opcode == "end" {
			t.Fatalf("the owner's clipboard reached the shadow view: %v", opcodes(ins))
		}
	}
	gd.mu.Lock()
	joinConnect := gd.joined["$fake-connection"]
	gd.mu.Unlock()
	if joinConnect["read-only"] != "true" || joinConnect["password"] != "" || joinConnect["hostname"] != "" {
		t.Fatalf("join connect values %v", joinConnect)
	}
	// Input from the view is dropped; sync goes through.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = view.Write(ctx, websocket.MessageText, []byte("3.key,2.65,1.1;5.mouse,1.5,1.5,1.1;4.size,3.640,3.480;4.sync,3.101;"))
	waitFor(t, "view sync", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, in := range shadowIn {
			if in.Opcode == "sync" && in.Arg(0) == "101" {
				return true
			}
		}
		return false
	})
	mu.Lock()
	for _, in := range shadowIn {
		if in.Opcode == "key" || in.Opcode == "mouse" || in.Opcode == "size" {
			t.Fatalf("read-only view sent %q", in.Opcode)
		}
	}
	mu.Unlock()
	// The owner's mouse movements are recorded.
	_ = owner.Write(ctx, websocket.MessageText, []byte("5.mouse,2.12,2.34,1.0;"))
	waitFor(t, "owner mouse", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, in := range ownerIn {
			if in.Opcode == "mouse" {
				return true
			}
		}
		return false
	})

	// The view leaving does not touch the session; the owner leaving ends it although the view is still attached.
	view2 := admin.ticket(rs.ID, map[string]any{"shadow": true})
	view.CloseNow()
	if s, _ := env.sessionState(rs.ID); s != model.StateConnected {
		t.Fatalf("state after the view left: %s", s)
	}
	v2 := admin.browserTunnel(t, rs.ID, view2.Token)
	defer v2.CloseNow()
	readInstructions(t, v2, 10*time.Second, func(in guac.Instruction) bool { return in.Opcode == "sync" })
	_ = owner.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "owner disconnected", func() bool { s, _ := env.sessionState(rs.ID); return s == model.StateDisconnected })
	waitFor(t, "join forgotten", func() bool { _, ok := env.h.joins.get(rs.ID); return !ok })

	// The recording: a guac file with the display stream and the owner's mouse, without the clipboard.
	var rec *rdpRecording
	recs, _ := env.h.recordings()
	deadline := time.Now().Add(20 * time.Second)
	for rec == nil {
		list, err := recs.listBySession(context.Background(), rs.ID)
		if len(list) == 1 && list[0].EndedAt != nil {
			rec = list[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording rows %d (%v): %+v", len(list), err, list)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec.Kind != recordingKindGuac || rec.OwnerID != bob.user.ID || rec.ConnectionID != conn.ID || rec.Size == 0 ||
		rec.Width != 800 || rec.Height != 600 {
		t.Fatalf("recording %+v", rec)
	}
	data, err := os.ReadFile(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(rec.Path); fi.Mode().Perm() != 0o600 || int64(len(data)) != rec.Size {
		t.Fatalf("recording file %v, %d bytes (row %d)", fi.Mode(), len(data), rec.Size)
	}
	recorded, err := guac.ParseAll(string(data), guac.CodePoints)
	if err != nil {
		t.Fatalf("recording parse: %v", err)
	}
	got := strings.Join(opcodes(recorded), ",")
	if !strings.Contains(got, "size") || !strings.Contains(got, "rect") || !strings.Contains(got, "sync") ||
		!strings.Contains(got, "mouse") || strings.Contains(got, "clipboard") || strings.Contains(got, "blob") {
		t.Fatalf("recorded %s", got)
	}

	// The recordings API: owners see theirs, administrators everybody's with ?all=1, others nothing.
	var list []rdpRecording
	bob.must("GET", "/api/rdp/recordings", nil, &list)
	if len(list) != 1 || list[0].ID != rec.ID || list[0].Kind != "guac" || list[0].EndedAt == nil {
		t.Fatalf("bob's recordings %+v", list)
	}
	admin.must("GET", "/api/rdp/recordings", nil, &list)
	if len(list) != 0 {
		t.Fatalf("admin's own recordings %+v", list)
	}
	admin.must("GET", "/api/rdp/recordings?all=1", nil, &list)
	if len(list) != 1 {
		t.Fatalf("all recordings %+v", list)
	}
	carol.must("GET", "/api/rdp/recordings?all=1", nil, &list)
	if len(list) != 0 {
		t.Fatalf("carol sees %+v", list)
	}
	if st, _, _ := carol.do("GET", "/api/rdp/recordings/"+rec.ID+"/file", nil); st != 404 {
		t.Fatalf("carol file: %d", st)
	}
	st2, body, hdr := bob.do("GET", "/api/rdp/recordings/"+rec.ID+"/file", nil)
	if st2 != 200 || string(body) != string(data) || !strings.Contains(hdr.Get("Content-Disposition"), ".guac") {
		t.Fatalf("file: %d %q (%d bytes)", st2, hdr.Get("Content-Disposition"), len(body))
	}
	if st, _, _ := carol.do("DELETE", "/api/rdp/recordings/"+rec.ID, nil); st != 404 {
		t.Fatalf("carol delete: %d", st)
	}
	bob.must("DELETE", "/api/rdp/recordings/"+rec.ID, nil, nil)
	if _, err := os.Stat(rec.Path); !os.IsNotExist(err) {
		t.Fatalf("recording file left behind: %v", err)
	}
	bob.must("GET", "/api/rdp/recordings", nil, &list)
	if len(list) != 0 {
		t.Fatalf("after delete %+v", list)
	}
}

// TestShadowIronRDPUnavailable: IronRDP sessions run in their owner's browser: nothing to join.
func TestShadowIronRDPUnavailable(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	bob := env.createUser(admin, "bob")
	conn := env.createConnection(bob.user, "10.0.0.5", 3389, "bob", nil, model.Options{})
	rs := bob.openSession(conn.ID)
	if st, code := admin.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", map[string]any{"shadow": true}); st != 409 || code != "shadow_unavailable" {
		t.Fatalf("shadow of an IronRDP session: %d %s", st, code)
	}
	if _, resp, err := admin.ws("/ws/rdp/" + rs.ID); err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("admin on the IronRDP relay: %v", err)
	}
}

// TestGuacShadowTestEnv joins a real guacd 1.6 connection (shared test environment, ASTRATERM_TESTENV=1) read-only as
// an administrator while its owner drives it, with the session recorded by AstraTerm.
func TestGuacShadowTestEnv(t *testing.T) {
	if !testEnvEnabled() {
		t.Skip("ASTRATERM_TESTENV=1 not set")
	}
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = "127.0.0.1:22822" })
	admin := env.setup()
	bob := env.createUser(admin, "bob")
	conn := env.createConnection(bob.user, "rdp", 3389, "ubuntu", map[string]string{"password": "ubuntu"},
		model.Options{"rdpEngine": "guacd", "ignoreCert": true, "security": "any", "recording": true})
	bob.answerPrompts(func(p model.Prompt) model.PromptResponse { return model.PromptResponse{Accept: true} })
	rs := bob.openSession(conn.ID)
	tr := bob.ticket(rs.ID, map[string]any{"width": 1024, "height": 768})
	owner := bob.browserTunnel(t, rs.ID, tr.Token)
	defer owner.CloseNow()
	readInstructions(t, owner, 45*time.Second, func(in guac.Instruction) bool { return in.Opcode == "sync" })
	// The owner keeps answering syncs in the background (guacd drops silent clients).
	go func() {
		for {
			_, data, err := owner.Read(context.Background())
			if err != nil {
				return
			}
			ins, _ := guac.ParseAll(string(data), guac.UTF16Units)
			for _, in := range ins {
				if in.Opcode == "sync" {
					_ = owner.Write(context.Background(), websocket.MessageText, guac.New("sync", in.Arg(0)).Encode(guac.UTF16Units))
				}
			}
		}
	}()
	waitFor(t, "join registered", func() bool { _, ok := env.h.joins.get(rs.ID); return ok })

	st := admin.ticket(rs.ID, map[string]any{"shadow": true})
	view := admin.browserTunnel(t, rs.ID, st.Token)
	defer view.CloseNow()
	seen := map[string]int{}
	ins, _ := readInstructions(t, view, 45*time.Second, func(in guac.Instruction) bool {
		seen[in.Opcode]++
		return in.Opcode == "error" || (seen["sync"] >= 2 && seen["size"] > 0 && (seen["img"] > 0 || seen["rect"] > 0))
	})
	if seen["error"] > 0 {
		t.Fatalf("guacd refused the join: %+v", ins[len(ins)-1])
	}
	t.Logf("read-only view received %d instructions: %v", len(ins), seen)
	view.CloseNow()
	_ = owner.Close(websocket.StatusNormalClosure, "")
	recs, _ := env.h.recordings()
	waitFor(t, "recording completed", func() bool {
		list, _ := recs.listBySession(context.Background(), rs.ID)
		return len(list) == 1 && list[0].EndedAt != nil && list[0].Size > 0
	})
	list, _ := recs.listBySession(context.Background(), rs.ID)
	t.Logf("recording %s: %d bytes, %dx%d", list[0].ID, list[0].Size, list[0].Width, list[0].Height)
}
