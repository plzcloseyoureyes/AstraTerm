// Package webauthn implements passkeys (RESEARCH MU-6, SEC-4): WebAuthn credential registration, rename and deletion,
// passwordless sign-in with discoverable credentials (including conditional-UI autofill), passkeys as the second
// factor of a password login (alternative to TOTP), and passkey re-verification for the lock screen and sensitive
// account changes. Built on github.com/go-webauthn/webauthn; the relying party (RP ID and origin) is derived from the
// request's Host unless an administrator pins it (GET/PUT /api/admin/webauthn/config).
package webauthn

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	gwa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/labstack/echo/v5"
	"golang.org/x/time/rate"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/auth"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
)

func init() {
	store.RegisterMigration("webauthn", 1, `
CREATE TABLE webauthn_users (
	user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	handle     BLOB NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);
CREATE TABLE webauthn_credentials (
	id              TEXT PRIMARY KEY,
	user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	credential_id   TEXT NOT NULL UNIQUE,
	rp_id           TEXT NOT NULL,
	name            TEXT NOT NULL,
	data            TEXT NOT NULL,
	aaguid          TEXT NOT NULL DEFAULT '',
	discoverable    INTEGER NOT NULL DEFAULT 0,
	backup_eligible INTEGER NOT NULL DEFAULT 0,
	backup_state    INTEGER NOT NULL DEFAULT 0,
	user_verified   INTEGER NOT NULL DEFAULT 0,
	transports      TEXT NOT NULL DEFAULT '[]',
	sign_count      INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	last_used_at    INTEGER
);
CREATE INDEX webauthn_credentials_user ON webauthn_credentials(user_id);`)
}

const (
	ceremonyTTL     = 5 * time.Minute
	maxCeremonies   = 5000
	maxCredsPerUser = 32
	maxNameLen      = 64
	rpDisplayName   = "NexTerm"
	configScope     = "auth"
	configKey       = "webauthn"
)

// Ceremony kinds.
const (
	kindRegister = "register"
	kindLogin    = "login"
	kindMFA      = "mfa"
	kindVerify   = "verify"
)

// Service is the passkey module.
type Service struct {
	d    *app.Deps
	auth *auth.Service
	log  *slog.Logger

	mu         sync.Mutex
	ceremonies map[string]*ceremony
	ipLimits   map[string]*ipLimit
}

type ceremony struct {
	kind     string
	session  gwa.SessionData
	userID   string // register / mfa / verify
	rpID     string
	origin   string
	mfaToken string
	expires  time.Time
}

type ipLimit struct {
	lim  *rate.Limiter
	seen time.Time
}

// Mount registers the passkey endpoints and plugs passkeys into the auth module's login flow.
func Mount(d *app.Deps, _ *core.Core) error {
	a := auth.ServiceFor(d)
	if a == nil {
		return errors.New("webauthn: the auth module is not mounted")
	}
	if err := d.Store.Migrate(d.Ctx); err != nil {
		return err
	}
	s := &Service{d: d, auth: a, log: d.Log.With("module", "webauthn"), ceremonies: map[string]*ceremony{}, ipLimits: map[string]*ipLimit{}}
	a.SetPasskeyProvider(s)

	pub, api, admin := d.Router.Public(), d.Router.API(), d.Router.Admin()
	api.GET("/auth/webauthn/credentials", s.handleList)
	api.POST("/auth/webauthn/register/begin", s.handleRegisterBegin)
	api.POST("/auth/webauthn/register/finish", s.handleRegisterFinish)
	api.PATCH("/auth/webauthn/credentials/:id", s.handleRename)
	api.DELETE("/auth/webauthn/credentials/:id", s.handleDelete)
	api.POST("/auth/webauthn/verify/begin", s.handleVerifyBegin)
	api.POST("/auth/webauthn/verify/finish", s.handleVerifyFinish)
	pub.GET("/auth/webauthn/status", s.handleStatus)
	pub.POST("/auth/webauthn/login/begin", s.handleLoginBegin)
	pub.POST("/auth/webauthn/login/finish", s.handleLoginFinish)
	pub.POST("/auth/webauthn/mfa/begin", s.handleMFABegin)
	pub.POST("/auth/webauthn/mfa/finish", s.handleMFAFinish)
	admin.GET("/admin/webauthn/config", s.handleGetConfig)
	admin.PUT("/admin/webauthn/config", s.handlePutConfig)

	go s.janitor()
	return nil
}

