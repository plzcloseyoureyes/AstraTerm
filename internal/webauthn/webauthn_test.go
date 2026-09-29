package webauthn_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

const adminPass = "correct horse battery"

var b64 = base64.RawURLEncoding

// vAuthenticator is a software FIDO2 authenticator (ES256, "none" attestation) used to drive real ceremonies.
type vAuthenticator struct {
	t          *testing.T
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	origin     string
	count      uint32
	backup     bool
}

func newAuthenticator(t *testing.T, origin string) *vAuthenticator {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	return &vAuthenticator{t: t, key: k, credID: id, origin: origin}
}

type options struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	RPID string `json:"rpId"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	AllowCredentials []struct {
		ID string `json:"id"`
	} `json:"allowCredentials"`
	ExcludeCredentials []struct {
		ID string `json:"id"`
	} `json:"excludeCredentials"`
}

func (a *vAuthenticator) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return b
}

func (a *vAuthenticator) authData(rpID string, flags byte, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, h[:]...)
	if a.backup {
		flags |= 0x08 | 0x10
	}
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.count)
	return append(out, attested...)
}

func (a *vAuthenticator) register(raw json.RawMessage) json.RawMessage {
	var o options
	if err := json.Unmarshal(raw, &o); err != nil {
		a.t.Fatalf("creation options: %v", err)
	}
	var err error
	if a.userHandle, err = b64.DecodeString(o.User.ID); err != nil {
		a.t.Fatalf("user id: %v", err)
	}
	pub := a.key.PublicKey
	x, y := make([]byte, 32), make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	cose, _ := webauthncbor.Marshal(map[int64]any{1: int64(2), 3: int64(-7), -1: int64(1), -2: x, -3: y})
	attested := make([]byte, 16) // zero AAGUID
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(a.credID)))
	attested = append(attested, a.credID...)
	attested = append(attested, cose...)
	ad := a.authData(o.RP.ID, 0x01|0x04|0x40, attested)
	att, _ := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	resp, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key",
		"response": map[string]any{"attestationObject": b64.EncodeToString(att),
			"clientDataJSON": b64.EncodeToString(a.clientData("webauthn.create", o.Challenge)), "transports": []string{"internal"}},
		"clientExtensionResults": map[string]any{"credProps": map[string]any{"rk": true}},
	})
	return resp
}

func (a *vAuthenticator) assert(raw json.RawMessage, key *ecdsa.PrivateKey) json.RawMessage {
	var o options
	if err := json.Unmarshal(raw, &o); err != nil {
		a.t.Fatalf("request options: %v", err)
	}
	if key == nil {
		key = a.key
	}
	a.count++
	ad := a.authData(o.RPID, 0x01|0x04, nil)
	cd := a.clientData("webauthn.get", o.Challenge)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		a.t.Fatal(err)
	}
	resp, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key",
		"response": map[string]any{"authenticatorData": b64.EncodeToString(ad), "clientDataJSON": b64.EncodeToString(cd),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(a.userHandle)},
		"clientExtensionResults": map[string]any{},
	})
	return resp
}

type ceremony struct {
	CeremonyID string          `json:"ceremonyId"`
	Options    json.RawMessage `json:"options"`
}

// newEnv starts a server reached as http://localhost:<port> (passkeys need a host name, not an IP).
func newEnv(t *testing.T) (*servertest.Env, string) {
	env := servertest.New(t)
	env.HTTP.URL = strings.Replace(env.HTTP.URL, "127.0.0.1", "localhost", 1)
	return env, env.HTTP.URL
}

func registerPasskey(t *testing.T, c *servertest.Client, a *vAuthenticator, name string) map[string]any {
	t.Helper()
	var begin ceremony
	c.MustJSON("POST", "/api/auth/webauthn/register/begin", map[string]any{}, &begin)
	var cred map[string]any
	if st := c.JSON("POST", "/api/auth/webauthn/register/finish", map[string]any{
		"ceremonyId": begin.CeremonyID, "name": name, "credential": a.register(begin.Options)}, &cred); st != 201 {
		_, body := c.Do("GET", "/api/auth/state", nil)
		t.Fatalf("register finish: %d %s", st, body)
	}
	return cred
}

func TestPasskeyLifecycle(t *testing.T) {
	env, origin := newEnv(t)
	admin := env.Setup("admin", adminPass)

	var status struct {
		Available bool   `json:"available"`
		RPID      string `json:"rpId"`
	}
	env.Client().MustJSON("GET", "/api/auth/webauthn/status", nil, &status)
	if !status.Available || status.RPID != "localhost" {
		t.Fatalf("status: %+v", status)
	}

	a := newAuthenticator(t, origin)
	cred := registerPasskey(t, admin, a, "  My   laptop ")
	if cred["name"] != "My laptop" || cred["discoverable"] != true || cred["rpId"] != "localhost" {
		t.Fatalf("credential: %v", cred)
	}
	// The same authenticator cannot be registered twice: it is excluded, and a replayed ceremony is refused.
	var begin ceremony
	admin.MustJSON("POST", "/api/auth/webauthn/register/begin", map[string]any{}, &begin)
	var opts options
	json.Unmarshal(begin.Options, &opts)
	if len(opts.ExcludeCredentials) != 1 {
		t.Fatalf("exclude list: %s", begin.Options)
	}

	// Passwordless sign-in (discoverable credential, user verification).
	anon := env.Client()
	var lb ceremony
	anon.MustJSON("POST", "/api/auth/webauthn/login/begin", map[string]any{}, &lb)
	assertion := a.assert(lb.Options, nil)
	var out struct {
		User model.User `json:"user"`
	}
	anon.MustJSON("POST", "/api/auth/webauthn/login/finish", map[string]any{"ceremonyId": lb.CeremonyID, "credential": assertion, "remember": true}, &out)
	if out.User.Username != "admin" {
		t.Fatalf("passkey login user: %+v", out.User)
	}
	var st struct {
		Authenticated bool `json:"authenticated"`
	}
	anon.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.Authenticated {
		t.Fatal("not signed in after passkey login")
	}
	// Ceremonies are single use.
	if code, ec := env.Client().ErrorCode("POST", "/api/auth/webauthn/login/finish", map[string]any{"ceremonyId": lb.CeremonyID, "credential": assertion}); code != 400 || ec != "webauthn_expired" {
		t.Fatalf("replayed ceremony: %d %s", code, ec)
	}

	// A signature from another key is rejected.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	env.Client().MustJSON("POST", "/api/auth/webauthn/login/begin", map[string]any{}, &lb)
	if code, ec := env.Client().ErrorCode("POST", "/api/auth/webauthn/login/finish", map[string]any{"ceremonyId": lb.CeremonyID, "credential": a.assert(lb.Options, other)}); code != 400 || ec != "webauthn_failed" {
		t.Fatalf("forged assertion: %d %s", code, ec)
	}

	// With a passkey registered, a password login needs a second factor (passkey).
	c := env.Client()
	resp, body := c.Do("POST", "/api/auth/login", map[string]any{"username": "admin", "password": adminPass})
	var chal struct {
		Code     string   `json:"code"`
		Methods  []string `json:"methods"`
		MFAToken string   `json:"mfaToken"`
	}
	json.Unmarshal(body, &chal)
	if resp.StatusCode != 401 || chal.Code != "mfa_required" || len(chal.Methods) != 1 || chal.Methods[0] != "webauthn" || chal.MFAToken == "" {
		t.Fatalf("mfa challenge: %d %s", resp.StatusCode, body)
	}
	var mb ceremony
	c.MustJSON("POST", "/api/auth/webauthn/mfa/begin", map[string]any{"mfaToken": chal.MFAToken}, &mb)
	c.MustJSON("POST", "/api/auth/webauthn/mfa/finish", map[string]any{"mfaToken": chal.MFAToken, "ceremonyId": mb.CeremonyID, "credential": a.assert(mb.Options, nil)}, &out)
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.Authenticated {
		t.Fatal("not signed in after password + passkey")
	}
	// The MFA token was consumed.
	if code, ec := env.Client().ErrorCode("POST", "/api/auth/webauthn/mfa/begin", map[string]any{"mfaToken": chal.MFAToken}); code != 401 || ec != "mfa_expired" {
		t.Fatalf("reused mfa token: %d %s", code, ec)
	}

	// Lock-screen re-verification.
	var vb ceremony
	c.MustJSON("POST", "/api/auth/webauthn/verify/begin", nil, &vb)
	c.MustJSON("POST", "/api/auth/webauthn/verify/finish", map[string]any{"ceremonyId": vb.CeremonyID, "credential": a.assert(vb.Options, nil)}, nil)

	// A counter that goes backwards (cloned authenticator) is refused.
	a.count = 0
	c.MustJSON("POST", "/api/auth/webauthn/verify/begin", nil, &vb)
	if code, ec := c.ErrorCode("POST", "/api/auth/webauthn/verify/finish", map[string]any{"ceremonyId": vb.CeremonyID, "credential": a.assert(vb.Options, nil)}); code != 403 || ec != "webauthn_clone_warning" {
		t.Fatalf("cloned authenticator: %d %s", code, ec)
	}
	a.count = 100

	// Rename, list, admin view, reset.
	id := cred["id"].(string)
	var renamed map[string]any
	admin.MustJSON("PATCH", "/api/auth/webauthn/credentials/"+id, map[string]string{"name": "YubiKey"}, &renamed)
	if renamed["name"] != "YubiKey" {
		t.Fatalf("rename: %v", renamed)
	}
	var users []struct {
		Username string `json:"username"`
		Passkeys int    `json:"passkeys"`
		ID       string `json:"id"`
	}
	admin.MustJSON("GET", "/api/admin/users", nil, &users)
	if len(users) != 1 || users[0].Passkeys != 1 {
		t.Fatalf("admin users: %+v", users)
	}
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	if code, _ := bob.ErrorCode("PATCH", "/api/auth/webauthn/credentials/"+id, map[string]string{"name": "mine"}); code != 404 {
		t.Fatalf("foreign passkey rename: %d", code)
	}
	if code, _ := bob.ErrorCode("DELETE", "/api/auth/webauthn/credentials/"+id, nil); code != 404 {
		t.Fatalf("foreign passkey delete: %d", code)
	}
	var reset map[string]any
	admin.MustJSON("POST", "/api/admin/users/"+users[0].ID+"/reset-mfa", map[string]any{"totp": false}, &reset)
	if reset["passkeysRemoved"] != float64(1) {
		t.Fatalf("reset: %v", reset)
	}
	var list []map[string]any
	admin.MustJSON("GET", "/api/auth/webauthn/credentials", nil, &list)
	if len(list) != 0 {
		t.Fatalf("passkeys after reset: %v", list)
	}
	// Without passkeys the password alone signs in again.
	env.Login("admin", adminPass)

	// Registration needs a fresh sign-in or the password: a wrong password is refused.
	if code, ec := admin.ErrorCode("POST", "/api/auth/webauthn/register/begin", map[string]string{"password": "wrong password"}); code != 403 || ec != "invalid_password" {
		t.Fatalf("register with wrong password: %d %s", code, ec)
	}
}

func TestPasskeysNeedHostName(t *testing.T) {
	env := servertest.New(t) // http://127.0.0.1:port
	admin := env.Setup("admin", adminPass)
	var status struct {
		Available bool   `json:"available"`
		Code      string `json:"code"`
	}
	env.Client().MustJSON("GET", "/api/auth/webauthn/status", nil, &status)
	if status.Available || status.Code != "webauthn_unavailable" {
		t.Fatalf("status on an IP: %+v", status)
	}
	if code, ec := admin.ErrorCode("POST", "/api/auth/webauthn/register/begin", nil); code != 409 || ec != "webauthn_unavailable" {
		t.Fatalf("register on an IP: %d %s", code, ec)
	}
}

func TestPinnedRelyingParty(t *testing.T) {
	env, origin := newEnv(t)
	admin := env.Setup("admin", adminPass)
	if code, ec := admin.ErrorCode("PUT", "/api/admin/webauthn/config", map[string]any{"rpId": "example.com", "origins": []string{"https://nexterm.example.com"}}); code != 409 || ec != "self_lockout" {
		t.Fatalf("pin excluding own origin: %d %s", code, ec)
	}
	if code, _ := admin.ErrorCode("PUT", "/api/admin/webauthn/config", map[string]any{"rpId": "localhost", "origins": []string{"https://evil.test"}}); code != 400 {
		t.Fatalf("origin outside rp: %d", code)
	}
	admin.MustJSON("PUT", "/api/admin/webauthn/config", map[string]any{"rpId": "localhost", "origins": []string{origin}}, nil)
	a := newAuthenticator(t, origin)
	cred := registerPasskey(t, admin, a, "")
	if cred["name"] != "Passkey" {
		t.Fatalf("default name: %v", cred)
	}
}
