// Package serial implements the "serial" terminal protocol (PROTO-10, CC-18, RESEARCH §3.7): a console on a serial
// port of the machine running AstraTerm (go.bug.st/serial), with hardware/software flow control, initial and live
// DTR/RTS control, modem-status polling (CTS/DSR/DCD/RI), BREAK, local echo and Enter translation, reconnect after a
// replug (autoReconnect) and auto-baud detection. Serial ports are host hardware, so in server mode they are
// restricted to administrators (SPEC principle 7).
//
// Endpoints (SPEC §6.0 + module extensions):
//
//	GET  /api/serial/ports                               list host serial ports (with USB details where available)
//	POST /api/serial/autobaud {device, rates?, probe?}   probe baud rates, scoring printable output (CC-18)
//	POST /api/sessions/:id/serial {dtr?, rts?}            toggle the DTR/RTS output lines of a live serial session
//	GET  /api/sessions/:id/serial/status                  modem status bits + current DTR/RTS of a live serial session
package serial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	goserial "go.bug.st/serial"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/proto/rawtcp/linedisc"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// PortInfo is one entry of GET /api/serial/ports.
type PortInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	VID         string `json:"vid"`
	PID         string `json:"pid"`
	Serial      string `json:"serial"`
}

// Flow-control modes (options.flowControl).
const (
	flowNone    = "none"
	flowRTSCTS  = "rtscts"
	flowXONXOFF = "xonxoff"
	flowDSRDTR  = "dsrdtr"
)

func flowLabel(mode string) string {
	switch mode {
	case flowRTSCTS:
		return "RTS/CTS"
	case flowXONXOFF:
		return "XON/XOFF"
	case flowDSRDTR:
		return "DSR/DTR"
	}
	return "no"
}

// registry tracks live serial backends by session id so the DTR/RTS and status endpoints can reach them.
var registry = struct {
	sync.Mutex
	backends map[string]*backend
}{backends: map[string]*backend{}}

func track(id string, b *backend) {
	registry.Lock()
	registry.backends[id] = b
	registry.Unlock()
}

func untrack(id string, b *backend) {
	registry.Lock()
	if registry.backends[id] == b {
		delete(registry.backends, id)
	}
	registry.Unlock()
}

func lookup(id string) *backend {
	registry.Lock()
	defer registry.Unlock()
	return registry.backends[id]
}

