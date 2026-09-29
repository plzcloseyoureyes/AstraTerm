package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/events"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// Privileged actions (MON-3 "optional sudo"): `sudo -n` first; when sudo wants a password, the session's stored
// sudoPassword, then its login password, then the user is asked through the prompt broker (with "remember" saving it
// as the connection's sudoPassword secret). Everything runs as POSIX sh scripts: the password is fed to `sudo -S`
// through a here-document with a random delimiter, so it never appears on a command line and never depends on the
// remote user's login shell.

// execArgv runs a plain command on the target, through sudo when asked.
func (s *Service) execArgv(ctx context.Context, t *target, argv []string, sudo bool) (*result, error) {
	if !sudo {
		if t.local {
			return s.run(ctx, t, command{argv: argv})
		}
		return s.run(ctx, t, command{sh: "exec " + shJoin(argv)})
	}
	if t.host.Platform == platWindows {
		return nil, httpx.BadRequest("sudo is not available on Windows hosts")
	}
	pw, err := s.sudoAuth(ctx, t)
	if err != nil {
		return nil, err
	}
	line, doc := sudoLine(argv, pw)
	return s.run(ctx, t, command{sh: "exec " + line + doc})
}

// sudoLine renders a sudo invocation of argv as a command line plus the here-document lines that follow it: sudo -n
// when no password is needed, else `sudo -S` reading the password from a quoted here-document (no expansion).
func sudoLine(argv []string, pw string) (line, doc string) {
	if pw == "" {
		return "env LC_ALL=C sudo -n -- " + shJoin(argv), ""
	}
	d := heredocDelimiter()
	return "env LC_ALL=C sudo -S -p '' -- " + shJoin(argv) + " <<'" + d + "'", "\n" + pw + "\n" + d
}

func heredocDelimiter() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "NX_PW_" + strings.ToUpper(hex.EncodeToString(b))
}

// sudoAuth returns the password sudo needs on the target ("" when none is needed).
func (s *Service) sudoAuth(ctx context.Context, t *target) (string, error) {
	res, err := s.run(ctx, t, command{sh: "exec env LC_ALL=C sudo -n true"})
	if err != nil {
		return "", err
	}
	if res.ok() {
		return "", nil
	}
	if err := sudoFatal(res, t); err != nil {
		return "", err
	}
	var cands []string
	if t.sess != nil {
		if _, sec, err := t.sess.Resolve(ctx); err == nil {
			for _, k := range []string{model.SecretSudoPassword, model.SecretPassword} {
				if v := sec[k]; v != "" && !strings.ContainsAny(v, "\r\n\x00") && !slices.Contains(cands, v) {
					cands = append(cands, v)
				}
			}
		}
	}
	for _, pw := range cands {
		ok, err := s.sudoCheck(ctx, t, pw)
		if err != nil {
			return "", err
		}
		if ok {
			s.rememberSudo(t, pw)
			return pw, nil
		}
	}
	msg := ""
	for attempt := 0; attempt < 3; attempt++ {
		pw, save, err := s.askSudo(ctx, t, msg)
		if err != nil {
			return "", err
		}
		ok, err := s.sudoCheck(ctx, t, pw)
		if err != nil {
			return "", err
		}
		if ok {
			s.rememberSudo(t, pw)
			if save {
				if err := s.saveSudo(ctx, t, pw); err != nil {
					s.log.Warn("could not save the sudo password", "session", t.id, "err", err)
				}
			}
			return pw, nil
		}
		msg = "Sorry, try again."
	}
	return "", errPermission("sudo authentication failed on " + t.label())
}

// sudoFatal recognizes sudo failures a password cannot fix.
func sudoFatal(res *result, t *target) error {
	m := strings.ToLower(res.errText())
	switch {
	case res.Code == 127 || strings.Contains(m, "not found") || strings.Contains(m, "no such file"):
		return httpx.NewError(http.StatusUnprocessableEntity, "sudo_unavailable", "sudo is not installed on "+t.label())
	case strings.Contains(m, "not in the sudoers") || strings.Contains(m, "is not allowed to") || strings.Contains(m, "may not run sudo"):
		return errPermission("the account is not allowed to use sudo on " + t.label())
	case strings.Contains(m, "must have a tty") || strings.Contains(m, "requiretty"):
		return httpx.NewError(http.StatusUnprocessableEntity, "sudo_unavailable", "sudo on "+t.label()+" requires a terminal (Defaults requiretty)")
	}
	return nil
}

