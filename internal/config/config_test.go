package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAndFlags(t *testing.T) {
	dir := t.TempDir()
	c, err := Load([]string{"--data-dir", dir})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.Mode != ModeDesktop || !c.Open || c.DetachedSessionTTL != 24*time.Hour ||
		c.ScrollbackBytes != 4<<20 || c.Guacd != DefaultGuacd || !c.ListenIsLoopback() {
		t.Fatalf("defaults: %+v", c)
	}
	for _, sub := range []string{"recordings", "logs", "tmp"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("subdir %s: %v", sub, err)
		}
	}

	c, err = Load([]string{"--data-dir", dir, "--mode", "server", "--scrollback-bytes", "8MiB", "--detached-ttl", "0",
		"--trusted-proxies", "10.0.0.0/8, 192.168.1.1", "--guacd", "off"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Open || c.ScrollbackBytes != 8<<20 || c.DetachedSessionTTL != 0 || len(c.TrustedProxies) != 2 || c.Guacd != "" {
		t.Fatalf("flags: %+v", c)
	}
	if c, _ = Load([]string{"--data-dir", dir, "--open", "--mode", "server"}); !c.Open {
		t.Fatal("--open ignored in server mode")
	}
	if c, _ = Load([]string{"--data-dir", dir, "--no-open"}); c.Open {
		t.Fatal("--no-open ignored")
	}

	t.Setenv("ASTRATERM_LISTEN", "127.0.0.1:9999")
	if c, _ = Load([]string{"--data-dir", dir}); c.Listen != "127.0.0.1:9999" {
		t.Fatalf("env: %s", c.Listen)
	}
	if c, _ = Load([]string{"--data-dir", dir, "--listen", "localhost:1"}); c.Listen != "localhost:1" || !c.ListenIsLoopback() {
		t.Fatalf("flag over env: %s", c.Listen)
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	bad := [][]string{
		{"--mode", "kiosk"},
		{"--listen", "0.0.0.0:7822"}, // non-loopback without TLS
		{"--listen", "nope"},
		{"--tls-cert", "a.pem"},
		{"--log-level", "chatty"},
		{"--scrollback-bytes", "1k"},
		{"--trusted-proxies", "not-an-ip"},
		{"--detached-ttl", "soon"},
		{"--allowed-hosts", "https://astraterm.example.com"},
		{"--allowed-hosts", "*.example.com"},
		{"--allowed-hosts", "bad host!"},
	}
	for _, args := range bad {
		if _, err := Load(append([]string{"--data-dir", dir}, args...)); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
	for _, args := range [][]string{
		{"--listen", "0.0.0.0:7822", "--insecure-http"},
		{"--listen", "0.0.0.0:7822", "--tls-self-signed"},
		{"--listen", "[::1]:7822"},
	} {
		if _, err := Load(append([]string{"--data-dir", dir}, args...)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestAllowedHosts(t *testing.T) {
	dir := t.TempDir()
	c, err := Load([]string{"--data-dir", dir})
	if err != nil || len(c.AllowedHosts) != 0 {
		t.Fatalf("default: %v %v", c.AllowedHosts, err)
	}
	c, err = Load([]string{"--data-dir", dir, "--allowed-hosts", "AstraTerm.Example.com:443, [fd00::1]:8443 10.0.0.5;astraterm.example.com."})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.AllowedHosts, ","); got != "astraterm.example.com,fd00::1,10.0.0.5" {
		t.Fatalf("flag: %q", got)
	}
	t.Setenv("ASTRATERM_ALLOWED_HOSTS", "proxy.lan")
	if c, _ = Load([]string{"--data-dir", dir}); len(c.AllowedHosts) != 1 || c.AllowedHosts[0] != "proxy.lan" {
		t.Fatalf("env: %v", c.AllowedHosts)
	}
	if c, _ = Load([]string{"--data-dir", dir, "--allowed-hosts", ""}); len(c.AllowedHosts) != 0 {
		t.Fatalf("flag over env: %v", c.AllowedHosts)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"4194304": 4 << 20, "4MiB": 4 << 20, "4m": 4 << 20, "512k": 512 << 10, "1GB": 1 << 30, "1.5KiB": 1536} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Fatalf("%s: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "4 parsecs", "-1"} {
		if _, err := ParseSize(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
}

func TestResolveDataDir(t *testing.T) {
	dir, portable, err := ResolveDataDir("rel/dir", false)
	if err != nil || !filepath.IsAbs(dir) || portable {
		t.Fatalf("flag dir: %s %v %v", dir, portable, err)
	}
	dir, portable, err = ResolveDataDir("", true)
	if err != nil || filepath.Base(dir) != PortableDirName || !portable {
		t.Fatalf("portable: %s %v %v", dir, portable, err)
	}
}
