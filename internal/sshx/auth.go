package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kayrus/putty"
	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/events"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// Authentication orchestration (SSH-1/2/3/6/8/9): the ssh.ClientConfig.AuthCallback picks the next method from the
// server's allowed list in a fixed preference order, supplying stored credentials first and asking the user (events
// prompt broker) only for what is missing. Answers checked with "save" are written into the owner's connection
// secrets once they proved correct; answers are also remembered in the runtime session for reconnects.

const (
	maxPasswordAttempts = 3
	maxKIRounds         = 3
	maxAgentKeys        = 6
)

var errPromptCanceled = errors.New("authentication canceled by the user")

// authError is the final "permission denied" error.
type authError struct {
	allowed []string
	cause   error
}

func (e *authError) Error() string {
	msg := "authentication failed: permission denied (" + strings.Join(e.allowed, ",") + ")"
	if e.cause != nil {
		msg += ": " + e.cause.Error()
	}
	return msg
}

func (e *authError) Unwrap() error { return e.cause }

type pendingSecret struct{ key, value string }

type authFlow struct {
	p        *Pool
	ctx      context.Context
	user     *model.User
	conn     *model.Connection
	secrets  map[string]string
	session  *term.Session
	dc       *deadlineConn
	label    string // non-empty for jump hops
	username string

	pkDone         bool
	kiRounds       int
	pwAttempts     int
	storedPwUsed   bool
	storedPwKIUsed bool
	canceled       bool
	lastErr        error

	agentCloser io.Closer

	inflight    *pendingSecret    // "save" requested for the attempt in flight
	confirmed   []pendingSecret   // answers proven correct and to be saved
	prevPartial int               // partial-success count at the previous callback
	answered    map[string]string // prompt answers to remember for reconnects
}

// order returns the preferred method sequence for the connection's authMethod.
func (a *authFlow) order() []string {
	switch a.conn.AuthMethod {
	case model.AuthPassword:
		return []string{"password", "keyboard-interactive"}
	case model.AuthKey, model.AuthAgent:
		return []string{"publickey"}
	case model.AuthKeyboardInteractive:
		return []string{"keyboard-interactive"}
	case model.AuthNone:
		return nil
	}
	if a.secrets[model.SecretPassword] != "" {
		return []string{"publickey", "password", "keyboard-interactive"}
	}
	return []string{"publickey", "keyboard-interactive", "password"}
}

// next is the ssh.ClientConfig.AuthCallback.
func (a *authFlow) next(ac *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
	if a.inflight != nil {
		if len(ac.PartialSuccessMethods) > a.prevPartial {
			a.confirmed = append(a.confirmed, *a.inflight)
		}
		a.inflight = nil
	}
	a.prevPartial = len(ac.PartialSuccessMethods)
	if a.canceled {
		return nil, errPromptCanceled
	}
	for _, m := range a.order() {
		if !slices.Contains(ac.AllowedMethods, m) {
			continue
		}
		switch m {
		case "publickey":
			if a.pkDone {
				continue
			}
			a.pkDone = true
			signers, err := a.signers()
			if err != nil {
				a.lastErr = err
			}
			if len(signers) == 0 {
				continue
			}
			return ssh.PublicKeys(signers...), nil
		case "keyboard-interactive":
			if a.kiRounds >= maxKIRounds {
				continue
			}
			a.kiRounds++
			return ssh.KeyboardInteractive(a.challenge), nil
		case "password":
			if a.pwAttempts >= maxPasswordAttempts {
				continue
			}
			pw, err := a.password()
			if err != nil {
				if a.canceled {
					return nil, errPromptCanceled
				}
				a.lastErr = err
				a.pwAttempts = maxPasswordAttempts
				continue
			}
			a.pwAttempts++
			return ssh.Password(pw), nil
		}
	}
	if a.canceled {
		return nil, errPromptCanceled
	}
	return nil, &authError{allowed: ac.AllowedMethods, cause: a.lastErr}
}

// onSuccess persists confirmed "save" answers and remembers prompt answers in the runtime session.
func (a *authFlow) onSuccess() {
	if a.inflight != nil {
		a.confirmed = append(a.confirmed, *a.inflight)
		a.inflight = nil
	}
	for _, ps := range a.confirmed {
		if err := a.p.saveConnectionSecret(a.ctx, a.user, a.conn, ps.key, ps.value); err != nil {
			a.p.log.Warn("ssh: saving secret failed", "connection", a.conn.ID, "key", ps.key, "err", err)
			if a.session != nil {
				a.session.Notice("Could not save the " + ps.key + ": " + err.Error())
			}
		}
	}
	if a.session != nil {
		for k, v := range a.answered {
			a.session.RememberSecret(a.rememberKey(k), v)
		}
	}
}

