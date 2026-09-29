package rdp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
)

func TestTicketPromptsForPasswordAndSavesAfterConnect(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	conn := env.createConnection(admin.user, "win.example", 3389, `CORP\alice`, nil, model.Options{"security": "nla"})
	pa := admin.answerPrompts(func(p model.Prompt) model.PromptResponse {
		return model.PromptResponse{Accept: true, Values: []string{"typed-secret"}, Save: true}
	})
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, nil)
	if tr.Password != "typed-secret" || tr.Username != "alice" || tr.Domain != "CORP" || !tr.EnableCredssp || tr.Security != secNLA {
		t.Fatalf("ticket %+v", tr)
	}
	prompts := pa.seen()
	if len(prompts) != 1 || prompts[0].Kind != model.PromptPassword || len(prompts[0].Fields) != 1 || !prompts[0].AllowSave ||
		!strings.Contains(prompts[0].Message, `CORP\alice`) {
		t.Fatalf("prompts %+v", prompts)
	}
	// Remembered for reconnects: no second prompt.
	tr = admin.ticket(rs.ID, nil)
	if tr.Password != "typed-secret" || len(pa.seen()) != 1 {
		t.Fatalf("second ticket %+v prompts %d", tr, len(pa.seen()))
	}
	// Not saved until the connection succeeded.
	saved, _ := env.d.Store.Connections.Get(context.Background(), conn.ID)
	if len(saved.SecretKeys) != 0 {
		t.Fatalf("saved too early: %v", saved.SecretKeys)
	}
	admin.must("POST", "/api/sessions/"+rs.ID+"/rdp-state", map[string]string{"state": "connected"}, nil)
	saved, _ = env.d.Store.Connections.Get(context.Background(), conn.ID)
	secrets, err := env.d.Vault.OpenJSON(saved.SecretsEnc)
	if err != nil || secrets["password"] != "typed-secret" || strings.Join(saved.SecretKeys, ",") != "password" {
		t.Fatalf("saved %v %v %v", saved.SecretKeys, secrets, err)
	}
}

func TestTicketCancelledPromptFallsBackToLogonScreen(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	conn := env.createConnection(admin.user, "win.example", 0, "alice", nil, model.Options{})
	admin.answerPrompts(func(p model.Prompt) model.PromptResponse { return model.PromptResponse{Accept: false} })
	rs := admin.openSession(conn.ID)
	tr := admin.ticket(rs.ID, nil)
	if tr.EnableCredssp || tr.Password != "" || tr.Port != 3389 {
		t.Fatalf("ticket %+v", tr)
	}
	// With NLA required, cancelling fails the ticket instead.
	conn2 := env.createConnection(admin.user, "win.example", 0, "alice", nil, model.Options{"security": "nla"})
	rs2 := admin.openSession(conn2.ID)
	if st, _ := admin.errorCode("POST", "/api/sessions/"+rs2.ID+"/rdp-ticket", nil); st < 400 {
		t.Fatalf("cancelled NLA prompt: %d", st)
	}
}

func TestTicketEngineAndOptionErrors(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = "" })
	admin := env.setup()
	conn := env.createConnection(admin.user, "h.example", 3389, "", nil, model.Options{"rdpEngine": "guacd"})
	rs := admin.openSession(conn.ID)
	if st, code := admin.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", nil); st != 409 || code != "guacd_unavailable" {
		t.Fatalf("guacd unavailable: %d %s", st, code)
	}
	// The engine can be overridden per attempt.
	if tr := admin.ticket(rs.ID, map[string]string{"engine": "ironrdp"}); tr.Engine != engineIronRDP {
		t.Fatalf("override %+v", tr)
	}
	conn2 := env.createConnection(admin.user, "h.example", 3389, "", nil, model.Options{"security": "rdp"})
	rs2 := admin.openSession(conn2.ID)
	if st, code := admin.errorCode("POST", "/api/sessions/"+rs2.ID+"/rdp-ticket", nil); st != 400 || code != "unsupported_security" {
		t.Fatalf("rdp security: %d %s", st, code)
	}
	// Fixed size wins over the viewer's size; out-of-range sizes are clamped.
	conn3 := env.createConnection(admin.user, "h.example", 3389, "", nil, model.Options{"width": 1600, "height": 900})
	rs3 := admin.openSession(conn3.ID)
	if tr := admin.ticket(rs3.ID, map[string]any{"width": 300, "height": 300}); tr.Width != 1600 || tr.Height != 900 || !tr.FixedSize {
		t.Fatalf("fixed size %+v", tr)
	}
	conn4 := env.createConnection(admin.user, "h.example", 3389, "", nil, model.Options{})
	rs4 := admin.openSession(conn4.ID)
	if tr := admin.ticket(rs4.ID, map[string]any{"width": 99999, "height": 5}); tr.Width != maxDesktop || tr.Height != minDesktop {
		t.Fatalf("clamped %+v", tr)
	}
	// The global default engine applies to connections without rdpEngine.
	if err := env.h.updateGlobalSettings(context.Background(), map[string]any{"defaultEngine": "guacd"}); err != nil {
		t.Fatal(err)
	}
	if st, code := admin.errorCode("POST", "/api/sessions/"+rs4.ID+"/rdp-ticket", nil); st != 409 || code != "guacd_unavailable" {
		t.Fatalf("default engine: %d %s", st, code)
	}
}

