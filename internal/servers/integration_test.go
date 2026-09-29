package servers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/pin/tftp/v3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	cfgpkg "github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
)

func TestAPIListAndGating(t *testing.T) {
	h := newHarness(t)
	admin := h.user("admin", true)
	var list []Status
	h.must(admin, http.MethodGet, "/api/servers", nil, &list, http.StatusOK)
	if len(list) != len(Kinds) {
		t.Fatalf("%d statuses", len(list))
	}
	for i, st := range list {
		if st.Kind != Kinds[i] || st.Running || st.State != stateStopped || st.Config == nil || st.Name == "" {
			t.Errorf("status %d: %+v", i, st)
		}
	}
	var host HostInfo
	h.must(admin, http.MethodGet, "/api/servers/host", nil, &host, http.StatusOK)
	if host.Platform != runtime.GOOS || host.NexTermPort != 7822 || host.DefaultRoot == "" {
		t.Fatalf("host: %+v", host)
	}
	if code, _ := h.call(admin, http.MethodGet, "/api/servers/nfs", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown kind: %d", code)
	}
	if code, _ := h.call(nil, http.MethodGet, "/api/servers", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	// Desktop mode: every signed-in user may use the servers.
	user := h.user("user", false)
	h.must(user, http.MethodGet, "/api/servers", nil, nil, http.StatusOK)
}

func TestServerModeAdminOnly(t *testing.T) {
	h := newHarness(t, func(c *cfgpkg.Config) { c.Mode = cfgpkg.ModeServer })
	admin := h.user("admin", true)
	user := h.user("user", false)
	h.must(admin, http.MethodGet, "/api/servers", nil, nil, http.StatusOK)
	for _, req := range [][2]string{{http.MethodGet, "/api/servers"}, {http.MethodGet, "/api/servers/http"},
		{http.MethodPost, "/api/servers/http/start"}, {http.MethodPut, "/api/servers/http"},
		{http.MethodGet, "/api/servers/syslog/messages"}, {http.MethodGet, "/api/servers/http/logs"}} {
		if code, _ := h.call(user, req[0], req[1], map[string]any{}, nil); code != http.StatusForbidden {
			t.Errorf("%s %s as user: %d", req[0], req[1], code)
		}
	}
	// Topic subscriptions are refused too.
	ev := h.events(user)
	ev.send(map[string]any{"type": "subscribe", "topic": topicStatus})
	ev.wait(func(m map[string]any) bool { return m["type"] == "subscribe.error" && m["topic"] == topicStatus })
}

func TestHTTPServer(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	st := h.configure(u, KindHTTP, map[string]any{"root": root, "port": port})
	if st.Running || st.Config["root"] != root {
		t.Fatalf("configure: %+v", st)
	}
	st = h.start(u, KindHTTP)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	if st.Addr != "127.0.0.1:"+strconv.Itoa(port) || st.URL != base+"/" {
		t.Fatalf("addr %q url %q", st.Addr, st.URL)
	}
	get := func(path string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "hello.txt") || !strings.Contains(body, "sub/") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("listing %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "Upload") {
		t.Fatal("upload form shown on a read-only server")
	}
	resp, body = get("/hello.txt", nil)
	if resp.StatusCode != 200 || body != "hello world\n" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("file %d %q", resp.StatusCode, body)
	}
	resp, body = get("/hello.txt", map[string]string{"Range": "bytes=6-10"})
	if resp.StatusCode != http.StatusPartialContent || body != "world" {
		t.Fatalf("range %d %q", resp.StatusCode, body)
	}
	resp, _ = get("/sub", nil)
	if resp.Request.URL.Path != "/sub/" {
		t.Fatalf("directory redirect: %s", resp.Request.URL)
	}
	resp, _ = get("/hello.txt?download", nil)
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download disposition missing")
	}
	// Escapes.
	for _, p := range []string{"/../secret.txt", "/%2e%2e/secret.txt", "/sub/../../secret.txt"} {
		if resp, body := get(p, nil); strings.Contains(body, "top secret") {
			t.Fatalf("%s escaped (%d)", p, resp.StatusCode)
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Symlink(filepath.Join(h.dir, "secret.txt"), filepath.Join(root, "leak"))
		if resp, body := get("/leak", nil); resp.StatusCode == 200 || strings.Contains(body, "top secret") {
			t.Fatalf("symlink escape: %d %q", resp.StatusCode, body)
		}
	}
	// Read-only: PUT refused.
	req, _ := http.NewRequest(http.MethodPut, base+"/new.txt", strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT on read-only: %d", resp.StatusCode)
	}

	// Writable with a 1 MB upload limit (the running server restarts).
	st = h.configure(u, KindHTTP, map[string]any{"readOnly": false, "upload": true, "maxUploadMB": 1})
	if !st.Running {
		t.Fatalf("not restarted: %+v", st)
	}
	req, _ = http.NewRequest(http.MethodPut, base+"/sub/put.txt", strings.NewReader("put body"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "put.txt")); string(b) != "put body" {
		t.Fatalf("PUT content %q", b)
	}
	big := bytes.Repeat([]byte("x"), 2<<20)
	req, _ = http.NewRequest(http.MethodPut, base+"/big.bin", bytes.NewReader(big))
	resp, err = http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized PUT: %d", resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin")); err == nil {
		t.Fatal("oversized upload stored")
	}
	// Browser upload (multipart) + new folder.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "up load.txt")
	_, _ = fw.Write([]byte("multipart!"))
	_ = mw.WriteField("mkdir", "newdir")
	_ = mw.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ = http.NewRequest(http.MethodPost, base+"/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", base)
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("upload: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if b, _ := os.ReadFile(filepath.Join(root, "up load.txt")); string(b) != "multipart!" {
		t.Fatalf("multipart content %q", b)
	}
	if st, err := os.Stat(filepath.Join(root, "newdir")); err != nil || !st.IsDir() {
		t.Fatal("folder not created")
	}
	// Cross-site form posts are refused.
	req, _ = http.NewRequest(http.MethodPost, base+"/", strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("Origin", "https://evil.example")
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site upload: %d", resp.StatusCode)
	}
	_, body = get("/", nil)
	if !strings.Contains(body, "Upload") || !strings.Contains(body, "up load.txt") || !strings.Contains(body, "./up%20load.txt") {
		t.Fatal("listing lacks the upload form or the new file")
	}

	// Basic authentication.
	h.configure(u, KindHTTP, map[string]any{"requireAuth": true,
		"users": []map[string]any{{"username": "alice", "password": "wonderland"}}})
	resp, _ = get("/hello.txt", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no auth: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/hello.txt", nil)
	req.SetBasicAuth("alice", "wrong")
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	req.SetBasicAuth("alice", "wonderland")
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("right password: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	var status Status
	h.must(u, http.MethodGet, "/api/servers/http", nil, &status, http.StatusOK)
	users := status.Config["users"].([]any)
	if users[0].(map[string]any)["hasPassword"] != true || strings.Contains(fmt.Sprint(status.Config), "argon2") {
		t.Fatalf("config leaks or lacks hasPassword: %v", status.Config)
	}
	if status.Stats.AuthFailures < 1 || status.Stats.Transfers < 1 || status.Stats.Connections < 1 {
		t.Fatalf("stats: %+v", status.Stats)
	}
	e := h.waitLog(u, KindHTTP, "GET /hello.txt → 200")
	if e.User != "alice" {
		t.Fatalf("log user %q", e.User)
	}
	h.waitLog(u, KindHTTP, "Authentication failed")

	h.stop(u, KindHTTP)
	if _, err := http.Get(base + "/"); err == nil {
		t.Fatal("still serving after stop")
	}
}

func TestHTTPS(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": port, "tls": true})
	st := h.start(u, KindHTTP)
	if !strings.HasPrefix(st.URL, "https://") || len(st.Fingerprint) != 95 {
		t.Fatalf("https status: %+v", st)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get(st.URL + "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello world\n" || resp.TLS == nil {
		t.Fatalf("https body %q", b)
	}
	sum := colonHexSHA256(resp.TLS.PeerCertificates[0].Raw)
	if sum != st.Fingerprint {
		t.Fatalf("fingerprint %s != %s", sum, st.Fingerprint)
	}
}

func TestFTPServer(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	// No users and no anonymous access: refused.
	h.configure(u, KindFTP, map[string]any{"root": root, "port": port, "tls": "optional"})
	if code, body := h.call(u, http.MethodPost, "/api/servers/ftp/start", nil, nil); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "invalid_config") {
		t.Fatalf("start without users: %d %s", code, body)
	}
	h.configure(u, KindFTP, map[string]any{"users": []map[string]any{
		{"username": "alice", "password": "alice-pass"},
		{"username": "bob", "password": "bob-pass", "readOnly": true},
	}})
	st := h.start(u, KindFTP)
	if st.Fingerprint == "" || !strings.HasPrefix(st.URL, "ftp://") {
		t.Fatalf("status: %+v", st)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	c, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("alice", "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
	c.Quit()
	c, err = ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("alice", "alice-pass"); err != nil {
		t.Fatal(err)
	}
	entries, err := c.List("/")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] || !names["sub"] {
		t.Fatalf("list: %v", names)
	}
	r, err := c.Retr("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "hello world\n" {
		t.Fatalf("retr %q", b)
	}
	if r, err := c.Retr("/../secret.txt"); err == nil {
		b, _ := io.ReadAll(r)
		r.Close()
		if strings.Contains(string(b), "secret") {
			t.Fatal("escaped the root")
		}
	}
	if err := c.Stor("/sub/up.txt", strings.NewReader("uploaded by ftp")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "up.txt")); string(b) != "uploaded by ftp" {
		t.Fatalf("stored %q", b)
	}
	if err := c.MakeDir("/made"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("/sub/up.txt", "/made/moved.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("/made/moved.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDir("/made"); err != nil {
		t.Fatal(err)
	}
	// Connected clients show the user; the transfer is logged.
	var clients []ClientInfo
	h.must(u, http.MethodGet, "/api/servers/ftp/clients", nil, &clients, http.StatusOK)
	if len(clients) != 1 || clients[0].User != "alice" {
		t.Fatalf("clients: %+v", clients)
	}
	h.waitLog(u, KindFTP, "Uploaded /sub/up.txt")
	h.waitLog(u, KindFTP, "Downloaded /hello.txt")

	// Read-only user.
	cb, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.Login("bob", "bob-pass"); err != nil {
		t.Fatal(err)
	}
	if err := cb.Stor("/bob.txt", strings.NewReader("x")); err == nil {
		t.Fatal("read-only user uploaded")
	}
	if err := cb.Delete("/hello.txt"); err == nil {
		t.Fatal("read-only user deleted")
	}
	cb.Quit()
	if _, err := os.Stat(filepath.Join(root, "bob.txt")); err == nil {
		t.Fatal("read-only upload stored")
	}

	// Anonymous (read-only).
	if ca, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second)); err == nil {
		if err := ca.Login("anonymous", "a@b.c"); err == nil {
			t.Fatal("anonymous login while disabled")
		}
		ca.Quit()
	}
	h.configure(u, KindFTP, map[string]any{"anonymous": true})
	ca, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Login("anonymous", "a@b.c"); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.List("/"); err != nil {
		t.Fatal(err)
	}
	if err := ca.Stor("/anon.txt", strings.NewReader("x")); err == nil {
		t.Fatal("anonymous upload allowed")
	}
	ca.Quit()

	// Explicit TLS (control and data channels).
	ct, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second),
		ftp.DialWithExplicitTLS(&tls.Config{InsecureSkipVerify: true, ClientSessionCache: tls.NewLRUClientSessionCache(4)}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ct.Login("alice", "alice-pass"); err != nil {
		t.Fatal(err)
	}
	r, err = ct.Retr("/sub/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r)
	r.Close()
	if string(b) != "nested\n" {
		t.Fatalf("tls retr %q", b)
	}
	ct.Quit()
}

func TestFTPKickAndPasswordChange(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindFTP, map[string]any{"root": root, "port": port, "tls": "off",
		"users": []map[string]any{{"username": "alice", "password": "first-pass"}}})
	h.start(u, KindFTP)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	c, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("alice", "first-pass"); err != nil {
		t.Fatal(err)
	}
	var clients []ClientInfo
	h.must(u, http.MethodGet, "/api/servers/ftp/clients", nil, &clients, http.StatusOK)
	if len(clients) != 1 {
		t.Fatalf("clients %+v", clients)
	}
	h.must(u, http.MethodDelete, "/api/servers/ftp/clients/"+clients[0].ID, nil, nil, http.StatusOK)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.NoOp(); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := c.NoOp(); err == nil {
		t.Fatal("kicked client still connected")
	}
	// A password change restarts the server: the old password stops working.
	var status Status
	h.must(u, http.MethodGet, "/api/servers/ftp", nil, &status, http.StatusOK)
	id := status.Config["users"].([]any)[0].(map[string]any)["id"].(string)
	h.configure(u, KindFTP, map[string]any{"users": []map[string]any{{"id": id, "username": "alice", "password": "second-pass"}}})
	c2, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Login("alice", "first-pass"); err == nil {
		t.Fatal("old password still works")
	}
	c2.Quit()
	c3, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := c3.Login("alice", "second-pass"); err != nil {
		t.Fatal(err)
	}
	c3.Quit()
	// Keeping the password (omitted) while renaming the user.
	h.configure(u, KindFTP, map[string]any{"users": []map[string]any{{"id": id, "username": "alicia"}}})
	c4, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := c4.Login("alicia", "second-pass"); err != nil {
		t.Fatalf("renamed user: %v", err)
	}
	c4.Quit()
}

func sshClient(t *testing.T, addr, user string, auth ssh.AuthMethod) (*ssh.Client, ssh.PublicKey, error) {
	t.Helper()
	var hostKey ssh.PublicKey
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{auth}, Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error { hostKey = key; return nil }}
	c, err := ssh.Dial("tcp", addr, cfg)
	return c, hostKey, err
}

func TestSFTPServer(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	spk, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	h.configure(u, KindSFTP, map[string]any{"root": root, "port": port, "users": []map[string]any{
		{"username": "alice", "password": "alice-pass"},
		{"username": "carol", "publicKeys": []string{string(ssh.MarshalAuthorizedKey(spk))}, "readOnly": true},
	}})
	st := h.start(u, KindSFTP)
	addr := "127.0.0.1:" + strconv.Itoa(port)

	if _, _, err := sshClient(t, addr, "alice", ssh.Password("nope")); err == nil {
		t.Fatal("wrong password accepted")
	}
	c, hostKey, err := sshClient(t, addr, "alice", ssh.Password("alice-pass"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if fp := ssh.FingerprintSHA256(hostKey); fp != st.Fingerprint && hostKey.Type() == ssh.KeyAlgoED25519 {
		t.Fatalf("host key %s != %s", fp, st.Fingerprint)
	}
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	list, err := sc.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 2 {
		t.Fatalf("readdir: %d entries", len(list))
	}
	if wd, _ := sc.Getwd(); wd != "/" {
		t.Fatalf("working directory %q", wd)
	}
	f, err := sc.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "hello world\n" {
		t.Fatalf("read %q", b)
	}
	// Escapes stay in the jail.
	if _, err := sc.Stat("/../secret.txt"); err == nil {
		if f, err := sc.Open("/../secret.txt"); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if strings.Contains(string(b), "secret") {
				t.Fatal("escaped the jail")
			}
		}
	}
	if list, err := sc.ReadDir("/../.."); err != nil || len(list) == 0 {
		t.Fatalf("parent of / should be /: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := sc.Symlink("/etc/passwd", "/pw"); err == nil {
			t.Fatal("absolute symlink created")
		}
		if err := sc.Symlink("../../secret.txt", "/sub/leak"); err == nil {
			t.Fatal("escaping symlink created")
		}
		_ = os.Symlink(filepath.Join(h.dir, "secret.txt"), filepath.Join(root, "leak"))
		if f, err := sc.Open("/leak"); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if strings.Contains(string(b), "secret") {
				t.Fatal("followed a symlink out of the jail")
			}
		}
	}
	w, err := sc.Create("/sub/upload.bin")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("0123456789"), 50_000)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if got, _ := os.ReadFile(filepath.Join(root, "sub", "upload.bin")); !bytes.Equal(got, payload) {
		t.Fatalf("upload: %d bytes", len(got))
	}
	if err := sc.Mkdir("/newdir"); err != nil {
		t.Fatal(err)
	}
	if err := sc.Rename("/sub/upload.bin", "/newdir/upload.bin"); err != nil {
		t.Fatal(err)
	}
	if err := sc.Rename("/newdir/upload.bin", "/hello.txt"); err == nil {
		t.Fatal("SFTP rename replaced an existing file")
	}
	if err := sc.PosixRename("/newdir/upload.bin", "/newdir/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	if err := sc.Chmod("/newdir/renamed.bin", 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := sc.Chtimes("/newdir/renamed.bin", mt, mt); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "newdir", "renamed.bin")); err != nil || !fi.ModTime().Equal(mt) {
		t.Fatalf("chtimes: %v", err)
	}
	if err := sc.Remove("/newdir/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	if err := sc.RemoveDirectory("/newdir"); err != nil {
		t.Fatal(err)
	}
	h.waitLog(u, KindSFTP, "Uploaded /sub/upload.bin")

	// Shell disabled: commands are refused with a message.
	sess, _ := c.NewSession()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	err = sess.Run("echo hi")
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 1 || !strings.Contains(stderr.String(), "SFTP") {
		t.Fatalf("command with shell disabled: %v %q", err, stderr.String())
	}
	// Port forwarding is refused.
	if conn, err := c.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Fatal("port forwarding allowed")
	}

	// Public key, read-only user.
	ck, _, err := sshClient(t, addr, "carol", ssh.PublicKeys(signer))
	if err != nil {
		t.Fatal(err)
	}
	defer ck.Close()
	sk, err := sftp.NewClient(ck)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Create("/carol.txt"); err == nil {
		t.Fatal("read-only user created a file")
	}
	if err := sk.Remove("/hello.txt"); err == nil {
		t.Fatal("read-only user removed a file")
	}
	if _, err := sk.Stat("/hello.txt"); err != nil {
		t.Fatal(err)
	}
	sk.Close()

	var clients []ClientInfo
	h.must(u, http.MethodGet, "/api/servers/sftp/clients", nil, &clients, http.StatusOK)
	users := map[string]bool{}
	for _, cl := range clients {
		users[cl.User] = true
	}
	if !users["alice"] || !users["carol"] {
		t.Fatalf("clients: %+v", clients)
	}
}

func TestSFTPShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindSFTP, map[string]any{"root": root, "port": port, "shell": true, "shellCommand": "/bin/sh",
		"users": []map[string]any{{"username": "alice", "password": "alice-pass"}}})
	st := h.start(u, KindSFTP)
	if !strings.Contains(strings.Join(st.Warnings, " "), "Shell access") {
		t.Fatalf("no shell warning: %v", st.Warnings)
	}
	c, _, err := sshClient(t, "127.0.0.1:"+strconv.Itoa(port), "alice", ssh.Password("alice-pass"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, _ := c.NewSession()
	out, err := sess.Output("echo $((6*7)); pwd; exit 3")
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("exit status: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if lines[0] != "42" || filepath.Base(resolvePath(lines[len(lines)-1])) != "share" {
		t.Fatalf("output %q", out)
	}
	// Interactive shell on a PTY.
	sess, _ = c.NewSession()
	if err := sess.RequestPty("xterm", 40, 100, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := sess.StdinPipe()
	var outBuf safeBuffer
	sess.Stdout = &outBuf
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdin, "stty size; echo pty-$((40+2)); exit\n")
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shell did not exit")
	}
	if s := outBuf.String(); !strings.Contains(s, "pty-42") || !strings.Contains(s, "40 100") {
		t.Fatalf("pty output %q", s)
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTFTPServer(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "udp")
	h.configure(u, KindTFTP, map[string]any{"root": root, "port": port, "timeoutSec": 1, "retries": 2})
	h.start(u, KindTFTP)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	c, err := tftp.NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(2 * time.Second)
	c.SetRetries(2)
	wt, err := c.Receive("hello.txt", "octet")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := wt.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello world\n" {
		t.Fatalf("tftp read %q", buf.String())
	}
	if wt, err := c.Receive("../secret.txt", "octet"); err == nil {
		var b bytes.Buffer
		_, _ = wt.WriteTo(&b)
		if strings.Contains(b.String(), "secret") {
			t.Fatal("escaped the root")
		}
	}
	if wt, err := c.Receive("missing.bin", "octet"); err == nil {
		if _, err := wt.WriteTo(io.Discard); err == nil {
			t.Fatal("missing file served")
		}
	}
	// Read-only: writes are refused.
	rf, err := c.Send("upload.bin", "octet")
	if err == nil {
		_, err = rf.ReadFrom(strings.NewReader("data"))
	}
	if err == nil {
		t.Fatal("write accepted on a read-only server")
	}
	h.waitLog(u, KindTFTP, "read-only")
	h.configure(u, KindTFTP, map[string]any{"readOnly": false})
	payload := bytes.Repeat([]byte("firmware"), 4096)
	rf, err = c.Send("sub/fw.bin", "octet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.ReadFrom(bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i := 0; i < 50; i++ {
		if got, _ = os.ReadFile(filepath.Join(root, "sub", "fw.bin")); bytes.Equal(got, payload) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("tftp write: %d bytes", len(got))
	}
	h.waitLog(u, KindTFTP, "Received sub/fw.bin")
	var st Status
	h.must(u, http.MethodGet, "/api/servers/tftp", nil, &st, http.StatusOK)
	if st.Stats.Transfers < 1 || st.Stats.BytesIn < int64(len(payload)) { // counters of the current run
		t.Fatalf("stats %+v", st.Stats)
	}
}

