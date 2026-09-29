package servers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/auth"
)

func TestLogRing(t *testing.T) {
	var mu sync.Mutex
	var published []LogEntry
	r := newLogRing(func(e []LogEntry, _ int) {
		mu.Lock()
		published = append(published, e...)
		mu.Unlock()
	})
	for i := 0; i < logRingSize+10; i++ {
		r.add(levelInfo, "1.2.3.4:5", "", "line")
	}
	all, last := r.since(0, maxLogLimit)
	if len(all) != logRingSize || last != int64(logRingSize+10) || all[0].ID != 11 {
		t.Fatalf("ring: %d entries, last %d, first %d", len(all), last, all[0].ID)
	}
	tail, _ := r.since(last-3, 100)
	if len(tail) != 3 || tail[2].ID != last {
		t.Fatalf("since: %+v", tail)
	}
	few, _ := r.since(0, 5)
	if len(few) != 5 || few[4].ID != last {
		t.Fatalf("limit keeps the newest: %+v", few)
	}
	r.add(levelWarn, "", "u\x1b[31m", "evil\r\ninjected\x00")
	e, _ := r.since(last, 1)
	if strings.ContainsAny(e[0].Message, "\r\n\x00") || strings.Contains(e[0].User, "\x1b") {
		t.Fatalf("control characters kept: %q %q", e[0].Message, e[0].User)
	}
	time.Sleep(logFlushEvery + 100*time.Millisecond)
	mu.Lock()
	n := len(published)
	mu.Unlock()
	if n == 0 || n > logMaxBatch+1 {
		t.Fatalf("published %d entries", n)
	}
	r.clear()
	if all, _ := r.since(0, 10); len(all) != 0 {
		t.Fatal("clear kept entries")
	}
}

