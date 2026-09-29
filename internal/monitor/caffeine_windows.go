package monitor

import (
	"errors"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
)

func inhibitMethod() string { return "SetThreadExecutionState" }

func inhibitSupported() bool { return procSetThreadExecutionState.Find() == nil }

// winInhibitor holds ES_CONTINUOUS|ES_SYSTEM_REQUIRED|ES_DISPLAY_REQUIRED on a dedicated OS thread (the execution
// state belongs to the thread that set it) until released; Windows also drops it when the process exits.
type winInhibitor struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func startInhibit() (inhibitor, error) {
	if err := procSetThreadExecutionState.Find(); err != nil {
		return nil, errors.New("SetThreadExecutionState is not available")
	}
	w := &winInhibitor{stop: make(chan struct{}), done: make(chan struct{})}
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(w.done)
		r, _, e := procSetThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired | esDisplayRequired))
		if r == 0 {
			errc <- e
			return
		}
		errc <- nil
		<-w.stop
		_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous))
	}()
	if err := <-errc; err != nil {
		return nil, err
	}
	return w, nil
}

func (w *winInhibitor) alive() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

func (w *winInhibitor) release() {
	w.once.Do(func() {
		close(w.stop)
		<-w.done
	})
}
