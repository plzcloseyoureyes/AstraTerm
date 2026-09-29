package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// Message is one conversation turn sent to a provider.
type Message struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

// Request is a provider-neutral completion request.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
	Effort    string // "" (provider default) | low | medium | high
}

// Event is one streamed provider event.
type Event struct {
	Type       string // "text" | "thinking" | "done"
	Text       string
	StopReason string
	Usage      *Usage
}

// Usage reports token counts when the provider returns them.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Stream yields provider events until a "done" event or an error.
type Stream interface {
	Next() (Event, error)
	Close() error
}

// Provider talks to one LLM API.
type Provider interface {
	// Open sends the request and returns the event stream once the provider accepted it. HTTP-level failures are
	// returned as *ProviderError before anything was streamed.
	Open(ctx context.Context, req Request) (Stream, error)
	// Models lists the models the endpoint offers.
	Models(ctx context.Context) ([]ModelInfo, error)
}

// ProviderError is a failure reported by (or while reaching) the provider.
type ProviderError struct {
	Status  int    // provider HTTP status (0 = transport failure)
	Kind    string // provider error type, e.g. authentication_error
	Message string
	Label   string // provider label for messages
}

func (e *ProviderError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s is unreachable: %s", e.Label, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Label, e.Message)
}

// code maps the failure to an API error code.
func (e *ProviderError) code() string {
	switch {
	case e.Status == 0:
		return "provider_unreachable"
	case e.Status == 401 || e.Status == 403:
		return "provider_auth"
	case e.Status == 429:
		return "provider_rate_limited"
	case e.Status == 529 || e.Status == 503 || e.Kind == "overloaded_error":
		return "provider_overloaded"
	case e.Status == 404:
		return "provider_not_found"
	case e.Status >= 400 && e.Status < 500:
		return "provider_bad_request"
	default:
		return "provider_error"
	}
}

// httpError converts the failure into the API error returned to the browser: 424 Failed Dependency with a specific
// code. (A 5xx would have its message replaced by the router; the user needs the provider's explanation.)
func (e *ProviderError) httpError() error {
	return httpx.NewError(http.StatusFailedDependency, e.code(), clip(e.Error(), 400))
}

// friendlyTransport turns low-level network errors into short messages.
func friendlyTransport(err error) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		return "the host name could not be resolved (" + dnsErr.Name + ")"
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		if strings.Contains(opErr.Err.Error(), "refused") {
			return "the connection was refused — is the server running?"
		}
		return clip(opErr.Err.Error(), 200)
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		msg = msg[i+2:]
	}
	return clip(msg, 200)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// ---- HTTP plumbing ------------------------------------------------------------------------------------------------

const (
	// idleReadTimeout ends a stream when the provider sends nothing (not even a ping) for this long.
	idleReadTimeout = 120 * time.Second
	maxErrorBody    = 64 << 10
	maxSSELine      = 4 << 20
)

// newHTTPClient returns a client for provider calls; transport may be customised (network guard).
func newHTTPClient(transport *http.Transport) *http.Client {
	if transport == nil {
		transport = defaultTransport()
	}
	return &http.Client{
		Transport: transport, // no overall timeout: streams can be long; see idleReader
		// Never follow redirects: Go forwards custom headers such as x-api-key to the new host, and a 307/308 would
		// replay the (redacted, but still private) conversation to wherever the endpoint points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second, // local servers may load a model before answering
		ExpectContinueTimeout: time.Second,
	}
}

// idleReader cancels the request when no bytes arrive for idleReadTimeout.
type idleReader struct {
	r      io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	once   sync.Once
}

func newIdleReader(r io.ReadCloser, cancel context.CancelFunc, d time.Duration) *idleReader {
	ir := &idleReader{r: r, cancel: cancel}
	ir.timer = time.AfterFunc(d, cancel)
	return ir
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.timer.Reset(idleReadTimeout)
	}
	return n, err
}

func (ir *idleReader) Close() error {
	var err error
	ir.once.Do(func() {
		ir.timer.Stop()
		err = ir.r.Close()
		ir.cancel()
	})
	return err
}

// doJSON posts (or GETs when body is nil) and returns the response for 2xx; other statuses become *ProviderError
// via parseErr.
func doRequest(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, label string,
	parseErr func(status int, b []byte) *ProviderError) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, &ProviderError{Label: label, Message: "invalid endpoint URL"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "AstraTerm")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ProviderError{Label: label, Message: friendlyTransport(err)}
	}
	if res.StatusCode/100 == 3 {
		loc := res.Header.Get("Location")
		_ = res.Body.Close()
		msg := fmt.Sprintf("the endpoint redirects (HTTP %d); enter the final URL as base URL", res.StatusCode)
		if u, err := neturl.Parse(loc); err == nil && u.Host != "" {
			msg = fmt.Sprintf("the endpoint redirects to %s://%s (HTTP %d); enter the final URL as base URL", u.Scheme, u.Host, res.StatusCode)
		}
		return nil, &ProviderError{Status: res.StatusCode, Label: label, Message: msg}
	}
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		_ = res.Body.Close()
		return nil, parseErr(res.StatusCode, b)
	}
	return res, nil
}

// ---- Server-Sent Events reader ------------------------------------------------------------------------------------

type sseEvent struct {
	Event string
	Data  string
}

type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 64<<10)}
}

func (s *sseReader) readLine() (string, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := s.br.ReadLine()
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxSSELine {
			return "", errors.New("event stream line too long")
		}
		if !isPrefix {
			return string(buf), nil
		}
	}
}

// Next returns the next dispatched event (io.EOF at the end of the stream).
func (s *sseReader) Next() (sseEvent, error) {
	var ev sseEvent
	var data []string
	has := false
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF && has {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return sseEvent{}, err
		}
		if line == "" {
			if !has {
				continue
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
			has = true
		case "data":
			data = append(data, value)
			has = true
		}
	}
}
