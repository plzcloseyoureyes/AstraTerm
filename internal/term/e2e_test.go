package term_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestE2ERunningServer exercises a running NexTerm binary end to end (enable with NEXTERM_E2E_URL=http://host:port and
// NEXTERM_E2E_SSH=host:port of an SSH server accepting test/test; a fresh server-mode target also needs
// NEXTERM_E2E_SETUP_TOKEN, the ?setup= token from its banner):
// setup/login → events socket answering host-key and password prompts → SSH quick-connect session → terminal
// WebSocket echo → detach / re-attach with a delta → flood while detached / re-attach with a reset → local shell.
func TestE2ERunningServer(t *testing.T) {
	base := os.Getenv("NEXTERM_E2E_URL")
	sshAddr := os.Getenv("NEXTERM_E2E_SSH")
	if base == "" || sshAddr == "" {
		t.Skip("set NEXTERM_E2E_URL and NEXTERM_E2E_SSH to run against a live server")
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	call := func(method, path string, body, out any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NexTerm", "1")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if out != nil && resp.StatusCode < 300 {
			if err := json.Unmarshal(data, out); err != nil {
				t.Fatalf("%s %s: %v (%s)", method, path, err, data)
			}
		}
		if resp.StatusCode >= 300 {
			t.Logf("%s %s → %d %s", method, path, resp.StatusCode, data)
		}
		return resp.StatusCode
	}

	// 1. Setup (fresh data dir) or login.
	var state struct {
		SetupRequired bool `json:"setupRequired"`
	}
	call("GET", "/api/auth/state", nil, &state)
	creds := map[string]string{"username": "e2e", "password": "e2e correct horse battery"}
	if state.SetupRequired {
		setup := map[string]string{"username": creds["username"], "password": creds["password"]}
		if tok := os.Getenv("NEXTERM_E2E_SETUP_TOKEN"); tok != "" { // server mode: the ?setup= token from the banner
			setup["setupToken"] = tok
		}
		if st := call("POST", "/api/auth/setup", setup, nil); st != 200 && st != 201 {
			t.Fatalf("setup: %d", st)
		}
	} else if st := call("POST", "/api/auth/login", creds, nil); st != 200 {
		t.Fatalf("login: %d", st)
	}
	wsBase := strings.Replace(base, "http", "ws", 1)

	// 2. Events socket answering prompts.
	ev, _, err := websocket.Dial(context.Background(), wsBase+"/ws/events", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	defer ev.CloseNow()
	var pmu sync.Mutex
	var prompts []string
	hello := make(chan struct{})
	go func() {
		var once sync.Once
		for {
			_, data, err := ev.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				Type   string `json:"type"`
				Prompt struct {
					ID   string `json:"id"`
					Kind string `json:"kind"`
				} `json:"prompt"`
			}
			json.Unmarshal(data, &m)
			switch m.Type {
			case "hello":
				once.Do(func() { close(hello) })
			case "prompt":
				pmu.Lock()
				prompts = append(prompts, m.Prompt.Kind)
				pmu.Unlock()
				resp := map[string]any{"type": "prompt.response", "id": m.Prompt.ID, "accept": true, "save": true}
				if m.Prompt.Kind == "password" {
					resp["values"], resp["save"] = []string{"test"}, false
				}
				b, _ := json.Marshal(resp)
				ev.Write(context.Background(), websocket.MessageText, b)
			}
		}
	}()
	<-hello

	// 3. SSH quick-connect session.
	host, portStr, _ := net.SplitHostPort(sshAddr)
	port, _ := strconv.Atoi(portStr)
	var rs struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if st := call("POST", "/api/sessions", map[string]any{
		"quick": map[string]any{"protocol": "ssh", "host": host, "port": port, "username": "test"},
		"cols":  100, "rows": 30,
	}, &rs); st != 201 {
		t.Fatalf("create ssh session: %d", st)
	}
	waitState := func(id, want string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			var cur struct {
				State        string `json:"state"`
				StateMessage string `json:"stateMessage"`
			}
			call("GET", "/api/sessions/"+id, nil, &cur)
			if cur.State == want {
				return
			}
			if cur.State == "error" {
				t.Fatalf("session error: %s", cur.StateMessage)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("session %s never reached %s", id, want)
	}
	waitState(rs.ID, "connected")
	pmu.Lock()
	t.Logf("prompts answered: %v", prompts)
	pmu.Unlock()

	// 4. Attach and echo.
	term := attachE2E(t, wsBase, hc, rs.ID, 0)
	if term.mode != "delta" && term.mode != "reset" {
		t.Fatalf("attach mode %q", term.mode)
	}
	term.send("echo hello-nexterm\r")
	term.waitCount("hello-nexterm", 2) // the echoed command line and the command's output
	t.Logf("echo observed at offset %d", term.offset())

	// 5. Detach, produce output, re-attach with the last offset → delta.
	last := term.offset()
	term.close()
	call("POST", "/api/sessions/"+rs.ID+"/input", map[string]string{"data": "echo while-detached-$((40+2))\r"}, nil)
	time.Sleep(500 * time.Millisecond)
	term2 := attachE2E(t, wsBase, hc, rs.ID, last)
	if term2.mode != "delta" || term2.from != last {
		t.Fatalf("re-attach: mode %s from %d (want delta from %d)", term2.mode, term2.from, last)
	}
	term2.waitFor("while-detached-42")
	t.Logf("delta re-attach from %d replayed %d bytes", last, term2.buf.Len())
	term2.close()

	// 6. Flood more than the ring while detached, then re-attach from 0 → reset from the ring tail.
	call("POST", "/api/sessions/"+rs.ID+"/input", map[string]string{"data": "seq 1 1200000; echo flood-done\r"}, nil)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(base + "/api/sessions/" + rs.ID + "/scrollback?raw=0")
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(data), "\nflood-done") {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	term3 := attachE2E(t, wsBase, hc, rs.ID, 0)
	if term3.mode != "reset" || term3.from == 0 {
		t.Fatalf("flooded re-attach: mode %s from %d (want reset from the ring tail)", term3.mode, term3.from)
	}
	term3.waitFor("\nflood-done")
	t.Logf("reset re-attach from %d (head %d) received %d bytes", term3.from, term3.head, term3.buf.Len())
	term3.send("exit\r")
	term3.waitState("disconnected")
	term3.close()
	call("DELETE", "/api/sessions/"+rs.ID, nil, nil)

	// 7. Local shell.
	var ls struct {
		ID string `json:"id"`
	}
	if st := call("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "local"}, "cols": 80, "rows": 24}, &ls); st != 201 {
		t.Fatalf("create local session: %d", st)
	}
	waitState(ls.ID, "connected")
	lt := attachE2E(t, wsBase, hc, ls.ID, 0)
	lt.send("echo local-$((2+3))\r")
	lt.waitFor("local-5")
	lt.close()
	call("DELETE", "/api/sessions/"+ls.ID, nil, nil)
}

type e2eTerm struct {
	t          *testing.T
	ws         *websocket.Conn
	mode       string
	from, head int64
	mu         sync.Mutex
	buf        bytes.Buffer
	states     []string
	done       chan struct{}
}

func attachE2E(t *testing.T, wsBase string, hc *http.Client, id string, offset int64) *e2eTerm {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), fmt.Sprintf("%s/ws/terminal/%s?offset=%d", wsBase, id, offset),
		&websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(16 << 20)
	tm := &e2eTerm{t: t, ws: ws, done: make(chan struct{})}
	attached := make(chan struct{})
	go func() {
		defer close(tm.done)
		var once sync.Once
		var sinceAck int
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				tm.mu.Lock()
				tm.buf.Write(data)
				sinceAck += len(data)
				off := tm.from + int64(tm.buf.Len())
				tm.mu.Unlock()
				if sinceAck >= 64<<10 {
					sinceAck = 0
					tm.ack(off)
				}
				continue
			}
			var m struct {
				Type  string `json:"type"`
				Mode  string `json:"mode"`
				From  int64  `json:"from"`
				Head  int64  `json:"head"`
				State string `json:"state"`
			}
			json.Unmarshal(data, &m)
			tm.mu.Lock()
			switch m.Type {
			case "attach":
				tm.mode, tm.from, tm.head = m.Mode, m.From, m.Head
				tm.buf.Reset()
				once.Do(func() { close(attached) })
			case "attach-end":
				off := tm.from + int64(tm.buf.Len())
				tm.mu.Unlock()
				tm.ack(off)
				tm.mu.Lock()
			case "state":
				tm.states = append(tm.states, m.State)
			}
			tm.mu.Unlock()
		}
	}()
	select {
	case <-attached:
	case <-time.After(10 * time.Second):
		t.Fatal("no attach message")
	}
	return tm
}

