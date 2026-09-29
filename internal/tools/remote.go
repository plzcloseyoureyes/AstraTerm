package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/sshx"
)

// "Run via" a saved SSH connection: ping and traceroute execute the remote host's own ping / traceroute (tracepath)
// programs; the host argument is validated by safeHostArg (no shell metacharacters, no leading '-') and single-quoted,
// and every script runs under `sh -c` so the remote login shell (csh, fish…) does not matter.

// sshClientFor dials (or reuses) the SSH client for a saved connection the user may use. The caller must release it.
func (cl *call) sshClientFor(ctx context.Context, connID string) (*sshx.Client, func(), error) {
	if cl.h == nil || cl.h.c == nil || cl.h.c.SSH == nil {
		return nil, nil, httpx.BadRequest("SSH is not available")
	}
	return cl.h.c.SSH.Get(ctx, cl.user, connID)
}

// shScript wraps a POSIX sh script for exec on the remote host.
func shScript(script string) string { return "sh -c " + shellQuote(script) }

// streamExec runs cmd on client without a PTY, calling onLine for every stdout / stderr line until the command exits
// or ctx is cancelled (which kills it). It returns the command's exit code (-1 when unknown).
func streamExec(ctx context.Context, client *sshx.Client, cmd string, onLine func(text string, isErr bool)) (int, error) {
	sess, _, release, err := client.NewSessionContext(ctx)
	if err != nil {
		return -1, err
	}
	defer release()
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := sess.Start(cmd); err != nil {
		return -1, err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		case <-done:
		}
	}()

	var mu sync.Mutex // serializes onLine across the two readers
	var wg sync.WaitGroup
	scan := func(r io.Reader, isErr bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 16*1024), 1<<20)
		for sc.Scan() {
			mu.Lock()
			onLine(sc.Text(), isErr)
			mu.Unlock()
		}
		_, _ = io.Copy(io.Discard, r) // a too-long line: keep draining so the command can finish
	}
	wg.Add(2)
	go scan(stdout, false)
	go scan(stderr, true)
	wg.Wait()

	werr := sess.Wait()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var ee *ssh.ExitError
	switch {
	case werr == nil:
		return 0, nil
	case errors.As(werr, &ee):
		return ee.ExitStatus(), nil
	default:
		return -1, nil // exit status unknown (e.g. killed by a signal): not a transport error
	}
}

// ---- remote ping ---------------------------------------------------------------------------------------------------

var (
	// iputils / BSD / macOS: "64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=12.3 ms"; busybox: "… seq=0 ttl=64 time=…".
	rePingReply   = regexp.MustCompile(`from\s+(\S+?)(?:\s+\(([^)]+)\))?:?\s.*?\b(?:icmp_)?seq=(\d+).*?\bttl=(\d+).*?\btime[=<]\s*([\d.]+)\s*ms`)
	rePingTimeout = regexp.MustCompile(`(?i)request timeout for icmp_seq[ =](\d+)|no answer yet for icmp_seq=(\d+)`)
	rePingLoss    = regexp.MustCompile(`(\d+) packets transmitted, (\d+) (?:packets )?received.*?([\d.]+)% packet loss`)
	rePingRTTStat = regexp.MustCompile(`min/avg/max(?:/(?:mdev|stddev|std-dev))?\s*=\s*([\d.]+)/([\d.]+)/([\d.]+)(?:/([\d.]+))?\s*ms`)
)

// remotePingScript builds the POSIX sh script for a remote ping. BSD/macOS take -W in milliseconds (Linux iputils and
// busybox in seconds) and use ping6 for IPv6.
func remotePingScript(req *pingRequest) string {
	waitSec := max(1, (req.TimeoutMs+999)/1000)
	interval := ""
	if req.IntervalMs != 1000 {
		interval = fmt.Sprintf(" -i %.1f", float64(max(req.IntervalMs, 200))/1000) // non-root minimum is 0.2 s
	}
	size := ""
	if req.Size > 0 {
		size = " -s " + strconv.Itoa(req.Size)
	}
	v6 := ternary(req.IPv6, "1", "0")
	return fmt.Sprintf(`W=%d; P=ping; A=; case "$(uname -s 2>/dev/null)" in Darwin|*BSD|DragonFly) W=%d; [ %s = 1 ] && P=ping6;; *) [ %s = 1 ] && A=-6;; esac; exec $P $A -c %d%s%s -W "$W" %s`,
		waitSec, waitSec*1000, v6, v6, req.Count, interval, size, shellQuote(req.Host))
}

