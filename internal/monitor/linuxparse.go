package monitor

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseLinuxSample parses one @@S…@@E block of the Linux sampler.
func parseLinuxSample(lines []string, at time.Time) *rawSample {
	sec := sections(lines)
	s := &rawSample{at: at}

	if f := strings.Fields(sec["UPTIME"].first()); len(f) > 0 {
		s.uptime = atof(f[0])
		s.clock = s.uptime
	}
	if f := strings.Fields(sec["LOAD"].first()); len(f) >= 4 {
		s.load = [3]float64{atof(f[0]), atof(f[1]), atof(f[2])}
		if _, total, ok := strings.Cut(f[3], "/"); ok {
			s.threads, _ = strconv.Atoi(total)
		}
	}
	if c := sec["CPU"]; c != nil {
		for _, l := range c.lines {
			f := strings.Fields(l)
			if len(f) < 5 || !strings.HasPrefix(f[0], "cpu") {
				continue
			}
			t := cpuTicks{}
			v := func(i int) uint64 {
				if i < len(f) {
					return atou64(f[i])
				}
				return 0
			}
			// user nice system idle iowait irq softirq steal (guest time is already included in user)
			t.user, t.nice, t.system, t.idle, t.iowait, t.irq, t.softirq, t.steal = v(1), v(2), v(3), v(4), v(5), v(6), v(7), v(8)
			if f[0] == "cpu" {
				s.cpu = append([]cpuTicks{t}, s.cpu...)
			} else {
				s.cpu = append(s.cpu, t)
			}
		}
		if len(s.cpu) > 0 && s.cpu[0] == (cpuTicks{}) {
			s.cpu = nil // aggregate line missing: ignore the sample's CPU part
		}
		if len(s.cpu) > 1 {
			s.cores = len(s.cpu) - 1
		}
	}
	s.mem = parseMeminfo(sec["MEM"])

	virtual := map[string]bool{}
	if v := sec["VNET"]; v != nil {
		for _, l := range v.lines {
			if n := strings.TrimSpace(l); n != "" {
				virtual[n] = true
			}
		}
	}
	s.net = parseProcNetDev(sec["NET"], virtual)
	s.diskIO = parseDiskstats(sec["DISKIO"])

	if f := strings.Fields(sec["FD"].first()); len(f) >= 3 {
		alloc, free, maxfd := atoi64(f[0]), atoi64(f[1]), atoi64(f[2])
		if maxfd <= 0 || maxfd >= 1<<62 {
			maxfd = 0 // unlimited
		}
		s.fds = &FDStats{Used: max(alloc-free, 0), Max: maxfd}
	}
	s.hostname = sec["HOST"].first()

	types := map[string]string{}
	if m := sec["MOUNTS"]; m != nil {
		for _, l := range m.lines {
			if t, mount, ok := strings.Cut(l, "\t"); ok {
				types[mount] = t
			}
		}
	}
	if d := sec["DF"]; d != nil {
		s.disks = parseDF(d.lines, types, platLinux)
	}
	s.users = sec["USERS"].int()
	s.procs = sec["PROCS"].int()
	return s
}

// parseMeminfo turns /proc/meminfo lines (kB) into MemStats. Used = MemTotal − MemAvailable; kernels older than 3.14
// have no MemAvailable, it is then estimated as free + buffers + page cache + reclaimable slab − shmem.
func parseMeminfo(sec *section) MemStats {
	if sec == nil {
		return MemStats{}
	}
	kv := map[string]int64{}
	for _, l := range sec.lines {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		kv[strings.TrimSpace(k)] = atoi64(f[0]) * 1024
	}
	m := MemStats{Total: kv["MemTotal"], SwapTotal: kv["SwapTotal"]}
	avail, ok := kv["MemAvailable"]
	if !ok {
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"] + kv["SReclaimable"] - kv["Shmem"]
	}
	m.Available = min(max(avail, 0), m.Total)
	m.Used = m.Total - m.Available
	m.SwapUsed = max(kv["SwapTotal"]-kv["SwapFree"], 0)
	m.Cached = max(kv["Buffers"]+kv["Cached"]+kv["SReclaimable"]-kv["Shmem"], 0)
	return m
}

// parseProcNetDev parses /proc/net/dev (loopback excluded). virtual lists /sys/devices/virtual/net entries.
func parseProcNetDev(sec *section, virtual map[string]bool) []netCounter {
	if sec == nil {
		return nil
	}
	var out []netCounter
	for _, l := range sec.lines {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		out = append(out, netCounter{iface: name, rx: atou64(f[0]), tx: atou64(f[8]),
			virtual: virtual[name] || (len(virtual) == 0 && virtualIface(name))})
	}
	return out
}

// wholeDiskRE matches physical whole-disk devices of /proc/diskstats (partitions, device-mapper and md devices would
// count the same I/O twice).
var wholeDiskRE = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

// parseDiskstats sums the sectors read / written by physical disks (sectors are always 512 bytes in diskstats).
func parseDiskstats(sec *section) *ioCounter {
	if sec == nil {
		return nil
	}
	var io ioCounter
	found := false
	for _, l := range sec.lines {
		f := strings.Fields(l)
		if len(f) < 10 || !wholeDiskRE.MatchString(f[2]) {
			continue
		}
		found = true
		io.read += atou64(f[5]) * 512
		io.write += atou64(f[9]) * 512
	}
	if !found {
		return nil
	}
	return &io
}
