package transfer_test

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/transfer"
	"github.com/plzcloseyoureyes/astraterm/internal/vfs"
)

// evilFS is a malicious server: its listing of /tree names entries "..", "../../escape.txt", "a/b", a symlink
// "x" to a folder outside the destination AND a folder "x" (same name) containing "passwd". A recursive download
// must never write outside the destination folder, nor through the symlink it recreated.
type evilFS struct {
	vfs.FS
	outside string
}

func ent(name, p, typ string) *vfs.Entry {
	mode := map[string]uint32{"file": 0o100644, "dir": 0o040755, "symlink": 0o120777}[typ]
	return &vfs.Entry{Name: name, Path: p, Type: typ, Mode: mode, Size: 6, Mtime: time.Now()}
}

func (e *evilFS) List(ctx context.Context, dir string) ([]*vfs.Entry, error) {
	switch dir {
	case "/tree":
		x := ent("x", "/tree/x", "symlink")
		x.LinkTarget = e.outside
		return []*vfs.Entry{ent("..", "/", "dir"), ent("../../escape.txt", "/escape.txt", "file"),
			ent("a/b", "/tree/a/b", "file"), x, ent("x", "/tree/x", "dir"), ent("ok.txt", "/tree/ok.txt", "file")}, nil
	case "/tree/x":
		return []*vfs.Entry{ent("passwd", "/tree/x/passwd", "file")}, nil
	}
	return []*vfs.Entry{ent("..", "/", "dir"), ent("passwd", dir+"/passwd", "file")}, nil
}

func (e *evilFS) Lstat(ctx context.Context, p string) (*vfs.Entry, error) {
	if p == "/tree" {
		return ent("tree", p, "dir"), nil
	}
	return ent(vfs.BaseName(p), p, "file"), nil
}
func (e *evilFS) Stat(ctx context.Context, p string) (*vfs.Entry, error) { return e.Lstat(ctx, p) }
func (e *evilFS) Readlink(ctx context.Context, p string) (string, error) { return e.outside, nil }
func (e *evilFS) Open(ctx context.Context, p string, off int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("PWNED!"[off:])), nil
}

func TestMaliciousListingCannotEscapeDestination(t *testing.T) {
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
	root := realTemp(t)
	dst, outside := root+"/dst/deep", root+"/outside"
	os.MkdirAll(dst, 0o755)
	os.MkdirAll(outside, 0o755)
	src := reg.Register(user, &vfs.Handle{Kind: "sftp", Driver: "evil", FS: &evilFS{FS: local.FS, outside: outside}, Home: "/"})
	m := transfer.New(d, reg)
	in, err := m.Create(ctx, user, transfer.Request{SrcFS: src.ID, SrcPaths: []string{"/tree"}, DstFS: local.ID,
		DstDir: vfs.HostToAPI(dst), Overwrite: "overwrite"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for in.FinishedAt == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		in, _ = m.Get(user, in.ID)
	}
	if in.FinishedAt == nil {
		t.Fatalf("transfer did not finish: %+v", in)
	}
	for _, p := range []string{root + "/escape.txt", root + "/dst/escape.txt", outside + "/passwd", root + "/passwd",
		dst + "/passwd", dst + "/tree/a/b"} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("malicious listing wrote %s (transfer %+v)", p, in)
		}
	}
	if b, err := os.ReadFile(dst + "/tree/ok.txt"); err != nil || string(b) != "PWNED!" {
		t.Fatalf("the regular file was not copied: %q %v", b, err)
	}
	var names []string
	filepathWalk(root, &names)
	t.Logf("files: %v; transfer %s %q", names, in.State, in.Errors)
}

func filepathWalk(root string, out *[]string) {
	_ = fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		*out = append(*out, p)
		return nil
	})
}
