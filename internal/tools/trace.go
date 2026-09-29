package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/httpx"
)

// Traceroute (RESEARCH TOOL-8) comes in two modes:
//
//   - "trace": classic traceroute — probes per hop with increasing TTL until the destination answers.
//   - "mtr": continuous (mtr-style) — every round probes all hops at once and streams per-hop statistics (loss,
//     last/avg/best/worst RTT, standard deviation) until stopped or the requested number of rounds is done.
//
// Probing engines, in order of preference (see openTracer):
//   - Linux: ICMP echo over an unprivileged ping socket or UDP probes, both with IP_RECVERR so ICMP "time exceeded"
//     errors are read from the socket error queue (the only way unprivileged sockets see them on Linux).
//   - macOS / BSD: unprivileged ICMP datagram sockets (which receive every ICMP message); Windows / root: raw ICMP.
//   - Otherwise the system traceroute / tracepath / tracert is run and its output parsed (classic mode only).
//
// With viaConnectionId the trace runs on a saved SSH host (remote traceroute/tracepath; mtr mode repeats one-probe
// rounds there).

type tracerouteRequest struct {
	Host            string `json:"host"`
	Mode            string `json:"mode"`     // "trace" (default) | "mtr"
	Protocol        string `json:"protocol"` // "icmp" (default) | "udp"
	MaxHops         int    `json:"maxHops"`
	Probes          int    `json:"probes"` // per hop (trace mode)
	TimeoutMs       int    `json:"timeoutMs"`
	ResolveNames    *bool  `json:"resolveNames"` // default true
	IPv6            bool   `json:"ipv6"`
	Rounds          int    `json:"rounds"`     // mtr: rounds to run (0 = until stopped, capped at maxMTRRounds)
	IntervalMs      int    `json:"intervalMs"` // mtr: time between rounds
	ViaConnectionID string `json:"viaConnectionId"`
}

const maxMTRRounds = 3600

func (r *tracerouteRequest) normalize() error {
	r.Host = strings.TrimSpace(r.Host)
	if r.Host == "" {
		return httpx.BadRequest("host is required")
	}
	switch r.Mode = strings.ToLower(strings.TrimSpace(r.Mode)); r.Mode {
	case "", "trace", "traceroute":
		r.Mode = "trace"
	case "mtr":
	default:
		return httpx.BadRequest("mode must be trace or mtr")
	}
	switch r.Protocol = strings.ToLower(strings.TrimSpace(r.Protocol)); r.Protocol {
	case "", "icmp":
		r.Protocol = "icmp"
	case "udp":
	default:
		return httpx.BadRequest("protocol must be icmp or udp")
	}
	if r.MaxHops <= 0 {
		r.MaxHops = 30
	}
	r.MaxHops = clampInt(r.MaxHops, 1, 64)
	if r.Probes <= 0 {
		r.Probes = 3
	}
	r.Probes = clampInt(r.Probes, 1, 10)
	if r.TimeoutMs <= 0 {
		r.TimeoutMs = ternary(r.Mode == "mtr", 1000, 2000)
	}
	r.TimeoutMs = clampInt(r.TimeoutMs, 200, 15000)
	if r.IntervalMs <= 0 {
		r.IntervalMs = 1000
	}
	r.IntervalMs = clampInt(r.IntervalMs, 200, 60000)
	if r.Rounds < 0 {
		r.Rounds = 0
	}
	if r.Rounds == 0 || r.Rounds > maxMTRRounds {
		r.Rounds = maxMTRRounds
	}
	return nil
}

func (r *tracerouteRequest) resolveNames() bool { return r.ResolveNames == nil || *r.ResolveNames }