func runPingViaSSH(ctx context.Context, cl *call, req *pingRequest, out *sink) error {
	client, release, err := cl.sshClientFor(ctx, req.ViaConnectionID)
	if err != nil {
		return err
	}
	defer release()

	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("Running on %s: ping -c %d %s", client.Conn.Name, req.Count, req.Host)})
	summary := row{"kind": "summary", "target": req.Host, "remote": true}
	var haveSummary bool
	var errLines []string
	code, err := streamExec(ctx, client, shScript(remotePingScript(req)), func(text string, isErr bool) {
		if m := rePingReply.FindStringSubmatch(text); m != nil {
			from := m[1]
			if m[2] != "" {
				from = m[2]
			}
			seq, _ := strconv.Atoi(m[3])
			ttl, _ := strconv.Atoi(m[4])
			rtt, _ := strconv.ParseFloat(m[5], 64)
			out.add(row{"kind": "reply", "seq": seq, "from": strings.TrimSuffix(from, ":"), "rttMs": rtt, "ttl": ttl, "remote": true})
			return
		}
		if m := rePingTimeout.FindStringSubmatch(text); m != nil {
			seq, _ := strconv.Atoi(m[1] + m[2])
			out.add(row{"kind": "reply", "seq": seq, "error": "timeout", "remote": true})
			return
		}
		if m := rePingLoss.FindStringSubmatch(text); m != nil {
			sent, _ := strconv.Atoi(m[1])
			recv, _ := strconv.Atoi(m[2])
			loss, _ := strconv.ParseFloat(m[3], 64)
			summary["sent"], summary["recv"], summary["loss"] = sent, recv, round1(loss)
			haveSummary = true
			return
		}
		if m := rePingRTTStat.FindStringSubmatch(text); m != nil {
			mn, _ := strconv.ParseFloat(m[1], 64)
			avg, _ := strconv.ParseFloat(m[2], 64)
			mx, _ := strconv.ParseFloat(m[3], 64)
			summary["minMs"], summary["avgMs"], summary["maxMs"] = mn, avg, mx
			if m[4] != "" {
				sd, _ := strconv.ParseFloat(m[4], 64)
				summary["stddevMs"] = sd
			}
			return
		}
		if isErr && strings.TrimSpace(text) != "" && len(errLines) < 20 {
			errLines = append(errLines, strings.TrimSpace(text))
			out.add(row{"kind": "info", "message": strings.TrimSpace(text)})
		}
	})
	if err != nil {
		return err
	}
	if !haveSummary {
		msg := "remote ping produced no result"
		if len(errLines) > 0 {
			msg = errLines[len(errLines)-1]
		} else if code == 127 {
			msg = "ping is not installed on the remote host"
		}
		return errors.New(msg)
	}
	out.emitNow(summary)
	return nil
}

// ---- remote traceroute / mtr ---------------------------------------------------------------------------------------

// remoteTraceScript picks the remote tracer: root uses traceroute (ICMP probes with -I when asked); other users
// prefer tracepath (unprivileged by design), then traceroute (the modern Linux one works unprivileged with UDP; busybox
// needs root — its failure is explained by explainRemoteTraceError). probes/waitSec override the request for mtr rounds.
func remoteTraceScript(req *tracerouteRequest, probes, waitSec int) string {
	icmp := ternary(req.Protocol == "icmp", "1", "0")
	fam := ternary(req.IPv6, "-6 ", "")
	host := shellQuote(req.Host)
	return fmt.Sprintf(`R=0; [ "$(id -u 2>/dev/null)" = 0 ] && R=1; M=; `+
		`if [ %s = 1 ]; then if [ $R = 1 ]; then M=-I; else echo "note: ICMP probes need root on this host; using UDP probes" >&2; fi; fi; `+
		`if [ $R = 0 ] && command -v tracepath >/dev/null 2>&1; then exec tracepath %s-n %s; fi; `+
		`if command -v traceroute >/dev/null 2>&1; then exec traceroute %s$M -n -q %d -m %d -w %d %s; fi; `+
		`if command -v tracepath >/dev/null 2>&1; then exec tracepath %s-n %s; fi; `+
		`echo "neither traceroute nor tracepath is installed on this host" >&2; exit 127`,
		icmp, fam, host, fam, probes, req.MaxHops, waitSec, host, fam, host)
}

// explainRemoteTraceError turns a remote tracer failure into guidance.
func explainRemoteTraceError(hostName, lastErr string, code int) error {
	low := strings.ToLower(lastErr)
	switch {
	case strings.Contains(low, "not permitted") || strings.Contains(low, "permission denied") || strings.Contains(low, "privilege"):
		return fmt.Errorf("traceroute on %s needs root privileges (raw sockets): install tracepath (iputils) there or connect with an account that has root rights (%s)", hostName, lastErr)
	case lastErr != "":
		return errors.New(lastErr)
	default:
		return fmt.Errorf("remote traceroute failed (exit %d)", code)
	}
}