func TestStateEndpoint(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	conn := env.createConnection(admin.user, "h.example", 3389, "", nil, model.Options{})
	rs := admin.openSession(conn.ID)
	if st, _ := admin.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-state", map[string]string{"state": "closed"}); st != 400 {
		t.Fatalf("closed via report: %d", st)
	}
	var info model.RuntimeSession
	admin.must("POST", "/api/sessions/"+rs.ID+"/rdp-state", map[string]any{"state": "error", "message": "Logon failure\nbad", "authFailed": true}, &info)
	if info.State != model.StateError || info.StateMessage != "Logon failure bad" {
		t.Fatalf("info %+v", info)
	}
	bob := env.createUser(admin, "bob")
	if st, _ := bob.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-state", map[string]string{"state": "connected"}); st != 404 {
		t.Fatalf("bob: %d", st)
	}
}

func TestRDPFileEndpoints(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	conn := env.createConnection(admin.user, "win.example", 3390, "alice", map[string]string{"password": "never-in-file"},
		model.Options{"domain": "CORP", "console": true, "enableAudio": true, "disableClipboard": true, "width": 1920, "height": 1080})
	st, body, hdr := admin.do("GET", "/api/connections/"+conn.ID+"/rdp-file", nil)
	if st != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/x-rdp") ||
		!strings.Contains(hdr.Get("Content-Disposition"), `filename="rdp win.example.rdp"`) {
		t.Fatalf("file: %d %v", st, hdr)
	}
	text := string(body)
	for _, want := range []string{"full address:s:win.example:3390\r\n", `username:s:CORP\alice`, "domain:s:CORP",
		"administrative session:i:1", "audiomode:i:0", "redirectclipboard:i:0", "desktopwidth:i:1920", "screen mode id:i:1"} {
		if !strings.Contains(text, want) {
			t.Errorf("file lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "never-in-file") {
		t.Fatal("password leaked into the .rdp file")
	}
	rs := admin.openSession(conn.ID)
	if st, body, _ := admin.do("GET", "/api/sessions/"+rs.ID+"/rdp-file", nil); st != 200 || !strings.Contains(string(body), "win.example:3390") {
		t.Fatalf("session file: %d %s", st, body)
	}
	bob := env.createUser(admin, "bob")
	if st, _ := bob.errorCode("GET", "/api/connections/"+conn.ID+"/rdp-file", nil); st != 404 {
		t.Fatalf("bob: %d", st)
	}
}

func TestLaunchNative(t *testing.T) {
	env := newTestEnv(t)
	admin := env.setup()
	conn := env.createConnection(admin.user, "win.example", 3389, "alice", map[string]string{"password": "pw"}, model.Options{"domain": "CORP"})
	var mu sync.Mutex
	var started []*exec.Cmd
	env.h.native.goos = "linux"
	env.h.native.lookPath = func(name string) (string, error) {
		if name == "xfreerdp" {
			return "/usr/bin/xfreerdp", nil
		}
		return "", exec.ErrNotFound
	}
	env.h.native.start = func(cmd *exec.Cmd) error {
		mu.Lock()
		started = append(started, cmd)
		mu.Unlock()
		return nil
	}
	var res launchResult
	admin.must("POST", "/api/connections/"+conn.ID+"/launch-native", nil, &res)
	if !res.PasswordInjected || !strings.Contains(res.Client, "xfreerdp") {
		t.Fatalf("result %+v", res)
	}
	mu.Lock()
	cmd := started[0]
	mu.Unlock()
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "/v:win.example:3389") || !strings.Contains(args, "/u:alice") || !strings.Contains(args, "/d:CORP") ||
		!strings.Contains(args, "/from-stdin:force") || strings.Contains(args, "pw") {
		t.Fatalf("args %q", args)
	}

	// mstsc on Windows: the .rdp file is written to the data dir's tmp folder.
	env.h.native.goos = "windows"
	env.h.native.lookPath = func(name string) (string, error) {
		if name == "mstsc.exe" {
			return `C:\Windows\System32\mstsc.exe`, nil
		}
		return "", exec.ErrNotFound
	}
	admin.must("POST", "/api/connections/"+conn.ID+"/launch-native", nil, &res)
	mu.Lock()
	cmd = started[len(started)-1]
	mu.Unlock()
	file := cmd.Args[len(cmd.Args)-1]
	if filepath.Dir(file) != env.cfg.TmpDir() {
		t.Fatalf("file %s", file)
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), "full address:s:win.example") || strings.Contains(string(data), "pw\r\n") {
		t.Fatalf("rdp file %q %v", data, err)
	}
	if res.PasswordInjected {
		t.Fatal("cmdkey is not available here, the password cannot have been injected")
	}

	// No client → 409; server mode → 403.
	env.h.native.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if st, code := admin.errorCode("POST", "/api/connections/"+conn.ID+"/launch-native", nil); st != 409 || code != "no_native_client" {
		t.Fatalf("no client: %d %s", st, code)
	}
	srvEnv := newTestEnv(t, func(c *config.Config) { c.Mode = config.ModeServer })
	srvAdmin := srvEnv.setup()
	conn2 := srvEnv.createConnection(srvAdmin.user, "win.example", 3389, "alice", nil, model.Options{})
	if st, code := srvAdmin.errorCode("POST", "/api/connections/"+conn2.ID+"/launch-native", nil); st != 403 || code != "desktop_only" {
		t.Fatalf("server mode: %d %s", st, code)
	}
}

