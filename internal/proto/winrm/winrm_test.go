package winrm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

func TestLineEditor(t *testing.T) {
	e := lineEditor{echo: true}
	echo, lines := e.feed([]byte("dir C:\\x\x7f\r"))
	if len(lines) != 1 || lines[0] != `dir C:\` {
		t.Fatalf("lines = %q", lines)
	}
	if string(echo) != "dir C:\\x\b \b\r\n" {
		t.Fatalf("echo = %q", echo)
	}
	// Arrow keys never reach the line (they used to insert "[A").
	echo, lines = e.feed([]byte("a\x1b[Db\x1bOC\r"))
	if len(lines) != 1 || lines[0] != "ab" || strings.Contains(string(echo), "[") {
		t.Fatalf("escape handling: lines %q echo %q", lines, echo)
	}
	// History: Up recalls previous lines, Down returns to the edited one.
	_, _ = e.feed([]byte("partial"))
	echo, lines = e.feed([]byte("\x1b[A"))
	if len(lines) != 0 || !strings.HasSuffix(string(echo), "ab") {
		t.Fatalf("up: %q", echo)
	}
	_, _ = e.feed([]byte("\x1b[A\x1b[B\x1b[B"))
	if string(e.line) != "partial" {
		t.Fatalf("down restores the typed line, got %q", string(e.line))
	}
	// Ctrl+U, Ctrl+W, multi-byte characters, CR LF pastes.
	e.feed([]byte("\x15"))
	echo, lines = e.feed([]byte("héllo wörld\x17x\r\nnext\r"))
	if len(lines) != 2 || lines[0] != "héllo x" || lines[1] != "next" {
		t.Fatalf("lines = %q (echo %q)", lines, echo)
	}
	// Ctrl+C drops the line; Ctrl+D on an empty line exits.
	_, lines = e.feed([]byte("oops\x03\x04"))
	if len(lines) != 1 || lines[0] != "exit" {
		t.Fatalf("ctrl-c/ctrl-d lines = %q", lines)
	}
	// Without local echo nothing is echoed but editing still works.
	q := lineEditor{}
	echo, lines = q.feed([]byte("secret\x7fT\r"))
	if len(echo) != 0 || len(lines) != 1 || lines[0] != "secreT" {
		t.Fatalf("no-echo: %q %q", echo, lines)
	}
}

func TestNewlineNormalizer(t *testing.T) {
	var n newlineNormalizer
	got := string(n.normalize([]byte("a\nb\r\nc\r"))) + string(n.normalize([]byte("\nd\n")))
	if got != "a\r\nb\r\nc\r\nd\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestInterpreterCommand(t *testing.T) {
	if interpreterCommand("cmd") != "cmd.exe" || !strings.HasPrefix(interpreterCommand(""), "powershell.exe") {
		t.Fatal("interpreter mapping")
	}
}

func TestClassifyWinrmError(t *testing.T) {
	if !term.IsPermanent(classifyWinrmError(errors.New("http error 401: "))) {
		t.Error("401 should be permanent")
	}
	if term.IsPermanent(classifyWinrmError(errors.New("dial tcp: i/o timeout"))) {
		t.Error("network errors must stay retryable")
	}
}

// ---- mock WS-Management server --------------------------------------------------------------------------------------

// wsman implements the WinRM shell operations masterzen/winrm uses (Create, Command, Send, Receive, Signal, Delete)
// with a toy interpreter: every stdin line is answered "OUT:<line>", "err" writes to stderr, "exit N" ends it.
type wsman struct {
	user, pass string

	mu       sync.Mutex
	out      [][2]string // pending (stream, data)
	ended    bool
	exitCode int
	signaled bool
	deleted  bool
	stdin    []string
	wake     chan struct{}
}

func newWSMan(user, pass string) *wsman {
	w := &wsman{user: user, pass: pass, wake: make(chan struct{}, 1)}
	w.out = append(w.out, [2]string{"stdout", "Windows PowerShell\nPS C:\\> "})
	return w
}

var (
	actionRe = regexp.MustCompile(`Action[^>]*>([^<]+)<`)
	stdinRe  = regexp.MustCompile(`<rsp:Stream[^>]*Name="stdin"[^>]*>([^<]*)</rsp:Stream>`)
)

const envelopeOpen = `<s:Envelope xml:lang="en-US" xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:x="http://schemas.xmlsoap.org/ws/2004/09/transfer" xmlns:w="http://schemas.dmtf.org/wbem/wsman/1/wsman.xsd" xmlns:rsp="http://schemas.microsoft.com/wbem/wsman/1/windows/shell" xmlns:p="http://schemas.microsoft.com/wbem/wsman/1/wsman.xsd">`

func envelope(action, body string) string {
	return envelopeOpen + `<s:Header><a:Action>` + action + `</a:Action><a:MessageID>uuid:1</a:MessageID></s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
}

