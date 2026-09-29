//go:build linux || darwin

package serial

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"
	goserial "go.bug.st/serial"
	"golang.org/x/sys/unix"

	"github.com/termstead/termstead/internal/model"
)

// A pseudo-terminal slave is a real tty: go.bug.st/serial opens and configures it like a serial port, and its master
// side plays the device. This exercises the whole backend (termios flow control included) without hardware.

func newPty(t *testing.T) *xpty.UnixPty {
	t.Helper()
	p, err := xpty.NewUnixPty(80, 24)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// masterReader collects what the backend sends to the "device".
type masterReader struct {
	mu  sync.Mutex
	buf []byte
}

func readMaster(p *xpty.UnixPty) *masterReader {
	m := &masterReader{}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := p.Master().Read(buf)
			m.mu.Lock()
			m.buf = append(m.buf, buf[:n]...)
			m.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return m
}

func (m *masterReader) waitFor(d time.Duration, want string) string {
	deadline := time.Now().Add(d)
	for {
		m.mu.Lock()
		got := string(m.buf)
		m.mu.Unlock()
		if bytes.Contains([]byte(got), []byte(want)) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func openPtyBackend(t *testing.T, p *xpty.UnixPty, o model.Options) *backend {
	t.Helper()
	mode, flow, err := buildMode(o)
	if err != nil {
		t.Fatal(err)
	}
	port, err := goserial.Open(p.SlaveName(), mode)
	if err != nil {
		t.Skipf("cannot open pty slave as a serial port: %v", err)
	}
	if err := applyFlowControl(port, flow); err != nil {
		port.Close()
		t.Fatalf("flow control %s: %v", flow, err)
	}
	b := newBackend(port, p.SlaveName(), flow, o)
	t.Cleanup(func() { b.Close() })
	return b
}

func TestPortHandleReflection(t *testing.T) {
	p := newPty(t)
	port, err := goserial.Open(p.SlaveName(), &goserial.Mode{BaudRate: 9600})
	if err != nil {
		t.Skipf("open: %v", err)
	}
	defer port.Close()
	h, err := portHandle(port)
	if err != nil {
		t.Fatalf("go.bug.st/serial no longer exposes its handle field (flow control would break): %v", err)
	}
	if _, err := unix.IoctlGetTermios(int(h), ioctlGetTermios); err != nil {
		t.Fatalf("handle %d is not the port's tty: %v", h, err)
	}
}

func TestBackendOnPty(t *testing.T) {
	p := newPty(t)
	m := readMaster(p)
	b := openPtyBackend(t, p, model.Options{"localEcho": true, "lineEnding": "crlf"})
	col := collect(b)

	// Device → terminal.
	if _, err := p.Master().Write([]byte("U-Boot> ")); err != nil {
		t.Fatal(err)
	}
	if got := col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("U-Boot> ")) }); string(got) != "U-Boot> " {
		t.Fatalf("read %q", got)
	}
	// Terminal → device, with Enter translated and echoed locally at once.
	if _, err := b.Write([]byte("help\r")); err != nil {
		t.Fatal(err)
	}
	if got := m.waitFor(2*time.Second, "help\r\n"); got != "help\r\n" {
		t.Fatalf("device received %q", got)
	}
	if got := col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("\r\n")) }); string(got) != "help\r\n" {
		t.Fatalf("local echo %q", got)
	}
	st := b.status()
	if st.Device != p.SlaveName() || st.FlowControl != flowNone {
		t.Fatalf("status %+v", st)
	}
	// Close ends Read with EOF (the session's normal end).
	b.Close()
	col.until(2*time.Second, func([]byte) bool { return false })
	if err := col.error(); !errors.Is(err, io.EOF) {
		t.Fatalf("after Close: %v, want EOF", err)
	}
}

// TestXonXoffFlowControlOnPty checks software flow control end to end: after the device sends XOFF, output stops
// until XON; the XOFF/XON bytes never reach the terminal.
func TestXonXoffFlowControlOnPty(t *testing.T) {
	p := newPty(t)
	m := readMaster(p)
	b := openPtyBackend(t, p, model.Options{"flowControl": "xonxoff"})
	col := collect(b)

	h, _ := portHandle(b.port)
	tio, err := unix.IoctlGetTermios(int(h), ioctlGetTermios)
	if err != nil || tio.Iflag&unix.IXON == 0 || tio.Iflag&unix.IXOFF == 0 {
		t.Fatalf("IXON/IXOFF not set: %+v %v", tio, err)
	}
	if _, err := p.Master().Write([]byte{0x13}); err != nil { // XOFF from the device
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		_, _ = b.Write([]byte("held"))
		close(done)
	}()
	if got := m.waitFor(400*time.Millisecond, "held"); got != "" {
		t.Fatalf("output passed despite XOFF: %q", got)
	}
	if _, err := p.Master().Write([]byte{0x11}); err != nil { // XON
		t.Fatal(err)
	}
	if got := m.waitFor(2*time.Second, "held"); got != "held" {
		t.Fatalf("output after XON = %q", got)
	}
	<-done
	if got := col.until(200*time.Millisecond, func(acc []byte) bool { return len(acc) > 0 }); len(got) != 0 {
		t.Fatalf("XON/XOFF leaked into the terminal: %q", got)
	}
}

