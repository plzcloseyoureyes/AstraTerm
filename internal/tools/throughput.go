package tools

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/httpx"
)

// TCP throughput test (RESEARCH TOOL-8 "iperf client"). Two modes, labelled as such in the UI:
//
//   - "iperf3": a client for the iperf3 control/data protocol (TCP only) against any `iperf3 -s` server
//     (default port 5201): parameter exchange, N parallel data streams, forward (upload) or reverse (download), the
//     results exchange, and per-second interval rows.
//   - "ssh": throughput of an SSH channel to a saved SSH host (upload = `cat > /dev/null`, download =
//     `cat /dev/zero`), i.e. what file transfers to that host can reach, including SSH encryption overhead.

type throughputRequest struct {
	Mode            string `json:"mode"` // "iperf3" (default) | "ssh"
	Host            string `json:"host"`
	Port            int    `json:"port"`
	DurationSec     int    `json:"durationSec"`
	Parallel        int    `json:"parallel"`
	Reverse         bool   `json:"reverse"` // download (server → Termstead) instead of upload
	BlockSize       int    `json:"blockSize"`
	ViaConnectionID string `json:"viaConnectionId"` // mode "ssh": the SSH host to measure against
}

func prepareThroughput(ctx context.Context, cl *call) (runner, error) {
	var req throughputRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Mode == "" {
		req.Mode = "iperf3"
	}
	req.DurationSec = clampInt(orDefault(req.DurationSec, 10), 1, 60)
	req.Parallel = clampInt(orDefault(req.Parallel, 1), 1, 8)
	req.BlockSize = clampInt(orDefault(req.BlockSize, 128*1024), 4096, 1<<20)
	switch req.Mode {
	case "iperf3":
		req.Host = strings.Trim(strings.TrimSpace(req.Host), "[]")
		if req.Host == "" {
			return nil, httpx.BadRequest("host is required")
		}
		if _, err := safeHostArg(req.Host); err != nil {
			return nil, err
		}
		if req.Port == 0 {
			req.Port = 5201
		}
		if req.Port < 1 || req.Port > 65535 {
			return nil, httpx.BadRequest("port must be between 1 and 65535")
		}
		cl.target = net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
		guard := cl.guard
		return func(ctx context.Context, out *sink) error { return runIperf3(ctx, guard, &req, out) }, nil
	case "ssh":
		if req.ViaConnectionID == "" {
			return nil, httpx.BadRequest("choose the SSH connection to measure")
		}
		req.Parallel = min(req.Parallel, 4)
		if err := cl.checkSSHConnection(ctx, req.ViaConnectionID); err != nil {
			return nil, err
		}
		cl.target = req.ViaConnectionID
		return func(ctx context.Context, out *sink) error { return runSSHThroughput(ctx, cl, &req, out) }, nil
	default:
		return nil, httpx.BadRequest("mode must be iperf3 or ssh")
	}
}

// ---- measurement bookkeeping ---------------------------------------------------------------------------------------

// meter counts transferred bytes and emits one interval row per second.
type meter struct {
	bytes    atomic.Int64
	counting atomic.Bool
	start    time.Time
}

// run reports intervals until stop is closed; it returns the byte count and the exact measured duration.
func (m *meter) run(ctx context.Context, out *sink, duration time.Duration, stop <-chan struct{}) (int64, time.Duration) {
	m.start = time.Now()
	m.counting.Store(true)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	end := time.NewTimer(duration)
	defer end.Stop()
	var last int64
	lastAt := m.start
	emit := func(now time.Time) {
		b := m.bytes.Load()
		d := now.Sub(lastAt)
		if d <= 0 {
			return
		}
		out.add(row{
			"kind": "interval", "startSec": round3(lastAt.Sub(m.start).Seconds()), "endSec": round3(now.Sub(m.start).Seconds()),
			"bytes": b - last, "bitsPerSec": float64(b-last) * 8 / d.Seconds(),
		})
		last, lastAt = b, now
	}
	for {
		select {
		case now := <-t.C:
			emit(now)
		case <-end.C:
			now := time.Now()
			m.counting.Store(false)
			if now.Sub(lastAt) > 50*time.Millisecond {
				emit(now)
			}
			return m.bytes.Load(), now.Sub(m.start)
		case <-stop:
			now := time.Now()
			m.counting.Store(false)
			return m.bytes.Load(), now.Sub(m.start)
		case <-ctx.Done():
			m.counting.Store(false)
			return m.bytes.Load(), time.Since(m.start)
		}
	}
}

