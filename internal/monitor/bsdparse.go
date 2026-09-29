package monitor

import (
	"strconv"
	"strings"
	"time"
)

// parseBSDSample parses one @@S…@@E block of the BSD sampler (FreeBSD, OpenBSD, NetBSD, DragonFly).
func parseBSDSample(lines []string, at time.Time) *rawSample {
	sec := sections(lines)
	s := &rawSample{at: at}
	sc := parseSysctl(sec["SYSCTL"])
	now := atoi64(sec["NOW"].first())
	if boot := parseBSDBoottime(sc["kern.boottime"]); boot > 0 && now > boot {
		s.uptime = float64(now - boot)
	}
	s.clock = float64(now)
	s.hostname = sc["kern.hostname"]
	s.load = parseLoadavg(sc["vm.loadavg"])
	s.cores, _ = strconv.Atoi(sc["hw.ncpu"])

	if t, ok := bsdTicks(sc["kern.cp_time"]); ok {
		s.cpu = []cpuTicks{t}
		// FreeBSD kern.cp_times: 5 counters per CPU.
		if f := numFields(sc["kern.cp_times"]); len(f) >= 10 && len(f)%5 == 0 {
			for i := 0; i+5 <= len(f); i += 5 {
				s.cpu = append(s.cpu, cpuTicks{user: f[i], nice: f[i+1], system: f[i+2], irq: f[i+3], idle: f[i+4]})
			}
			if s.cores == 0 {
				s.cores = len(s.cpu) - 1
			}
		}
	}

	total := atoi64(sc["hw.physmem"])
	page := atoi64(sc["hw.pagesize"])
	if page <= 0 {
		page = 4096
	}
	if pc := atoi64(sc["vm.stats.vm.v_page_count"]); pc > 0 { // FreeBSD / DragonFly
		free := atoi64(sc["vm.stats.vm.v_free_count"])
		inactive := atoi64(sc["vm.stats.vm.v_inactive_count"])
		cache := atoi64(sc["vm.stats.vm.v_cache_count"])
		if total <= 0 {
			total = pc * page
		}
		avail := min((free+inactive+cache)*page, total)
		s.mem = MemStats{Total: total, Available: avail, Used: total - avail, Cached: (inactive + cache) * page}
	} else if vs := parseVmstatS(sec["VMSTAT"]); len(vs) > 0 { // OpenBSD / NetBSD
		if bpp := vs["bytes per page"]; bpp > 0 {
			page = bpp
		}
		avail := min((vs["pages free"]+vs["pages inactive"])*page, total)
		if total > 0 {
			s.mem = MemStats{Total: total, Available: avail, Used: total - avail, Cached: vs["pages inactive"] * page}
		}
	} else if total > 0 {
		s.mem = MemStats{Total: total, Available: total}
	}
	s.mem.SwapTotal, s.mem.SwapUsed = parseSwapList(sec["SWAP"])

	open := atoi64(sc["kern.openfiles"])
	if open == 0 {
		open = atoi64(sc["kern.nfiles"])
	}
	if open > 0 {
		s.fds = &FDStats{Used: open, Max: atoi64(sc["kern.maxfiles"])}
	}

	s.net = parseNetstatIbn(sec["NET"])
	types := parseMountTypes(sec["MOUNT"])
	if d := sec["DF"]; d != nil {
		s.disks = parseDF(d.lines, types, platFreeBSD)
	}
	s.users = sec["USERS"].int()
	s.procs = sec["PROCS"].int()
	return s
}

// bsdTicks parses kern.cp_time: "user nice sys intr idle" (FreeBSD, NetBSD) or "user,nice,sys,spin,intr,idle"
// (OpenBSD).
func bsdTicks(v string) (cpuTicks, bool) {
	f := numFields(v)
	switch len(f) {
	case 5:
		return cpuTicks{user: f[0], nice: f[1], system: f[2], irq: f[3], idle: f[4]}, true
	case 6:
		return cpuTicks{user: f[0], nice: f[1], system: f[2] + f[3], irq: f[4], idle: f[5]}, true
	}
	return cpuTicks{}, false
}

func numFields(v string) []uint64 {
	var out []uint64
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// parseBSDBoottime handles "{ sec = N, … }", a plain epoch and OpenBSD's formatted date.
func parseBSDBoottime(v string) int64 {
	if n := parseBoottime(v); n > 1e8 {
		return n
	}
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon Jan 2 15:04:05 2006"} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(v), time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// parseVmstatS parses `vmstat -s` lines ("   12345 pages free") into {"pages free": 12345}.
func parseVmstatS(sec *section) map[string]int64 {
	out := map[string]int64{}
	if sec == nil {
		return out
	}
	for _, l := range sec.lines {
		f := strings.Fields(l)
		if len(f) < 2 || !isNum(f[0]) {
			continue
		}
		out[strings.Join(f[1:], " ")] = atoi64(f[0])
	}
	return out
}

// parseSwapList parses `swapinfo -k` / `swapctl -lk` (1K blocks): device lines are summed, the "Total" line skipped.
func parseSwapList(sec *section) (total, used int64) {
	if sec == nil {
		return 0, 0
	}
	for _, l := range sec.lines {
		f := strings.Fields(l)
		if len(f) < 3 || f[0] == "Device" || f[0] == "Total" || !isNum(f[1]) || !isNum(f[2]) {
			continue
		}
		total += atoi64(f[1]) * 1024
		used += atoi64(f[2]) * 1024
	}
	return total, used
}