func readUntil(t *testing.T, r *bufio.Reader, conn net.Conn, marker string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	var got []byte
	for !bytes.Contains(got, []byte(marker)) {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", marker, err, got)
		}
		got = append(got, b)
	}
	return string(got)
}

func TestTelnetServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	h := newHarness(t)
	u := h.user("admin", true)
	port := freePort(t, "tcp")
	if code, _ := h.call(u, http.MethodPost, "/api/servers/telnet/start", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("start without users: %d", code)
	}
	h.configure(u, KindTelnet, map[string]any{"port": port, "shellCommand": "/bin/sh",
		"users": []map[string]any{{"username": "alice", "password": "alice-pass"}}})
	h.start(u, KindTelnet)
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	readUntil(t, r, conn, "login: ")
	_, _ = conn.Write([]byte("alice\r\n"))
	readUntil(t, r, conn, "Password: ")
	_, _ = conn.Write([]byte("wrong\r\n"))
	readUntil(t, r, conn, "Login incorrect")
	readUntil(t, r, conn, "login: ")
	_, _ = conn.Write([]byte("alice\r\n"))
	readUntil(t, r, conn, "Password: ")
	// Window size via NAWS before the shell starts.
	_, _ = conn.Write([]byte{tnIAC, tnSB, optNAWS, 0, 99, 0, 33, tnIAC, tnSE})
	_, _ = conn.Write([]byte("alice-pass\r\n"))
	_, _ = conn.Write([]byte("stty size; echo tel$((40+2))net; exit\r\n"))
	out := readUntil(t, r, conn, "tel42net")
	if !strings.Contains(out, "33 99") {
		t.Fatalf("NAWS size not applied: %q", out)
	}
	h.waitLog(u, KindTelnet, "Logged in")
	h.waitLog(u, KindTelnet, "Login failed")
}