func prepareTraceroute(ctx context.Context, cl *call) (runner, error) {
	var req tracerouteRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	if err := req.normalize(); err != nil {
		return nil, err
	}
	host, err := safeHostArg(req.Host) // also passed to system/remote traceroute programs
	if err != nil {
		return nil, err
	}
	req.Host = host
	cl.target = req.Host
	cl.details = map[string]any{"mode": req.Mode}
	if req.ViaConnectionID != "" {
		if err := cl.checkSSHConnection(ctx, req.ViaConnectionID); err != nil {
			return nil, err
		}
		cl.details["via"] = req.ViaConnectionID
		if req.Mode == "mtr" {
			return func(ctx context.Context, out *sink) error { return runMTRViaSSH(ctx, cl, &req, out) }, nil
		}
		return func(ctx context.Context, out *sink) error { return runTracerouteViaSSH(ctx, cl, &req, out) }, nil
	}
	dst, err := resolveOne(ctx, req.Host, req.IPv6)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, out *sink) error { return runLocalTrace(ctx, &req, dst, out) }, nil
}

func runLocalTrace(ctx context.Context, req *tracerouteRequest, dst net.IP, out *sink) error {
	t, err := openTracer(dst, req.Protocol == "udp")
	if err != nil {
		if req.Mode == "mtr" {
			return fmt.Errorf("mtr mode needs ICMP or UDP probing, which is not available to this process (%v); run it via an SSH host", err)
		}
		out.emitNow(row{"kind": "info", "message": "Direct probing is unavailable (" + err.Error() + "); using the system traceroute"})
		return runSystemTraceroute(ctx, req, dst, out)
	}
	rs := startReader(ctx, t)
	defer func() {
		rs.stop() // the reader must be gone before the socket is closed (its fd number could be reused)
		_ = t.close()
	}()
	names := newRDNSCache()
	if req.Mode == "mtr" {
		return traceMTR(ctx, t, rs, req, dst, names, out)
	}
	return traceClassic(ctx, t, rs, req, dst, names, out)
}

// readerStream receives a tracer's answers on a dedicated goroutine, so replies are timestamped when they arrive
// even while probes are still being sent (a burst of probes would otherwise inflate the measured RTTs).
type readerStream struct {
	answers chan traceAnswer
	errs    chan error
	cancel  context.CancelFunc
	done    chan struct{}
}

func startReader(ctx context.Context, t tracer) *readerStream {
	rctx, cancel := context.WithCancel(ctx)
	rs := &readerStream{answers: make(chan traceAnswer, 1024), errs: make(chan error, 1), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(rs.done)
		for rctx.Err() == nil {
			a, err := t.recv(rctx, time.Now().Add(200*time.Millisecond))
			if errors.Is(err, errNoAnswer) {
				continue
			}
			if err != nil {
				rs.errs <- err
				return
			}
			select {
			case rs.answers <- a:
			case <-rctx.Done():
				return
			}
		}
	}()
	return rs
}

