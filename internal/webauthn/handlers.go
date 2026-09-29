package webauthn

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	gwa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/auth"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// ceremonyResponse starts a ceremony in the browser: options is the PublicKeyCredential{Creation,Request}OptionsJSON
// to pass to navigator.credentials (e.g. @simplewebauthn/browser's startRegistration / startAuthentication).
type ceremonyResponse struct {
	CeremonyID string `json:"ceremonyId"`
	Options    any    `json:"options"`
	Mediation  string `json:"mediation,omitempty"`
}

// ---- status ---------------------------------------------------------------------------------------------------------

// handleStatus tells the UI whether passkeys can work on the address it is using (GET /api/auth/webauthn/status).
func (s *Service) handleStatus(c *echo.Context) error {
	rp, err := s.relyingParty(c)
	out := map[string]any{"available": err == nil}
	if err != nil {
		var he *httpx.HTTPError
		if errors.As(err, &he) {
			out["reason"], out["code"] = he.Message, he.Code
		}
	} else {
		out["rpId"], out["origin"] = rp.id, rp.origin
	}
	return c.JSON(http.StatusOK, out)
}

// ---- management -----------------------------------------------------------------------------------------------------

func (s *Service) handleList(c *echo.Context) error {
	creds, err := s.listCredentials(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	out := make([]CredentialView, 0, len(creds))
	for _, cr := range creds {
		out = append(out, cr.view())
	}
	return c.JSON(http.StatusOK, out)
}

type registerBeginRequest struct {
	Password string `json:"password"`
}

// handleRegisterBegin starts adding a passkey. Adding a sign-in method needs a recent login / re-verification (or
// the password in the body), so a hijacked session cannot plant its own passkey.
func (s *Service) handleRegisterBegin(c *echo.Context) error {
	var req registerBeginRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	if err := s.auth.RequireRecentAuth(c, req.Password); err != nil {
		return err
	}
	rp, err := s.relyingParty(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	me := httpx.UserFrom(c)
	w, err := s.loadUser(ctx, me.ID, "", true)
	if err != nil {
		return err
	}
	if len(w.creds) >= maxCredsPerUser {
		return httpx.Conflict("you already have the maximum number of passkeys; remove one first")
	}
	var exclude []protocol.CredentialDescriptor
	for _, cr := range w.creds {
		if cr.RPID == rp.id {
			exclude = append(exclude, cr.cred.Descriptor())
		}
	}
	wa, err := rp.webauthn(protocol.VerificationPreferred)
	if err != nil {
		return httpx.Internal(err)
	}
	creation, session, err := wa.BeginRegistration(w, gwa.WithExclusions(exclude), gwa.WithExtensions(gwa.WithExtensionCredProps()))
	if err != nil {
		return httpx.Internal(err)
	}
	id, err := s.putCeremony(&ceremony{kind: kindRegister, session: *session, userID: me.ID, rpID: rp.id, origin: rp.origin})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, ceremonyResponse{CeremonyID: id, Options: creation.Response})
}

type registerFinishRequest struct {
	CeremonyID string          `json:"ceremonyId"`
	Name       string          `json:"name"`
	Credential json.RawMessage `json:"credential"`
}

func cleanName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	for utf8.RuneCountInString(name) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

func (s *Service) handleRegisterFinish(c *echo.Context) error {
	var req registerFinishRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	cer, err := s.takeCeremony(c, req.CeremonyID, kindRegister)
	if err != nil {
		return err
	}
	me := httpx.UserFrom(c)
	if cer.userID != me.ID {
		return errCeremony
	}
	ctx := c.Request().Context()
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		return s.ceremonyError(kindRegister, err)
	}
	w, err := s.loadUser(ctx, me.ID, "", false)
	if err != nil {
		return err
	}
	wa, err := relyingParty{id: cer.rpID, origin: cer.origin}.webauthn(protocol.VerificationPreferred)
	if err != nil {
		return httpx.Internal(err)
	}
	cred, err := wa.CreateCredential(w, cer.session, parsed)
	if err != nil {
		return s.ceremonyError(kindRegister, err)
	}
	aaguid := ""
	if u, err := uuid.FromBytes(cred.Authenticator.AAGUID); err == nil && u != uuid.Nil {
		aaguid = u.String()
	}
	name := cleanName(req.Name)
	if name == "" {
		name = cmp(authenticatorName(aaguid), "Passkey")
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	discoverable := cred.Extensions.RK != nil && *cred.Extensions.RK
	data, err := json.Marshal(cred)
	if err != nil {
		return httpx.Internal(err)
	}
	tj, _ := json.Marshal(transports)
	now := store.Now()
	sc := &storedCredential{ID: model.NewID(), UserID: me.ID, CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID),
		RPID: cer.rpID, Name: name, AAGUID: aaguid, Discoverable: discoverable, BackupElig: cred.Flags.BackupEligible,
		BackupState: cred.Flags.BackupState, UserVerified: cred.Flags.UserVerified, Transports: transports,
		SignCount: cred.Authenticator.SignCount, CreatedAt: now}
	_, err = s.d.Store.DB.ExecContext(ctx, `INSERT INTO webauthn_credentials (`+credCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		sc.ID, sc.UserID, sc.CredentialID, sc.RPID, sc.Name, string(data), sc.AAGUID, store.B2I(sc.Discoverable), store.B2I(sc.BackupElig),
		store.B2I(sc.BackupState), store.B2I(sc.UserVerified), string(tj), int64(sc.SignCount), now.UnixMilli())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return httpx.Conflict("this passkey is already registered")
		}
		return err
	}
	s.d.Audit.Log(c, "auth.passkey.add", sc.ID, map[string]any{"name": sc.Name, "rpId": sc.RPID, "authenticator": authenticatorName(aaguid),
		"discoverable": discoverable, "synced": sc.BackupState})
	return c.JSON(http.StatusCreated, sc.view())
}