func (m *meter) add(n int) {
	if n > 0 && m.counting.Load() {
		m.bytes.Add(int64(n))
	}
}

func bps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds()
}

// ---- iperf3 protocol -----------------------------------------------------------------------------------------------

// iperf3 control-channel states (iperf3 src/iperf_api.h).
const (
	iperfTestStart       = 1
	iperfTestRunning     = 2
	iperfTestEnd         = 4
	iperfParamExchange   = 9
	iperfCreateStreams   = 10
	iperfServerTerminate = 11
	iperfClientTerminate = 12
	iperfExchangeResults = 13
	iperfDisplayResults  = 14
	iperfStart           = 15
	iperfDone            = 16
	iperfAccessDenied    = -1
	iperfServerError     = -2
	iperfCookieSize      = 37
	iperfMaxJSON         = 1 << 20
)

// iperfCookie is 36 random characters from iperf3's alphabet plus a NUL terminator.
func iperfCookie() []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, iperfCookieSize)
	_, _ = rand.Read(b[:iperfCookieSize-1])
	for i := 0; i < iperfCookieSize-1; i++ {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	b[iperfCookieSize-1] = 0
	return b
}

func writeIperfJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	msg := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(data)), uint32(len(data)))
	_, err = w.Write(append(msg, data...))
	return err
}

