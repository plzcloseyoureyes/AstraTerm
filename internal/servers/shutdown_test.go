package servers

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// WaitShutdown (called by internal/server's Close) returns only once the syslog file has been flushed and closed, so
// messages received just before Termstead exits are on disk.
func TestWaitShutdownFlushesSyslogFile(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	port := freePort(t, "both")
	logDir := filepath.Join(h.dir, "syslog-files")
	h.configure(u, KindSyslog, map[string]any{"port": port, "logToFile": true, "logDir": logDir})
	h.start(u, KindSyslog)
	udp, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_, _ = udp.Write([]byte("<34>Oct 11 22:14:15 mymachine su: last words before shutdown"))
	var page SyslogPage
	for deadline := time.Now().Add(5 * time.Second); len(page.Messages) == 0 && time.Now().Before(deadline); {
		h.must(u, http.MethodGet, "/api/servers/syslog/messages", nil, &page, http.StatusOK)
		time.Sleep(10 * time.Millisecond)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("message not received: %+v", page)
	}

	h.cancel() // Termstead shuts down
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := WaitShutdown(ctx, h.d); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(logDir, "syslog-*.log"))
	if len(files) != 1 {
		t.Fatalf("log files %v", files)
	}
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), "last words before shutdown") {
		t.Fatalf("log file not flushed at shutdown: %q", b)
	}
	if err := WaitShutdown(ctx, h.d); err != nil { // finished managers are forgotten; waiting again is a no-op
		t.Fatal(err)
	}
}