// next waits for the next answer until deadline (false on timeout or cancellation).
func (rs *readerStream) next(ctx context.Context, deadline time.Time) (traceAnswer, error) {
	d := time.Until(deadline)
	if d <= 0 {
		select {
		case a := <-rs.answers:
			return a, nil
		default:
			return traceAnswer{}, errNoAnswer
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case a := <-rs.answers:
		return a, nil
	case err := <-rs.errs:
		return traceAnswer{}, fmt.Errorf("receiving answers: %w", err)
	case <-timer.C:
		return traceAnswer{}, errNoAnswer
	case <-ctx.Done():
		return traceAnswer{}, ctx.Err()
	}
}

func (rs *readerStream) stop() {
	rs.cancel()
	<-rs.done
}

// ---- probing engine contract ---------------------------------------------------------------------------------------

// tracer sends TTL-limited probes and reports the answers (ICMP time exceeded / unreachable from routers, echo reply
// or port unreachable from the destination).
type tracer interface {
	// send transmits one probe with the given TTL / hop limit and returns its key (unique among recent probes).
	// It is called from one goroutine, concurrently with recv (running on the reader goroutine).
	send(ttl int) (key int, err error)
	// recv returns the next answer to one of our probes, or errNoAnswer once deadline passes or ctx is done. The
	// answer's timestamp is taken as soon as it is read.
	recv(ctx context.Context, deadline time.Time) (traceAnswer, error)
	close() error
	// engine describes the probing method for the UI ("ICMP echo", "UDP", …).
	engine() string
}

type traceAnswer struct {
	key     int
	from    net.IP
	at      time.Time
	reached bool   // the destination itself answered
	code    string // unreachable annotation (!H !N !P !X !F …), "" for time exceeded / echo reply
}

var errNoAnswer = errors.New("no answer")

// UDP probes target the classic traceroute port range (33434 + key); keys cycle through udpPortSpan ports.
const (
	udpBasePort = 33434
	udpPortSpan = 1024
)

// unreachCode4 maps an ICMPv4 destination-unreachable code to traceroute's annotation.
func unreachCode4(code int) string {
	switch code {
	case 0:
		return "!N"
	case 1:
		return "!H"
	case 2:
		return "!P"
	case 3:
		return "" // port unreachable: the UDP probe reached the destination
	case 4:
		return "!F"
	case 5:
		return "!S"
	case 9, 10, 13:
		return "!X"
	default:
		return "!" + strconv.Itoa(code)
	}
}

// unreachCode6 maps an ICMPv6 destination-unreachable code to traceroute's annotation.
func unreachCode6(code int) string {
	switch code {
	case 0:
		return "!N"
	case 1:
		return "!X"
	case 3:
		return "!H"
	case 4:
		return "" // port unreachable
	default:
		return "!" + strconv.Itoa(code)
	}
}

// ---- classic mode --------------------------------------------------------------------------------------------------

// traceWindow is how many consecutive TTLs are probed at once (like modern traceroute's -N): a path with silent
// hops costs one timeout per window instead of one per hop.
const traceWindow = 8

type probeRef struct {
	ttl, idx int
}

func traceClassic(ctx context.Context, t tracer, rs *readerStream, req *tracerouteRequest, dst net.IP, names *rdnsCache, out *sink) error {
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("traceroute to %s (%s), %d hops max, %s probes", req.Host, dst, req.MaxHops, t.engine())})
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	for first := 1; first <= req.MaxHops; first += traceWindow {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last := min(first+traceWindow-1, req.MaxHops)
		n := last - first + 1
		keys := make(map[int]probeRef, n*req.Probes)
		sentAt := make([][]time.Time, n)
		answers := make([][]*traceAnswer, n)
		for i := range answers {
			sentAt[i] = make([]time.Time, req.Probes)
			answers[i] = make([]*traceAnswer, req.Probes)
		}
		// Round-robin over the window so each router sees its probes spread out (ICMP rate limits).
		for p := 0; p < req.Probes; p++ {
			for ttl := first; ttl <= last; ttl++ {
				// Timestamp before the syscall: the reader goroutine may see the answer before send returns.
				at := time.Now()
				k, err := t.send(ttl)
				if err != nil {
					return fmt.Errorf("sending probe: %w", err)
				}
				keys[k] = probeRef{ttl, p}
				sentAt[ttl-first][p] = at
				sleep(ctx, 3*time.Millisecond)
			}
		}
		deadline := time.Now().Add(timeout)
		reachedAt := 0 // lowest TTL in this window that reached the destination
		for pending := n * req.Probes; pending > 0; {
			a, err := rs.next(ctx, deadline)
			if errors.Is(err, errNoAnswer) || ctx.Err() != nil {
				break
			}
			if err != nil {
				return err
			}
			ref, ok := keys[a.key]
			if !ok || answers[ref.ttl-first][ref.idx] != nil {
				continue // a late answer to an earlier window
			}
			answers[ref.ttl-first][ref.idx] = &a
			pending--
			if (a.reached || a.code != "") && (reachedAt == 0 || ref.ttl < reachedAt) {
				reachedAt = ref.ttl
				// Probes beyond the destination only duplicate it: stop waiting for them.
				for ttl := reachedAt + 1; ttl <= last; ttl++ {
					for i := range answers[ttl-first] {
						if answers[ttl-first][i] == nil {
							answers[ttl-first][i] = &traceAnswer{}
							pending--
						}
					}
				}
			}
			if reachedAt > 0 && windowComplete(answers[:reachedAt-first+1]) {
				break
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stopAt := last
		if reachedAt > 0 {
			stopAt = reachedAt
		}
		if req.resolveNames() {
			for ttl := first; ttl <= stopAt; ttl++ {
				for _, f := range hopResponders(answers[ttl-first]) {
					names.lookup(ctx, f)
				}
			}
			names.wait(ctx, 2500*time.Millisecond)
		}
		for ttl := first; ttl <= stopAt; ttl++ {
			hop, reached, code := classicHopRow(ttl, answers[ttl-first], sentAt[ttl-first])
			if froms := hopResponders(answers[ttl-first]); len(froms) > 0 && req.resolveNames() {
				if n, _ := names.lookup(ctx, froms[0]); n != "" {
					hop["host"] = n
				}
			}
			out.emitNow(hop)
			if reached || code != "" {
				out.emitNow(row{"kind": "summary", "target": dst.String(), "hops": ttl, "reached": reached, "engine": t.engine()})
				return nil
			}
		}
	}
	out.emitNow(row{"kind": "summary", "target": dst.String(), "hops": req.MaxHops, "reached": false, "engine": t.engine()})
	return nil
}

// windowComplete reports whether every probe of the given hops was answered.
func windowComplete(hops [][]*traceAnswer) bool {
	for _, h := range hops {
		for _, a := range h {
			if a == nil {
				return false
			}
		}
	}
	return true
}

func hopResponders(answers []*traceAnswer) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range answers {
		if a == nil || a.from == nil || a.at.IsZero() {
			continue
		}
		if s := a.from.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// classicHopRow renders one hop: per-probe RTTs (null = no answer), the responder(s), an unreachable annotation.
func classicHopRow(ttl int, answers []*traceAnswer, sentAt []time.Time) (row, bool, string) {
	times := make([]any, len(answers))
	reached := false
	code := ""
	for p, a := range answers {
		if a == nil || a.at.IsZero() { // no answer (or a placeholder for probes beyond the destination)
			continue
		}
		times[p] = ms(a.at.Sub(sentAt[p]))
		reached = reached || a.reached
		if a.code != "" {
			code = a.code
		}
	}
	froms := hopResponders(answers)
	r := row{"kind": "hop", "ttl": ttl, "timesMs": times, "timeout": len(froms) == 0}
	switch len(froms) {
	case 0:
	case 1:
		r["from"] = froms[0]
	default:
		r["from"] = froms
	}
	if code != "" {
		r["annotation"] = code
	}
	return r, reached, code
}

// ---- mtr mode ------------------------------------------------------------------------------------------------------

// hopStats accumulates mtr statistics for one TTL (Welford's algorithm for the standard deviation).
type hopStats struct {
	ttl         int
	sent, recv  int
	last        float64
	best, worst float64
	mean, m2    float64
	hosts       map[string]int
	order       []string // responders in first-seen order
	code        string
}

func newHopStats(ttl int) *hopStats { return &hopStats{ttl: ttl, hosts: map[string]int{}} }

func (s *hopStats) addReply(from string, rttMs float64) {
	s.recv++
	s.last = rttMs
	if s.recv == 1 || rttMs < s.best {
		s.best = rttMs
	}
	s.worst = max(s.worst, rttMs)
	d := rttMs - s.mean
	s.mean += d / float64(s.recv)
	s.m2 += d * (rttMs - s.mean)
	if from != "" {
		if _, ok := s.hosts[from]; !ok {
			s.order = append(s.order, from)
		}
		s.hosts[from]++
	}
}

func (s *hopStats) stdev() float64 {
	if s.recv < 2 {
		return 0
	}
	return math.Sqrt(s.m2 / float64(s.recv))
}

// primary returns the responder seen most often (ties: first seen).
func (s *hopStats) primary() string {
	best, n := "", 0
	for _, h := range s.order {
		if s.hosts[h] > n {
			best, n = h, s.hosts[h]
		}
	}
	return best
}

func (s *hopStats) row(ctx context.Context, names *rdnsCache, resolve bool) row {
	loss := 0.0
	if s.sent > 0 {
		loss = float64(s.sent-s.recv) / float64(s.sent) * 100
	}
	r := row{"kind": "mtr", "ttl": s.ttl, "sent": s.sent, "recv": s.recv, "loss": round1(loss)}
	if s.recv > 0 {
		r["lastMs"], r["avgMs"], r["bestMs"], r["worstMs"], r["stdevMs"] = round3(s.last), round3(s.mean), round3(s.best), round3(s.worst), round3(s.stdev())
	}
	if p := s.primary(); p != "" {
		r["from"] = p
		if len(s.order) > 1 {
			r["hosts"] = append([]string(nil), s.order...)
		}
		if resolve {
			if n, _ := names.lookup(ctx, p); n != "" {
				r["host"] = n
			}
		}
	}
	if s.code != "" {
		r["annotation"] = s.code
	}
	return r
}

type mtrPending struct {
	ttl int
	at  time.Time
}

func traceMTR(ctx context.Context, t tracer, rs *readerStream, req *tracerouteRequest, dst net.IP, names *rdnsCache, out *sink) error {
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("mtr to %s (%s), %d hops max, %s probes every %s", req.Host, dst, req.MaxHops, t.engine(), time.Duration(req.IntervalMs)*time.Millisecond)})
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	interval := time.Duration(req.IntervalMs) * time.Millisecond
	stats := make([]*hopStats, req.MaxHops+1)
	for i := 1; i <= req.MaxHops; i++ {
		stats[i] = newHopStats(i)
	}
	destTTL := 0 // lowest TTL at which the destination answered (0 = not yet)
	rounds := 0  // completed rounds
	for rounds < req.Rounds && ctx.Err() == nil {
		start := time.Now()
		limit := req.MaxHops
		if destTTL > 0 {
			limit = destTTL
		}
		pending := make(map[int]mtrPending, limit)
		for ttl := 1; ttl <= limit; ttl++ {
			at := time.Now() // before the syscall (see traceClassic)
			k, err := t.send(ttl)
			if err != nil {
				return fmt.Errorf("sending probe: %w", err)
			}
			pending[k] = mtrPending{ttl: ttl, at: at}
			stats[ttl].sent++
			sleep(ctx, 2*time.Millisecond)
		}
		deadline := time.Now().Add(timeout)
		for len(pending) > 0 {
			a, err := rs.next(ctx, deadline)
			if errors.Is(err, errNoAnswer) || ctx.Err() != nil {
				break
			}
			if err != nil {
				return err
			}
			p, ok := pending[a.key]
			if !ok {
				continue
			}
			delete(pending, a.key)
			st := stats[p.ttl]
			from := ""
			if a.from != nil {
				from = a.from.String()
			}
			st.addReply(from, float64(a.at.Sub(p.at).Microseconds())/1000)
			if a.code != "" {
				st.code = a.code
			}
			if (a.reached || a.code != "") && (destTTL == 0 || p.ttl < destTTL) {
				destTTL = p.ttl
			}
		}
		if ctx.Err() != nil {
			break
		}
		rounds++
		// Hops beyond the destination (probed before it was found) are dropped from the display.
		last := destTTL
		if last == 0 {
			for i := req.MaxHops; i >= 1; i-- {
				if stats[i].recv > 0 {
					last = min(i+1, req.MaxHops)
					break
				}
			}
		}
		for i := 1; i <= last; i++ {
			out.add(stats[i].row(ctx, names, req.resolveNames()))
		}
		out.emitNow(row{"kind": "round", "round": rounds, "lastTtl": last, "reached": destTTL > 0})
		if rounds < req.Rounds && !sleep(ctx, interval-time.Since(start)) {
			break
		}
	}
	// Final summary (also after Stop, so the table keeps its last state).
	out.emitNow(row{"kind": "summary", "target": dst.String(), "rounds": rounds, "reached": destTTL > 0, "hops": destTTL, "engine": t.engine()})
	return ctx.Err()
}

