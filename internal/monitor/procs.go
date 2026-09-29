package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/nexterm/nexterm/internal/httpx"
)

// Process is one row of a process list: a JSON superset of model.Process (SPEC §6.0). CPU is a percentage of one core
// (like top: a busy multi-threaded process may exceed 100); Mem a percentage of physical memory; RSS and VSZ in bytes;
// Started RFC 3339.
type Process struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSS     int64   `json:"rss"`
	State   string  `json:"state"`
	Started string  `json:"started"`
	Command string  `json:"command"`
	Name    string  `json:"name,omitempty"`
	Threads int     `json:"threads,omitempty"`
	Nice    int     `json:"nice"`
	VSZ     int64   `json:"vsz,omitempty"`
	CPUTime float64 `json:"cpuTime"` // seconds of CPU consumed
}

var (
	errNoProcess = httpx.NotFound("no such process")
	errBadSignal = httpx.BadRequest("unsupported signal")
)

// Signals accepted by the kill endpoint.
var allowedSignals = map[string]bool{"TERM": true, "KILL": true, "INT": true, "HUP": true, "QUIT": true, "STOP": true,
	"CONT": true, "USR1": true, "USR2": true}

// normalizeSignal accepts "TERM", "SIGTERM", "term", "15"-style numbers of the common signals; default TERM.
func normalizeSignal(sig string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(sig))
	s = strings.TrimPrefix(s, "SIG")
	switch s {
	case "":
		return "TERM", nil
	case "1":
		s = "HUP"
	case "2":
		s = "INT"
	case "3":
		s = "QUIT"
	case "9":
		s = "KILL"
	case "15":
		s = "TERM"
	}
	if !allowedSignals[s] {
		return "", errBadSignal
	}
	return s, nil
}

// procState remembers the CPU counters of the previous listing of a target, for top-like CPU percentages.
type procState struct {
	at    time.Time
	clock float64 // host clock in seconds (Linux uptime, else local wall clock)
	cpu   map[int]procTick
	procs []Process // cached result (served again for requests within a second)
}

type procTick struct {
	cpu   float64 // CPU seconds
	start float64 // start time (unix seconds or seconds since boot), to detect PID reuse
}