func TestSyslogServer(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	port := freePort(t, "both")
	logDir := filepath.Join(h.dir, "syslog-files")
	h.configure(u, KindSyslog, map[string]any{"port": port, "logToFile": true, "logDir": logDir})
	st := h.start(u, KindSyslog)
	if len(st.Addrs) != 2 || !strings.HasSuffix(st.Addrs[0], "/udp") || !strings.HasSuffix(st.Addrs[1], "/tcp") {
		t.Fatalf("addrs %v", st.Addrs)
	}
	ev := h.events(u)
	ev.send(map[string]any{"type": "subscribe", "topic": topicSyslog})
	time.Sleep(100 * time.Millisecond) // subscription runs asynchronously
	udp, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_, _ = udp.Write([]byte("<34>Oct 11 22:14:15 mymachine su: 'su root' failed for lonvick"))
	_, _ = udp.Write([]byte("<165>1 2003-10-11T22:14:15.003Z host5424 app 1 ID [x@1 a=\"b\"] structured"))
	tcp, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	msg := "<11>1 - tcphost tcpapp - - - octet counted"
	fmt.Fprintf(tcp, "%d %s", len(msg), msg)
	fmt.Fprintf(tcp, "<14>Oct 11 22:14:16 tcphost lf: newline framed\n")
	tcp.Close()

	var page SyslogPage
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.must(u, http.MethodGet, "/api/servers/syslog/messages", nil, &page, http.StatusOK)
		if len(page.Messages) >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(page.Messages) != 4 || page.Total != 4 {
		t.Fatalf("messages: %+v", page)
	}
	byHost := map[string]SyslogMessage{}
	for _, m := range page.Messages {
		byHost[m.Hostname+"/"+m.Transport] = m
		if m.Source != "127.0.0.1" {
			t.Errorf("source %q", m.Source)
		}
	}
	if m := byHost["host5424/udp"]; m.StructuredData != `[x@1 a="b"]` || m.Message != "structured" || m.Format != "rfc5424" {
		t.Fatalf("5424: %+v", m)
	}
	if m := byHost["tcphost/tcp"]; m.Message == "" {
		t.Fatalf("tcp messages: %+v", byHost)
	}
	// Filters.
	h.must(u, http.MethodGet, "/api/servers/syslog/messages?severity=3", nil, &page, http.StatusOK)
	if len(page.Messages) != 2 { // <34> crit and <11> err
		t.Fatalf("severity filter: %d", len(page.Messages))
	}
	h.must(u, http.MethodGet, "/api/servers/syslog/messages?q=LONVICK", nil, &page, http.StatusOK)
	if len(page.Messages) != 1 || page.Messages[0].AppName != "su" {
		t.Fatalf("text filter: %+v", page.Messages)
	}
	h.must(u, http.MethodGet, "/api/servers/syslog/messages?filter=lonvick", nil, &page, http.StatusOK)
	if len(page.Messages) != 1 {
		t.Fatalf("filter alias: %+v", page.Messages)
	}
	h.must(u, http.MethodGet, "/api/servers/syslog/messages?q=octet%7Cnewline&regex=1", nil, &page, http.StatusOK)
	if len(page.Messages) != 2 {
		t.Fatalf("regex filter: %d", len(page.Messages))
	}
	if code, _ := h.call(u, http.MethodGet, "/api/servers/syslog/messages?q=(&regex=1", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad regex: %d", code)
	}
	h.must(u, http.MethodGet, "/api/servers/syslog/messages?limit=2", nil, &page, http.StatusOK)
	if len(page.Messages) != 2 || !page.HasMore || page.Messages[1].ID != page.LastID {
		t.Fatalf("paging: %+v", page)
	}
	before := page.Messages[0].ID
	h.must(u, http.MethodGet, "/api/servers/syslog/messages?limit=10&before="+strconv.FormatInt(before, 10), nil, &page, http.StatusOK)
	if len(page.Messages) != 2 || page.HasMore {
		t.Fatalf("older page: %+v", page)
	}
	// Live events.
	e := ev.wait(func(m map[string]any) bool { return m["type"] == evSyslog })
	if msgs, _ := e["messages"].([]any); len(msgs) == 0 {
		t.Fatalf("syslog event: %v", e)
	}
	// Senders are the "clients".
	var clients []ClientInfo
	h.must(u, http.MethodGet, "/api/servers/syslog/clients", nil, &clients, http.StatusOK)
	if len(clients) != 1 || clients[0].Addr != "127.0.0.1" {
		t.Fatalf("clients %+v", clients)
	}
	// Export.
	code, body := h.call(u, http.MethodGet, "/api/servers/syslog/export?q=framed", nil, nil)
	if code != 200 || !strings.Contains(string(body), "newline framed") || strings.Count(string(body), "\n") != 1 {
		t.Fatalf("export %d %q", code, body)
	}
	// File logging.
	h.stop(u, KindSyslog)
	files, _ := filepath.Glob(filepath.Join(logDir, "syslog-*.log"))
	if len(files) != 1 {
		t.Fatalf("log files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if strings.Count(string(b), "\n") != 4 || !strings.Contains(string(b), "local4.notice host5424 app[1]:") {
		t.Fatalf("log file %q", b)
	}
	if fi, _ := os.Stat(files[0]); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode %v", fi.Mode())
	}
	// Messages survive the stop; clear empties the buffer.
	h.must(u, http.MethodGet, "/api/servers/syslog/messages", nil, &page, http.StatusOK)
	if len(page.Messages) != 4 {
		t.Fatal("messages lost on stop")
	}
	h.must(u, http.MethodDelete, "/api/servers/syslog/messages", nil, nil, http.StatusOK)
	h.must(u, http.MethodGet, "/api/servers/syslog/messages", nil, &page, http.StatusOK)
	if len(page.Messages) != 0 {
		t.Fatal("clear kept messages")
	}
}