// ---- system / remote traceroute output -----------------------------------------------------------------------------

var (
	reHopLine  = regexp.MustCompile(`^\s*(\d{1,3})(\?)?:?\s+(.*)$`)
	reTimeTok  = regexp.MustCompile(`^<?(\d+(?:\.\d+)?)(ms)?$`)
	reAnnotTok = regexp.MustCompile(`^!(?:[A-Z]|\d+|<\d+>)$`)
	reTraceTo  = regexp.MustCompile(`(?i)^(?:traceroute6?|tracing route) to \S+\s*[(\[]([0-9a-fA-F:.]+)[)\]]`)
)

// parseTraceLine parses one hop line of traceroute (Linux, BSD, macOS, busybox), tracepath or Windows tracert output
// into a hop row. Lines that are not hops (headers, tracepath's [LOCALHOST]/Resume lines, "Trace complete.") return
// nil.
func parseTraceLine(line string) row {
	m := reHopLine.FindStringSubmatch(line)
	if m == nil || strings.Contains(m[3], "[LOCALHOST]") || strings.Contains(m[3], "pmtu") && !strings.Contains(m[3], "ms") {
		return nil
	}
	ttl, _ := strconv.Atoi(m[1])
	if ttl <= 0 {
		return nil
	}
	toks := strings.Fields(m[3])
	var froms []string
	var times []any
	annotation := ""
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch {
		case tok == "*":
			times = append(times, nil)
		case reAnnotTok.MatchString(tok):
			annotation = tok
		case reTimeTok.MatchString(tok) && (strings.HasSuffix(tok, "ms") || i+1 < len(toks) && toks[i+1] == "ms"):
			v, _ := strconv.ParseFloat(reTimeTok.FindStringSubmatch(tok)[1], 64)
			times = append(times, v)
			if !strings.HasSuffix(tok, "ms") {
				i++ // skip the "ms" token
			}
		default:
			ipTok := strings.Trim(tok, "()[],")
			if ip := net.ParseIP(ipTok); ip != nil {
				if s := ip.String(); !containsString(froms, s) {
					froms = append(froms, s)
				}
			}
		}
	}
	if len(froms) == 0 && len(times) == 0 && !strings.Contains(strings.ToLower(m[3]), "no reply") &&
		!strings.Contains(strings.ToLower(m[3]), "timed out") {
		return nil
	}
	r := row{"kind": "hop", "ttl": ttl, "timesMs": times, "timeout": len(froms) == 0}
	switch len(froms) {
	case 0:
	case 1:
		r["from"] = froms[0]
	default:
		r["from"] = froms
	}
	if annotation != "" {
		r["annotation"] = annotation
	}
	if strings.Contains(m[3], "reached") { // tracepath marks the destination hop
		r["reached"] = true
	}
	return r
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// hopFroms returns the responder list of a parsed hop row.
func hopFroms(r row) []string {
	switch v := r["from"].(type) {
	case string:
		return []string{v}
	case []string:
		return v
	}
	return nil
}

