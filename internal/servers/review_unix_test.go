//go:build !windows

package servers

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// FIFOs in a shared folder are never opened (an open blocks until a writer appears).
func TestOpenRegularRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skip(err)
	}
	fsys, err := openRootFS(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	done := make(chan error, 1)
	go func() {
		_, err := fsys.OpenRegular("/pipe")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNotRegular) {
			t.Fatalf("OpenRegular(fifo) = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenRegular blocked on a FIFO")
	}
	if err := fsys.checkUploadTarget("/pipe"); !errors.Is(err, errNotRegular) {
		t.Fatalf("upload onto a FIFO: %v", err)
	}
	if err := fsys.checkUploadTarget("/new.bin"); err != nil {
		t.Fatalf("new upload refused: %v", err)
	}
}
