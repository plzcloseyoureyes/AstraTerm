package transfer_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/events"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
	"github.com/nexterm/nexterm/internal/transfer"
	"github.com/nexterm/nexterm/internal/vfs"
)

// slowFS reads slowly so a transfer is still running when the "server" stops.
type slowFS struct{ vfs.FS }

func (s *slowFS) Open(ctx context.Context, p string, off int64) (io.ReadCloser, error) {
	r, err := s.FS.Open(ctx, p, off)
	if err != nil {
		return nil, err
	}
	return &slowReader{r}, nil
}

type slowReader struct{ io.ReadCloser }

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(60 * time.Millisecond)
	return s.ReadCloser.Read(p[:min(len(p), 16<<10)]) // ~270 KiB/s: 4 MiB take ~15 s
}

func TestTransfersSurviveRestart(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	user := &model.User{ID: "u1", Username: "alice", Role: model.RoleAdmin}
	start := func(ctx context.Context) (*vfs.Registry, *transfer.Manager) {
		d := &app.Deps{Ctx: ctx, Log: log, Store: st, Cfg: &config.Config{Mode: config.ModeDesktop, DataDir: dataDir},
			Events: events.NewHub(ctx, log)}
		reg := vfs.NewRegistry(d, nil)
		return reg, transfer.New(d, reg)
	}
	srcDir, dstDir := realTemp(t), realTemp(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB
	os.WriteFile(srcDir+"/big.bin", payload, 0o644)
	os.WriteFile(srcDir+"/small.txt", []byte("small"), 0o644)

	// Run 1: the transfer is cut by the shutdown.
	ctx1, stop := context.WithCancel(context.Background())
	reg, m := start(ctx1)
	local, err := reg.Open(ctx1, user, vfs.OpenRequest{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	slow := reg.Register(user, &vfs.Handle{Kind: "local", Driver: "local", FS: &slowFS{local.FS}, Home: "/"})
	in, err := m.Create(ctx1, user, transfer.Request{SrcFS: slow.ID, SrcPaths: []string{srcDir}, DstFS: local.ID,
		DstDir: dstDir, Overwrite: "overwrite"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		in, _ = m.Get(user, in.ID)
		if in.DoneBytes > 256<<10 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(6 * time.Second) // let a progress record happen
	stop()
	time.Sleep(200 * time.Millisecond)

	// Run 2: the transfer is listed as interrupted and resumable.
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	_, m2 := start(ctx2)
	list := m2.List(user)
	if len(list) != 1 || !list[0].Interrupted || !list[0].Resumable || list[0].State != "error" || list[0].FinishedAt == nil {
		t.Fatalf("after restart: %+v", list)
	}
	if other := m2.List(&model.User{ID: "u2"}); len(other) != 0 {
		t.Fatal("foreign transfer visible")
	}
	re, err := m2.Retry(ctx2, user, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for re.FinishedAt == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		re, _ = m2.Get(user, re.ID)
	}
	if re.State != "done" {
		t.Fatalf("retry: %+v", re)
	}
	name := filepath.Base(srcDir)
	if b, _ := os.ReadFile(dstDir + "/" + name + "/big.bin"); !bytes.Equal(b, payload) {
		t.Fatal("resumed content differs")
	}
	if l := m2.List(user); len(l) != 1 || l[0].ID != re.ID {
		t.Fatalf("the interrupted record must be replaced by the retry: %+v", l)
	}
	if _, err := m2.Get(user, list[0].ID); err == nil {
		t.Fatal("old record still there")
	}
}