// Mount registers the "serial" protocol, its creation policy and the REST endpoints.
func Mount(d *app.Deps, c *core.Core) error {
	allowed := func(user *model.User) error {
		if user == nil {
			return httpx.ErrUnauthorized
		}
		if d.Cfg != nil && d.Cfg.IsServer() && !user.IsAdmin() {
			return httpx.Forbidden("serial ports are only available to administrators in server mode")
		}
		return nil
	}
	term.RegisterPolicy(string(model.ProtoSerial), func(_ context.Context, user *model.User, _ *model.Connection) error {
		return allowed(user)
	})
	term.RegisterProtocol(string(model.ProtoSerial), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		if err := allowed(req.User); err != nil {
			return nil, term.Permanent(err)
		}
		return open(ctx, req)
	})

	api := d.Router.API()
	api.GET("/serial/ports", func(c *echo.Context) error {
		if err := allowed(httpx.UserFrom(c)); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, listPorts())
	})
	api.POST("/serial/autobaud", func(c *echo.Context) error {
		if err := allowed(httpx.UserFrom(c)); err != nil {
			return err
		}
		var body struct {
			Device string `json:"device"`
			Rates  []int  `json:"rates"`
			Probe  bool   `json:"probe"`
		}
		if err := httpx.Bind(c, &body); err != nil {
			return err
		}
		body.Device = strings.TrimSpace(body.Device)
		if body.Device == "" {
			return httpx.BadRequest("device is required")
		}
		if err := validateDevice(body.Device); err != nil {
			return httpx.BadRequest(err.Error())
		}
		if len(body.Rates) > 16 {
			return httpx.BadRequest("at most 16 rates can be probed")
		}
		for _, r := range body.Rates {
			if r < 50 || r > 20_000_000 {
				return httpx.BadRequest(fmt.Sprintf("invalid baud rate %d", r))
			}
		}
		res, err := autoBaud(c.Request().Context(), body.Device, body.Rates, body.Probe)
		if err != nil {
			var pe *goserial.PortError
			if errors.As(err, &pe) && pe.Code() == goserial.PortBusy {
				return httpx.Conflict("the port is in use (close its session first)")
			}
			return httpx.BadRequest(err.Error())
		}
		return c.JSON(http.StatusOK, res)
	})

	sessionBackend := func(ec *echo.Context) (*backend, error) {
		u := httpx.UserFrom(ec)
		if u == nil {
			return nil, httpx.ErrUnauthorized
		}
		if c == nil || c.Sessions == nil {
			return nil, httpx.ErrNotFound
		}
		id := ec.Param("id")
		sess := c.Sessions.Get(id)
		if sess == nil || sess.OwnerID != u.ID {
			return nil, httpx.ErrNotFound
		}
		b := lookup(id)
		if b == nil {
			return nil, httpx.Conflict("session is not a connected serial port")
		}
		return b, nil
	}
	api.POST("/sessions/:id/serial", func(c *echo.Context) error {
		b, err := sessionBackend(c)
		if err != nil {
			return err
		}
		var body struct {
			DTR *bool `json:"dtr"`
			RTS *bool `json:"rts"`
		}
		if err := httpx.Bind(c, &body); err != nil {
			return err
		}
		if body.DTR != nil {
			if b.flow == flowDSRDTR {
				return httpx.Conflict("DTR is driven by DSR/DTR flow control")
			}
			if err := b.setDTR(*body.DTR); err != nil {
				return httpx.BadRequest("set DTR: " + err.Error())
			}
		}
		if body.RTS != nil {
			if b.flow == flowRTSCTS {
				return httpx.Conflict("RTS is driven by RTS/CTS flow control")
			}
			if err := b.setRTS(*body.RTS); err != nil {
				return httpx.BadRequest("set RTS: " + err.Error())
			}
		}
		return c.JSON(http.StatusOK, b.status())
	})
	api.GET("/sessions/:id/serial/status", func(c *echo.Context) error {
		b, err := sessionBackend(c)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, b.status())
	})
	return nil
}

// ---- opener -------------------------------------------------------------------------------------------------------

// replugWait bounds how long a reconnect waits for an unplugged device to come back (autoReconnect then retries
// with its own backoff).
var replugWait = 60 * time.Second

func open(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("serial: missing connection"))
	}
	o := conn.Options
	device := strings.TrimSpace(o.String("device", ""))
	if device == "" {
		return nil, term.Permanent(errors.New("serial: no device configured"))
	}
	if err := validateDevice(device); err != nil {
		return nil, term.Permanent(err)
	}
	mode, flow, err := buildMode(o)
	if err != nil {
		return nil, term.Permanent(err)
	}
	reconnect := req.Session != nil && req.Session.Info().ConnectedAt != nil
	port, err := goserial.Open(device, mode)
	if err != nil && reconnect && isNotFound(err) {
		// Reconnect after an unplug: wait for the device to reappear instead of failing at once.
		port, err = waitForDevice(ctx, req.Session, device, mode)
	}
	if err != nil {
		return nil, classifyOpenError(device, err, reconnect)
	}
	if err := applyFlowControl(port, flow); err != nil {
		port.Close()
		return nil, term.Permanent(err)
	}
	b := newBackend(port, device, flow, o)
	if req.Session != nil {
		b.sessionID = req.Session.ID
		track(b.sessionID, b)
	}
	return b, nil
}

func waitForDevice(ctx context.Context, sess *term.Session, device string, mode *goserial.Mode) (goserial.Port, error) {
	if sess != nil {
		sess.SetStatus(model.StateConnecting, "Waiting for "+device+" to be plugged in again")
	}
	deadline := time.Now().Add(replugWait)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		port, err := goserial.Open(device, mode)
		if err == nil || !isNotFound(err) || time.Now().After(deadline) {
			if err == nil {
				// Freshly enumerated USB adapters may need a moment before they accept configuration.
				time.Sleep(200 * time.Millisecond)
			}
			return port, err
		}
	}
}