func readIperfJSON(r io.Reader, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > iperfMaxJSON {
		return fmt.Errorf("invalid JSON message size %d", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// iperfStreamResult is one stream of an iperf3 results message.
type iperfStreamResult struct {
	ID             int     `json:"id"`
	Bytes          int64   `json:"bytes"`
	Retransmits    int64   `json:"retransmits"`
	Jitter         float64 `json:"jitter"`
	Errors         int64   `json:"errors"`
	OmittedErrors  int64   `json:"omitted_errors"`
	Packets        int64   `json:"packets"`
	OmittedPackets int64   `json:"omitted_packets"`
	StartTime      float64 `json:"start_time"`
	EndTime        float64 `json:"end_time"`
}

type iperfResults struct {
	CPUUtilTotal         float64             `json:"cpu_util_total"`
	CPUUtilUser          float64             `json:"cpu_util_user"`
	CPUUtilSystem        float64             `json:"cpu_util_system"`
	SenderHasRetransmits int                 `json:"sender_has_retransmits"`
	CongestionUsed       string              `json:"congestion_used,omitempty"`
	Streams              []iperfStreamResult `json:"streams"`
}

// iperfStreamID reproduces iperf3's stream numbering (iperf_add_stream): 1, 3, 4, 5, …
func iperfStreamID(i int) int {
	if i == 0 {
		return 1
	}
	return i + 2
}

func runIperf3(ctx context.Context, guard *netGuard, req *throughputRequest, out *sink) error {
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	dir := ternary(req.Reverse, "download (server → Termstead)", "upload (Termstead → server)")
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("iperf3 TCP test with %s: %d stream(s), %ds, %s", addr, req.Parallel, req.DurationSec, dir)})

	dialer := guard.dialer(10 * time.Second)
	ctrl, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cannot reach the iperf3 server: %w", err)
	}
	var streams []net.Conn
	var streamsMu sync.Mutex
	closeAll := func() {
		streamsMu.Lock()
		defer streamsMu.Unlock()
		for _, s := range streams {
			_ = s.Close()
		}
		_ = ctrl.Close()
	}
	defer closeAll()
	// Stop: tell the server we are going away (like iperf3's SIGINT handler) and unblock every read/write.
	stopAfter := context.AfterFunc(ctx, func() {
		_ = ctrl.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = ctrl.Write([]byte{iperfClientTerminate})
		closeAll()
	})
	defer stopAfter()

	cookie := iperfCookie()
	if _, err := ctrl.Write(cookie); err != nil {
		return fmt.Errorf("iperf3 handshake: %w", err)
	}
	readState := func(timeout time.Duration) (int8, error) {
		_ = ctrl.SetReadDeadline(time.Now().Add(timeout))
		var b [1]byte
		if _, err := io.ReadFull(ctrl, b[:]); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("iperf3 control connection: %w", err)
		}
		return int8(b[0]), nil
	}
	sendState := func(s int8) error {
		_ = ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := ctrl.Write([]byte{byte(s)})
		return err
	}

	block := make([]byte, req.BlockSize)
	_, _ = rand.Read(block)
	m := &meter{}
	var wg sync.WaitGroup
	var sent int64
	var measured time.Duration
	stopData := make(chan struct{})
	var stopOnce sync.Once
	haltData := func() { stopOnce.Do(func() { close(stopData) }) }
	defer haltData()
	var serverRes iperfResults

	for {
		st, err := readState(30 * time.Second)
		if err != nil {
			return err
		}
		switch st {
		case iperfParamExchange:
			params := map[string]any{
				"tcp": true, "omit": 0, "time": req.DurationSec, "num": 0, "blockcount": 0,
				"parallel": req.Parallel, "len": req.BlockSize, "pacing_timer": 1000, "client_version": "3.16",
			}
			if req.Reverse {
				params["reverse"] = true
			}
			if err := writeIperfJSON(ctrl, params); err != nil {
				return fmt.Errorf("iperf3 parameter exchange: %w", err)
			}
		case iperfCreateStreams:
			for i := 0; i < req.Parallel; i++ {
				c, err := dialer.DialContext(ctx, "tcp", addr)
				if err != nil {
					return fmt.Errorf("iperf3 data stream: %w", err)
				}
				streamsMu.Lock()
				streams = append(streams, c)
				streamsMu.Unlock()
				if _, err := c.Write(cookie); err != nil {
					return fmt.Errorf("iperf3 data stream: %w", err)
				}
			}
		case iperfTestStart, iperfStart:
		case iperfTestRunning:
			for _, c := range streams {
				wg.Add(1)
				go func(c net.Conn) {
					defer wg.Done()
					if req.Reverse {
						buf := make([]byte, 128*1024)
						for {
							n, err := c.Read(buf) // keep draining after the end so the server never blocks
							m.add(n)
							if err != nil {
								return
							}
						}
					}
					for {
						select {
						case <-stopData:
							return
						default:
						}
						_ = c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
						n, err := c.Write(block)
						m.add(n)
						if err != nil && !isTimeout(err) {
							return
						}
					}
				}(c)
			}
			sent, measured = m.run(ctx, out, time.Duration(req.DurationSec)*time.Second, nil)
			if !req.Reverse {
				haltData()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := sendState(iperfTestEnd); err != nil {
				return fmt.Errorf("iperf3 control connection: %w", err)
			}
		case iperfExchangeResults:
			ours := iperfResults{SenderHasRetransmits: ternary(req.Reverse, -1, 0)}
			perStream := sent / int64(max(1, len(streams)))
			for i := range streams {
				s := iperfStreamResult{ID: iperfStreamID(i), Bytes: perStream, Retransmits: -1, EndTime: measured.Seconds()}
				ours.Streams = append(ours.Streams, s)
			}
			if err := writeIperfJSON(ctrl, ours); err != nil {
				return fmt.Errorf("iperf3 results exchange: %w", err)
			}
			_ = ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
			if err := readIperfJSON(ctrl, &serverRes); err != nil {
				return fmt.Errorf("iperf3 results exchange: %w", err)
			}
		case iperfDisplayResults:
			_ = sendState(iperfDone)
			haltData()
			closeAll()
			wg.Wait()
			var serverBytes int64
			var retrans int64 = -1
			for _, s := range serverRes.Streams {
				serverBytes += s.Bytes
				if s.Retransmits >= 0 {
					retrans = max(retrans, 0) + s.Retransmits
				}
			}
			senderBytes, receiverBytes := sent, serverBytes
			if req.Reverse {
				senderBytes, receiverBytes = serverBytes, sent
			}
			sum := row{
				"kind": "summary", "mode": "iperf3", "reverse": req.Reverse, "streams": len(streams),
				"durationSec": round3(measured.Seconds()), "senderBytes": senderBytes, "receiverBytes": receiverBytes,
				"senderBitsPerSec": bps(senderBytes, measured), "receiverBitsPerSec": bps(receiverBytes, measured),
			}
			if req.Reverse && retrans >= 0 {
				sum["retransmits"] = retrans
			}
			if serverRes.CongestionUsed != "" {
				sum["congestion"] = serverRes.CongestionUsed
			}
			out.emitNow(sum)
			return nil
		case iperfAccessDenied:
			return errors.New("the iperf3 server is busy running another test; try again later")
		case iperfServerError:
			var codes [8]byte
			_ = ctrl.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.ReadFull(ctrl, codes[:]); err == nil {
				return fmt.Errorf("the iperf3 server reported an error (i_errno %d, errno %d)", int32(binary.BigEndian.Uint32(codes[:4])), int32(binary.BigEndian.Uint32(codes[4:])))
			}
			return errors.New("the iperf3 server reported an error")
		case iperfServerTerminate, iperfClientTerminate:
			return errors.New("the iperf3 server terminated the test")
		default:
			return fmt.Errorf("unexpected iperf3 control state %d", st)
		}
	}
}

