package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeRunner serves canned stdout for streams and records how each stream ended.
type fakeRunner struct {
	out  string // written to stdout when the stream starts
	hang bool   // keep stdout open after out until the stream is stopped

	mu    sync.Mutex
	stops int
	waits int
}

func (f *fakeRunner) run(context.Context, command) (*result, error) { return &result{}, nil }

func (f *fakeRunner) start(context.Context, command) (*stream, error) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(f.out))
		if !f.hang {
			_ = pw.Close()
		}
	}()
	var once, waitOnce sync.Once
	stop := func() {
		once.Do(func() {
			f.mu.Lock()
			f.stops++
			f.mu.Unlock()
		})
		_ = pw.CloseWithError(io.EOF)
		_ = pr.Close()
	}
	return &stream{Stdout: pr, stop: stop, wait: func() (*result, error) {
		waitOnce.Do(func() { // idempotent, like the real runners' wait
			f.mu.Lock()
			f.waits++
			f.mu.Unlock()
		})
		return &result{}, nil
	}}, nil
}

func (f *fakeRunner) counts() (stops, waits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops, f.waits
}

const oneLinuxSample = "@@S\n@@UPTIME\n1000.00 1500.00\n@@LOAD\n0.10 0.20 0.30 1/100 42\n@@CPU\ncpu  10 0 10 100 0 0 0 0 0 0\n" +
	"cpu0 10 0 10 100 0 0 0 0 0 0\n@@MEM\nMemTotal: 1000 kB\nMemAvailable: 400 kB\n@@USERS\n1\n@@PROCS 12\n@@E\n"

// A sampler that stops producing samples (a hung df, …) is ended by the watchdog and reported, instead of leaving
// the bar stale forever.
func TestSamplerWatchdog(t *testing.T) {
	old := stallTimeout
	stallTimeout = 150 * time.Millisecond
	defer func() { stallTimeout = old }()
	f := &fakeRunner{out: oneLinuxSample, hang: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := 0
	start := time.Now()
	err := streamSamples(ctx, f, &hostInfo{Platform: platLinux}, 10*time.Millisecond, func(*Stats) { got++ })
	if err == nil || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("err = %v", err)
	}
	if got != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("samples %d after %v", got, time.Since(start))
	}
	if stops, waits := f.counts(); stops == 0 || waits != 1 {
		t.Fatalf("stream not finished: stops %d waits %d", stops, waits)
	}
}

// The log follower must finish its stream (stop + wait: releases an overflow SSH connection, reaps a local process)
// on every exit path — here the browser simply goes away.
func TestFollowLogsFinishesStreamOnDisconnect(t *testing.T) {
	f := &fakeRunner{out: "hello\n", hang: true}
	s := New(nil, nil)
	tg := &target{id: "t", r: f, host: &hostInfo{Platform: platLinux}, key: "t", release: func() {}}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		s.followLogs(r.Context(), ws, tg, tailRequest{paths: []string{"/var/log/x"}, lines: 10})
		close(done)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Type  string   `json:"type"`
			Lines [][2]any `json:"lines"`
		}
		_ = json.Unmarshal(data, &m)
		if m.Type == "lines" && len(m.Lines) == 1 && m.Lines[0][1] == "hello" {
			break
		}
	}
	_ = c.CloseNow() // no {type:'stop'}, no close handshake
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("follower still running after the client left")
	}
	if stops, waits := f.counts(); stops == 0 || waits != 1 {
		t.Fatalf("stream not finished: stops %d waits %d", stops, waits)
	}
}

