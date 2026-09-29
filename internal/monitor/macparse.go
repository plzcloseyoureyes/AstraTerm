package monitor

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseSysctl parses `sysctl name…` output: "name: value" (macOS, FreeBSD) or "name=value" (OpenBSD, NetBSD).
func parseSysctl(sec *section) map[string]string {
	out := map[string]string{}
	if sec == nil {
		return out
	}
	for _, l := range sec.lines {
		i := strings.IndexAny(l, ":=")
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(l[:i])
		if strings.ContainsAny(k, " \t") {
			continue
		}
		out[k] = strings.TrimSpace(l[i+1:])
	}
	return out
}

var (
	bootSecRE  = regexp.MustCompile(`sec\s*=\s*(\d+)`)
	swapUsage  = regexp.MustCompile(`(total|used|free)\s*=\s*([\d.]+)([KMGT]?)`)
	vmStatPage = regexp.MustCompile(`page size of (\d+) bytes`)
)

// parseBoottime extracts the boot time from "{ sec = 1790369566, usec = 632415 } …" (macOS / FreeBSD) or a plain
// epoch (OpenBSD).
func parseBoottime(v string) int64 {
	if m := bootSecRE.FindStringSubmatch(v); m != nil {
		return atoi64(m[1])
	}
	if f := strings.Fields(v); len(f) > 0 {
		return atoi64(f[0])
	}
	return 0
}

// parseLoadavg parses "{ 1.23 1.45 1.67 }" or "1.23 1.45 1.67".
func parseLoadavg(v string) [3]float64 {
	f := strings.Fields(strings.NewReplacer("{", " ", "}", " ", ",", " ").Replace(v))
	var l [3]float64
	for i := 0; i < 3 && i < len(f); i++ {
		l[i] = atof(f[i])
	}
	return l
}

func unitBytes(num, unit string) int64 {
	v := atof(num)
	switch unit {
	case "K":
		v *= 1 << 10
	case "M":
		v *= 1 << 20
	case "G":
		v *= 1 << 30
	case "T":
		v *= 1 << 40
	}
	return int64(v)
}

// parseDarwinSample parses one @@S…@@E block of the macOS sampler.
func parseDarwinSample(lines []string, at time.Time) *rawSample {
	sec := sections(lines)
	s := &rawSample{at: at}
	sc := parseSysctl(sec["SYSCTL"])
	now := atoi64(sec["NOW"].first())
	if boot := parseBoottime(sc["kern.boottime"]); boot > 0 && now > boot {
		s.uptime = float64(now - boot)
	}
	s.clock = float64(now)
	s.hostname = sc["kern.hostname"]
	s.load = parseLoadavg(sc["vm.loadavg"])
	s.cores, _ = strconv.Atoi(sc["hw.ncpu"])
	if n, m := atoi64(sc["kern.num_files"]), atoi64(sc["kern.maxfiles"]); n > 0 {
		s.fds = &FDStats{Used: n, Max: m}
	}

	// CPU: last line of `iostat -n0 -c2 -w1` = "us sy id 1m 5m 15m" over the last second.
	if f := strings.Fields(sec["IOSTAT"].first()); len(f) >= 3 && isNum(f[0]) && isNum(f[1]) && isNum(f[2]) {
		s.cpuPct = &cpuPercent{user: atof(f[0]), system: atof(f[1]), idle: atof(f[2])}
	}

	total := atoi64(sc["hw.memsize"])
	var vmLines []string
	if v := sec["VM"]; v != nil {
		vmLines = v.lines
	}
	if m, ok := darwinMemory(total, atoi64(sc["hw.pagesize"]), vmLines); ok {
		s.mem = m
	} else if total > 0 {
		s.mem = MemStats{Total: total, Available: total}
	}
	for _, m := range swapUsage.FindAllStringSubmatch(sc["vm.swapusage"], -1) {
		switch m[1] {
		case "total":
			s.mem.SwapTotal = unitBytes(m[2], m[3])
		case "used":
			s.mem.SwapUsed = unitBytes(m[2], m[3])
		}
	}

	s.net = parseNetstatIbn(sec["NET"])
	types := parseMountTypes(sec["MOUNT"])
	if d := sec["DF"]; d != nil {
		s.disks = parseDF(d.lines, types, platDarwin)
		markPrimary(s.disks, "/System/Volumes/Data")
	}
	s.users = sec["USERS"].int()
	s.procs = sec["PROCS"].int()
	return s
}