const timeoutFault = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:w="http://schemas.dmtf.org/wbem/wsman/1/wsman.xsd"><s:Header><a:Action>http://schemas.dmtf.org/wbem/wsman/1/wsman/fault</a:Action></s:Header><s:Body><s:Fault><s:Code><s:Value>s:Receiver</s:Value><s:Subcode><s:Value>w:TimedOut</s:Value></s:Subcode></s:Code><s:Reason><s:Text xml:lang="en-US">The WS-Management service cannot complete the operation within the time specified in OperationTimeout.</s:Text></s:Reason></s:Fault></s:Body></s:Envelope>`

func (w *wsman) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != w.user || p != w.pass {
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m := actionRe.FindSubmatch(body)
	if m == nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	action := string(m[1])
	rw.Header().Set("Content-Type", "application/soap+xml;charset=UTF-8")
	const shell = "http://schemas.microsoft.com/wbem/wsman/1/windows/shell/"
	switch action {
	case "http://schemas.xmlsoap.org/ws/2004/09/transfer/Create":
		fmt.Fprint(rw, envelope("http://schemas.xmlsoap.org/ws/2004/09/transfer/CreateResponse", `<rsp:Shell><rsp:ShellId>SHELL-1</rsp:ShellId></rsp:Shell>`))
	case shell + "Command":
		fmt.Fprint(rw, envelope(shell+"CommandResponse", `<rsp:CommandResponse><rsp:CommandId>CMD-1</rsp:CommandId></rsp:CommandResponse>`))
	case shell + "Send":
		if sm := stdinRe.FindSubmatch(body); sm != nil {
			data, _ := base64.StdEncoding.DecodeString(string(sm[1]))
			w.input(string(data))
		}
		fmt.Fprint(rw, envelope(shell+"SendResponse", `<rsp:SendResponse/>`))
	case shell + "Receive":
		w.receive(rw, shell)
	case shell + "Signal":
		w.mu.Lock()
		w.signaled = true
		w.mu.Unlock()
		fmt.Fprint(rw, envelope(shell+"SignalResponse", `<rsp:SignalResponse/>`))
	case "http://schemas.xmlsoap.org/ws/2004/09/transfer/Delete":
		w.mu.Lock()
		w.deleted = true
		w.mu.Unlock()
		fmt.Fprint(rw, envelope("http://schemas.xmlsoap.org/ws/2004/09/transfer/DeleteResponse", ``))
	default:
		rw.WriteHeader(http.StatusBadRequest)
	}
}

func (w *wsman) input(data string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSuffix(data, "\r\n"), "\r\n") {
		w.stdin = append(w.stdin, line)
		switch {
		case strings.HasPrefix(line, "exit"):
			w.ended = true
			w.exitCode, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "exit")))
		case line == "err":
			w.out = append(w.out, [2]string{"stderr", "boom\n"})
		default:
			w.out = append(w.out, [2]string{"stdout", "OUT:" + line + "\nPS C:\\> "})
		}
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *wsman) receive(rw http.ResponseWriter, shell string) {
	deadline := time.After(time.Second)
	for {
		w.mu.Lock()
		if len(w.out) > 0 || w.ended {
			var sb strings.Builder
			for _, o := range w.out {
				fmt.Fprintf(&sb, `<rsp:Stream Name="%s" CommandId="CMD-1">%s</rsp:Stream>`, o[0], base64.StdEncoding.EncodeToString([]byte(o[1])))
			}
			w.out = nil
			state := `<rsp:CommandState CommandId="CMD-1" State="` + shell + `CommandState/Running"></rsp:CommandState>`
			if w.ended {
				state = `<rsp:CommandState CommandId="CMD-1" State="` + shell + `CommandState/Done"><rsp:ExitCode>` + strconv.Itoa(w.exitCode) + `</rsp:ExitCode></rsp:CommandState>`
			}
			w.mu.Unlock()
			fmt.Fprint(rw, envelope(shell+"ReceiveResponse", `<rsp:ReceiveResponse>`+sb.String()+state+`</rsp:ReceiveResponse>`))
			return
		}
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-deadline:
			rw.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(rw, timeoutFault)
			return
		}
	}
}

