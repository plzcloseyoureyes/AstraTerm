//go:build windows

package docker

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// npipeDialer dials a Windows named pipe (the Docker Desktop engine endpoint).
func npipeDialer(pipe string) (dialer, error) {
	return func(ctx context.Context) (net.Conn, error) {
		timeout := 15 * time.Second
		if dl, ok := ctx.Deadline(); ok {
			timeout = time.Until(dl)
		}
		return winio.DialPipe(pipe, &timeout)
	}, nil
}
