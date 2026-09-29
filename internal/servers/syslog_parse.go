package servers

import (
	"bytes"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SyslogMessage is one received syslog message (CC-5).
type SyslogMessage struct {
	ID        int64      `json:"id"`
	Received  time.Time  `json:"received"`
	Timestamp *time.Time `json:"timestamp,omitempty"`
	Source    string     `json:"source"`
	Transport string     `json:"transport"`
	Facility  int        `json:"facility"`
	Severity  int        `json:"severity"`
	Hostname  string     `json:"hostname,omitempty"`
	AppName   string     `json:"appName,omitempty"`
	ProcID    string     `json:"procId,omitempty"`
	MsgID     string     `json:"msgId,omitempty"`
	// StructuredData is the RFC 5424 STRUCTURED-DATA as sent ("[id k=\"v\"]…").
	StructuredData string `json:"structuredData,omitempty"`
	Message        string `json:"message"`
	// Format: rfc5424 | rfc3164 | raw.
	Format string `json:"format"`
}

const maxSyslogMessage = 64 << 10

var facilityNames = []string{"kern", "user", "mail", "daemon", "auth", "syslog", "lpr", "news", "uucp", "cron",
	"authpriv", "ftp", "ntp", "security", "console", "solaris-cron", "local0", "local1", "local2", "local3", "local4",
	"local5", "local6", "local7"}

var severityNames = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

func facilityName(f int) string {
	if f >= 0 && f < len(facilityNames) {
		return facilityNames[f]
	}
	return strconv.Itoa(f)
}

func severityName(s int) string {
	if s >= 0 && s < len(severityNames) {
		return severityNames[s]
	}
	return strconv.Itoa(s)
}

// parseSyslog parses one RFC 5424 or RFC 3164 (BSD) message; anything else is kept as a raw user.notice message.
func parseSyslog(raw []byte, now time.Time) SyslogMessage {
	if len(raw) > maxSyslogMessage {
		raw = raw[:maxSyslogMessage]
	}
	raw = bytes.TrimRight(raw, "\r\n\x00")
	s := string(raw)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	m := SyslogMessage{Received: now.UTC(), Facility: 1, Severity: 5, Format: "raw"}
	pri, rest, ok := parsePRI(s)
	if !ok {
		m.Message = strings.TrimLeft(s, " ")
		return m
	}
	m.Facility, m.Severity = pri/8, pri%8
	if strings.HasPrefix(rest, "1 ") {
		if parse5424(&m, rest[2:]) {
			return m
		}
	}
	parse3164(&m, rest, now)
	return m
}

// parsePRI reads "<N>" (0…191).
func parsePRI(s string) (int, string, bool) {
	if len(s) < 3 || s[0] != '<' {
		return 0, s, false
	}
	end := strings.IndexByte(s[:min(len(s), 5)], '>')
	if end < 2 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[1:end])
	if err != nil || n < 0 || n > 191 || (len(s[1:end]) > 1 && s[1] == '0') {
		return 0, s, false
	}
	return n, s[end+1:], true
}

// parse5424 parses "TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA [MSG]" (after "<PRI>1 ").
func parse5424(m *SyslogMessage, s string) bool {
	fields := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		sp := strings.IndexByte(s, ' ')
		if sp <= 0 {
			return false
		}
		fields = append(fields, s[:sp])
		s = s[sp+1:]
	}
	nil5424 := func(v string) string {
		if v == "-" {
			return ""
		}
		return v
	}
	if ts := fields[0]; ts != "-" {
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return false
		}
		m.Timestamp = &t
	}
	m.Hostname = nil5424(fields[1])
	m.AppName = nil5424(fields[2])
	m.ProcID = nil5424(fields[3])
	m.MsgID = nil5424(fields[4])
	sd, rest, ok := splitStructuredData(s)
	if !ok {
		return false
	}
	if sd != "-" {
		m.StructuredData = sd
	}
	rest = strings.TrimPrefix(rest, " ")
	rest = strings.TrimPrefix(rest, "\ufeff") // BOM before a UTF-8 MSG
	m.Message = rest
	m.Format = "rfc5424"
	return true
}

// splitStructuredData splits "-" or "[…][…]" (with \" \] \\ escapes inside param values) from the rest.
func splitStructuredData(s string) (sd, rest string, ok bool) {
	if strings.HasPrefix(s, "-") {
		return "-", s[1:], true
	}
	if !strings.HasPrefix(s, "[") {
		return "", s, false
	}
	i := 0
	for i < len(s) && s[i] == '[' {
		inQuote := false
		j := i + 1
		for ; j < len(s); j++ {
			c := s[j]
			if inQuote {
				if c == '\\' && j+1 < len(s) {
					j++
					continue
				}
				if c == '"' {
					inQuote = false
				}
				continue
			}
			if c == '"' {
				inQuote = true
			} else if c == ']' {
				break
			}
		}
		if j >= len(s) {
			return "", s, false
		}
		i = j + 1
	}
	return s[:i], s[i:], true
}

var months = map[string]time.Month{"Jan": 1, "Feb": 2, "Mar": 3, "Apr": 4, "May": 5, "Jun": 6, "Jul": 7, "Aug": 8,
	"Sep": 9, "Oct": 10, "Nov": 11, "Dec": 12}