func startMock(t *testing.T, user, pass string) (*wsman, *model.Connection, dialFunc) {
	t.Helper()
	w := newWSMan(user, pass)
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	conn := &model.Connection{Protocol: model.ProtoWinRM, Host: host, Port: port, Username: user, Options: model.Options{"winrmAuth": "basic"}}
	dial := func(network, addr string) (net.Conn, error) { return net.Dial(network, addr) }
	return w, conn, dial
}

type collector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func collect(r io.Reader) *collector {
	c := &collector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			c.mu.Lock()
			c.buf = append(c.buf, buf[:n]...)
			if err != nil {
				c.err = err
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *collector) until(d time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *collector) error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// TestWinRMShellLifecycle runs the backend (masterzen/winrm underneath) against the mock server: banner, local echo,
// line submission, history, stderr, the interpreter's exit code, and cleanup of the remote shell.
func TestWinRMShellLifecycle(t *testing.T) {
	w, conn, dial := startMock(t, "admin", "p4ss")
	released := make(chan struct{})
	b, err := start(context.Background(), conn, "p4ss", dial, true, func() { close(released) })
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col := collect(b)
	if out := col.until(5*time.Second, func(o []byte) bool { return bytes.Contains(o, []byte("PS C:\\> ")) }); !bytes.Contains(out, []byte("Windows PowerShell\r\nPS C:\\> ")) {
		t.Fatalf("banner (CR LF normalised) = %q", out)
	}
	if _, err := b.Write([]byte("dir\r")); err != nil {
		t.Fatal(err)
	}
	if out := col.until(5*time.Second, func(o []byte) bool { return bytes.Contains(o, []byte("OUT:dir")) }); !bytes.Contains(out, []byte("dir\r\nOUT:dir\r\n")) {
		t.Fatalf("echo + output = %q", out)
	}
	_, _ = b.Write([]byte("\x1b[A\r")) // history: run "dir" again
	_, _ = b.Write([]byte("err\r"))
	if out := col.until(5*time.Second, func(o []byte) bool { return bytes.Contains(o, []byte("boom")) }); !bytes.Contains(out, []byte("\x1b[31mboom\r\n\x1b[0m")) {
		t.Fatalf("stderr = %q", out)
	}
	_, _ = b.Write([]byte("exit 5\r"))
	col.until(5*time.Second, func([]byte) bool { return false })
	if err := col.error(); err != io.EOF {
		t.Fatalf("end = %v, want EOF", err)
	}
	if b.ExitCode() != 5 {
		t.Fatalf("exit code = %d, want 5", b.ExitCode())
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("route not released")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.Join(w.stdin, "|") != "dir|dir|err|exit 5" {
		t.Errorf("stdin lines = %q", w.stdin)
	}
	if !w.deleted {
		t.Error("remote shell not deleted")
	}
}

func TestWinRMCloseCleansUp(t *testing.T) {
	w, conn, dial := startMock(t, "admin", "p4ss")
	b, err := start(context.Background(), conn, "p4ss", dial, true, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col := collect(b)
	col.until(5*time.Second, func(o []byte) bool { return bytes.Contains(o, []byte("PS C:\\> ")) })
	b.Close()
	col.until(3*time.Second, func([]byte) bool { return false })
	if err := col.error(); err != io.EOF {
		t.Fatalf("after Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := w.signaled && w.deleted
		w.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Close did not terminate the command and delete the shell")
}

func TestWinRMWrongPasswordIsPermanent(t *testing.T) {
	_, conn, dial := startMock(t, "admin", "p4ss")
	_, err := start(context.Background(), conn, "wrong", dial, true, nil)
	if err == nil || !term.IsPermanent(err) {
		t.Fatalf("wrong password: %v (permanent=%v)", err, term.IsPermanent(err))
	}
}

// TestServeMockWSMan is a manual end-to-end harness: with NEXTERM_SERVE_WSMAN=host:port it serves the mock WinRM
// endpoint (Basic auth admin / p4ss, path /wsman) until the process is killed.
func TestServeMockWSMan(t *testing.T) {
	addr := os.Getenv("NEXTERM_SERVE_WSMAN")
	if addr == "" {
		t.Skip("set NEXTERM_SERVE_WSMAN=host:port to serve a mock WinRM endpoint for manual end-to-end tests")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("mock WS-Management listening on", ln.Addr())
	// A fresh interpreter per shell (each session creates its own).
	var mu sync.Mutex
	var cur *wsman
	_ = http.Serve(ln, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body, _ := io.ReadAll(r.Body)
		if cur == nil || bytes.Contains(body, []byte("transfer/Create")) {
			cur = newWSMan("admin", "p4ss")
		}
		w := cur
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		w.ServeHTTP(rw, r)
	}))
}
