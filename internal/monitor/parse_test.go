package monitor

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// samplesOf runs the sampler output parser over a fixture.
func samplesOf(t *testing.T, platform string, out []byte) []*rawSample {
	t.Helper()
	var got []*rawSample
	consumeSamples(strings.NewReader(string(out)), &hostInfo{Platform: platform}, func(s *rawSample) { got = append(got, s) })
	return got
}

func TestLinuxSampleParsing(t *testing.T) {
	raw := samplesOf(t, platLinux, fixture(t, "linux_loop.txt"))
	if len(raw) != 2 {
		t.Fatalf("samples: %d", len(raw))
	}
	host := &hostInfo{Platform: platLinux, OS: "Alpine Linux v3.24", Kernel: "6.8.0", Hostname: "probe-name", Arch: "aarch64"}
	first := computeStats(nil, raw[0], host)
	if !first.Warmup || first.NetTotal.RxBps != 0 || first.DiskIO == nil || first.DiskIO.ReadBps != 0 {
		t.Fatalf("first sample: %+v", first)
	}
	st := computeStats(raw[0], raw[1], host)
	if st.Warmup || st.Hostname != "ssh1" || st.OS != host.OS || st.Platform != platLinux || st.UptimeSec != 76014 {
		t.Fatalf("identity: %+v", st)
	}
	// Δbusy = (93987+4+89581+5397) − (93985+4+89579+5397) = 4, Δtotal = 4 + (14947995−14947795) = 204 → 2.0 %.
	if st.CPU.Usage != 2 || st.CPU.Cores != 2 || len(st.CPU.PerCore) != 2 {
		t.Fatalf("cpu: %+v", st.CPU)
	}
	if st.Load != [3]float64{0.27, 0.45, 0.35} || st.Threads != 747 || st.Processes != 20 || st.Users != 0 {
		t.Fatalf("load/threads: %+v %d %d", st.Load, st.Threads, st.Processes)
	}
	if st.Mem.Total != 2006672*1024 || st.Mem.Available != 1184476*1024 || st.Mem.Used != (2006672-1184476)*1024 || st.Mem.SwapTotal != 0 {
		t.Fatalf("mem: %+v", st.Mem)
	}
	// "/" (overlay) and the /config, /etc/hosts… bind mounts of the same filesystem collapse into "/".
	if len(st.Disks) != 1 || st.Disks[0].Mount != "/" || st.Disks[0].FS != "overlay" || st.Disks[0].Total != 102624184*1024 ||
		st.Disks[0].Avail != 90772208*1024 {
		t.Fatalf("disks: %+v", st.Disks)
	}
	if len(st.Net) != 1 || st.Net[0].Iface != "eth0" || !st.Net[0].Virtual || st.Net[0].RxTotal != 338049394 {
		t.Fatalf("net: %+v", st.Net)
	}
	// Container: every interface is virtual, so the total falls back to all of them. The interval comes from the
	// host's uptime clock (1.01 s), not from when the samples arrived.
	if want := round1(float64(338049394-338048668) / 1.01); st.NetTotal.RxBps != want || st.Net[0].RxBps != want || st.IntervalSec != 1.01 {
		t.Fatalf("rates: %+v want %v (interval %v)", st.NetTotal, want, st.IntervalSec)
	}
	if st.FDs == nil || st.FDs.Max != 0 || st.FDs.Used <= 0 {
		t.Fatalf("fds: %+v", st.FDs)
	}
	if st.DiskIO == nil {
		t.Fatal("disk io missing")
	}
	// JSON stays a superset of the SPEC contract.
	b, _ := json.Marshal(st)
	var m model.MonitorStats
	if err := json.Unmarshal(b, &m); err != nil || m.Hostname != "ssh1" || m.CPU.Usage != st.CPU.Usage || len(m.Disks) != 1 || m.Mem.Used != st.Mem.Used {
		t.Fatalf("contract: %v %+v", err, m)
	}
}

func TestMeminfoWithoutMemAvailable(t *testing.T) {
	m := parseMeminfo(&section{lines: []string{"MemTotal: 1000 kB", "MemFree: 100 kB", "Buffers: 50 kB", "Cached: 250 kB",
		"SReclaimable: 20 kB", "Shmem: 20 kB", "SwapTotal: 500 kB", "SwapFree: 200 kB"}})
	if m.Available != 400*1024 || m.Used != 600*1024 || m.SwapUsed != 300*1024 || m.Cached != 300*1024 {
		t.Fatalf("%+v", m)
	}
}