// rememberKey namespaces remembered answers of jump hops so they do not shadow the target's secrets.
func (a *authFlow) rememberKey(k string) string {
	if a.label == "" {
		return k
	}
	return hopSecretKey(a.conn, k)
}

func hopSecretKey(hop *model.Connection, k string) string {
	id := hop.ID
	if id == "" {
		id = strings.ToLower(hop.Username + "@" + hop.Host + ":" + fmt.Sprint(hop.Port))
	}
	return "hop:" + id + ":" + k
}

func (a *authFlow) answer(k, v string) {
	if a.answered == nil {
		a.answered = map[string]string{}
	}
	a.answered[k] = v
}

func (a *authFlow) closeAgent() {
	if a.agentCloser != nil {
		_ = a.agentCloser.Close()
		a.agentCloser = nil
	}
}

// canSave reports whether prompt answers may be saved: only into a saved connection owned by the user.
func (a *authFlow) canSave() bool {
	return a.conn != nil && a.conn.ID != "" && a.user != nil && a.conn.OwnerID == a.user.ID && a.p.d != nil &&
		a.p.d.Vault != nil && !a.p.d.Vault.Locked()
}

func (a *authFlow) who() string {
	u := a.username
	if u == "" {
		u = a.conn.Username
	}
	if u == "" {
		return a.conn.Host
	}
	return u + "@" + a.conn.Host
}

// prompt relays a question to the user's browser windows, pausing the handshake deadline meanwhile.
func (a *authFlow) prompt(p model.Prompt) (model.PromptResponse, error) {
	if a.p.d == nil || a.p.d.Events == nil || a.user == nil {
		return model.PromptResponse{}, errors.New("interactive prompts are not available")
	}
	if a.session != nil {
		p.SessionID = a.session.ID
		a.session.SetStatus(model.StateAuthenticating, "Waiting for "+p.Kind+" input")
	}
	if a.conn != nil && a.conn.ID != "" {
		p.ConnectionID = a.conn.ID
	}
	if a.label != "" {
		p.Title = a.label + ": " + p.Title
	}
	if a.dc != nil {
		a.dc.pause()
		defer a.dc.resume()
	}
	resp, err := a.p.d.Events.Prompt(a.ctx, a.user.ID, p)
	if a.session != nil {
		a.session.SetStatus(model.StateConnecting, "Authenticating")
	}
	switch {
	case err == nil:
		return resp, nil
	case errors.Is(err, events.ErrNoInteractiveClient):
		return resp, fmt.Errorf("%s input is required but no NexTerm window is connected", p.Kind)
	case errors.Is(err, events.ErrPromptTimeout):
		return resp, fmt.Errorf("no answer to the %s prompt", p.Kind)
	}
	return resp, err
}

func (a *authFlow) askUsername() (string, error) {
	resp, err := a.prompt(model.Prompt{
		Kind:    model.PromptKeyboardInteractive,
		Title:   "Login as",
		Message: "User name for " + a.conn.Host,
		Fields:  []model.PromptField{{Label: "Username", Echo: true}},
	})
	if err != nil {
		return "", err
	}
	if !resp.Accept || len(resp.Values) == 0 || strings.TrimSpace(resp.Values[0]) == "" {
		return "", term.Permanent(errPromptCanceled)
	}
	u := strings.TrimSpace(resp.Values[0])
	if len(u) > 256 || strings.ContainsFunc(u, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", term.Permanent(errors.New("invalid user name"))
	}
	a.answer("username", u)
	return u, nil
}

func (a *authFlow) password() (string, error) {
	if !a.storedPwUsed {
		a.storedPwUsed = true
		if pw := a.secrets[model.SecretPassword]; pw != "" {
			return pw, nil
		}
	}
	msg := ""
	if a.pwAttempts > 0 {
		msg = "Permission denied, please try again."
	}
	resp, err := a.prompt(model.Prompt{
		Kind:      model.PromptPassword,
		Title:     "Password for " + a.who(),
		Message:   msg,
		Fields:    []model.PromptField{{Label: "Password", Echo: false}},
		AllowSave: a.canSave(),
	})
	if err != nil {
		return "", err
	}
	if !resp.Accept || len(resp.Values) == 0 {
		a.canceled = true
		return "", errPromptCanceled
	}
	pw := resp.Values[0]
	a.answer(model.SecretPassword, pw)
	if resp.Save && a.canSave() {
		a.inflight = &pendingSecret{key: model.SecretPassword, value: pw}
	}
	return pw, nil
}

