package monitor

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// localState caches the AstraTerm host's static description.
type localState struct {
	once sync.Once
	host *hostInfo
}

// localHost describes the AstraTerm host (MON-6), computed once.
func (s *Service) localHost() *hostInfo {
	s.local.once.Do(func() { s.local.host = buildLocalHost() })
	return s.local.host
}

func buildLocalHost() *hostInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := &hostInfo{Platform: runtime.GOOS, Arch: runtime.GOARCH, Tools: map[string]bool{}}
	if hi, err := host.InfoWithContext(ctx); err == nil {
		h.Hostname, h.Kernel = hi.Hostname, hi.KernelVersion
		if hi.KernelArch != "" {
			h.Arch = hi.KernelArch
		}
		h.OS = prettyLocalOS(hi)
		if hi.VirtualizationRole == "guest" {
			h.Virt = hi.VirtualizationSystem
		}
	}
	if h.Hostname == "" {
		h.Hostname, _ = os.Hostname()
	}
	if ci, err := cpu.InfoWithContext(ctx); err == nil && len(ci) > 0 {
		h.CPUModel = strings.Join(strings.Fields(ci[0].ModelName), " ")
	}
	h.Cores, _ = cpu.CountsWithContext(ctx, true)
	if runtime.GOOS == "windows" {
		h.Tools["powershell"] = true
	} else {
		for _, t := range []string{"sudo", "systemctl", "journalctl", "ss", "lsof", "sockstat"} {
			if _, err := exec.LookPath(t); err == nil {
				h.Tools[t] = true
			}
		}
	}
	return h
}

var prettyNameRE = regexp.MustCompile(`(?m)^PRETTY_NAME="?([^"\n]+)"?`)

func prettyLocalOS(hi *host.InfoStat) string {
	switch runtime.GOOS {
	case "linux":
		for _, f := range []string{"/etc/os-release", "/usr/lib/os-release"} {
			if b, err := os.ReadFile(f); err == nil {
				if m := prettyNameRE.FindSubmatch(b); m != nil {
					return string(m[1])
				}
			}
		}
	case "darwin":
		return strings.TrimSpace("macOS " + hi.PlatformVersion)
	case "windows":
		return strings.TrimSpace(strings.TrimPrefix(hi.Platform, "Microsoft "))
	}
	return strings.TrimSpace(hi.Platform + " " + hi.PlatformVersion)
}

// ---- sampler --------------------------------------------------------------------------------------------------------

// localSource samples the AstraTerm host in-process with gopsutil (monitoring of local shell sessions).
type localSource struct{ s *Service }

func (l *localSource) run(ctx context.Context, emit func(*Stats), fail func(state, msg string)) {
	host := l.s.localHost()
	var prev *rawSample
	t := time.NewTicker(max(l.s.Interval, 500*time.Millisecond))
	defer t.Stop()
	first := time.NewTimer(time.Second) // quick second reading so CPU usage and rates settle
	defer first.Stop()
	for {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cur, err := sampleLocal(sctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fail(stateError, "Local monitoring failed: "+err.Error())
		} else {
			emit(computeStats(prev, cur, host))
			prev = cur
		}
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-t.C:
		}
	}
}

var monoStart = time.Now()

// sampleLocal reads the AstraTerm host's counters.
func sampleLocal(ctx context.Context) (*rawSample, error) {
	now := time.Now()
	s := &rawSample{at: now, clock: time.Since(monoStart).Seconds()}
	all, err := cpu.TimesWithContext(ctx, false)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	s.cpu = []cpuTicks{ticksOf(all[0])}
	if per, err := cpu.TimesWithContext(ctx, true); err == nil {
		for _, c := range per {
			s.cpu = append(s.cpu, ticksOf(c))
		}
		s.cores = len(per)
	}
	s.mem = localMem(ctx)
	if la, err := load.AvgWithContext(ctx); err == nil {
		s.load = [3]float64{round2(la.Load1), round2(la.Load5), round2(la.Load15)}
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.uptime = float64(up)
	}
	s.hostname, _ = os.Hostname()
	s.disks = localDisks(ctx)
	s.net = localNet(ctx)
	s.diskIO = localDiskIO(ctx)
	if pids, err := process.PidsWithContext(ctx); err == nil {
		s.procs = len(pids)
	}
	if us, err := host.UsersWithContext(ctx); err == nil {
		s.users = len(us)
	}
	s.fds = localFDs()
	return s, nil
}

