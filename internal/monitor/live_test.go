package monitor

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Live tests against the shared Docker test environment (ssh1: Alpine/BusyBox with procps + coreutils, user test/test,
// sudo with password). Enable with TERMSTEAD_TESTENV=1; TERMSTEAD_TESTENV_SSH1 overrides the address.

func liveClient(t *testing.T) *ssh.Client {
	t.Helper()
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run tests against the Docker test environment")
	}
	addr := os.Getenv("TERMSTEAD_TESTENV_SSH1")
	if addr == "" {
		addr = "127.0.0.1:22022"
	}
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.Password("test")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // test container only
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func rawRunner(c *ssh.Client) sshRunner {
	return sshRunner{open: func(context.Context) (*ssh.Session, func(), error) {
		s, err := c.NewSession()
		return s, func() {}, err
	}}
}

func liveTarget(t *testing.T) (*Service, *target, *ssh.Client) {
	t.Helper()
	c := liveClient(t)
	s := New(nil, nil)
	r := rawRunner(c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, err := probeHost(ctx, r)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return s, &target{id: "live", r: r, host: host, key: "live", release: func() {}}, c
}

func remoteOut(t *testing.T, c *ssh.Client, cmd string) string {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		t.Fatalf("%s: %v %s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLiveProbeAndSampler(t *testing.T) {
	_, tg, _ := liveTarget(t)
	h := tg.host
	if h.Platform != platLinux || !strings.Contains(h.OS, "Alpine") || h.Hostname == "" || h.Kernel == "" || h.Cores < 1 {
		t.Fatalf("probe: %+v", h)
	}
	if !h.has("sudo") {
		t.Fatalf("sudo not detected: %+v", h.Tools)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4500*time.Millisecond)
	defer cancel()
	var got []*Stats
	err := streamSamples(ctx, tg.r, h, time.Second, func(st *Stats) { got = append(got, st) })
	if err == nil || ctx.Err() == nil {
		t.Fatalf("sampler ended early: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("got %d samples", len(got))
	}
	if !got[0].Warmup || got[1].Warmup {
		t.Fatalf("warmup flags: %v %v", got[0].Warmup, got[1].Warmup)
	}
	st := got[len(got)-1]
	if st.Platform != platLinux || st.Hostname == "" || st.UptimeSec <= 0 || st.Processes <= 0 {
		t.Fatalf("stats: %+v", st)
	}
	if st.CPU.Usage < 0 || st.CPU.Usage > 100 || st.CPU.Cores < 1 || len(st.CPU.PerCore) != st.CPU.Cores {
		t.Fatalf("cpu: %+v", st.CPU)
	}
	if st.Mem.Total <= 0 || st.Mem.Used <= 0 || st.Mem.Used > st.Mem.Total || st.Mem.Available <= 0 {
		t.Fatalf("mem: %+v", st.Mem)
	}
	root := false
	for _, d := range st.Disks {
		if d.Mount == "/" && d.Total > 0 && d.Used > 0 {
			root = true
		}
	}
	if !root || st.Disks[0].Mount != "/" {
		t.Fatalf("disks: %+v", st.Disks)
	}
	eth := false
	for _, n := range st.Net {
		if n.Iface == "eth0" && n.RxTotal > 0 {
			eth = true
		}
		if n.Iface == "lo" {
			t.Fatal("loopback listed")
		}
	}
	if !eth || st.IntervalSec < 0.5 || st.IntervalSec > 3 {
		t.Fatalf("net: %+v interval %v", st.Net, st.IntervalSec)
	}
	if st.FDs == nil || st.FDs.Used <= 0 || st.Threads <= 0 {
		t.Fatalf("fds %+v threads %d", st.FDs, st.Threads)
	}
}

func TestLiveOneShot(t *testing.T) {
	s, tg, _ := liveTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := s.oneShot(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Warmup || st.Mem.Total == 0 || len(st.Disks) == 0 || st.IntervalSec <= 0 {
		t.Fatalf("one-shot: %+v", st)
	}
}

func TestLiveProcessesReniceKill(t *testing.T) {
	s, tg, c := liveTarget(t)
	pid, _ := strconv.Atoi(remoteOut(t, c, "nohup sleep 311 >/dev/null 2>&1 & echo $!"))
	if pid <= 1 {
		t.Fatal("no pid")
	}
	t.Cleanup(func() { _ = c.Close })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	find := func() *Process {
		s.forgetProcesses(tg.id)
		procs, err := s.processes(ctx, tg)
		if err != nil {
			t.Fatal(err)
		}
		if len(procs) < 3 {
			t.Fatalf("only %d processes", len(procs))
		}
		for i := range procs {
			if procs[i].PID == pid {
				return &procs[i]
			}
		}
		return nil
	}
	p := find()
	if p == nil || p.User != "test" || !strings.Contains(p.Command, "sleep 311") || p.Started == "" || p.RSS < 0 {
		t.Fatalf("process: %+v", p)
	}
	if err := s.renice(ctx, tg, pid, 10, false); err != nil {
		t.Fatal(err)
	}
	if p := find(); p == nil || p.Nice != 10 {
		t.Fatalf("renice: %+v", p)
	}
	// Signalling a root process without sudo is refused with a permission error.
	if err := s.kill(ctx, tg, 1, "TERM", false); err == nil {
		t.Fatal("pid 1 accepted")
	}
	if err := s.kill(ctx, tg, pid, "SIGTERM", false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for find() != nil {
		if time.Now().After(deadline) {
			t.Fatal("process still alive after TERM")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := s.kill(ctx, tg, pid, "KILL", false); err != errNoProcess {
		t.Fatalf("kill of a gone process: %v", err)
	}
}

func TestLivePortsAndDiskUsage(t *testing.T) {
	s, tg, _ := liveTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ports, err := s.listPorts(ctx, tg, false)
	if err != nil {
		t.Fatal(err)
	}
	sshd := false
	for _, p := range ports {
		if p.Proto == "tcp" && p.Port == 2222 {
			sshd = true
		}
	}
	if !sshd {
		t.Fatalf("sshd port missing: %+v", ports)
	}
	du, err := s.diskUsage(ctx, tg, "/usr", false)
	if err != nil {
		t.Fatal(err)
	}
	if du.Total <= 0 || len(du.Entries) == 0 || du.Parent != "/" {
		t.Fatalf("du: %+v", du)
	}
	var sum int64
	for _, e := range du.Entries {
		sum += e.Size
	}
	if sum != du.Total {
		t.Fatalf("entries sum %d != total %d", sum, du.Total)
	}
	if _, err := s.diskUsage(ctx, tg, "relative", false); err == nil {
		t.Fatal("relative path accepted")
	}
	list, err := s.listServices(ctx, tg)
	if err != nil || list.Manager != "" || list.Message == "" {
		t.Fatalf("services on a systemd-less host: %+v %v", list, err)
	}
}

func TestLiveFollowerStopsRemoteTail(t *testing.T) {
	s, tg, c := liveTarget(t)
	file := remoteOut(t, c, "f=$(mktemp); echo first-line >> $f; echo $f")
	t.Cleanup(func() { remoteOut(t, c, "rm -f "+file) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := tailRequest{paths: []string{file}, lines: 10}
	st, err := s.startFollower(ctx, tg, req)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go readFollower(st.Stdout, req.paths, func(l tailLine) bool { lines <- l.text; return true })
	expect := func(want string) {
		t.Helper()
		select {
		case l := <-lines:
			if l != want {
				t.Fatalf("line %q, want %q", l, want)
			}
		case <-time.After(8 * time.Second):
			t.Fatalf("no line %q", want)
		}
	}
	expect("first-line")
	remoteOut(t, c, "echo second-line >> "+file)
	expect("second-line")
	count := func() int {
		out := remoteOut(t, c, "ps -o args= -u test | grep -c '^tail -n 10 -F' || true")
		n, _ := strconv.Atoi(strings.TrimSpace(out))
		return n
	}
	if count() != 1 {
		t.Fatalf("tail processes: %d", count())
	}
	st.stop()
	_, _ = st.wait()
	deadline := time.Now().Add(5 * time.Second)
	for count() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("remote tail survived the closed channel")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestLiveMultiFileHeaders(t *testing.T) {
	s, tg, c := liveTarget(t)
	a := remoteOut(t, c, "f=$(mktemp); echo a1 >> $f; echo $f")
	b := remoteOut(t, c, "f=$(mktemp); echo b1 >> $f; echo $f")
	t.Cleanup(func() { remoteOut(t, c, "rm -f "+a+" "+b) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := tailRequest{paths: []string{a, b}, lines: 5}
	st, err := s.startFollower(ctx, tg, req)
	if err != nil {
		t.Fatal(err)
	}
	defer st.stop()
	got := map[string]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		readFollower(bufio.NewReader(st.Stdout), req.paths, func(l tailLine) bool {
			if l.text != "" {
				got[l.text] = l.src
			}
			return len(got) < 3
		})
	}()
	time.Sleep(time.Second)
	remoteOut(t, c, "echo a2 >> "+a)
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("lines: %v", got)
	}
	if got["a1"] != 0 || got["b1"] != 1 || got["a2"] != 0 {
		t.Fatalf("attribution: %v", got)
	}
}