func TestGuacdStatusEndpoint(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = "" })
	admin := env.setup()
	var st guacdStatus
	admin.must("GET", "/api/guacd/status", nil, &st)
	if st.Configured || st.Reachable || st.Sidecar == nil || st.DefaultEngine != engineIronRDP {
		t.Fatalf("status %+v", st)
	}
	bob := env.createUser(admin, "bob")
	var bs guacdStatus
	bob.must("GET", "/api/guacd/status", nil, &bs)
	if bs.Sidecar != nil || bs.Address != "" {
		t.Fatalf("non-admin status %+v", bs)
	}
	if status, _ := bob.errorCode("POST", "/api/guacd/sidecar", map[string]string{"action": "start"}); status != 403 {
		t.Fatalf("non-admin sidecar: %d", status)
	}
	if status, _ := admin.errorCode("POST", "/api/guacd/sidecar", map[string]string{"action": "explode"}); status != 400 {
		t.Fatalf("bad action: %d", status)
	}
	// An address configured by the admin takes over; "off" disables guacd.
	if err := env.h.updateGlobalSettings(context.Background(), map[string]any{"guacdAddress": "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	env.h.guacd.invalidate()
	admin.must("GET", "/api/guacd/status", nil, &st)
	if !st.Configured || st.Reachable || st.Address != "127.0.0.1:1" || st.Source != "settings" || st.Error == "" {
		t.Fatalf("configured status %+v", st)
	}
	if err := env.h.updateGlobalSettings(context.Background(), map[string]any{"guacdAddress": "off"}); err != nil {
		t.Fatal(err)
	}
	if env.h.guacd.address(context.Background()) != "" || env.h.guacd.probe(context.Background()) {
		t.Fatal("guacd not disabled")
	}
	if testEnvEnabled() {
		if err := env.h.updateGlobalSettings(context.Background(), map[string]any{"guacdAddress": "127.0.0.1:22822"}); err != nil {
			t.Fatal(err)
		}
		env.h.guacd.invalidate()
		admin.must("GET", "/api/guacd/status", nil, &st)
		if !st.Reachable || st.Version == "" {
			t.Fatalf("testenv guacd %+v", st)
		}
	}
}

// TestSidecar starts, stops and removes a real guacd container through the docker CLI (NEXTERM_TESTENV=1).
func TestSidecar(t *testing.T) {
	if !testEnvEnabled() {
		t.Skip("NEXTERM_TESTENV=1 not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	env := newTestEnv(t)
	sc := env.h.guacd.sidecar
	sc.cfg.Name, sc.cfg.Port = "nexterm-rdp-sidecar-test", 23090 // module port slot 9
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		_ = sc.remove(cctx)
	})
	var stages []string
	emit := func(v any) {
		if p, ok := v.(progress); ok {
			stages = append(stages, p.Stage)
		}
	}
	if err := sc.start(ctx, emit); err != nil {
		t.Fatal(err)
	}
	st := sc.status(ctx)
	if !st.Running || !st.Managed || !st.Active {
		t.Fatalf("status after start %+v (stages %v)", st, stages)
	}
	if addr := env.h.guacd.address(ctx); addr != "127.0.0.1:"+strconv.Itoa(23090) {
		t.Fatalf("address %s", addr)
	}
	if !env.h.guacd.probe(ctx) {
		t.Fatal("sidecar guacd not reachable")
	}
	// Starting again is idempotent.
	if err := sc.start(ctx, func(any) {}); err != nil {
		t.Fatal(err)
	}
	if err := sc.stop(ctx, func(any) {}); err != nil {
		t.Fatal(err)
	}
	if st := sc.status(ctx); st.Running || !st.Exists {
		t.Fatalf("status after stop %+v", st)
	}
}
