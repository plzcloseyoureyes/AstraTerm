package transfer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
	"github.com/plzcloseyoureyes/astraterm/internal/transfer"
	"github.com/plzcloseyoureyes/astraterm/internal/vfs"
)

// flakyFS fails the first read of every file after half of it with a transient network error.
type flakyFS struct {
	vfs.FS
	failures atomic.Int32
	opens    []int64
	mu       sync.Mutex
}

func (f *flakyFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opens = append(f.opens, offset)
	first := len(f.opens) == 1
	f.mu.Unlock()
	r, err := f.FS.Open(ctx, p, offset)
	if err != nil || !first {
		return r, err
	}
	e, _ := f.FS.Stat(ctx, p)
	return &failingReader{r: r, left: e.Size / 2, f: f}, nil
}

type failingReader struct {
	r    io.ReadCloser
	left int64
	f    *flakyFS
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		r.f.failures.Add(1)
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	return n, err
}

func (r *failingReader) Close() error { return r.r.Close() }

func realTemp(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(d)
}

func TestRetryResumesAfterTransientError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &app.Deps{Ctx: ctx, Log: log, Cfg: &config.Config{Mode: config.ModeDesktop, DataDir: t.TempDir()},
		Events: events.NewHub(ctx, log)}
	reg := vfs.NewRegistry(d, nil)
	user := &model.User{ID: "u1", Username: "alice", Role: model.RoleAdmin}
	local, err := reg.Open(ctx, user, vfs.OpenRequest{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyFS{FS: local.FS}
	src := reg.Register(user, &vfs.Handle{Kind: "local", Driver: "flaky", FS: flaky, Home: "/"})
	m := transfer.New(d, reg)

	srcDir, dstDir := realTemp(t), realTemp(t)
	payload := bytes.Repeat([]byte("resumable "), 300000)
	os.WriteFile(srcDir+"/f.bin", payload, 0o644)
	in, err := m.Create(ctx, user, transfer.Request{SrcFS: src.ID, SrcPaths: []string{srcDir + "/f.bin"}, DstFS: local.ID,
		DstDir: dstDir, Overwrite: "overwrite", Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for in.FinishedAt == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		in, _ = m.Get(user, in.ID)
	}
	if in.State != "done" || in.DoneBytes != int64(len(payload)) {
		t.Fatalf("transfer %+v", in)
	}
	if got, _ := os.ReadFile(dstDir + "/f.bin"); !bytes.Equal(got, payload) {
		t.Fatal("content after retry")
	}
	flaky.mu.Lock()
	opens := append([]int64(nil), flaky.opens...)
	flaky.mu.Unlock()
	if flaky.failures.Load() != 1 || len(opens) < 2 || opens[1] <= 0 {
		t.Fatalf("expected one failure and a resumed second read, opens at %v", opens)
	}
	// Other users see nothing.
	if _, err := m.Get(&model.User{ID: "u2"}, in.ID); err == nil {
		t.Fatal("foreign transfer visible")
	}
}

// answerPrompts connects an events socket for c and answers every confirm prompt with resp; stop disconnects it.
func answerPrompts(t *testing.T, env *servertest.Env, c *servertest.Client, resp map[string]any) (n *atomic.Int32, stop func()) {
	t.Helper()
	u, _ := url.Parse(env.HTTP.URL)
	hdr := http.Header{}
	for _, ck := range c.HTTP.Jar.Cookies(u) {
		hdr.Add("Cookie", ck.Name+"="+ck.Value)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ws, _, err := websocket.Dial(ctx, strings.Replace(env.HTTP.URL, "http", "ws", 1)+"/ws/events", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	stop = func() {
		cancel()
		ws.CloseNow()
	}
	t.Cleanup(stop)
	n = new(atomic.Int32)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				Type   string       `json:"type"`
				Prompt model.Prompt `json:"prompt"`
			}
			if json.Unmarshal(data, &m) != nil || m.Type != "prompt" || m.Prompt.Kind != "confirm" {
				continue
			}
			n.Add(1)
			out := map[string]any{"type": "prompt.response", "id": m.Prompt.ID}
			maps.Copy(out, resp)
			b, _ := json.Marshal(out)
			ws.Write(ctx, websocket.MessageText, b)
		}
	}()
	return n, stop
}

func TestAskApplyToAll(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery staple")
	var h vfs.Handle
	admin.MustJSON("POST", "/api/fs", map[string]any{"local": true}, &h)
	src, dst := realTemp(t), realTemp(t)
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		os.WriteFile(src+"/"+n, []byte("new "+n), 0o644)
		os.WriteFile(dst+"/"+n, []byte("old"), 0o644)
	}
	run := func(resp map[string]any) (transfer.Info, int32) {
		n, stop := answerPrompts(t, env, admin, resp)
		defer stop()
		time.Sleep(100 * time.Millisecond) // socket registered
		var in transfer.Info
		admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": h.ID, "srcPaths": []string{src + "/a.txt", src + "/b.txt", src + "/c.txt"},
			"dstFs": h.ID, "dstDir": dst, "overwrite": "ask"}, &in)
		deadline := time.Now().Add(20 * time.Second)
		for in.FinishedAt == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			admin.MustJSON("GET", "/api/transfers/"+in.ID, nil, &in)
		}
		return in, n.Load()
	}
	// "No" + remember → every conflict skipped after one question.
	in, asked := run(map[string]any{"accept": false, "save": true})
	if in.State != "done" || in.SkippedFiles != 3 || asked != 1 {
		t.Fatalf("skip all: %+v (asked %d)", in, asked)
	}
	// Custom answer "rename" + remember.
	in, asked = run(map[string]any{"accept": true, "save": true, "values": []string{"rename"}})
	if in.State != "done" || in.DoneFiles != 3 || asked != 1 {
		t.Fatalf("rename all: %+v (asked %d)", in, asked)
	}
	for _, n := range []string{"a (1).txt", "b (1).txt", "c (1).txt"} {
		if b, err := os.ReadFile(dst + "/" + n); err != nil || !strings.HasPrefix(string(b), "new ") {
			t.Fatalf("%s: %q %v", n, b, err)
		}
	}
	// "Yes" without remembering → asked for every file, all overwritten.
	in, asked = run(map[string]any{"accept": true})
	if in.State != "done" || in.DoneFiles != 3 || asked != 3 {
		t.Fatalf("overwrite each: %+v (asked %d)", in, asked)
	}
	if b, _ := os.ReadFile(dst + "/a.txt"); string(b) != "new a.txt" {
		t.Fatalf("overwrite: %q", b)
	}
}