func TestEventsAndLogsTopic(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": port})
	ev := h.events(u)
	ev.send(map[string]any{"type": "subscribe", "topic": topicStatus})
	ev.send(map[string]any{"type": "subscribe", "topic": topicLogs, "kind": "http"})
	time.Sleep(100 * time.Millisecond)
	h.start(u, KindHTTP)
	e := ev.wait(func(m map[string]any) bool {
		st, _ := m["status"].(map[string]any)
		return m["type"] == model.EvServer && st["kind"] == "http" && st["state"] == stateRunning
	})
	if st := e["status"].(map[string]any); st["running"] != true {
		t.Fatalf("status event %v", e)
	}
	e = ev.wait(func(m map[string]any) bool { return m["type"] == evServerLog && m["kind"] == "http" })
	entries := e["entries"].([]any)
	if !strings.Contains(entries[len(entries)-1].(map[string]any)["message"].(string), "Server started") {
		t.Fatalf("log event %v", e)
	}
	// Client counts are pushed.
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ev.wait(func(m map[string]any) bool {
		st, _ := m["status"].(map[string]any)
		c, _ := st["clients"].(float64)
		return m["type"] == model.EvServer && c >= 1
	})
	h.stop(u, KindHTTP)
	ev.wait(func(m map[string]any) bool {
		st, _ := m["status"].(map[string]any)
		return m["type"] == model.EvServer && st["state"] == stateStopped
	})
}

