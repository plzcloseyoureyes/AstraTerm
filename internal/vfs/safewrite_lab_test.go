package vfs_test

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

// sshRun runs a command on ssh1 as test (outside AstraTerm) and returns its trimmed output.
func sshRun(t *testing.T, cmd string) string {
	t.Helper()
	c, err := ssh.Dial("tcp", "127.0.0.1:22022", &ssh.ClientConfig{User: "test", Auth: []ssh.AuthMethod{ssh.Password("test")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey()}) //nolint:gosec // lab
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, _ := c.NewSession()
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	if err != nil {
		t.Fatalf("%s: %v %s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLabSafeWrite(t *testing.T) {
	labOnly(t)
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	startResponder(t, env, admin, map[string]string{"password": "test"})
	dir := "/tmp/astraterm-sw-" + randHex(4)
	sshRun(t, "mkdir -p "+dir+" && printf old > "+dir+"/f.txt && chmod 640 "+dir+"/f.txt && printf x > "+dir+"/h.txt && ln "+dir+"/h.txt "+dir+"/h2.txt")
	defer sshRun(t, "rm -rf "+dir)
	for _, mode := range []string{"sftp", "scp"} {
		id := createConn(t, admin, map[string]any{"name": "sw-" + mode, "protocol": "ssh", "host": "127.0.0.1", "port": 22022,
			"username": "test", "authMethod": "password", "secrets": map[string]string{"password": "test"},
			"options": map[string]any{"sshBrowser": mode}})
		f, _ := openFS(t, admin, map[string]any{"connectionId": id})
		ino := sshRun(t, "stat -c %i "+dir+"/f.txt")
		var e model.FileEntry
		f.must("PUT", "write", nil, map[string]any{"path": dir + "/f.txt", "content": "new " + mode}, &e)
		if got := sshRun(t, "cat "+dir+"/f.txt; echo; stat -c '%a %i' "+dir+"/f.txt"); !strings.HasPrefix(got, "new "+mode+"\n640 ") || strings.HasSuffix(got, " "+ino) {
			t.Fatalf("%s: atomic save expected (new inode, mode kept): %q (old inode %s)", mode, got, ino)
		}
		hino := sshRun(t, "stat -c %i "+dir+"/h.txt")
		f.must("PUT", "write", nil, map[string]any{"path": dir + "/h.txt", "content": "via " + mode}, &e)
		if got := sshRun(t, "cat "+dir+"/h2.txt; echo; stat -c %i "+dir+"/h.txt"); got != "via "+mode+"\n"+hino {
			t.Fatalf("%s: hard-linked file must be rewritten in place: %q", mode, got)
		}
		if leftovers := sshRun(t, "ls -A "+dir); strings.Contains(leftovers, "astraterm-tmp") {
			t.Fatalf("temporary file left: %s", leftovers)
		}
	}
	// Browse as root: saves keep the inode (system files: labels, ACLs, xattrs).
	id := createConn(t, admin, map[string]any{"name": "sw-root", "protocol": "ssh", "host": "127.0.0.1", "port": 22022,
		"username": "test", "authMethod": "password", "secrets": map[string]string{"password": "test", "sudoPassword": "test"}})
	root, _ := openFS(t, admin, map[string]any{"connectionId": id, "sudo": true})
	ino := sshRun(t, "stat -c %i "+dir+"/f.txt")
	var e model.FileEntry
	root.must("PUT", "write", nil, map[string]any{"path": dir + "/f.txt", "content": "as root"}, &e)
	if got := sshRun(t, "cat "+dir+"/f.txt; echo; stat -c '%i %U' "+dir+"/f.txt"); got != "as root\n"+ino+" test" {
		t.Fatalf("root save must be in place: %q", got)
	}
}