// challenge answers keyboard-interactive rounds (SSH-6): info-only rounds are shown in the terminal, a lone
// password question is answered with the stored password once, everything else is asked in the browser.
func (a *authFlow) challenge(name, instruction string, questions []string, echos []bool) ([]string, error) {
	if len(questions) != len(echos) {
		return nil, errors.New("malformed keyboard-interactive request")
	}
	if len(questions) == 0 {
		if text := strings.TrimSpace(strings.TrimSpace(name) + "\n" + strings.TrimSpace(instruction)); text != "" && a.session != nil {
			a.session.Banner(text)
		}
		return []string{}, nil
	}
	pwLike := len(questions) == 1 && !echos[0] && isPasswordPrompt(questions[0])
	if pwLike && !a.storedPwKIUsed {
		a.storedPwKIUsed = true
		if pw := a.secrets[model.SecretPassword]; pw != "" {
			return []string{pw}, nil
		}
	}
	fields := make([]model.PromptField, len(questions))
	for i, q := range questions {
		fields[i] = model.PromptField{Label: cleanText(q, 256), Echo: echos[i]}
	}
	title := cleanText(name, 128)
	if title == "" {
		title = "Authentication for " + a.who()
	}
	allowSave := pwLike && a.canSave()
	resp, err := a.prompt(model.Prompt{
		Kind:      model.PromptKeyboardInteractive,
		Title:     title,
		Message:   cleanText(instruction, 2048),
		Fields:    fields,
		AllowSave: allowSave,
	})
	if err != nil {
		return nil, err
	}
	if !resp.Accept {
		a.canceled = true
		return nil, errPromptCanceled
	}
	answers := make([]string, len(questions))
	copy(answers, resp.Values)
	if pwLike {
		a.answer(model.SecretPassword, answers[0])
		if resp.Save && allowSave {
			a.inflight = &pendingSecret{key: model.SecretPassword, value: answers[0]}
		}
	}
	return answers, nil
}

func isPasswordPrompt(q string) bool {
	q = strings.ToLower(q)
	for _, w := range []string{"password", "passwort", "mot de passe", "contraseña", "senha", "пароль", "parola"} {
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

// cleanText strips control characters from server-provided text shown in prompts.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return strings.ToValidUTF8(s, "")
}

// ---- public keys --------------------------------------------------------------------------------------------------

// signers returns the public-key identities to offer: the stored key (and its certificate) first, then host agent
// keys when allowed.
func (a *authFlow) signers() ([]ssh.Signer, error) {
	var out []ssh.Signer
	var firstErr error
	method := a.conn.AuthMethod
	if a.conn.KeyID != "" && method != model.AuthAgent {
		s, err := a.storedKeySigners(a.conn.KeyID)
		if err != nil {
			firstErr = err
		}
		out = append(out, s...)
	}
	if a.useAgent() {
		ag, closer, err := a.dialAgent() // host agent (+ running built-in agent, agent_builtin.go)
		if err == nil {
			a.agentCloser = closer
			if ss, err := ag.Signers(); err == nil {
				out = append(out, ss[:min(len(ss), max(1, maxAgentKeys-len(out)))]...)
			} else if firstErr == nil {
				firstErr = fmt.Errorf("ssh agent: %w", err)
			}
		} else if method == model.AuthAgent && firstErr == nil {
			firstErr = err
		}
	}
	if len(out) == 0 && firstErr == nil && (method == model.AuthKey) {
		firstErr = errors.New("no private key is configured for this connection")
	}
	return out, firstErr
}

func (a *authFlow) useAgent() bool {
	if !a.p.allowLocalExec(a.user) {
		return false
	}
	switch a.conn.AuthMethod {
	case model.AuthAgent:
		return true
	case model.AuthAuto:
		def := a.p.d != nil && a.p.d.Cfg != nil && a.p.d.Cfg.IsDesktop()
		return a.conn.Options.Bool("useAgent", def)
	}
	return false
}

// storedKeySigners loads a stored key (vault) and its certificate. Encrypted keys without a known passphrase get a
// lazy signer that asks for the passphrase only if the server accepts the key (SSH-3).
func (a *authFlow) storedKeySigners(keyID string) ([]ssh.Signer, error) {
	if a.p.d == nil || a.p.d.Store == nil {
		return nil, errors.New("stored keys are not available")
	}
	pemBytes, pass, err := a.p.d.KeyMaterial(a.ctx, a.user, keyID)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return nil, fmt.Errorf("the vault is locked; unlock it to use the stored key: %w", err)
		}
		return nil, fmt.Errorf("stored key: %w", err)
	}
	meta, _ := a.p.d.Store.Keys.Get(a.ctx, keyID)
	keyName := keyID
	if meta != nil && meta.Name != "" {
		keyName = meta.Name
	}
	if pass == "" {
		pass = a.secrets[model.SecretPassphrase]
	}
	var signer ssh.Signer
	s, err := ParsePrivateKey(pemBytes, pass)
	var np *NeedsPassphraseError
	switch {
	case err == nil:
		signer = s
	case errors.As(err, &np):
		pub := np.PublicKey
		if pub == nil && meta != nil && meta.PublicKey != "" {
			pub, _, _, _, _ = ssh.ParseAuthorizedKey([]byte(meta.PublicKey))
		}
		if pub == nil {
			return nil, fmt.Errorf("key %q is encrypted and its public key is unknown", keyName)
		}
		signer = &lazySigner{a: a, pub: pub, pem: pemBytes, name: keyName}
	default:
		return nil, fmt.Errorf("key %q: %w", keyName, err)
	}
	out := []ssh.Signer{signer}
	if meta != nil && strings.TrimSpace(meta.Certificate) != "" {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(meta.Certificate)); err == nil {
			if cert, ok := pk.(*ssh.Certificate); ok {
				if cs, err := ssh.NewCertSigner(cert, signer); err == nil {
					out = []ssh.Signer{cs, signer}
				}
			}
		}
	}
	return out, nil
}

