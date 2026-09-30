package keys

import (
	"bufio"
	"crypto/dsa" //nolint:staticcheck // legacy PuTTY "dss" host keys
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// known_hosts text formats (SSH-19/20): OpenSSH known_hosts (plain, [host]:port, hashed |1| entries, @cert-authority
// and @revoked markers) and the PuTTY host key cache (registry export "SshHostKeys" or MobaXterm.ini
// "[SSH_Hostkeys]"), whose hex key parameters are converted to SSH wire keys.

// Marker kinds (known_hosts "@cert-authority" / "@revoked").
const (
	markerCertAuthority = "cert-authority"
	markerRevoked       = "revoked"
)

// khEntry is one parsed known_hosts line.
type khEntry struct {
	Line    int
	Marker  string   // "", markerCertAuthority, markerRevoked
	Hosts   []string // host patterns, lower-cased (hashed entries keep their |1| form)
	Key     ssh.PublicKey
	Comment string
}

// khLineError is a line that could not be parsed.
type khLineError struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}

const maxKnownHostsLines = 20000

// parseOpenSSHKnownHosts parses known_hosts text. Blank lines and comments are skipped; bad lines are reported.
func parseOpenSSHKnownHosts(text string) ([]khEntry, []khLineError) {
	var (
		out  []khEntry
		errs []khLineError
	)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		if n > maxKnownHostsLines {
			errs = append(errs, khLineError{Line: n, Error: fmt.Sprintf("too many lines (at most %d are imported)", maxKnownHostsLines)})
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e, err := parseKnownHostsLine(line)
		if err != nil {
			errs = append(errs, khLineError{Line: n, Error: err.Error()})
			continue
		}
		e.Line = n
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, khLineError{Line: n + 1, Error: "line too long"})
	}
	return out, errs
}

func parseKnownHostsLine(line string) (khEntry, error) {
	var e khEntry
	fields := strings.Fields(line)
	if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
		switch m := strings.ToLower(fields[0][1:]); m {
		case markerCertAuthority, markerRevoked:
			e.Marker = m
		default:
			return e, fmt.Errorf("unknown marker %q", fields[0])
		}
		fields = fields[1:]
	}
	if len(fields) < 3 {
		return e, fmt.Errorf("expected \"hosts key-type base64-key [comment]\"")
	}
	raw, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		return e, fmt.Errorf("invalid base64 key")
	}
	pk, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return e, fmt.Errorf("invalid key: %s", trimSSHErr(err))
	}
	if pk.Type() != fields[1] {
		return e, fmt.Errorf("key type mismatch: %q declared, %q encoded", fields[1], pk.Type())
	}
	if _, isCert := pk.(*ssh.Certificate); isCert {
		return e, fmt.Errorf("certificates cannot be trusted directly; trust their CA with @cert-authority")
	}
	for h := range strings.SplitSeq(fields[0], ",") {
		if h = strings.TrimSpace(h); h != "" {
			if !strings.HasPrefix(h, "|") {
				h = strings.ToLower(h)
			}
			e.Hosts = append(e.Hosts, h)
		}
	}
	if len(e.Hosts) == 0 {
		return e, fmt.Errorf("no host names")
	}
	e.Key, e.Comment = pk, cleanComment(strings.Join(fields[3:], " "))
	return e, nil
}

// hostKind classifies a host field of a plain entry.
type hostKind int

const (
	hostPlain hostKind = iota
	hostHashed
	hostPattern
	hostInvalid
)

// splitKnownHost parses "host" or "[host]:port" of a plain (non-marker) entry.
func splitKnownHost(h string) (host string, port int, kind hostKind) {
	switch {
	case strings.HasPrefix(h, "|"):
		return "", 0, hostHashed
	case strings.HasPrefix(h, "!") || strings.ContainsAny(h, "*?"):
		return "", 0, hostPattern
	}
	host, port = h, 22
	if strings.HasPrefix(h, "[") {
		end := strings.Index(h, "]")
		if end < 0 {
			return "", 0, hostInvalid
		}
		host = h[1:end]
		if rest := h[end+1:]; rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", 0, hostInvalid
			}
			p, err := strconv.Atoi(rest[1:])
			if err != nil || p < 1 || p > 65535 {
				return "", 0, hostInvalid
			}
			port = p
		}
	}
	host, err := normalizeHost(host)
	if err != nil {
		return "", 0, hostInvalid
	}
	return host, port, hostPlain
}

