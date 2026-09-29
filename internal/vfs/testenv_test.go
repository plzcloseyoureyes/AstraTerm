package vfs_test

// Integration tests against the Docker lab (scripts/testenv/docker-compose.yml). Run with ASTRATERM_TESTENV=1:
//
//	ASTRATERM_TESTENV=1 go test ./internal/vfs/ -run TestLab -v
//
// ssh1 127.0.0.1:22022 (test/test, sudo with password), FTP 127.0.0.1:22021, S3 (SeaweedFS) 127.0.0.1:22090,
// WebDAV 127.0.0.1:22080, SMB 127.0.0.1:22445 share "share".

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
	"github.com/plzcloseyoureyes/astraterm/internal/transfer"
)

func labOnly(t *testing.T) {
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Skip("set ASTRATERM_TESTENV=1 to run against the Docker lab")
	}
}

// promptResponder answers every prompt of the user: host keys are trusted and saved, passwords answered.
type promptResponder struct {
	mu      sync.Mutex
	kinds   []string
	answers map[string]string // prompt kind → value
	events  []json.RawMessage
}

func startResponder(t *testing.T, env *servertest.Env, c *servertest.Client, answers map[string]string) *promptResponder {
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
	ws.SetReadLimit(16 << 20)
	pr := &promptResponder{answers: answers}
	t.Cleanup(func() {
		cancel()
		ws.CloseNow()
	})
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				Type   string `json:"type"`
				Prompt struct {
					ID     string `json:"id"`
					Kind   string `json:"kind"`
					Fields []struct {
						Value string `json:"value"`
					} `json:"fields"`
				} `json:"prompt"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			pr.mu.Lock()
			pr.events = append(pr.events, json.RawMessage(append([]byte(nil), data...)))
			pr.mu.Unlock()
			if m.Type != "prompt" {
				continue
			}
			pr.mu.Lock()
			pr.kinds = append(pr.kinds, m.Prompt.Kind)
			pr.mu.Unlock()
			resp := map[string]any{"type": "prompt.response", "id": m.Prompt.ID, "accept": true, "save": true}
			if v, ok := pr.answers[m.Prompt.Kind]; ok {
				resp["values"] = []string{v}
			}
			b, _ := json.Marshal(resp)
			_ = ws.Write(ctx, websocket.MessageText, b)
		}
	}()
	return pr
}

func (p *promptResponder) count(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, k := range p.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func createConn(t *testing.T, c *servertest.Client, body map[string]any) string {
	t.Helper()
	var conn model.Connection
	c.MustJSON("POST", "/api/connections", body, &conn)
	return conn.ID
}

func openFS(t *testing.T, c *servertest.Client, body map[string]any) (*fsClient, handleView) {
	t.Helper()
	var h handleView
	c.MustJSON("POST", "/api/fs", body, &h)
	return &fsClient{t: t, c: c, id: h.ID}, h
}

// exercise runs the common file operations on a handle below dir (created and removed).
func exercise(t *testing.T, f *fsClient, dir string, opts exerciseOpts) {
	t.Helper()
	var e model.FileEntry
	f.must("POST", "mkdir", nil, map[string]any{"path": dir, "parents": true}, &e)
	if e.Type != "dir" {
		t.Fatalf("mkdir %+v", e)
	}
	defer f.c.JSON("POST", f.url("delete", nil), map[string]any{"paths": []string{dir}, "recursive": true}, nil)

	data := make([]byte, opts.size)
	rand.Read(data)
	target := dir + "/data.bin"
	chunk := opts.chunk
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		qv := q("path", target, "offset", fmt.Sprint(off), "mtime", "1600000000")
		if end == len(data) {
			qv.Set("final", "1")
		}
		resp, body := f.c.Do("PUT", f.url("upload", qv), data[off:end])
		if resp.StatusCode != 200 {
			t.Fatalf("upload chunk at %d: %d %s", off, resp.StatusCode, body)
		}
		if end < len(data) && off == 0 {
			var st struct{ Size int64 }
			f.must("GET", "upload", q("path", target), nil, &st)
			if st.Size != int64(end) {
				t.Fatalf("resume offset %d, want %d", st.Size, end)
			}
			// A retried (duplicate) first chunk must be accepted when resending from a lower offset.
			if opts.retryChunk {
				resp, body := f.c.Do("PUT", f.url("upload", q("path", target, "offset", "0")), data[:end])
				if resp.StatusCode != 200 {
					t.Fatalf("chunk retry: %d %s", resp.StatusCode, body)
				}
			}
		}
	}
	f.must("GET", "stat", q("path", target), nil, &e)
	if e.Size != int64(len(data)) || e.Type != "file" {
		t.Fatalf("stat after upload %+v", e)
	}
	if opts.mtime && e.Mtime.Unix() != 1600000000 {
		t.Errorf("mtime not applied: %v", e.Mtime)
	}
	resp, body := f.c.Do("GET", f.url("download", q("path", target)), nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("download: %d, %d bytes", resp.StatusCode, len(body))
	}
	checkRanges(t, f.c, f.url("download", q("path", target)), data)
	var sum struct{ Hash string }
	f.must("POST", "checksum", nil, map[string]any{"path": target, "algo": "sha256"}, &sum)
	want := sha256.Sum256(data)
	if sum.Hash != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum %s", sum.Hash)
	}
	f.must("POST", "rename", nil, map[string]any{"from": target, "to": dir + "/renamed.bin"}, &e)
	f.must("PUT", "write", nil, map[string]any{"path": dir + "/note.txt", "content": "hello from astraterm\n"}, &e)
	var rd struct{ Content string }
	f.must("GET", "read", q("path", dir+"/note.txt"), nil, &rd)
	if rd.Content != "hello from astraterm\n" {
		t.Fatalf("read %q", rd.Content)
	}
	var list struct{ Entries []model.FileEntry }
	f.must("GET", "list", q("path", dir), nil, &list)
	if len(list.Entries) != 2 {
		t.Fatalf("list %+v", list.Entries)
	}
	if opts.owner != "" && (list.Entries[0].Owner != opts.owner || list.Entries[0].Group != opts.group) {
		t.Errorf("owner/group names: %q %q", list.Entries[0].Owner, list.Entries[0].Group)
	}
	var sr struct{ Entries []model.FileEntry }
	f.must("POST", "search", nil, map[string]any{"path": dir, "pattern": "*.txt", "maxResults": 10}, &sr)
	if len(sr.Entries) != 1 || sr.Entries[0].Name != "note.txt" {
		t.Fatalf("search %+v", sr.Entries)
	}
	var cp struct{ Entries []model.FileEntry }
	f.must("POST", "copy", nil, map[string]any{"from": []string{dir + "/note.txt"}, "toDir": dir}, &cp)
	if len(cp.Entries) != 1 {
		t.Fatalf("copy %+v", cp)
	}
	if opts.archive {
		f.must("POST", "archive", nil, map[string]any{"paths": []string{dir + "/note.txt", dir + "/renamed.bin"}, "dest": dir + "/pack.zip", "format": "zip"}, &e)
		f.must("POST", "extract", nil, map[string]any{"path": dir + "/pack.zip", "destDir": dir + "/out"}, nil)
		f.must("GET", "read", q("path", dir+"/out/note.txt"), nil, &rd)
		if rd.Content != "hello from astraterm\n" {
			t.Fatalf("extracted %q", rd.Content)
		}
	}
	if opts.chmod {
		f.must("POST", "chmod", nil, map[string]any{"paths": []string{dir}, "mode": "go-rwx", "recursive": true}, nil)
		f.must("GET", "stat", q("path", dir+"/note.txt"), nil, &e)
		if e.Mode&0o077 != 0 {
			t.Fatalf("recursive chmod: %s", e.Perm)
		}
		f.must("POST", "chmod", nil, map[string]any{"paths": []string{dir + "/note.txt"}, "mode": 0o640}, nil)
		f.must("GET", "stat", q("path", dir+"/note.txt"), nil, &e)
		if e.Perm != "-rw-r-----" {
			t.Fatalf("chmod: %s", e.Perm)
		}
	}
	f.must("POST", "delete", nil, map[string]any{"paths": []string{dir + "/renamed.bin"}}, nil)
	if st, _ := f.code("GET", "stat", q("path", dir+"/renamed.bin"), nil); st != 404 {
		t.Fatalf("deleted file still there: %d", st)
	}
}

type exerciseOpts struct {
	size, chunk  int
	retryChunk   bool
	mtime        bool
	archive      bool
	chmod        bool
	owner, group string
}

func waitSession(t *testing.T, c *servertest.Client, id string, cond func(model.RuntimeSession) bool, what string) model.RuntimeSession {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var s model.RuntimeSession
		c.MustJSON("GET", "/api/sessions/"+id, nil, &s)
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s: %+v", what, s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLab(t *testing.T) {
	labOnly(t)
	env := servertest.New(t, func(c *config.Config) {
		if os.Getenv("ASTRATERM_TEST_DEBUG") == "1" {
			c.LogLevel = "debug"
		}
	})
	admin := env.Setup("admin", pw)
	pr := startResponder(t, env, admin, map[string]string{"password": "test"})
	run := "astraterm-it-" + randHex(4)

	sshConn := createConn(t, admin, map[string]any{"name": "ssh1", "protocol": "ssh", "host": "127.0.0.1", "port": 22022,
		"username": "test", "authMethod": "password", "secrets": map[string]string{"password": "test"}})

	t.Run("SSHBrowserFromSession", func(t *testing.T) {
		var sess model.RuntimeSession
		admin.MustJSON("POST", "/api/sessions", map[string]any{"connectionId": sshConn, "cols": 120, "rows": 40}, &sess)
		defer admin.JSON("DELETE", "/api/sessions/"+sess.ID, nil, nil)
		waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.State == model.StateConnected }, "connected")
		prompts := pr.count("password") + pr.count("keyboard-interactive")

		f, h := openFS(t, admin, map[string]any{"sessionId": sess.ID})
		if h.Kind != "sftp" || h.Driver != "sftp" || h.Home != "/config" || !h.Capabilities.Exec || !h.Capabilities.Chmod || h.SessionID != sess.ID {
			t.Fatalf("handle %+v", h)
		}
		if n := pr.count("password") + pr.count("keyboard-interactive"); n != prompts {
			t.Fatal("opening the SSH browser of a session must not log in again")
		}
		exercise(t, f, "/tmp/"+run+"-sftp", exerciseOpts{size: 3<<20 + 123, chunk: 1 << 20, retryChunk: true, mtime: true,
			archive: true, chmod: true, owner: "test", group: "users"})

		// FILE-2: the shell reports its folder (injected integration), the terminal shows no trace of it.
		waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/config" }, "initial cwd")
		admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "cd /tmp\r"}, nil)
		waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/tmp" }, "cwd after cd")
		var cwd struct{ Path string }
		f.must("GET", "cwd", nil, nil, &cwd)
		if cwd.Path != "/tmp" {
			t.Fatalf("cwd endpoint %q", cwd.Path)
		}
		_, sb := admin.Do("GET", "/api/sessions/"+sess.ID+"/scrollback?raw=0", nil)
		if bytes.Contains(sb, []byte("__nx7")) || bytes.Contains(sb, []byte("PROMPT_COMMAND")) {
			t.Fatalf("injected line visible in the terminal:\n%s", sb)
		}
		admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "history | tail -3\r"}, nil)
		time.Sleep(700 * time.Millisecond)
		_, sb = admin.Do("GET", "/api/sessions/"+sess.ID+"/scrollback?raw=0", nil)
		if bytes.Contains(sb, []byte("__nx7")) {
			t.Fatalf("setup line left in the shell history:\n%s", sb)
		}
		t.Logf("terminal after integration:\n%s", sb)

		// Closing the session closes its handles.
		admin.MustJSON("DELETE", "/api/sessions/"+sess.ID, nil, nil)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if st, code := f.code("GET", "list", q("path", "/"), nil); st == 404 && code == "fs_not_found" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("handle survived its session")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("FollowCwdAfterEarlyTyping", func(t *testing.T) {
		var sess model.RuntimeSession
		admin.MustJSON("POST", "/api/sessions", map[string]any{"connectionId": sshConn, "cols": 100, "rows": 30}, &sess)
		defer admin.JSON("DELETE", "/api/sessions/"+sess.ID, nil, nil)
		waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.State == model.StateConnected }, "connected")
		// Typing right away (before the first prompt settled) must not abandon the integration...
		admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "echo early-bird\r"}, nil)
		// ...and a half-typed line must never get the hidden command appended.
		time.Sleep(300 * time.Millisecond)
		admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "echo half"}, nil)
		time.Sleep(1500 * time.Millisecond)
		var s model.RuntimeSession
		admin.MustJSON("GET", "/api/sessions/"+sess.ID, nil, &s)
		if s.Cwd != "" {
			t.Fatal("injected while the user had a half-typed line")
		}
		admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "-typed\r"}, nil)
		waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/config" }, "cwd after early typing")
		admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "cd /tmp\r"}, nil)
		waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/tmp" }, "cwd after cd")
		_, sb := admin.Do("GET", "/api/sessions/"+sess.ID+"/scrollback?raw=0", nil)
		if bytes.Contains(sb, []byte("__nx7")) || !bytes.Contains(sb, []byte("\nearly-bird")) || !bytes.Contains(sb, []byte("\nhalf-typed")) {
			t.Fatalf("terminal content:\n%s", sb)
		}
		t.Logf("terminal:\n%s", sb)
	})

	t.Run("SCPFallback", func(t *testing.T) {
		id := createConn(t, admin, map[string]any{"name": "ssh1-scp", "protocol": "ssh", "host": "127.0.0.1", "port": 22022,
			"username": "test", "authMethod": "password", "secrets": map[string]string{"password": "test"},
			"options": map[string]any{"sshBrowser": "scp"}})
		f, h := openFS(t, admin, map[string]any{"connectionId": id})
		if h.Driver != "scp" || h.Kind != "sftp" {
			t.Fatalf("handle %+v", h)
		}
		exercise(t, f, "/tmp/"+run+"-scp", exerciseOpts{size: 1<<20 + 7, chunk: 400 << 10, retryChunk: true, mtime: true,
			archive: true, chmod: true, owner: "test", group: "users"})
		// single-shot upload goes through scp -t
		resp, body := admin.Do("PUT", f.url("upload", q("path", "/tmp/"+run+"-scp1.txt", "offset", "0", "final", "1")), []byte("scp sink"))
		if resp.StatusCode != 200 {
			t.Fatalf("scp upload %d %s", resp.StatusCode, body)
		}
		var rd struct{ Content string }
		f.must("GET", "read", q("path", "/tmp/"+run+"-scp1.txt"), nil, &rd)
		if rd.Content != "scp sink" {
			t.Fatalf("scp content %q", rd.Content)
		}
		f.must("POST", "delete", nil, map[string]any{"paths": []string{"/tmp/" + run + "-scp1.txt"}}, nil)
		// shell-injection attempt in a path stays a literal file name
		evil := "/tmp/" + run + "-$(touch pwned-" + run + ");touch pwned2-" + run + "'`touch pwned3`\"$HOME"
		f.must("PUT", "write", nil, map[string]any{"path": evil, "content": "x"}, nil)
		var e model.FileEntry
		f.must("GET", "stat", q("path", evil), nil, &e)
		var list struct{ Entries []model.FileEntry }
		f.must("GET", "list", q("path", "/tmp"), nil, &list)
		found := false
		for _, le := range list.Entries {
			if le.Path == evil {
				found = true
			}
		}
		for _, p := range []string{"/config/pwned-" + run, "/config/pwned2-" + run, "/config/pwned3", "/tmp/pwned-" + run} {
			if st, _ := f.code("GET", "stat", q("path", p), nil); st != 404 {
				t.Fatalf("command injection through a path: %s exists", p)
			}
		}
		if !found {
			t.Fatal("the literal file name is not listed")
		}
		f.must("POST", "delete", nil, map[string]any{"paths": []string{evil}}, nil)
	})

	t.Run("SudoBrowseAndSave", func(t *testing.T) {
		id := createConn(t, admin, map[string]any{"name": "ssh1-sudo", "protocol": "ssh", "host": "127.0.0.1", "port": 22022,
			"username": "test", "authMethod": "password", "secrets": map[string]string{"password": "test", "sudoPassword": "test"}})
		f, h := openFS(t, admin, map[string]any{"connectionId": id})
		if !h.Capabilities.Sudo {
			t.Fatalf("sudo capability %+v", h.Capabilities)
		}
		target := "/etc/" + run + ".conf"
		if st, code := f.code("PUT", "write", nil, map[string]any{"path": target, "content": "a=1\n"}); st != 403 || code != "permission_denied" {
			t.Fatalf("write without sudo: %d %s", st, code)
		}
		var e model.FileEntry
		f.must("PUT", "write", nil, map[string]any{"path": target, "content": "a=1\n", "sudo": true}, &e)
		if e.Owner != "root" || e.Size != 4 {
			t.Fatalf("sudo write entry %+v", e)
		}
		root, rh := openFS(t, admin, map[string]any{"connectionId": id, "sudo": true})
		if rh.Driver != "sudo-sftp" {
			t.Fatalf("sudo handle %+v", rh)
		}
		var list struct{ Entries []model.FileEntry }
		root.must("GET", "list", q("path", "/root"), nil, &list)
		root.must("POST", "delete", nil, map[string]any{"paths": []string{target}}, nil)
		if st, _ := f.code("GET", "stat", q("path", target), nil); st != 404 {
			t.Fatal("sudo delete failed")
		}
	})

	var ftpF, s3F, davF, smbF *fsClient
	t.Run("FTP", func(t *testing.T) {
		id := createConn(t, admin, map[string]any{"name": "ftp", "protocol": "ftp", "host": "127.0.0.1", "port": 22021,
			"username": "test", "secrets": map[string]string{"password": "test"}})
		var h handleView
		ftpF, h = openFS(t, admin, map[string]any{"connectionId": id})
		if h.Kind != "ftp" || h.Home != "/ftp/test" {
			t.Fatalf("handle %+v", h)
		}
		exercise(t, ftpF, "/ftp/test/"+run, exerciseOpts{size: 2<<20 + 1, chunk: 700 << 10, mtime: true, archive: true})
	})

	t.Run("S3", func(t *testing.T) {
		id := createConn(t, admin, map[string]any{"name": "seaweed", "protocol": "s3", "host": "", "port": 0,
			"options": map[string]any{"endpoint": "http://127.0.0.1:22090", "pathStyle": true, "accessKeyId": "astraterm",
				"region": "us-east-1"}, "secrets": map[string]string{"secretAccessKey": "astraterm-secret"}})
		var h handleView
		s3F, h = openFS(t, admin, map[string]any{"connectionId": id})
		if h.Kind != "s3" || !h.Capabilities.Presign {
			t.Fatalf("handle %+v", h)
		}
		bucket := "/" + run
		s3F.must("POST", "mkdir", nil, map[string]any{"path": bucket}, nil)
		exercise(t, s3F, bucket+"/dir", exerciseOpts{size: 20<<20 + 5, chunk: 7 << 20, retryChunk: false, archive: true})
		s3F.must("PUT", "write", nil, map[string]any{"path": bucket + "/p.txt", "content": "presigned!"}, nil)
		var ps struct{ URL string }
		s3F.must("POST", "presign", nil, map[string]any{"path": bucket + "/p.txt", "expiresSec": 60}, &ps)
		resp, err := http.Get(ps.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "presigned!" {
			t.Fatalf("presigned GET: %d %q", resp.StatusCode, b)
		}
	})

	t.Run("WebDAV", func(t *testing.T) {
		id := createConn(t, admin, map[string]any{"name": "dav", "protocol": "webdav", "host": "127.0.0.1", "port": 22080,
			"username": "test", "options": map[string]any{"https": false}, "secrets": map[string]string{"password": "test"}})
		var h handleView
		davF, h = openFS(t, admin, map[string]any{"connectionId": id})
		if h.Kind != "webdav" {
			t.Fatalf("handle %+v", h)
		}
		exercise(t, davF, "/"+run, exerciseOpts{size: 1<<20 + 3, chunk: 300 << 10, retryChunk: true, archive: true})
	})

	t.Run("SMB", func(t *testing.T) {
		id := createConn(t, admin, map[string]any{"name": "smb", "protocol": "smb", "host": "127.0.0.1", "port": 22445,
			"username": "test", "options": map[string]any{"share": "share"}, "secrets": map[string]string{"password": "test"}})
		var h handleView
		smbF, h = openFS(t, admin, map[string]any{"connectionId": id})
		if h.Kind != "smb" {
			t.Fatalf("handle %+v", h)
		}
		exercise(t, smbF, "/"+run, exerciseOpts{size: 1<<20 + 9, chunk: 256 << 10, retryChunk: true, mtime: true, archive: true})
	})

	t.Run("CrossTransfers", func(t *testing.T) {
		sftp, _ := openFS(t, admin, map[string]any{"connectionId": sshConn})
		local := openLocal(t, admin)
		dir := tempDir(t)
		os.MkdirAll(dir+"/tree/sub", 0o755)
		payload := bytes.Repeat([]byte("astraterm transfer "), 200000)
		os.WriteFile(dir+"/tree/a.bin", payload, 0o644)
		os.WriteFile(dir+"/tree/sub/b.txt", []byte("bee"), 0o644)
		hops := []struct {
			name     string
			src, dst *fsClient
			srcDir   string
			dstDir   string
		}{
			{"local→sftp", local, sftp, dir, "/tmp"},
			{"sftp→ftp", sftp, ftpF, "/tmp", "/ftp/test"},
			{"ftp→s3", ftpF, s3F, "/ftp/test", "/" + run},
			{"s3→smb", s3F, smbF, "/" + run, "/"},
			{"smb→webdav", smbF, davF, "/", "/"},
			{"webdav→local", davF, local, "/", dir + "/back"},
		}
		os.MkdirAll(dir+"/back", 0o755)
		for _, hop := range hops {
			if hop.src == nil || hop.dst == nil {
				t.Fatalf("%s: handle missing (earlier subtest failed)", hop.name)
			}
			srcPath := hop.srcDir + "/tree"
			if hop.srcDir == "/" {
				srcPath = "/tree"
			}
			if hop.name == "local→sftp" {
				srcPath = dir + "/tree"
			}
			var in transfer.Info
			admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": hop.src.id, "srcPaths": []string{srcPath},
				"dstFs": hop.dst.id, "dstDir": hop.dstDir, "overwrite": "overwrite", "verify": true}, &in)
			in = waitTransfer(t, admin, in.ID)
			if in.State != "done" || in.DoneFiles != 2 || in.DoneBytes != int64(len(payload)+3) {
				t.Fatalf("%s: %+v", hop.name, in)
			}
			t.Logf("%s: %d bytes, %d files", hop.name, in.DoneBytes, in.DoneFiles)
		}
		if b, _ := os.ReadFile(dir + "/back/tree/a.bin"); !bytes.Equal(b, payload) {
			t.Fatal("round trip content mismatch")
		}
		// cleanup
		sftp.must("POST", "delete", nil, map[string]any{"paths": []string{"/tmp/tree"}, "recursive": true}, nil)
		ftpF.must("POST", "delete", nil, map[string]any{"paths": []string{"/ftp/test/tree"}, "recursive": true}, nil)
		smbF.must("POST", "delete", nil, map[string]any{"paths": []string{"/tree"}, "recursive": true}, nil)
		davF.must("POST", "delete", nil, map[string]any{"paths": []string{"/tree"}, "recursive": true}, nil)
		s3F.must("POST", "delete", nil, map[string]any{"paths": []string{"/" + run}, "recursive": true}, nil)
	})
}