const darwinSample = `@@S
@@SYSCTL
hw.ncpu: 8
hw.memsize: 17179869184
hw.pagesize: 16384
vm.loadavg: { 2.10 1.90 1.80 }
kern.boottime: { sec = 1790000000, usec = 632415 } Fri Sep 25 23:52:46 2026
vm.swapusage: total = 2048.00M  used = 512.50M  free = 1535.50M  (encrypted)
kern.hostname: mac.example
kern.num_files: 9161
kern.maxfiles: 122880
@@NOW 1790086400
@@IOSTAT
 12  8 80  2.10 1.90 1.80
@@VM
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     6474.
Pages active:                                 177038.
Pages inactive:                               174852.
Pages speculative:                               729.
Pages wired down:                             100000.
Pages purgeable:                                1000.
File-backed pages:                            118804.
Anonymous pages:                              301000.
Pages occupied by compressor:                  50000.
@@NET
Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                       1618661     0 1012235415  1618661     0 1012235415     0
lo0        16384 127           127.0.0.1        1618661     - 1012235415  1618661     - 1012235415     -
gif0*      1280  <Link#2>                             0     0          0        0     0          0     0
utun0      1500  <Link#18>                            0     0          0        1     0         80     0
en0        1500  <Link#14>   02:00:00:00:00:01 39087293     0 48657563363 12241213     0 3889404978     0
en0        1500  192.168.1/24 192.168.1.10      39087293     - 48657563363 12241213     - 3889404978     -
bridge100  1500  <Link#24>   02:00:00:00:00:02    57045     0    7198028   336974     0  380575774     0
@@MOUNT
/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk3s6 on /System/Volumes/VM (apfs, local, noexec, journaled, noatime, nobrowse)
/dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled, nobrowse, protect, root data)
/dev/disk4s2 on /Volumes/Backup Disk (hfs, local, nodev, nosuid, journaled)
@@DF
Filesystem     1024-blocks      Used Available Capacity  Mounted on
/dev/disk3s1s1   482797652  18548392  74595452    20%    /
/dev/disk3s6     482797652  14681040  74595452    17%    /System/Volumes/VM
/dev/disk3s5     482797652 349251880  74595452    83%    /System/Volumes/Data
/dev/disk4s2      19247636  18049144   1198492    94%    /Volumes/Backup Disk
@@USERS
       3
@@PROCS
     714
@@E
`

func TestDarwinSampleParsing(t *testing.T) {
	raw := samplesOf(t, platDarwin, []byte(darwinSample))
	if len(raw) != 1 {
		t.Fatalf("samples: %d", len(raw))
	}
	st := computeStats(nil, raw[0], &hostInfo{Platform: platDarwin})
	if st.CPU.Usage != 20 || st.CPU.User != 12 || st.CPU.System != 8 || st.CPU.Cores != 8 {
		t.Fatalf("cpu: %+v", st.CPU)
	}
	if st.UptimeSec != 86400 || st.Hostname != "mac.example" || st.Load[0] != 2.1 || st.Users != 3 || st.Processes != 714 {
		t.Fatalf("host: %+v", st)
	}
	used := int64(301000-1000+100000+50000) * 16384
	if st.Mem.Total != 17179869184 || st.Mem.Used != used || st.Mem.Available != 17179869184-used {
		t.Fatalf("mem: %+v want used %d", st.Mem, used)
	}
	if st.Mem.SwapTotal != 2048<<20 || st.Mem.SwapUsed != int64(512.5*(1<<20)) {
		t.Fatalf("swap: %+v", st.Mem)
	}
	if st.FDs == nil || st.FDs.Used != 9161 || st.FDs.Max != 122880 {
		t.Fatalf("fds: %+v", st.FDs)
	}
	// The Data volume comes first; APFS system volumes and devfs are hidden; spaces in mount points survive.
	var mounts []string
	for _, d := range st.Disks {
		mounts = append(mounts, d.Mount+"|"+d.FS)
	}
	if strings.Join(mounts, ",") != "/System/Volumes/Data|apfs,/|apfs,/Volumes/Backup Disk|hfs" {
		t.Fatalf("disks: %v", mounts)
	}
	names := map[string]bool{}
	for _, n := range st.Net {
		names[n.Iface] = n.Virtual
	}
	if _, ok := names["lo0"]; ok || names["en0"] || !names["utun0"] || !names["bridge100"] || !names["gif0"] {
		t.Fatalf("net: %+v", st.Net)
	}
	if raw[0].net[1].iface != "utun0" || raw[0].net[1].tx != 80 {
		t.Fatalf("tunnel without address: %+v", raw[0].net)
	}
}

