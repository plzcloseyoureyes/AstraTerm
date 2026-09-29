package tools

import (
	"context"
	"io"
	"net"
	"strings"
	"time"

	"github.com/likexian/whois"

	"github.com/nexterm/nexterm/internal/httpx"
)

type whoisRequest struct {
	Query  string `json:"query"`
	Server string `json:"server"` // optional explicit whois server (host or host:port)
}

// maxWhoisBytes caps what a (possibly hostile, user-chosen) whois server may send per connection.
const maxWhoisBytes = 1 << 20

// ctxDialer adapts a context to likexian/whois's proxy.Dialer interface so the query (and every referral it follows)
// is cancelled with the job, vetted by the network guard and size-limited.
type ctxDialer struct {
	ctx   context.Context
	guard *netGuard
}

func (d ctxDialer) Dial(network, addr string) (net.Conn, error) {
	c, err := d.guard.dialer(10*time.Second).DialContext(d.ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &limitedConn{Conn: c, left: maxWhoisBytes}, nil
}

// limitedConn ends the stream (EOF) after left bytes have been read (single reader).
type limitedConn struct {
	net.Conn
	left int64
}

func (c *limitedConn) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.Conn.Read(p)
	c.left -= int64(n)
	return n, err
}

func prepareWhois(_ context.Context, cl *call) (runner, error) {
	var req whoisRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	req.Query = strings.TrimSpace(req.Query)
	req.Server = strings.TrimSpace(req.Server)
	if req.Query == "" {
		return nil, httpx.BadRequest("a domain, IP or ASN is required")
	}
	if len(req.Query) > 255 || strings.ContainsAny(req.Query, "\r\n") {
		return nil, httpx.BadRequest("invalid query")
	}
	if req.Server != "" {
		host := req.Server
		if h, _, err := net.SplitHostPort(req.Server); err == nil {
			host = h
		}
		if _, err := safeHostArg(host); err != nil {
			return nil, httpx.BadRequest("invalid whois server")
		}
	}
	cl.target = req.Query
	guard := cl.guard
	return func(ctx context.Context, out *sink) error { return runWhois(ctx, guard, &req, out) }, nil
}

func runWhois(ctx context.Context, guard *netGuard, req *whoisRequest, out *sink) error {
	client := whois.NewClient().SetTimeout(15 * time.Second).SetDialer(ctxDialer{ctx: ctx, guard: guard})
	out.emitNow(row{"kind": "info", "message": "Looking up " + req.Query})

	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() { // the library call is not context-aware; the dialer ties its connections to ctx
		var r result
		if req.Server != "" {
			r.text, r.err = client.Whois(req.Query, req.Server)
		} else {
			r.text, r.err = client.Whois(req.Query)
		}
		ch <- r
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err != nil && strings.TrimSpace(r.text) == "" {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return httpx.BadRequest("whois lookup failed: " + r.err.Error())
		}
		out.emitNow(row{"kind": "text", "text": sanitizeText(r.text)})
		summary := row{"kind": "summary", "query": req.Query}
		if r.err != nil {
			summary["warning"] = r.err.Error()
		}
		out.emitNow(summary)
		return nil
	}
}

// sanitizeText drops control characters (except newlines and tabs) from remote text before it reaches the UI.
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f) {
			return r
		}
		return -1
	}, s)
}