// localMem reads the AstraTerm host's memory: used = total − available (like `free`), refined per OS by localMemory.
func localMem(ctx context.Context) MemStats {
	var m MemStats
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		avail := min(vm.Available, vm.Total)
		m = MemStats{Total: int64(vm.Total), Available: int64(avail), Used: int64(vm.Total - avail),
			Cached: int64(vm.Cached + vm.Buffers)}
		if r, ok := localMemory(ctx, m.Total); ok {
			m.Used, m.Available, m.Cached = r.Used, r.Available, r.Cached
		}
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		m.SwapTotal, m.SwapUsed = int64(sw.Total), int64(sw.Used)
	}
	return m
}

func ticksOf(t cpu.TimesStat) cpuTicks {
	c := func(v float64) uint64 {
		if v <= 0 {
			return 0
		}
		return uint64(v * 1000) // milliseconds
	}
	return cpuTicks{user: c(t.User), nice: c(t.Nice), system: c(t.System), idle: c(t.Idle), iowait: c(t.Iowait),
		irq: c(t.Irq), softirq: c(t.Softirq), steal: c(t.Steal)}
}

// localDisks lists local filesystems; each usage call is bounded so a stuck mount cannot block the sampler.
func localDisks(ctx context.Context) []DiskStats {
	// all=true: gopsutil's "physical only" mode (Linux) drops nodev filesystems (overlay) and every bind mount — inside
	// a container (AstraTerm deployed with Docker) that is every filesystem. Pseudo, network and container-runtime
	// mounts are filtered here instead (before any statfs: a hung network mount must not be touched), duplicates merged
	// by mergeLocalDisks.
	parts, err := disk.PartitionsWithContext(ctx, true)
	if err != nil && len(parts) == 0 {
		return []DiskStats{}
	}
	var found []DiskStats
	for _, p := range parts {
		if skipMount(p.Mountpoint, p.Fstype, p.Device, runtime.GOOS) || isNetworkFS(p.Fstype) {
			continue
		}
		if runtime.GOOS != "windows" {
			// Containers bind-mount single files (/etc/hosts, /etc/resolv.conf): not volumes.
			if fi, err := os.Stat(p.Mountpoint); err != nil || !fi.IsDir() {
				continue
			}
		}
		u := usageWithTimeout(ctx, p.Mountpoint, 2*time.Second)
		if u == nil || u.Total == 0 {
			continue
		}
		found = append(found, DiskStats{Mount: p.Mountpoint, FS: p.Fstype, Device: p.Device, Total: int64(u.Total),
			Used: int64(u.Used), Avail: int64(u.Free)})
	}
	return mergeLocalDisks(runtime.GOOS, found)
}

// macDataVolume is where a Mac keeps user files (firmlinked into /).
const macDataVolume = "/System/Volumes/Data"

// apfsContainerRE extracts the container disk of an APFS volume device (/dev/disk3s1s1, /dev/disk3s5 → disk3).
var apfsContainerRE = regexp.MustCompile(`^/dev/(disk\d+)s\d+`)