// parse3164 parses BSD syslog: "[TIMESTAMP ]HOSTNAME TAG[PID]: MSG", including Cisco IOS style
// "SEQ: *Mmm dd hh:mm:ss.mmm TZ: %FAC-SEV-MNEMONIC: text" and ISO timestamps.
func parse3164(m *SyslogMessage, s string, now time.Time) {
	m.Format = "rfc3164"
	s = strings.TrimLeft(s, " ")
	// Cisco sequence number "123: ".
	if i := strings.Index(s, ": "); i > 0 && i <= 10 && isDigits(s[:i]) {
		s = s[i+2:]
	}
	s = strings.TrimLeft(s, "*.") // Cisco: '*' = clock not synchronized, '.' = synchronized once
	ts, rest, ok := parseBSDTime(s, now)
	if !ok {
		ts, rest, ok = parseISOTime(s)
	}
	if ok {
		m.Timestamp = &ts
		s = rest
		if strings.HasPrefix(s, ": ") { // Cisco: no host name, the timestamp ends with ':'
			m.Message = s[2:]
			return
		}
		s = strings.TrimLeft(s, " ")
		// HOSTNAME unless the next token already is the TAG ("app:" / "app[pid]:").
		if sp := strings.IndexByte(s, ' '); sp > 0 && !isTag(s[:sp]) {
			m.Hostname = s[:sp]
			s = s[sp+1:]
		}
	}
	// TAG
	if sp := strings.IndexByte(s, ' '); sp > 0 && isTag(s[:sp]) {
		tag := strings.TrimSuffix(s[:sp], ":")
		if lb := strings.IndexByte(tag, '['); lb > 0 && strings.HasSuffix(tag, "]") {
			m.ProcID = tag[lb+1 : len(tag)-1]
			tag = tag[:lb]
		}
		m.AppName = tag
		s = s[sp+1:]
	}
	m.Message = s
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isTag reports whether tok looks like "app:" or "app[pid]:" (1–48 printable characters before the colon).
func isTag(tok string) bool {
	if !strings.HasSuffix(tok, ":") || len(tok) < 2 || len(tok) > 64 {
		return false
	}
	t := tok[:len(tok)-1]
	if lb := strings.IndexByte(t, '['); lb >= 0 {
		if lb == 0 || !strings.HasSuffix(t, "]") {
			return false
		}
		t = t[:lb]
	}
	if len(t) > 48 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c <= ' ' || c > '~' || c == ':' {
			return false
		}
	}
	return true
}

// parseBSDTime parses "Mmm dd hh:mm:ss[.fff][ YYYY][ TZ]" (day possibly space padded).
func parseBSDTime(s string, now time.Time) (time.Time, string, bool) {
	if len(s) < 15 {
		return time.Time{}, s, false
	}
	mon, ok := months[s[:3]]
	if !ok || s[3] != ' ' {
		return time.Time{}, s, false
	}
	dayStr := strings.TrimLeft(s[4:6], " ")
	day, err := strconv.Atoi(dayStr)
	if err != nil || day < 1 || day > 31 || s[6] != ' ' {
		return time.Time{}, s, false
	}
	clock := s[7:15]
	if len(clock) != 8 || clock[2] != ':' || clock[5] != ':' {
		return time.Time{}, s, false
	}
	hh, e1 := strconv.Atoi(clock[0:2])
	mm, e2 := strconv.Atoi(clock[3:5])
	ss, e3 := strconv.Atoi(clock[6:8])
	if e1 != nil || e2 != nil || e3 != nil || hh > 23 || mm > 59 || ss > 60 {
		return time.Time{}, s, false
	}
	rest := s[15:]
	nsec := 0
	if len(rest) > 1 && rest[0] == '.' {
		j := 1
		for j < len(rest) && j <= 9 && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		frac := rest[1:j]
		if frac != "" {
			f, _ := strconv.Atoi((frac + "000000000")[:9])
			nsec = f
			rest = rest[j:]
		}
	}
	year := now.Year()
	// Optional year: " 2026".
	if len(rest) >= 5 && rest[0] == ' ' && isDigits(rest[1:5]) && (len(rest) == 5 || rest[5] == ' ' || rest[5] == ':') {
		if y, err := strconv.Atoi(rest[1:5]); err == nil && y > 1970 && y < 3000 {
			year = y
			rest = rest[5:]
		}
	}
	loc := now.Location()
	// Optional zone abbreviation before a Cisco-style ':' ("UTC:", "CET:").
	if len(rest) > 2 && rest[0] == ' ' {
		if c := strings.IndexByte(rest[1:], ':'); c > 0 && c <= 5 && isUpper(rest[1:1+c]) {
			if rest[1:1+c] == "UTC" || rest[1:1+c] == "GMT" {
				loc = time.UTC
			}
			rest = rest[1+c:]
		}
	}
	t := time.Date(year, mon, day, hh, mm, ss, nsec, loc)
	// No year in the message: a date "in the future" belongs to the previous year (December logs read in January).
	if year == now.Year() && t.After(now.Add(48*time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t, rest, true
}

func isUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return s != ""
}

// parseISOTime parses a leading RFC 3339 timestamp token.
func parseISOTime(s string) (time.Time, string, bool) {
	sp := strings.IndexByte(s, ' ')
	if sp < 19 || sp > 40 {
		return time.Time{}, s, false
	}
	tok := strings.TrimSuffix(s[:sp], ":")
	t, err := time.Parse(time.RFC3339Nano, tok)
	if err != nil {
		return time.Time{}, s, false
	}
	return t, s[sp:], true
}
