package importer

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// badRequest is a thin alias so parsers can return typed 400s without importing httpx everywhere.
func badRequest(msg string) error { return httpx.BadRequest(msg) }

// parseSource parses content in the given format (or auto-detects when format is "auto"/""), returning the normalized
// model. opts carry per-format hints (CSV mapping, decryption passphrase, legacy charset).
func parseSource(format string, content []byte, opts previewOptions) (*parsed, error) {
	f := strings.ToLower(strings.TrimSpace(format))
	// An explicit legacy charset converts text formats to UTF-8 up front (XML declares its own encoding; JSON is UTF-8).
	if cs := normCharset(opts.Charset); cs != "" && !isXMLFormat(f) && f != fmtJSON {
		s, err := decodeWithCharset(content, cs)
		if err != nil {
			return nil, err
		}
		content = []byte(s)
	}
	if f == "" || f == fmtAuto {
		f = detectFormat(content)
		if f == "" {
			return nil, badRequest("could not detect the file format; choose one explicitly")
		}
	}
	switch f {
	case fmtMobaXterm:
		return parseMobaXterm(content)
	case fmtPuttyReg:
		return parsePuttyReg(content)
	case fmtSSHConfig:
		return parseSSHConfig(content)
	case fmtTermiusCSV:
		return parseTermiusCSV(content)
	case fmtMRemoteNG:
		return parseMRemoteNG(content)
	case fmtRemmina:
		return parseRemmina(content)
	case fmtFileZilla:
		return parseFileZilla(content)
	case fmtWinSCP:
		return parseWinSCP(content)
	case fmtSecureCRT:
		return parseSecureCRT(content)
	case fmtCSV:
		return parseGenericCSV(content, opts)
	case fmtJSON:
		return parseAstraTermJSON(content, opts)
	case fmtKnownHosts:
		return parseKnownHostsSource(content)
	default:
		return nil, badRequest("unknown import format " + truncate(f, 40))
	}
}

func isXMLFormat(f string) bool {
	return f == fmtMRemoteNG || f == fmtFileZilla || f == fmtSecureCRT
}

var (
	// A MobaXterm bookmark line: "Name=#<icon>#<type>%…", optionally with the ";  logout" prefix MobaXterm writes when
	// "Display reconnection message" is off (or a single space).
	reMobaSession = regexp.MustCompile(`(?m)^[^=\r\n\[]+=[^#\r\n]{0,16}#\d+#\d+%`)
	reMobaHeader  = regexp.MustCompile(`(?im)^\s*\[Bookmarks(_\d+)?\]\s*$`)
	rePuttyReg    = regexp.MustCompile(`(?i)(SimonTatham\\PuTTY\\(Sessions|SshHostKeys)|9bis\.com\\KiTTY\\Sessions)`)
	reRegHeader   = regexp.MustCompile(`(?i)^\s*(Windows Registry Editor Version|REGEDIT4)`)
	reSSHConfig   = regexp.MustCompile(`(?im)^\s*Host(Name)?(\s+|\s*=\s*)\S`)
	reWinSCP      = regexp.MustCompile(`(?im)^\s*\[Sessions\\`)
)

// detectFormat guesses the import format from the raw bytes (best effort). Returns "" when unsure.
func detectFormat(content []byte) string {
	s := decodeMaybeCP1252(content) // normalise UTF-16 / CP1252 before sniffing
	trimmed := strings.TrimSpace(s)

	// JSON (AstraTerm export): starts with '{' and mentions our marker.
	if strings.HasPrefix(trimmed, "{") {
		if looksLikeAstraTermJSON(trimmed) {
			return fmtJSON
		}
		return ""
	}
	// XML-based formats.
	if strings.HasPrefix(trimmed, "<") {
		low := strings.ToLower(trimmed)
		switch {
		case strings.Contains(low, "<filezilla3"):
			return fmtFileZilla
		case strings.Contains(low, "mremoteng") || strings.Contains(low, "<connections") && strings.Contains(low, "<node"):
			return fmtMRemoteNG
		case strings.Contains(low, "<vandyke") || strings.Contains(low, `name="sessions"`):
			return fmtSecureCRT
		}
		return ""
	}
	// MobaXterm: session lines "Name=#109#0%..." (very distinctive), or a [Bookmarks] section (MobaXterm.ini).
	if reMobaSession.MatchString(s) || reMobaHeader.MatchString(s) {
		return fmtMobaXterm
	}
	// PuTTY / KiTTY registry export.
	if reRegHeader.MatchString(s) || rePuttyReg.MatchString(s) {
		return fmtPuttyReg
	}
	// WinSCP.ini.
	if reWinSCP.MatchString(s) {
		return fmtWinSCP
	}
	// Remmina connection INI.
	if strings.Contains(strings.ToLower(s), "[remmina]") {
		return fmtRemmina
	}
	// known_hosts: "@cert-authority", "|1|" hashed, or "host ssh-…" lines, or PuTTY "type@port:host".
	if looksLikeKnownHosts(s) {
		return fmtKnownHosts
	}
	// CSV with a recognisable header (any common delimiter).
	if h := firstNonEmptyLine(s); h != "" {
		if d := sniffDelimiter(h); d != 0 {
			cols := map[string]bool{}
			for c := range strings.SplitSeq(h, string(d)) {
				cols[mapHeader(strings.Trim(strings.TrimSpace(c), `"`), nil)] = true
			}
			if cols["host"] {
				low := strings.ToLower(h)
				if strings.Contains(low, "hostname/ip") && strings.Contains(low, "label") {
					return fmtTermiusCSV
				}
				return fmtCSV
			}
		}
	}
	// ssh_config: Host / HostName directives.
	if reSSHConfig.MatchString(s) {
		return fmtSSHConfig
	}
	return ""
}

// sniffDelimiter returns the most frequent CSV delimiter of a header line (',' ';' '\t' '|'), or 0.
func sniffDelimiter(line string) rune {
	best, bestN := rune(0), 0
	for _, d := range []rune{',', ';', '\t', '|'} {
		if n := strings.Count(line, string(d)); n > bestN {
			best, bestN = d, n
		}
	}
	return best
}

func firstNonEmptyLine(s string) string {
	for ln := range strings.SplitSeq(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			return ln
		}
	}
	return ""
}

// looksLikeAstraTermJSON reports whether trimmed JSON is a AstraTerm export (plain or encrypted envelope).
func looksLikeAstraTermJSON(trimmed string) bool {
	if isEncryptedEnvelope([]byte(trimmed)) {
		return true
	}
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err == nil && probe.Format == jsonFormatMarker {
		return true
	}
	// Also accept an export that at least has folders/connections arrays.
	return strings.Contains(trimmed, `"connections"`) && (strings.Contains(trimmed, `"folders"`) || strings.Contains(trimmed, `"protocol"`))
}

// looksLikeKnownHosts reports whether s looks like an OpenSSH known_hosts or a PuTTY host-key cache.
func looksLikeKnownHosts(s string) bool {
	for ln := range strings.SplitSeq(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if strings.HasPrefix(ln, "@cert-authority ") || strings.HasPrefix(ln, "@revoked ") || strings.HasPrefix(ln, "|1|") {
			return true
		}
		if reKnownHostSSHKey.MatchString(ln) || rePuttyHostKeyLine.MatchString(ln) {
			return true
		}
		return false // first meaningful line settles it
	}
	return false
}

var reKnownHostSSHKey = regexp.MustCompile(`(?i)\s(ssh-ed25519|ssh-rsa|ssh-dss|ecdsa-sha2-nistp\d+|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)\s+[A-Za-z0-9+/=]+`)