// mergeLocalDisks merges the filesystems of the AstraTerm host that are one volume: bind mounts of a device and
// identical numbers keep the shortest mount path; the APFS volumes of one container (which all report the container's
// space) become the Data volume — user files live there, so the disk-usage drill-down starts somewhere useful and the
// bar reads like the remote macOS sampler's. The primary volume comes first ("/", the Mac's Data volume, C:).
func mergeLocalDisks(goos string, found []DiskStats) []DiskStats {
	var out []DiskStats
	seen := map[[3]int64]int{}
	seenDev := map[string]int{}
	keep := func(i int, d DiskStats) {
		cur := out[i].Mount
		if goos == "darwin" && (d.Mount == macDataVolume || cur == macDataVolume) {
			if d.Mount == macDataVolume {
				out[i] = d
			}
			return
		}
		if len(d.Mount) < len(cur) {
			out[i] = d
		}
	}
	for _, d := range found {
		devKey := ""
		if strings.HasPrefix(d.Device, "/dev/") {
			devKey = d.Device + "\x00" + d.FS
			if m := apfsContainerRE.FindStringSubmatch(d.Device); m != nil && d.FS == "apfs" {
				devKey = "apfs\x00" + m[1] // volumes of one APFS container share its space (and report it)
			}
		}
		key := [3]int64{d.Total, d.Used, d.Avail}
		if i, ok := seenDev[devKey]; ok && devKey != "" {
			keep(i, d)
			continue
		}
		if i, ok := seen[key]; ok {
			keep(i, d)
			if devKey != "" {
				seenDev[devKey] = i
			}
			continue
		}
		seen[key] = len(out)
		if devKey != "" {
			seenDev[devKey] = len(out)
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Mount == "/") != (out[j].Mount == "/") {
			return out[i].Mount == "/"
		}
		return out[i].Mount < out[j].Mount
	})
	switch goos {
	case "darwin":
		markPrimary(out, macDataVolume)
	case "windows":
		markPrimary(out, "C:")
	}
	if out == nil {
		out = []DiskStats{}
	}
	return out
}

func isNetworkFS(fs string) bool {
	switch strings.ToLower(fs) {
	case "nfs", "nfs4", "cifs", "smb", "smbfs", "smb3", "afpfs", "webdav", "9p", "virtiofs", "ceph", "glusterfs", "afs", "sshfs":
		return true
	}
	return false
}

func usageWithTimeout(ctx context.Context, path string, d time.Duration) *disk.UsageStat {
	ch := make(chan *disk.UsageStat, 1)
	go func() {
		u, err := disk.UsageWithContext(ctx, path)
		if err != nil {
			u = nil
		}
		ch <- u
	}()
	select {
	case u := <-ch:
		return u
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return nil
	}
}

func localNet(ctx context.Context) []netCounter {
	io, err := net.IOCountersWithContext(ctx, true)
	if err != nil {
		return nil
	}
	virtual := linuxVirtualIfaces()
	var out []netCounter
	for _, n := range io {
		name := n.Name
		lower := strings.ToLower(name)
		if name == "lo" || name == "lo0" || strings.HasPrefix(lower, "loopback") {
			continue
		}
		v := virtualIface(name)
		if virtual != nil {
			v = virtual[name]
		}
		out = append(out, netCounter{iface: name, rx: n.BytesRecv, tx: n.BytesSent, virtual: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].iface < out[j].iface })
	return out
}

// linuxVirtualIfaces lists /sys/devices/virtual/net (nil elsewhere).
func linuxVirtualIfaces() map[string]bool {
	if runtime.GOOS != "linux" {
		return nil
	}
	ents, err := os.ReadDir("/sys/devices/virtual/net")
	if err != nil {
		return nil
	}
	m := map[string]bool{}
	for _, e := range ents {
		m[e.Name()] = true
	}
	return m
}

var darwinDiskRE = regexp.MustCompile(`^disk\d+$`)

func localDiskIO(ctx context.Context) *ioCounter {
	m, err := disk.IOCountersWithContext(ctx)
	if err != nil || len(m) == 0 {
		return nil
	}
	var io ioCounter
	found := false
	for name, c := range m {
		switch runtime.GOOS {
		case "linux":
			if !wholeDiskRE.MatchString(name) {
				continue
			}
		case "darwin":
			if !darwinDiskRE.MatchString(name) {
				continue
			}
		}
		found = true
		io.read += c.ReadBytes
		io.write += c.WriteBytes
	}
	if !found {
		return nil
	}
	return &io
}

// ---- System info (GET /api/monitor/local) ---------------------------------------------------------------------------

