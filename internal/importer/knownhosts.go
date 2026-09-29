package importer

import (
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// OpenSSH known_hosts importer (SSH-19). Plain and "[host]:port" entries become trusted host keys, and so do PuTTY-style
// cache lines ("ssh-ed25519@22:host=0x…" — MobaXterm [SSH_Hostkeys], ~/.putty/sshhostkeys). Hashed (|1|),
// wildcard/negated patterns and @cert-authority/@revoked markers are reported (not imported) — CA/revoked markers are
// managed by the Keys → Known Hosts screen, and hashed names cannot be reversed.

const maxKnownHostLines = 50000

func parseKnownHostsSource(content []byte) (*parsed, error) {
	b := newBuilder(fmtKnownHosts)
	text := decodeMaybeCP1252(content)
	n := 0
	hashed, patterns, markers, invalid, puttyBad := 0, 0, 0, 0, 0
	stopped := false
	forEachLine(text, func(raw string) {
		if stopped {
			return
		}
		n++
		if n > maxKnownHostLines {
			b.warn("Stopped after %d lines", maxKnownHostLines)
			stopped = true
			return
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			return
		}
		fields := strings.Fields(line)
		if strings.HasPrefix(fields[0], "@") {
			markers++
			return
		}
		if kh, ok, err := parsePuttyHostKeyLine(line); ok {
			if err != nil {
				puttyBad++
				return
			}
			b.knownHost(kh)
			return
		}
		if len(fields) < 3 {
			invalid++
			return
		}
		hostField := fields[0]
		if strings.HasPrefix(hostField, "|1|") {
			hashed++
			return
		}
		key, err := sshx.ParseKnownHostKey(fields[1] + " " + fields[2])
		if err != nil {
			invalid++
			return
		}
		comment := truncate(strings.TrimSpace(strings.Join(fields[3:], " ")), 200)
		for _, h := range strings.Split(hostField, ",") {
			host, port, ok := splitKnownHostName(h)
			if !ok {
				patterns++
				continue
			}
			b.knownHost(&pknownHost{
				host:        host,
				port:        port,
				keyType:     key.Type(),
				publicKey:   sshx.FormatKnownHostKey(key),
				fingerprint: ssh.FingerprintSHA256(key),
				comment:     comment,
			})
		}
	})
	if hashed > 0 {
		b.warn("%d hashed host name(s) were skipped (they cannot be reversed for display)", hashed)
	}
	if patterns > 0 {
		b.warn("%d wildcard host pattern(s) were skipped", patterns)
	}
	if markers > 0 {
		b.warn("%d @cert-authority/@revoked marker(s) were skipped (manage those in Keys → Known Hosts)", markers)
	}
	if invalid > 0 {
		b.warn("%d unreadable line(s) were skipped", invalid)
	}
	if puttyBad > 0 {
		b.warn("%d PuTTY-style host key(s) could not be converted (unsupported type or malformed)", puttyBad)
	}
	if len(b.p.knownHosts) == 0 {
		return nil, badRequest("no importable host keys found (only hashed/pattern/marker entries?)")
	}
	return b.finish()
}

// splitKnownHostName parses "host" or "[host]:port"; ok is false for wildcard/negated patterns or invalid names.
func splitKnownHostName(h string) (string, int, bool) {
	h = strings.TrimSpace(h)
	if h == "" || strings.ContainsAny(h, "*?!") || strings.HasPrefix(h, "|") {
		return "", 0, false
	}
	host, port := h, 22
	if strings.HasPrefix(h, "[") {
		end := strings.Index(h, "]")
		if end < 0 {
			return "", 0, false
		}
		host = h[1:end]
		rest := h[end+1:]
		if strings.HasPrefix(rest, ":") {
			p, err := strconv.Atoi(rest[1:])
			if err != nil || p < 1 || p > 65535 {
				return "", 0, false
			}
			port = p
		}
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t/\\@\"'") {
		return "", 0, false
	}
	return host, port, true
}
