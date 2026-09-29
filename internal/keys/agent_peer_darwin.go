//go:build darwin

package keys

import (
	"fmt"
	"net"
	"os"

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
		cred       *unix.Xucred
		pid        int
		cerr, perr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, perr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return peerInfo{}, err
	}
	if cerr != nil {
		return peerInfo{}, cerr
	}
	if int(cred.Uid) != os.Getuid() && cred.Uid != 0 {
		return peerInfo{}, fmt.Errorf("the peer runs as another user (uid %d)", cred.Uid)
	}
	p := peerInfo{}
	if perr == nil && pid > 0 {
		p.pid = pid
		if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil {
			p.name = unix.ByteSliceToString(kp.Proc.P_comm[:])
		}
	}
	return p, nil
}
