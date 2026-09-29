package vfs_test

// FILE-2 across login shells and the sudo password handshake, against a private container (not part of the shared
// lab): TERMSTEAD_TESTENV=1 and a container "termstead-files-backend-shells" publishing sshd on 127.0.0.1:23020 with
// users ubash / uzsh / ufish / umksh / uash (password "test", login shell as named) and sglob (bash, sudo with
// "Defaults timestamp_type=global"). Build and run (a private container, not the shared lab):
//
//	docker build -t termstead-files-backend-shells -f internal/vfs/testdata/shells.Dockerfile internal/vfs/testdata
//	docker run -d --name termstead-files-backend-shells -p 127.0.0.1:23020:22 termstead-files-backend-shells
//
// The test is skipped when the port is closed.

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/server/servertest"
)

const shellsAddr = "127.0.0.1:23020"

func shellsLab(t *testing.T) {
	labOnly(t)
	c, err := net.DialTimeout("tcp", shellsAddr, time.Second)
	if err != nil {
		t.Skip("shells container not running on " + shellsAddr)
	}
	c.Close()
}

func TestShellsLab(t *testing.T) {
	shellsLab(t)
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	startResponder(t, env, admin, map[string]string{"password": "test"})
	users := map[string]bool{"ubash": true, "uzsh": true, "ufish": true, "umksh": true, "uash": true}
	for user, follows := range users {
		t.Run(user, func(t *testing.T) {
			id := createConn(t, admin, map[string]any{"name": user, "protocol": "ssh", "host": "127.0.0.1", "port": 23020,
				"username": user, "authMethod": "password", "secrets": map[string]string{"password": "test"}})
			var sess model.RuntimeSession
			admin.MustJSON("POST", "/api/sessions", map[string]any{"connectionId": id, "cols": 100, "rows": 30}, &sess)
			defer admin.JSON("DELETE", "/api/sessions/"+sess.ID, nil, nil)
			waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/home/"+user }, "initial cwd")
			time.Sleep(300 * time.Millisecond)
			admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "echo nx-4'2'\r"}, nil)
			if follows {
				admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "cd /tmp\r"}, nil)
				waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/tmp" }, "cwd after cd")
			}
			if follows {
				// A reconnect starts a new shell: the old folder is dropped and the integration installed again.
				admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/reconnect", nil, nil)
				waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool {
					return s.State == model.StateConnected && s.Cwd == "/home/"+user
				}, "cwd after reconnect")
				admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "cd /usr\r"}, nil)
				waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.Cwd == "/usr" }, "cwd after reconnect + cd")
			}
			time.Sleep(500 * time.Millisecond)
			_, sb := admin.Do("GET", "/api/sessions/"+sess.ID+"/scrollback?raw=0", nil)
			for _, leak := range []string{"nx7", "OPTIND", "uname", "PROMPT_COMMAND", "--on-variable"} {
				if bytes.Contains(sb, []byte(leak)) {
					t.Fatalf("injected line visible (%s):\n%s", leak, sb)
				}
			}
			if !bytes.Contains(sb, []byte("nx-42")) {
				t.Fatalf("typed command missing:\n%s", sb)
			}
			t.Logf("%s:\n%s", user, sb)
		})
	}
}

// With cached sudo credentials shared by every process of the user (timestamp_type=global), sudo does not ask for
// the password: it must never end up in the saved file.
func TestShellsLabSudoGlobalTimestamp(t *testing.T) {
	shellsLab(t)
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	startResponder(t, env, admin, map[string]string{"password": "test"})
	id := createConn(t, admin, map[string]any{"name": "sglob", "protocol": "ssh", "host": "127.0.0.1", "port": 23020,
		"username": "sglob", "authMethod": "password", "secrets": map[string]string{"password": "test"}})
	// Cache the credentials from a terminal, as the user would.
	var sess model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{"connectionId": id, "cols": 100, "rows": 30}, &sess)
	defer admin.JSON("DELETE", "/api/sessions/"+sess.ID, nil, nil)
	waitSession(t, admin, sess.ID, func(s model.RuntimeSession) bool { return s.State == model.StateConnected }, "connected")
	time.Sleep(time.Second)
	admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "echo test | sudo -S -v; echo cached-$?\r"}, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, sb := admin.Do("GET", "/api/sessions/"+sess.ID+"/scrollback?raw=0", nil)
		if bytes.Contains(sb, []byte("cached-0")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not cache sudo credentials:\n%s", sb)
		}
		time.Sleep(100 * time.Millisecond)
	}
	f, _ := openFS(t, admin, map[string]any{"connectionId": id})
	target := "/etc/termstead-glob-" + randHex(3) + ".conf"
	for i, content := range []string{"a=1\n", "b=22\n"} { // create, then overwrite
		var e model.FileEntry
		f.must("PUT", "write", nil, map[string]any{"path": target, "content": content, "sudo": true}, &e)
		var rd struct{ Content string }
		f.must("GET", "read", q("path", target), nil, &rd)
		if rd.Content != content || strings.Contains(rd.Content, "test") {
			t.Fatalf("write %d: file content %q (password leaked into the file?)", i, rd.Content)
		}
	}
	admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/input", map[string]string{"data": "echo test | sudo -S rm -f " + target + "\r"}, nil)

}
