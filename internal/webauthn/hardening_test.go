package webauthn_test

import (
	"testing"
)

// The last passkey cannot be removed while it is the only second factor the login policy requires, and a stale
// session must re-verify before removing (or adding) one.
func TestLastPasskeyGuards(t *testing.T) {
	env, origin := newEnv(t)
	admin := env.Setup("admin", adminPass)
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	cred := registerPasskey(t, bob, newAuthenticator(t, origin), "key")
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"requireMfa": "all"}, nil)

	if code, ec := bob.ErrorCode("DELETE", "/api/auth/webauthn/credentials/"+cred["id"].(string), nil); code != 409 || ec != "mfa_required" {
		t.Fatalf("removing the only required factor: %d %s", code, ec)
	}
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"requireMfa": "off"}, nil)

	if _, err := env.Server.Deps.Store.DB.Exec(`UPDATE auth_sessions SET created_at = created_at - 3600000`); err != nil {
		t.Fatal(err)
	}
	if code, ec := bob.ErrorCode("DELETE", "/api/auth/webauthn/credentials/"+cred["id"].(string), nil); code != 403 || ec != "reauth_required" {
		t.Fatalf("stale delete: %d %s", code, ec)
	}
	if code, ec := bob.ErrorCode("POST", "/api/auth/webauthn/register/begin", map[string]any{}); code != 403 || ec != "reauth_required" {
		t.Fatalf("stale register: %d %s", code, ec)
	}
	bob.MustJSON("DELETE", "/api/auth/webauthn/credentials/"+cred["id"].(string), map[string]string{"password": "bob password"}, nil)
}
