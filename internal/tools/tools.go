// Package tools implements Termstead's network tools module (RESEARCH TOOL-4..9, TOOL-15, CC-4, CC-13, CC-17):
// ping, traceroute (classic and mtr-style continuous), port scan, network scan, DNS, whois, Wake-on-LAN, HTTP check,
// TLS certificate inspection, SNMP, an ssh-audit-style SSH server audit and a TCP throughput test (iperf3 client /
// SSH channel), plus the synchronous host endpoints (interfaces, listening ports) and a store-less key generator.
// Long-running tools run as cancellable jobs whose rows stream to the caller as {type:'job'} events (SPEC §6.0 /
// §6.1); the frontend renders them in the 'tools' tab.
//
// Every tool request is validated synchronously (a bad request is an HTTP 4xx, not a failed job) and then runs as a
// job. Tools run from the Termstead host; ping, traceroute and port scan can optionally run "via" a saved SSH connection
// so they execute from a remote vantage point. In server mode the scanners, the throughput test and the host-info
// endpoints are admin-only (SPEC principle 7, RESEARCH TOOL-5), and non-admin users cannot aim tools at the Termstead
// host itself or at link-local / cloud-metadata addresses (see guard.go).
package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// Job limits: a user may run a handful of tools at once (a scan, an mtr and a ping side by side); the global cap keeps
// a busy multi-user server from being flooded with probes.
const (
	maxJobsPerUser = 8
	maxJobsTotal   = 64
)

type handler struct {
	d *app.Deps
	c *core.Core

	mu      sync.Mutex
	running map[string]map[string]int // user ID → tool → running jobs
	total   int

	keygenSem chan struct{} // bounds concurrent (CPU-heavy) RSA key generations
}

func newHandler(d *app.Deps, c *core.Core) *handler {
	return &handler{d: d, c: c, running: map[string]map[string]int{}, keygenSem: make(chan struct{}, 2)}
}

// Mount registers the tools module's routes (SPEC §10.1).
func Mount(d *app.Deps, c *core.Core) error {
	h := newHandler(d, c)
	api := d.Router.API()

	// Long-running, cancellable tools → {jobId}; results stream as job events.
	api.POST("/tools/:tool", h.runTool)

	// Synchronous host queries.
	api.GET("/tools/interfaces", h.interfaces) // network inventory of the Termstead host
	api.GET("/tools/listening", h.listening)   // TOOL-6 local listening/established sockets
	api.POST("/tools/listening/kill", h.killListener)

	// Store-less key generation (SPEC §6.0: same shape as /keys/generate but returns material without storing).
	api.POST("/tools/keygen", h.keygen)
	return nil
}

// call is one tool invocation: the caller, its raw request body and the network policy that applies to it.
type call struct {
	h     *handler
	user  *model.User
	body  []byte
	guard *netGuard // nil: unrestricted (desktop mode or an admin)

	// Filled by prepare functions for the audit entry.
	target  string
	details map[string]any
}

// runner is a validated tool invocation, executed inside its job.
type runner func(ctx context.Context, out *sink) error

// prepareFunc parses and validates a request synchronously (errors become HTTP 4xx) and returns the job body. ctx is
// the request context; prepare must not block for long (no dialing or prompting — that happens in the runner).
type prepareFunc func(ctx context.Context, cl *call) (runner, error)

type toolSpec struct {
	prepare   prepareFunc
	adminOnly bool // requires an admin in server mode
	perUser   int  // max concurrent jobs of this tool per user (0 = only the global per-user limit)
}

// Tool names accepted by POST /api/tools/:tool.
const (
	toolPing       = "ping"
	toolTraceroute = "traceroute"
	toolPortscan   = "portscan"
	toolNetscan    = "netscan"
	toolDNS        = "dns"
	toolWhois      = "whois"
	toolWOL        = "wol"
	toolHTTPCheck  = "httpcheck"
	toolTLSCert    = "tlscert"
	toolSNMP       = "snmp"
	toolSSHAudit   = "sshaudit"
	toolThroughput = "throughput"
)

var toolSpecs = map[string]toolSpec{
	toolPing:       {prepare: preparePing},
	toolTraceroute: {prepare: prepareTraceroute},
	toolPortscan:   {prepare: preparePortscan, adminOnly: true},
	toolNetscan:    {prepare: prepareNetscan, adminOnly: true},
	toolDNS:        {prepare: prepareDNS},
	toolWhois:      {prepare: prepareWhois},
	toolWOL:        {prepare: prepareWOL},
	toolHTTPCheck:  {prepare: prepareHTTPCheck},
	toolTLSCert:    {prepare: prepareTLSCert},
	toolSNMP:       {prepare: prepareSNMP},
	toolSSHAudit:   {prepare: prepareSSHAudit},
	toolThroughput: {prepare: prepareThroughput, adminOnly: true, perUser: 1},
}

