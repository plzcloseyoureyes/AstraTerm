package serial

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	goserial "go.bug.st/serial"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

func TestBuildMode(t *testing.T) {
	m, flow, err := buildMode(model.Options{"baud": 115200, "dataBits": 7, "parity": "even", "stopBits": "2", "flowControl": "rtscts", "dtr": false})
	if err != nil {
		t.Fatal(err)
	}
	if m.BaudRate != 115200 || m.DataBits != 7 || m.Parity != goserial.EvenParity || m.StopBits != goserial.TwoStopBits || flow != flowRTSCTS {
		t.Fatalf("mode = %+v flow=%s", m, flow)
	}
	if m.InitialStatusBits == nil || m.InitialStatusBits.DTR || !m.InitialStatusBits.RTS {
		t.Fatalf("initial lines = %+v, want DTR off, RTS on", m.InitialStatusBits)
	}
	def, flow, err := buildMode(model.Options{})
	if err != nil || def.BaudRate != 9600 || def.DataBits != 8 || def.Parity != goserial.NoParity || def.StopBits != goserial.OneStopBit || flow != flowNone {
		t.Fatalf("defaults = %+v flow=%s err=%v", def, flow, err)
	}
	// By default the lines are left to the OS (no modem-control ioctl at open: some drivers lack it).
	if def.InitialStatusBits != nil {
		t.Fatalf("default initial lines = %+v, want nil", def.InitialStatusBits)
	}
	if _, f, _ := buildMode(model.Options{"flowControl": "XON/XOFF"}); f != flowXONXOFF {
		t.Errorf("flow alias = %s", f)
	}
	for _, bad := range []model.Options{
		{"parity": "bogus"}, {"baud": 0}, {"baud": -5}, {"dataBits": 9}, {"stopBits": "3"}, {"flowControl": "carrier-pigeon"},
	} {
		if _, _, err := buildMode(bad); err == nil {
			t.Errorf("options %v accepted", bad)
		}
	}
	_, _, err = buildMode(model.Options{"stopBits": "1.5"})
	if (err == nil) != (runtime.GOOS == "windows") {
		t.Errorf("1.5 stop bits on %s: err=%v", runtime.GOOS, err)
	}
}

func TestClassifyOpenError(t *testing.T) {
	_, err := goserial.Open("/nonexistent-nexterm-serial-test", &goserial.Mode{BaudRate: 9600})
	if err == nil {
		t.Skip("unexpected success opening a bogus device")
	}
	if !isNotFound(err) {
		t.Errorf("missing device not recognised as not-found: %v", err)
	}
	if term.IsPermanent(classifyOpenError("/x", err, false)) {
		t.Error("a missing device must stay retryable (replug)")
	}
	if term.IsPermanent(classifyOpenError("/dev/x", io.ErrUnexpectedEOF, false)) {
		t.Error("plain error should be retryable")
	}
}

func TestScoreBytes(t *testing.T) {
	if s := scoreBytes([]byte("hello world, this is a console\r\n")); s < 0.99 {
		t.Fatalf("text score = %v", s)
	}
	if s := scoreBytes([]byte{0x00, 0x01, 0xff, 0xfe, 0x80, 0x00, 0xff, 0x13, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff}); s != 0 {
		t.Fatalf("garbage score = %v", s)
	}
	// Two printable bytes are weak evidence compared with a full line.
	if short, long := scoreBytes([]byte("ab")), scoreBytes([]byte("login: please enter your name\r\n")); short >= long {
		t.Fatalf("short sample %v should score below a long one %v", short, long)
	}
	if scoreBytes(nil) != 0 {
		t.Error("empty score should be 0")
	}
}

func TestPortHandleRejectsForeignTypes(t *testing.T) {
	if _, err := portHandle(nil); !errors.Is(err, errNoHandle) {
		t.Fatalf("nil port: %v", err)
	}
	if _, err := portHandle(fakePort{}); !errors.Is(err, errNoHandle) {
		t.Fatalf("non-pointer port: %v", err)
	}
	if err := applyFlowControl(fakePort{}, flowNone); err != nil {
		t.Fatalf("flow none on a foreign port should be a no-op: %v", err)
	}
	if err := applyFlowControl(fakePort{}, flowRTSCTS); err == nil {
		t.Fatal("flow control on a port without a handle must fail loudly")
	}
}

type fakePort struct{ goserial.Port }

func TestValidateDevicePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		for _, ok := range []string{"COM3", `\\.\COM12`, `\\.\CNCA0`} {
			if err := validateDevice(ok); err != nil {
				t.Errorf("%s rejected: %v", ok, err)
			}
		}
		for _, bad := range []string{`C:\Windows\win.ini`, `..\x`, "COM3:", ""} {
			if validateDevice(bad) == nil {
				t.Errorf("%q accepted", bad)
			}
		}
		return
	}
	for _, bad := range []string{"", "ttyUSB0", "/etc/passwd", "/tmp", "/dev/../etc/passwd", "/tmp/not-there"} {
		if err := validateDevice(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := validateDevice("/dev/ttyNEXTERM-not-plugged-in"); err != nil {
		t.Errorf("a not (yet) present device under /dev must be accepted (replug): %v", err)
	}
	if err := validateDevice("/dev/null"); err != nil {
		t.Errorf("/dev/null is a character device: %v", err)
	}
	if !strings.Contains(validateDevice("/etc/passwd").Error(), "not a device") {
		t.Errorf("message: %v", validateDevice("/etc/passwd"))
	}
}
