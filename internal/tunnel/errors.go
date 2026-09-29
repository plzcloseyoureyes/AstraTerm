package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
)

// Windows socket error numbers (syscall.EADDRINUSE & co. are synthetic values there).
const (
	wsaEACCES        = 10013
	wsaEADDRINUSE    = 10048
	wsaEADDRNOTAVAIL = 10049
)

func errnoIs(err error, unix syscall.Errno, windows uintptr) bool {
	if errors.Is(err, unix) {
		return true
	}
	var en syscall.Errno
	return errors.As(err, &en) && uintptr(en) == windows
}

func isAddrInUse(err error) bool { return errnoIs(err, syscall.EADDRINUSE, wsaEADDRINUSE) }

// listenError turns a failure to open a listener on the AstraTerm host into a typed API error with a clear message.
func listenError(addr string, err error) error {
	switch {
	case isAddrInUse(err):
		return httpx.Conflict(fmt.Sprintf("%s is already in use by another program", addr))
	case errnoIs(err, syscall.EACCES, wsaEACCES):
		return httpx.Forbidden(fmt.Sprintf("permission denied listening on %s (ports below 1024 need administrator rights)", addr))
	case errnoIs(err, syscall.EADDRNOTAVAIL, wsaEADDRNOTAVAIL):
		return httpx.BadRequest(fmt.Sprintf("cannot listen on %s: the address does not belong to this machine", addr))
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return httpx.BadRequest(fmt.Sprintf("cannot listen on %s: %s", addr, dnsErr.Err))
	}
	return httpx.BadRequest(fmt.Sprintf("cannot listen on %s: %s", addr, cleanNetErr(err)))
}

// remoteListenError explains why the SSH server refused a remote listener.
func remoteListenError(addr string, unix bool, err error) error {
	msg := err.Error()
	if strings.Contains(msg, "denied by peer") {
		if unix {
			return fmt.Errorf("the SSH server refused to listen on %s (the socket file may already exist, or AllowStreamLocalForwarding is disabled)", addr)
		}
		return fmt.Errorf("the SSH server refused to listen on %s (port already in use, privileged port, or remote forwarding disabled by AllowTcpForwarding / GatewayPorts)", addr)
	}
	return fmt.Errorf("remote listen on %s failed: %s", addr, cleanNetErr(err))
}

// dialError explains why a connection to a destination failed; via is "the SSH server" or "this host".
func dialError(dest, via string, err error) string {
	if be, ok := netguard.IsBlocked(err); ok {
		return be.Error() // refused by the destination policy (SEC-7): the message names the address and the reason
	}
	var oce *ssh.OpenChannelError
	if errors.As(err, &oce) {
		m := strings.TrimSpace(oce.Message)
		switch oce.Reason {
		case ssh.Prohibited:
			return fmt.Sprintf("%s refused to forward to %s (administratively prohibited%s)", via, dest, prefixed(": ", m))
		case ssh.ConnectionFailed:
			if m == "" {
				m = "connection failed"
			}
			return fmt.Sprintf("%s could not connect to %s: %s", via, dest, m)
		case ssh.UnknownChannelType:
			return fmt.Sprintf("%s does not support this kind of forwarding", via)
		case ssh.ResourceShortage:
			return fmt.Sprintf("%s has no resources left for another connection", via)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timed out connecting to %s", dest)
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF):
		return fmt.Sprintf("the SSH connection closed while connecting to %s", dest)
	}
	return fmt.Sprintf("cannot connect to %s: %s", dest, cleanNetErr(err))
}

func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

// cleanNetErr strips Go's "dial tcp 1.2.3.4:5:" style prefixes from network errors.
func cleanNetErr(err error) string {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		var se interface{ Unwrap() error }
		inner := oe.Err
		if errors.As(inner, &se) {
			if u := se.Unwrap(); u != nil {
				inner = u
			}
		}
		return inner.Error()
	}
	return err.Error()
}