func (s *Service) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.d.Ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			for k, c := range s.ceremonies {
				if now.After(c.expires) {
					delete(s.ceremonies, k)
				}
			}
			for k, l := range s.ipLimits {
				if now.Sub(l.seen) > 10*time.Minute {
					delete(s.ipLimits, k)
				}
			}
			s.mu.Unlock()
		}
	}
}

// ---- relying party --------------------------------------------------------------------------------------------------

// Config pins the relying party (optional; stored in the module-private settings scope "auth", key "webauthn").
type Config struct {
	// RPID is the registrable domain passkeys are bound to ("" = the host name of each request).
	RPID string `json:"rpId"`
	// Origins are the accepted page origins, e.g. "https://nexterm.example.com" (required with RPID).
	Origins []string `json:"origins"`
}

func (s *Service) config(ctx context.Context) Config {
	var cfg Config
	if _, err := s.d.Store.Settings.GetJSON(ctx, configScope, configKey, &cfg); err != nil {
		s.log.Warn("load passkey config", "err", err)
	}
	if cfg.Origins == nil {
		cfg.Origins = []string{}
	}
	return cfg
}

var errUnavailable = httpx.NewError(http.StatusConflict, "webauthn_unavailable",
	"passkeys need a host name (not an IP address): open NexTerm via http://localhost or its DNS name")

type relyingParty struct {
	id     string
	origin string
}

// requestOrigin returns the scheme://host[:port] the browser used.
func (s *Service) requestOrigin(c *echo.Context) (origin, hostname string) {
	host := c.Request().Host
	scheme := "http"
	if s.auth.CookieSecure(c) {
		scheme = "https"
	}
	hostname = host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.ToLower(strings.Trim(hostname, "[]"))
	return scheme + "://" + strings.ToLower(host), hostname
}

func (s *Service) relyingParty(c *echo.Context) (relyingParty, error) {
	origin, hostname := s.requestOrigin(c)
	if cfg := s.config(c.Request().Context()); cfg.RPID != "" {
		for _, o := range cfg.Origins {
			if strings.EqualFold(strings.TrimRight(o, "/"), origin) {
				return relyingParty{id: cfg.RPID, origin: origin}, nil
			}
		}
		return relyingParty{}, httpx.NewError(http.StatusConflict, "webauthn_origin",
			"passkeys are configured for "+strings.Join(cfg.Origins, ", ")+": open NexTerm there to use them")
	}
	if hostname == "" {
		return relyingParty{}, errUnavailable
	}
	if _, err := netip.ParseAddr(hostname); err == nil {
		return relyingParty{}, errUnavailable
	}
	if err := protocol.ValidateRPID(hostname); err != nil {
		return relyingParty{}, errUnavailable
	}
	return relyingParty{id: hostname, origin: origin}, nil
}

func (rp relyingParty) webauthn(uv protocol.UserVerificationRequirement) (*gwa.WebAuthn, error) {
	return gwa.New(&gwa.Config{
		RPID:                  rp.id,
		RPDisplayName:         rpDisplayName,
		RPOrigins:             []string{rp.origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: uv,
		},
		Timeouts: gwa.TimeoutsConfig{
			Login:        gwa.TimeoutConfig{Enforce: true, Timeout: 3 * time.Minute, TimeoutUVD: 3 * time.Minute},
			Registration: gwa.TimeoutConfig{Enforce: true, Timeout: 5 * time.Minute, TimeoutUVD: 5 * time.Minute},
		},
	})
}

// ---- users & credentials --------------------------------------------------------------------------------------------

// waUser adapts a NexTerm user to go-webauthn's User.
type waUser struct {
	u      *model.User
	handle []byte
	creds  []storedCredential
}

