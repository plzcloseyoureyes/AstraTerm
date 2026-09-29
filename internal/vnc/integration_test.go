package vnc_test

import (
	"encoding/binary"
	"io"
	"os"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/vnc"
	"github.com/nexterm/nexterm/internal/vnc/vnctest"
)

// Integration tests against the shared Docker test environment (NEXTERM_TESTENV=1): TigerVNC (VeNCrypt TLSVnc +
// VncAuth, password "vncpassword") at 127.0.0.1:22059, and the same server reached as "vnc:5901" through the ssh1
// gateway (127.0.0.1:22022, test/test) with options.sshTunnelVia.

func testenv(t *testing.T) {
	if os.Getenv("NEXTERM_TESTENV") != "1" {
		t.Skip("set NEXTERM_TESTENV=1 to run against the shared test environment")
	}
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// requestPixels asks for a small raw framebuffer region and checks that a FramebufferUpdate arrives.
func requestPixels(t *testing.T, v *vncConn) {
	t.Helper()
	setEnc := []byte{2, 0, 0, 1, 0, 0, 0, 0}      // SetEncodings: Raw
	req := []byte{3, 0, 0, 0, 0, 0, 0, 16, 0, 16} // FramebufferUpdateRequest 16x16, not incremental
	if _, err := v.nc.Write(append(setEnc, req...)); err != nil {
		t.Fatal(err)
	}
	_ = v.nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(v.nc, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[0] != 0 || binary.BigEndian.Uint16(hdr[2:]) == 0 {
		t.Fatalf("expected a FramebufferUpdate, got %x", hdr)
	}
}

func TestTestenvTigerVNCDirect(t *testing.T) {
	testenv(t)
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	addr := envDefault("NEXTERM_TEST_VNC", "127.0.0.1:22059")
	connID := createVNCConnection(t, admin, "tigervnc", addr,
		map[string]string{"vncPassword": envDefault("NEXTERM_TEST_VNC_PASSWORD", "vncpassword")}, nil)
	s := openSession(t, admin, map[string]any{"connectionId": connID})
	v := dialVNC(t, admin, s.ID)
	init, err := vnctest.Viewer(v.nc)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("desktop %dx%d %q", binary.BigEndian.Uint16(init[0:]), binary.BigEndian.Uint16(init[2:]), init[24:])
	requestPixels(t, v)
	var info vnc.Info
	admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
	t.Logf("info %+v tls %+v", info, info.TLS)
	if !info.Connected || !info.Encrypted || info.TLS == nil || !info.TLS.Anonymous || info.Security != "VeNCrypt TLSVnc" {
		t.Fatalf("expected VeNCrypt TLSVnc over anonymous TLS, got %+v", info)
	}
}

func TestTestenvTigerVNCViaSSHGateway(t *testing.T) {
	testenv(t)
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	pa := answerPrompts(t, admin, func(p model.Prompt) (bool, []string, bool) {
		return p.Kind == model.PromptHostKey, nil, false // trust the gateway's host key once
	})
	sshID := h.connection(admin, &model.Connection{Name: "ssh1", Protocol: model.ProtoSSH, Host: "127.0.0.1",
		Port: 22022, Username: "test", AuthMethod: model.AuthPassword}, map[string]string{"password": "test"})
	connID := createVNCConnection(t, admin, "vnc via ssh1", "vnc:5901",
		map[string]string{"vncPassword": envDefault("NEXTERM_TEST_VNC_PASSWORD", "vncpassword")},
		map[string]any{"sshTunnelVia": sshID})
	s := openSession(t, admin, map[string]any{"connectionId": connID})
	v := dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatalf("%v (prompts: %+v)", err, pa.got())
	}
	requestPixels(t, v)
	var info vnc.Info
	admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
	if !info.Connected || info.Route != "SSH gateway" || !info.Encrypted {
		t.Fatalf("info %+v", info)
	}
}
