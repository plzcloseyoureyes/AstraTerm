package monitor

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stats is one monitor sample, pushed as {type:'monitor', sessionId, stats} and returned by the snapshot endpoint. It
// is a JSON superset of model.MonitorStats (SPEC §6.0): every field of the contract keeps its name and meaning; the
// rest are additions (CPU breakdown, file descriptors, disk I/O, the physical network total, ...).
type Stats struct {
	TS        time.Time   `json:"ts"`
	OS        string      `json:"os"`
	Hostname  string      `json:"hostname"`
	Kernel    string      `json:"kernel"`
	UptimeSec int64       `json:"uptimeSec"`
	Load      [3]float64  `json:"load"`
	CPU       CPUStats    `json:"cpu"`
	Mem       MemStats    `json:"mem"`
	Disks     []DiskStats `json:"disks"`
	Net       []NetStats  `json:"net"`
	Users     int         `json:"users"`
	Processes int         `json:"processes"`

	Platform    string   `json:"platform"`
	Arch        string   `json:"arch,omitempty"`
	CPUModel    string   `json:"cpuModel,omitempty"`
	Virt        string   `json:"virt,omitempty"`
	Threads     int      `json:"threads,omitempty"`
	FDs         *FDStats `json:"fds,omitempty"`
	DiskIO      *IORate  `json:"diskIo,omitempty"`
	NetTotal    NetRate  `json:"netTotal"`
	IntervalSec float64  `json:"intervalSec"`
	// Warmup marks the first sample of a collector: rates (network, disk I/O) need a second sample.
	Warmup bool `json:"warmup,omitempty"`
}

// CPUStats: usage and per-core usage in percent (0–100) plus the breakdown of the usage.
type CPUStats struct {
	Usage   float64   `json:"usage"`
	Cores   int       `json:"cores"`
	PerCore []float64 `json:"perCore"`
	User    float64   `json:"user"`
	System  float64   `json:"system"`
	IOWait  float64   `json:"iowait"`
	Steal   float64   `json:"steal"`
}

// MemStats in bytes. Used = Total − Available; Cached = page cache + buffers (reclaimable).
type MemStats struct {
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
	SwapTotal int64 `json:"swapTotal"`
	SwapUsed  int64 `json:"swapUsed"`
	Cached    int64 `json:"cached,omitempty"`
}