// NeedsPassphraseError reports an encrypted private key without (or with a wrong) passphrase. PublicKey is set
// when it can be derived without decrypting.
type NeedsPassphraseError struct {
	PublicKey ssh.PublicKey
	Wrong     bool
}

func (e *NeedsPassphraseError) Error() string {
	if e.Wrong {
		return "incorrect passphrase"
	}
	return "the private key is encrypted and needs a passphrase"
}

// ParsePrivateKey parses an OpenSSH / PEM (PKCS#1, PKCS#8, SEC1, legacy encrypted PEM) or PuTTY PPK v2/v3 private
// key, decrypting it with passphrase when needed.
func ParsePrivateKey(data []byte, passphrase string) (ssh.Signer, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("PuTTY-User-Key-File-")) {
		k, err := putty.New(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid PuTTY key: %w", err)
		}
		var pub ssh.PublicKey
		if len(k.PublicKey) > 0 {
			pub, _ = ssh.ParsePublicKey(k.PublicKey)
		}
		if k.Encryption != "" && k.Encryption != "none" && passphrase == "" {
			return nil, &NeedsPassphraseError{PublicKey: pub}
		}
		raw, err := k.ParseRawPrivateKey([]byte(passphrase))
		if err != nil {
			if k.Encryption != "" && k.Encryption != "none" {
				return nil, &NeedsPassphraseError{PublicKey: pub, Wrong: true}
			}
			return nil, err
		}
		if p, ok := raw.(*ed25519.PrivateKey); ok {
			raw = *p
		}
		return ssh.NewSignerFromKey(raw)
	}
	s, err := ssh.ParsePrivateKey(trimmed)
	if err == nil {
		return s, nil
	}
	var pm *ssh.PassphraseMissingError
	if !errors.As(err, &pm) {
		return nil, err
	}
	if passphrase == "" {
		return nil, &NeedsPassphraseError{PublicKey: pm.PublicKey}
	}
	s, err = ssh.ParsePrivateKeyWithPassphrase(trimmed, []byte(passphrase))
	if err != nil {
		return nil, &NeedsPassphraseError{PublicKey: pm.PublicKey, Wrong: true}
	}
	return s, nil
}

