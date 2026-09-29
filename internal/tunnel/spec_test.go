package tunnel

import (
	"runtime"
	"strings"
	"testing"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
)

func TestDefNormalize(t *testing.T) {
	abs := "/tmp/x.sock"
	if runtime.GOOS == "windows" {
		abs = `C:\tmp\x.sock`
	}
	cases := []struct {
		name string
		in   def
		want def
		err  string
	}{
		{"local defaults", def{Type: "local", DestPort: 80}, def{Type: "local", BindHost: "127.0.0.1", DestHost: "localhost", DestPort: 80}, ""},
		{"type case", def{Type: " LOCAL ", DestHost: "web", DestPort: 80}, def{Type: "local", BindHost: "127.0.0.1", DestHost: "web", DestPort: 80}, ""},
		{"missing type", def{DestPort: 80}, def{}, "type is required"},
		{"bad type", def{Type: "udp"}, def{}, "type must be"},
		{"wildcard", def{Type: "local", BindHost: "*", BindPort: 8080, DestHost: "h", DestPort: 1}, def{Type: "local", BindHost: "*", BindPort: 8080, DestHost: "h", DestPort: 1}, ""},
		{"ipv6 brackets", def{Type: "local", BindHost: "[::1]", DestHost: "[fe80::1]", DestPort: 22}, def{Type: "local", BindHost: "::1", DestHost: "fe80::1", DestPort: 22}, ""},
		{"local hostname bind", def{Type: "local", BindHost: "myhost", DestPort: 1}, def{}, "bind address must be"},
		{"remote hostname bind", def{Type: "remote", BindHost: "gw.example.com", DestPort: 1}, def{Type: "remote", BindHost: "gw.example.com", DestHost: "localhost", DestPort: 1}, ""},
		{"bad bind port", def{Type: "local", BindPort: 70000, DestPort: 1}, def{}, "bind port"},
		{"bad dest port", def{Type: "local", DestHost: "x"}, def{}, "destination port"},
		{"bad dest host", def{Type: "local", DestHost: "a b", DestPort: 1}, def{}, "invalid host"},
		{"dynamic drops dest", def{Type: "dynamic", BindPort: 1080, DestHost: "x", DestPort: 5}, def{Type: "dynamic", BindHost: "127.0.0.1", BindPort: 1080}, ""},
		{"reverse only for dynamic", def{Type: "local", Reverse: true, DestPort: 1}, def{Type: "local", BindHost: "127.0.0.1", DestHost: "localhost", DestPort: 1}, ""},
		{"dest socket", def{Type: "local", DestSocket: "/var/run/docker.sock", DestHost: "x", DestPort: 3}, def{Type: "local", BindHost: "127.0.0.1", DestSocket: "/var/run/docker.sock"}, ""},
		{"relative remote socket", def{Type: "local", DestSocket: "run/docker.sock"}, def{}, "absolute path on the SSH server"},
		{"local bind socket", def{Type: "local", BindSocket: abs, BindPort: 5, DestPort: 1}, def{Type: "local", BindSocket: abs, DestHost: "localhost", DestPort: 1}, ""},
		{"relative local socket", def{Type: "local", BindSocket: "x.sock", DestPort: 1}, def{}, "absolute path on the NexTerm host"},
		{"socket too long", def{Type: "remote", BindSocket: "/" + strings.Repeat("a", 200), DestPort: 1}, def{}, "at most"},
		{"control chars", def{Type: "remote", BindSocket: "/tmp/a\nb", DestPort: 1}, def{}, "invalid characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := c.in
			err := d.normalize()
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d != c.want {
				t.Fatalf("got %+v, want %+v", d, c.want)
			}
		})
	}
}

func TestNormalizeOptions(t *testing.T) {
	o := Options{SocksUsername: " bob ", AllowFrom: []string{"10.0.0.1", " 192.168.0.0/16 ", "", "10.1.2.3/8"}, IdleTimeoutSec: 3}
	if err := normalizeOptions(&o, kindDynamic); err != nil {
		t.Fatal(err)
	}
	if o.SocksUsername != "bob" || o.HTTPProxy == nil || !*o.HTTPProxy || o.AutoReconnect == nil || !*o.AutoReconnect {
		t.Fatalf("defaults: %+v", o)
	}
	if strings.Join(o.AllowFrom, ",") != "10.0.0.1,192.168.0.0/16,10.0.0.0/8" || o.IdleTimeoutSec != 10 {
		t.Fatalf("normalized: %+v", o)
	}
	o = Options{SocksUsername: "x", OnDemand: true, AllowFrom: []string{"1.2.3.4"}}
	if err := normalizeOptions(&o, kindRemote); err != nil {
		t.Fatal(err)
	}
	if o.SocksUsername != "" || o.OnDemand || o.AllowFrom != nil || o.HTTPProxy != nil {
		t.Fatalf("remote tunnels drop proxy/listener options: %+v", o)
	}
	for _, bad := range []Options{{AllowFrom: []string{"nope"}}, {Scheme: "ftp"}, {MaxConns: -1}, {IdleTimeoutSec: 90000}} {
		if err := normalizeOptions(&bad, kindLocal); err == nil {
			t.Fatalf("accepted invalid options %+v", bad)
		}
	}
}

