//go:build !windows

package keys

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAgentEndpointPrivateDir: the agent socket goes into the data directory only while that directory is private
// to this user; otherwise it moves to a private 0700 directory in the temp directory.
func TestAgentEndpointPrivateDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "nxk") // short: the socket path must fit sun_path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p, err := agentEndpoint(dir)
	if err != nil || filepath.Dir(p) != dir {
		t.Fatalf("private data dir: %q %v", p, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err = agentEndpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(filepath.Dir(p)) })
	if filepath.Dir(p) == dir {
		t.Fatalf("socket placed in a world-readable directory: %s", p)
	}
	if err := checkPrivateDir(filepath.Dir(p)); err != nil {
		t.Fatalf("fallback directory: %v", err)
	}
	// Stopping removes the socket and the (then empty) fallback directory.
	ln, err := listenAgent(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v %v", fi, err)
	}
	ln.Close()
	cleanupEndpoint(p)
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Fatalf("fallback directory left behind: %v", err)
	}
	// A symlink is never trusted as the private directory.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDir(link); err == nil {
		t.Fatal("symlink accepted")
	}
}