func (w *waUser) WebAuthnID() []byte          { return w.handle }
func (w *waUser) WebAuthnName() string        { return w.u.Username }
func (w *waUser) WebAuthnDisplayName() string { return cmp(w.u.DisplayName, w.u.Username) }
func (w *waUser) WebAuthnCredentials() []gwa.Credential {
	out := make([]gwa.Credential, 0, len(w.creds))
	for _, c := range w.creds {
		out = append(out, c.cred)
	}
	return out
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type storedCredential struct {
	ID           string
	UserID       string
	CredentialID string
	RPID         string
	Name         string
	AAGUID       string
	Discoverable bool
	BackupElig   bool
	BackupState  bool
	UserVerified bool
	Transports   []string
	SignCount    uint32
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	cred         gwa.Credential
}

// CredentialView is the JSON shape of a passkey.
type CredentialView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	RPID          string     `json:"rpId"`
	Authenticator string     `json:"authenticator,omitempty"`
	AAGUID        string     `json:"aaguid,omitempty"`
	Discoverable  bool       `json:"discoverable"`
	Synced        bool       `json:"synced"`
	BackupCapable bool       `json:"backupEligible"`
	UserVerified  bool       `json:"userVerified"`
	Transports    []string   `json:"transports"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUsedAt    *time.Time `json:"lastUsedAt,omitempty"`
}

func (c *storedCredential) view() CredentialView {
	t := c.Transports
	if t == nil {
		t = []string{}
	}
	return CredentialView{ID: c.ID, Name: c.Name, RPID: c.RPID, Authenticator: authenticatorName(c.AAGUID), AAGUID: c.AAGUID,
		Discoverable: c.Discoverable, Synced: c.BackupState, BackupCapable: c.BackupElig, UserVerified: c.UserVerified,
		Transports: t, CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt}
}

const credCols = `id, user_id, credential_id, rp_id, name, data, aaguid, discoverable, backup_eligible, backup_state,
	user_verified, transports, sign_count, created_at, last_used_at`

func scanCredential(sc interface{ Scan(...any) error }) (*storedCredential, error) {
	var (
		c                  storedCredential
		data, transports   string
		disc, be, bs, uv   int
		signCount, created int64
		lastUsed           sql.NullInt64
	)
	if err := sc.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.RPID, &c.Name, &data, &c.AAGUID, &disc, &be, &bs, &uv,
		&transports, &signCount, &created, &lastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(data), &c.cred); err != nil {
		return nil, fmt.Errorf("decode credential %s: %w", c.ID, err)
	}
	_ = json.Unmarshal([]byte(transports), &c.Transports)
	c.Discoverable, c.BackupElig, c.BackupState, c.UserVerified = disc != 0, be != 0, bs != 0, uv != 0
	c.SignCount = uint32(signCount)
	c.CreatedAt = time.UnixMilli(created).UTC()
	if lastUsed.Valid {
		t := time.UnixMilli(lastUsed.Int64).UTC()
		c.LastUsedAt = &t
	}
	return &c, nil
}

func (s *Service) listCredentials(ctx context.Context, userID string) ([]*storedCredential, error) {
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT `+credCols+` FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*storedCredential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) getCredential(ctx context.Context, userID, id string) (*storedCredential, error) {
	return scanCredential(s.d.Store.DB.QueryRowContext(ctx, `SELECT `+credCols+` FROM webauthn_credentials WHERE id = ? AND user_id = ?`, id, userID))
}

// userHandle returns (creating when asked) the random WebAuthn user handle of userID.
func (s *Service) userHandle(ctx context.Context, userID string, create bool) ([]byte, error) {
	var h []byte
	err := s.d.Store.DB.QueryRowContext(ctx, `SELECT handle FROM webauthn_users WHERE user_id = ?`, userID).Scan(&h)
	if err == nil || !errors.Is(err, sql.ErrNoRows) || !create {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return h, err
	}
	h = make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		return nil, err
	}
	if _, err := s.d.Store.DB.ExecContext(ctx, `INSERT INTO webauthn_users (user_id, handle, created_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO NOTHING`, userID, h, store.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return s.userHandle(ctx, userID, false)
}

// loadUser builds the go-webauthn user for userID with the credentials bound to rpID.
func (s *Service) loadUser(ctx context.Context, userID, rpID string, createHandle bool) (*waUser, error) {
	u, err := s.d.Store.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	h, err := s.userHandle(ctx, userID, createHandle)
	if err != nil {
		return nil, err
	}
	creds, err := s.listCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	w := &waUser{u: u, handle: h}
	for _, c := range creds {
		if rpID == "" || c.RPID == rpID {
			w.creds = append(w.creds, *c)
		}
	}
	return w, nil
}

// userByHandle resolves a discoverable credential's user handle.
func (s *Service) userByHandle(ctx context.Context, handle []byte) (string, error) {
	var id string
	err := s.d.Store.DB.QueryRowContext(ctx, `SELECT user_id FROM webauthn_users WHERE handle = ?`, handle).Scan(&id)
	return id, err
}

// ---- auth.PasskeyProvider -------------------------------------------------------------------------------------------

// CountCredentials implements auth.PasskeyProvider.
func (s *Service) CountCredentials(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.d.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// CountAll implements auth.PasskeyProvider.
func (s *Service) CountAll(ctx context.Context) (map[string]int, error) {
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT user_id, COUNT(*) FROM webauthn_credentials GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// DeleteAll implements auth.PasskeyProvider.
func (s *Service) DeleteAll(ctx context.Context, userID string) (int, error) {
	res, err := s.d.Store.DB.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- ceremonies -----------------------------------------------------------------------------------------------------

var errCeremony = httpx.NewError(http.StatusBadRequest, "webauthn_expired", "the passkey request expired, please try again")

func randomID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("webauthn: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) putCeremony(c *ceremony) (string, error) {
	id := randomID()
	c.expires = time.Now().Add(ceremonyTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ceremonies) >= maxCeremonies {
		now := time.Now()
		for k, v := range s.ceremonies {
			if now.After(v.expires) {
				delete(s.ceremonies, k)
			}
		}
		if len(s.ceremonies) >= maxCeremonies {
			return "", httpx.TooManyRequests("too many pending passkey requests, try again shortly", 5)
		}
	}
	s.ceremonies[id] = c
	return id, nil
}

// takeCeremony removes and returns ceremony id (single use) when it is of kind and was started at this origin.
func (s *Service) takeCeremony(c *echo.Context, id, kind string) (*ceremony, error) {
	s.mu.Lock()
	cer := s.ceremonies[id]
	delete(s.ceremonies, id)
	s.mu.Unlock()
	if cer == nil || cer.kind != kind || time.Now().After(cer.expires) {
		return nil, errCeremony
	}
	if origin, _ := s.requestOrigin(c); origin != cer.origin {
		return nil, errCeremony
	}
	return cer, nil
}

// allowIP rate-limits unauthenticated ceremony starts per client IP.
func (s *Service) allowIP(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ipLimits[ip]
	if l == nil {
		l = &ipLimit{lim: rate.NewLimiter(rate.Every(time.Second), 20)}
		s.ipLimits[ip] = l
	}
	l.seen = time.Now()
	return l.lim.Allow()
}

// ceremonyError turns a go-webauthn verification failure into a client error (details logged, never secrets).
func (s *Service) ceremonyError(kind string, err error) error {
	var pe *protocol.Error
	msg := "the passkey could not be verified"
	if errors.As(err, &pe) {
		s.log.Info("passkey ceremony failed", "kind", kind, "type", pe.Type, "details", pe.Details, "info", pe.DevInfo)
		if pe.Details != "" {
			msg += ": " + pe.Details
		}
	} else {
		s.log.Info("passkey ceremony failed", "kind", kind, "err", err)
	}
	return httpx.NewError(http.StatusBadRequest, "webauthn_failed", msg)
}

// saveAfterAssertion records a successful assertion (sign count, flags, last use). A signature counter that went
// backwards marks a possibly cloned authenticator: the login is refused.
func (s *Service) saveAfterAssertion(ctx context.Context, stored *storedCredential, cred *gwa.Credential) error {
	if cred.Authenticator.CloneWarning {
		return httpx.NewError(http.StatusForbidden, "webauthn_clone_warning",
			"this passkey's signature counter went backwards (possible cloned authenticator); it was not accepted")
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	_, err = s.d.Store.DB.ExecContext(ctx, `UPDATE webauthn_credentials SET data = ?, sign_count = ?, backup_state = ?,
		user_verified = MAX(user_verified, ?), last_used_at = ? WHERE id = ?`, string(data), int64(cred.Authenticator.SignCount),
		b2i(cred.Flags.BackupState), b2i(cred.Flags.UserVerified), store.Now().UnixMilli(), stored.ID)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// findStored returns the stored credential matching cred's raw ID among w's credentials.
func findStored(w *waUser, rawID []byte) *storedCredential {
	id := base64.RawURLEncoding.EncodeToString(rawID)
	for i := range w.creds {
		if w.creds[i].CredentialID == id {
			return &w.creds[i]
		}
	}
	return nil
}
