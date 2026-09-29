package auth_test

import (
	"testing"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/server/servertest"
)

// In server mode (and on non-loopback binds) the first administrator can only be created with the one-time setup
// token from the startup banner, so a stranger cannot claim a freshly started instance.
func TestSetupTokenRequiredInServerMode(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	tok := env.Server.Auth.SetupToken()
	if len(tok) < 32 {
		t.Fatalf("server mode must generate a setup token, got %q", tok)
	}
	c := env.Client()
	var st struct {
		SetupRequired      bool `json:"setupRequired"`
		SetupTokenRequired bool `json:"setupTokenRequired"`
	}
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.SetupRequired || !st.SetupTokenRequired {
		t.Fatalf("state = %+v, want setup and setup token required", st)
	}

	body := map[string]string{"username": "root", "password": adminPass}
	if code, ec := c.ErrorCode("POST", "/api/auth/setup", body); code != 403 || ec != "setup_token_required" {
		t.Fatalf("setup without token: %d %s", code, ec)
	}
	body["setupToken"] = tok[:len(tok)-1] + "x"
	if tok[len(tok)-1] == 'x' {
		body["setupToken"] = tok[:len(tok)-1] + "y"
	}
	if code, ec := c.ErrorCode("POST", "/api/auth/setup", body); code != 403 || ec != "setup_token_required" {
		t.Fatalf("setup with a wrong token: %d %s", code, ec)
	}
	body["setupToken"] = tok
	c.MustJSON("POST", "/api/auth/setup", body, nil)

	if env.Server.Auth.SetupToken() != "" {
		t.Fatal("setup token must be cleared once setup has completed")
	}
	st.SetupRequired, st.SetupTokenRequired = false, false // omitted when false: decode into a clean value
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if st.SetupRequired || st.SetupTokenRequired {
		t.Fatalf("state after setup = %+v", st)
	}
	if code, _ := env.Client().ErrorCode("POST", "/api/auth/setup", body); code != 409 {
		t.Fatalf("second setup with the old token: %d, want 409", code)
	}
}

// Desktop mode on loopback keeps the token-free setup (only local processes can reach it; the browser is opened
// with the launch link).
func TestNoSetupTokenInDesktopLoopback(t *testing.T) {
	env := servertest.New(t)
	if tok := env.Server.Auth.SetupToken(); tok != "" {
		t.Fatalf("desktop loopback must not require a setup token, got %q", tok)
	}
	env.Client().MustJSON("POST", "/api/auth/setup", map[string]string{"username": "admin", "password": adminPass}, nil)
}

// A desktop instance bound to a non-loopback address is reachable by others: setup needs the token as well.
func TestSetupTokenOnNonLoopbackDesktop(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Listen = "0.0.0.0:7822"; c.InsecureHTTP = true })
	if env.Server.Auth.SetupToken() == "" {
		t.Fatal("non-loopback desktop bind must require a setup token")
	}
}
