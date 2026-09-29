package ipmi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/clock"
	"github.com/bougou/go-ipmi/pkg/hal/mock"
	"github.com/bougou/go-ipmi/pkg/server"
	"github.com/bougou/go-ipmi/pkg/transport/udp"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// referenceBMC starts go-ipmi's reference IPMI v2.0 BMC (RMCP+, SOL, chassis) on UDP loopback with a fake system
// console, and returns a connection pointing at it.
func referenceBMC(t *testing.T) (*model.Connection, *bmc.BMC, *mock.FakeConsoleConn, *mock.HAL) {
	t.Helper()
	fake := &mock.FakeConsoleConn{}
	hal := mock.New()
	hal.SetConsole(&mock.Console{Conn: fake})
	b := bmc.New(bmc.DeviceInfo{IPMIVersion: 0x20}, [16]byte{}, hal, bmc.WithClock(clock.Real))
	user, err := b.Users.Add(2, "ADMIN")
	if err != nil {
		t.Fatalf("add user: %v", err)
	}
	user.SetPassword([]byte("s3cret"))
	user.Enabled = true
	user.ChannelAccess[1] = bmc.UserChannelAccess{MaxPrivilege: bmc.PrivilegeLevelAdministrator, Enabled: true}

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp: %v", err)
	}
	srv := server.NewServer(b, udp.Wrap(pc))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = srv.Close(); _ = pc.Close() })

	addr := pc.LocalAddr().(*net.UDPAddr)
	conn := &model.Connection{Protocol: model.ProtoIPMI, Host: addr.IP.String(), Port: addr.Port, Username: "ADMIN"}
	return conn, b, fake, hal
}

type collector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func collect(r io.Reader) *collector {
	c := &collector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			c.mu.Lock()
			c.buf = append(c.buf, buf[:n]...)
			if err != nil {
				c.err = err
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *collector) until(d time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSOLSessionAgainstReferenceBMC drives the real opener: RMCP+ login, SOL activation, console output to the
// terminal, keystrokes to the console, and payload deactivation on Close.
func TestSOLSessionAgainstReferenceBMC(t *testing.T) {
	conn, b, fake, _ := referenceBMC(t)
	m := &module{}
	be, err := m.open(context.Background(), term.OpenRequest{Connection: conn, Secrets: map[string]string{model.SecretPassword: "s3cret"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitFor(t, "SOL activation", func() bool { return b.SOL.ActiveSessionID(1) != 0 })
	col := collect(be)

	fake.FeedRX([]byte("server login: "))
	if out := col.until(5*time.Second, func(o []byte) bool { return bytes.Contains(o, []byte("login: ")) }); !bytes.Contains(out, []byte("server login: ")) {
		t.Fatalf("console output = %q", out)
	}
	if _, err := be.Write([]byte("root\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "keystrokes at the console", func() bool { return strings.Contains(fake.TXString(), "root\r") })

	be.Close()
	col.until(5*time.Second, func([]byte) bool { return false })
	col.mu.Lock()
	err = col.err
	col.mu.Unlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("after Close: %v, want EOF", err)
	}
	waitFor(t, "SOL deactivation", func() bool { return b.SOL.ActiveSessionID(1) == 0 })
}

func TestSOLWrongPasswordIsPermanent(t *testing.T) {
	conn, _, _, _ := referenceBMC(t)
	m := &module{}
	_, err := m.open(context.Background(), term.OpenRequest{Connection: conn, Secrets: map[string]string{model.SecretPassword: "nope"}})
	if err == nil || !term.IsPermanent(err) {
		t.Fatalf("wrong password: %v (permanent=%v)", err, term.IsPermanent(err))
	}
}

func TestPowerControlAgainstReferenceBMC(t *testing.T) {
	conn, _, _, hal := referenceBMC(t)
	p := paramsFrom(conn, map[string]string{model.SecretPassword: "s3cret"})
	ctx := context.Background()
	on, _ := hal.Chassis().PowerState(ctx)

	st, err := power(ctx, p, "status")
	if err != nil || st.PowerOn != on {
		t.Fatalf("status = %+v %v (hal says %v)", st, err, on)
	}
	res, err := power(ctx, p, "off")
	if err != nil || res.PowerOn {
		t.Fatalf("off = %+v %v", res, err)
	}
	if now, _ := hal.Chassis().PowerState(ctx); now {
		t.Fatal("chassis still powered after off")
	}
	res, err = power(ctx, p, "on")
	if err != nil || !res.PowerOn {
		t.Fatalf("on = %+v %v", res, err)
	}
	if _, err := power(ctx, p, "explode"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestRoutedIPMIRefused(t *testing.T) {
	for _, o := range []model.Options{{"sshTunnelVia": "x"}, {"proxy": map[string]any{"type": "socks5", "host": "h", "port": 1}}, {"jumpHosts": []any{"h"}}} {
		if err := checkRoute(&model.Connection{Options: o}); err == nil {
			t.Errorf("routing %v accepted", o)
		}
	}
	if err := checkRoute(&model.Connection{Options: model.Options{"proxy": map[string]any{"type": "none"}}}); err != nil {
		t.Errorf("proxy none refused: %v", err)
	}
}

// TestServeReferenceBMC is a manual end-to-end harness, not a unit test: with NEXTERM_SERVE_REFBMC=host:port it serves
// the reference BMC (user ADMIN / s3cret) until the process is killed. The system console echoes keystrokes back
// upper-cased and prints "bmc-console> " once a SOL session attaches.
func TestServeReferenceBMC(t *testing.T) {
	addr := os.Getenv("NEXTERM_SERVE_REFBMC")
	if addr == "" {
		t.Skip("set NEXTERM_SERVE_REFBMC=host:port to serve a reference BMC for manual end-to-end tests")
	}
	fake := &mock.FakeConsoleConn{}
	hal := mock.New()
	hal.SetConsole(&mock.Console{Conn: fake})
	b := bmc.New(bmc.DeviceInfo{IPMIVersion: 0x20}, [16]byte{}, hal, bmc.WithClock(clock.Real))
	user, err := b.Users.Add(2, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	user.SetPassword([]byte("s3cret"))
	user.Enabled = true
	user.ChannelAccess[1] = bmc.UserChannelAccess{MaxPrivilege: bmc.PrivilegeLevelAdministrator, Enabled: true}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenUDP("udp", ua)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(b, udp.Wrap(pc))
	go func() { _ = srv.Serve(context.Background()) }()
	fmt.Println("reference BMC listening on", pc.LocalAddr())
	seen := 0
	prompted := false
	for {
		time.Sleep(50 * time.Millisecond)
		if b.SOL.ActiveSessionID(1) == 0 {
			prompted = false
			continue
		}
		if !prompted {
			fake.FeedRX([]byte("bmc-console> "))
			prompted = true
		}
		tx := fake.TXString()
		if len(tx) > seen {
			fake.FeedRX([]byte(strings.ToUpper(tx[seen:])))
			seen = len(tx)
		}
	}
}