// darwinMemory computes macOS memory from `vm_stat` output the way Activity Monitor does: used = app memory
// (anonymous − purgeable pages) + wired + compressed (pages occupied by the compressor); cached = file-backed +
// purgeable pages. It serves the remote macOS sampler and the local sampler of a macOS Termstead host, so both report
// the same numbers for the same Mac. page is the fallback page size (vm_stat prints its own).
func darwinMemory(total, page int64, lines []string) (MemStats, bool) {
	vm := map[string]int64{}
	for _, l := range lines {
		if m := vmStatPage.FindStringSubmatch(l); m != nil {
			page = atoi64(m[1])
			continue
		}
		k, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		vm[strings.Trim(strings.TrimSpace(k), `"`)] = atoi64(strings.TrimSuffix(strings.TrimSpace(val), "."))
	}
	if total <= 0 || len(vm) == 0 {
		return MemStats{}, false
	}
	if page <= 0 {
		page = 4096
	}
	app := max(vm["Anonymous pages"]-vm["Pages purgeable"], 0)
	if _, ok := vm["Anonymous pages"]; !ok { // older macOS: active + inactive approximates app + cache
		app = vm["Pages active"] + vm["Pages speculative"]
	}
	used := min((app+vm["Pages wired down"]+vm["Pages occupied by compressor"])*page, total)
	return MemStats{Total: total, Used: used, Available: total - used,
		Cached: (vm["File-backed pages"] + vm["Pages purgeable"]) * page}, true
}

// parseNetstatIbn parses `netstat -ibn` (macOS / BSD): the <Link#N> row of each interface carries its byte counters.
// Columns are located by header name (FreeBSD adds Idrop, OpenBSD -b prints only byte columns).
func parseNetstatIbn(sec *section) []netCounter {
	if sec == nil || len(sec.lines) == 0 {
		return nil
	}
	var hdr []string
	for _, l := range sec.lines {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "Name" {
			hdr = f
			break
		}
	}
	col := func(name string) int {
		for i, h := range hdr {
			if h == name {
				return i
			}
		}
		return -1
	}
	ib, ob, netCol := col("Ibytes"), col("Obytes"), col("Network")
	if ib < 0 || ob < 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []netCounter
	for _, l := range sec.lines {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] == "Name" {
			continue
		}
		name := strings.TrimSuffix(f[0], "*")
		if seen[name] || strings.HasPrefix(name, "lo") && (len(name) == 2 || isNum(name[2:])) {
			continue
		}
		// Link rows have an empty Address for some interfaces (tunnels): the row is then one field shorter.
		link := netCol >= 0 && netCol < len(f) && strings.HasPrefix(f[netCol], "<Link")
		if !link {
			continue
		}
		shift := 0
		if len(f) == len(hdr)-1 {
			shift = -1 // no Address column in this row
		}
		i, o := ib+shift, ob+shift
		if i < 0 || o < 0 || o >= len(f) || !isNum(f[i]) || !isNum(f[o]) {
			continue
		}
		seen[name] = true
		out = append(out, netCounter{iface: name, rx: atou64(f[i]), tx: atou64(f[o]), virtual: virtualIface(name)})
	}
	return out
}

var (
	mountParenRE = regexp.MustCompile(`^(.+?) on (.+) \(([^,)]+)`) // macOS / FreeBSD: "dev on /mnt (type, …)"
	mountTypeRE  = regexp.MustCompile(`^(.+?) on (.+) type (\S+)`) // OpenBSD / NetBSD / Linux: "dev on /mnt type t (…)"
)

// parseMountTypes maps mount points to filesystem types from `mount` output.
func parseMountTypes(sec *section) map[string]string {
	out := map[string]string{}
	if sec == nil {
		return out
	}
	for _, l := range sec.lines {
		if m := mountTypeRE.FindStringSubmatch(l); m != nil {
			out[m[2]] = m[3]
			continue
		}
		if m := mountParenRE.FindStringSubmatch(l); m != nil {
			out[m[2]] = strings.TrimSpace(m[3])
		}
	}
	return out
}

// markPrimary moves the preferred "main" volume (e.g. the macOS Data volume) to the front of disks.
func markPrimary(disks []DiskStats, mount string) {
	for i, d := range disks {
		if d.Mount == mount && i > 0 {
			copy(disks[1:i+1], disks[:i])
			disks[0] = d
			return
		}
	}
}
