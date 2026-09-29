//go:build !windows

package docker

import "errors"

// npipeDialer is only available on Windows; other platforms cannot reach a npipe:// engine.
func npipeDialer(string) (dialer, error) {
	return nil, errors.New("npipe:// Docker hosts are only supported on Windows")
}