func isNotFound(err error) bool {
	if pe, ok := errors.AsType[*goserial.PortError](err); ok {
		return pe.Code() == goserial.PortNotFound
	}
	return errors.Is(err, fs.ErrNotExist)
}

// buildMode validates the line settings (so configuration errors are reported as such, not as a generic "invalid
// serial port") and returns the port mode and flow-control mode.
func buildMode(o model.Options) (*goserial.Mode, string, error) {
	mode := &goserial.Mode{
		BaudRate: o.Int("baud", 9600),
		DataBits: o.Int("dataBits", 8),
	}
	// Output lines at open: the OS asserts DTR and RTS by default; options dtr/rts=false keep them low (an
	// Arduino-style board resets when DTR rises). Only set when asked: it needs modem-control ioctls, which some
	// drivers (virtual and Bluetooth ports, ptys) do not implement.
	if dtr, rts := o.Bool("dtr", true), o.Bool("rts", true); !dtr || !rts {
		mode.InitialStatusBits = &goserial.ModemOutputBits{DTR: dtr, RTS: rts}
	}
	if mode.BaudRate < 50 || mode.BaudRate > 20_000_000 {
		return nil, "", fmt.Errorf("serial: invalid baud rate %d", mode.BaudRate)
	}
	if mode.DataBits < 5 || mode.DataBits > 8 {
		return nil, "", fmt.Errorf("serial: invalid data bits %d (5-8)", mode.DataBits)
	}
	switch strings.ToLower(strings.TrimSpace(o.String("parity", "none"))) {
	case "", "none", "n":
		mode.Parity = goserial.NoParity
	case "odd", "o":
		mode.Parity = goserial.OddParity
	case "even", "e":
		mode.Parity = goserial.EvenParity
	case "mark", "m":
		mode.Parity = goserial.MarkParity
	case "space", "s":
		mode.Parity = goserial.SpaceParity
	default:
		return nil, "", fmt.Errorf("serial: invalid parity %q", o.String("parity"))
	}
	switch strings.TrimSpace(o.String("stopBits", "1")) {
	case "", "1":
		mode.StopBits = goserial.OneStopBit
	case "1.5":
		if !stopBits15Supported {
			return nil, "", errors.New("serial: 1.5 stop bits are only supported on Windows hosts")
		}
		mode.StopBits = goserial.OnePointFiveStopBits
	case "2":
		mode.StopBits = goserial.TwoStopBits
	default:
		return nil, "", fmt.Errorf("serial: invalid stop bits %q", o.String("stopBits"))
	}
	flow := strings.ToLower(strings.TrimSpace(o.String("flowControl", flowNone)))
	switch flow {
	case "", flowNone:
		flow = flowNone
	case flowRTSCTS, "hardware", "rts/cts":
		flow = flowRTSCTS
	case flowXONXOFF, "software", "xon/xoff":
		flow = flowXONXOFF
	case flowDSRDTR, "dsr/dtr":
		flow = flowDSRDTR
	default:
		return nil, "", fmt.Errorf("serial: invalid flow control %q (none, rtscts, xonxoff or dsrdtr)", flow)
	}
	return mode, flow, nil
}

// classifyOpenError marks errors that retrying cannot fix as permanent. While reconnecting after a replug, a device
// that is not ready yet can refuse its configuration briefly, so that is only permanent on the first connect.
func classifyOpenError(device string, err error, reconnect bool) error {
	if pe, ok := errors.AsType[*goserial.PortError](err); ok {
		switch pe.Code() {
		case goserial.InvalidSpeed, goserial.InvalidDataBits, goserial.InvalidParity, goserial.InvalidStopBits, goserial.InvalidSerialPort:
			// The device refused the configuration (or is not a serial port).
			if reconnect {
				return fmt.Errorf("serial: %s: %s", device, pe.Error())
			}
			return term.Permanent(fmt.Errorf("serial: %s: %s", device, pe.Error()))
		case goserial.PermissionDenied:
			return term.Permanent(fmt.Errorf("serial: %s: permission denied (on Linux, add the AstraTerm user to the dialout group)", device))
		}
		// PortBusy / PortNotFound are transient: auto-reconnect keeps trying (e.g. until the device is replugged).
		return fmt.Errorf("serial: %s: %s", device, pe.EncodedErrorString())
	}
	return fmt.Errorf("serial: open %s: %w", device, err)
}

