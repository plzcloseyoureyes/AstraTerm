package recording

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/httpx"
)

// wsPair returns two WebSocket endpoints connected in memory (net.Pipe + a real handshake): srv is the accepted side,
// cli the dialing side. The share relay attaches srv to the terminal manager (term.Manager.Attach wants a
// *websocket.Conn) and forwards between cli and the viewer's socket, so every frame a share viewer sends or receives
// passes through this module's policy (read-only enforcement, input pause, attribution, metadata filtering).
func wsPair(ctx context.Context) (srv, cli *websocket.Conn, err error) {
	a, b := net.Pipe()
	type result struct {
		c   *websocket.Conn
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		br := bufio.NewReader(a)
		req, err := http.ReadRequest(br)
		if err != nil {
			a.Close()
			accepted <- result{err: err}
			return
		}
		w := &pipeResponse{conn: a, br: br, h: http.Header{}}
		c, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			a.Close()
		}
		accepted <- result{c: c, err: err}
	}()
	var used atomic.Bool
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("wspipe: single use")
			}
			return b, nil
		},
	}
	cli, _, err = websocket.Dial(ctx, "ws://share.internal/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}})
	if err != nil {
		a.Close()
		b.Close()
		r := <-accepted
		if r.c != nil {
			r.c.CloseNow()
		}
		return nil, nil, fmt.Errorf("wspipe: dial: %w", err)
	}
	r := <-accepted
	if r.err != nil {
		cli.CloseNow()
		return nil, nil, fmt.Errorf("wspipe: accept: %w", r.err)
	}
	cli.SetReadLimit(httpx.WSReadLimit)
	return r.c, cli, nil
}

// pipeResponse is the minimal hijackable http.ResponseWriter websocket.Accept needs.
type pipeResponse struct {
	conn   net.Conn
	br     *bufio.Reader
	h      http.Header
	status int
}

func (w *pipeResponse) Header() http.Header         { return w.h }
func (w *pipeResponse) WriteHeader(code int)        { w.status = code }
func (w *pipeResponse) Write(p []byte) (int, error) { return len(p), nil } // handshake errors only

func (w *pipeResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	status := w.status
	if status == 0 {
		status = http.StatusSwitchingProtocols
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	if err := w.h.Write(&buf); err != nil {
		return nil, nil, err
	}
	buf.WriteString("\r\n")
	if _, err := w.conn.Write(buf.Bytes()); err != nil {
		return nil, nil, err
	}
	return w.conn, bufio.NewReadWriter(w.br, bufio.NewWriter(w.conn)), nil
}
