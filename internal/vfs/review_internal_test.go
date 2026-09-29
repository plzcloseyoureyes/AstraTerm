package vfs

import (
	"context"
	"strings"
	"testing"
	"time"
)

// listFS returns a fixed listing (a broken or malicious server).
type listFS struct {
	FS
	entries []*Entry
}

func (l *listFS) List(context.Context, string) ([]*Entry, error) { return l.entries, nil }

func TestListDirSanitizes(t *testing.T) {
	mk := func(name, typ string) *Entry { return &Entry{Name: name, Path: "/elsewhere/" + name, Type: typ} }
	fsys := &listFS{entries: []*Entry{mk("..", "dir"), mk(".", "dir"), mk("", "file"), mk("a/../../b", "file"),
		mk("nul\x00x", "file"), mk("dup", "symlink"), mk("dup", "dir"), mk("ok", "file"), nil, mk("\xff\xfe", "file")}}
	got, err := ListDir(bg, fsys, "/d")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name+"@"+e.Path+":"+e.Type)
	}
	if strings.Join(names, ",") != "dup@/d/dup:symlink,ok@/d/ok:file,\xff\xfe@/d/\xff\xfe:file" {
		t.Fatalf("sanitized listing %q", names)
	}
	res := withinResults("/d", []*Entry{{Path: "/d/x"}, {Path: "/etc/passwd"}, {Path: "/d"}, {Path: "/d/../e"}, {Path: "/dx"}})
	if len(res) != 1 || res[0].Path != "/d/x" {
		t.Fatalf("search results %v", res)
	}
}

func TestFTPPathGuard(t *testing.T) {
	if ftpPathOK("/a b/ü.txt") != nil {
		t.Fatal("plain names must pass")
	}
	for _, p := range []string{"/x\r\nDELE /important", "/a\nb", "/a\x00b"} {
		if ftpPathOK(p) == nil {
			t.Errorf("%q must be refused on FTP", p)
		}
	}
}

func TestRFC5987(t *testing.T) {
	if got := rfc5987("a b\"é;\r\n.txt"); got != "a%20b%22%C3%A9%3B%0D%0A.txt" {
		t.Fatalf("rfc5987 %q", got)
	}
	cd := contentDisposition("attachment", "x\"\r\ny.txt")
	if strings.ContainsAny(cd, "\r\n") || strings.Count(cd, `"`) != 2 {
		t.Fatalf("content disposition %q", cd)
	}
}

func TestChmodArgExplicitClass(t *testing.T) {
	cases := map[string]string{"-w": "a-w", "u+x,-w": "u+x,a-w", "go-rwx": "go-rwx", "=r": "a=r", "755": "0755"}
	for in, want := range cases {
		sp, err := parseMode(in)
		if err != nil || sp.chmodArg() != want {
			t.Errorf("parseMode(%q).chmodArg() = %q, %v; want %q", in, sp.chmodArg(), err, want)
		}
	}
}

func TestPromptWatch(t *testing.T) {
	old := sudoPromptQuiet
	sudoPromptQuiet = 30 * time.Millisecond
	defer func() { sudoPromptQuiet = old }()
	m := sudoMarker()
	if strings.Contains(m, "%") {
		t.Fatal("sudo expands % in prompts")
	}
	// The marker split across writes counts once, and its started half is no quiet prompt of its own.
	w := newPromptWatch(m)
	w.Write([]byte("We trust you have received the usual lecture\n\n" + m[:7]))
	time.Sleep(80 * time.Millisecond)
	w.Write([]byte(m[7:]))
	if w.count() != 1 {
		t.Fatalf("prompts %d", w.count())
	}
	w.Write([]byte("Sorry, try again.\n" + m))
	if w.count() != 2 || strings.Contains(w.message(), m) {
		t.Fatalf("second prompt %d / message %q", w.count(), w.message())
	}
	// A PAM-supplied prompt (no marker): a partial line followed by silence.
	w2 := newPromptWatch(sudoMarker())
	w2.Write([]byte("LDAP Password: "))
	time.Sleep(80 * time.Millisecond)
	if w2.count() != 1 {
		t.Fatalf("PAM prompt not detected: %d", w2.count())
	}
	// Complete lines are not prompts.
	w3 := newPromptWatch(sudoMarker())
	w3.Write([]byte("sudo: a password is required\n"))
	time.Sleep(80 * time.Millisecond)
	if w3.count() != 0 || !passwordRequired([]byte(w3.message())) {
		t.Fatalf("error line counted as prompt: %d", w3.count())
	}
}