func (h *handler) runTool(c *echo.Context) error {
	tool := c.Param("tool")
	spec, ok := toolSpecs[tool]
	if !ok {
		return httpx.NotFound("unknown tool")
	}
	user := httpx.UserFrom(c)
	if err := h.gate(user, spec.adminOnly); err != nil {
		return err
	}
	body, err := readBody(c)
	if err != nil {
		return err
	}
	cl := &call{h: h, user: user, body: body, guard: h.guardFor(user)}
	run, err := spec.prepare(c.Request().Context(), cl)
	if err != nil {
		return err
	}
	release, err := h.acquireSlot(user, tool, spec.perUser)
	if err != nil {
		return err
	}
	h.auditLog(c, "tools."+tool, cl.target, cl.details)

	// The job context outlives the request: carry the caller and client IP so audit entries written by a tool (e.g.
	// Wake-on-LAN) are attributed correctly.
	ip := httpx.ClientIP(c)
	jobID := h.d.Jobs.Start(user, "tools."+tool, func(ctx context.Context, emit func(any)) error {
		defer release()
		ctx = httpx.WithClientIP(httpx.WithUser(ctx, user), ip)
		s := newSink(emit)
		defer s.close()
		return run(ctx, s)
	})
	return c.JSON(http.StatusOK, map[string]any{"jobId": jobID})
}

// acquireSlot reserves a job slot for user (429 when the user or the server runs too many tools). The returned
// release func is idempotent.
func (h *handler) acquireSlot(user *model.User, tool string, perTool int) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	byTool := h.running[user.ID]
	n := 0
	for _, v := range byTool {
		n += v
	}
	switch {
	case perTool > 0 && byTool[tool] >= perTool:
		return nil, httpx.TooManyRequests("this tool is already running; stop it first", 2)
	case n >= maxJobsPerUser:
		return nil, httpx.TooManyRequests("too many tools are running; stop one first", 2)
	case h.total >= maxJobsTotal:
		return nil, httpx.TooManyRequests("the server is busy running other tools; try again shortly", 5)
	}
	if byTool == nil {
		byTool = map[string]int{}
		h.running[user.ID] = byTool
	}
	byTool[tool]++
	h.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if m := h.running[user.ID]; m != nil {
				if m[tool]--; m[tool] <= 0 {
					delete(m, tool)
				}
				if len(m) == 0 {
					delete(h.running, user.ID)
				}
			}
			h.total--
		})
	}, nil
}

// auditLog records an audit entry when a logger is configured (nil-safe so job runners can be unit-tested with a
// bare handler).
func (h *handler) auditLog(src any, action, target string, details any) {
	if h != nil && h.d != nil && h.d.Audit != nil {
		if m, ok := details.(map[string]any); ok && len(m) == 0 {
			details = nil
		}
		h.d.Audit.Log(src, action, target, details)
	}
}

// isDesktop reports desktop mode (a bare handler in unit tests counts as desktop).
func (h *handler) isDesktop() bool {
	return h == nil || h.d == nil || h.d.Cfg == nil || h.d.Cfg.IsDesktop()
}

// gate enforces the server-mode admin restriction of the scanners and host endpoints.
func (h *handler) gate(user *model.User, adminOnly bool) error {
	if !adminOnly || h.isDesktop() || user.IsAdmin() {
		return nil
	}
	return httpx.Forbidden("this tool is available to administrators only in server mode")
}

// readBody returns the (optional) JSON request body bytes, enforcing the default size limit and rejecting malformed
// JSON.
func readBody(c *echo.Context) ([]byte, error) {
	var raw json.RawMessage
	if err := httpx.BindOptional(c, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("{}"), nil
	}
	return raw, nil
}

// decode unmarshals a tool's request body into v (a JSON object is required).
func decode(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return httpx.BadRequest("invalid request: " + err.Error())
	}
	return nil
}

// ---- streaming sink ------------------------------------------------------------------------------------------------

// row is one streamed result. Every job data event is {rows:[…]}; each row carries its own kind so the frontend can
// dispatch heterogeneous rows (per-reply results, progress and summaries) from one stream.
type row map[string]any

// sink batches rows so a burst of results (a wide port scan) never floods the events hub's per-client queue: rows are
// coalesced and flushed every flushEvery, or immediately once maxRows are pending, so a job produces at most ~10
// events per second plus one per maxRows rows. It is safe for concurrent use; emit is called under the sink's lock so
// batches keep their order (emit must not block — the events hub only enqueues).
type sink struct {
	mu        sync.Mutex
	emit      func(any)
	buf       []row
	maxRows   int
	closed    bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

const flushEvery = 100 * time.Millisecond

func newSink(emit func(any)) *sink {
	s := &sink{emit: emit, maxRows: 1000, stop: make(chan struct{}), done: make(chan struct{})}
	go s.loop()
	return s
}

func (s *sink) loop() {
	defer close(s.done)
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.flush()
		}
	}
}

// add queues a row; it is flushed by the ticker or when the batch is full.
func (s *sink) add(r row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.buf = append(s.buf, r)
	if len(s.buf) >= s.maxRows {
		s.flushLocked()
	}
}

// emitNow queues r and flushes immediately (summaries, info lines, single-shot results).
func (s *sink) emitNow(r row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.buf = append(s.buf, r)
	s.flushLocked()
}

func (s *sink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

func (s *sink) flushLocked() {
	if len(s.buf) == 0 {
		return
	}
	rows := s.buf
	s.buf = nil
	s.emit(map[string]any{"rows": rows})
}

// close stops the flush ticker and sends whatever is pending; later rows are dropped.
func (s *sink) close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.flushLocked()
		s.closed = true
	})
}
