package vnc

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/model"
)

// Secret keys used by VNC connections: the VNC password (SPEC §5.3), falling back to the generic password (quick
// connect "vnc://:pw@host" puts it there). A user name answered at a prompt is remembered as "username" (like sshx).
const (
	rememberedUser = "username"
)

// viewerCreds supplies credentials for one viewer's connection attempts: stored secrets first, then prompts.
type viewerCreds struct {
	v        *viewer
	conn     *model.Connection
	username string
	password string

	fromPrompt   bool // the current password was typed by the user
	userPrompted bool // the current username was typed by the user
	save         bool // the user asked to remember the typed credentials
	rejected     bool
	reason       string
	storedFailed bool // the stored/remembered password was rejected at least once
	prompts      int  // number of prompts shown (lets connect() spot connections lost during a prompt)
}

func newViewerCreds(v *viewer, conn *model.Connection, secrets map[string]string) *viewerCreds {
	c := &viewerCreds{v: v, conn: conn, username: conn.Username}
	if c.username == "" {
		c.username = secrets[rememberedUser]
	}
	for _, k := range []string{model.SecretVNCPassword, model.SecretPassword} {
		if pw := secrets[k]; pw != "" {
			c.password = pw
			break
		}
	}
	return c
}

func (c *viewerCreds) available() (hasUser, hasPassword bool) {
	return c.username != "", c.password != "" && !c.rejected
}

// canSave reports whether typed credentials may be saved: only into a saved connection the viewing user owns, while
// the vault is unlocked.
func (c *viewerCreds) canSave() bool {
	d := c.v.m.d
	return c.v.owner && c.conn.ID != "" && c.conn.OwnerID == c.v.user.ID && d.Vault != nil && !d.Vault.Locked() &&
		d.Store != nil
}

// reject records a failed authentication; false when no further attempt makes sense.
func (c *viewerCreds) reject(reason string) bool {
	if !c.fromPrompt {
		c.storedFailed = true
	}
	c.rejected, c.reason = true, reason
	return true
}

func (c *viewerCreds) get(ctx context.Context, req credRequest) (string, string, error) {
	needPass := c.password == "" || c.rejected
	needUser := req.needUser && (c.username == "" || (c.rejected && c.userPrompted))
	if !needPass && !needUser {
		return c.username, c.password, nil
	}
	target := hostPort(c.conn.Host, portOf(c.conn))
	var msg []string
	switch {
	case c.rejected && c.storedFailed && !c.fromPrompt:
		msg = append(msg, "The saved password was rejected by the server.")
	case c.rejected:
		msg = append(msg, "Authentication failed. Please try again.")
	}
	if c.rejected && c.reason != "" {
		msg = append(msg, "Server: "+c.reason)
	}
	msg = append(msg, req.scheme+" on "+target)
	var fields []model.PromptField
	if req.needUser {
		fields = append(fields, model.PromptField{Label: "Username", Echo: true, Value: c.username})
	}
	fields = append(fields, model.PromptField{Label: "Password", Echo: false})
	title := "VNC password"
	if req.needUser {
		title = "VNC login"
	}
	c.prompts++
	resp, err := c.v.prompt(ctx, model.Prompt{
		Kind:      model.PromptPassword,
		Title:     title,
		Message:   strings.Join(msg, "\n"),
		Fields:    fields,
		AllowSave: c.canSave(),
	}, "Waiting for the "+strings.ToLower(title))
	if err != nil {
		return "", "", err
	}
	if !resp.Accept || len(resp.Values) < len(fields) {
		return "", "", errCanceled
	}
	if req.needUser {
		u := strings.TrimSpace(resp.Values[0])
		if u == "" || len(u) > 256 || strings.ContainsFunc(u, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return "", "", errors.New("invalid user name")
		}
		if u != c.username {
			c.userPrompted = true
		}
		c.username = u
	}
	pw := resp.Values[len(resp.Values)-1]
	if len(pw) > 1024 {
		return "", "", errors.New("the password is too long")
	}
	c.password, c.fromPrompt, c.rejected, c.reason = pw, true, false, ""
	c.save = resp.Save && c.canSave()
	return c.username, c.password, nil
}

// succeeded remembers typed credentials in the session (never persisted) and saves them when requested.
func (c *viewerCreds) succeeded(ctx context.Context, res *handshakeResult) {
	if !res.usedPassword || !c.fromPrompt || !c.v.owner {
		return
	}
	c.v.s.RememberSecret(model.SecretVNCPassword, c.password)
	if c.userPrompted && c.conn.Username == "" {
		c.v.s.RememberSecret(rememberedUser, c.username)
	}
	if !c.save {
		return
	}
	user := ""
	if c.userPrompted && c.conn.Username == "" {
		user = c.username
	}
	if err := c.v.m.saveCredentials(ctx, c.v.user, c.conn.ID, user, c.password); err != nil {
		c.v.m.log.Warn("vnc: saving the password failed", "connection", c.conn.ID, "err", err)
	}
}

