package guac

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Protocol versions negotiated in the handshake ("args" / "connect"). guacd 1.6 still announces VERSION_1_5_0.
const (
	Version100 = "VERSION_1_0_0"
	Version110 = "VERSION_1_1_0"
	Version130 = "VERSION_1_3_0"
	Version150 = "VERSION_1_5_0"
)

// ClientVersion is the newest protocol version this client speaks.
const ClientVersion = Version150

// HandshakeTimeout bounds the guacd handshake (select … ready).
const HandshakeTimeout = 20 * time.Second

// Handshake describes the connection guacd must establish.
type Handshake struct {
	// Protocol is the guacd protocol plugin ("rdp"), or "$<connection id>" to join an existing connection.
	Protocol string
	// Params are connection parameter values by name (hostname, port, username, …). Parameters guacd does not
	// advertise are ignored; advertised ones without a value are sent empty.
	Params map[string]string
	// Optimal display size and resolution of the client.
	Width, Height, DPI int
	// Supported mimetypes of the client.
	Audio, Video, Image []string
	// Timezone of the client (IANA name, protocol ≥ 1.1.0).
	Timezone string
	// Name of the joining user (protocol ≥ 1.5.0).
	Name string
}

// Conn is an established guacd connection, positioned right after the "ready" instruction.
type Conn struct {
	conn net.Conn
	// R reads instructions sent by guacd.
	R *Reader
	// ID is the connection identifier from "ready" ("$…"), usable to join the connection.
	ID string
	// Version is the negotiated protocol version.
	Version string
	// Args are the parameter names guacd advertised (without the version element).
	Args []string

	wmu sync.Mutex
}

// Error is an "error" instruction received from guacd.
type Error struct {
	Message string
	Status  int
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "guacd: " + StatusText(e.Status)
	}
	return "guacd: " + e.Message
}

// ErrNotReady means guacd closed the connection during the handshake.
var ErrNotReady = errors.New("guacd closed the connection during the handshake")

// Connect performs the client handshake on an established TCP connection to guacd. On success the returned Conn
// owns c; on failure the caller still owns (and must close) c.
func Connect(ctx context.Context, c net.Conn, hs Handshake) (*Conn, error) {
	if hs.Protocol == "" {
		return nil, errors.New("guac: no protocol selected")
	}
	deadline := time.Now().Add(HandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	gc := &Conn{conn: c, R: NewReader(c, CodePoints)}
	if err := gc.Send(New("select", hs.Protocol)); err != nil {
		return nil, err
	}
	args, err := gc.expect("args")
	if err != nil {
		return nil, err
	}
	version := Version100
	params := args.Args
	if len(params) > 0 && strings.HasPrefix(params[0], "VERSION_") {
		version = lowerVersion(params[0], ClientVersion)
		params = params[1:]
	}
	gc.Version, gc.Args = version, params

	w, h, dpi := hs.Width, hs.Height, hs.DPI
	if w <= 0 || h <= 0 {
		w, h = 1024, 768
	}
	if dpi <= 0 {
		dpi = 96
	}
	msgs := []Instruction{
		New("size", strconv.Itoa(w), strconv.Itoa(h), strconv.Itoa(dpi)),
		New("audio", hs.Audio...),
		New("video", hs.Video...),
		New("image", hs.Image...),
	}
	if hs.Timezone != "" && AtLeast(version, Version110) {
		msgs = append(msgs, New("timezone", hs.Timezone))
	}
	if hs.Name != "" && AtLeast(version, Version150) {
		msgs = append(msgs, New("name", hs.Name))
	}
	values := make([]string, 0, len(params)+1)
	if version != Version100 {
		values = append(values, version)
	}
	for _, name := range params {
		values = append(values, hs.Params[name])
	}
	msgs = append(msgs, New("connect", values...))
	if err := gc.Send(msgs...); err != nil {
		return nil, err
	}
	ready, err := gc.expect("ready")
	if err != nil {
		return nil, err
	}
	gc.ID = ready.Arg(0)
	_ = c.SetDeadline(time.Time{})
	return gc, nil
}

// expect reads until an instruction with the given opcode; "error" (and a closed connection) fail the handshake.
func (g *Conn) expect(opcode string) (Instruction, error) {
	for {
		in, err := g.R.Read()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return in, ErrNotReady
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return in, fmt.Errorf("guacd did not answer in time (waiting for %q)", opcode)
			}
			return in, err
		}
		switch in.Opcode {
		case opcode:
			return in, nil
		case "error":
			st, _ := strconv.Atoi(in.Arg(1))
			return in, &Error{Message: in.Arg(0), Status: st}
		case "disconnect":
			return in, ErrNotReady
		}
		// Anything else (e.g. "log" / "nop") before the expected instruction is ignored.
	}
}

