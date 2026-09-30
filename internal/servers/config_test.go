package servers

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/auth"
)

func testEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	return &env{home: dir, dataDir: filepath.Join(dir, "data"), defaultRoot: filepath.Join(dir, "AstraTermShare"),
		nexHost: "127.0.0.1", nexPort: 7822}
}

func TestNormalizeBind(t *testing.T) {
	cases := map[string]string{"": "127.0.0.1", "localhost": "127.0.0.1", "*": "0.0.0.0", "0.0.0.0": "0.0.0.0",
		"::": "::", "[::1]": "::1", "::ffff:10.0.0.1": "10.0.0.1", " 192.168.1.5 ": "192.168.1.5"}
	for in, want := range cases {
		got, err := normalizeBind(in)
		if err != nil || got != want {
			t.Errorf("normalizeBind(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"example.com", "300.1.1.1", "1.2.3", "http://x"} {
		if _, err := normalizeBind(bad); err == nil || !isInvalid(err) {
			t.Errorf("normalizeBind(%q) accepted", bad)
		}
	}
}

func TestCommonValidate(t *testing.T) {
	e := testEnv(t)
	c := Common{BindAddress: "", Port: 7822}
	if err := c.validate(e, true); err == nil || !strings.Contains(err.Error(), "AstraTerm's own port") {
		t.Fatalf("own port accepted: %v", err)
	}
	c = Common{BindAddress: "0.0.0.0", Port: 7822}
	if err := c.validate(e, true); err == nil {
		t.Fatal("wildcard bind of AstraTerm's port accepted")
	}
	c = Common{BindAddress: "127.0.0.1", Port: 7822}
	if err := c.validate(e, false); err != nil {
		t.Fatalf("UDP on AstraTerm's port refused: %v", err)
	}
	for _, p := range []int{0, -1, 70000} {
		c = Common{Port: p}
		if err := c.validate(e, true); err == nil {
			t.Errorf("port %d accepted", p)
		}
	}
	c = Common{Port: 8080, StopAfterSec: 3}
	if err := c.validate(e, true); err != nil || c.StopAfterSec != minStopAfterSec || c.BindAddress != "127.0.0.1" {
		t.Fatalf("validate: %v %+v", err, c)
	}
	c = Common{Port: 8080, StopAfterSec: -5}
	if err := c.validate(e, true); err == nil {
		t.Fatal("negative stopAfterSec accepted")
	}
}

func TestCleanRoot(t *testing.T) {
	e := testEnv(t)
	got, err := cleanRoot("~/files/../share", e, "root")
	if err != nil || got != filepath.Join(e.home, "share") {
		t.Fatalf("cleanRoot(~) = %q, %v", got, err)
	}
	for _, bad := range []string{"", "relative/dir", "a\x00b"} {
		if _, err := cleanRoot(bad, e, "root"); err == nil {
			t.Errorf("cleanRoot(%q) accepted", bad)
		}
	}
}

func TestValidateUsers(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	spk, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spk))) + " me@laptop"

	list := []User{{Username: " alice ", PasswordHash: "h"}, {Username: "bob", PublicKeys: []string{line, "", "# comment", line}}}
	if err := validateUsers(&list, true, true); err != nil {
		t.Fatal(err)
	}
	if list[0].Username != "alice" || list[0].ID == "" || list[1].ID == "" || list[0].ID == list[1].ID {
		t.Fatalf("names / ids not normalized: %+v", list)
	}
	if len(list[1].PublicKeys) != 1 || !strings.HasSuffix(list[1].PublicKeys[0], " me@laptop") {
		t.Fatalf("keys not normalized: %q", list[1].PublicKeys)
	}
	bad := [][]User{
		{{Username: "a", PasswordHash: "h"}, {Username: "A", PasswordHash: "h"}}, // duplicate (case-insensitive)
		{{Username: "anonymous", PasswordHash: "h"}},                             // reserved
		{{Username: "bad name", PasswordHash: "h"}},                              // invalid characters
		{{Username: "", PasswordHash: "h"}},                                      // empty
		{{Username: "nocred"}},                                                   // no credential
		{{Username: "k", PublicKeys: []string{"ssh-ed25519 notbase64"}}},         // bad key
	}
	for i, l := range bad {
		if err := validateUsers(&l, true, true); err == nil {
			t.Errorf("case %d accepted: %+v", i, l)
		}
	}
	// Keys are dropped where not supported, so a key-only user then lacks a credential.
	l := []User{{Username: "k", PublicKeys: []string{line}}}
	if err := validateUsers(&l, false, true); err == nil {
		t.Fatal("key-only user accepted for a password-only server")
	}
}