// parseRawPrivateKey is ParsePrivateKey returning the raw crypto key (for the forwarding keyring).
func parseRawPrivateKey(data []byte, passphrase string) (any, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("PuTTY-User-Key-File-")) {
		k, err := putty.New(trimmed)
		if err != nil {
			return nil, err
		}
		if k.Encryption != "" && k.Encryption != "none" && passphrase == "" {
			return nil, &NeedsPassphraseError{}
		}
		raw, err := k.ParseRawPrivateKey([]byte(passphrase))
		if err != nil {
			return nil, err
		}
		if p, ok := raw.(*ed25519.PrivateKey); ok {
			raw = *p
		}
		return raw, nil
	}
	raw, err := ssh.ParseRawPrivateKey(trimmed)
	if err == nil {
		return raw, nil
	}
	var pm *ssh.PassphraseMissingError
	if errors.As(err, &pm) && passphrase != "" {
		return ssh.ParseRawPrivateKeyWithPassphrase(trimmed, []byte(passphrase))
	}
	return nil, err
}

// lazySigner asks for the passphrase of an encrypted key when a signature is actually needed.
type lazySigner struct {
	a    *authFlow
	pub  ssh.PublicKey
	pem  []byte
	name string

	mu     sync.Mutex
	signer ssh.Signer
	err    error
}

func (l *lazySigner) PublicKey() ssh.PublicKey { return l.pub }

func (l *lazySigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	return l.SignWithAlgorithm(rand, data, "")
}

func (l *lazySigner) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	s, err := l.unlock()
	if err != nil {
		return nil, err
	}
	if as, ok := s.(ssh.AlgorithmSigner); ok && algorithm != "" {
		return as.SignWithAlgorithm(rand, data, algorithm)
	}
	return s.Sign(rand, data)
}

// Algorithms lists the signature algorithms of the key type (MultiAlgorithmSigner), so RSA keys use SHA-2.
func (l *lazySigner) Algorithms() []string {
	if l.pub.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA}
	}
	return []string{l.pub.Type()}
}

func (l *lazySigner) unlock() (ssh.Signer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.signer != nil || l.err != nil {
		return l.signer, l.err
	}
	a := l.a
	msg := fmt.Sprintf("Key %q (%s) is encrypted.", l.name, ssh.FingerprintSHA256(l.pub))
	for attempt := 0; attempt < maxPasswordAttempts; attempt++ {
		if attempt > 0 {
			msg = "Incorrect passphrase, please try again."
		}
		resp, err := a.prompt(model.Prompt{
			Kind:      model.PromptPassphrase,
			Title:     "Passphrase for key " + l.name,
			Message:   msg,
			Fields:    []model.PromptField{{Label: "Passphrase", Echo: false}},
			AllowSave: a.canSave(),
		})
		if err != nil {
			l.err = err
			return nil, err
		}
		if !resp.Accept || len(resp.Values) == 0 {
			a.canceled = true
			l.err = errPromptCanceled
			return nil, l.err
		}
		s, err := ParsePrivateKey(l.pem, resp.Values[0])
		if err != nil {
			var np *NeedsPassphraseError
			if errors.As(err, &np) {
				continue
			}
			l.err = err
			return nil, err
		}
		l.signer = s
		a.answer(model.SecretPassphrase, resp.Values[0])
		if resp.Save && a.canSave() {
			// A successful decryption proves the passphrase; save it regardless of the server's verdict.
			a.confirmed = append(a.confirmed, pendingSecret{key: model.SecretPassphrase, value: resp.Values[0]})
		}
		return s, nil
	}
	l.err = errors.New("incorrect passphrase")
	return nil, l.err
}

// ---- saving secrets -----------------------------------------------------------------------------------------------

// saveConnectionSecret stores a prompt answer into the owner's saved connection (vault-encrypted).
func (p *Pool) saveConnectionSecret(ctx context.Context, user *model.User, conn *model.Connection, key, value string) error {
	if p.d == nil || p.d.Store == nil || p.d.Vault == nil || conn == nil || conn.ID == "" || user == nil {
		return errors.New("secrets cannot be saved for this connection")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	c, err := p.d.Store.Connections.Get(ctx, conn.ID)
	if err != nil {
		return err
	}
	if c.OwnerID != user.ID {
		return errors.New("only the owner can save secrets of this connection")
	}
	secrets, err := p.d.Vault.OpenJSON(c.SecretsEnc)
	if err != nil {
		return err
	}
	secrets[key] = value
	enc, err := p.d.Vault.SealJSON(secrets)
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
	if err := p.d.Store.Connections.Update(ctx, c); err != nil {
		return err
	}
	p.audit(ctx, user, "connection.secret.save", c.ID, map[string]any{"key": key})
	return nil
}

func (p *Pool) audit(ctx context.Context, user *model.User, action, target string, details any) {
	if p.d == nil || p.d.Audit == nil {
		return
	}
	p.d.Audit.LogUser(context.WithoutCancel(ctx), user, action, target, details)
}