// Send writes instructions to guacd (safe for concurrent use; every call writes whole instructions).
func (g *Conn) Send(ins ...Instruction) error {
	var b []byte
	for _, in := range ins {
		b = in.Append(b, CodePoints)
	}
	return g.Write(b)
}

// Write writes already encoded instructions (code point lengths) to guacd.
func (g *Conn) Write(b []byte) error {
	g.wmu.Lock()
	defer g.wmu.Unlock()
	_, err := g.conn.Write(b)
	return err
}

// SetWriteDeadline bounds the next writes.
func (g *Conn) SetWriteDeadline(t time.Time) error { return g.conn.SetWriteDeadline(t) }

// SetReadDeadline bounds the next reads.
func (g *Conn) SetReadDeadline(t time.Time) error { return g.conn.SetReadDeadline(t) }

// Close closes the connection to guacd.
func (g *Conn) Close() error { return g.conn.Close() }

// NetConn returns the underlying connection.
func (g *Conn) NetConn() net.Conn { return g.conn }

// ProbeVersion connects to guacd, asks for the arguments of protocol and returns the protocol version guacd
// announces (VERSION_x_y_z, or VERSION_1_0_0 for servers that announce none) and the advertised parameter names.
func ProbeVersion(ctx context.Context, dial func(ctx context.Context) (net.Conn, error), protocol string) (string, []string, error) {
	c, err := dial(ctx)
	if err != nil {
		return "", nil, err
	}
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	gc := &Conn{conn: c, R: NewReader(c, CodePoints)}
	if err := gc.Send(New("select", protocol)); err != nil {
		return "", nil, err
	}
	args, err := gc.expect("args")
	if err != nil {
		return "", nil, err
	}
	if len(args.Args) > 0 && strings.HasPrefix(args.Args[0], "VERSION_") {
		return args.Args[0], args.Args[1:], nil
	}
	return Version100, args.Args, nil
}

// ---- versions -------------------------------------------------------------------------------------------------------

// parseVersion parses "VERSION_1_5_0" into comparable numbers.
func parseVersion(v string) (int, int, int, bool) {
	rest, ok := strings.CutPrefix(v, "VERSION_")
	if !ok {
		return 0, 0, 0, false
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return 0, 0, 0, false
		}
		n[i] = x
	}
	return n[0], n[1], n[2], true
}

func cmpVersion(a, b string) int {
	a1, a2, a3, aok := parseVersion(a)
	b1, b2, b3, bok := parseVersion(b)
	if !aok || !bok {
		if aok == bok {
			return 0
		}
		if !aok {
			return -1
		}
		return 1
	}
	for _, d := range [][2]int{{a1, b1}, {a2, b2}, {a3, b3}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// AtLeast reports whether version v is at least min.
func AtLeast(v, min string) bool { return cmpVersion(v, min) >= 0 }

// lowerVersion returns the lower of two versions (unknown server versions fall back to ours: guacd ignores versions
// it does not understand only when they are newer, so echoing ours is the safe choice).
func lowerVersion(server, client string) string {
	if _, _, _, ok := parseVersion(server); !ok {
		return client
	}
	if cmpVersion(server, client) < 0 {
		return server
	}
	return client
}