// ---- backend ------------------------------------------------------------------------------------------------------

type backend struct {
	port      goserial.Port
	device    string
	flow      string
	sessionID string
	out       *linedisc.Reader
	lineMode  linedisc.Ending
	localEcho bool

	writeMu sync.Mutex
	echo    linedisc.Echo // guarded by writeMu

	mu       sync.Mutex
	dtr, rts bool

	closeOnce sync.Once
	closed    chan struct{}
}

func newBackend(port goserial.Port, device, flow string, o model.Options) *backend {
	b := &backend{
		port:      port,
		device:    device,
		flow:      flow,
		lineMode:  linedisc.ParseEnding(o.String("lineEnding", ""), linedisc.CR),
		localEcho: o.Bool("localEcho"),
		dtr:       o.Bool("dtr", true),
		rts:       o.Bool("rts", true),
		closed:    make(chan struct{}),
	}
	b.out = linedisc.NewReader(portReader{b})
	return b
}

// Status is the JSON of GET /api/sessions/:id/serial/status.
type Status struct {
	Device      string `json:"device"`
	DTR         bool   `json:"dtr"`
	RTS         bool   `json:"rts"`
	CTS         bool   `json:"cts"`
	DSR         bool   `json:"dsr"`
	DCD         bool   `json:"dcd"`
	RI          bool   `json:"ri"`
	FlowControl string `json:"flowControl"`
}

// portReader turns port errors into the session contract: io.EOF after our Close, an error (auto-reconnect) when the
// device went away.
type portReader struct{ b *backend }

func (r portReader) Read(p []byte) (int, error) {
	for {
		n, err := r.b.port.Read(p)
		if err != nil {
			select {
			case <-r.b.closed:
				return n, io.EOF
			default:
			}
			return n, fmt.Errorf("serial %s disconnected: %w", r.b.device, err)
		}
		if n > 0 {
			return n, nil
		}
		// A zero-length read without error (read timeout): keep waiting.
		select {
		case <-r.b.closed:
			return 0, io.EOF
		default:
		}
	}
}

func (b *backend) Read(p []byte) (int, error) { return b.out.Read(p) }

func (b *backend) Write(p []byte) (int, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if _, err := b.port.Write(b.lineMode.Translate(p)); err != nil {
		return 0, err
	}
	if b.localEcho {
		b.out.Inject(b.echo.Render(p))
	}
	return len(p), nil
}

// Resize is a no-op: a serial line has no window size.
func (b *backend) Resize(int, int) error { return nil }

// SendBreak holds the line in the BREAK condition for 250ms.
func (b *backend) SendBreak() error { return b.port.Break(250 * time.Millisecond) }

func (b *backend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		close(b.closed)
		if b.sessionID != "" {
			untrack(b.sessionID, b)
		}
		b.out.Close()
		// A writer held back by flow control (XOFF, CTS low) could block the close forever: let the output go and
		// discard what is still queued before closing.
		if b.flow != flowNone {
			releaseFlow(b.port)
		}
		_ = b.port.ResetOutputBuffer()
		err = b.port.Close()
	})
	return err
}

func (b *backend) setDTR(v bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.port.SetDTR(v); err != nil {
		return err
	}
	b.dtr = v
	return nil
}

func (b *backend) setRTS(v bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.port.SetRTS(v); err != nil {
		return err
	}
	b.rts = v
	return nil
}

func (b *backend) status() Status {
	b.mu.Lock()
	st := Status{Device: b.device, DTR: b.dtr, RTS: b.rts, FlowControl: b.flow}
	b.mu.Unlock()
	if bits, err := b.port.GetModemStatusBits(); err == nil && bits != nil {
		st.CTS, st.DSR, st.DCD, st.RI = bits.CTS, bits.DSR, bits.DCD, bits.RI
	}
	return st
}

var (
	_ term.Backend = (*backend)(nil)
	_ term.Breaker = (*backend)(nil)
)

// ---- auto-baud (CC-18) --------------------------------------------------------------------------------------------