// ---- SSH channel throughput ----------------------------------------------------------------------------------------

func runSSHThroughput(ctx context.Context, cl *call, req *throughputRequest, out *sink) error {
	client, release, err := cl.sshClientFor(ctx, req.ViaConnectionID)
	if err != nil {
		return err
	}
	defer release()
	dir := ternary(req.Reverse, "download (host → Termstead)", "upload (Termstead → host)")
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("SSH channel throughput with %s: %d channel(s), %ds, %s", client.Conn.Name, req.Parallel, req.DurationSec, dir)})

	type chanSess struct {
		sess    *ssh.Session
		release func()
		stdin   io.WriteCloser
		stdout  io.Reader
	}
	var sessions []chanSess
	defer func() {
		for _, s := range sessions {
			_ = s.sess.Close()
			s.release()
		}
	}()
	cmd := shScript(ternary(req.Reverse, "exec cat /dev/zero", "exec cat > /dev/null"))
	for i := 0; i < req.Parallel; i++ {
		sess, _, rel, err := client.NewSessionContext(ctx)
		if err != nil {
			return fmt.Errorf("opening an SSH channel: %w", err)
		}
		cs := chanSess{sess: sess, release: rel}
		if req.Reverse {
			cs.stdout, err = sess.StdoutPipe()
		} else {
			cs.stdin, err = sess.StdinPipe()
		}
		if err == nil {
			err = sess.Start(cmd)
		}
		sessions = append(sessions, cs)
		if err != nil {
			return fmt.Errorf("starting the remote command: %w", err)
		}
	}

	m := &meter{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	block := make([]byte, 32*1024) // SSH channel packets are ≤ 32 KiB
	_, _ = rand.Read(block)
	for _, s := range sessions {
		wg.Add(1)
		go func(s chanSess) {
			defer wg.Done()
			if req.Reverse {
				buf := make([]byte, 64*1024)
				for {
					n, err := s.stdout.Read(buf)
					m.add(n)
					if err != nil {
						return
					}
					select {
					case <-stop:
						return
					default:
					}
				}
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				n, err := s.stdin.Write(block)
				m.add(n)
				if err != nil {
					return
				}
			}
		}(s)
	}
	total, measured := m.run(ctx, out, time.Duration(req.DurationSec)*time.Second, nil)
	close(stop)
	for _, s := range sessions { // unblock readers / writers
		_ = s.sess.Signal(ssh.SIGKILL)
		_ = s.sess.Close()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	out.emitNow(row{
		"kind": "summary", "mode": "ssh", "reverse": req.Reverse, "streams": len(sessions),
		"durationSec": round3(measured.Seconds()), "senderBytes": total, "receiverBytes": total,
		"senderBitsPerSec": bps(total, measured), "receiverBitsPerSec": bps(total, measured), "host": client.Conn.Name,
	})
	return nil
}