// SystemInfo is the local System information view (MON-6: MobaSwInfo / MobaHwInfo).
type SystemInfo struct {
	Host struct {
		Hostname        string `json:"hostname"`
		OS              string `json:"os"`
		Platform        string `json:"platform"`
		PlatformFamily  string `json:"platformFamily,omitempty"`
		PlatformVersion string `json:"platformVersion,omitempty"`
		KernelVersion   string `json:"kernelVersion,omitempty"`
		Arch            string `json:"arch"`
		Virtualization  string `json:"virtualization,omitempty"`
		HostID          string `json:"hostId,omitempty"`
		BootTime        string `json:"bootTime,omitempty"`
		UptimeSec       uint64 `json:"uptimeSec"`
		Procs           uint64 `json:"procs"`
		Timezone        string `json:"timezone"`
	} `json:"host"`
	CPU struct {
		Model         string    `json:"model"`
		Vendor        string    `json:"vendor,omitempty"`
		PhysicalCores int       `json:"physicalCores"`
		LogicalCores  int       `json:"logicalCores"`
		Mhz           float64   `json:"mhz,omitempty"`
		CacheKB       int32     `json:"cacheKb,omitempty"`
		Usage         float64   `json:"usage"`
		PerCore       []float64 `json:"perCore"`
	} `json:"cpu"`
	Load   [3]float64   `json:"load"`
	Mem    MemStats     `json:"mem"`
	Disks  []LocalDisk  `json:"disks"`
	Net    []LocalIface `json:"net"`
	Users  []LocalUser  `json:"users"`
	Top    []Process    `json:"topProcesses"`
	Server struct {
		Version    string `json:"version"`
		Mode       string `json:"mode"`
		PID        int    `json:"pid"`
		GoVersion  string `json:"goVersion"`
		Goroutines int    `json:"goroutines"`
		HeapBytes  uint64 `json:"heapBytes"`
		DataDir    string `json:"dataDir,omitempty"`
		Listen     string `json:"listen,omitempty"`
	} `json:"server"`
}

// LocalDisk is one mounted filesystem of the AstraTerm host.
type LocalDisk struct {
	Mount   string  `json:"mount"`
	Device  string  `json:"device"`
	FS      string  `json:"fs"`
	Total   int64   `json:"total"`
	Used    int64   `json:"used"`
	Avail   int64   `json:"avail"`
	Percent float64 `json:"percent"`
}

// LocalIface is one network interface of the AstraTerm host.
type LocalIface struct {
	Name    string   `json:"name"`
	MTU     int      `json:"mtu"`
	MAC     string   `json:"mac,omitempty"`
	Addrs   []string `json:"addrs"`
	Flags   []string `json:"flags"`
	Up      bool     `json:"up"`
	RxBytes uint64   `json:"rxBytes"`
	TxBytes uint64   `json:"txBytes"`
	Virtual bool     `json:"virtual,omitempty"`
}

// LocalUser is a logged-in user session of the AstraTerm host.
type LocalUser struct {
	User     string `json:"user"`
	Terminal string `json:"terminal,omitempty"`
	Host     string `json:"host,omitempty"`
	Started  string `json:"started,omitempty"`
}