// processes returns the target's process list (cached for a second; concurrent requests share one listing).
func (s *Service) processes(ctx context.Context, t *target) ([]Process, error) {
	s.mu.Lock()
	if ps := s.procHist[t.id]; ps != nil && ps.procs != nil && time.Since(ps.at) < time.Second {
		out := ps.procs
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()
	v, err, _ := s.sf.Do("procs:"+t.id, func() (any, error) { return s.processList(ctx, t) })
	if err != nil {
		return nil, err
	}
	return v.([]Process), nil
}

// processList lists the target's processes and turns cumulative CPU time into a recent CPU percentage by comparing
// with the previous listing (the first listing shows lifetime averages, like ps).
func (s *Service) processList(ctx context.Context, t *target) ([]Process, error) {
	now := time.Now()
	var procs []Process
	var cur *procState
	var err error
	switch {
	case t.local && runtime.GOOS == "windows":
		procs, cur, err = localWindowsProcesses(ctx, now)
	case t.host.Platform == platLinux:
		var res *result
		if res, err = s.run(ctx, t, command{sh: script(scriptLinuxProcs)}); err == nil {
			procs, cur = parseLinuxProcs(res.Stdout, now)
			if len(procs) == 0 {
				err = fmt.Errorf("process list unavailable: %s", orDefault(res.errText(), "no output"))
			}
		}
	case t.host.Platform == platDarwin || isBSD(t.host.Platform):
		var res *result
		if res, err = s.run(ctx, t, command{sh: script(scriptUnixProcs)}); err == nil {
			procs, cur = parseUnixProcs(res.Stdout, now)
			if len(procs) == 0 {
				err = fmt.Errorf("process list unavailable: %s", orDefault(res.errText(), "no output"))
			}
		}
	case t.host.Platform == platWindows:
		var res *result
		if res, err = s.run(ctx, t, command{ps: script(scriptWindowsProcs)}); err == nil {
			procs, cur, err = parseWindowsProcs(res.Stdout, now)
			if err != nil {
				err = fmt.Errorf("process list unavailable: %s", orDefault(res.errText(), err.Error()))
			}
		}
	default:
		return nil, httpx.NewError(422, "monitor_unavailable", "process listing is not supported on "+t.host.Platform)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	prev := s.procHist[t.id]
	s.mu.Unlock()
	if cur != nil && prev != nil && prev.cpu != nil && time.Since(prev.at) < 2*time.Minute {
		dt := cur.clock - prev.clock
		if dt >= 0.5 {
			for i := range procs {
				p := &procs[i]
				pt, ok := prev.cpu[p.PID]
				ct, ok2 := cur.cpu[p.PID]
				if ok && ok2 && math.Abs(pt.start-ct.start) < 3 {
					p.CPU = round1(max(ct.cpu-pt.cpu, 0) / dt * 100)
				}
			}
		}
	}
	sort.Slice(procs, func(i, j int) bool {
		if procs[i].CPU != procs[j].CPU {
			return procs[i].CPU > procs[j].CPU
		}
		return procs[i].PID < procs[j].PID
	})
	if cur == nil {
		cur = &procState{}
	}
	cur.at, cur.procs = now, procs
	s.mu.Lock()
	if prev == nil || cur.clock-prev.clock >= 0.5 || cur.cpu == nil || time.Since(prev.at) > 2*time.Minute {
		s.procHist[t.id] = cur
	} else { // too close to the previous baseline: keep it for the next delta, but cache this result
		prev.procs, prev.at = procs, now
	}
	s.mu.Unlock()
	return procs, nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// parseLinuxProcs parses the Linux process script: /proc/<pid>/stat lines joined with ps user/args.
func parseLinuxProcs(out []byte, now time.Time) ([]Process, *procState) {
	sec := sections(splitLines(out))
	hz := atof(sec["HZ"].first())
	if hz <= 0 {
		hz = 100
	}
	page := atoi64(sec["PAGE"].first())
	if page <= 0 {
		page = 4096
	}
	uptime := atof(sec["UPTIME"].first())
	memTotal := atoi64(sec["MEMTOTAL"].first()) * 1024
	self := sec["SELF"].int() // the listing script: hidden with its children
	boot := now.Add(-time.Duration(uptime * float64(time.Second)))

	type psRow struct{ user, args string }
	rows := map[int]psRow{}
	if ps := sec["PS"]; ps != nil {
		for _, l := range ps.lines {
			f, rest := fieldsN(l, 2)
			if len(f) < 2 || !isNum(f[0]) {
				continue // header of the BusyBox form
			}
			pid, _ := strconv.Atoi(f[0])
			rows[pid] = psRow{user: f[1], args: rest}
		}
	}
	state := &procState{clock: uptime, cpu: map[int]procTick{}}
	var procs []Process
	if st := sec["STAT"]; st != nil {
		for _, l := range st.lines {
			p, tick, ok := parseProcStat(l, hz, page, uptime, boot)
			if !ok || (self > 0 && (p.PID == self || p.PPID == self)) {
				continue
			}
			if memTotal > 0 {
				p.Mem = round1(float64(p.RSS) / float64(memTotal) * 100)
			}
			if r, ok := rows[p.PID]; ok {
				p.User = r.user
				p.Command = clip(strings.TrimSpace(r.args), 4096)
			}
			if p.Command == "" {
				p.Command = "[" + p.Name + "]"
			}
			state.cpu[p.PID] = tick
			procs = append(procs, p)
		}
	}
	return procs, state
}

// parseProcStat parses one /proc/<pid>/stat line (the command name may contain spaces and parentheses).
func parseProcStat(l string, hz float64, page int64, uptime float64, boot time.Time) (Process, procTick, bool) {
	open, closeIdx := strings.IndexByte(l, '('), strings.LastIndexByte(l, ')')
	if open < 1 || closeIdx < open || closeIdx+2 > len(l) {
		return Process{}, procTick{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(l[:open]))
	if err != nil || pid <= 0 {
		return Process{}, procTick{}, false
	}
	f := strings.Fields(l[closeIdx+1:])
	if len(f) < 22 {
		return Process{}, procTick{}, false
	}
	utime, stime := atof(f[11]), atof(f[12])
	start := atof(f[19])
	cpuSec := (utime + stime) / hz
	p := Process{
		PID:     pid,
		PPID:    int(atoi64(f[1])),
		State:   f[0],
		Name:    l[open+1 : closeIdx],
		Nice:    int(atoi64(f[16])),
		Threads: int(atoi64(f[17])),
		VSZ:     atoi64(f[20]),
		RSS:     atoi64(f[21]) * page,
		CPUTime: round2(cpuSec),
	}
	if uptime > 0 {
		startSec := start / hz
		p.Started = boot.Add(time.Duration(startSec * float64(time.Second))).UTC().Format(time.RFC3339)
		if elapsed := uptime - startSec; elapsed > 0 {
			p.CPU = round1(cpuSec / elapsed * 100)
		}
	}
	return p, procTick{cpu: cpuSec, start: start / hz}, true
}

// fieldsN splits the first n whitespace-separated fields of l and returns the rest of the line (original spacing).
func fieldsN(l string, n int) ([]string, string) {
	var out []string
	rest := strings.TrimLeft(l, " \t")
	for len(out) < n && rest != "" {
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			out = append(out, rest)
			rest = ""
			break
		}
		out = append(out, rest[:i])
		rest = strings.TrimLeft(rest[i:], " \t")
	}
	return out, rest
}

// parseUnixProcs parses `ps -axww -o pid=,ppid=,%cpu=,%mem=,rss=,vsz=,state=,nice=,etime=,time=,user=,command=`
// (macOS and the BSDs; rss / vsz in KiB).
func parseUnixProcs(out []byte, now time.Time) ([]Process, *procState) {
	sec := sections(splitLines(out))
	var procs []Process
	state := &procState{clock: float64(now.UnixNano()) / 1e9, cpu: map[int]procTick{}}
	ps := sec["PS"]
	if ps == nil {
		return nil, nil
	}
	self := sec["SELF"].int() // the listing script: hidden with its children
	for _, l := range ps.lines {
		f, cmd := fieldsN(l, 11)
		if len(f) < 11 || !isNum(f[0]) {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		if self > 0 && (pid == self || ppid == self) {
			continue
		}
		nice, _ := strconv.Atoi(f[7])
		elapsed := parseClock(f[8])
		cpuSec := parseClock(f[9])
		p := Process{PID: pid, PPID: ppid, CPU: round1(atof(f[2])), Mem: round1(atof(f[3])), RSS: atoi64(f[4]) * 1024,
			VSZ: atoi64(f[5]) * 1024, State: f[6], Nice: nice, User: f[10], CPUTime: round2(cpuSec),
			Command: clip(strings.TrimSpace(cmd), 4096)}
		if elapsed >= 0 {
			started := now.Add(-time.Duration(elapsed * float64(time.Second))).UTC()
			p.Started = started.Format(time.RFC3339)
			state.cpu[pid] = procTick{cpu: cpuSec, start: float64(started.UnixNano()) / 1e9}
		}
		if i := strings.LastIndexByte(firstWord(p.Command), '/'); i >= 0 {
			p.Name = firstWord(p.Command)[i+1:]
		} else {
			p.Name = firstWord(p.Command)
		}
		procs = append(procs, p)
	}
	return procs, state
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// parseClock parses ps time formats "[[dd-]hh:]mm:ss[.cc]" into seconds (-1 when malformed).
func parseClock(v string) float64 {
	v = strings.TrimSpace(v)
	if v == "" || v == "-" {
		return -1
	}
	days := 0.0
	if d, rest, ok := strings.Cut(v, "-"); ok {
		days = atof(d)
		v = rest
	}
	parts := strings.Split(v, ":")
	total := 0.0
	for _, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return -1
		}
		total = total*60 + n
	}
	return days*86400 + total
}

type winProc struct {
	P   int     `json:"p"`
	PP  int     `json:"pp"`
	N   string  `json:"n"`
	C   *string `json:"c"`
	WS  int64   `json:"ws"`
	VS  int64   `json:"vs"`
	CPU int64   `json:"cpu"` // 100 ns units
	TH  int     `json:"th"`
	PR  int     `json:"pr"`
	S   int64   `json:"s"` // seconds since start
	U   *string `json:"u"`
}

// parseWindowsProcs parses the PowerShell process list ({mem, procs}).
func parseWindowsProcs(out []byte, now time.Time) ([]Process, *procState, error) {
	var doc struct {
		Mem   int64     `json:"mem"`
		Self  int       `json:"self"`
		Procs []winProc `json:"procs"`
	}
	line := ""
	for _, l := range splitLines(out) {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "{") {
			line = t
		}
	}
	if line == "" {
		return nil, nil, fmt.Errorf("no process data")
	}
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return nil, nil, err
	}
	state := &procState{clock: float64(now.UnixNano()) / 1e9, cpu: map[int]procTick{}}
	procs := make([]Process, 0, len(doc.Procs))
	for _, w := range doc.Procs {
		if doc.Self > 0 && (w.P == doc.Self || w.PP == doc.Self) {
			continue // the listing PowerShell itself
		}
		cpuSec := float64(w.CPU) / 1e7
		p := Process{PID: w.P, PPID: w.PP, Name: w.N, Command: w.N, RSS: w.WS, VSZ: w.VS, Threads: w.TH,
			CPUTime: round2(cpuSec), Nice: winNice(w.PR), State: "R"}
		if w.C != nil && strings.TrimSpace(*w.C) != "" {
			p.Command = clip(strings.TrimSpace(*w.C), 4096)
		}
		if w.U != nil {
			p.User = *w.U
		}
		if doc.Mem > 0 {
			p.Mem = round1(float64(w.WS) / float64(doc.Mem) * 100)
		}
		startAt := 0.0
		if w.S >= 0 {
			started := now.Add(-time.Duration(w.S) * time.Second)
			p.Started = started.UTC().Format(time.RFC3339)
			startAt = float64(started.Unix())
			if w.S > 0 {
				p.CPU = round1(cpuSec / float64(w.S) * 100)
			}
		}
		state.cpu[w.P] = procTick{cpu: cpuSec, start: startAt}
		procs = append(procs, p)
	}
	return procs, state, nil
}

// winNice maps a Windows base priority to the Unix nice scale (for display and sorting).
func winNice(prio int) int {
	switch {
	case prio <= 0:
		return 0
	case prio <= 4:
		return 19
	case prio <= 6:
		return 10
	case prio <= 8:
		return 0
	case prio <= 10:
		return -5
	case prio <= 13:
		return -10
	}
	return -20
}

// localWindowsProcesses lists the processes of a Windows NexTerm host natively (gopsutil).
func localWindowsProcesses(ctx context.Context, now time.Time) ([]Process, *procState, error) {
	list, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	var total uint64
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		total = vm.Total
	}
	state := &procState{clock: float64(now.UnixNano()) / 1e9, cpu: map[int]procTick{}}
	procs := make([]Process, 0, len(list))
	for _, pr := range list {
		p := Process{PID: int(pr.Pid), State: "R"}
		if pp, err := pr.PpidWithContext(ctx); err == nil {
			p.PPID = int(pp)
		}
		p.Name, _ = pr.NameWithContext(ctx)
		p.Command, _ = pr.CmdlineWithContext(ctx)
		if strings.TrimSpace(p.Command) == "" {
			p.Command = p.Name
		}
		p.Command = clip(p.Command, 4096)
		p.User, _ = pr.UsernameWithContext(ctx)
		if mi, err := pr.MemoryInfoWithContext(ctx); err == nil {
			p.RSS, p.VSZ = int64(mi.RSS), int64(mi.VMS)
			if total > 0 {
				p.Mem = round1(float64(mi.RSS) / float64(total) * 100)
			}
		}
		if th, err := pr.NumThreadsWithContext(ctx); err == nil {
			p.Threads = int(th)
		}
		var cpuSec, startAt float64
		if ts, err := pr.TimesWithContext(ctx); err == nil {
			cpuSec = ts.User + ts.System
			p.CPUTime = round2(cpuSec)
		}
		if ct, err := pr.CreateTimeWithContext(ctx); err == nil && ct > 0 {
			started := time.UnixMilli(ct)
			p.Started = started.UTC().Format(time.RFC3339)
			startAt = float64(ct) / 1000
			if el := now.Sub(started).Seconds(); el > 0 {
				p.CPU = round1(cpuSec / el * 100)
			}
		}
		state.cpu[p.PID] = procTick{cpu: cpuSec, start: startAt}
		procs = append(procs, p)
	}
	return procs, state, nil
}

