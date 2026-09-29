package rdp

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/nexterm/nexterm/internal/events"
	"github.com/nexterm/nexterm/internal/model"
)

// Server certificate verification for the RDCleanPath relay. RDP servers usually present self-signed certificates,
// so the relay accepts a certificate that
//   - chains to a system root and matches the host name (like mstsc), or
//   - is pinned in the global known_hosts table (key type "rdp-tls", fingerprint of the DER certificate), or
//   - the user accepts interactively through the prompt broker (trust on first use; "Accept & save" pins it).
//
// options.ignoreCert skips the checks. A changed certificate raises a loud mismatch prompt; replacing the pinned
// certificate (a global change) is allowed in desktop mode or for administrators only, as for SSH host keys.

// rdpKeyType is the known_hosts key type of pinned RDP server certificates.
const rdpKeyType = "rdp-tls"

// errCertRejected is returned when the user (or policy) rejected the server certificate.
var errCertRejected = errors.New("certificate rejected")

// certRejectedError carries the user-facing reason of a rejected certificate.
type certRejectedError struct{ msg string }

func (e *certRejectedError) Error() string { return e.msg }
func (e *certRejectedError) Unwrap() error { return errCertRejected }

// certFingerprint is the SHA-256 fingerprint of a DER certificate, formatted like SSH host key fingerprints.
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func certFingerprintMD5(der []byte) string {
	sum := md5.Sum(der)
	return "MD5:" + colonHex(sum[:])
}

// certThumbprint is the SHA-1 thumbprint Windows shows in its certificate dialogs.
func certThumbprint(der []byte) string {
	sum := sha1.Sum(der)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func colonHex(b []byte) string {
	var sb strings.Builder
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(hex.EncodeToString([]byte{c}))
	}
	return sb.String()
}

// certTrust verifies the certificate of one relay connection. It survives a reconnect of the relay (the user is
// not asked twice for a certificate accepted a moment ago).
type certTrust struct {
	h          *handler
	ctx        context.Context
	user       *model.User
	sessionID  string
	connID     string
	host       string // lower-case, as stored in known_hosts
	port       int
	serverName string
	ignore     bool

	mu       sync.Mutex
	accepted string // fingerprint accepted during this relay
	prompted bool   // an interactive prompt was answered positively
}

// verify is the tls.Config.VerifyConnection callback.
func (t *certTrust) verify(cs tls.ConnectionState) error {
	certs := cs.PeerCertificates
	if len(certs) == 0 {
		return &certRejectedError{msg: "the server presented no TLS certificate"}
	}
	leaf := certs[0]
	fp := certFingerprint(leaf.Raw)

	t.mu.Lock()
	accepted := t.accepted
	t.mu.Unlock()
	if accepted == fp || t.ignore {
		return nil
	}
	sysErr := t.verifySystem(certs)
	if sysErr == nil {
		return nil
	}
	known, err := t.knownFingerprints()
	if err != nil {
		return fmt.Errorf("known hosts lookup failed: %w", err)
	}
	for _, k := range known {
		if k.Fingerprint == fp {
			t.accept(fp, false)
			return nil
		}
	}
	return t.ask(leaf, fp, known, sysErr)
}

func (t *certTrust) verifySystem(certs []*x509.Certificate) error {
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{DNSName: t.serverName, Intermediates: inter})
	return err
}

func (t *certTrust) knownFingerprints() ([]*model.KnownHost, error) {
	if t.h.d == nil || t.h.d.Store == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), 10*time.Second)
	defer cancel()
	all, err := t.h.d.Store.KnownHosts.Find(ctx, t.host, t.port)
	if err != nil {
		return nil, err
	}
	var out []*model.KnownHost
	for _, k := range all {
		if k.KeyType == rdpKeyType {
			out = append(out, k)
		}
	}
	return out, nil
}

func (t *certTrust) accept(fp string, prompted bool) {
	t.mu.Lock()
	t.accepted = fp
	if prompted {
		t.prompted = true
	}
	t.mu.Unlock()
}

// promptedAccept reports whether the certificate was accepted interactively during this relay.
func (t *certTrust) promptedAccept() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prompted
}

func (t *certTrust) target() string {
	if t.port == 3389 {
		return t.host
	}
	return net.JoinHostPort(t.host, fmt.Sprint(t.port))
}

