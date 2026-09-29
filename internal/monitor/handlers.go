package monitor

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
)

type handlers struct{ s *Service }

// Request timeouts.
const (
	timeoutQuick   = 20 * time.Second
	timeoutList    = 45 * time.Second
	timeoutAction  = 90 * time.Second
	timeoutScan    = 3 * time.Minute
	timeoutPrompts = 4 * time.Minute // sudo may ask the user (the prompt broker waits up to 3 minutes)
)

// withTarget resolves the :id target for the request and releases it afterwards.
func (h *handlers) withTarget(c *echo.Context, timeout time.Duration, fn func(ctx context.Context, t *target) error) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), timeout)
	defer cancel()
	t, err := h.s.resolveTarget(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return apiError(err)
	}
	defer t.release()
	return apiError(fn(ctx, t))
}

// apiError keeps typed errors and timeouts, and reports anything else (SSH channel refused, connection lost, command
// could not start) as 502 with its reason instead of an opaque 500. These messages never contain secrets.
func apiError(err error) error {
	if err == nil {
		return nil
	}
	var he *httpx.HTTPError
	var coded interface{ ErrorCode() string }
	if errors.As(err, &he) || errors.As(err, &coded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return httpx.NewError(http.StatusBadGateway, "monitor_failed", clip(strings.TrimSpace(err.Error()), 300))
}

// GET /api/monitor/local — System information of the Termstead host (MON-6).
func (h *handlers) systemInfo(c *echo.Context) error {
	user := httpx.UserFrom(c)
	if !h.s.allowLocal(user) {
		return errLocalForbidden
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), timeoutList)
	defer cancel()
	info, err := h.s.systemInfo(ctx, user)
	if err != nil {
		return apiError(err)
	}
	return c.JSON(http.StatusOK, info)
}

// GET /api/monitor/:id/host — what the probe learned about the host (OS, kernel, CPU, platform).
func (h *handlers) host(c *echo.Context) error {
	return h.withTarget(c, timeoutQuick+10*time.Second, func(ctx context.Context, t *target) error {
		return c.JSON(http.StatusOK, t.host)
	})
}

// GET /api/monitor/:id/snapshot → Stats (the running collector's sample when fresh, else a one-shot reading).
func (h *handlers) snapshot(c *echo.Context) error {
	return h.withTarget(c, timeoutQuick, func(ctx context.Context, t *target) error {
		if t.local {
			if st := h.s.latest(localKey{}); st != nil {
				return c.JSON(http.StatusOK, st)
			}
			prev, err := sampleLocal(ctx)
			if err != nil {
				return err
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			cur, err := sampleLocal(ctx)
			if err != nil {
				return err
			}
			return c.JSON(http.StatusOK, computeStats(prev, cur, h.s.localHost()))
		}
		if st := h.s.latest(t.client); st != nil {
			return c.JSON(http.StatusOK, st)
		}
		st, err := h.s.oneShot(ctx, t)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, st)
	})
}

// GET /api/monitor/:id/processes → Process[]
func (h *handlers) processes(c *echo.Context) error {
	return h.withTarget(c, timeoutList, func(ctx context.Context, t *target) error {
		procs, err := h.s.processes(ctx, t)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, procs)
	})
}

// POST /api/monitor/:id/kill {pid, signal, sudo?}
func (h *handlers) kill(c *echo.Context) error {
	var req struct {
		PID    int    `json:"pid"`
		Signal string `json:"signal"`
		Sudo   bool   `json:"sudo"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	return h.withTarget(c, timeoutPrompts, func(ctx context.Context, t *target) error {
		sig, err := normalizeSignal(req.Signal)
		if err != nil {
			return err
		}
		err = h.s.kill(ctx, t, req.PID, sig, req.Sudo)
		h.s.audit(c.Request().Context(), auditName(t, "kill"), t.id, map[string]any{"pid": req.PID, "signal": sig,
			"sudo": req.Sudo, "host": t.label(), "ok": err == nil})
		if err != nil {
			return err
		}
		h.s.forgetProcesses(t.id)
		return httpx.OK(c)
	})
}

// POST /api/monitor/:id/renice {pid, nice, sudo?}
func (h *handlers) renice(c *echo.Context) error {
	var req struct {
		PID  int  `json:"pid"`
		Nice *int `json:"nice"`
		Sudo bool `json:"sudo"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.Nice == nil {
		return httpx.BadRequest("nice is required")
	}
	return h.withTarget(c, timeoutPrompts, func(ctx context.Context, t *target) error {
		err := h.s.renice(ctx, t, req.PID, *req.Nice, req.Sudo)
		h.s.audit(c.Request().Context(), auditName(t, "renice"), t.id, map[string]any{"pid": req.PID, "nice": *req.Nice,
			"sudo": req.Sudo, "host": t.label(), "ok": err == nil})
		if err != nil {
			return err
		}
		h.s.forgetProcesses(t.id)
		return httpx.OK(c)
	})
}

// GET /api/monitor/:id/services → ServiceList
func (h *handlers) services(c *echo.Context) error {
	return h.withTarget(c, timeoutList, func(ctx context.Context, t *target) error {
		list, err := h.s.listServices(ctx, t)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, list)
	})
}

// POST /api/monitor/:id/services/:name/:action {sudo?}
func (h *handlers) serviceAction(c *echo.Context) error {
	var req struct {
		Sudo bool `json:"sudo"`
	}
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	name, action := c.Param("name"), strings.ToLower(c.Param("action"))
	if err := validUnit(name); err != nil {
		return err
	}
	if !serviceActions[action] {
		return httpx.BadRequest("unknown action (start, stop, restart, reload, enable, disable)")
	}
	return h.withTarget(c, timeoutPrompts, func(ctx context.Context, t *target) error {
		err := h.s.serviceAction(ctx, t, name, action, req.Sudo)
		h.s.audit(c.Request().Context(), auditName(t, "service."+action), t.id, map[string]any{"service": name,
			"sudo": req.Sudo, "host": t.label(), "ok": err == nil})
		if err != nil {
			return err
		}
		return httpx.OK(c)
	})
}