func TestRelName(t *testing.T) {
	cases := map[string]string{"": ".", "/": ".", "/a/b": "a/b", "a/../../b": "b", "/../../etc/passwd": "etc/passwd",
		"./x/./y/": "x/y"}
	for in, want := range cases {
		if got := filepath.ToSlash(relName(in)); got != want {
			t.Errorf("relName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRootFSJail(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "outside.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "in.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, err := openRootFS(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	if f, err := fsys.Open("/../outside.txt"); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) == "secret" {
			t.Fatal("escaped with ..")
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Symlink(filepath.Join(base, "outside.txt"), filepath.Join(root, "abs-link"))
		_ = os.Symlink("../outside.txt", filepath.Join(root, "rel-link"))
		_ = os.Symlink("in.txt", filepath.Join(root, "ok-link"))
		for _, name := range []string{"abs-link", "rel-link"} {
			if f, err := fsys.Open(name); err == nil {
				f.Close()
				t.Errorf("symlink %s escaped the root", name)
			}
		}
		if f, err := fsys.Open("ok-link"); err != nil {
			t.Errorf("inner symlink refused: %v", err)
		} else {
			f.Close()
		}
		// Creating links: absolute or escaping targets are refused.
		for _, target := range []string{"/etc/passwd", "../outside.txt", "dir/../../x"} {
			if err := fsys.Symlink(target, "new-link"); err == nil {
				t.Errorf("symlink to %q created", target)
				_ = fsys.Remove("new-link")
			}
		}
		if err := fsys.Symlink("sub/file.txt", "dir/down-link"); err != nil {
			t.Errorf("valid relative symlink refused: %v", err)
		}
		// ".." is refused even when it looks local: through "self" → "." the link below lands one level higher
		// than its name suggests ("self/dir/x" is really "dir/x", so "../../outside.txt" would leave the root).
		if err := fsys.Symlink(".", "self"); err != nil {
			t.Fatalf("symlink to . refused: %v", err)
		}
		for _, target := range []string{"../in.txt", "../../outside.txt", `..\..\outside.txt`} {
			if err := fsys.Symlink(target, "self/dir/x"); err == nil {
				t.Errorf("symlink to %q created", target)
			}
		}
		if _, err := os.Lstat(filepath.Join(root, "dir", "x")); err == nil {
			t.Error("escaping symlink exists on disk")
		}
	}
	if err := fsys.Chmod("/", 0o777); err == nil {
		t.Error("chmod of the shared folder allowed")
	}
	if err := fsys.Remove("/"); err == nil {
		t.Error("removing the root allowed")
	}
	if err := fsys.Rename("/", "/x"); err == nil {
		t.Error("renaming the root allowed")
	}
	ro := fsys.withReadOnly(true)
	checks := map[string]error{
		"mkdir":  ro.Mkdir("new", 0o755),
		"remove": ro.Remove("in.txt"),
		"rename": ro.Rename("in.txt", "x.txt"),
		"chmod":  ro.Chmod("in.txt", 0o600),
		"write": func() error {
			f, err := ro.OpenFile("in.txt", os.O_WRONLY|os.O_TRUNC, 0)
			if err == nil {
				f.Close()
			}
			return err
		}(),
	}
	for op, err := range checks {
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("read-only %s: %v", op, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "in.txt")); string(b) != "ok" {
		t.Fatal("read-only view modified a file")
	}
	list, err := fsys.ReadDir("/")
	if err != nil || len(list) == 0 {
		t.Fatalf("ReadDir: %v", err)
	}
}

func TestTransferFileCounts(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	var doneCalls int
	tf := &transferFile{File: f, upload: true, done: func(*transferFile) { doneCalls++ }}
	if _, err := tf.ReadFrom(strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.WriteAt([]byte("!!"), 5); err != nil {
		t.Fatal(err)
	}
	_ = tf.Close()
	_ = tf.Close()
	if tf.bytes() != 7 || doneCalls != 1 {
		t.Fatalf("bytes %d, done %d", tf.bytes(), doneCalls)
	}
	// Append mode ignores offsets.
	f2, _ := os.Create(filepath.Join(t.TempDir(), "a"))
	_, _ = f2.WriteString("abc")
	ta := &transferFile{File: f2, upload: true, appendMu: &sync.Mutex{}}
	if _, err := ta.WriteAt([]byte("def"), 0); err != nil {
		t.Fatal(err)
	}
	_ = ta.Close()
	if b, _ := os.ReadFile(f2.Name()); string(b) != "abcdef" {
		t.Fatalf("append: %q", b)
	}
}

func TestUserDB(t *testing.T) {
	h, _ := auth.HashPassword("s3cret-pass")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	spk, _ := ssh.NewPublicKey(pub)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	opk, _ := ssh.NewPublicKey(other)
	var stats counters
	db := newUserDB([]User{{Username: "alice", PasswordHash: h, PublicKeys: []string{string(ssh.MarshalAuthorizedKey(spk))}}}, &stats)
	db.sleepFn = nil
	if u, err := db.checkPassword("1.1.1.1", "alice", "s3cret-pass"); err != nil || u.Username != "alice" {
		t.Fatalf("login: %v", err)
	}
	// Cached second check.
	if _, err := db.checkPassword("1.1.1.1", "alice", "s3cret-pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.checkPassword("1.1.1.1", "alice", "wrong"); !errors.Is(err, errBadCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := db.checkPassword("1.1.1.1", "nobody", "s3cret-pass"); !errors.Is(err, errBadCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	if _, err := db.checkKey("1.1.1.1", "alice", spk); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := db.checkKey("1.1.1.1", "alice", opk); err == nil {
		t.Fatal("foreign key accepted")
	}
	// Brute force: the IP gets blocked, even for the right password; another IP is unaffected.
	var err error
	for i := 0; i < failMax; i++ {
		_, err = db.checkPassword("6.6.6.6", "alice", "guess")
	}
	if !errors.Is(err, errTooManyFails) {
		t.Fatalf("not blocked after %d failures: %v", failMax, err)
	}
	if _, err := db.checkPassword("6.6.6.6", "alice", "s3cret-pass"); !errors.Is(err, errTooManyFails) {
		t.Fatalf("blocked IP logged in: %v", err)
	}
	if _, err := db.checkPassword("7.7.7.7", "alice", "s3cret-pass"); err != nil {
		t.Fatalf("other IP: %v", err)
	}
	if stats.authFailures.Load() < int64(failMax) {
		t.Fatalf("auth failures %d", stats.authFailures.Load())
	}
	// The block expires.
	now := time.Now().Add(failBlock + time.Minute)
	db.lim.now = func() time.Time { return now }
	if db.lim.blocked("6.6.6.6") {
		t.Fatal("block did not expire")
	}
}

func TestTelnetConnParsing(t *testing.T) {
	server, peer := net.Pipe()
	defer server.Close()
	defer peer.Close()
	tc := newTelnetConn(server)
	var resized [2]int
	var rmu sync.Mutex
	tc.onResize(func(c, r int) {
		rmu.Lock()
		resized = [2]int{c, r}
		rmu.Unlock()
	})
	replies := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				close(replies)
				return
			}
			replies <- append([]byte(nil), buf[:n]...)
		}
	}()
	input := []byte{'a', tnIAC, tnIAC, 'b', '\r', 0, 'c', '\r', '\n', 'd',
		tnIAC, tnSB, optNAWS, 0, 120, 0, 40, tnIAC, tnSE,
		tnIAC, tnWILL, optTType,
		tnIAC, tnSB, optTType, 0, 'X', 'T', 'E', 'R', 'M', tnIAC, tnSE,
		tnIAC, tnDO, 99, // unknown option → WONT
		tnIAC, tnIP,
	}
	go func() { _, _ = peer.Write(input) }()
	var got []byte
	buf := make([]byte, 64)
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < 8 && time.Now().Before(deadline) {
		n, err := tc.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	if want := []byte{'a', 0xff, 'b', '\r', 'c', '\r', 'd', 0x03}; !bytes.Equal(got, want) {
		t.Fatalf("data %q, want %q", got, want)
	}
	rmu.Lock()
	if resized != [2]int{120, 40} {
		t.Fatalf("NAWS: %v", resized)
	}
	rmu.Unlock()
	if tc.termType() != "xterm-256color" {
		t.Fatalf("TTYPE XTERM mapped to %q", tc.termType())
	}
	var all []byte
	timeout := time.After(time.Second)
collect:
	for {
		select {
		case r, ok := <-replies:
			if !ok {
				break collect
			}
			all = append(all, r...)
			if bytes.Contains(all, []byte{tnIAC, tnWONT, 99}) && bytes.Contains(all, []byte{tnIAC, tnSB, optTType, 1}) {
				break collect
			}
		case <-timeout:
			break collect
		}
	}
	if !bytes.Contains(all, []byte{tnIAC, tnWONT, 99}) || !bytes.Contains(all, []byte{tnIAC, tnSB, optTType, 1, tnIAC, tnSE}) {
		t.Fatalf("negotiation replies: %v", all)
	}
	// Output escapes IAC.
	go func() { _ = tc.writeData([]byte{1, 0xff, 2}) }()
	select {
	case r := <-replies:
		if !bytes.Equal(r, []byte{1, 0xff, 0xff, 2}) {
			t.Fatalf("escaped output %v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no output")
	}
}

func TestTFTPName(t *testing.T) {
	cases := map[string]string{"file.bin": "file.bin", "/boot/pxe.0": "boot/pxe.0", `\\dir\\f`: "dir/f",
		`C:\fw\image.bin`: "fw/image.bin", "../../etc/passwd": "etc/passwd"}
	for in, want := range cases {
		got, err := tftpName(in)
		if err != nil || got != want {
			t.Errorf("tftpName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/", "a\x00b", ".."} {
		if _, err := tftpName(bad); err == nil {
			t.Errorf("tftpName(%q) accepted", bad)
		}
	}
}

func TestHumanBytesAndDisposition(t *testing.T) {
	if humanBytes(512) != "512 B" || humanBytes(1536) != "1.5 KB" || humanBytes(5<<20) != "5.0 MB" {
		t.Fatal(humanBytes(512), humanBytes(1536), humanBytes(5<<20))
	}
	d := contentDisposition(`we"ird\ñame.txt`)
	if !strings.Contains(d, `filename="we_ird__ame.txt"`) || !strings.Contains(d, "filename*=UTF-8''we%22ird%5C%C3%B1ame.txt") {
		t.Fatalf("disposition %q", d)
	}
}