// TestHardwareFlowControlFlags: RTS/CTS is set in termios (or refused loudly when the tty cannot do it).
func TestHardwareFlowControlFlags(t *testing.T) {
	p := newPty(t)
	port, err := goserial.Open(p.SlaveName(), &goserial.Mode{BaudRate: 9600})
	if err != nil {
		t.Skipf("open: %v", err)
	}
	defer port.Close()
	if err := applyFlowControl(port, flowRTSCTS); err != nil {
		t.Logf("pty refused RTS/CTS (acceptable, reported): %v", err)
		return
	}
	h, _ := portHandle(port)
	tio, _ := unix.IoctlGetTermios(int(h), ioctlGetTermios)
	if uint64(tio.Cflag)&uint64(tcCRTSCTS) != uint64(tcCRTSCTS) {
		t.Fatalf("CRTSCTS not set: cflag %#x", tio.Cflag)
	}
	if err := applyFlowControl(port, flowNone); err != nil {
		t.Fatal(err)
	}
	tio, _ = unix.IoctlGetTermios(int(h), ioctlGetTermios)
	if uint64(tio.Cflag)&uint64(tcCRTSCTS) != 0 {
		t.Fatal("CRTSCTS still set after flow none")
	}
}

// TestDeviceLossIsAnError: the device disappearing (master closed) must end Read with an error, not EOF, so
// autoReconnect kicks in.
func TestDeviceLossIsAnError(t *testing.T) {
	p := newPty(t)
	b := openPtyBackend(t, p, model.Options{})
	col := collect(b)
	p.Master().Close()
	col.until(3*time.Second, func([]byte) bool { return false })
	err := col.error()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("device loss = %v, want a non-EOF error", err)
	}
}

func TestWaitForReplug(t *testing.T) {
	p := newPty(t)
	link := filepath.Join(t.TempDir(), "ttyREPLUG")
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = os.Symlink(p.SlaveName(), link)
	}()
	start := time.Now()
	port, err := waitForDevice(context.Background(), nil, link, &goserial.Mode{BaudRate: 9600})
	if err != nil {
		t.Fatalf("waitForDevice: %v", err)
	}
	port.Close()
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("returned before the device appeared")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := waitForDevice(ctx, nil, filepath.Join(t.TempDir(), "never"), &goserial.Mode{BaudRate: 9600}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestAutoBaudOnPty(t *testing.T) {
	p := newPty(t)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				_, _ = p.Master().Write([]byte("login: "))
			}
		}
	}()
	prev := probeWindow
	probeWindow = 250 * time.Millisecond
	defer func() { probeWindow = prev }()
	res, err := autoBaud(context.Background(), p.SlaveName(), []int{9600, 115200}, false)
	if err != nil {
		t.Fatalf("autobaud: %v", err)
	}
	if res.Baud == 0 || res.Score <= 0.5 || len(res.Tried) != 2 || res.Sample == "" {
		t.Fatalf("result %+v", res)
	}

	// A port in use by a session is reported as busy.
	port, err := goserial.Open(p.SlaveName(), &goserial.Mode{BaudRate: 9600})
	if err != nil {
		t.Skipf("open: %v", err)
	}
	_, err = autoBaud(context.Background(), p.SlaveName(), []int{9600}, false)
	port.Close()
	if err == nil {
		t.Log("this platform does not enforce TIOCEXCL on ptys; busy detection not exercised")
		return
	}
	var pe *goserial.PortError
	if !errors.As(err, &pe) || pe.Code() != goserial.PortBusy {
		t.Fatalf("busy port: %v", err)
	}
}

func TestAutoBaudNoData(t *testing.T) {
	p := newPty(t)
	prev := probeWindow
	probeWindow = 100 * time.Millisecond
	defer func() { probeWindow = prev }()
	if _, err := autoBaud(context.Background(), p.SlaveName(), []int{9600, 19200}, false); err == nil {
		t.Fatal("silence must not yield a baud rate")
	}
}

// collector reads a backend with one goroutine.
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
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *collector) error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// TestCloseReleasesWriterBlockedByXOFF: output held back by flow control must not keep the session's writer blocked
// after Close.
func TestCloseReleasesWriterBlockedByXOFF(t *testing.T) {
	p := newPty(t)
	readMaster(p) // the "device" keeps reading: only XOFF holds the output back
	b := openPtyBackend(t, p, model.Options{"flowControl": "xonxoff"})
	if _, err := p.Master().Write([]byte{0x13}); err != nil { // XOFF
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := b.Write(bytes.Repeat([]byte("x"), 256<<10)) // far more than the tty buffers
		done <- err
	}()
	select {
	case <-done:
		t.Skip("the write did not block on this platform's pty")
	case <-time.After(300 * time.Millisecond):
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("writer still blocked after Close")
	}
}