// ---- actions --------------------------------------------------------------------------------------------------------

// kill sends a signal to a process of the target (optionally through sudo).
func (s *Service) kill(ctx context.Context, t *target, pid int, sig string, sudo bool) error {
	if pid <= 1 {
		return httpx.BadRequest("refusing to signal PID 0 or 1")
	}
	if t.local && pid == os.Getpid() {
		return httpx.Forbidden("refusing to signal the NexTerm server itself")
	}
	sig, err := normalizeSignal(sig)
	if err != nil {
		return err
	}
	if t.host.Platform == platWindows {
		if sudo {
			return httpx.BadRequest("sudo is not available on Windows hosts")
		}
		if t.local {
			return signalLocal(pid, sig)
		}
		if sig == "STOP" || sig == "CONT" || sig == "USR1" || sig == "USR2" {
			return errBadSignal
		}
		res, err := s.run(ctx, t, command{ps: fmt.Sprintf(
			"$ErrorActionPreference='Stop';try{Stop-Process -Id %d -Force}catch{[Console]::Error.WriteLine($_.Exception.Message);exit 1}", pid)})
		if err != nil {
			return err
		}
		return actionError(res, "the process")
	}
	if t.local && !sudo {
		return signalLocal(pid, sig)
	}
	res, err := s.execArgv(ctx, t, []string{"kill", "-" + sig, strconv.Itoa(pid)}, sudo)
	if err != nil {
		return err
	}
	return actionError(res, "the process")
}