const bsdSample = `@@S
@@SYSCTL
kern.cp_time: 1000 10 500 90 8400
kern.cp_times: 500 5 250 45 4200 500 5 250 45 4200
hw.ncpu: 2
hw.physmem: 4294967296
hw.pagesize: 4096
vm.loadavg: { 0.50 0.40 0.30 }
kern.boottime: { sec = 1790000000, usec = 1 } Wed Sep 25 10:00:00 2026
kern.hostname: bsd.example
vm.stats.vm.v_page_count: 1000000
vm.stats.vm.v_free_count: 200000
vm.stats.vm.v_inactive_count: 100000
vm.stats.vm.v_cache_count: 0
kern.openfiles: 321
kern.maxfiles: 65536
@@NOW 1790003600
@@VMSTAT
@@SWAP
Device          1K-blocks     Used    Avail Capacity
/dev/ada0p3       2097152    10240  2086912     0%
@@NET
Name    Mtu Network       Address              Ipkts Ierrs Idrop     Ibytes    Opkts Oerrs     Obytes  Coll
vtnet0 1500 <Link#1>      58:9c:fc:00:00:01    10000     0     0    5000000     8000     0    2000000     0
vtnet0    - 10.0.0.0/24   10.0.0.5              9000     -     -    4000000     7000     -    1500000     -
lo0   16384 <Link#2>      lo0                     10     0     0       1000       10     0       1000     0
@@MOUNT
/dev/ada0p2 on / (ufs, local, journaled soft-updates)
devfs on /dev (devfs)
@@DF
Filesystem  1024-blocks    Used    Avail Capacity  Mounted on
/dev/ada0p2    20307196 5000000 13682620    27%    /
devfs                 1       1        0   100%    /dev
@@USERS
1
@@PROCS
42
@@E
`

func TestBSDSampleParsing(t *testing.T) {
	raw := samplesOf(t, platFreeBSD, []byte(bsdSample))
	if len(raw) != 1 {
		t.Fatalf("samples: %d", len(raw))
	}
	r := raw[0]
	if len(r.cpu) != 3 || r.cores != 2 || r.uptime != 3600 {
		t.Fatalf("cpu/uptime: %+v", r)
	}
	st := computeStats(nil, r, &hostInfo{Platform: platFreeBSD})
	if st.CPU.Usage != 16 || len(st.CPU.PerCore) != 2 {
		t.Fatalf("cpu: %+v", st.CPU)
	}
	if st.Mem.Total != 4294967296 || st.Mem.Available != 300000*4096 || st.Mem.SwapTotal != 2097152*1024 || st.Mem.SwapUsed != 10240*1024 {
		t.Fatalf("mem: %+v", st.Mem)
	}
	if len(st.Disks) != 1 || st.Disks[0].FS != "ufs" || len(st.Net) != 1 || st.Net[0].RxTotal != 5000000 || st.FDs.Used != 321 {
		t.Fatalf("disks/net: %+v %+v %+v", st.Disks, st.Net, st.FDs)
	}
	// OpenBSD's "=" sysctl format and 6-field cp_time.
	ticks, ok := bsdTicks("1,2,3,4,5,6")
	if !ok || ticks.system != 7 || ticks.irq != 5 || ticks.idle != 6 {
		t.Fatalf("openbsd ticks: %+v", ticks)
	}
	if m := parseSysctl(&section{lines: []string{"kern.hostname=obsd", "hw.ncpu=4"}}); m["kern.hostname"] != "obsd" || m["hw.ncpu"] != "4" {
		t.Fatalf("sysctl: %v", m)
	}
}

func TestWindowsSampleParsing(t *testing.T) {
	a := `{"h":"WINSRV","up":3600,"mt":8589934592,"mf":4294967296,"pt":1073741824,"pu":268435456,"np":120,"th":1500,"ql":1,"u":2,` +
		`"cpu":[{"n":"_Total","i":8000000,"u":1000000,"k":1000000,"ts":10000000},{"n":"0","i":4000000,"u":500000,"k":500000,"ts":10000000},{"n":"1","i":4000000,"u":500000,"k":500000,"ts":10000000}],` +
		`"net":[{"n":"Intel[R] Ethernet Connection","r":1000,"t":2000},{"n":"vEthernet (WSL)","r":500,"t":500}],` +
		`"dk":[{"m":"D:","f":"NTFS","s":1000,"v":400},{"m":"C:","f":"NTFS","s":2000,"v":500}]}`
	b := strings.NewReplacer(`"i":8000000,"u":1000000,"k":1000000,"ts":10000000`, `"i":14000000,"u":4000000,"k":2000000,"ts":30000000`,
		`"r":1000,"t":2000`, `"r":21000,"t":4000`).Replace(a)
	raw := samplesOf(t, platWindows, []byte(a+"\r\n"+b+"\r\n"))
	if len(raw) != 2 {
		t.Fatalf("samples: %d", len(raw))
	}
	st := computeStats(raw[0], raw[1], &hostInfo{Platform: platWindows})
	// Δidle 6e6, Δuser 3e6, Δkernel 1e6 over Δ10e6 → 40 % busy; the timestamp clock gives a 2 s interval.
	if st.CPU.Usage != 40 || st.CPU.User != 30 || st.CPU.System != 10 || st.CPU.Cores != 2 || st.IntervalSec != 2 {
		t.Fatalf("cpu: %+v interval %v", st.CPU, st.IntervalSec)
	}
	if st.NetTotal.RxBps != 10000 || st.NetTotal.TxBps != 1000 {
		t.Fatalf("net total (physical only): %+v", st.NetTotal)
	}
	if st.Disks[0].Mount != "C:" || st.Disks[0].Used != 1500 || st.Mem.Used != 4294967296 || st.Users != 2 || st.Processes != 120 {
		t.Fatalf("disks/mem: %+v %+v", st.Disks, st.Mem)
	}
}

