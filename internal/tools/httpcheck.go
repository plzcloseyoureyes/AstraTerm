package tools

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"

	"github.com/nexterm/nexterm/internal/httpx"
)

type httpCheckRequest struct {
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Headers         map[string]string `json:"headers"`
	Body            string            `json:"body"`
	FollowRedirects bool              `json:"followRedirects"`
	InsecureTLS     bool              `json:"insecureTls"`
	TimeoutMs       int               `json:"timeoutMs"`
	Count           int               `json:"count"`      // httping: repeat the request (default 1)
	IntervalMs      int               `json:"intervalMs"` // between repeats (default 1000)
}

const (
	maxHTTPBodyRead   = 8 << 20 // bytes read (and discarded) per response
	httpPreviewBytes  = 2048
	maxHTTPCheckCount = 100
)

func prepareHTTPCheck(_ context.Context, cl *call) (runner, error) {
	var req httpCheckRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		return nil, httpx.BadRequest("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, httpx.BadRequest("invalid URL (http:// or https:// with a host)")
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !httpguts.ValidHeaderFieldName(method) || method == http.MethodConnect {
		return nil, httpx.BadRequest("invalid method")
	}
	for k, v := range req.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return nil, httpx.BadRequest("invalid header: " + k)
		}
	}
	if len(req.Body) > 1<<20 {
		return nil, httpx.BadRequest("request body too large (max 1 MiB)")
	}
	req.Method = method
	req.TimeoutMs = clampInt(orDefault(req.TimeoutMs, 15000), 500, 120000)
	req.Count = clampInt(orDefault(req.Count, 1), 1, maxHTTPCheckCount)
	req.IntervalMs = clampInt(orDefault(req.IntervalMs, 1000), 100, 60000)
	cl.target = redactURL(u)
	cl.details = map[string]any{"method": method}
	guard := cl.guard
	return func(ctx context.Context, out *sink) error { return runHTTPCheck(ctx, guard, &req, u, out) }, nil
}

// redactURL drops the password of a user:password@ URL (audit / info lines).
func redactURL(u *url.URL) string {
	c := *u
	if c.User != nil {
		if _, ok := c.User.Password(); ok {
			c.User = url.UserPassword(c.User.Username(), "xxxxx")
		}
	}
	return c.String()
}

// hopTiming records the httptrace breakpoints of the current request hop (the final hop after redirects). Trace
// hooks run on transport goroutines (and happy-eyeballs dials race), hence the mutex.
type hopTiming struct {
	mu sync.Mutex
	d  hopData
}

type hopData struct {
	start               time.Time // this hop's request start (GetConn)
	dnsStart, dnsDone   time.Time
	connStart, connDone time.Time
	tlsStart, tlsDone   time.Time
	firstByte           time.Time
	remoteAddr          string
}

func (t *hopTiming) set(f func(d *hopData)) {
	t.mu.Lock()
	f(&t.d)
	t.mu.Unlock()
}

func (t *hopTiming) snapshot() hopData {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.d
}

func (t *hopTiming) trace() *httptrace.ClientTrace {
	first := func(p *time.Time) {
		if p.IsZero() {
			*p = time.Now()
		}
	}
	return &httptrace.ClientTrace{
		GetConn:  func(string) { t.set(func(d *hopData) { *d = hopData{start: time.Now()} }) },
		DNSStart: func(httptrace.DNSStartInfo) { t.set(func(d *hopData) { first(&d.dnsStart) }) },
		DNSDone:  func(httptrace.DNSDoneInfo) { t.set(func(d *hopData) { first(&d.dnsDone) }) },
		ConnectStart: func(_, _ string) {
			t.set(func(d *hopData) { first(&d.connStart) })
		},
		ConnectDone: func(_, addr string, err error) {
			t.set(func(d *hopData) {
				if err == nil && d.connDone.IsZero() {
					d.connDone, d.remoteAddr = time.Now(), addr
				}
			})
		},
		TLSHandshakeStart:    func() { t.set(func(d *hopData) { d.tlsStart = time.Now() }) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.set(func(d *hopData) { d.tlsDone = time.Now() }) },
		GotFirstResponseByte: func() { t.set(func(d *hopData) { d.firstByte = time.Now() }) },
	}
}

func span(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return 0
	}
	return ms(b.Sub(a))
}

func runHTTPCheck(ctx context.Context, guard *netGuard, req *httpCheckRequest, u *url.URL, out *sink) error {
	out.emitNow(row{"kind": "info", "message": req.Method + " " + redactURL(u)})
	var okCount int
	var totals []float64
	for attempt := 1; attempt <= req.Count; attempt++ {
		if attempt > 1 && !sleep(ctx, time.Duration(req.IntervalMs)*time.Millisecond) {
			break
		}
		r, total, err := httpAttempt(ctx, guard, req, u, attempt, out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if req.Count == 1 {
				return err
			}
			out.add(row{"kind": "result", "attempt": attempt, "error": err.Error()})
			continue
		}
		okCount++
		totals = append(totals, total)
		out.emitNow(r)
	}
	if req.Count > 1 {
		s := row{"kind": "summary", "count": req.Count, "ok": okCount, "failed": req.Count - okCount}
		if len(totals) > 0 {
			mn, mx, sum := totals[0], totals[0], 0.0
			for _, v := range totals {
				mn, mx, sum = min(mn, v), max(mx, v), sum+v
			}
			s["minMs"], s["avgMs"], s["maxMs"] = round3(mn), round3(sum/float64(len(totals))), round3(mx)
		}
		out.emitNow(s)
	}
	return nil
}

