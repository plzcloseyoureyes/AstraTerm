package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// readUntil reads from b until out contains want (or the deadline passes).
func readUntil(t *testing.T, b term.Backend, out *bytes.Buffer, want string) {
	t.Helper()
	type res struct {
		n   int
		err error
		buf []byte
	}
	deadline := time.After(15 * time.Second)
	for !strings.Contains(out.String(), want) {
		ch := make(chan res, 1)
		go func() {
			buf := make([]byte, 4096)
			n, err := b.Read(buf)
			ch <- res{n, err, buf[:n]}
		}()
		select {
		case r := <-ch:
			out.Write(r.buf)
			if r.err != nil && !strings.Contains(out.String(), want) {
				t.Fatalf("read error %v before %q; output %q", r.err, want, out.String())
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; output %q", want, out.String())
		}
	}
}

func TestLocalShellEchoAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	dir := t.TempDir()
	conn := &model.Connection{Protocol: model.ProtoLocal, Options: model.Options{
		"shell": "/bin/sh", "loginShell": false, "cwd": dir, "env": map[string]any{"TERMSTEAD_TEST_VAR": "fromenv"},
	}}
	b, err := Open(context.Background(), term.OpenRequest{Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := b.Write([]byte("echo marker-$((20+22)) $TERMSTEAD_TEST_VAR $TERM; stty size; pwd\r")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, b, &out, "marker-42 fromenv xterm-256color")
	readUntil(t, b, &out, "30 100")
	real, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(out.String(), dir) && !strings.Contains(out.String(), real) {
		readUntil(t, b, &out, filepath.Base(dir))
	}
	if _, err := b.Write([]byte("exit 7\r")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := b.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("shell did not exit")
		}
	}
	if code := b.(term.ExitCoder).ExitCode(); code != 7 {
		t.Fatalf("exit code %d", code)
	}
}

func TestLocalShellSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	conn := &model.Connection{Protocol: model.ProtoLocal, Options: model.Options{"shell": "/bin/sh", "loginShell": false}}
	b, err := Open(context.Background(), term.OpenRequest{Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var out bytes.Buffer
	b.Write([]byte("sleep 30; echo after-sleep\r"))
	time.Sleep(300 * time.Millisecond)
	// INT goes to the foreground process group (sleep), not just the shell.
	if err := b.(term.Signaler).Signal("INT"); err != nil {
		t.Fatal(err)
	}
	b.Write([]byte("echo alive-$((1+1))\r"))
	readUntil(t, b, &out, "alive-2")
	if strings.Contains(out.String(), "after-sleep\r\n") && !strings.Contains(out.String(), "echo after-sleep") {
		t.Fatal("sleep was not interrupted")
	}
}

func TestLocalShellCloseKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	conn := &model.Connection{Protocol: model.ProtoLocal, Options: model.Options{"shell": "sh", "loginShell": false}}
	b, err := Open(context.Background(), term.OpenRequest{Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	lb := b.(*backend)
	b.Close()
	select {
	case <-lb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("process survived Close")
	}
}

func TestShellDetection(t *testing.T) {
	shells := Shells(context.Background())
	if len(shells) == 0 {
		t.Fatal("no shells detected")
	}
	ids := map[string]bool{}
	for _, s := range shells {
		if s.ID == "" || s.Name == "" || s.Path == "" || s.Args == nil {
			t.Fatalf("incomplete shell %+v", s)
		}
		if ids[s.ID] {
			t.Fatalf("duplicate id %q", s.ID)
		}
		ids[s.ID] = true
		if _, err := os.Stat(s.Path); err != nil {
			t.Fatalf("shell path %q: %v", s.Path, err)
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" && runtime.GOOS != "windows" {
		if _, err := os.Stat(sh); err == nil && shells[0].Path != sh {
			t.Fatalf("$SHELL %q is not first: %+v", sh, shells[0])
		}
	}
	if _, err := resolveShell(context.Background(), "definitely-not-a-shell-xyz"); err == nil {
		t.Fatal("unknown shell resolved")
	}
	if s, err := resolveShell(context.Background(), shells[0].ID); err != nil || s.Path != shells[0].Path {
		t.Fatalf("resolve by id: %+v %v", s, err)
	}
}

func TestDecodeWSLList(t *testing.T) {
	utf16le := func(s string, bom bool) []byte {
		var b []byte
		if bom {
			b = append(b, 0xff, 0xfe)
		}
		for _, r := range s {
			b = append(b, byte(r), byte(r>>8))
		}
		return b
	}
	for _, bom := range []bool{false, true} {
		got := decodeWSLList(utf16le("Ubuntu-24.04\r\ndocker-desktop\r\nDebian\r\n\r\n", bom))
		if strings.Join(got, ",") != "Ubuntu-24.04,Debian" {
			t.Fatalf("bom=%v: %q", bom, got)
		}
	}
	if got := decodeWSLList([]byte("Ubuntu\nkali-linux\n")); strings.Join(got, ",") != "Ubuntu,kali-linux" {
		t.Fatalf("utf-8: %q", got)
	}
}

func TestWorkingDirValidation(t *testing.T) {
	if _, err := workingDir("/definitely/not/here"); err == nil {
		t.Fatal("missing directory accepted")
	}
	home, _ := os.UserHomeDir()
	if d, err := workingDir("~"); err != nil || d != home {
		t.Fatalf("~ → %q %v", d, err)
	}
}