func (s *Service) handleRename(c *echo.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	name := cleanName(req.Name)
	if name == "" {
		return httpx.BadRequest("name must not be empty")
	}
	ctx := c.Request().Context()
	me := httpx.UserFrom(c)
	res, err := s.d.Store.DB.ExecContext(ctx, `UPDATE webauthn_credentials SET name = ? WHERE id = ? AND user_id = ?`, name, c.Param("id"), me.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return httpx.ErrNotFound
	}
	cr, err := s.getCredential(ctx, me.ID, c.Param("id"))
	if err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.passkey.rename", cr.ID, map[string]string{"name": name})
	return c.JSON(http.StatusOK, cr.view())
}

// handleDelete removes a passkey (recent authentication or the password required).
func (s *Service) handleDelete(c *echo.Context) error {
	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	ctx := c.Request().Context()
	me := httpx.UserFrom(c)
	cr, err := s.getCredential(ctx, me.ID, c.Param("id"))
	if err != nil {
		return err
	}
	if err := s.auth.RequireRecentAuth(c, req.Password); err != nil {
		return err
	}
	if err := s.checkLastPasskey(c, me); err != nil {
		return err
	}
	if _, err := s.d.Store.DB.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?`, cr.ID, me.ID); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.passkey.delete", cr.ID, map[string]string{"name": cr.Name})
	return httpx.OK(c)
}

// checkLastPasskey refuses to remove u's last passkey when it is the account's only way to sign in (no password, no
// linked single-sign-on identity) or its only second factor while the login policy requires one.
func (s *Service) checkLastPasskey(c *echo.Context, u *model.User) error {
	ctx := c.Request().Context()
	n, err := s.CountCredentials(ctx, u.ID)
	if err != nil || n > 1 {
		return err
	}
	hasPw, err := s.auth.HasPassword(ctx, u.ID)
	if err != nil {
		return err
	}
	if !hasPw {
		sso, err := s.auth.HasSSOIdentity(ctx, u.ID)
		if err != nil {
			return err
		}
		if !sso {
			return httpx.Conflict("this passkey is your only way to sign in: set a password first")
		}
	}
	if hasPw && !u.TOTPEnabled && s.auth.MFARequiredFor(ctx, u) {
		return httpx.NewError(http.StatusConflict, "mfa_required",
			"the login policy requires a second factor: set up an authenticator app before removing your last passkey")
	}
	return nil
}

// ---- passwordless sign-in -------------------------------------------------------------------------------------------

type loginBeginRequest struct {
	// Conditional requests options for conditional mediation (passkey autofill in the username field).
	Conditional bool `json:"conditional"`
}

func (s *Service) handleLoginBegin(c *echo.Context) error {
	var req loginBeginRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	if !s.allowIP(auth.RateKeyIP(httpx.ClientIP(c))) {
		return httpx.TooManyRequests("too many passkey requests, try again shortly", 2)
	}
	rp, err := s.relyingParty(c)
	if err != nil {
		return err
	}
	wa, err := rp.webauthn(protocol.VerificationRequired)
	if err != nil {
		return httpx.Internal(err)
	}
	mediation := protocol.MediationDefault
	if req.Conditional {
		mediation = protocol.MediationConditional
	}
	assertion, session, err := wa.BeginDiscoverableMediatedLogin(mediation)
	if err != nil {
		return httpx.Internal(err)
	}
	id, err := s.putCeremony(&ceremony{kind: kindLogin, session: *session, rpID: rp.id, origin: rp.origin})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, ceremonyResponse{CeremonyID: id, Options: assertion.Response, Mediation: string(assertion.Mediation)})
}

type loginFinishRequest struct {
	CeremonyID string          `json:"ceremonyId"`
	Credential json.RawMessage `json:"credential"`
	Remember   bool            `json:"remember"`
}

type userResponse struct {
	User *model.User `json:"user"`
}

func (s *Service) handleLoginFinish(c *echo.Context) error {
	var req loginFinishRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	cer, err := s.takeCeremony(c, req.CeremonyID, kindLogin)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		return s.ceremonyError(kindLogin, err)
	}
	wa, err := relyingParty{id: cer.rpID, origin: cer.origin}.webauthn(protocol.VerificationRequired)
	if err != nil {
		return httpx.Internal(err)
	}
	var owner *waUser
	handler := func(rawID, userHandle []byte) (gwa.User, error) {
		userID, err := s.userByHandle(ctx, userHandle)
		if err != nil {
			return nil, errors.New("unknown passkey")
		}
		w, err := s.loadUser(ctx, userID, cer.rpID, false)
		if err != nil {
			return nil, errors.New("unknown passkey")
		}
		owner = w
		return w, nil
	}
	_, cred, err := wa.ValidatePasskeyLogin(handler, cer.session, parsed)
	if err != nil || owner == nil {
		s.d.Audit.LogUser(c, nil, "auth.login.failed", "", map[string]string{"reason": "passkey", "method": "passkey"})
		if err == nil {
			err = errors.New("unknown passkey")
		}
		return s.ceremonyError(kindLogin, err)
	}
	stored := findStored(owner, cred.ID)
	if stored == nil {
		return s.ceremonyError(kindLogin, errors.New("unknown passkey"))
	}
	if err := s.saveAfterAssertion(ctx, stored, cred); err != nil {
		s.d.Audit.LogUser(c, owner.u, "auth.passkey.clone_warning", stored.ID, map[string]string{"name": stored.Name})
		return err
	}
	u, err := s.auth.CompleteLogin(c, owner.u.ID, loginOptions(req.Remember, stored))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, userResponse{User: u})
}

// ---- passkey as a second factor ---------------------------------------------------------------------------------------

type mfaBeginRequest struct {
	MFAToken string `json:"mfaToken"`
}

func (s *Service) handleMFABegin(c *echo.Context) error {
	var req mfaBeginRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ch, err := s.auth.LookupMFA(req.MFAToken)
	if err != nil {
		return err
	}
	if ch.Enroll || !slices.Contains(ch.Methods, "webauthn") {
		return httpx.BadRequest("this sign-in does not accept a passkey")
	}
	rp, err := s.relyingParty(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	w, err := s.loadUser(ctx, ch.UserID, rp.id, false)
	if err != nil {
		return err
	}
	if len(w.creds) == 0 {
		return httpx.NewError(http.StatusConflict, "webauthn_no_credentials",
			"none of your passkeys works on this address ("+rp.id+"); use your authenticator app instead")
	}
	wa, err := rp.webauthn(protocol.VerificationPreferred)
	if err != nil {
		return httpx.Internal(err)
	}
	assertion, session, err := wa.BeginLogin(w)
	if err != nil {
		return httpx.Internal(err)
	}
	id, err := s.putCeremony(&ceremony{kind: kindMFA, session: *session, userID: ch.UserID, rpID: rp.id, origin: rp.origin, mfaToken: req.MFAToken})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, ceremonyResponse{CeremonyID: id, Options: assertion.Response})
}

type mfaFinishRequest struct {
	MFAToken   string          `json:"mfaToken"`
	CeremonyID string          `json:"ceremonyId"`
	Credential json.RawMessage `json:"credential"`
}

func (s *Service) handleMFAFinish(c *echo.Context) error {
	var req mfaFinishRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	cer, err := s.takeCeremony(c, req.CeremonyID, kindMFA)
	if err != nil {
		return err
	}
	if cer.mfaToken != req.MFAToken {
		return errCeremony
	}
	ch, err := s.auth.LookupMFA(req.MFAToken)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	w, stored, cred, err := s.verifyAssertion(c, cer, ch.UserID, req.Credential, protocol.VerificationPreferred)
	if err != nil {
		s.auth.MFAFailed(req.MFAToken)
		if w != nil {
			s.d.Audit.LogUser(c, w.u, "auth.login.failed", "", map[string]string{"username": w.u.Username, "reason": "bad passkey"})
		}
		return err
	}
	if err := s.saveAfterAssertion(ctx, stored, cred); err != nil {
		s.auth.MFAFailed(req.MFAToken)
		return err
	}
	u, err := s.auth.FinishMFA(c, req.MFAToken, "webauthn")
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, userResponse{User: u})
}

// ---- re-verification (lock screen, sensitive changes) ---------------------------------------------------------------

func (s *Service) handleVerifyBegin(c *echo.Context) error {
	rp, err := s.relyingParty(c)
	if err != nil {
		return err
	}
	me := httpx.UserFrom(c)
	w, err := s.loadUser(c.Request().Context(), me.ID, rp.id, false)
	if err != nil {
		return err
	}
	if len(w.creds) == 0 {
		return httpx.NewError(http.StatusConflict, "webauthn_no_credentials", "you have no passkey for this address")
	}
	wa, err := rp.webauthn(protocol.VerificationRequired)
	if err != nil {
		return httpx.Internal(err)
	}
	assertion, session, err := wa.BeginLogin(w)
	if err != nil {
		return httpx.Internal(err)
	}
	id, err := s.putCeremony(&ceremony{kind: kindVerify, session: *session, userID: me.ID, rpID: rp.id, origin: rp.origin})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, ceremonyResponse{CeremonyID: id, Options: assertion.Response})
}

type verifyFinishRequest struct {
	CeremonyID string          `json:"ceremonyId"`
	Credential json.RawMessage `json:"credential"`
}

func (s *Service) handleVerifyFinish(c *echo.Context) error {
	var req verifyFinishRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	cer, err := s.takeCeremony(c, req.CeremonyID, kindVerify)
	if err != nil {
		return err
	}
	me := httpx.UserFrom(c)
	if cer.userID != me.ID {
		return errCeremony
	}
	_, stored, cred, err := s.verifyAssertion(c, cer, me.ID, req.Credential, protocol.VerificationRequired)
	if err != nil {
		s.d.Audit.Log(c, "auth.reauth.failed", me.ID, map[string]string{"method": "passkey"})
		return err
	}
	if err := s.saveAfterAssertion(c.Request().Context(), stored, cred); err != nil {
		return err
	}
	s.auth.MarkReauthenticated(c)
	s.d.Audit.Log(c, "auth.reauth", me.ID, map[string]string{"method": "passkey", "passkey": stored.Name})
	return httpx.OK(c)
}

// verifyAssertion validates an assertion of userID's passkeys for a non-discoverable ceremony.
func (s *Service) verifyAssertion(c *echo.Context, cer *ceremony, userID string, raw json.RawMessage, uv protocol.UserVerificationRequirement) (*waUser, *storedCredential, *gwa.Credential, error) {
	ctx := c.Request().Context()
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, nil, nil, s.ceremonyError(cer.kind, err)
	}
	w, err := s.loadUser(ctx, userID, cer.rpID, false)
	if err != nil {
		return nil, nil, nil, err
	}
	wa, err := relyingParty{id: cer.rpID, origin: cer.origin}.webauthn(uv)
	if err != nil {
		return w, nil, nil, httpx.Internal(err)
	}
	cred, err := wa.ValidateLogin(w, cer.session, parsed)
	if err != nil {
		return w, nil, nil, s.ceremonyError(cer.kind, err)
	}
	stored := findStored(w, cred.ID)
	if stored == nil {
		return w, nil, nil, s.ceremonyError(cer.kind, errors.New("unknown passkey"))
	}
	return w, stored, cred, nil
}

// ---- admin configuration ----------------------------------------------------------------------------------------------

func (s *Service) handleGetConfig(c *echo.Context) error {
	cfg := s.config(c.Request().Context())
	origin, host := s.requestOrigin(c)
	return c.JSON(http.StatusOK, map[string]any{"rpId": cfg.RPID, "origins": cfg.Origins, "requestOrigin": origin, "requestHost": host})
}

func (s *Service) handlePutConfig(c *echo.Context) error {
	var cfg Config
	if err := httpx.Bind(c, &cfg); err != nil {
		return err
	}
	if err := s.auth.RequireAdminSudo(c); err != nil {
		return err
	}
	cfg.RPID = strings.ToLower(strings.TrimSpace(cfg.RPID))
	origins := []string{}
	for _, o := range cfg.Origins {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "" {
			continue
		}
		if !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://") {
			return httpx.BadRequest("origins must start with https:// (or http:// for localhost)")
		}
		origins = append(origins, strings.ToLower(o))
	}
	cfg.Origins = origins
	if cfg.RPID != "" {
		if err := protocol.ValidateRPID(cfg.RPID); err != nil {
			return httpx.BadRequest("rpId must be a domain name such as astraterm.example.com")
		}
		if len(cfg.Origins) == 0 {
			return httpx.BadRequest("list at least one origin, e.g. https://" + cfg.RPID)
		}
		for _, o := range cfg.Origins {
			host := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://"), ":", 2)[0]
			if host != cfg.RPID && !strings.HasSuffix(host, "."+cfg.RPID) {
				return httpx.BadRequest("origin " + o + " is not on " + cfg.RPID + " or one of its subdomains")
			}
		}
		origin, _ := s.requestOrigin(c)
		if !slices.Contains(cfg.Origins, origin) {
			return httpx.NewError(http.StatusConflict, "self_lockout", "include the address you are using ("+origin+") in the origins")
		}
	} else {
		cfg.Origins = []string{}
	}
	if err := s.d.Store.Settings.SetJSON(c.Request().Context(), configScope, configKey, cfg); err != nil {
		return err
	}
	s.d.Audit.Log(c, "admin.passkeys.config", "", cfg)
	return s.handleGetConfig(c)
}