func TestProbeParsing(t *testing.T) {
	h := parseProbe(fixture(t, "linux_probe.txt"))
	if h.Platform != platLinux || h.OS != "Alpine Linux v3.24" || h.Hostname != "ssh1" || h.Arch != "aarch64" || h.Cores != 2 {
		t.Fatalf("linux probe: %+v", h)
	}
	if h.CPUModel != "Apple (ARM64)" || !h.has("sudo") || h.has("systemctl") {
		t.Fatalf("linux probe details: %q %v", h.CPUModel, h.Tools)
	}
	mac := parseProbe([]byte("@@OS Darwin\n@@HOSTNAME m1\n@@KERNEL 24.1.0\n@@ARCH arm64\n@@RELEASE\nProductName:\t\tmacOS\nProductVersion:\t\t15.1\nBuildVersion:\t\t24B83\n@@CPUMODEL\nApple M3 Pro\n@@NCPU\n11\n@@TOOLS sudo=/usr/bin/sudo lsof=/usr/sbin/lsof\n@@END\n"))
	if mac.Platform != platDarwin || mac.OS != "macOS 15.1" || mac.CPUModel != "Apple M3 Pro" || mac.Cores != 11 || !mac.has("lsof") {
		t.Fatalf("mac probe: %+v", mac)
	}
	bsd := parseProbe([]byte("@@OS FreeBSD\n@@HOSTNAME fb\n@@KERNEL 14.1-RELEASE\n@@ARCH amd64\n@@RELEASE\n14.1-RELEASE-p5\n@@CPUMODEL\nIntel(R) Xeon(R)\n@@NCPU\n4\n@@END\n"))
	if bsd.Platform != platFreeBSD || bsd.OS != "FreeBSD 14.1-RELEASE-p5" {
		t.Fatalf("bsd probe: %+v", bsd)
	}
	x86 := linuxCPUModel(&section{lines: []string{"model name: Intel(R)  Xeon(R) CPU   E5-2680 v4", "vendor_id: GenuineIntel"}})
	arm := linuxCPUModel(&section{lines: []string{"CPU implementer: 0x41", "CPU part: 0xd0c"}})
	if x86 != "Intel(R) Xeon(R) CPU E5-2680 v4" || arm != "ARM Neoverse-N1" {
		t.Fatalf("cpu models: %q %q", x86, arm)
	}
	w, ok := parseWindowsProbe([]byte("#< CLIXML\r\n" + `{"os":"Windows","h":"SRV1","cap":"Microsoft Windows Server 2022 Datacenter","ver":"10.0.20348","arch":"AMD64","cpu":"Intel(R) Xeon(R)  Gold","n":8,"model":"VMware7,1","maker":"VMware, Inc."}` + "\r\n"))
	if !ok || w.Platform != platWindows || w.OS != "Windows Server 2022 Datacenter" || w.Arch != "amd64" || w.Cores != 8 || w.CPUModel != "Intel(R) Xeon(R) Gold" {
		t.Fatalf("windows probe: %+v %v", w, ok)
	}
}