func TestStartErrors(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": busy})
	code, body := h.call(u, http.MethodPost, "/api/servers/http/start", nil, nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "port_in_use") {
		t.Fatalf("busy port: %d %s", code, body)
	}
	var st Status
	h.must(u, http.MethodGet, "/api/servers/http", nil, &st, http.StatusOK)
	if st.State != stateError || st.ErrorCode != "port_in_use" || st.Error == "" {
		t.Fatalf("error state: %+v", st)
	}
	// Stopping clears the error.
	st = h.stop(u, KindHTTP)
	if st.State != stateStopped || st.Error != "" {
		t.Fatalf("after stop: %+v", st)
	}
	// Missing root.
	h.configure(u, KindHTTP, map[string]any{"root": filepath.Join(h.dir, "missing"), "port": freePort(t, "tcp")})
	if code, body := h.call(u, http.MethodPost, "/api/servers/http/start", nil, nil); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "does not exist") {
		t.Fatalf("missing root: %d %s", code, body)
	}
	// Invalid configurations are rejected at save time.
	for _, cfg := range []map[string]any{{"port": 0}, {"bindAddress": "example.com"}, {"root": "relative"},
		{"port": "80"}, {"users": []map[string]any{{"username": "x y", "password": "long enough"}}}} {
		if code, _ := h.call(u, http.MethodPut, "/api/servers/http", cfg, nil); code != http.StatusBadRequest {
			t.Errorf("config %v accepted: %d", cfg, code)
		}
	}
	// The data directory is never shared.
	h.configure(u, KindHTTP, map[string]any{"root": h.cfg.DataDir})
	if code, body := h.call(u, http.MethodPost, "/api/servers/http/start", nil, nil); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "data directory") {
		t.Fatalf("data dir root: %d %s", code, body)
	}
}

