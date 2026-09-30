package keys

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// Installing a public key on a server (TOOL-1 "one-click install", ssh-copy-id semantics): ~/.ssh is created with
// mode 0700, the key line is appended to ~/.ssh/authorized_keys unless the same key is already listed (compared by
// key blob, options and comments ignored), and authorized_keys gets mode 0600. SFTP is used when the server offers
// it (no dependence on the login shell); otherwise a POSIX sh script runs over exec.

const maxAuthorizedKeysSize = 4 << 20

// installResult is the response of POST /api/keys/{id}/install.
type installResult struct {
	Installed      bool   `json:"installed"`      // the key was appended
	AlreadyPresent bool   `json:"alreadyPresent"` // the key was already authorized
	Method         string `json:"method"`         // sftp | shell
	Path           string `json:"path"`           // authorized_keys path on the server
	Target         string `json:"target"`         // user@host:port
}

// installTimeout bounds one installation (the SFTP client is shared with other users of the connection, so a stuck
// server is waited for in the background instead of being torn down).
const installTimeout = 60 * time.Second

// installPublicKey appends line (an authorized_keys line of pub) on the server of cl.
func installPublicKey(ctx context.Context, cl *sshx.Client, pub ssh.PublicKey, line string) (*installResult, error) {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	sc, err := cl.SFTP()
	if err == nil {
		type outcome struct {
			res *installResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := installViaSFTP(sc, pub, line)
			done <- outcome{res, err}
		}()
		var o outcome
		select {
		case o = <-done:
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, &installError{"the server did not answer in time"}
			}
			return nil, ctx.Err()
		}
		if o.err == nil {
			o.res.Method = "sftp"
			relabelSELinux(ctx, cl, sc)
			return o.res, nil
		}
		if _, ok := errors.AsType[*installError](o.err); ok {
			return nil, o.err // a definite problem (not a missing SFTP subsystem): do not retry over the shell
		}
		return nil, fmt.Errorf("SFTP: %w", o.err)
	}
	res, err := installViaShell(ctx, cl, pub, line)
	if err != nil {
		return nil, err
	}
	res.Method = "shell"
	return res, nil
}

// installError is a problem found on the server (not a transport failure).
type installError struct{ msg string }

func (e *installError) Error() string { return e.msg }

func installViaSFTP(sc *sftp.Client, pub ssh.PublicKey, line string) (*installResult, error) {
	home, err := sc.Getwd()
	if err != nil || home == "" {
		home = "."
	}
	dir := path.Join(home, ".ssh")
	file := path.Join(dir, "authorized_keys")
	res := &installResult{Path: file}

	st, err := sc.Stat(dir)
	switch {
	case err == nil && !st.IsDir():
		return nil, &installError{fmt.Sprintf("%s exists and is not a directory", dir)}
	case err == nil:
		if st.Mode().Perm() != 0o700 {
			if err := sc.Chmod(dir, 0o700); err != nil {
				return nil, &installError{fmt.Sprintf("cannot set the mode of %s to 0700: %v", dir, err)}
			}
		}
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist):
		if err := sc.Mkdir(dir); err != nil {
			return nil, &installError{fmt.Sprintf("cannot create %s: %v", dir, err)}
		}
		if err := sc.Chmod(dir, 0o700); err != nil {
			return nil, &installError{fmt.Sprintf("cannot set the mode of %s: %v", dir, err)}
		}
	default:
		return nil, err
	}

	var existing []byte
	if f, err := sc.Open(file); err == nil {
		existing, err = io.ReadAll(io.LimitReader(f, maxAuthorizedKeysSize+1))
		f.Close()
		if err != nil {
			return nil, &installError{fmt.Sprintf("cannot read %s: %v", file, err)}
		}
		if len(existing) > maxAuthorizedKeysSize {
			return nil, &installError{fmt.Sprintf("%s is too large", file)}
		}
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		return nil, &installError{fmt.Sprintf("cannot read %s: %v", file, err)}
	}

	if authorizedKeysContain(existing, pub) {
		res.AlreadyPresent = true
	} else {
		if err := appendAuthorizedKey(sc, file, existing, line); err != nil {
			return nil, err
		}
		res.Installed = true
	}
	if st, err := sc.Stat(file); err == nil && st.Mode().Perm() != 0o600 {
		if err := sc.Chmod(file, 0o600); err != nil {
			return nil, &installError{fmt.Sprintf("cannot set the mode of %s to 0600: %v", file, err)}
		}
	}
	if res.Installed {
		// Verify: some servers (read-only home, quota) accept writes they do not keep, and the previous content must
		// have survived.
		if f, err := sc.Open(file); err == nil {
			data, _ := io.ReadAll(io.LimitReader(f, maxAuthorizedKeysSize+int64(len(line))+2))
			f.Close()
			if !authorizedKeysContain(data, pub) {
				return nil, &installError{fmt.Sprintf("the key was written but %s does not contain it", file)}
			}
			if !bytes.HasPrefix(data, existing) {
				return nil, &installError{fmt.Sprintf("%s changed unexpectedly while the key was added; check the file", file)}
			}
		}
	}
	return res, nil
}