func (s *Service) systemInfo(ctx context.Context, user *model.User) (*SystemInfo, error) {
	info := &SystemInfo{}
	h := s.localHost()
	if hi, err := host.InfoWithContext(ctx); err == nil {
		info.Host.Hostname, info.Host.Platform, info.Host.PlatformFamily = hi.Hostname, hi.Platform, hi.PlatformFamily
		info.Host.PlatformVersion, info.Host.KernelVersion, info.Host.HostID = hi.PlatformVersion, hi.KernelVersion, hi.HostID
		info.Host.UptimeSec, info.Host.Procs = hi.Uptime, hi.Procs
		if hi.BootTime > 0 {
			info.Host.BootTime = time.Unix(int64(hi.BootTime), 0).UTC().Format(time.RFC3339)
		}
		if hi.VirtualizationSystem != "" {
			info.Host.Virtualization = strings.TrimSpace(hi.VirtualizationSystem + " " + hi.VirtualizationRole)
		}
	}
	info.Host.OS, info.Host.Arch = h.OS, h.Arch
	if info.Host.Hostname == "" {
		info.Host.Hostname = h.Hostname
	}
	info.Host.Timezone, _ = time.Now().Zone()
	if tz := time.Local.String(); tz != "" && tz != "Local" {
		info.Host.Timezone = tz
	}

	if ci, err := cpu.InfoWithContext(ctx); err == nil && len(ci) > 0 {
		info.CPU.Model = strings.Join(strings.Fields(ci[0].ModelName), " ")
		info.CPU.Vendor, info.CPU.Mhz, info.CPU.CacheKB = ci[0].VendorID, ci[0].Mhz, ci[0].CacheSize
	}
	info.CPU.PhysicalCores, _ = cpu.CountsWithContext(ctx, false)
	info.CPU.LogicalCores, _ = cpu.CountsWithContext(ctx, true)
	if st := s.latest(localKey{}); st != nil {
		info.CPU.Usage, info.CPU.PerCore = st.CPU.Usage, st.CPU.PerCore
	} else if per, err := cpu.PercentWithContext(ctx, 400*time.Millisecond, true); err == nil {
		sum := 0.0
		for i, p := range per {
			per[i] = round1(clampPct(p))
			sum += per[i]
		}
		info.CPU.PerCore = per
		if len(per) > 0 {
			info.CPU.Usage = round1(sum / float64(len(per)))
		}
	}
	if info.CPU.PerCore == nil {
		info.CPU.PerCore = []float64{}
	}
	if la, err := load.AvgWithContext(ctx); err == nil {
		info.Load = [3]float64{round2(la.Load1), round2(la.Load5), round2(la.Load15)}
	}
	info.Mem = localMem(ctx)
	info.Disks = []LocalDisk{}
	for _, d := range localDisks(ctx) {
		pct := 0.0
		if d.Used+d.Avail > 0 {
			pct = round1(float64(d.Used) / float64(d.Used+d.Avail) * 100)
		}
		info.Disks = append(info.Disks, LocalDisk{Mount: d.Mount, Device: d.Device, FS: d.FS, Total: d.Total, Used: d.Used, Avail: d.Avail, Percent: pct})
	}
	info.Net = localInterfaces(ctx)
	info.Users = []LocalUser{}
	if us, err := host.UsersWithContext(ctx); err == nil {
		for _, u := range us {
			lu := LocalUser{User: u.User, Terminal: u.Terminal, Host: u.Host}
			if u.Started > 0 {
				lu.Started = time.Unix(int64(u.Started), 0).UTC().Format(time.RFC3339)
			}
			info.Users = append(info.Users, lu)
		}
	}
	info.Top = []Process{}
	if user != nil {
		t := s.localTarget(user, nil)
		if procs, err := s.processList(ctx, t); err == nil {
			sort.Slice(procs, func(i, j int) bool { return procs[i].CPU > procs[j].CPU })
			info.Top = procs[:min(len(procs), 10)]
		}
	}
	info.Server.PID, info.Server.GoVersion, info.Server.Goroutines = os.Getpid(), runtime.Version(), runtime.NumGoroutine()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	info.Server.HeapBytes = ms.HeapAlloc
	if s.d != nil && s.d.Cfg != nil {
		info.Server.Version, info.Server.Mode, info.Server.DataDir, info.Server.Listen = s.d.Cfg.Version, s.d.Cfg.Mode, s.d.Cfg.DataDir, s.d.Cfg.Listen
	}
	return info, nil
}

func localInterfaces(ctx context.Context) []LocalIface {
	ifs, err := net.InterfacesWithContext(ctx)
	if err != nil {
		return []LocalIface{}
	}
	counters := map[string]net.IOCountersStat{}
	if io, err := net.IOCountersWithContext(ctx, true); err == nil {
		for _, c := range io {
			counters[c.Name] = c
		}
	}
	virtual := linuxVirtualIfaces()
	out := []LocalIface{}
	for _, i := range ifs {
		li := LocalIface{Name: i.Name, MTU: i.MTU, MAC: i.HardwareAddr, Flags: i.Flags, Addrs: []string{}}
		if li.Flags == nil {
			li.Flags = []string{}
		}
		for _, f := range i.Flags {
			if strings.EqualFold(f, "up") {
				li.Up = true
			}
		}
		for _, a := range i.Addrs {
			li.Addrs = append(li.Addrs, a.Addr)
		}
		if c, ok := counters[i.Name]; ok {
			li.RxBytes, li.TxBytes = c.BytesRecv, c.BytesSent
		}
		li.Virtual = virtualIface(i.Name)
		if virtual != nil {
			li.Virtual = virtual[i.Name]
		}
		out = append(out, li)
	}
	return out
}