func TestPersistenceAutostartAndAutoStop(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": port, "autoStart": true, "stopAfterSec": 600})
	// A second manager on the same database loads the configuration and autostarts it.
	m2 := newManager(h.d)
	m2.loadConfigs(h.ctx)
	c := m2.slots[KindHTTP].config().(*HTTPConfig)
	if c.Root != root || c.Port != port || !c.AutoStart || c.StopAfterSec != 600 {
		t.Fatalf("reloaded config %+v", c)
	}
	m2.autostart()
	st := m2.slots[KindHTTP].status()
	if !st.Running || st.StopAt == nil || time.Until(*st.StopAt) < 9*time.Minute {
		t.Fatalf("autostart: %+v", st)
	}
	// The automatic stop.
	s := m2.slots[KindHTTP]
	s.mu.Lock()
	in := s.inst
	s.mu.Unlock()
	s.autoStop(in, 10*time.Minute)
	if st := s.status(); st.Running || st.State != stateStopped {
		t.Fatalf("auto-stop: %+v", st)
	}
	entries, _ := s.logs.since(0, 100)
	if !strings.Contains(entries[len(entries)-1].Message, "stopped automatically") {
		t.Fatalf("log %+v", entries[len(entries)-1])
	}
	// Changing only autoStart does not restart a running server.
	h.start(u, KindHTTP)
	var before Status
	h.must(u, http.MethodGet, "/api/servers/http", nil, &before, http.StatusOK)
	after := h.configure(u, KindHTTP, map[string]any{"autoStart": false})
	if !after.Running || !after.StartedAt.Equal(*before.StartedAt) {
		t.Fatalf("restarted on an autoStart change: %v vs %v", before.StartedAt, after.StartedAt)
	}
	// Stop-all.
	var list []Status
	h.must(u, http.MethodPost, "/api/servers/stop-all", nil, &list, http.StatusOK)
	for _, st := range list {
		if st.Running {
			t.Fatalf("%s still running", st.Kind)
		}
	}
}

func TestShutdownStopsServers(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": port})
	h.start(u, KindHTTP)
	h.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.m.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		conn.Close()
		t.Fatal("server still listening after shutdown")
	}
}

func colonHexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return colonHex(sum[:])
}

func TestTFTPSinglePort(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 8192) // 128 KiB
	if err := os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	port := freePort(t, "udp")
	h.configure(u, KindTFTP, map[string]any{"root": root, "port": port, "singlePort": true, "timeoutSec": 1, "retries": 2})
	for round := 0; round < 2; round++ { // the port is released on stop: a restart binds it again
		h.start(u, KindTFTP)
		c, err := tftp.NewClient("127.0.0.1:" + strconv.Itoa(port))
		if err != nil {
			t.Fatal(err)
		}
		c.SetTimeout(2 * time.Second)
		c.SetBlockSize(1428)
		wt, err := c.Receive("big.bin", "octet")
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := wt.WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), big) {
			t.Fatalf("round %d: %d bytes", round, buf.Len())
		}
		h.stop(u, KindTFTP)
	}
}