func httpAttempt(ctx context.Context, guard *netGuard, req *httpCheckRequest, u *url.URL, attempt int, out *sink) (row, float64, error) {
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	tm := &hopTiming{}
	reqCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, tm.trace()), timeout)
	defer cancel()
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(reqCtx, req.Method, u.String(), body)
	if err != nil {
		return nil, 0, httpx.BadRequest("invalid request: " + err.Error())
	}
	hreq.Header.Set("User-Agent", "NexTerm-httpcheck")
	for k, v := range req.Headers {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if strings.EqualFold(k, "Host") {
			hreq.Host = v
			continue
		}
		hreq.Header.Set(k, v)
	}
	transport := &http.Transport{
		DialContext:            guard.dialer(timeout).DialContext,
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: req.InsecureTLS, MinVersion: tls.VersionTLS10}, //nolint:gosec // user-requested diagnostics
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: 1 << 20,
	}
	defer transport.CloseIdleConnections()
	redirects := 0
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if !req.FollowRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			redirects++
			out.add(row{"kind": "redirect", "attempt": attempt, "to": redactURL(r.URL), "hop": len(via), "status": r.Response.StatusCode})
			return nil
		},
	}
	start := time.Now()
	resp, err := client.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	preview := make([]byte, 0, httpPreviewBytes)
	buf := make([]byte, 32*1024)
	var n int64
	for n < maxHTTPBodyRead {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if room := httpPreviewBytes - len(preview); room > 0 {
				preview = append(preview, buf[:min(k, room)]...)
			}
			n += int64(k)
		}
		if rerr != nil {
			break
		}
	}
	end := time.Now()
	total := ms(end.Sub(start))

	names := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	headers := make([]row, 0, len(names))
	for _, k := range names {
		headers = append(headers, row{"name": k, "value": strings.Join(resp.Header[k], ", ")})
	}

	d := tm.snapshot()
	timing := row{
		"dnsMs": span(d.dnsStart, d.dnsDone), "connectMs": span(d.connStart, d.connDone),
		"tlsMs": span(d.tlsStart, d.tlsDone), "ttfbMs": span(d.start, d.firstByte),
		"redirectMs": span(start, d.start), "totalMs": total,
	}
	remote := d.remoteAddr

	result := row{
		"kind": "result", "attempt": attempt, "status": resp.StatusCode, "statusText": resp.Status, "proto": resp.Proto,
		"remoteAddr": remote, "bodyBytes": n, "truncated": n >= maxHTTPBodyRead, "contentType": resp.Header.Get("Content-Type"),
		"headers": headers, "timing": timing, "redirects": redirects, "url": redactURL(resp.Request.URL),
	}
	if p := textPreview(preview, resp.Header.Get("Content-Type")); p != "" {
		result["bodyPreview"] = p
	}
	if resp.TLS != nil {
		result["tls"] = tlsStateRow(resp.TLS)
	}
	return result, total, nil
}

// textPreview returns the start of a textual body (sanitized) for display; binary bodies yield "".
func textPreview(b []byte, contentType string) string {
	if len(b) == 0 {
		return ""
	}
	mt, _, _ := mime.ParseMediaType(contentType)
	textual := strings.HasPrefix(mt, "text/") || strings.Contains(mt, "json") || strings.Contains(mt, "xml") ||
		strings.Contains(mt, "javascript") || mt == "application/x-www-form-urlencoded" || mt == ""
	if !textual {
		return ""
	}
	for len(b) > 0 && !utf8.Valid(b) { // do not cut a rune in half
		b = b[:len(b)-1]
	}
	if mt == "" && strings.ContainsRune(string(b), 0) {
		return ""
	}
	return sanitizeText(string(b))
}

// tlsStateRow summarizes a negotiated TLS connection for the HTTP check result.
func tlsStateRow(st *tls.ConnectionState) row {
	r := row{"version": tlsVersionName(st.Version), "cipher": tls.CipherSuiteName(st.CipherSuite), "alpn": st.NegotiatedProtocol}
	if len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		r["subject"] = leaf.Subject.String()
		r["issuer"] = leaf.Issuer.String()
		r["notAfter"] = leaf.NotAfter.UTC().Format(time.RFC3339)
		r["daysRemaining"] = int(time.Until(leaf.NotAfter).Hours() / 24)
	}
	return r
}