// DiskStats is one mounted filesystem (bytes). Avail excludes reserved blocks, so df's "Use%" is Used/(Used+Avail).
type DiskStats struct {
	Mount  string `json:"mount"`
	FS     string `json:"fs"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Avail  int64  `json:"avail"`
	Device string `json:"device,omitempty"`
}

// NetStats is one network interface: rates in bytes/s, totals in bytes since boot.
type NetStats struct {
	Iface   string  `json:"iface"`
	RxBps   float64 `json:"rxBps"`
	TxBps   float64 `json:"txBps"`
	RxTotal int64   `json:"rxTotal"`
	TxTotal int64   `json:"txTotal"`
	Virtual bool    `json:"virtual,omitempty"`
}

// FDStats: open file handles and the system limit (0 = unknown / unlimited).
type FDStats struct {
	Used int64 `json:"used"`
	Max  int64 `json:"max"`
}

// IORate is a read/write throughput in bytes/s.
type IORate struct {
	ReadBps  float64 `json:"readBps"`
	WriteBps float64 `json:"writeBps"`
}

// NetRate is the receive/transmit throughput of the host's physical interfaces (all non-loopback interfaces when it
// has no physical one, e.g. in a container).
type NetRate struct {
	RxBps float64 `json:"rxBps"`
	TxBps float64 `json:"txBps"`
}

// ---- raw samples ----------------------------------------------------------------------------------------------------

// cpuTicks are cumulative CPU time counters (any unit).
type cpuTicks struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (t cpuTicks) busy() uint64  { return t.user + t.nice + t.system + t.irq + t.softirq + t.steal }
func (t cpuTicks) total() uint64 { return t.busy() + t.idle + t.iowait }

// cpuPercent is a CPU usage measured by the host itself (macOS iostat window).
type cpuPercent struct {
	user, system, idle float64
}

type netCounter struct {
	iface   string
	rx, tx  uint64
	virtual bool
}

type ioCounter struct{ read, write uint64 }

// rawSample is one parsed sample before rates are computed against the previous one.
type rawSample struct {
	at       time.Time // receive time
	clock    float64   // seconds on a monotonic clock of the host (uptime) or the host's wall clock; 0 = unknown
	uptime   float64
	hostname string
	load     [3]float64
	threads  int
	cpu      []cpuTicks  // [0] = all CPUs, [1:] = per core
	cpuPct   *cpuPercent // when the host reports percentages directly
	cores    int
	mem      MemStats
	net      []netCounter
	diskIO   *ioCounter
	disks    []DiskStats
	users    int
	procs    int
	fds      *FDStats
}

// computeStats turns a raw sample into Stats, deriving CPU usage and rates from the previous sample of the same
// collector (nil for the first one).
func computeStats(prev, cur *rawSample, host *hostInfo) *Stats {
	st := &Stats{
		TS:        cur.at.UTC(),
		UptimeSec: int64(cur.uptime),
		Load:      cur.load,
		Users:     cur.users,
		Processes: cur.procs,
		Threads:   cur.threads,
		Mem:       cur.mem,
		FDs:       cur.fds,
		Disks:     cur.disks,
		Net:       []NetStats{},
	}
	if host != nil {
		st.OS, st.Kernel, st.Platform, st.Arch, st.CPUModel, st.Virt = host.OS, host.Kernel, host.Platform, host.Arch, host.CPUModel, host.Virt
		st.Hostname = host.Hostname
	}
	if cur.hostname != "" {
		st.Hostname = cur.hostname
	}
	if st.Disks == nil {
		st.Disks = []DiskStats{}
	}

	dt := 0.0
	if prev != nil {
		if cur.clock > 0 && prev.clock > 0 {
			dt = cur.clock - prev.clock
		}
		if dt <= 0 {
			dt = cur.at.Sub(prev.at).Seconds()
		}
	}
	st.IntervalSec = round2(dt)
	st.Warmup = prev == nil || dt <= 0

	// CPU
	switch {
	case cur.cpuPct != nil:
		p := cur.cpuPct
		st.CPU.User, st.CPU.System = round1(p.user), round1(p.system)
		st.CPU.Usage = round1(clampPct(100 - p.idle))
	case len(cur.cpu) > 0:
		var pc []cpuTicks
		if prev != nil && len(prev.cpu) == len(cur.cpu) {
			pc = prev.cpu
		}
		base := cpuTicks{}
		if pc != nil {
			base = pc[0]
		}
		st.CPU.Usage, st.CPU.User, st.CPU.System, st.CPU.IOWait, st.CPU.Steal = cpuUsage(base, cur.cpu[0])
		if len(cur.cpu) > 1 {
			st.CPU.PerCore = make([]float64, 0, len(cur.cpu)-1)
			for i := 1; i < len(cur.cpu); i++ {
				b := cpuTicks{}
				if pc != nil {
					b = pc[i]
				}
				u, _, _, _, _ := cpuUsage(b, cur.cpu[i])
				st.CPU.PerCore = append(st.CPU.PerCore, u)
			}
		}
	}
	if st.CPU.PerCore == nil {
		st.CPU.PerCore = []float64{}
	}
	st.CPU.Cores = cur.cores
	if st.CPU.Cores == 0 {
		st.CPU.Cores = len(st.CPU.PerCore)
	}
	if st.CPU.Cores == 0 && host != nil {
		st.CPU.Cores = host.Cores
	}

	// Network
	prevNet := map[string]netCounter{}
	if prev != nil {
		for _, n := range prev.net {
			prevNet[n.iface] = n
		}
	}
	var phys, all NetRate
	havePhys := false
	for _, n := range cur.net {
		ns := NetStats{Iface: n.iface, RxTotal: int64(n.rx), TxTotal: int64(n.tx), Virtual: n.virtual}
		if p, ok := prevNet[n.iface]; ok && dt > 0 {
			ns.RxBps = rate(p.rx, n.rx, dt)
			ns.TxBps = rate(p.tx, n.tx, dt)
		}
		all.RxBps += ns.RxBps
		all.TxBps += ns.TxBps
		if !n.virtual {
			havePhys = true
			phys.RxBps += ns.RxBps
			phys.TxBps += ns.TxBps
		}
		st.Net = append(st.Net, ns)
	}
	if havePhys {
		st.NetTotal = NetRate{RxBps: round1(phys.RxBps), TxBps: round1(phys.TxBps)}
	} else {
		st.NetTotal = NetRate{RxBps: round1(all.RxBps), TxBps: round1(all.TxBps)}
	}

	// Disk I/O
	if cur.diskIO != nil && prev != nil && prev.diskIO != nil && dt > 0 {
		st.DiskIO = &IORate{ReadBps: round1(rate(prev.diskIO.read, cur.diskIO.read, dt)), WriteBps: round1(rate(prev.diskIO.write, cur.diskIO.write, dt))}
	} else if cur.diskIO != nil {
		st.DiskIO = &IORate{}
	}
	return st
}

// cpuUsage returns usage, user, system, iowait and steal percentages between two tick snapshots (b may be zero: usage
// since boot).
func cpuUsage(b, c cpuTicks) (usage, user, system, iowait, steal float64) {
	total := float64(sub(b.total(), c.total()))
	if total <= 0 {
		return 0, 0, 0, 0, 0
	}
	pct := func(x, y uint64) float64 { return round1(clampPct(float64(sub(x, y)) / total * 100)) }
	usage = pct(b.busy(), c.busy())
	user = pct(b.user+b.nice, c.user+c.nice)
	system = pct(b.system+b.irq+b.softirq, c.system+c.irq+c.softirq)
	iowait = pct(b.iowait, c.iowait)
	steal = pct(b.steal, c.steal)
	return
}

// sub returns y−x, or 0 when the counter went backwards (reset / wrap).
func sub(x, y uint64) uint64 {
	if y < x {
		return 0
	}
	return y - x
}

func rate(prev, cur uint64, dt float64) float64 {
	if dt <= 0 || cur < prev {
		return 0
	}
	return round1(float64(cur-prev) / dt)
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---- shared parsing helpers -----------------------------------------------------------------------------------------

// section is one "@@NAME [inline]" block of a script's output.
type section struct {
	inline string
	lines  []string
}

// sections splits script output into @@-marked sections. Lines before the first marker are dropped.
func sections(lines []string) map[string]*section {
	out := map[string]*section{}
	var cur *section
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "@@") {
			name, inline, _ := strings.Cut(l[2:], " ")
			cur = &section{inline: strings.TrimSpace(inline)}
			out[name] = cur
			continue
		}
		if cur != nil {
			cur.lines = append(cur.lines, l)
		}
	}
	return out
}

func (s *section) text() string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(strings.Join(s.lines, "\n"))
}

func (s *section) first() string {
	if s == nil {
		return ""
	}
	if s.inline != "" {
		return s.inline
	}
	for _, l := range s.lines {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

func (s *section) int() int {
	n, _ := strconv.Atoi(strings.TrimSpace(s.first()))
	return n
}

func splitLines(b []byte) []string {
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func atou64(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ",")), 64)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// parseDF parses `df -kP` output (1 KiB blocks). types maps mount points to filesystem types. Pseudo filesystems and
// container / snap internals are dropped; bind mounts of the same filesystem (identical numbers) are merged, keeping
// the shortest mount path.
func parseDF(lines []string, types map[string]string, platform string) []DiskStats {
	var out []DiskStats
	seen := map[[3]int64]int{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 6 {
			continue
		}
		// Locate the capacity column ("NN%") so device names or mount points with spaces still parse.
		ci := -1
		for i := 4; i < len(f); i++ {
			if strings.HasSuffix(f[i], "%") && isNum(f[i-1]) && isNum(f[i-2]) && isNum(f[i-3]) {
				ci = i
				break
			}
		}
		if ci < 0 || ci+1 >= len(f) {
			continue
		}
		dev := strings.Join(f[:ci-3], " ")
		total, used, avail := atoi64(f[ci-3])*1024, atoi64(f[ci-2])*1024, atoi64(f[ci-1])*1024
		mount := strings.Join(f[ci+1:], " ")
		fs := types[mount]
		if total <= 0 || skipMount(mount, fs, dev, platform) {
			continue
		}
		d := DiskStats{Mount: mount, FS: fs, Total: total, Used: used, Avail: avail, Device: dev}
		key := [3]int64{total, used, avail}
		if i, ok := seen[key]; ok {
			if len(mount) < len(out[i].Mount) {
				out[i] = d
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Mount == "/") != (out[j].Mount == "/") {
			return out[i].Mount == "/"
		}
		return out[i].Mount < out[j].Mount
	})
	return out
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "cgroup": true, "cgroup2": true,
	"pstore": true, "bpf": true, "tracefs": true, "debugfs": true, "securityfs": true, "configfs": true, "fusectl": true,
	"mqueue": true, "hugetlbfs": true, "autofs": true, "binfmt_misc": true, "rpc_pipefs": true, "nsfs": true,
	"efivarfs": true, "ramfs": true, "squashfs": true, "selinuxfs": true, "devfs": true, "fdescfs": true,
	"procfs": true, "linprocfs": true, "linsysfs": true, "kernfs": true, "ptyfs": true, "mfs": true, "map": true,
	"nullfs": true,
}

var containerPaths = []string{"/proc", "/sys", "/dev", "/run", "/snap", "/var/lib/docker", "/var/lib/containers",
	"/var/lib/kubelet", "/var/snap", "/var/lib/lxcfs"}

func skipMount(mount, fs, dev, platform string) bool {
	if pseudoFS[fs] || strings.HasPrefix(fs, "fuse.") {
		return true
	}
	if dev == "tmpfs" || dev == "devfs" || dev == "shm" || dev == "none" && fs == "" {
		return true
	}
	for _, p := range containerPaths {
		if mount == p || strings.HasPrefix(mount, p+"/") {
			// udisks mounts removable media under /run/media/<user>/ (Fedora, Arch…): real disks.
			return !(p == "/run" && strings.HasPrefix(mount, "/run/media/"))
		}
	}
	if platform == platDarwin {
		// APFS system volumes (VM, Preboot, Update, xarts, iSCPreboot, Hardware) are noise; Data holds user files.
		if strings.HasPrefix(mount, "/System/Volumes/") && mount != "/System/Volumes/Data" {
			return true
		}
		if strings.HasPrefix(mount, "/Library/Developer/CoreSimulator/") || strings.HasPrefix(mount, "/private/var/vm") {
			return true
		}
	}
	return false
}

// virtualIface reports whether a network interface name looks virtual (bridges, veths, tunnels, container networks).
var virtualIfaceRE = regexp.MustCompile(`^(lo\d*|veth|docker|br-|virbr|vnet|vmnet|vmenet|vboxnet|tun|tap|cni|flannel|cali|weave|kube-|vxlan|genev|wg|tailscale|zt|utun|awdl|llw|nan\d|bridge|gif|stf|anpi|ap\d|ipsec|ppp|pflog|pfsync|enc\d|lxc|lxd|podman|cilium|nodelocaldns|dummy|ifb|isatap|teredo|6to4|p2p)`)

// Windows adapter descriptions of virtual / tunnel adapters.
var virtualWinIface = []string{"vethernet", "loopback", "isatap", "teredo", "6to4", "npcap", "virtualbox", "vmware virtual",
	"wan miniport", "hyper-v virtual", "tap-windows", "wintun", "wireguard", "tailscale", "zerotier", "bluetooth",
	"kernel debug", "pseudo-interface"}

func virtualIface(name string) bool {
	n := strings.ToLower(name)
	if virtualIfaceRE.MatchString(n) {
		return true
	}
	for _, v := range virtualWinIface {
		if strings.Contains(n, v) {
			return true
		}
	}
	return false
}
