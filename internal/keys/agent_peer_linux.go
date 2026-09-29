//go:build linux

package keys

import (
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// checkPeer identifies the process on the other end of the agent socket and rejects other users (root excepted,
// like ssh-agent).
func checkPeer(c net.Conn) (peerInfo, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peerInfo{}, nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peerInfo{}, err
	}
	var (
		cred *unix.Ucred
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return peerInfo{}, err
	}
	if cerr != nil {
		return peerInfo{}, cerr
	}
	if int(cred.Uid) != os.Getuid() && cred.Uid != 0 {
		return peerInfo{}, fmt.Errorf("the peer runs as another user (uid %d)", cred.Uid)
	}
	p := peerInfo{pid: int(cred.Pid)}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", cred.Pid)); err == nil {
		p.name = strings.TrimSpace(string(b))
	}
	return p, nil
}
