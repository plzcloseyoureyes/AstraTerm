package automation

import (
	"errors"
	"log/slog"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"sync"
	"time"

	"github.com/dop251/goja"
	"golang.org/x/time/rate"
)

// Script resource guards. goja has no allocation hooks, so memory is bounded by a watchdog: while scripts run, the
// Go heap is sampled every 50 ms and, when it grew by more than the script heap budget over the level it had before
// the first script started, every running script is interrupted with errScriptMemory (the process cannot tell which
// VM allocated; a runaway script is the only plausible cause of such growth). Budget: ASTRATERM_SCRIPT_HEAP_MB
// (default 1 GiB; the baseline follows the heap down, so garbage collected later does not count). CPU time is bounded by the run timeout, call depth by SetMaxCallStackSize, output by caps.

var errScriptMemory = errors.New("the script used too much memory and was stopped")

const (
	defaultScriptHeapBudget = 1 << 30
	heapSampleEvery         = 50 * time.Millisecond
)

type heapWatch struct {
	mu       sync.Mutex
	vms      map[*goja.Runtime]struct{}
	baseline uint64
	running  bool
	budget   uint64
	sample   func() uint64
	log      *slog.Logger
}

var scriptHeap = &heapWatch{vms: map[*goja.Runtime]struct{}{}, budget: heapBudgetFromEnv(), sample: heapObjectBytes}

func heapBudgetFromEnv() uint64 {
	if v, err := strconv.Atoi(os.Getenv("ASTRATERM_SCRIPT_HEAP_MB")); err == nil && v >= 32 {
		return uint64(v) << 20
	}
	return defaultScriptHeapBudget
}

func heapObjectBytes() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindUint64 {
		return s[0].Value.Uint64()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// add registers a running VM (starting the watchdog); the returned func unregisters it.
func (w *heapWatch) add(vm *goja.Runtime, log *slog.Logger) func() {
	w.mu.Lock()
	if len(w.vms) == 0 {
		w.baseline = w.sample()
	}
	w.vms[vm] = struct{}{}
	if log != nil {
		w.log = log
	}
	if !w.running {
		w.running = true
		go w.loop()
	}
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.vms, vm)
		w.mu.Unlock()
	}
}

func (w *heapWatch) loop() {
	t := time.NewTicker(heapSampleEvery)
	defer t.Stop()
	for range t.C {
		w.mu.Lock()
		if len(w.vms) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		heap := w.sample()
		if heap < w.baseline {
			w.baseline = heap
		}
		if heap > w.baseline+w.budget {
			for vm := range w.vms {
				vm.Interrupt(errScriptMemory)
				delete(w.vms, vm)
			}
			if w.log != nil {
				w.log.Warn("scripts interrupted: heap budget exceeded", "heapMiB", heap>>20, "baselineMiB", w.baseline>>20,
					"budgetMiB", w.budget>>20)
			}
			w.running = false
			w.mu.Unlock()
			go runtime.GC()
			return
		}
		w.mu.Unlock()
	}
}

// logThrottle bounds the live log events of one run (the run log itself keeps a bounded tail of everything): a
// script printing in a tight loop must not flood the owner's events socket.
type logThrottle struct {
	mu       sync.Mutex
	lim      *rate.Limiter
	sent     int
	dropped  int
	lastWarn time.Time
}

const (
	maxLogEventsPerRun = 20000
	logEventsPerSec    = 200
	logEventsBurst     = 500
)

func newLogThrottle() *logThrottle {
	return &logThrottle{lim: rate.NewLimiter(logEventsPerSec, logEventsBurst)}
}

// allow reports whether a log line may be emitted as an event; when lines were dropped it returns a notice to emit
// (at most once per second).
func (l *logThrottle) allow() (ok bool, notice string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent < maxLogEventsPerRun && l.lim.Allow() {
		l.sent++
		return true, ""
	}
	l.dropped++
	if time.Since(l.lastWarn) >= time.Second {
		l.lastWarn = time.Now()
		return false, strconv.Itoa(l.dropped) + " log lines were not streamed (too much output); the run log keeps the last ones"
	}
	return false, ""
}
