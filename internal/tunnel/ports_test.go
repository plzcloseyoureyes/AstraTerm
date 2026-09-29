package tunnel

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const ssSample = `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
LISTEN 0      4096   127.0.0.53%lo:53         0.0.0.0:*     users:(("systemd-resolve",pid=551,fd=14))
LISTEN 0      128          0.0.0.0:22         0.0.0.0:*     users:(("sshd",pid=1021,fd=3))
LISTEN 0      511                *:8080             *:*     users:(("node",pid=4242,fd=20))
LISTEN 0      128             [::]:22            [::]:*     users:(("sshd",pid=1021,fd=4))
LISTEN 0      244        127.0.0.1:5432       0.0.0.0:*
LISTEN 0      244            [::1]:5432          [::]:*
LISTEN 0      511      10.1.2.3:9000          0.0.0.0:*
`

const busyboxNetstatSample = `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 127.0.0.11:46263        0.0.0.0:*               LISTEN      -
tcp        0      0 0.0.0.0:2222            0.0.0.0:*               LISTEN      237/ssh_host_rsa_ke
tcp        0      0 :::2222                 :::*                    LISTEN      237/ssh_host_rsa_ke
tcp        0      0 10.0.0.5:44321          10.0.0.9:22             ESTABLISHED 999/ssh
`

const procSample = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0B00007F:B4B7 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 637350 2 0000000000000000 100 0 0 10 0
   1: 00000000:08AE 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 640178 1 0000000000000000 100 0 0 10 0
   2: 0A0013AC:08AE 010013AC:E4FC 01 000001AC:00000000 01:00000014 00000000  1000        0 1096225 4 0000000000000000 20 4 19 10 -1
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:08AE 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 640179 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:0277 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 640180 1 0000000000000000 100 0 0 10 0
`

const lsofSample = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
launchd       1 root   13u  IPv6 0x1234567890abcdef      0t0  TCP *:22 (LISTEN)
cupsd       450 root    5u  IPv4 0x1234567890abcdee      0t0  TCP 127.0.0.1:631 (LISTEN)
cupsd       450 root    6u  IPv6 0x1234567890abcded      0t0  TCP [::1]:631 (LISTEN)
Google\x20C 900 me     40u  IPv4 0x1234567890abcdec      0t0  TCP 127.0.0.1:9222 (LISTEN)
`

const bsdSample = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  127.0.0.1.7822         *.*                    LISTEN
tcp46      0      0  *.22                   *.*                    LISTEN
tcp6       0      0  ::1.631                *.*                    LISTEN
tcp4       0      0  192.168.1.5.52000      1.2.3.4.443            ESTABLISHED
`

const windowsSample = `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1012
  TCP    127.0.0.1:5939         0.0.0.0:0              LISTENING       4120
  TCP    192.168.1.10:139       0.0.0.0:0              LISTENING       4
  TCP    [::]:135               [::]:0                 LISTENING       1012
  TCP    192.168.1.10:50000     1.2.3.4:443            ESTABLISHED     5000
  UDP    0.0.0.0:500            *:*                                    3000
