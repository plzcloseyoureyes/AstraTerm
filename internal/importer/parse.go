package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nexterm/nexterm/internal/model"
)

// maxImportItems caps how many connections/folders a single import may contain (guards pathological inputs).
const maxImportItems = 20000

// Field limits, mirroring the connections / folders API (internal/server validateConnection): the importer writes
// through the store directly, so it enforces them itself.
const (
	maxNameRunes    = 200
	maxHostRunes    = 255
	maxUserRunes    = 255
	maxNotesBytes   = 64 << 10
	maxColorRunes   = 64
	maxIconBytes    = 64 << 10
	maxOptionsBytes = 256 << 10
	maxSecretBytes  = 64 << 10
	maxTags         = 64
)

var secretNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// builder accumulates the normalized model while a parser walks a source, assigning deterministic temp IDs.
type builder struct {
	p          parsed
	folderByID map[string]*pfolder
	folderIdx  map[string]string // parent temp id + "\x00" + lower(name) → temp id
	overflow   bool
}

func newBuilder(format string) *builder {
	return &builder{p: parsed{format: format}, folderByID: map[string]*pfolder{}, folderIdx: map[string]string{}}
}

// folder adds a folder with the given parent temp id ("" = top level) and returns its temp id. Folders with the same
// (parent, name) are merged so a path is only created once.
func (b *builder) folder(parentID, name, icon, color string) string {
	name = truncate(strings.TrimSpace(stripControl(name)), maxNameRunes)
	if name == "" {
		return parentID
	}
	key := parentID + "\x00" + strings.ToLower(name)
	if id, ok := b.folderIdx[key]; ok {
		return id
	}
	if len(b.p.folders) >= maxImportItems {
		b.overflow = true
		return parentID
	}
	id := "f" + strconv.Itoa(len(b.p.folders))
	f := &pfolder{tempID: id, parentID: parentID, name: name, icon: cleanIcon(icon), color: cleanColor(color)}
	b.p.folders = append(b.p.folders, f)
	b.folderByID[id] = f
	b.folderIdx[key] = id
	return id
}

// folderPath creates a nested folder path ("A", "B", …) under parentID and returns the leaf temp id.
func (b *builder) folderPath(parentID string, parts ...string) string {
	cur := parentID
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cur = b.folder(cur, part, "", "")
	}
	return cur
}

// conn appends a connection and returns it for further field-setting. An entry that cannot be stored (invalid
// protocol / host) is reported and returned detached (not part of the result), so callers need no special casing.
func (b *builder) conn(folderID string, c model.Connection) *pconn {
	if len(b.p.conns) >= maxImportItems {
		b.overflow = true
		return &pconn{conn: c}
	}
	if err := sanitizeConn(&c); err != nil {
		b.p.unsupported++
		b.warn("Skipped %q: %v", truncate(c.Name, 80), err)
		return &pconn{conn: c}
	}
	pc := &pconn{tempID: "c" + strconv.Itoa(len(b.p.conns)), folderID: folderID, conn: c}
	b.p.conns = append(b.p.conns, pc)
	return pc
}

// key appends key material to import and returns its temp id.
func (b *builder) key(name string, text []byte, path string) string {
	id := "k" + strconv.Itoa(len(b.p.keys))
	b.p.keys = append(b.p.keys, &pkey{tempID: id, name: name, text: text, path: path})
	return id
}

// knownHost appends a trusted host key (deduplicated within the import).
func (b *builder) knownHost(kh *pknownHost) {
	for _, e := range b.p.knownHosts {
		if e.host == kh.host && e.port == kh.port && e.keyType == kh.keyType && e.publicKey == kh.publicKey {
			return
		}
	}
	if len(b.p.knownHosts) >= maxImportItems {
		b.overflow = true
		return
	}
	b.p.knownHosts = append(b.p.knownHosts, kh)
}

func (b *builder) warn(format string, args ...any) {
	if len(b.p.warnings) >= 500 {
		return
	}
	b.p.warnings = append(b.p.warnings, fmt.Sprintf(format, args...))
}

// finish returns the accumulated result, rejecting imports over the item cap.
func (b *builder) finish() (*parsed, error) {
	if b.overflow {
		return nil, badRequest(fmt.Sprintf("import is too large (at most %d items)", maxImportItems))
	}
	return &b.p, nil
}

// ---- normalization helpers ---------------------------------------------------------------------------------------