// sudoCheck validates a password with `sudo -S -v`.
func (s *Service) sudoCheck(ctx context.Context, t *target, pw string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d := heredocDelimiter()
	res, err := s.run(cctx, t, command{sh: "exec env LC_ALL=C sudo -S -p '' -v <<'" + d + "'\n" + pw + "\n" + d})
	if err != nil {
		return false, err
	}
	if res.ok() {
		return true, nil
	}
	return false, sudoFatal(res, t)
}

// askSudo asks the user for the sudo password through the prompt broker.
func (s *Service) askSudo(ctx context.Context, t *target, message string) (pw string, save bool, err error) {
	if s.d == nil || s.d.Events == nil || t.user == nil {
		return "", false, errors.New("interactive prompts are not available")
	}
	p := model.Prompt{
		Kind:      model.PromptPassword,
		Title:     "sudo password for " + t.label(),
		Message:   message,
		Fields:    []model.PromptField{{Label: "Password", Echo: false}},
		AllowSave: s.canSaveSudo(t),
	}
	if t.sess != nil {
		p.SessionID = t.sess.ID
		if c := t.sess.Connection(); c != nil && c.ID != "" {
			p.ConnectionID = c.ID
		}
	}
	resp, err := s.d.Events.Prompt(ctx, t.user.ID, p)
	switch {
	case errors.Is(err, events.ErrNoInteractiveClient):
		return "", false, httpx.Conflict("sudo needs a password but no Termstead window is connected to ask for it")
	case errors.Is(err, events.ErrPromptTimeout):
		return "", false, httpx.NewError(http.StatusRequestTimeout, "timeout", "no answer to the sudo password prompt")
	case err != nil:
		return "", false, err
	}
	if !resp.Accept || len(resp.Values) == 0 || resp.Values[0] == "" {
		return "", false, httpx.NewError(http.StatusForbidden, "sudo_cancelled", "the sudo password prompt was cancelled")
	}
	pw = resp.Values[0]
	if strings.ContainsAny(pw, "\r\n\x00") {
		return "", false, httpx.BadRequest("invalid password")
	}
	return pw, resp.Save, nil
}

// rememberSudo keeps a working sudo password in the session's memory (never persisted by this call).
func (s *Service) rememberSudo(t *target, pw string) {
	if t.sess != nil {
		t.sess.RememberSecret(model.SecretSudoPassword, pw)
	}
}

// canSaveSudo reports whether a prompted sudo password may be saved: only into a saved connection the user owns, with
// the vault unlocked.
func (s *Service) canSaveSudo(t *target) bool {
	if t.sess == nil || t.user == nil || s.d == nil || s.d.Vault == nil || s.d.Store == nil || s.d.Vault.Locked() {
		return false
	}
	c := t.sess.Connection()
	return c != nil && c.ID != "" && c.OwnerID == t.user.ID
}

// saveSudo stores the sudo password as the connection's sudoPassword secret (vault-encrypted), audited.
func (s *Service) saveSudo(ctx context.Context, t *target, pw string) error {
	if !s.canSaveSudo(t) {
		return errors.New("the sudo password cannot be saved for this session")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	conn := t.sess.Connection()
	c, err := s.d.Store.Connections.Get(ctx, conn.ID)
	if err != nil {
		return err
	}
	if c.OwnerID != t.user.ID {
		return errors.New("only the owner can save secrets of this connection")
	}
	secrets, err := s.d.Vault.OpenJSON(c.SecretsEnc)
	if err != nil {
		return err
	}
	secrets[model.SecretSudoPassword] = pw
	enc, err := s.d.Vault.SealJSON(secrets)
	if err != nil {
		return err
	}
	c.SecretsEnc = enc
	keys := make([]string, 0, len(secrets))
	for k, v := range secrets {
		if v != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	c.SecretKeys = keys
	if err := s.d.Store.Connections.Update(ctx, c); err != nil {
		return err
	}
	if s.d.Audit != nil {
		s.d.Audit.LogUser(ctx, t.user, "connection.secret.save", c.ID, map[string]any{"key": model.SecretSudoPassword})
	}
	return nil
}

// watchScript runs a long-running command line in the background and kills it when the channel's stdin reaches EOF (a
// non-PTY exec gets no SIGHUP when the channel closes): the script's exit status is the command's. The whole script is
// one brace group, so the shell has parsed all of it before the reader starts consuming stdin. Background lists get
// /dev/null as stdin in shells without job control, so the channel is handed to the reader on fd 3.
func watchScript(cmdline, doc string) string {
	return "{ exec 3<&0\n" + cmdline + " 3<&- &" + doc + "\nnx_p=$!\n( cat <&3 >/dev/null 2>&1; kill $nx_p 2>/dev/null ) &\n" +
		"nx_c=$!\nexec 3<&-\nwait $nx_p\nnx_rc=$?\nkill $nx_c 2>/dev/null\nexit $nx_rc\n}"
}