func (tm *e2eTerm) ack(off int64) {
	b, _ := json.Marshal(map[string]any{"type": "ack", "offset": off})
	tm.ws.Write(context.Background(), websocket.MessageText, b)
}

func (tm *e2eTerm) send(s string) {
	tm.t.Helper()
	if err := tm.ws.Write(context.Background(), websocket.MessageBinary, []byte(s)); err != nil {
		tm.t.Fatal(err)
	}
}

func (tm *e2eTerm) offset() int64 {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.from + int64(tm.buf.Len())
}

func (tm *e2eTerm) waitFor(s string) {
	tm.t.Helper()
	tm.waitCount(s, 1)
}

func (tm *e2eTerm) waitCount(s string, n int) {
	tm.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		tm.mu.Lock()
		ok := strings.Count(tm.buf.String(), s) >= n
		off := tm.from + int64(tm.buf.Len())
		tm.mu.Unlock()
		if ok {
			tm.ack(off)
			return
		}
		tm.ack(off)
		time.Sleep(50 * time.Millisecond)
	}
	tm.mu.Lock()
	tail := tm.buf.String()
	tm.mu.Unlock()
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	tm.t.Fatalf("timed out waiting for %q; output tail %q", s, tail)
}

func (tm *e2eTerm) waitState(state string) {
	tm.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		tm.mu.Lock()
		for _, s := range tm.states {
			if s == state {
				tm.mu.Unlock()
				return
			}
		}
		tm.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	tm.t.Fatalf("state %s not received", state)
}

func (tm *e2eTerm) close() {
	tm.ws.Close(websocket.StatusNormalClosure, "")
	<-tm.done
}
