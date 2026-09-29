package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/config"
)

func TestListenURLs(t *testing.T) {
	addrs := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("::1"),
		netip.MustParseAddr("fe80::1"),         // link-local: skipped
		netip.MustParseAddr("169.254.10.1"),    // link-local: skipped
		netip.MustParseAddr("2001:db8::5"),     // global IPv6
		netip.MustParseAddr("203.0.113.7"),     // public IPv4
		netip.MustParseAddr("192.168.1.20"),    // private IPv4: primary
		netip.MustParseAddr("::ffff:10.0.0.9"), // mapped: unmapped to 10.0.0.9
		netip.MustParseAddr("192.168.1.20"),    // duplicate
		netip.MustParseAddr("fd00::20"),        // unique-local IPv6
		netip.MustParseAddr("ff02::1"),         // multicast: skipped
		netip.MustParseAddr("0.0.0.0"),         // unspecified: skipped
	}
	join := func(p string, o []string) string { return p + " | " + strings.Join(o, " ") }
	for _, c := range []struct {
		name, scheme, bound, configured, hostname string
		addrs                                     []netip.Addr
		want                                      string
	}{
		{"loopback", "http", "127.0.0.1:7822", "127.0.0.1:7822", "box", addrs, "http://127.0.0.1:7822/ | "},
		{"localhost keeps its name", "http", "127.0.0.1:7822", "localhost:7822", "box", addrs, "http://localhost:7822/ | "},
		{"ipv6 loopback", "http", "[::1]:7822", "[::1]:7822", "box", addrs, "http://[::1]:7822/ | "},
		{"specific address", "https", "10.0.0.9:443", "10.0.0.9:443", "box", addrs, "https://10.0.0.9:443/ | "},
		// Go reports a 0.0.0.0 listener as [::]; the banner must not turn that into [::1].
		{"0.0.0.0 lists IPv4 only", "https", "[::]:7822", "0.0.0.0:7822", "Box", addrs,
			"https://192.168.1.20:7822/ | https://box:7822/ https://10.0.0.9:7822/ https://203.0.113.7:7822/ https://127.0.0.1:7822/"},
		{"empty host lists IPv6 too", "https", "[::]:7822", ":7822", "box", addrs,
			"https://192.168.1.20:7822/ | https://box:7822/ https://10.0.0.9:7822/ https://203.0.113.7:7822/ " +
				"https://[fd00::20]:7822/ https://[2001:db8::5]:7822/ https://127.0.0.1:7822/ https://[::1]:7822/"},
		{"no network: loopback", "http", "[::]:7822", "0.0.0.0:7822", "box", nil, "http://127.0.0.1:7822/ | "},
		{"localhost hostname is not listed", "http", "0.0.0.0:7822", "0.0.0.0:7822", "localhost",
			[]netip.Addr{netip.MustParseAddr("10.1.1.1")}, "http://10.1.1.1:7822/ | http://127.0.0.1:7822/"},
	} {
		if got := join(listenURLs(c.scheme, c.bound, c.configured, c.addrs, c.hostname)); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The start-up banner of a server listening on all interfaces shows a reachable URL with the setup link, lists the
// other addresses and never shows the IPv6 loopback Go reports for a wildcard IPv4 listener.
func TestRunBannerWildcard(t *testing.T) {
	cfg, err := config.Load([]string{"--data-dir", t.TempDir(), "--mode", "server", "--listen", "0.0.0.0:0",
		"--insecure-http", "--no-open", "--guacd", "off"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &out) }()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), "Data dir:") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no banner: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	banner := out.String()
	if strings.Contains(banner, "[::1]") || strings.Contains(banner, "[::]") {
		t.Fatalf("banner shows an IPv6 wildcard/loopback URL for 0.0.0.0:\n%s", banner)
	}
	lines := strings.Split(banner, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "  URL:      http://") || !strings.Contains(lines[1], "/?setup=") {
		t.Fatalf("URL line:\n%s", banner)
	}
	if strings.Count(banner, "?setup=") != 1 {
		t.Fatalf("the setup token must appear once:\n%s", banner)
	}
	if !strings.Contains(banner, "http://127.0.0.1:") {
		t.Fatalf("loopback URL missing:\n%s", banner)
	}
}
