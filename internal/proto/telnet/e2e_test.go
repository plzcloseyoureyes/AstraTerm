package telnet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/server/servertest"
)

// TestTelnetSessionEndToEnd drives a telnet session through the whole app (REST create + /ws/terminal), connecting to
// the busybox telnetd in the shared docker test environment (127.0.0.1:22023).
func TestTelnetSessionEndToEnd(t *testing.T) {
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run against the docker test environment")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery staple")

	var rs model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{
		"quick": map[string]any{"protocol": "telnet", "host": "127.0.0.1", "port": 22023},
		"cols":  80, "rows": 24, "title": "telnet e2e",
	}, &rs)
	if rs.ID == "" || rs.Protocol != model.ProtoTelnet {
		t.Fatalf("created %+v", rs)
	}

	wsURL := strings.Replace(env.HTTP.URL, "http", "ws", 1) + "/ws/terminal/" + rs.ID + "?offset=0"
	ws, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPClient: admin.HTTP})
	if err != nil {
		t.Fatalf("attach terminal ws: %v", err)
	}
	defer ws.CloseNow()

	deadline := time.Now().Add(15 * time.Second)
	var out bytes.Buffer
	connected := false
	sentCmd := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err == nil {
			switch typ {
			case websocket.MessageBinary:
				out.Write(data)
			case websocket.MessageText:
				var m map[string]any
				_ = json.Unmarshal(data, &m)
				if m["type"] == "state" && m["state"] == "connected" {
					connected = true
				}
			}
		}
		// Once the shell is up (state connected or first output arrived), send the command exactly once.
		if !sentCmd && (connected || out.Len() > 0) {
			sentCmd = true
			if werr := ws.Write(context.Background(), websocket.MessageBinary, []byte("echo TERMSTEAD_E2E\r")); werr != nil {
				t.Fatalf("send command: %v", werr)
			}
		}
		if bytes.Contains(out.Bytes(), []byte("TERMSTEAD_E2E")) {
			break
		}
	}
	if !bytes.Contains(out.Bytes(), []byte("TERMSTEAD_E2E")) {
		t.Fatalf("did not observe command output; got %q", out.String())
	}

	// Clean close.
	admin.MustJSON("DELETE", "/api/sessions/"+rs.ID, nil, nil)
}
