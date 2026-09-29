package monitor

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// winSample is one JSON line of the Windows sampler (raw perf counters; see scripts/windows_loop.ps1).
type winSample struct {
	H   string `json:"h"`
	Up  int64  `json:"up"`
	MT  uint64 `json:"mt"`
	MF  uint64 `json:"mf"`
	PT  uint64 `json:"pt"`
	PU  uint64 `json:"pu"`
	NP  int    `json:"np"`
	TH  int    `json:"th"`
	QL  int    `json:"ql"`
	U   int    `json:"u"`
	CPU []struct {
		N  string `json:"n"`
		I  uint64 `json:"i"` // idle time (PercentProcessorTime is an inverse 100 ns timer)
		U  uint64 `json:"u"`
		K  uint64 `json:"k"`
		TS uint64 `json:"ts"`
	} `json:"cpu"`
	Net []struct {
		N string `json:"n"`
		R uint64 `json:"r"`
		T uint64 `json:"t"`
	} `json:"net"`
	Dk []struct {
		M string `json:"m"`
		F string `json:"f"`
		S uint64 `json:"s"`
		V uint64 `json:"v"`
	} `json:"dk"`
}

// parseWindowsSample parses one JSON line of the Windows sampler.
func parseWindowsSample(line []byte, at time.Time) (*rawSample, bool) {
	var w winSample
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, false
	}
	s := &rawSample{at: at, hostname: w.H, uptime: float64(w.Up), procs: w.NP, threads: w.TH, users: max(w.U, 0)}
	// Windows has no load average; the processor queue length is the closest instantaneous equivalent.
	s.load = [3]float64{float64(w.QL), 0, 0}
	total := int64(w.MT)
	free := min(int64(w.MF), total)
	s.mem = MemStats{Total: total, Available: free, Used: total - free, SwapTotal: int64(w.PT), SwapUsed: int64(min(w.PU, w.PT))}

	type core struct {
		idx int
		t   cpuTicks
	}
	var cores []core
	for _, c := range w.CPU {
		t := cpuTicks{user: c.U, system: c.K, idle: c.I}
		if c.N == "_Total" {
			s.cpu = append([]cpuTicks{t}, s.cpu...)
			s.clock = float64(c.TS) / 1e7
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(c.N)); err == nil {
			cores = append(cores, core{n, t})
		}
	}
	if len(s.cpu) == 1 {
		sort.Slice(cores, func(i, j int) bool { return cores[i].idx < cores[j].idx })
		for _, c := range cores {
			s.cpu = append(s.cpu, c.t)
		}
		s.cores = len(cores)
	} else {
		s.cpu = nil
	}

	for _, n := range w.Net {
		if n.N == "" {
			continue
		}
		s.net = append(s.net, netCounter{iface: n.N, rx: n.R, tx: n.T, virtual: virtualIface(n.N)})
	}
	for _, d := range w.Dk {
		if d.S == 0 {
			continue
		}
		s.disks = append(s.disks, DiskStats{Mount: d.M, FS: d.F, Total: int64(d.S), Avail: int64(min(d.V, d.S)), Used: int64(d.S - min(d.V, d.S))})
	}
	sort.SliceStable(s.disks, func(i, j int) bool { return s.disks[i].Mount < s.disks[j].Mount })
	markPrimary(s.disks, "C:")
	if s.disks == nil {
		s.disks = []DiskStats{}
	}
	return s, true
}