`

func portsSummary(ports []RemotePort) string {
	var b []string
	for _, p := range ports {
		s := fmt.Sprintf("%s:%d/%s→%s", p.Address, p.Port, p.Scope, p.ConnectHost)
		if p.Process != "" || p.PID != 0 {
			s += fmt.Sprintf("(%s#%d)", p.Process, p.PID)
		}
		b = append(b, s)
	}
	return strings.Join(b, " ")
}

func TestPortParsers(t *testing.T) {
	cases := []struct {
		name string
		got  []RemotePort
		want string
	}{
		{"ss", finishPorts(parseSS([]byte(ssSample))),
			"0.0.0.0:22/all→127.0.0.1(sshd#1021) 127.0.0.53:53/loopback→127.0.0.53(systemd-resolve#551) 127.0.0.1:5432/loopback→127.0.0.1 *:8080/all→127.0.0.1(node#4242) 10.1.2.3:9000/address→10.1.2.3"},
		{"busybox netstat", finishPorts(parseNetstat([]byte(busyboxNetstatSample))),
			"0.0.0.0:2222/all→127.0.0.1(ssh_host_rsa_ke#237) 127.0.0.11:46263/loopback→127.0.0.11"},
		{"proc", finishPorts(parseProcNet([]byte(procSample))),
			"::1:631/loopback→::1 0.0.0.0:2222/all→127.0.0.1 127.0.0.11:46263/loopback→127.0.0.11"},
		{"lsof", finishPorts(parseLsof([]byte(lsofSample))),
			"*:22/all→127.0.0.1(launchd#1) 127.0.0.1:631/loopback→127.0.0.1(cupsd#450) 127.0.0.1:9222/loopback→127.0.0.1(Google C#900)"},
		{"bsd", finishPorts(parseBSDNetstat([]byte(bsdSample))),
			"*:22/all→127.0.0.1 ::1:631/loopback→::1 127.0.0.1:7822/loopback→127.0.0.1"},
		{"windows", finishPorts(parseWindowsNetstat([]byte(windowsSample))),
			"0.0.0.0:135/all→127.0.0.1(#1012) 192.168.1.10:139/address→192.168.1.10(#4) 127.0.0.1:5939/loopback→127.0.0.1(#4120)"},
	}
	for _, c := range cases {
		// Windows rows have PIDs but no names until tasklist is merged.
		if got := portsSummary(c.got); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

func TestSplitMarkerAndTasklist(t *testing.T) {
	m, body := splitMarker([]byte("motd junk\n@@ss\nLISTEN 0 1 1.2.3.4:5 0.0.0.0:*\n"))
	if m != "ss" || !strings.HasPrefix(string(body), "LISTEN") {
		t.Fatalf("marker %q body %q", m, body)
	}
	if m, _ := splitMarker([]byte("no marker")); m != "" {
		t.Fatalf("marker %q", m)
	}
	names := parseTasklist([]byte("\"System\",\"4\",\"Services\",\"0\",\"144 K\"\r\n\"svchost.exe\",\"1012\",\"Services\",\"0\",\"10,000 K\"\r\n"))
	if names[4] != "System" || names[1012] != "svchost.exe" {
		t.Fatalf("tasklist = %v", names)
	}
}

// fakeExec answers the probe script with canned output.
type fakeExec map[string]string

func (f fakeExec) Exec(_ context.Context, cmd string) ([]byte, []byte, int, error) {
	for k, v := range f {
		if strings.Contains(cmd, k) {
			return []byte(v), nil, 0, nil
		}
	}
	return nil, []byte("sh: not found"), 127, nil
}

func TestDetectPorts(t *testing.T) {
	res, err := detectPorts(context.Background(), fakeExec{"@@ss": "@@ss\n" + ssSample})
	if err != nil || res.Method != "ss" || len(res.Ports) != 5 || res.Note == "" {
		t.Fatalf("ss: %+v %v", res, err)
	}
	res, err = detectPorts(context.Background(), fakeExec{"@@ss": "@@proc\n" + procSample})
	if err != nil || res.Method != "proc" || len(res.Ports) != 3 {
		t.Fatalf("proc: %+v %v", res, err)
	}
	// Windows: the POSIX probe prints no marker.
	res, err = detectPorts(context.Background(), fakeExec{
		"netstat -ano": windowsSample,
		"tasklist":     "\"svchost.exe\",\"1012\",\"Services\",\"0\",\"1 K\"\r\n",
	})
	if err != nil || res.Method != "windows" || len(res.Ports) != 3 || res.Ports[0].Process != "svchost.exe" {
		t.Fatalf("windows: %+v %v", res, err)
	}
	if _, err := detectPorts(context.Background(), fakeExec{"@@ss": "@@none\n"}); err == nil {
		t.Fatal("no tool: expected an error")
	}
}

func TestGatewayWarning(t *testing.T) {
	ports := finishPorts(parseSS([]byte(ssSample)))
	cases := []struct {
		port int
		warn bool
	}{
		{5432, true},  // 127.0.0.1 and ::1 only
		{53, true},    // 127.0.0.53%lo
		{22, false},   // 0.0.0.0 and ::
		{8080, false}, // *
		{9000, false}, // a specific non-loopback address
		{4444, false}, // not listed: no conclusion
	}
	for _, c := range cases {
		if got := gatewayWarning(ports, c.port) != ""; got != c.warn {
			t.Errorf("port %d: warning = %v, want %v", c.port, got, c.warn)
		}
	}
}