// sanitizeConn normalizes an imported connection the way the connections API validates one. It returns an error for
// an entry that cannot be stored (the caller skips it with a warning).
func sanitizeConn(c *model.Connection) error {
	c.Protocol = model.Protocol(strings.ToLower(strings.TrimSpace(string(c.Protocol))))
	if !model.ValidProtocol(c.Protocol) {
		return fmt.Errorf("unsupported protocol %q", truncate(string(c.Protocol), 32))
	}
	c.Host = strings.TrimSpace(c.Host)
	if strings.IndexFunc(c.Host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("the host name contains spaces or control characters")
	}
	if utf8.RuneCountInString(c.Host) > maxHostRunes {
		return errors.New("the host name is too long")
	}
	c.Name = cleanName(stripControl(c.Name), c.Host)
	c.Username = truncate(strings.TrimSpace(stripControl(c.Username)), maxUserRunes)
	c.Notes = truncateBytes(c.Notes, maxNotesBytes)
	c.Color = cleanColor(c.Color)
	c.Icon = cleanIcon(c.Icon)
	if c.Port < 0 || c.Port > 65535 {
		c.Port = 0
	}
	if c.Port == 0 {
		c.Port = model.DefaultPort(c.Protocol)
	}
	if !model.ValidAuthMethod(c.AuthMethod) {
		c.AuthMethod = model.AuthAuto
	}
	c.Tags = cleanTags(c.Tags)
	if c.Options == nil {
		c.Options = model.Options{}
	}
	if b, err := json.Marshal(c.Options); err != nil || len(b) > maxOptionsBytes {
		return errors.New("its options are invalid or too large")
	}
	c.Normalize()
	return nil
}

// cleanSecrets keeps well-formed, bounded secret entries (names like the API accepts).
func cleanSecrets(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v == "" || !secretNameRE.MatchString(k) || len(v) > maxSecretBytes {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cleanColor keeps a short CSS colour value ("#rrggbb" from importers, anything ≤ 64 runes from a NexTerm export).
func cleanColor(s string) string {
	s = strings.TrimSpace(stripControl(s))
	if utf8.RuneCountInString(s) > maxColorRunes {
		return ""
	}
	return s
}

// cleanIcon keeps the icon forms the UI renders (`lucide:<Name>` or a base64 data:image URL) within the API limit.
func cleanIcon(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxIconBytes {
		return ""
	}
	if strings.HasPrefix(s, "lucide:") || strings.HasPrefix(s, "data:image/") {
		return s
	}
	return ""
}

// stripControl removes control characters (newlines, tabs, NUL…) from a single-line value.
func stripControl(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// clampPort returns a valid port or the protocol default when out of range / zero.
func clampPort(port int, proto model.Protocol) int {
	if port < 0 || port > 65535 {
		port = 0
	}
	if port == 0 {
		return model.DefaultPort(proto)
	}
	return port
}

// truncate limits a string to n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// truncateBytes limits s to at most n bytes without splitting a UTF-8 sequence.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// cleanName trims and bounds a connection/folder name, falling back to alt when empty.
func cleanName(name, alt string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(alt)
	}
	if name == "" {
		name = "Imported session"
	}
	return truncate(name, maxNameRunes)
}

// authMethodFor derives a sensible authMethod from what the source provided.
func authMethodFor(hasKey, hasPassword bool) string {
	switch {
	case hasKey:
		return model.AuthKey
	case hasPassword:
		return model.AuthPassword
	default:
		return model.AuthAuto
	}
}

// atoiSafe parses an int, returning def on failure.
func atoiSafe(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// splitList splits s on any of the separator runes into trimmed non-empty tokens.
func splitList(s string, seps string) []string {
	f := func(r rune) bool { return strings.ContainsRune(seps, r) }
	out := []string{}
	for _, t := range strings.FieldsFunc(s, f) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// cleanTags de-duplicates and bounds imported tags.
func cleanTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(stripControl(t))
		if t == "" || seen[strings.ToLower(t)] || utf8.RuneCountInString(t) > 64 {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// dupKey is the case-insensitive identity used to detect duplicate connections.
func dupKey(name string, proto model.Protocol, host string, port int, user string) string {
	if port == 0 {
		port = model.DefaultPort(proto)
	}
	return strings.ToLower(strings.TrimSpace(name)) + "\x00" + string(proto) + "\x00" +
		strings.ToLower(strings.TrimSpace(host)) + "\x00" + strconv.Itoa(port) + "\x00" +
		strings.ToLower(strings.TrimSpace(user))
}

// sortedSecretKeys returns the secret key names of m, sorted.
func sortedSecretKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// isSSHFamily reports whether p reaches its host over SSH itself (jump chains apply; others use a gateway).
func isSSHFamily(p model.Protocol) bool {
	return p == model.ProtoSSH || p == model.ProtoSFTP || p == model.ProtoMosh
}

// runsLocalCommand reports whether opening c executes a program on the NexTerm host (ProxyCommand or a local shell).
func runsLocalCommand(c *model.Connection) bool {
	if c.Protocol == model.ProtoLocal || c.Protocol == model.ProtoKube {
		return true
	}
	return strings.TrimSpace(c.Options.String("proxyCommand", "")) != ""
}