// mergeHop folds a second line for the same TTL (tracepath prints one line per probe) into the first.
func mergeHop(dst, src row) {
	froms := hopFroms(dst)
	for _, f := range hopFroms(src) {
		if !containsString(froms, f) {
			froms = append(froms, f)
		}
	}
	switch len(froms) {
	case 0:
	case 1:
		dst["from"] = froms[0]
	default:
		dst["from"] = froms
	}
	dt, _ := dst["timesMs"].([]any)
	st, _ := src["timesMs"].([]any)
	dst["timesMs"] = append(dt, st...)
	dst["timeout"] = len(froms) == 0
	if a, ok := src["annotation"]; ok {
		dst["annotation"] = a
	}
}

// traceLineStream turns traceroute output lines into hop rows: consecutive lines for one TTL are merged, reverse DNS
// is added when requested, and each hop is emitted once complete (when the next hop starts or the output ends).
type traceLineStream struct {
	ctx        context.Context
	out        *sink
	names      *rdnsCache
	resolve    bool
	cur        row
	dest       string // destination IP from the header line, when printed
	lastIP     string
	hops       int
	sawReached bool      // tracepath printed "reached"
	onHop      func(row) // optional observer (mtr aggregation) instead of emitting
}

func (s *traceLineStream) line(text string) {
	if m := reTraceTo.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		s.dest = m[1]
		return
	}
	hop := parseTraceLine(text)
	if hop == nil {
		return
	}
	if s.cur != nil && s.cur["ttl"] == hop["ttl"] {
		mergeHop(s.cur, hop)
		return
	}
	s.finishHop()
	s.cur = hop
}