// normalizeHost validates a host name or IP address of a known host and lower-cases it.
func normalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(h), "[]")))
	if h == "" || len(h) > 253 {
		return "", fmt.Errorf("invalid host name")
	}
	if strings.ContainsAny(h, " \t,*?!/\\[]@\"'") || strings.ContainsFunc(h, isControl) {
		return "", fmt.Errorf("invalid host name %q", h)
	}
	return h, nil
}

// knownHostName renders host:port the way known_hosts stores it.
func knownHostName(host string, port int) string {
	if port == 0 || port == 22 {
		return host
	}
	return "[" + host + "]:" + strconv.Itoa(port)
}

// hashHostName returns OpenSSH's hashed form |1|salt|HMAC-SHA1(salt, name)| (HashKnownHosts yes).
func hashHostName(name string) string {
	salt := make([]byte, sha1.Size)
	_, _ = rand.Read(salt)
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(name))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// validHashedHost reports whether entry is a well-formed |1|salt|hash name (20-byte salt and HMAC-SHA1).
func validHashedHost(entry string) bool {
	parts := strings.Split(entry, "|")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "1" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	sum, err2 := base64.StdEncoding.DecodeString(parts[3])
	return err1 == nil && err2 == nil && len(salt) == sha1.Size && len(sum) == sha1.Size
}

// hashedHostMatches reports whether a |1|salt|hash entry is name.
func hashedHostMatches(entry, name string) bool {
	parts := strings.Split(entry, "|")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "1" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(name))
	return hmac.Equal(mac.Sum(nil), want)
}

// matchHostPatterns reports whether one of names (e.g. "[host]:port" and "host") matches the comma-separated OpenSSH
// pattern list: "*" and "?" wildcards, hashed entries, and "!" negations, which win when they match any of the names.
func matchHostPatterns(patterns string, names ...string) bool {
	matched := false
	for p := range strings.SplitSeq(patterns, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		for _, name := range names {
			name = strings.ToLower(name)
			var ok bool
			if strings.HasPrefix(p, "|") {
				ok = hashedHostMatches(p, name)
			} else {
				ok = wildcardMatch(strings.ToLower(p), name)
			}
			if ok {
				if neg {
					return false
				}
				matched = true
			}
		}
	}
	return matched
}

// markerNames are the names a marker's patterns are matched against for host:port: the known_hosts form ("[host]:port"
// for other ports than 22) and — because certificates name hosts, not ports — the bare host name, so that
// "*.example.com" covers every port (OpenSSH would require "[*.example.com]:*"); "[pattern]:port" limits to a port.
func markerNames(host string, port int) []string {
	host = strings.ToLower(host)
	if name := knownHostName(host, port); name != host {
		return []string{name, host}
	}
	return []string{host}
}

// wildcardMatch matches s against a glob pattern with "*" (any run) and "?" (any single character).
func wildcardMatch(pattern, s string) bool {
	px, sx := 0, 0
	starP, starS := -1, -1
	for sx < len(s) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]):
			px++
			sx++
		case px < len(pattern) && pattern[px] == '*':
			starP, starS = px, sx
			px++
		case starP >= 0:
			px = starP + 1
			starS++
			sx = starS
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// validateHostPatterns normalizes a marker's comma-separated host pattern list.
func validateHostPatterns(s string) (string, error) {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		body := strings.TrimPrefix(p, "!")
		if strings.HasPrefix(body, "|") {
			// A hashed name (HashKnownHosts): base64 salt and HMAC, which may contain "/" and "+".
			if !validHashedHost(body) {
				return "", fmt.Errorf("invalid hashed host %q", p)
			}
			out = append(out, p)
			continue
		}
		p, body = strings.ToLower(p), strings.ToLower(body)
		if body == "" || len(body) > 260 || strings.ContainsAny(body, " \t\"'\\/@|") || strings.ContainsFunc(body, isControl) {
			return "", fmt.Errorf("invalid host pattern %q", p)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("at least one host pattern is required (e.g. *.example.com, or * for all hosts)")
	}
	if len(out) > 64 {
		return "", fmt.Errorf("too many host patterns")
	}
	return strings.Join(out, ","), nil
}

// ---- PuTTY / MobaXterm host key cache ---------------------------------------------------------------------------

// puttyHostKeyLine matches `"ssh-ed25519@22:host"="0x…,0x…"` (.reg export) and `ssh-ed25519@22:host=0x…,0x…`
// (MobaXterm.ini [SSH_Hostkeys]).
var puttyHostKeyLine = regexp.MustCompile(`^"?([A-Za-z0-9-]+)@(\d{1,5}):([^"=\s]+)"?\s*=\s*"?([^"]*)"?\s*$`)