// appendAuthorizedKey appends line to file, whose current content is existing. The write goes to the explicit end
// offset: OpenSSH's sftp-server honors SSH_FXF_APPEND (and ignores the offset), but a server that ignores the flag
// would otherwise overwrite the beginning of the file and lock the user out.
func appendAuthorizedKey(sc *sftp.Client, file string, existing []byte, line string) error {
	f, err := sc.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return &installError{fmt.Sprintf("cannot open %s for writing: %v", file, err)}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return &installError{fmt.Sprintf("cannot read the size of %s: %v", file, err)}
	}
	if st.Size() != int64(len(existing)) {
		return &installError{fmt.Sprintf("%s changed while the key was being added; try again", file)}
	}
	var b bytes.Buffer
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(line + "\n")
	_, werr := f.WriteAt(b.Bytes(), st.Size())
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return &installError{fmt.Sprintf("cannot write %s: %v", file, errors.Join(werr, cerr))}
	}
	return nil
}

// relabelSELinux restores the SELinux context of ~/.ssh like ssh-copy-id (best effort, only where SELinux is on).
func relabelSELinux(ctx context.Context, cl *sshx.Client, sc *sftp.Client) {
	if _, err := sc.Stat("/sys/fs/selinux/enforce"); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _, _, _ = cl.Exec(ctx, "command -v restorecon >/dev/null 2>&1 && restorecon -F ~/.ssh ~/.ssh/authorized_keys")
}

// authorizedKeysContain reports whether an authorized_keys file lists pub (by key blob; options, comments and
// cert-authority lines are handled by ssh.ParseAuthorizedKey).
func authorizedKeysContain(data []byte, pub ssh.PublicKey) bool {
	want := pub.Marshal()
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pk, _, opts, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			continue
		}
		if isCertAuthorityLine(opts) {
			continue
		}
		if bytes.Equal(pk.Marshal(), want) {
			return true
		}
	}
	return false
}

func isCertAuthorityLine(opts []string) bool {
	for _, o := range opts {
		if strings.EqualFold(strings.TrimSpace(o), "cert-authority") {
			return true
		}
	}
	return false
}

// installViaShell runs ssh-copy-id's logic in a POSIX shell fed through stdin (works whatever the login shell is, as
// long as `sh` exists). The key line only contains base64 and a sanitized comment, so it is safe inside single quotes.
func installViaShell(ctx context.Context, cl *sshx.Client, pub ssh.PublicKey, line string) (*installResult, error) {
	blob := strings.Fields(line)[1]
	safeLine := strings.Map(func(r rune) rune {
		if r == '\'' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
	script := strings.Join([]string{
		"umask 077",
		"cd || exit 1",
		"mkdir -p .ssh && chmod 700 .ssh || exit 1",
		"f=.ssh/authorized_keys",
		"if [ -f \"$f\" ] && grep -q -F -e '" + blob + "' \"$f\"; then chmod 600 \"$f\"; echo ASTRATERM_KEY_PRESENT; exit 0; fi",
		"if [ -s \"$f\" ] && [ -n \"$(tail -c 1 \"$f\")\" ]; then echo >> \"$f\" || exit 1; fi",
		"printf '%s\\n' '" + safeLine + "' >> \"$f\" && chmod 600 \"$f\" || exit 1",
		"command -v restorecon >/dev/null 2>&1 && restorecon -F .ssh \"$f\" >/dev/null 2>&1",
		"echo ASTRATERM_KEY_ADDED",
		"",
	}, "\n")
	sess, _, release, err := cl.NewSessionContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	defer sess.Close()
	var out, errOut bytes.Buffer
	sess.Stdin = strings.NewReader(script)
	sess.Stdout, sess.Stderr = &out, &errOut
	done := make(chan error, 1)
	go func() { done <- sess.Run("sh -s") }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Close()
		return nil, ctx.Err()
	case <-time.After(60 * time.Second):
		_ = sess.Close()
		return nil, errors.New("the server did not answer in time")
	}
	res := &installResult{Path: "~/.ssh/authorized_keys"}
	switch {
	case strings.Contains(out.String(), "ASTRATERM_KEY_PRESENT"):
		res.AlreadyPresent = true
	case strings.Contains(out.String(), "ASTRATERM_KEY_ADDED"):
		res.Installed = true
	default:
		msg := strings.TrimSpace(errOut.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return nil, &installError{"the server has no SFTP subsystem and the shell script failed: " + msg}
	}
	return res, nil
}