func TestApplyUserInput(t *testing.T) {
	prevHash, _ := auth.HashPassword("old-password")
	prev := []User{{ID: "u1", Username: "alice", PasswordHash: prevHash}, {ID: "u2", Username: "bob", PasswordHash: prevHash}}
	next := []User{
		{ID: "u1", Username: "alice-renamed"},                                   // keeps the hash (by id)
		{Username: "bob", PasswordHash: "$argon2id$forged"},                     // matched by name, forged hash ignored
		{Username: "carol", Password: new("new-secret")},                        // new password
		{ID: "u9", Username: "dave", Password: new(""), PublicKeys: []string{}}, // removed password
	}
	if err := applyUserInput(next, prev); err != nil {
		t.Fatal(err)
	}
	if next[0].PasswordHash != prevHash || next[1].PasswordHash != prevHash || next[1].ID != "u2" {
		t.Fatalf("stored hashes not kept: %+v", next[:2])
	}
	if !auth.VerifyPassword(next[2].PasswordHash, "new-secret") || next[2].Password != nil {
		t.Fatal("new password not hashed")
	}
	if next[3].PasswordHash != "" {
		t.Fatal("empty password did not remove the hash")
	}
	if err := applyUserInput([]User{{Username: "x", Password: new("abc")}}, nil); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestPublicConfigHidesSecrets(t *testing.T) {
	e := testEnv(t)
	c := defaultConfig(KindFTP, e).(*FTPConfig)
	c.Users = []User{{ID: "u1", Username: "alice", PasswordHash: "$argon2id$secret"}, {ID: "u2", Username: "bob"}}
	m := publicConfig(c)
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "argon2") || strings.Contains(string(b), "passwordHash") {
		t.Fatalf("hash leaked: %s", b)
	}
	users := m["users"].([]any)
	if users[0].(map[string]any)["hasPassword"] != true || users[1].(map[string]any)["hasPassword"] != false {
		t.Fatalf("hasPassword wrong: %s", b)
	}
}

func TestDefaultConfigsValidate(t *testing.T) {
	e := testEnv(t)
	for _, k := range Kinds {
		c := defaultConfig(k, e)
		if err := c.validate(e); err != nil {
			t.Errorf("%s default invalid: %v", k, err)
		}
		if c.base().BindAddress != "127.0.0.1" {
			t.Errorf("%s does not bind loopback by default", k)
		}
		cl := cloneConfig(k, c, e)
		a, _ := json.Marshal(c)
		b, _ := json.Marshal(cl)
		if string(a) != string(b) {
			t.Errorf("%s clone differs: %s vs %s", k, a, b)
		}
	}
}

func TestKindSpecificValidation(t *testing.T) {
	e := testEnv(t)
	ftp := defaultConfig(KindFTP, e).(*FTPConfig)
	ftp.TLS = "bogus"
	if err := ftp.validate(e); err == nil {
		t.Error("bad TLS mode accepted")
	}
	ftp = defaultConfig(KindFTP, e).(*FTPConfig)
	ftp.PassivePortMin, ftp.PassivePortMax = 3000, 2000
	if err := ftp.validate(e); err == nil {
		t.Error("inverted passive range accepted")
	}
	ftp = defaultConfig(KindFTP, e).(*FTPConfig)
	ftp.PublicHost = "::1"
	if err := ftp.validate(e); err == nil {
		t.Error("IPv6 passive address accepted")
	}
	tftp := defaultConfig(KindTFTP, e).(*TFTPConfig)
	tftp.BlockSize = 100
	if err := tftp.validate(e); err == nil {
		t.Error("tiny block size accepted")
	}
	sl := defaultConfig(KindSyslog, e).(*SyslogConfig)
	sl.UDP, sl.TCP = false, false
	if err := sl.validate(e); err == nil {
		t.Error("syslog without transports accepted")
	}
	sl = defaultConfig(KindSyslog, e).(*SyslogConfig)
	sl.BufferSize = maxBufferSize + 1
	if err := sl.validate(e); err == nil {
		t.Error("huge buffer accepted")
	}
	hc := defaultConfig(KindHTTP, e).(*HTTPConfig)
	hc.RequireAuth = true
	if err := hc.validate(e); err == nil {
		t.Error("auth without users accepted")
	}
}

func TestCheckRoot(t *testing.T) {
	e := testEnv(t)
	// The default root is created on demand.
	if err := checkRoot(e.defaultRoot, e); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(e.defaultRoot); err != nil || !st.IsDir() {
		t.Fatal("default root not created")
	}
	// Other missing folders are an error.
	if err := checkRoot(filepath.Join(e.home, "missing"), e); err == nil || !isInvalid(err) {
		t.Fatalf("missing root accepted: %v", err)
	}
	// Folders overlapping the data directory are refused (parent and child).
	if err := os.MkdirAll(filepath.Join(e.dataDir, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{e.home, e.dataDir, filepath.Join(e.dataDir, "inner")} {
		if err := checkRoot(p, e); err == nil || !strings.Contains(err.Error(), "data directory") {
			t.Errorf("checkRoot(%s) = %v", p, err)
		}
	}
	// Through a symlink too.
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(e.home, link); err == nil {
			if err := checkRoot(link, e); err == nil {
				t.Error("symlink to a parent of the data directory accepted")
			}
		}
	}
	f := filepath.Join(e.home, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if err := checkRoot(f, e); err == nil {
		t.Error("file accepted as root")
	}
}

func TestWithin(t *testing.T) {
	sep := string(filepath.Separator)
	base := sep + filepath.Join("a", "b")
	if !within(base, base) || !within(filepath.Join(base, "c"), base) || within(sep+"a", base) ||
		within(sep+filepath.Join("a", "bc"), base) {
		t.Fatal("within is wrong")
	}
}