// saveCredentials stores the VNC password (and a user name typed for an empty username field) in the owner's saved
// connection, vault-encrypted.
func (m *Module) saveCredentials(ctx context.Context, user *model.User, connID, username, password string) error {
	if m.d.Store == nil || m.d.Vault == nil || connID == "" || user == nil {
		return errors.New("credentials cannot be saved for this connection")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	c, err := m.d.Store.Connections.Get(ctx, connID)
	if err != nil {
		return err
	}
	if c.OwnerID != user.ID {
		return errors.New("only the owner can save credentials of this connection")
	}
	secrets, err := m.d.Vault.OpenJSON(c.SecretsEnc)
	if err != nil {
		return err
	}
	secrets[model.SecretVNCPassword] = password
	enc, err := m.d.Vault.SealJSON(secrets)
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
	if username != "" && c.Username == "" {
		c.Username = username
	}
	if err := m.d.Store.Connections.Update(ctx, c); err != nil {
		return err
	}
	m.audit(ctx, user, "connection.secret.save", c.ID, map[string]any{"key": model.SecretVNCPassword})
	return nil
}

// ---- certificate trust (VeNCrypt X509*) ---------------------------------------------------------------------------

var errCertRejected = errors.New("the VNC server certificate was not accepted")

// viewerTrust verifies X.509 certificates for a viewer: system roots, then the certificate accepted earlier in this
// session, then the saved fingerprint, else it asks (TOFU). Replacing a saved fingerprint that changed affects every
// user, so only administrators may save it in server mode.
type viewerTrust struct{ v *viewer }

func (t *viewerTrust) verify(ctx context.Context, host string, port int, certs []*x509.Certificate) (string, error) {
	m := t.v.m
	leaf := certs[0]
	fp := certFingerprint(leaf)
	hp := hostPort(strings.ToLower(host), port)
	if systemTrusted(host, certs) {
		return "system", nil
	}
	if m.acceptedCert(t.v.s.ID, hp) == fp {
		return "accepted", nil
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	saved, err := m.certs.find(lctx, host, port)
	cancel()
	if err != nil {
		return "", fmt.Errorf("trusted certificates lookup failed: %w", err)
	}
	if saved != nil && saved.Fingerprint == fp {
		return "saved", nil
	}
	info := &model.HostKeyInfo{Host: strings.ToLower(host), Port: port, KeyType: certKeyType(leaf), Fingerprint: fp,
		FingerprintMD5: certFingerprintMD5(leaf), Status: model.HostKeyUnknown}
	p := model.Prompt{
		Kind:      model.PromptHostKey,
		Title:     "Untrusted VNC server certificate",
		Message:   fmt.Sprintf("The TLS certificate of %s is not signed by a trusted authority.\n%s", hp, describeCert(leaf)),
		HostKey:   info,
		AllowSave: true,
	}
	mismatch := saved != nil
	if mismatch {
		info.Status, info.KnownFingerprint = model.HostKeyMismatch, saved.Fingerprint
		p.Title = "WARNING: VNC SERVER CERTIFICATE HAS CHANGED"
		p.Message = fmt.Sprintf("The TLS certificate of %s does not match the trusted one. Someone could be intercepting "+
			"the connection (man-in-the-middle attack), or the certificate was legitimately replaced. Only continue if you "+
			"know why it changed.\nTrusted: %s\nOffered: %s\n%s", hp, saved.Fingerprint, fp, describeCert(leaf))
		p.AllowSave = m.mayChangeGlobalTrust(t.v.user)
	}
	resp, err := t.v.prompt(ctx, p, "Waiting for certificate confirmation")
	if err != nil {
		return "", fmt.Errorf("the certificate of %s could not be confirmed: %w", hp, err)
	}
	if !resp.Accept {
		return "", errCertRejected
	}
	m.rememberCert(t.v.s.ID, hp, fp)
	if mismatch {
		m.audit(ctx, t.v.user, "vnc.cert.mismatch_accepted", hp, map[string]any{"fingerprint": fp,
			"knownFingerprint": saved.Fingerprint, "saved": resp.Save && p.AllowSave})
	}
	if !resp.Save || !p.AllowSave {
		return "accepted", nil
	}
	var notAfter *time.Time
	if !leaf.NotAfter.IsZero() {
		na := leaf.NotAfter.UTC()
		notAfter = &na
	}
	rec := &TrustedCert{ID: model.NewID(), Host: strings.ToLower(host), Port: port, Fingerprint: fp,
		Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(), NotAfter: notAfter,
		Comment:   fmt.Sprintf("accepted by %s on %s", t.v.user.Username, time.Now().UTC().Format("2006-01-02")),
		CreatedAt: time.Now().UTC()}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer scancel()
	if err := m.certs.save(sctx, rec); err != nil {
		m.log.Warn("vnc: saving the trusted certificate failed", "target", hp, "err", err)
		return "accepted", nil
	}
	action := "vnc.cert.trust"
	if mismatch {
		action = "vnc.cert.replace"
	}
	m.audit(ctx, t.v.user, action, hp, map[string]any{"fingerprint": fp, "subject": rec.Subject})
	return "accepted-saved", nil
}
