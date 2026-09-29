package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Close returns only after connections that were busy with a query are closed too, so the WAL is checkpointed and
// the -wal / -shm files are gone (regression: a query in flight during Close left them behind, recreated after the
// data directory had been removed).
func TestCloseWaitsForBusyConnections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "termstead.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.DB.Conn(context.Background()) // a connection in use while Close runs
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE busy (x)"); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		conn.Close()
	}()
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Fatal("Close returned while a connection was still open")
	}
	if n := s.DB.Stats().OpenConnections; n != 0 {
		t.Fatalf("%d connections still open", n)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Errorf("%s left behind (err %v)", suffix, err)
		}
	}
}