func TestBuildSpecProxyExposure(t *testing.T) {
	d := def{Type: "dynamic", BindHost: "0.0.0.0", BindPort: 1080}
	if _, err := buildSpec(d, Options{}, nil); err == nil {
		t.Fatal("an unauthenticated proxy on all interfaces was accepted")
	}
	if _, err := buildSpec(d, Options{AllowFrom: []string{"10.0.0.0/8"}}, nil); err != nil {
		t.Fatalf("allow list: %v", err)
	}
	if _, err := buildSpec(d, Options{SocksUsername: "u"}, nil); err == nil {
		t.Fatal("username without password accepted")
	}
	sp, err := buildSpec(d, Options{SocksUsername: "u"}, map[string]string{secretSocksPassword: "p"})
	if err != nil || sp.socksUser != "u" || sp.socksPass != "p" || !sp.httpProxy || !sp.autoReconnect {
		t.Fatalf("spec = %+v, %v", sp, err)
	}
	if sp.label() != "SOCKS proxy on 0.0.0.0:1080" {
		t.Fatalf("label = %q", sp.label())
	}
}

func TestSpecLabelsAndAddresses(t *testing.T) {
	cases := []struct {
		sp          spec
		label       string
		lnet, laddr string
		rnet, raddr string
		dnet, daddr string
	}{
		{spec{kind: kindLocal, bindHost: "127.0.0.1", bindPort: 8080, destHost: "web", destPort: 80}, "127.0.0.1:8080 → web:80",
			"tcp", "127.0.0.1:8080", "tcp", "127.0.0.1:8080", "tcp", "web:80"},
		{spec{kind: kindLocal, bindHost: "*", bindPort: 0, destHost: "::1", destPort: 5432}, "*:auto → [::1]:5432",
			"tcp", ":0", "tcp", ":0", "tcp", "[::1]:5432"},
		{spec{kind: kindRemote, bindHost: "localhost", bindPort: 9000, destSocket: "/run/app.sock"}, "remote localhost:9000 → /run/app.sock",
			"tcp", "localhost:9000", "tcp", "localhost:9000", "unix", "/run/app.sock"},
		{spec{kind: kindRemoteDynamic, bindSocket: "/tmp/s"}, "remote SOCKS proxy on /tmp/s",
			"unix", "/tmp/s", "unix", "/tmp/s", "tcp", ":0"},
	}
	for _, c := range cases {
		if got := c.sp.label(); got != c.label {
			t.Errorf("label = %q, want %q", got, c.label)
		}
		if n, a := c.sp.localListenAddr(); n != c.lnet || a != c.laddr {
			t.Errorf("local listen = %s %s", n, a)
		}
		if n, a := c.sp.remoteListenAddr(); n != c.rnet || a != c.raddr {
			t.Errorf("remote listen = %s %s", n, a)
		}
		if n, a := c.sp.destAddr(); n != c.dnet || a != c.daddr {
			t.Errorf("dest = %s %s", n, a)
		}
	}
}

func TestPolicy(t *testing.T) {
	server := &Manager{d: &app.Deps{Cfg: &config.Config{Mode: config.ModeServer, Listen: "0.0.0.0:7822"}}}
	desktop := &Manager{d: &app.Deps{Cfg: &config.Config{Mode: config.ModeDesktop, Listen: "127.0.0.1:7822"}}}
	user := &model.User{ID: "u", Role: model.RoleUser}
	admin := &model.User{ID: "a", Role: model.RoleAdmin}
	loopback := &spec{kind: kindLocal, bindHost: "127.0.0.1", bindPort: 8080}
	cases := []struct {
		m    *Manager
		u    *model.User
		sp   *spec
		deny string
	}{
		{server, user, loopback, ""},
		{server, user, &spec{kind: kindLocal, bindHost: "localhost", bindPort: 0}, ""},
		{server, user, &spec{kind: kindRemote, bindHost: "127.0.0.1", bindPort: 9000}, "remote port forwarding"},
		{server, user, &spec{kind: kindRemoteDynamic, bindHost: "127.0.0.1"}, "remote port forwarding"},
		{server, user, &spec{kind: kindDynamic, bindHost: "0.0.0.0", bindPort: 1080}, "non-loopback"},
		{server, user, &spec{kind: kindLocal, bindHost: "*", bindPort: 8080}, "non-loopback"},
		{server, user, &spec{kind: kindLocal, bindSocket: "/tmp/x"}, "Unix socket"},
		{server, user, &spec{kind: kindLocal, bindHost: "127.0.0.1", bindPort: 80}, "below 1024"},
		{server, admin, &spec{kind: kindRemote, bindHost: "0.0.0.0", bindPort: 80}, ""},
		{server, admin, &spec{kind: kindLocal, bindHost: "127.0.0.1", bindPort: 7822}, "NexTerm's own port"},
		{desktop, user, &spec{kind: kindRemote, bindHost: "*", bindPort: 80}, ""},
		{desktop, user, &spec{kind: kindLocal, bindHost: "127.0.0.1", bindPort: 7822}, "NexTerm's own port"},
	}
	for i, c := range cases {
		err := c.m.policy(c.u, c.sp)
		switch {
		case c.deny == "" && err != nil:
			t.Errorf("case %d: unexpected denial %v", i, err)
		case c.deny != "" && (err == nil || !strings.Contains(err.Error(), c.deny)):
			t.Errorf("case %d: err = %v, want %q", i, err, c.deny)
		}
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]float64{1: 1, 2: 2, 3: 4, 7: 60, 20: 60} {
		d := backoff(n).Seconds()
		if d < want*0.8-0.001 || d > want*1.2+0.001 {
			t.Errorf("backoff(%d) = %.2fs, want ≈%.0fs", n, d, want)
		}
	}
}

func TestPermanentClassification(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errString("input is required but no NexTerm window is connected"), false},
		{errString("no answer to the password prompt"), true},
		{errString("connect to 1.2.3.4:22: connection refused"), false},
		{model.ErrNotFound, true},
	} {
		if got := permanent(c.err); got != c.want {
			t.Errorf("permanent(%v) = %v", c.err, got)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