// renice changes a process's scheduling priority (nice −20…19; Windows priority classes).
func (s *Service) renice(ctx context.Context, t *target, pid, nice int, sudo bool) error {
	if pid <= 0 {
		return httpx.BadRequest("invalid pid")
	}
	if nice < -20 || nice > 19 {
		return httpx.BadRequest("nice must be between -20 and 19")
	}
	if t.host.Platform == platWindows {
		if sudo {
			return httpx.BadRequest("sudo is not available on Windows hosts")
		}
		class := "Normal"
		switch {
		case nice <= -15:
			class = "High"
		case nice < 0:
			class = "AboveNormal"
		case nice >= 15:
			class = "Idle"
		case nice > 0:
			class = "BelowNormal"
		}
		res, err := s.run(ctx, t, command{ps: fmt.Sprintf(
			"$ErrorActionPreference='Stop';try{(Get-Process -Id %d).PriorityClass='%s'}catch{[Console]::Error.WriteLine($_.Exception.Message);exit 1}", pid, class)})
		if err != nil {
			return err
		}
		return actionError(res, "the process")
	}
	res, err := s.execArgv(ctx, t, []string{"renice", strconv.Itoa(nice), "-p", strconv.Itoa(pid)}, sudo)
	if err != nil {
		return err
	}
	return actionError(res, "the process")
}

// actionError maps a failed action's output to an API error.
func actionError(res *result, what string) error {
	if res.ok() {
		return nil
	}
	msg := res.errText()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "no such process") || strings.Contains(lower, "cannot find a process") ||
		strings.Contains(lower, "not found") && strings.Contains(lower, "process"):
		return errNoProcess
	case permissionDenied(msg):
		return errPermission(orDefault(msg, "permission denied"))
	}
	if msg == "" {
		msg = fmt.Sprintf("the command failed on %s (exit status %d)", what, res.Code)
	}
	return httpx.NewError(422, "command_failed", msg)
}
