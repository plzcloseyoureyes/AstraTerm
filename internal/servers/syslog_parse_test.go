package servers

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

func TestParseSyslog5424(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	raw := `<165>1 2003-10-11T22:14:15.003Z mymachine.example.com evntslog 1234 ID47 [exampleSDID@32473 iut="3" eventSource="Application" eventID="1011"][examplePriority@32473 class="high"]` +
		" \xef\xbb\xbfAn application event log entry..."
	m := parseSyslog([]byte(raw), now)
	if m.Format != "rfc5424" || m.Facility != 20 || m.Severity != 5 {
		t.Fatalf("pri/format: %+v", m)
	}
	if m.Hostname != "mymachine.example.com" || m.AppName != "evntslog" || m.ProcID != "1234" || m.MsgID != "ID47" {
		t.Fatalf("header: %+v", m)
	}
	if !strings.HasPrefix(m.StructuredData, "[exampleSDID@32473") || !strings.HasSuffix(m.StructuredData, `class="high"]`) {
		t.Fatalf("sd: %q", m.StructuredData)
	}
	if m.Message != "An application event log entry..." {
		t.Fatalf("msg: %q", m.Message)
	}
	if m.Timestamp == nil || !m.Timestamp.Equal(time.Date(2003, 10, 11, 22, 14, 15, 3e6, time.UTC)) {
		t.Fatalf("ts: %v", m.Timestamp)
	}

	m = parseSyslog([]byte(`<34>1 - - - - - -`), now)
	if m.Format != "rfc5424" || m.Timestamp != nil || m.Hostname != "" || m.Message != "" || m.Severity != 2 {
		t.Fatalf("nil fields: %+v", m)
	}
	// Escaped bracket inside a param value.
	m = parseSyslog([]byte(`<13>1 2026-01-02T03:04:05+02:00 h a p m [x@1 k="a\]b"] msg`), now)
	if m.StructuredData != `[x@1 k="a\]b"]` || m.Message != "msg" {
		t.Fatalf("escaped sd: %+v", m)
	}
}

func TestParseSyslog3164(t *testing.T) {
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	m := parseSyslog([]byte("<34>Oct 11 22:14:15 mymachine su: 'su root' failed for lonvick on /dev/pts/8\n"), now)
	if m.Format != "rfc3164" || m.Facility != 4 || m.Severity != 2 || m.Hostname != "mymachine" || m.AppName != "su" ||
		m.Message != "'su root' failed for lonvick on /dev/pts/8" {
		t.Fatalf("classic: %+v", m)
	}
	if m.Timestamp == nil || m.Timestamp.Month() != time.October || m.Timestamp.Day() != 11 || m.Timestamp.Year() != 2026 {
		t.Fatalf("ts: %v", m.Timestamp)
	}
	m = parseSyslog([]byte("<13>Oct  2 09:05:07 host sshd[4242]: Accepted publickey for bob"), now)
	if m.Hostname != "host" || m.AppName != "sshd" || m.ProcID != "4242" || m.Message != "Accepted publickey for bob" ||
		m.Timestamp.Day() != 2 {
		t.Fatalf("padded day / pid: %+v", m)
	}
	// No host name (macOS logger style).
	m = parseSyslog([]byte("<13>Oct 12 07:00:00 myapp[12]: started"), now)
	if m.Hostname != "" || m.AppName != "myapp" || m.ProcID != "12" || m.Message != "started" {
		t.Fatalf("no host: %+v", m)
	}
	// December message received in January belongs to the previous year.
	jan := time.Date(2027, 1, 1, 0, 5, 0, 0, time.UTC)
	m = parseSyslog([]byte("<13>Dec 31 23:59:59 h app: bye"), jan)
	if m.Timestamp.Year() != 2026 {
		t.Fatalf("year rollover: %v", m.Timestamp)
	}
	// ISO timestamp (rsyslog high precision).
	m = parseSyslog([]byte("<14>2026-09-27T10:11:12.5+02:00 router1 kernel: link up"), now)
	if m.Hostname != "router1" || m.AppName != "kernel" || m.Message != "link up" || m.Timestamp == nil {
		t.Fatalf("iso: %+v", m)
	}
}

func TestParseSyslogCisco(t *testing.T) {
	now := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	m := parseSyslog([]byte("<189>123: *Mar  1 00:00:05.123 UTC: %SYS-5-CONFIG_I: Configured from console by vty0"), now)
	if m.Facility != 23 || m.Severity != 5 || m.Hostname != "" ||
		m.Message != "%SYS-5-CONFIG_I: Configured from console by vty0" {
		t.Fatalf("cisco: %+v", m)
	}
	if m.Timestamp == nil || m.Timestamp.Nanosecond() != 123e6 || m.Timestamp.Location() != time.UTC {
		t.Fatalf("cisco ts: %v", m.Timestamp)
	}
	m = parseSyslog([]byte("<187>45: Mar  1 10:00:00: %LINK-3-UPDOWN: Interface Gi0/1, changed state to down"), now)
	if m.Severity != 3 || !strings.HasPrefix(m.Message, "%LINK-3-UPDOWN") {
		t.Fatalf("cisco 2: %+v", m)
	}
}

func TestParseSyslogRaw(t *testing.T) {
	now := time.Now()
	m := parseSyslog([]byte("just some text"), now)
	if m.Format != "raw" || m.Facility != 1 || m.Severity != 5 || m.Message != "just some text" {
		t.Fatalf("raw: %+v", m)
	}
	for _, s := range []string{"<999>x", "<>x", "<1a>x", "<007>x"} {
		if m := parseSyslog([]byte(s), now); m.Format != "raw" {
			t.Errorf("%q parsed as %s", s, m.Format)
		}
	}
	m = parseSyslog([]byte("<13>bad \xff\xfe utf8"), now)
	if !strings.Contains(m.Message, "�") {
		t.Fatalf("invalid UTF-8 kept: %q", m.Message)
	}
	long := "<13>" + strings.Repeat("x", maxSyslogMessage*2)
	if m := parseSyslog([]byte(long), now); len(m.Message) > maxSyslogMessage {
		t.Fatal("oversized message not truncated")
	}
}

func TestFrames(t *testing.T) {
	in := "11 <13>hello A12 <13>world B\n<13>line one\r\n<13>line two\n"
	br := bufio.NewReader(strings.NewReader(in))
	var got []string
	for {
		c, err := br.Peek(1)
		if err != nil {
			break
		}
		var f []byte
		if c[0] >= '1' && c[0] <= '9' {
			f, err = readOctetFrame(br)
		} else {
			f, err = readLineFrame(br)
		}
		if len(f) > 0 {
			got = append(got, string(f))
		}
		if err != nil {
			break
		}
	}
	want := []string{"<13>hello A", "<13>world B\n", "<13>line one", "<13>line two"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("frames %q, want %q", got, want)
	}
	if _, err := readOctetFrame(bufio.NewReader(strings.NewReader("99999999 x"))); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestFormatSyslogLine(t *testing.T) {
	m := SyslogMessage{Received: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Source: "10.0.0.1", Facility: 16, Severity: 6,
		Hostname: "sw1", AppName: "app", ProcID: "7", Message: "a\nb"}
	line := formatSyslogLine(&m)
	if !strings.Contains(line, "10.0.0.1 local0.info sw1 app[7]: a\\nb") {
		t.Fatalf("line: %q", line)
	}
}
