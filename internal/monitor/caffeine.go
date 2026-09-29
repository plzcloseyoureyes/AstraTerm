package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Caffeine (SEC-22) keeps the Termstead host from sleeping or blanking its display while long jobs run: `caffeinate
// -dimsu` on macOS, a systemd-inhibit idle:sleep lock on Linux, SetThreadExecutionState on Windows. The browser side
// additionally holds a Screen Wake Lock. The inhibitor is released when switched off, when its optional timer expires
// and when Termstead shuts down (the helper processes also watch Termstead's PID, so a crash cannot leak the lock).

// CaffeineStatus is the answer of GET/POST /api/system/caffeine and the payload of {type:'caffeine'} events.
type CaffeineStatus struct {
	Supported bool       `json:"supported"`
	Allowed   bool       `json:"allowed"`
	Enabled   bool       `json:"enabled"`
	Since     *time.Time `json:"since,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	Method    string     `json:"method,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// inhibitor is an active sleep inhibition.
type inhibitor interface {
	release()
	alive() bool
}

type caffeine struct {
	log *slog.Logger

	mu       sync.Mutex
	active   inhibitor
	since    time.Time
	until    time.Time
	timer    *time.Timer
	lastErr  string
	onChange func(CaffeineStatus)
}

func newCaffeine(ctx context.Context, log *slog.Logger) *caffeine {
	c := &caffeine{log: log}
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		c.stopLocked()
		c.mu.Unlock()
	}()
	return c
}

func (c *caffeine) supported() bool { return inhibitSupported() }

func (c *caffeine) status() CaffeineStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *caffeine) statusLocked() CaffeineStatus {
	st := CaffeineStatus{Supported: inhibitSupported(), Method: inhibitMethod(), Error: c.lastErr}
	if c.active != nil && !c.active.alive() {
		c.active = nil
		c.lastErr = "the " + inhibitMethod() + " helper exited unexpectedly"
		st.Error = c.lastErr
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
	}
	if c.active != nil {
		st.Enabled = true
		since := c.since.UTC()
		st.Since = &since
		if !c.until.IsZero() {
			until := c.until.UTC()
			st.Until = &until
		}
	}
	return st
}

// set switches Caffeine on (optionally for d) or off.
func (c *caffeine) set(enabled bool, d time.Duration) (CaffeineStatus, error) {
	c.mu.Lock()
	if !enabled {
		c.stopLocked()
		c.lastErr = ""
		st := c.statusLocked()
		cb := c.onChange
		c.mu.Unlock()
		if cb != nil {
			cb(st)
		}
		return st, nil
	}
	if !inhibitSupported() {
		c.mu.Unlock()
		return CaffeineStatus{Method: inhibitMethod()}, errors.New("preventing sleep is not supported on this system")
	}
	if c.active == nil || !c.active.alive() {
		inh, err := startInhibit()
		if err != nil {
			c.lastErr = err.Error()
			st := c.statusLocked()
			c.mu.Unlock()
			return st, err
		}
		c.active, c.since, c.lastErr = inh, time.Now(), ""
	}
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.until = time.Time{}
	if d > 0 {
		c.until = time.Now().Add(d)
		inh := c.active
		c.timer = time.AfterFunc(d, func() { c.expire(inh) })
	}
	st := c.statusLocked()
	cb := c.onChange
	c.mu.Unlock()
	if cb != nil {
		cb(st)
	}
	return st, nil
}

// expire ends a timed session (unless it was replaced meanwhile).
func (c *caffeine) expire(inh inhibitor) {
	c.mu.Lock()
	if c.active != inh {
		c.mu.Unlock()
		return
	}
	c.stopLocked()
	st := c.statusLocked()
	cb := c.onChange
	c.mu.Unlock()
	c.log.Info("caffeine timer expired")
	if cb != nil {
		cb(st)
	}
}

func (c *caffeine) stopLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.active != nil {
		c.active.release()
		c.active = nil
	}
	c.since, c.until = time.Time{}, time.Time{}
}
