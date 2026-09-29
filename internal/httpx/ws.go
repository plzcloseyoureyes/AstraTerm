package httpx

import (
	"context"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"
)

// WSReadLimit is the maximum size of a single incoming WebSocket message.
const WSReadLimit = 16 << 20

func withOriginPatterns(ctx context.Context, p []string) context.Context {
	return context.WithValue(ctx, originPatternsKey, p)
}

// AcceptWS upgrades the request to a WebSocket (github.com/coder/websocket over c.Response(), which supports
// Hijack). It enforces the same-origin policy (plus the Vite dev origins in --dev mode) and raises the read limit to
// WSReadLimit. opts may be nil. On failure the handshake error response has already been written: the handler
// should just return.
func AcceptWS(c *echo.Context, opts *websocket.AcceptOptions) (*websocket.Conn, error) {
	var o websocket.AcceptOptions
	if opts != nil {
		o = *opts
	}
	req := c.Request()
	if p, ok := req.Context().Value(originPatternsKey).([]string); ok {
		o.OriginPatterns = append(append([]string(nil), o.OriginPatterns...), p...)
	}
	conn, err := websocket.Accept(c.Response(), req, &o)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(WSReadLimit)
	return conn, nil
}