// AutoBaudResult is the JSON of POST /api/serial/autobaud.
type AutoBaudResult struct {
	Baud   int               `json:"baud"`
	Score  float64           `json:"score"`
	Sample string            `json:"sample"`
	Tried  []AutoBaudAttempt `json:"tried"`
}

// AutoBaudAttempt is one probed rate.
type AutoBaudAttempt struct {
	Baud  int     `json:"baud"`
	Score float64 `json:"score"`
	Bytes int     `json:"bytes"`
}

var defaultBaudRates = []int{115200, 9600, 57600, 38400, 19200, 230400, 4800, 2400, 460800, 921600}

// probeWindow is how long each rate listens for data.
var probeWindow = 600 * time.Millisecond

// autoBaud opens the device at each rate (8N1), optionally sends a CR to elicit a prompt, and scores what arrives.
// The port must not be in use by a session.
func autoBaud(ctx context.Context, device string, rates []int, probe bool) (*AutoBaudResult, error) {
	if len(rates) == 0 {
		rates = defaultBaudRates
	}
	res := &AutoBaudResult{Tried: []AutoBaudAttempt{}}
	best := -1.0
	received := 0
	for _, r := range rates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		buf, err := probeBaud(ctx, device, r, probe)
		if err != nil {
			var pe *goserial.PortError
			if errors.As(err, &pe) && (pe.Code() == goserial.PortBusy || pe.Code() == goserial.PortNotFound || pe.Code() == goserial.PermissionDenied) {
				return nil, err
			}
			res.Tried = append(res.Tried, AutoBaudAttempt{Baud: r})
			continue
		}
		score := scoreBytes(buf)
		received += len(buf)
		res.Tried = append(res.Tried, AutoBaudAttempt{Baud: r, Score: round(score), Bytes: len(buf)})
		if len(buf) > 0 && score > best {
			best, res.Baud, res.Score, res.Sample = score, r, round(score), sanitizeSample(buf[:min(len(buf), 120)])
		}
	}
	if received == 0 {
		hint := " — try again with \"send Enter\" so the device prints a prompt"
		if probe {
			hint = " — is the device connected and powered on?"
		}
		return nil, fmt.Errorf("no data received from %s at any tested rate%s", device, hint)
	}
	return res, nil
}

// probeBaud listens on the port at baud for probeWindow and returns what arrived (at most 4 KiB).
func probeBaud(ctx context.Context, device string, baud int, probe bool) ([]byte, error) {
	port, err := goserial.Open(device, &goserial.Mode{BaudRate: baud, DataBits: 8, Parity: goserial.NoParity, StopBits: goserial.OneStopBit})
	if err != nil {
		return nil, err
	}
	defer port.Close()
	_ = port.ResetInputBuffer()
	if err := port.SetReadTimeout(100 * time.Millisecond); err != nil {
		return nil, err
	}
	if probe {
		_, _ = port.Write([]byte{'\r'})
	}
	deadline := time.Now().Add(probeWindow)
	var buf []byte
	tmp := make([]byte, 512)
	for time.Now().Before(deadline) && ctx.Err() == nil && len(buf) < 4096 {
		n, err := port.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

// scoreBytes rates how plausible buf is as text at the right baud rate (0..1): the share of printable ASCII, damped
// for small samples (a few random bytes can look printable) and penalised for NUL/0xFF framing garbage.
func scoreBytes(buf []byte) float64 {
	if len(buf) == 0 {
		return 0
	}
	printable, garbage := 0, 0
	for _, c := range buf {
		switch {
		case c == '\r' || c == '\n' || c == '\t' || (c >= 0x20 && c <= 0x7e):
			printable++
		case c == 0x00 || c == 0xff:
			garbage++
		}
	}
	n := float64(len(buf))
	score := float64(printable)/n - 0.5*float64(garbage)/n
	if len(buf) < 16 {
		score *= float64(len(buf)) / 16
	}
	return max(score, 0)
}

func sanitizeSample(b []byte) string {
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c == '\r' || c == '\n' || c == '\t' || (c >= 0x20 && c <= 0x7e) {
			out = append(out, rune(c))
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

func round(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }
