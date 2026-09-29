package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

type fakeSource struct {
	runs    atomic.Int32
	stopped atomic.Int32
}

func (f *fakeSource) run(ctx context.Context, emit func(*Stats), fail func(state, msg string)) {
	f.runs.Add(1)
	fail(stateWaiting, "warming up")
	emit(&Stats{Hostname: "fake", TS: time.Now()})
	<-ctx.Done()
	f.stopped.Add(1)
}

func newFeed(s *Service, id string) *feed {
	ctx, cancel := context.WithCancel(context.Background())
	return &feed{s: s, id: id, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), subs: map[string]int{}}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lastEvent(f *feed) monitorEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ev monitorEvent
	_ = json.Unmarshal(f.last, &ev)
	return ev
}

func TestCollectorRefcountAndLinger(t *testing.T) {
	s := New(nil, nil)
	s.Linger = 60 * time.Millisecond
	src := &fakeSource{}
	mk := func() source { return src }
	f1, f2 := newFeed(s, "s1"), newFeed(s, "s2")

	c := s.attachCollector("transport", mk, f1)
	eventually(t, "first sample", func() bool { return lastEvent(f1).Stats != nil })
	if c2 := s.attachCollector("transport", mk, f2); c2 != c {
		t.Fatal("second subscriber got another collector")
	}
	// A late subscriber immediately receives the last sample, tagged with its own session.
	if ev := lastEvent(f2); ev.Stats == nil || ev.Stats.Hostname != "fake" || ev.SessionID != "s2" || ev.Type != model.EvMonitor {
		t.Fatalf("replay: %+v", ev)
	}
	c.removeFeed(f1)
	time.Sleep(120 * time.Millisecond)
	if src.stopped.Load() != 0 || s.collectorFor("transport") != c {
		t.Fatal("collector stopped while a subscriber remains")
	}
	// Last subscriber gone: the collector lingers, and a subscriber returning in time keeps it.
	c.removeFeed(f2)
	time.Sleep(20 * time.Millisecond)
	if c3 := s.attachCollector("transport", mk, f1); c3 != c {
		t.Fatal("re-subscribing within the linger period started a new collector")
	}
	time.Sleep(120 * time.Millisecond)
	if src.stopped.Load() != 0 || src.runs.Load() != 1 {
		t.Fatalf("runs %d stopped %d", src.runs.Load(), src.stopped.Load())
	}
	c.removeFeed(f1)
	eventually(t, "collector stop after linger", func() bool { return src.stopped.Load() == 1 && s.collectorFor("transport") == nil })
	// A new subscriber after the stop starts a fresh collector.
	c4 := s.attachCollector("transport", mk, f2)
	if c4 == c {
		t.Fatal("stopped collector reused")
	}
	eventually(t, "restart", func() bool { return src.runs.Load() == 2 })
	c4.removeFeed(f2)
}

func (s *Service) collectorFor(key any) *collector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collectors[key]
}

func TestSubscribeValidationAndLocalFeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &app.Deps{Ctx: ctx}
	sessions := term.New(d)
	s := New(d, &core.Core{Sessions: sessions})
	s.Interval = 300 * time.Millisecond
	s.Linger = 50 * time.Millisecond
	user := &model.User{ID: "u1", Role: model.RoleAdmin}
	if _, err := s.subscribe(ctx, user, "c1", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "sessionId") {
		t.Fatalf("missing sessionId: %v", err)
	}
	if _, err := s.subscribe(ctx, user, "c1", json.RawMessage(`{"sessionId":"nope"}`)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown session: %v", err)
	}

	// The local feed samples the AstraTerm host with gopsutil.
	f := s.attachSubscriber("local-session", user, true, "c1")
	if again := s.attachSubscriber("local-session", user, true, "c2"); again != f {
		t.Fatal("second socket got another feed")
	}
	eventually(t, "local stats", func() bool {
		ev := lastEvent(f)
		return ev.Stats != nil && !ev.Stats.Warmup
	})
	st := lastEvent(f).Stats
	if st.Mem.Total <= 0 || st.CPU.Cores <= 0 || st.Platform == "" || st.Hostname == "" || st.Processes <= 0 || len(st.Disks) == 0 {
		t.Fatalf("local stats: %+v", st)
	}
	s.detachSubscriber(f, "c1")
	if s.feeds["local-session"] != f {
		t.Fatal("feed dropped with a subscriber left")
	}
	s.detachSubscriber(f, "c2")
	if s.feeds["local-session"] != nil || f.ctx.Err() == nil {
		t.Fatal("feed kept without subscribers")
	}
	eventually(t, "local collector stop", func() bool { return s.collectorFor(localKey{}) == nil })

	// sessionId "local" watches the AstraTerm host itself (System information view)…
	unsub, err := s.subscribe(ctx, user, "c9", json.RawMessage(`{"sessionId":"local"}`))
	if err != nil || s.feeds["local"] == nil {
		t.Fatalf("local host subscription: %v", err)
	}
	unsub()
	if s.feeds["local"] != nil {
		t.Fatal("local host feed kept after unsubscribe")
	}
	// …which in server mode is reserved to administrators.
	d.Cfg = &config.Config{Mode: config.ModeServer}
	if _, err := s.subscribe(ctx, &model.User{ID: "u2", Role: model.RoleUser}, "c10", json.RawMessage(`{"sessionId":"local"}`)); err == nil {
		t.Fatal("non-admin subscribed to the local host in server mode")
	}
}
