package vfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"syscall"

	"github.com/pkg/sftp"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Error codes of the files API (SPEC §9 files-backend).
const (
	codeFSNotFound         = "fs_not_found"
	codePermissionDenied   = "permission_denied"
	codeExists             = "exists"
	codeNotEmpty           = "not_empty"
	codeNotSupported       = "not_supported"
	codeDisconnected       = "disconnected"
	codeSessionNotConn     = "session_not_connected"
	codeFSError            = "fs_error"
	codeOffsetMismatch     = "offset_mismatch"
	codeSSHBrowserDisabled = "ssh_browser_disabled"
	codeTooLarge           = "too_large"
)

var (
	errFSNotFound = httpx.NewError(http.StatusNotFound, codeFSNotFound, "file system handle not found (closed or expired)")
	errNotConn    = httpx.NewError(http.StatusConflict, codeSessionNotConn, "session is not connected")
)

// fsError converts a driver error into an HTTP error with a stable code. p (optional) names the path involved.
func fsError(err error, p string) error {
	if err == nil {
		return nil
	}
	var he *httpx.HTTPError
	if errors.As(err, &he) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, term.ErrNotConnected) || errors.Is(err, term.ErrClosed) {
		return errNotConn
	}
	var me *model.Error
	if errors.As(err, &me) {
		return err
	}
	suffix := ""
	if p != "" {
		suffix = ": " + p
	}
	switch {
	case errors.Is(err, ErrNotSupported):
		return httpx.NewError(http.StatusBadRequest, codeNotSupported, "operation not supported by this file system")
	case errors.Is(err, fs.ErrNotExist):
		return httpx.NewError(http.StatusNotFound, model.CodeNotFound, "no such file or directory"+suffix)
	case errors.Is(err, fs.ErrPermission):
		return httpx.NewError(http.StatusForbidden, codePermissionDenied, "permission denied"+suffix)
	case errors.Is(err, errNotEmpty), errors.Is(err, syscall.ENOTEMPTY): // before ErrExist: Go maps ENOTEMPTY to it
		return httpx.NewError(http.StatusConflict, codeNotEmpty, "directory not empty"+suffix)
	case errors.Is(err, fs.ErrExist):
		return httpx.NewError(http.StatusConflict, codeExists, "file already exists"+suffix)
	case errors.Is(err, syscall.ENOTDIR):
		return httpx.NewError(http.StatusConflict, codeFSError, "not a directory"+suffix)
	case errors.Is(err, syscall.EISDIR):
		return httpx.NewError(http.StatusConflict, codeFSError, "is a directory"+suffix)
	case isDisconnect(err):
		return httpx.NewError(http.StatusConflict, codeDisconnected, "connection lost: "+cleanMsg(err.Error()))
	}
	var se *sftp.StatusError
	if errors.As(err, &se) {
		switch se.FxCode() {
		case sftp.ErrSSHFxOpUnsupported:
			return httpx.NewError(http.StatusBadRequest, codeNotSupported, "operation not supported by the SFTP server")
		case sftp.ErrSSHFxConnectionLost, sftp.ErrSSHFxNoConnection:
			return httpx.NewError(http.StatusConflict, codeDisconnected, "connection lost")
		}
		return httpx.NewError(http.StatusUnprocessableEntity, codeFSError, "operation failed"+suffix+" ("+cleanMsg(se.Error())+")")
	}
	var tpe *textproto.Error
	if errors.As(err, &tpe) {
		switch tpe.Code {
		case 550, 553:
			msg := strings.ToLower(tpe.Msg)
			switch {
			case strings.Contains(msg, "permission") || strings.Contains(msg, "denied"):
				return httpx.NewError(http.StatusForbidden, codePermissionDenied, "permission denied"+suffix)
			case strings.Contains(msg, "no such") || strings.Contains(msg, "not found") || strings.Contains(msg, "can't find") ||
				strings.Contains(msg, "cannot find") || strings.Contains(msg, "does not exist"):
				return httpx.NewError(http.StatusNotFound, model.CodeNotFound, "no such file or directory"+suffix)
			}
		case 502, 504:
			return httpx.NewError(http.StatusBadRequest, codeNotSupported, "operation not supported by the FTP server")
		}
		return httpx.NewError(http.StatusUnprocessableEntity, codeFSError, cleanMsg(tpe.Error()))
	}
	return httpx.NewError(http.StatusUnprocessableEntity, codeFSError, cleanMsg(err.Error()))
}

// isDisconnect reports whether err means the transport itself failed (as opposed to a file-level error).
func isDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errDisconnected) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, sftp.ErrSSHFxNoConnection) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"connection lost", "ssh connection is closed", "use of closed network connection",
		"broken pipe", "connection reset", "sftp: no connection", "session is not connected"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// isTransient reports whether an operation that failed with err may succeed when retried (transfer retries).
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrExist) ||
		errors.Is(err, ErrNotSupported) {
		return false
	}
	return isDisconnect(err) || errors.Is(err, context.DeadlineExceeded)
}

// IsTransient is isTransient for other packages (transfer retries).
func IsTransient(err error) bool { return isTransient(err) }

// cleanMsg keeps error messages short and printable.
func cleanMsg(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// shellError maps the stderr of a failed shell command to an fs error.
func shellError(stderr []byte, code int, fallback string) error {
	msg := strings.TrimSpace(string(stderr))
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "no such file") || strings.Contains(low, "not found") && !strings.Contains(low, "command not found"):
		return &os.PathError{Op: fallback, Path: "", Err: fs.ErrNotExist}
	case strings.Contains(low, "permission denied") || strings.Contains(low, "operation not permitted") ||
		strings.Contains(low, "read-only file system"):
		return &os.PathError{Op: fallback, Path: "", Err: fs.ErrPermission}
	case strings.Contains(low, "file exists"):
		return &os.PathError{Op: fallback, Path: "", Err: fs.ErrExist}
	case strings.Contains(low, "not empty"):
		return errNotEmpty
	case strings.Contains(low, "command not found") || strings.Contains(low, "not found") || code == 127:
		return ErrNotSupported
	}
	if msg == "" {
		msg = fallback + " failed"
		if code > 0 {
			msg += " (exit status " + itoa(code) + ")"
		}
	}
	return errors.New(cleanMsg(firstLine(msg)))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