func TestLinuxProcsParsing(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	out := "@@SELF 99\n@@HZ 100\n@@PAGE 4096\n@@UPTIME 1000.00\n@@MEMTOTAL 1000000\n@@STAT\n" +
		"99 (sh) S 1 99 99 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 99990 0 0 1\n" +
		"100 (cat) R 99 99 99 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 99990 0 0 1\n" +
		"1 (init) S 0 1 1 0 -1 4194560 1 1 0 0 100 50 0 0 20 0 1 0 10 1000000 250 18446744073709551615\n" +
		"42 (my (odd) prog) R 1 42 42 0 -1 0 0 0 0 0 5000 5000 0 0 20 5 4 0 90000 2000000 1000 1\n" +
		"77 (kworker/0:1) I 2 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 50 0 0 1\n" +
		"garbage line\n" +
		"@@PS\n      1 root     /sbin/init splash\n     42 alice    /usr/bin/odd --flag   \"quoted arg\"\n@@END\n"
	procs, st := parseLinuxProcs([]byte(out), now)
	if len(procs) != 3 || st.clock != 1000 {
		t.Fatalf("procs: %+v", procs)
	}
	p := procs[1]
	if p.PID != 42 || p.PPID != 1 || p.Name != "my (odd) prog" || p.State != "R" || p.User != "alice" || p.Nice != 5 || p.Threads != 4 {
		t.Fatalf("odd: %+v", p)
	}
	// 100 s of CPU over 100 s of life (started at 900 s of 1000 s uptime) → 100 %.
	if p.CPU != 100 || p.CPUTime != 100 || p.RSS != 1000*4096 || p.Mem != round1(1000*4096/1024e6*100) {
		t.Fatalf("odd numbers: %+v", p)
	}
	if p.Command != `/usr/bin/odd --flag   "quoted arg"` || p.Started != "2026-09-27T11:58:20Z" {
		t.Fatalf("odd command/start: %q %q", p.Command, p.Started)
	}
	if procs[2].Command != "[kworker/0:1]" || procs[0].Command != "/sbin/init splash" {
		t.Fatalf("kernel thread / init: %+v", procs)
	}
	// BusyBox ps form (with header) and the real fixture from ssh1.
	bb, _ := parseLinuxProcs([]byte("@@HZ 100\n@@UPTIME 10\n@@STAT\n5 (sh) S 1 5 5 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 100 0 10 0\n@@PS\nPID   USER     COMMAND\n    5 bob      -sh\n"), now)
	if len(bb) != 1 || bb[0].User != "bob" || bb[0].Command != "-sh" {
		t.Fatalf("busybox: %+v", bb)
	}
	real, _ := parseLinuxProcs(fixture(t, "linux_procs.txt"), now)
	if len(real) < 5 || real[0].User != "root" || real[0].PID != 1 {
		t.Fatalf("fixture: %+v", real)
	}
}