// ask raises the TOFU (or mismatch) prompt.
func (t *certTrust) ask(leaf *x509.Certificate, fp string, known []*model.KnownHost, sysErr error) error {
	h := t.h
	if h.d == nil || h.d.Events == nil || t.user == nil {
		return &certRejectedError{msg: "the server certificate is not trusted and cannot be confirmed interactively"}
	}
	info := &model.HostKeyInfo{
		Host:           t.host,
		Port:           t.port,
		KeyType:        rdpKeyType,
		Fingerprint:    fp,
		FingerprintMD5: certFingerprintMD5(leaf.Raw),
		Status:         model.HostKeyUnknown,
	}
	mismatch := len(known) > 0
	allowSave := true
	title := "Unknown RDP server certificate"
	intro := fmt.Sprintf("The identity of the remote desktop %s cannot be verified: %s.", t.target(), certProblem(sysErr, leaf, t.serverName))
	if mismatch {
		info.Status, info.KnownFingerprint = model.HostKeyMismatch, known[0].Fingerprint
		allowSave = h.allowGlobalTrust(t.user)
		title = "WARNING: RDP SERVER CERTIFICATE HAS CHANGED"
		intro = fmt.Sprintf("The certificate of %s does not match the trusted certificate. Someone could be intercepting "+
			"the connection (man-in-the-middle attack), or the server certificate was renewed (Windows renews its "+
			"self-signed RDP certificate periodically). Only continue if you know why it changed.", t.target())
	}
	msg := intro + "\n" + describeCert(leaf)
	h.setState(t.sessionID, model.StateAuthenticating, "Waiting for the certificate to be confirmed")
	resp, err := h.d.Events.Prompt(t.ctx, t.user.ID, model.Prompt{
		Kind:         model.PromptHostKey,
		Title:        title,
		Message:      msg,
		SessionID:    t.sessionID,
		ConnectionID: t.connID,
		HostKey:      info,
		AllowSave:    allowSave,
	})
	h.setState(t.sessionID, model.StateConnecting, "Securing the connection (TLS)")
	switch {
	case errors.Is(err, events.ErrNoInteractiveClient):
		return &certRejectedError{msg: "the server certificate must be confirmed, but no NexTerm window is connected"}
	case errors.Is(err, events.ErrPromptTimeout):
		return &certRejectedError{msg: "the server certificate was not confirmed in time"}
	case err != nil:
		return &certRejectedError{msg: "the server certificate could not be confirmed: " + err.Error()}
	}
	if !resp.Accept {
		if mismatch {
			return &certRejectedError{msg: "certificate verification failed: the certificate of " + t.target() + " has changed"}
		}
		return &certRejectedError{msg: "the certificate of " + t.target() + " was rejected"}
	}
	if resp.Save && allowSave {
		t.save(leaf, fp, mismatch)
	}
	if mismatch {
		h.auditUser(t.ctx, t.user, "rdp.cert.mismatch_accepted", t.target(), map[string]any{"fingerprint": fp,
			"knownFingerprint": info.KnownFingerprint, "saved": resp.Save && allowSave, "sessionId": t.sessionID})
	}
	t.accept(fp, true)
	return nil
}

func (t *certTrust) save(leaf *x509.Certificate, fp string, replace bool) {
	h := t.h
	if h.d == nil || h.d.Store == nil {
		return
	}
	who := ""
	if t.user != nil {
		who = t.user.Username
	}
	subject := leaf.Subject.CommonName
	if subject == "" {
		subject = leaf.Subject.String()
	}
	kh := &model.KnownHost{
		Host:        t.host,
		Port:        t.port,
		KeyType:     rdpKeyType,
		PublicKey:   base64.StdEncoding.EncodeToString(leaf.Raw),
		Fingerprint: fp,
		Comment: clipRunes(fmt.Sprintf("RDP certificate %q (expires %s), accepted by %s on %s", subject,
			leaf.NotAfter.UTC().Format("2006-01-02"), who, time.Now().UTC().Format("2006-01-02")), 500),
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), 10*time.Second)
	defer cancel()
	var err error
	if replace {
		err = h.d.Store.KnownHosts.Replace(ctx, kh)
	} else {
		err = h.d.Store.KnownHosts.Add(ctx, kh)
	}
	if err != nil {
		h.log.Warn("rdp: saving the server certificate failed", "host", t.host, "port", t.port, "err", err)
		return
	}
	action := "known_host.add"
	if replace {
		action = "known_host.replace"
	}
	h.auditUser(t.ctx, t.user, action, t.target(), map[string]any{"keyType": rdpKeyType, "fingerprint": fp})
}

// certProblem explains why the certificate did not verify against the system roots. Platform verifiers (macOS,
// Windows) return untyped errors, so the certificate itself is examined too.
func certProblem(err error, leaf *x509.Certificate, name string) string {
	var (
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
	)
	now := time.Now()
	switch {
	case leaf != nil && (now.After(leaf.NotAfter) || now.Before(leaf.NotBefore)):
		return "the certificate has expired or is not yet valid"
	case leaf != nil && bytes.Equal(leaf.RawIssuer, leaf.RawSubject) && leaf.CheckSignatureFrom(leaf) == nil:
		return "the certificate is self-signed"
	case leaf != nil && name != "" && leaf.VerifyHostname(name) != nil:
		return fmt.Sprintf("the certificate is not valid for %q", name)
	case errors.As(err, &hostname):
		return fmt.Sprintf("the certificate is not valid for %q", name)
	case errors.As(err, &invalid):
		if invalid.Reason == x509.Expired {
			return "the certificate has expired or is not yet valid"
		}
		return "the certificate is invalid"
	case errors.As(err, &unknown):
		return "the certificate is self-signed or issued by an unknown authority"
	}
	return "the certificate could not be verified"
}

// describeCert summarizes a certificate for the prompt.
func describeCert(c *x509.Certificate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Subject: %s\n", c.Subject.String())
	if c.Issuer.String() != c.Subject.String() {
		fmt.Fprintf(&b, "Issuer: %s\n", c.Issuer.String())
	} else {
		b.WriteString("Issuer: self-signed\n")
	}
	if len(c.DNSNames) > 0 {
		fmt.Fprintf(&b, "Names: %s\n", strings.Join(c.DNSNames, ", "))
	}
	fmt.Fprintf(&b, "Valid: %s – %s\n", c.NotBefore.UTC().Format("2006-01-02"), c.NotAfter.UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "SHA-1 thumbprint: %s", certThumbprint(c.Raw))
	return b.String()
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