// The Linux sampler passes only local, non-runtime filesystems to df (a Docker / Kubernetes host has one mount per
// container), keeping removable media under /run/media and decoding spaces.
func TestLinuxSamplerMountFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
	src := script(scriptLinuxLoop)
	i, j := strings.Index(src, "nx_mounts() {"), strings.Index(src, "\n}\n")
	if i < 0 || j < i {
		t.Fatal("nx_mounts not found")
	}
	mounts := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(mounts, []byte(`overlay / overlay rw 0 0
proc /proc proc rw 0 0
tmpfs /dev tmpfs rw 0 0
/dev/vdb1 /config ext4 rw 0 0
/dev/sda1 /mnt/My\040Disk ext4 rw 0 0
overlay /var/lib/docker/overlay2/abc/merged overlay rw 0 0
/dev/sda2 /var/lib/kubelet/pods/x/volumes ext4 rw 0 0
/dev/sdb1 /run/media/lior/USB vfat rw 0 0
/dev/sdc1 /run/foo ext4 rw 0 0
lima /keys virtiofs rw 0 0
srv:/export /nfs nfs4 rw 0 0
nsfs /run/docker/netns/x nsfs rw 0 0
/dev/loop3 /snap/core/1 squashfs ro 0 0
/dev/sda3 /var/snapshots ext4 rw 0 0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	fn := strings.ReplaceAll(src[i:j+3], "/proc/mounts", mounts)
	out, err := exec.Command("/bin/sh", "-c", fn+"\nnx_mounts\n").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "overlay\t/\next4\t/config\next4\t/mnt/My Disk\nvfat\t/run/media/lior/USB\next4\t/var/snapshots\n"
	if string(out) != want {
		t.Fatalf("mounts:\n%s\nwant:\n%s", out, want)
	}
	if skipMount("/run/media/lior/USB", "vfat", "/dev/sdb1", platLinux) || !skipMount("/run/user/1000", "ext4", "x", platLinux) {
		t.Fatal("skipMount: /run/media must stay, the rest of /run must go")
	}
}

func TestUnitNameValidation(t *testing.T) {
	for _, ok := range []string{"nginx.service", `systemd-fsck@dev-disk-by\x2duuid-1a2b.service`, "getty@tty1.service", "Dhcp", "wuauserv"} {
		if validUnit(ok) != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "a;b", "a'b", `a"b`, "a/b", "a$b", "a`b`", "a\nb", "../x", strings.Repeat("a", 300)} {
		if validUnit(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// darwinMemory follows Activity Monitor: used = anonymous − purgeable + wired + compressed.
func TestDarwinMemory(t *testing.T) {
	lines := splitLines([]byte(`Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                5854.
Pages active:                            187067.
Pages inactive:                          176815.
Pages speculative:                         8795.
Pages wired down:                        352374.
Pages purgeable:                              2.
File-backed pages:                       106745.
Anonymous pages:                         204290.
Pages occupied by compressor:            454837.
`))
	const total = 19327352832
	m, ok := darwinMemory(total, 4096, lines)
	used := int64(204290-2+352374+454837) * 16384
	if !ok || m.Used != used || m.Available != total-used || m.Cached != (106745+2)*16384 {
		t.Fatalf("mem %+v, want used %d", m, used)
	}
	if _, ok := darwinMemory(total, 4096, nil); ok {
		t.Fatal("no vm_stat output must not produce figures")
	}
}

// Local filesystems: bind mounts of one device merge into the shortest path (a container's /config, /etc/hosts…
// with the overlay root by numbers), APFS volumes of one container into the Data volume even when their readings
// differ by a few blocks (statfs calls race with writes), and the primary volume comes first.
func TestMergeLocalDisks(t *testing.T) {
	const gb = int64(1) << 30
	mounts := func(ds []DiskStats) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Mount)
		}
		return strings.Join(out, ",")
	}
	linux := mergeLocalDisks("linux", []DiskStats{
		{Mount: "/config", FS: "ext4", Device: "/dev/vdb1", Total: 100 * gb, Used: 8 * gb, Avail: 90 * gb},
		{Mount: "/", FS: "overlay", Device: "overlay", Total: 100 * gb, Used: 8 * gb, Avail: 90 * gb},
		{Mount: "/data/sub", FS: "ext4", Device: "/dev/sdb1", Total: 50 * gb, Used: 1 * gb, Avail: 49 * gb},
		{Mount: "/data", FS: "ext4", Device: "/dev/sdb1", Total: 50 * gb, Used: 1*gb + 4096, Avail: 49 * gb},
		{Mount: "/boot/efi", FS: "vfat", Device: "/dev/sda1", Total: gb, Used: 1 << 20, Avail: gb - 1<<20},
	})
	if got := mounts(linux); got != "/,/boot/efi,/data" {
		t.Fatalf("linux: %s", got)
	}
	mac := mergeLocalDisks("darwin", []DiskStats{
		{Mount: "/", FS: "apfs", Device: "/dev/disk3s1s1", Total: 494 * gb, Used: 436 * gb, Avail: 46 * gb},
		{Mount: "/System/Volumes/Data", FS: "apfs", Device: "/dev/disk3s5", Total: 494 * gb, Used: 436*gb + 8192, Avail: 46*gb - 8192},
		{Mount: "/Volumes/Backup", FS: "apfs", Device: "/dev/disk5s1", Total: 2000 * gb, Used: 10 * gb, Avail: 1990 * gb},
	})
	if got := mounts(mac); got != "/System/Volumes/Data,/Volumes/Backup" || mac[0].Device != "/dev/disk3s5" {
		t.Fatalf("darwin: %s %+v", got, mac)
	}
	win := mergeLocalDisks("windows", []DiskStats{
		{Mount: "D:", FS: "NTFS", Total: 100 * gb, Used: gb, Avail: 99 * gb},
		{Mount: "C:", FS: "NTFS", Total: 500 * gb, Used: 200 * gb, Avail: 300 * gb},
	})
	if got := mounts(win); got != "C:,D:" {
		t.Fatalf("windows: %s", got)
	}
	if got := mergeLocalDisks("linux", nil); got == nil || len(got) != 0 {
		t.Fatalf("empty: %#v", got)
	}
}