// GET /api/monitor/:id/services/:name/logs?lines=&sudo= → {lines}
func (h *handlers) serviceLogs(c *echo.Context) error {
	lines := 200
	if v := c.QueryParam("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			return httpx.BadRequest("lines must be between 1 and 5000")
		}
		lines = n
	}
	sudo := truthy(c.QueryParam("sudo"))
	return h.withTarget(c, timeoutPrompts, func(ctx context.Context, t *target) error {
		out, err := h.s.serviceLogs(ctx, t, c.Param("name"), lines, sudo)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"lines": out})
	})
}

// GET /api/monitor/:id/ports?sudo= → ListeningPort[]
func (h *handlers) ports(c *echo.Context) error {
	sudo := truthy(c.QueryParam("sudo"))
	return h.withTarget(c, timeoutPrompts, func(ctx context.Context, t *target) error {
		ports, err := h.s.listPorts(ctx, t, sudo)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, ports)
	})
}

// GET /api/monitor/:id/du?path=&sudo= → DiskUsage
func (h *handlers) diskUsage(c *echo.Context) error {
	p := c.QueryParam("path")
	if p == "" {
		p = "/"
	}
	if _, err := validRemotePath(p); err != nil {
		return err
	}
	sudo := truthy(c.QueryParam("sudo"))
	return h.withTarget(c, timeoutScan, func(ctx context.Context, t *target) error {
		du, err := h.s.diskUsage(ctx, t, p, sudo)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, du)
	})
}

// GET /api/monitor/:id/ssh-info → SSHInfo (with a latency probe; SSH sessions only)
func (h *handlers) sshInfo(c *echo.Context) error {
	return h.withTarget(c, timeoutQuick, func(ctx context.Context, t *target) error {
		if t.local {
			return httpx.BadRequest("not an SSH session")
		}
		info, err := h.s.sshInfo(ctx, t)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, info)
	})
}

// WS /ws/monitor/:id/tail — log following (see tail.go).
func (h *handlers) tail(c *echo.Context) error {
	req, err := parseTailRequest(c.Request().URL.Query())
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	rctx, cancel := context.WithTimeout(ctx, timeoutQuick+10*time.Second)
	t, err := h.s.resolveTarget(rctx, httpx.UserFrom(c), c.Param("id"))
	cancel()
	if err != nil {
		return apiError(err)
	}
	defer t.release()
	if t.host.Platform == platWindows {
		return httpx.NewError(http.StatusUnprocessableEntity, "monitor_unavailable", "following logs is not supported on Windows hosts")
	}
	if req.journal && !t.host.has("journalctl") {
		return httpx.NewError(http.StatusUnprocessableEntity, "monitor_unavailable", "the host has no systemd journal (journalctl)")
	}
	ws, err := httpx.AcceptWS(c, nil)
	if err != nil {
		return nil // the handshake error response has been written
	}
	defer ws.CloseNow()
	h.s.audit(ctx, auditName(t, "tail"), t.id, map[string]any{"sources": req.sources(), "sudo": req.sudo, "host": t.label()})
	h.s.followLogs(ctx, ws, t, req)
	return nil
}

// GET /api/system/caffeine → CaffeineStatus
func (h *handlers) caffeineStatus(c *echo.Context) error {
	st := h.s.caffeine.status()
	st.Allowed = h.s.allowLocal(httpx.UserFrom(c))
	return c.JSON(http.StatusOK, st)
}

// POST /api/system/caffeine {enabled, durationMin?} → CaffeineStatus (desktop mode or administrators)
func (h *handlers) caffeineSet(c *echo.Context) error {
	user := httpx.UserFrom(c)
	if !h.s.allowLocal(user) {
		return httpx.Forbidden("Caffeine is available in desktop mode or to administrators")
	}
	var req struct {
		Enabled     bool `json:"enabled"`
		DurationMin int  `json:"durationMin"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.DurationMin < 0 || req.DurationMin > 7*24*60 {
		return httpx.BadRequest("durationMin must be between 0 (no limit) and 10080")
	}
	st, err := h.s.caffeine.set(req.Enabled, time.Duration(req.DurationMin)*time.Minute)
	h.s.audit(c.Request().Context(), "system.caffeine", "", map[string]any{"enabled": req.Enabled, "durationMin": req.DurationMin, "ok": err == nil})
	if err != nil {
		return httpx.NewError(http.StatusUnprocessableEntity, "caffeine_failed", err.Error())
	}
	st.Allowed = true
	return c.JSON(http.StatusOK, st)
}

func auditName(t *target, action string) string {
	if t.local {
		return "monitor.local." + action
	}
	return "monitor." + action
}

// forgetProcesses drops the cached process list of a target (after kill / renice).
func (s *Service) forgetProcesses(id string) {
	s.mu.Lock()
	if ps := s.procHist[id]; ps != nil {
		ps.procs = nil
	}
	s.mu.Unlock()
}

// caffeineEvent is broadcast on every Caffeine change ({type:'caffeine', status}).
type caffeineEvent struct {
	Type   string         `json:"type"`
	Status CaffeineStatus `json:"status"`
}

func (s *Service) broadcastCaffeine(st CaffeineStatus) {
	if s.d == nil || s.d.Events == nil {
		return
	}
	st.Allowed = false // per viewer; clients keep their own value
	s.d.Events.Broadcast(caffeineEvent{Type: "caffeine", Status: st})
}
