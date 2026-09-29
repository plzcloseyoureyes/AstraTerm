package vnc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// Trusted VeNCrypt certificates (TOFU): like known_hosts for SSH, one global entry per host:port holding the SHA-256
// fingerprint the user accepted and saved. Certificates that chain to a system root for the host name are trusted
// without an entry.

func init() {
	store.RegisterMigration("vnc", 1, `CREATE TABLE IF NOT EXISTS vnc_trusted_certs (
		id          TEXT PRIMARY KEY,
		host        TEXT NOT NULL,
		port        INTEGER NOT NULL,
		fingerprint TEXT NOT NULL,
		subject     TEXT NOT NULL DEFAULT '',
		issuer      TEXT NOT NULL DEFAULT '',
		not_after   INTEGER,
		comment     TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		UNIQUE (host, port)
	)`)
}

// TrustedCert is a saved VNC server certificate fingerprint (GET /api/vnc/certs).
type TrustedCert struct {
	ID          string     `json:"id"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	Fingerprint string     `json:"fingerprint"`
	Subject     string     `json:"subject"`
	Issuer      string     `json:"issuer"`
	NotAfter    *time.Time `json:"notAfter,omitempty"`
	Comment     string     `json:"comment"`
	CreatedAt   time.Time  `json:"createdAt"`
}

type certStore struct{ db *sql.DB }

const certCols = `id, host, port, fingerprint, subject, issuer, not_after, comment, created_at`

func scanCert(sc interface{ Scan(...any) error }) (*TrustedCert, error) {
	var c TrustedCert
	var notAfter sql.NullInt64
	var created int64
	if err := sc.Scan(&c.ID, &c.Host, &c.Port, &c.Fingerprint, &c.Subject, &c.Issuer, &notAfter, &c.Comment, &created); err != nil {
		return nil, err
	}
	if notAfter.Valid {
		t := time.UnixMilli(notAfter.Int64).UTC()
		c.NotAfter = &t
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	return &c, nil
}

func (s *certStore) find(ctx context.Context, host string, port int) (*TrustedCert, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+certCols+` FROM vnc_trusted_certs WHERE host = ? AND port = ?`,
		strings.ToLower(host), port)
	c, err := scanCert(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *certStore) list(ctx context.Context) ([]*TrustedCert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+certCols+` FROM vnc_trusted_certs ORDER BY host, port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TrustedCert{}
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// save stores (or replaces) the trusted certificate of host:port.
func (s *certStore) save(ctx context.Context, c *TrustedCert) error {
	var notAfter any
	if c.NotAfter != nil {
		notAfter = c.NotAfter.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO vnc_trusted_certs (`+certCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host, port) DO UPDATE SET id = excluded.id, fingerprint = excluded.fingerprint,
			subject = excluded.subject, issuer = excluded.issuer, not_after = excluded.not_after,
			comment = excluded.comment, created_at = excluded.created_at`,
		c.ID, strings.ToLower(c.Host), c.Port, c.Fingerprint, c.Subject, c.Issuer, notAfter, c.Comment, c.CreatedAt.UnixMilli())
	return err
}

func (s *certStore) get(ctx context.Context, id string) (*TrustedCert, error) {
	c, err := scanCert(s.db.QueryRowContext(ctx, `SELECT `+certCols+` FROM vnc_trusted_certs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, httpx.ErrNotFound
	}
	return c, err
}

func (s *certStore) delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM vnc_trusted_certs WHERE id = ?`, id)
	return err
}

// certFingerprint returns "SHA256:AB:CD:…" (the format of `openssl x509 -fingerprint -sha256`, prefixed).
func certFingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "SHA256:" + colonHex(sum[:])
}

func certFingerprintMD5(c *x509.Certificate) string {
	sum := md5.Sum(c.Raw)
	return "MD5:" + strings.ToLower(colonHex(sum[:]))
}

func colonHex(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	var sb strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(h[i : i+2])
	}
	return sb.String()
}

// certKeyType describes the certificate's public key ("X.509 certificate (RSA 2048-bit)", "… (ECDSA P-256)").
func certKeyType(c *x509.Certificate) string {
	switch k := c.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("X.509 certificate (RSA %d-bit)", k.N.BitLen())
	case *ecdsa.PublicKey:
		return "X.509 certificate (ECDSA " + k.Curve.Params().Name + ")"
	case ed25519.PublicKey:
		return "X.509 certificate (Ed25519)"
	}
	return "X.509 certificate"
}

// systemTrusted reports whether the chain verifies against the system roots for host.
func systemTrusted(host string, certs []*x509.Certificate) bool {
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{DNSName: strings.Trim(host, "[]"), Intermediates: inter})
	return err == nil
}

func describeCert(c *x509.Certificate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Subject: %s\nIssuer: %s\nValid: %s – %s", c.Subject.String(), c.Issuer.String(),
		c.NotBefore.UTC().Format("2006-01-02"), c.NotAfter.UTC().Format("2006-01-02"))
	if c.Subject.String() == c.Issuer.String() {
		b.WriteString(" (self-signed)")
	}
	now := time.Now()
	if now.After(c.NotAfter) {
		b.WriteString("\nThe certificate has EXPIRED.")
	} else if now.Before(c.NotBefore) {
		b.WriteString("\nThe certificate is not valid yet.")
	}
	if len(c.DNSNames) > 0 || len(c.IPAddresses) > 0 {
		names := append([]string{}, c.DNSNames...)
		for _, ip := range c.IPAddresses {
			names = append(names, ip.String())
		}
		fmt.Fprintf(&b, "\nNames: %s", strings.Join(names, ", "))
	}
	return b.String()
}

// ---- REST ---------------------------------------------------------------------------------------------------------

func (m *Module) handleListCerts(c *echo.Context) error {
	list, err := m.certs.list(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

// handleDeleteCert removes a trusted certificate. The table is global, so in server mode only administrators may
// change it (like replacing a changed SSH host key).
func (m *Module) handleDeleteCert(c *echo.Context) error {
	user := httpx.UserFrom(c)
	if !m.mayChangeGlobalTrust(user) {
		return httpx.Forbidden("only administrators can remove trusted certificates in server mode")
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return httpx.ErrNotFound
	}
	ctx := c.Request().Context()
	cert, err := m.certs.get(ctx, id)
	if err != nil {
		return err
	}
	if err := m.certs.delete(ctx, id); err != nil {
		return err
	}
	m.d.Audit.Log(c, "vnc.cert.delete", net.JoinHostPort(cert.Host, fmt.Sprint(cert.Port)),
		map[string]any{"fingerprint": cert.Fingerprint})
	return httpx.OK(c)
}

func (m *Module) mayChangeGlobalTrust(user *model.User) bool {
	return user != nil && (user.IsAdmin() || (m.d.Cfg != nil && m.d.Cfg.IsDesktop()))
}
