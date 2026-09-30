package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIperf3 is a minimal iperf3 server following iperf3's server-side state machine (iperf_server_api.c):
// cookie → PARAM_EXCHANGE → params JSON → CREATE_STREAMS → N data streams (each sends the cookie) → TEST_START,
// TEST_RUNNING → data until the client's TEST_END → EXCHANGE_RESULTS (client results first, validated like
// iperf3's get_results, then ours) → DISPLAY_RESULTS → IPERF_DONE.
type fakeIperf3 struct {
	t        *testing.T
	ln       net.Listener
	busy     bool
	gotParam map[string]any
	gotRes   map[string]any
	received atomic.Int64
	mu       sync.Mutex
	err      error
	done     chan struct{}
}

func startFakeIperf3(t *testing.T) *fakeIperf3 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIperf3{t: t, ln: ln, done: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeIperf3) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeIperf3) fail(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
}

// wait blocks until the server finished its exchange and returns the first protocol error it saw.
func (f *fakeIperf3) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("fake iperf3 server did not finish")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeIperf3) serve() {
	defer close(f.done)
	ctrl, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer ctrl.Close()
	_ = ctrl.SetDeadline(time.Now().Add(30 * time.Second))
	cookie := make([]byte, iperfCookieSize)
	if _, err := io.ReadFull(ctrl, cookie); err != nil || cookie[iperfCookieSize-1] != 0 {
		f.fail(fmt.Errorf("bad cookie %q", cookie))
		return
	}
	if f.busy {
		_, _ = ctrl.Write([]byte{byte(0xff)}) // ACCESS_DENIED (-1)
		return
	}
	state := func(s int8) { _, _ = ctrl.Write([]byte{byte(s)}) }
	state(iperfParamExchange)
	if err := readIperfJSON(ctrl, &f.gotParam); err != nil {
		f.fail(err)
		return
	}
	parallel := int(f.gotParam["parallel"].(float64))
	reverse, _ := f.gotParam["reverse"].(bool)
	blk := int(f.gotParam["len"].(float64))
	state(iperfCreateStreams)
	var streams []net.Conn
	for range parallel {
		c, err := f.ln.Accept()
		if err != nil {
			f.fail(err)
			return
		}
		defer c.Close()
		got := make([]byte, iperfCookieSize)
		if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, cookie) {
			f.fail(fmt.Errorf("stream cookie mismatch"))
			return
		}
		streams = append(streams, c)
	}
	state(iperfTestStart)
	state(iperfTestRunning)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var sent atomic.Int64
	for _, c := range streams {
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			buf := make([]byte, blk)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
				if reverse {
					n, err := c.Write(buf)
					sent.Add(int64(n))
					if err != nil && !isTimeout(err) {
						return
					}
				} else {
					n, err := c.Read(buf)
					f.received.Add(int64(n))
					if err != nil && !isTimeout(err) {
						return
					}
				}
			}
		}(c)
	}
	var b [1]byte
	if _, err := io.ReadFull(ctrl, b[:]); err != nil || int8(b[0]) != iperfTestEnd {
		f.fail(fmt.Errorf("expected TEST_END, got %v %v", b[0], err))
		return
	}
	close(stop)
	wg.Wait()
	state(iperfExchangeResults)
	if err := readIperfJSON(ctrl, &f.gotRes); err != nil {
		f.fail(err)
		return
	}
	for _, k := range []string{"cpu_util_total", "cpu_util_user", "cpu_util_system", "sender_has_retransmits"} {
		if _, ok := f.gotRes[k].(float64); !ok {
			f.fail(fmt.Errorf("client results lack %s", k))
		}
	}
	res := iperfResults{SenderHasRetransmits: 0, CongestionUsed: "cubic"}
	clientStreams, _ := f.gotRes["streams"].([]any)
	if len(clientStreams) != parallel {
		f.fail(fmt.Errorf("client reported %d streams", len(clientStreams)))
	}
	for i, cs := range clientStreams {
		m := cs.(map[string]any)
		for _, k := range []string{"id", "bytes", "retransmits", "jitter", "errors", "packets"} {
			if _, ok := m[k].(float64); !ok {
				f.fail(fmt.Errorf("client stream lacks %s", k))
			}
		}
		if int(m["id"].(float64)) != iperfStreamID(i) {
			f.fail(fmt.Errorf("stream %d has id %v", i, m["id"]))
		}
		b := f.received.Load() / int64(parallel)
		if reverse {
			b = sent.Load() / int64(parallel)
		}
		res.Streams = append(res.Streams, iperfStreamResult{ID: iperfStreamID(i), Bytes: b, Retransmits: 3, EndTime: 1})
	}
	if err := writeIperfJSON(ctrl, res); err != nil {
		f.fail(err)
		return
	}
	state(iperfDisplayResults)
	if _, err := io.ReadFull(ctrl, b[:]); err != nil || int8(b[0]) != iperfDone {
		f.fail(fmt.Errorf("expected IPERF_DONE, got %v %v", b[0], err))
	}
}

func TestIperf3ClientForwardAndReverse(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		f := startFakeIperf3(t)
		c := runJob(t, prepareThroughput, fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"durationSec":1,"parallel":2,"reverse":%v}`, f.port(), reverse))
		if err := f.wait(t); err != nil {
			t.Fatalf("reverse=%v: server saw a protocol error: %v", reverse, err)
		}
		if f.gotParam["tcp"] != true || f.gotParam["time"] != 1.0 || f.gotParam["parallel"] != 2.0 || (f.gotParam["reverse"] == true) != reverse {
			t.Fatalf("params = %v", f.gotParam)
		}
		sum := c.byKind("summary")
		if len(sum) != 1 {
			t.Fatalf("reverse=%v: rows = %v", reverse, c.all())
		}
		s := sum[0]
		if s["streams"] != 2 || s["senderBytes"].(int64) <= 0 || s["receiverBytes"].(int64) <= 0 || s["receiverBitsPerSec"].(float64) <= 0 {
			t.Fatalf("reverse=%v: summary = %v", reverse, s)
		}
		if reverse && s["retransmits"] != int64(6) {
			t.Errorf("reverse retransmits = %v", s["retransmits"])
		}
		if len(c.byKind("interval")) == 0 {
			t.Errorf("reverse=%v: no interval rows", reverse)
		}
	}
}

func TestIperf3ServerBusy(t *testing.T) {
	f := startFakeIperf3(t)
	f.busy = true
	_, err := tryJob(context.Background(), prepareThroughput, testCall(fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"durationSec":1}`, f.port())))
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy server: %v", err)
	}
}

func TestIperf3Cancel(t *testing.T) {
	f := startFakeIperf3(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := tryJob(ctx, prepareThroughput, testCall(fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"durationSec":30}`, f.port())))
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel: err=%v after %v", err, time.Since(start))
	}
}

func TestIperfJSONFraming(t *testing.T) {
	var buf bytes.Buffer
	if err := writeIperfJSON(&buf, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if b := buf.Bytes(); len(b) != 4+7 || b[3] != 7 {
		t.Fatalf("framing = % x", b)
	}
	var v map[string]any
	if err := readIperfJSON(&buf, &v); err != nil || v["a"] != 1.0 {
		t.Fatalf("round trip = %v %v", v, err)
	}
	if err := readIperfJSON(bytes.NewReader([]byte{0xff, 0, 0, 0}), &v); err == nil {
		t.Fatal("oversized JSON must be refused")
	}
	if c := iperfCookie(); len(c) != 37 || c[36] != 0 || strings.Trim(string(c[:36]), "abcdefghijklmnopqrstuvwxyz234567") != "" {
		t.Fatalf("cookie = %q", c)
	}
	if iperfStreamID(0) != 1 || iperfStreamID(1) != 3 || iperfStreamID(2) != 4 {
		t.Fatal("stream ids must follow iperf3's numbering")
	}
}
