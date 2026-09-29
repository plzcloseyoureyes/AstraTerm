//go:build windows

package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// agentEndpoint returns the agent's named pipe (unique per data directory). OpenSSH for Windows accepts it in
// SSH_AUTH_SOCK.
func agentEndpoint(dataDir string) (string, error) {
	sum := sha256.Sum256([]byte(strings.ToLower(dataDir)))
	return `\\.\pipe\astraterm-ssh-agent-` + hex.EncodeToString(sum[:6]), nil
}

// listenAgent creates the named pipe with a security descriptor granting access to the current user only.
func listenAgent(path string) (net.Listener, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("cannot determine the current user: %w", err)
	}
	sddl := "D:P(A;;GA;;;" + tu.User.Sid.String() + ")"
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
}

func cleanupEndpoint(string) {}

// checkPeer identifies the client process of the pipe (the pipe's DACL already restricts it to the current user).
func checkPeer(c net.Conn) (peerInfo, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return peerInfo{}, nil
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return peerInfo{}, nil
	}
	p := peerInfo{pid: int(pid)}
	if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid); err == nil {
		defer windows.CloseHandle(h)
		buf := make([]uint16, windows.MAX_PATH)
		n := uint32(len(buf))
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
			p.name = filepath.Base(windows.UTF16ToString(buf[:n]))
		}
	}
	return p, nil
}
