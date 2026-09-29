package servers

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/auth"
)

// Read-only users of the HTTP server cannot upload (the per-user flag used to be ignored), and non-canonical paths
// are redirected.
func TestHTTPReadOnlyUserAndCanonicalPaths(t *testing.T) {
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": port, "readOnly": false, "upload": true,
		"requireAuth": true, "users": []map[string]any{
			{"username": "reader", "password": "reader-pass", "readOnly": true},
			{"username": "writer", "password": "writer-pass"},
		}})
	h.start(u, KindHTTP)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	do := func(method, path, user, pass, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, _ := do(http.MethodPut, "/r.txt", "reader", "reader-pass", "x"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only user PUT: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "r.txt")); err == nil {
		t.Fatal("read-only user stored a file")
	}
	if resp, body := do(http.MethodGet, "/", "reader", "reader-pass", ""); resp.StatusCode != 200 || strings.Contains(body, `name="file"`) {
		t.Fatalf("read-only user listing %d (upload form shown: %v)", resp.StatusCode, strings.Contains(body, `name="file"`))
	}
	if resp, _ := do(http.MethodPut, "/w.txt", "writer", "writer-pass", "x"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("writer PUT: %d", resp.StatusCode)
	}
	if resp, body := do(http.MethodGet, "/", "writer", "writer-pass", ""); !strings.Contains(body, `name="file"`) {
		t.Fatalf("writer listing %d without upload form", resp.StatusCode)
	}
	// Canonical paths.
	resp, _ := do(http.MethodGet, "/sub/../hello.txt", "writer", "writer-pass", "")
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/hello.txt" {
		t.Fatalf("non-canonical GET: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = do(http.MethodGet, "//sub//", "writer", "writer-pass", "")
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/sub/" {
		t.Fatalf("double slashes: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := do(http.MethodPut, "/sub/../x.txt", "writer", "writer-pass", "x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-canonical PUT: %d", resp.StatusCode)
	}
}

func TestLimitKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7":                "192.0.2.7",
		"::ffff:192.0.2.7":         "192.0.2.7",
		"2001:db8:1:2:3:4:5:6":     "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":     "2001:db8:1:2::/64",
		"fe80::1%en0":              "fe80::/64",
		"not an ip":                "not an ip",
		"2001:db8:1:3::1":          "2001:db8:1:3::/64",
		"::1":                      "::/64",
		"2001:0db8:0001:0002::abc": "2001:db8:1:2::/64",
	}
	for in, want := range cases {
		if got := limitKey(in); got != want {
			t.Errorf("limitKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// An IPv6 attacker cannot dodge the lockout by rotating addresses inside its /64, and a valid account does not reset
// the failure count.
func TestLockoutIPv6AndNoResetOnSuccess(t *testing.T) {
	h := mustHash(t, "right-pass")
	db := newUserDB([]User{{Username: "guest", PasswordHash: h}, {Username: "admin", PasswordHash: h}}, &counters{})
	db.sleepFn = nil
	var err error
	for i := 0; i < failMax; i++ {
		_, err = db.checkPassword("2001:db8:1:2::"+strconv.Itoa(i+1), "admin", "guess")
	}
	if !errors.Is(err, errTooManyFails) {
		t.Fatalf("rotating IPv6 addresses not blocked: %v", err)
	}
	if !db.lim.blocked("2001:db8:1:2:dead::beef") || db.lim.blocked("2001:db8:1:3::1") {
		t.Fatal("block does not cover exactly the /64")
	}
	// Interleaving successful guest logins does not reset the count.
	for i := 0; i < failMax-1; i++ {
		if _, err := db.checkPassword("198.51.100.1", "admin", "guess"); !errors.Is(err, errBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if _, err := db.checkPassword("198.51.100.1", "guest", "right-pass"); err != nil {
			t.Fatalf("guest login: %v", err)
		}
	}
	if _, err := db.checkPassword("198.51.100.1", "admin", "guess"); !errors.Is(err, errTooManyFails) {
		t.Fatalf("interleaved successes kept the attacker unblocked: %v", err)
	}
}

func TestClientSetLimits(t *testing.T) {
	var stats counters
	s := newClientSet(&stats, nil)
	s.max, s.maxPerIP = 5, 2
	var refused []string
	s.onLimit = func(addr, why string) { refused = append(refused, addr+": "+why) }
	a1, err := s.add("192.0.2.1:1000", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.add("192.0.2.1:1001", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add("192.0.2.1:1002", nil); !errors.Is(err, errTooManyClients) {
		t.Fatalf("third connection from one address: %v", err)
	}
	if _, err := s.add("[2001:db8::1]:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add("[2001:db8::2]:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add("[2001:db8::3]:1", nil); !errors.Is(err, errTooManyClients) {
		t.Fatalf("third connection from one /64: %v", err)
	}
	s.remove(a1)
	if _, err := s.add("192.0.2.1:1003", nil); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	if _, err := s.add("192.0.2.9:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add("192.0.2.10:1", nil); !errors.Is(err, errTooManyClients) {
		t.Fatalf("total limit: %v", err)
	}
	if len(refused) != 1 { // rate limited
		t.Fatalf("refusals reported %d times: %q", len(refused), refused)
	}
	s.closeAll()
	if _, err := s.add("192.0.2.11:1", nil); !errors.Is(err, errClientsClosed) {
		t.Fatalf("closed set: %v", err)
	}
}

// The syslog buffer is bounded in bytes as well as in messages, clearing releases the memory, and the sender table
// stops growing under spoofed floods.
func TestSyslogMemoryBudget(t *testing.T) {
	m := &Manager{}
	m.subs = newSubscribers(m)
	st := newSyslogStore(m)
	st.maxBytes = 1 << 20
	big := strings.Repeat("x", 60<<10)
	for i := 0; i < 100; i++ {
		msg := parseSyslog([]byte("<14>"+big), time.Now())
		msg.Source = "192.0.2." + strconv.Itoa(i%250)
		st.add(msg, len(big))
	}
	if st.bytes > st.maxBytes || st.n == 0 || st.n > 20 {
		t.Fatalf("buffer holds %d messages / %d bytes (budget %d)", st.n, st.bytes, st.maxBytes)
	}
	page := st.query(syslogQuery{maxSev: -1, facility: -1})
	if len(page.Messages) != st.n || page.Messages[len(page.Messages)-1].ID != 100 {
		t.Fatalf("newest messages not kept: %d messages, last %d", len(page.Messages), page.Messages[len(page.Messages)-1].ID)
	}
	var sum int64
	for i := range page.Messages {
		sum += syslogMsgSize(&page.Messages[i])
	}
	if sum != st.bytes {
		t.Fatalf("byte accounting %d != %d", st.bytes, sum)
	}
	st.clear()
	if st.bytes != 0 || st.n != 0 {
		t.Fatal("clear kept messages")
	}
	for i := range st.ring {
		if st.ring[i].Message != "" {
			t.Fatal("clear kept message memory")
		}
	}
	for i := 0; i < syslogMaxSources+500; i++ {
		msg := parseSyslog([]byte("<14>spoofed"), time.Now())
		msg.Source = "10." + strconv.Itoa(i>>16&255) + "." + strconv.Itoa(i>>8&255) + "." + strconv.Itoa(i&255)
		st.add(msg, 11)
	}
	if len(st.sources) > syslogMaxSources {
		t.Fatalf("%d senders tracked", len(st.sources))
	}
}

// Unauthenticated FTP connections are closed after the login timeout.
func TestFTPLoginTimeout(t *testing.T) {
	old := ftpLoginTimeout
	ftpLoginTimeout = 300 * time.Millisecond
	t.Cleanup(func() { ftpLoginTimeout = old })
	h := newHarness(t)
	u := h.user("admin", true)
	port := freePort(t, "tcp")
	h.configure(u, KindFTP, map[string]any{"root": h.shareDir(), "port": port, "tls": "off",
		"users": []map[string]any{{"username": "alice", "password": "alice-pass"}}})
	h.start(u, KindFTP)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "220") {
		t.Fatalf("banner %q %v", line, err)
	}
	start := time.Now()
	_, err = io.ReadAll(br)
	if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("connection not closed: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("connection stayed open")
	}
	h.waitLog(u, KindFTP, "no login within")
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A client trickling a request body is cut off after the stall timeout (slowloris on uploads).
func TestHTTPStalledUploadIsCut(t *testing.T) {
	old := httpStallTimeout
	httpStallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { httpStallTimeout = old })
	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()
	port := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": port, "readOnly": false})
	h.start(u, KindHTTP)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "PUT /slow.bin HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\npartial")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, _ = io.ReadAll(c)
	if time.Since(start) > 4*time.Second {
		t.Fatal("stalled upload kept the connection open")
	}
	time.Sleep(100 * time.Millisecond)
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), "slow.bin") {
			t.Fatalf("partial upload left %s", e.Name())
		}
	}
}