func (s *traceLineStream) finishHop() {
	if s.cur == nil {
		return
	}
	hop := s.cur
	s.cur = nil
	s.hops, _ = hop["ttl"].(int)
	if hop["reached"] == true {
		s.sawReached = true
		delete(hop, "reached")
	}
	if froms := hopFroms(hop); len(froms) > 0 {
		s.lastIP = froms[len(froms)-1]
		if s.resolve && s.names != nil {
			for _, f := range froms {
				s.names.lookup(s.ctx, f)
			}
			s.names.wait(s.ctx, 2*time.Second)
			if n, _ := s.names.lookup(s.ctx, froms[0]); n != "" {
				hop["host"] = n
			}
		}
	}
	if s.onHop != nil {
		s.onHop(hop)
		return
	}
	s.out.emitNow(hop)
}

func (s *traceLineStream) reached() bool {
	return s.sawReached || s.dest != "" && s.lastIP != "" && net.ParseIP(s.dest).Equal(net.ParseIP(s.lastIP))
}

// systemTracerouteCommand picks the local traceroute program and its arguments (never a shell).
func systemTracerouteCommand(req *tracerouteRequest) (string, []string, error) {
	waitSec := strconv.Itoa(max(1, (req.TimeoutMs+999)/1000))
	if runtime.GOOS == "windows" {
		args := []string{"-d", "-h", strconv.Itoa(req.MaxHops), "-w", strconv.Itoa(req.TimeoutMs)}
		if req.IPv6 {
			args = append(args, "-6")
		}
		p, err := exec.LookPath("tracert")
		return p, append(args, req.Host), err
	}
	for _, name := range []string{"traceroute", "traceroute6", "tracepath"} {
		if name == "traceroute6" && !req.IPv6 {
			continue
		}
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if name == "tracepath" {
			args := []string{"-n"}
			if req.IPv6 {
				args = append(args, "-6")
			}
			return p, append(args, req.Host), nil
		}
		args := []string{"-n", "-q", strconv.Itoa(req.Probes), "-m", strconv.Itoa(req.MaxHops), "-w", waitSec}
		if req.IPv6 && name == "traceroute" && runtime.GOOS == "linux" {
			args = append(args, "-6")
		}
		return p, append(args, req.Host), nil
	}
	return "", nil, errors.New("no traceroute, tracepath or tracert program found")
}

// runSystemTraceroute runs the host's traceroute program and streams its parsed hops (classic mode fallback).
func runSystemTraceroute(ctx context.Context, req *tracerouteRequest, dst net.IP, out *sink) error {
	path, args, err := systemTracerouteCommand(req)
	if err != nil {
		return fmt.Errorf("traceroute is not available on this host: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", path, err)
	}
	out.emitNow(row{"kind": "info", "message": "Running " + path + " " + strings.Join(args, " ")})
	st := &traceLineStream{ctx: ctx, out: out, names: newRDNSCache(), resolve: req.resolveNames(), dest: dst.String()}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		st.line(sc.Text())
	}
	st.finishHop()
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if st.hops == 0 && werr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = werr.Error()
		}
		return fmt.Errorf("traceroute failed: %s", msg)
	}
	out.emitNow(row{"kind": "summary", "target": dst.String(), "hops": st.hops, "reached": st.reached(), "engine": "system"})
	return nil
}

// limitedWriter keeps at most n bytes.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