func runTracerouteViaSSH(ctx context.Context, cl *call, req *tracerouteRequest, out *sink) error {
	client, release, err := cl.sshClientFor(ctx, req.ViaConnectionID)
	if err != nil {
		return err
	}
	defer release()

	waitSec := max(1, (req.TimeoutMs+999)/1000)
	out.emitNow(row{"kind": "info", "message": "Running on " + client.Conn.Name + ": traceroute " + req.Host})
	st := &traceLineStream{ctx: ctx, out: out, names: newRDNSCache(), resolve: req.resolveNames()}
	var lastErr string
	code, err := streamExec(ctx, client, shScript(remoteTraceScript(req, req.Probes, waitSec)), func(text string, isErr bool) {
		if isErr {
			if t := strings.TrimSpace(text); t != "" {
				lastErr = t
				out.add(row{"kind": "info", "message": t})
			}
			return
		}
		st.line(text)
	})
	st.finishHop()
	if err != nil {
		return err
	}
	if st.hops == 0 {
		return explainRemoteTraceError(client.Conn.Name, lastErr, code)
	}
	out.emitNow(row{"kind": "summary", "target": orString(st.dest, req.Host), "hops": st.hops, "reached": st.reached(), "engine": "ssh", "remote": true})
	return nil
}

// runMTRViaSSH approximates mtr on a remote host by repeating one-probe traceroute rounds and aggregating per-hop
// statistics (the remote host needs no mtr).
func runMTRViaSSH(ctx context.Context, cl *call, req *tracerouteRequest, out *sink) error {
	client, release, err := cl.sshClientFor(ctx, req.ViaConnectionID)
	if err != nil {
		return err
	}
	defer release()

	waitSec := max(1, (req.TimeoutMs+999)/1000)
	interval := time.Duration(req.IntervalMs) * time.Millisecond
	names := newRDNSCache()
	stats := make([]*hopStats, req.MaxHops+1)
	for i := 1; i <= req.MaxHops; i++ {
		stats[i] = newHopStats(i)
	}
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("mtr to %s from %s (one-probe traceroute rounds)", req.Host, client.Conn.Name)})
	destTTL, rounds := 0, 0
	dest := ""
	for rounds < req.Rounds && ctx.Err() == nil {
		start := time.Now()
		var lastErr string
		st := &traceLineStream{ctx: ctx, out: out}
		st.onHop = func(hop row) {
			ttl, _ := hop["ttl"].(int)
			if ttl < 1 || ttl > req.MaxHops {
				return
			}
			s := stats[ttl]
			times, _ := hop["timesMs"].([]any)
			froms := hopFroms(hop)
			if len(times) == 0 {
				times = []any{nil}
			}
			for i, v := range times {
				s.sent++
				if rtt, ok := v.(float64); ok {
					from := ""
					if len(froms) > 0 {
						from = froms[min(i, len(froms)-1)]
					}
					s.addReply(from, rtt)
				}
			}
			if a, ok := hop["annotation"].(string); ok {
				s.code = a
			}
		}
		code, err := streamExec(ctx, client, shScript(remoteTraceScript(req, 1, waitSec)), func(text string, isErr bool) {
			if isErr {
				if t := strings.TrimSpace(text); t != "" {
					lastErr = t
				}
				return
			}
			st.line(text)
		})
		st.finishHop()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		if st.hops == 0 {
			return explainRemoteTraceError(client.Conn.Name, lastErr, code)
		}
		if st.dest != "" {
			dest = st.dest
		}
		if st.reached() && (destTTL == 0 || st.hops < destTTL) {
			destTTL = st.hops
		}
		rounds++
		last := destTTL
		if last == 0 { // not reached: show up to one hop past the farthest responder
			for i := min(st.hops, req.MaxHops); i >= 1; i-- {
				if stats[i].recv > 0 {
					last = min(i+1, st.hops)
					break
				}
			}
		}
		for i := 1; i <= min(last, req.MaxHops); i++ {
			if stats[i].sent > 0 {
				out.add(stats[i].row(ctx, names, req.resolveNames()))
			}
		}
		out.emitNow(row{"kind": "round", "round": rounds, "lastTtl": last, "reached": destTTL > 0})
		if rounds < req.Rounds && !sleep(ctx, interval-time.Since(start)) {
			break
		}
	}
	out.emitNow(row{"kind": "summary", "target": orString(dest, req.Host), "rounds": rounds, "reached": destTTL > 0, "hops": destTTL, "engine": "ssh", "remote": true})
	return ctx.Err()
}