func TestUnixAndWindowsProcsParsing(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	out := "@@SELF 700\n@@PS\n    1     0   0.0  0.1  12000  400000 Ss     0   2-03:04:05     1:23.45 root             /sbin/launchd\n" +
		"  700     1   1.0  0.0    100     200 S      0        00:01     0:00.01 alice            /bin/sh -s\n" +
		"  701   700   9.0  0.0    100     200 R      0        00:01     0:00.01 alice            ps -axww -o pid=\n" +
		"  512     1  25.5  3.2 262144 4194304 R      5        10:05     0:02.50 alice            /Applications/Some App.app/Contents/MacOS/Some App --arg\n@@END\n"
	procs, _ := parseUnixProcs([]byte(out), now)
	if len(procs) != 2 {
		t.Fatalf("procs: %+v", procs)
	}
	p := procs[1]
	if p.PID != 512 || p.CPU != 25.5 || p.Mem != 3.2 || p.RSS != 262144*1024 || p.Nice != 5 || p.User != "alice" || p.CPUTime != 2.5 {
		t.Fatalf("mac proc: %+v", p)
	}
	if p.Command != "/Applications/Some App.app/Contents/MacOS/Some App --arg" || p.Started != "2026-09-27T11:49:55Z" {
		t.Fatalf("mac command/start: %q %q", p.Command, p.Started)
	}
	if procs[0].Started != now.Add(-(2*86400+3*3600+4*60+5)*time.Second).Format(time.RFC3339) || procs[0].CPUTime != 83.45 {
		t.Fatalf("launchd: %+v", procs[0])
	}
	w := `{"mem":1000,"self":7000,"procs":[{"p":7000,"pp":1,"n":"powershell.exe","c":null,"ws":1,"vs":0,"cpu":1,"th":1,"pr":8,"s":1,"u":null},{"p":4,"pp":0,"n":"System","c":null,"ws":100,"vs":0,"cpu":20000000,"th":150,"pr":8,"s":100,"u":null},` +
		`{"p":900,"pp":4,"n":"svc.exe","c":"C:\\svc.exe -k x","ws":50,"vs":200,"cpu":10000000,"th":3,"pr":13,"s":10,"u":"NT AUTHORITY\\SYSTEM"}]}`
	wp, st, err := parseWindowsProcs([]byte("\r\n"+w+"\r\n"), now)
	if err != nil || len(wp) != 2 || st == nil {
		t.Fatalf("windows: %v %+v", err, wp)
	}
	if wp[0].Command != "System" || wp[0].CPU != 2 || wp[0].Mem != 10 || wp[1].Command != `C:\svc.exe -k x` || wp[1].User != `NT AUTHORITY\SYSTEM` || wp[1].Nice != -10 || wp[1].CPU != 10 {
		t.Fatalf("windows procs: %+v", wp)
	}
	for in, want := range map[string]float64{"05:06": 306, "1:02:03": 3723, "2-00:00:01": 172801, "0:01.50": 1.5, "x": -1} {
		if got := parseClock(in); got != want {
			t.Fatalf("parseClock(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPortParsing(t *testing.T) {
	bb := parseLinuxPorts(fixture(t, "linux_ports_busybox.txt"))
	bb = normalizePorts(bb)
	var got []string
	for _, p := range bb {
		got = append(got, p.Proto+" "+p.Address+" "+strconv.Itoa(p.Port)+" "+strconv.Itoa(p.PID)+" "+p.Process)
	}
	want := "tcp 0.0.0.0 2222 237 sshd.pam,tcp :: 2222 237 sshd.pam,tcp 127.0.0.11 46263 0 ,udp 127.0.0.11 51318 0 "
	if strings.Join(got, ",") != want {
		t.Fatalf("busybox netstat:\n%s\nwant\n%s", strings.Join(got, ","), want)
	}
	ss := parseSS([]string{
		"tcp   LISTEN 0      4096         0.0.0.0:22        0.0.0.0:*    users:((\"sshd\",pid=804,fd=3))",
		"tcp   LISTEN 0      511      127.0.0.53%lo:53      0.0.0.0:*",
		"tcp   LISTEN 0      128             [::]:80           [::]:*    users:((\"nginx\",pid=11,fd=6),(\"nginx\",pid=12,fd=6))",
		"udp   UNCONN 0      0          0.0.0.0:68        0.0.0.0:*",
		"tcp   ESTAB  0      0        10.0.0.2:22     10.0.0.1:51000",
	})
	if len(ss) != 4 || ss[0].PID != 804 || ss[0].Process != "sshd" || ss[1].Address != "127.0.0.53" || ss[2].Address != "::" || ss[2].PID != 11 || ss[3].Proto != "udp" {
		t.Fatalf("ss: %+v", ss)
	}
	proc := parseProcNet(sections(splitLines(fixture(t, "linux_ports_busybox.txt")))["PROC"])
	proc = normalizePorts(proc)
	if len(proc) != 4 || proc[0].Port != 2222 || proc[0].Address != "0.0.0.0" || proc[1].Address != "::" || proc[3].Proto != "udp" || proc[3].Port != 51318 {
		t.Fatalf("/proc/net: %+v", proc)
	}
	mac := parseUnixPorts([]byte("@@LSOFTCP\np321\ncsshd\nLroot\nf5\nPTCP\nn*:22\nf6\nPTCP\nn[::1]:631\n@@LSOFUDP\np99\ncmDNSResponder\nL_mdns\nf7\nPUDP\nn*:5353\nf8\nPUDP\nn10.0.0.2:5000->1.1.1.1:53\n" +
		"@@NETSTAT\ntcp4       0      0  *.22                   *.*                    LISTEN\ntcp6       0      0  ::1.631                *.*                    LISTEN\n" +
		"tcp4       0      0  127.0.0.1.8080         *.*                    LISTEN\nudp4       0      0  *.5353                 *.*\n@@END\n"))
	mac = normalizePorts(mac)
	var mg []string
	for _, p := range mac {
		mg = append(mg, p.Proto+" "+p.Address+" "+strconv.Itoa(p.Port)+" "+p.Process)
	}
	if strings.Join(mg, ",") != "tcp * 22 sshd,tcp ::1 631 sshd,udp * 5353 mDNSResponder,tcp 127.0.0.1 8080 " {
		t.Fatalf("mac ports: %v", mg)
	}
	sock := parseSockstat([]string{"USER     COMMAND    PID   FD PROTO  LOCAL ADDRESS         FOREIGN ADDRESS",
		"root     sshd       821   4  tcp6   *:22                  *:*", "root     syslogd    500   6  udp4   *:514                 *:*"})
	if len(sock) != 2 || sock[0].Proto != "tcp" || sock[0].PID != 821 || sock[1].Port != 514 {
		t.Fatalf("sockstat: %+v", sock)
	}
	wp, err := parseWindowsPorts([]byte(`[{"pr":"tcp","a":"0.0.0.0","p":3389,"i":1100,"c":"svchost"},{"pr":"udp","a":"::","p":123,"i":900,"c":"svchost"}]`))
	if err != nil || len(wp) != 2 || wp[0].Port != 3389 || wp[1].Address != "::" {
		t.Fatalf("windows ports: %+v %v", wp, err)
	}
	if a, p, ok := decodeProcAddr("0100007F:1F90"); !ok || a != "127.0.0.1" || p != 8080 {
		t.Fatalf("decode: %s %d", a, p)
	}
}

func TestServiceParsing(t *testing.T) {
	out := "@@UNITS\n" +
		"accounts-daemon.service loaded active running Accounts Service\n" +
		"● bad.service not-found inactive dead bad.service\n" +
		"cron.service loaded failed failed Regular background program processing daemon\n" +
		"@@FILES\n" +
		"accounts-daemon.service enabled enabled\n" +
		"cron.service enabled enabled\n" +
		"getty@.service enabled enabled\n" +
		"ssh.service disabled enabled\n" +
		"@@END\n"
	units := parseSystemdUnits([]byte(out))
	if len(units) != 4 {
		t.Fatalf("units: %+v", units)
	}
	byName := map[string]Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}
	if u := byName["accounts-daemon.service"]; u.Active != "active" || u.Sub != "running" || u.Enabled != "enabled" || u.Description != "Accounts Service" {
		t.Fatalf("accounts: %+v", u)
	}
	if u := byName["ssh.service"]; u.Load != "not-loaded" || u.Active != "inactive" || u.Enabled != "disabled" {
		t.Fatalf("ssh (never loaded): %+v", u)
	}
	if u := byName["cron.service"]; u.Active != "failed" || u.Description != "Regular background program processing daemon" {
		t.Fatalf("cron: %+v", u)
	}
	w, err := parseWindowsServices([]byte(`{"n":"Spooler","d":"Print Spooler","s":"Running","m":"Auto","i":1234,"x":null}`))
	if err != nil || len(w) != 1 || w[0].Active != "active" || w[0].Enabled != "enabled" || w[0].PID != 1234 {
		t.Fatalf("windows services: %+v %v", w, err)
	}
	for name, ok := range map[string]bool{"nginx.service": true, "getty@tty1.service": true, "sshd": true, "-rf": false,
		"a b": false, "x;rm": false, "../etc": false, "": false, "a'b": false} {
		if (validUnit(name) == nil) != ok {
			t.Fatalf("validUnit(%q) = %v", name, !ok)
		}
	}
}

func TestDiskUsageParsing(t *testing.T) {
	du := parseDU([]byte("100\t/var/log/apt\n2000\t/var/log/journal\n50\t/var/log/deep/nested\n2500\t/var/log\n"), "/var/log")
	if du.Total != 2500*1024 || du.Parent != "/var" || len(du.Entries) != 3 {
		t.Fatalf("du: %+v", du)
	}
	if du.Entries[0].Name != "journal" || du.Entries[1].Name != "(files)" || du.Entries[1].Size != 400*1024 || du.Entries[1].Dir {
		t.Fatalf("entries: %+v", du.Entries)
	}
	root := parseDU([]byte("10\t/bin\n20\t/\n"), "/")
	if root.Parent != "" || len(root.Entries) != 2 {
		t.Fatalf("root: %+v", root)
	}
	for in, ok := range map[string]bool{"/var/log": true, "/": true, "relative": false, "": false, "/a\nb": false, "/x/../y": true} {
		if _, err := validRemotePath(in); (err == nil) != ok {
			t.Fatalf("validRemotePath(%q)", in)
		}
	}
	if p, _ := validRemotePath("/x/../y//z/"); p != "/y/z" {
		t.Fatalf("clean: %q", p)
	}
}

func TestReadFollower(t *testing.T) {
	long := strings.Repeat("x", maxTailLineLen+100)
	in := "==> /a <==\nA1\nA2\n\n==> /b <==\nB1\n" + long + "\n\n==> /a <==\nA3\n==> /c <==\nnot a header\n"
	var got []tailLine
	readFollower(strings.NewReader(in), []string{"/a", "/b"}, func(l tailLine) bool { got = append(got, l); return true })
	want := []tailLine{{0, "A1"}, {0, "A2"}, {1, "B1"}, {1, strings.Repeat("x", maxTailLineLen)}, {0, "A3"}, {0, "==> /c <=="}, {0, "not a header"}}
	if len(got) != len(want) {
		t.Fatalf("lines: %d %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: %+v want %+v", i, got[i], want[i])
		}
	}
	// Single file: headers are plain text and blank lines are kept.
	got = nil
	readFollower(strings.NewReader("x\n\ny"), []string{"/a"}, func(l tailLine) bool { got = append(got, l); return true })
	if len(got) != 3 || got[1].text != "" || got[2].text != "y" {
		t.Fatalf("single: %+v", got)
	}
}

func TestTailRequest(t *testing.T) {
	q := map[string][]string{"path": {"/var/log/syslog", "/var/log/syslog", "/var/log/auth.log"}, "lines": {"50"}, "sudo": {"1"}}
	r, err := parseTailRequest(q)
	if err != nil || len(r.paths) != 2 || r.lines != 50 || !r.sudo {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Join(r.argv(), " ") != "tail -n 50 -F -- /var/log/syslog /var/log/auth.log" {
		t.Fatalf("argv: %v", r.argv())
	}
	j, err := parseTailRequest(map[string][]string{"unit": {"nginx.service"}})
	if err != nil || !j.journal || strings.Join(j.argv(), " ") != "journalctl -f -n 200 --no-pager -o short-iso -u nginx.service" {
		t.Fatalf("journal: %+v %v", j, err)
	}
	for _, bad := range []map[string][]string{{}, {"path": {"rel"}}, {"lines": {"-1"}, "path": {"/x"}}, {"journal": {"1"}, "path": {"/x"}},
		{"unit": {"a;b"}}, {"path": {"/1", "/2", "/3", "/4", "/5", "/6", "/7", "/8", "/9"}}} {
		if _, err := parseTailRequest(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestSignalsAndQuoting(t *testing.T) {
	for in, want := range map[string]string{"": "TERM", "sigkill": "KILL", "9": "KILL", "HUP": "HUP", "usr1": "USR1"} {
		if got, err := normalizeSignal(in); err != nil || got != want {
			t.Fatalf("normalizeSignal(%q) = %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"SEGV", "0", "TERM;ls", "-9"} {
		if _, err := normalizeSignal(bad); err == nil {
			t.Fatalf("accepted signal %q", bad)
		}
	}
	if shQuote("abc/d.e:f") != "abc/d.e:f" || shQuote("") != "''" || shQuote("it's") != `'it'\''s'` || shQuote("=x") != "'=x'" {
		t.Fatal("shQuote")
	}
	if psQuote("a'b\u2019c") != "'a''b\u2019\u2019c'" {
		t.Fatalf("psQuote: %s", psQuote("a'b\u2019c"))
	}
	enc := psEncode("# comment\r\n  Get-Date \n\n'x'")
	raw, _ := base64.StdEncoding.DecodeString(enc)
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if string(utf16.Decode(u)) != "Get-Date\n'x'" {
		t.Fatalf("psEncode: %q", string(utf16.Decode(u)))
	}
	// Every PowerShell script must fit cmd.exe's 8191-character command line once encoded.
	for _, name := range []string{scriptProbePS, scriptWindowsLoop, scriptWindowsProcs, scriptWindowsPorts, scriptWindowsServices} {
		if l := len(powershellLine(script(name))); l > 8000 {
			t.Fatalf("%s encodes to %d characters", name, l)
		}
	}
}

// TestShellScripts checks the POSIX scripts (and generated sudo / watchdog scripts) parse with /bin/sh and that
// argument quoting round-trips through a real shell.
func TestShellScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
	check := func(name, src string) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-n")
		cmd.Stdin = strings.NewReader(src)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s\n%s", name, err, out, src)
		}
	}
	for _, p := range []string{platLinux, platDarwin, platFreeBSD} {
		c, err := loopCommand(p, 2*time.Second, 0)
		if err != nil || strings.Contains(c.sh, "__") {
			t.Fatalf("%s loop: %v", p, err)
		}
		check(p+" loop", c.sh)
	}
	for _, n := range []string{scriptProbe, scriptLinuxProcs, scriptUnixProcs, scriptLinuxPorts, scriptUnixPorts, scriptSystemdServices} {
		check(n, script(n))
	}
	line, doc := sudoLine([]string{"kill", "-TERM", "42"}, `p@ss 'w"$x`)
	check("sudo", "exec "+line+doc)
	check("watch", watchScript(line+" 2>&1", doc))
	check("watch plain", watchScript(shJoin([]string{"tail", "-F", "--", "/var/log/x y"})+" </dev/null 2>&1", ""))
	if !strings.Contains(doc, "\n"+`p@ss 'w"$x`+"\n") || strings.Contains(line, "p@ss") {
		t.Fatalf("password must only be in the here-document: %q %q", line, doc)
	}
	args := []string{"plain", "with space", "it's", `$HOME`, "`x`", "a\\b", "=eq", ""}
	out, err := exec.Command("/bin/sh", "-c", `printf '%s\n' `+shJoin(args)).Output()
	if err != nil || string(out) != strings.Join(args, "\n")+"\n" {
		t.Fatalf("round trip: %q %v", out, err)
	}
}