// looksLikePuTTYHostKeys reports whether text contains PuTTY host key cache entries.
func looksLikePuTTYHostKeys(text string) bool {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for i := 0; sc.Scan() && i < 200; i++ {
		if puttyHostKeyLine.MatchString(strings.TrimSpace(sc.Text())) {
			return true
		}
	}
	return false
}

// parsePuTTYHostKeys parses PuTTY / MobaXterm host key cache entries; other lines (section headers, the registry
// header, blank lines) are ignored.
func parsePuTTYHostKeys(text string) ([]khEntry, []khLineError) {
	var (
		out  []khEntry
		errs []khLineError
	)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		if n > maxKnownHostsLines {
			errs = append(errs, khLineError{Line: n, Error: fmt.Sprintf("too many lines (at most %d are imported)", maxKnownHostsLines)})
			break
		}
		line := strings.TrimSpace(sc.Text())
		m := puttyHostKeyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		port, err := strconv.Atoi(m[2])
		if err != nil || port < 1 || port > 65535 {
			errs = append(errs, khLineError{Line: n, Error: "invalid port"})
			continue
		}
		host, err := normalizeHost(m[3])
		if err != nil {
			errs = append(errs, khLineError{Line: n, Error: err.Error()})
			continue
		}
		pk, err := puttyHostKey(m[1], m[4])
		if err != nil {
			errs = append(errs, khLineError{Line: n, Error: err.Error()})
			continue
		}
		out = append(out, khEntry{Line: n, Hosts: []string{knownHostName(host, port)}, Key: pk, Comment: "imported from PuTTY"})
	}
	return out, errs
}

// puttyHostKey converts PuTTY's cached host key representation (comma-separated hex numbers) to an SSH key.
func puttyHostKey(kind, value string) (ssh.PublicKey, error) {
	var nums []*big.Int
	curve := ""
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "nistp") {
			curve = part
			continue
		}
		hexs := strings.TrimPrefix(strings.TrimPrefix(part, "0x"), "0X")
		if hexs == "" || len(hexs) > 4096 {
			return nil, fmt.Errorf("invalid key value")
		}
		v, ok := new(big.Int).SetString(hexs, 16)
		if !ok {
			return nil, fmt.Errorf("invalid hex number in key value")
		}
		nums = append(nums, v)
	}
	switch kind {
	case "rsa2":
		if len(nums) != 2 || nums[0].BitLen() > 32 || nums[1].BitLen() > maxRSABits {
			return nil, fmt.Errorf("invalid RSA host key")
		}
		return ssh.NewPublicKey(&rsa.PublicKey{E: int(nums[0].Int64()), N: nums[1]})
	case "dss":
		if len(nums) != 4 {
			return nil, fmt.Errorf("invalid DSA host key")
		}
		return ssh.NewPublicKey(&dsa.PublicKey{Parameters: dsa.Parameters{P: nums[0], Q: nums[1], G: nums[2]}, Y: nums[3]})
	case "ssh-ed25519":
		if len(nums) != 2 || nums[1].BitLen() > 255 {
			return nil, fmt.Errorf("invalid Ed25519 host key")
		}
		// Encoding (RFC 8032): y little-endian with the sign (low bit) of x in the top bit.
		enc := nums[1].FillBytes(make([]byte, ed25519.PublicKeySize))
		for i, j := 0, len(enc)-1; i < j; i, j = i+1, j-1 {
			enc[i], enc[j] = enc[j], enc[i]
		}
		if nums[0].Bit(0) == 1 {
			enc[31] |= 0x80
		}
		return ssh.NewPublicKey(ed25519.PublicKey(enc))
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		c := curveByName(strings.TrimPrefix(kind, "ecdsa-sha2-"))
		if curve != "" && curve != strings.TrimPrefix(kind, "ecdsa-sha2-") {
			return nil, fmt.Errorf("curve mismatch in ECDSA host key")
		}
		if len(nums) != 2 {
			return nil, fmt.Errorf("invalid ECDSA host key")
		}
		size := (c.Params().BitSize + 7) / 8
		if nums[0].BitLen() > size*8 || nums[1].BitLen() > size*8 {
			return nil, fmt.Errorf("invalid ECDSA host key")
		}
		point := append([]byte{4}, nums[0].FillBytes(make([]byte, size))...)
		point = append(point, nums[1].FillBytes(make([]byte, size))...)
		pk, err := ecdsa.ParseUncompressedPublicKey(c, point)
		if err != nil {
			return nil, fmt.Errorf("invalid ECDSA host key: %v", err)
		}
		return ssh.NewPublicKey(pk)
	}
	return nil, fmt.Errorf("unsupported host key type %q", kind)
}
