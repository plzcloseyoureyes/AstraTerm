package term

import (
	"sync"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

func TestSensitiveMask(t *testing.T) {
	for in, want := range map[string]string{"": "", "pw": "**", "s3cr3t\r": "******\r", "a\r\n": "*\r\n", "\r": "\r"} {
		if got := string(SensitiveMask([]byte(in))); got != want {
			t.Errorf("SensitiveMask(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteSensitive(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	var mu sync.Mutex
	var plain, masked []string
	defer h.m.AddHooks(Hooks{OnInput: func(_ *Session, d []byte) { mu.Lock(); plain = append(plain, string(d)); mu.Unlock() }})()
	defer h.m.AddHooks(Hooks{
		OnInput:          func(_ *Session, d []byte) { t.Errorf("OnInput called although OnSensitiveInput is set: %q", d) },
		OnSensitiveInput: func(_ *Session, d []byte) { mu.Lock(); masked = append(masked, string(d)); mu.Unlock() },
	})()

	if err := h.m.WriteSensitive(s.ID, []byte("hunter2\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.input() == "hunter2\r" }, "secret reaches the backend")
	mu.Lock()
	defer mu.Unlock()
	if len(plain) != 1 || plain[0] != "*******\r" {
		t.Fatalf("OnInput saw %q, want the mask", plain)
	}
	if len(masked) != 1 || masked[0] != "*******\r" {
		t.Fatalf("OnSensitiveInput saw %q", masked)
	}
}

func TestStartupHandler(t *testing.T) {
	h := newHarness(t, 0)
	var mu sync.Mutex
	var sends []func()
	h.m.SetStartupHandler(func(_ *Session, conn *model.Connection, send func()) bool {
		if conn.Options.String("startupCommand", "") != "cd /tmp" {
			t.Errorf("handler got connection options %v", conn.Options)
		}
		mu.Lock()
		sends = append(sends, send)
		mu.Unlock()
		return true
	})
	b := newFake()
	s := h.create(b, model.Options{"startupCommand": "cd /tmp"})
	h.waitState(s, model.StateConnected)
	time.Sleep(100 * time.Millisecond)
	if in := b.input(); in != "" {
		t.Fatalf("startup command sent although the handler claimed it: %q", in)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sends) == 1 }, "handler call")
	mu.Lock()
	first := sends[0]
	mu.Unlock()
	if err := h.m.Write(s.ID, []byte("step\r")); err != nil { // e.g. a logon action
		t.Fatal(err)
	}
	first()
	first() // at most once
	waitFor(t, func() bool { return b.input() == "step\rcd /tmp\r" }, "startup command after the logon step")

	// A send kept from before a reconnect does nothing on the new connection.
	b2 := newFake()
	h.backends <- b2
	if err := h.m.Reconnect(s.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(s, model.StateConnected)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sends) == 2 }, "handler call after reconnect")
	first()
	time.Sleep(100 * time.Millisecond)
	if in := b2.input(); in != "" {
		t.Fatalf("stale send typed %q into the new connection", in)
	}

	// Without a handler (or when it declines) the command is sent at once, as before.
	h.m.SetStartupHandler(func(*Session, *model.Connection, func()) bool { return false })
	b3 := newFake()
	s3 := h.create(b3, model.Options{"startupCommand": "id"})
	h.waitState(s3, model.StateConnected)
	waitFor(t, func() bool { return b3.input() == "id\r" }, "declined handler")
}

// An administrator's read-only shadow attach is announced to the owner's views ({type:'shadow', viewers}) and the
// announcement ends when the shadow leaves; plain read-only viewers (shares) are not shadows.
func TestShadowViewersAnnounced(t *testing.T) {
	h := newHarness(t, 0)
	s := h.create(newFake(), nil)
	owner := h.attach(s, 0, false)
	owner.until(func() bool { return owner.hasMsg("attach-end") }, 5*time.Second)

	share := h.attach(s, 0, true)
	share.until(func() bool { return share.hasMsg("attach-end") }, 5*time.Second)
	if owner.hasMsg("shadow") {
		t.Fatal("a share viewer was announced as an administrator")
	}

	shadow := h.attach(s, 0, true, "shadow=1", "user=root2")
	shadow.until(func() bool { return shadow.hasMsg("attach-end") }, 5*time.Second)
	owner.until(func() bool {
		m := owner.msg("shadow")
		v, _ := m["viewers"].([]any)
		return len(v) == 1 && v[0] == "root2"
	}, 5*time.Second)
	if shadow.hasMsg("shadow") {
		t.Fatal("the shadow viewer itself got the owner's hint")
	}

	// A view of the owner attaching later learns about the shadow in its attach burst.
	late := h.attach(s, 0, false)
	late.until(func() bool { m := late.msg("shadow"); v, _ := m["viewers"].([]any); return len(v) == 1 }, 5*time.Second)

	shadow.ws.CloseNow()
	owner.until(func() bool {
		m := owner.msg("shadow")
		v, _ := m["viewers"].([]any)
		return m != nil && len(v) == 0
	}, 5*time.Second)
}
