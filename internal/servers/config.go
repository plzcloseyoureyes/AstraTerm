package servers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/auth"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Kind identifies one embedded server.
type Kind string

// Server kinds (SPEC §6.0 "Embedded servers" plus telnet and syslog).
const (
	KindHTTP   Kind = "http"
	KindFTP    Kind = "ftp"
	KindSFTP   Kind = "sftp"
	KindTFTP   Kind = "tftp"
	KindTelnet Kind = "telnet"
	KindSyslog Kind = "syslog"
)

// Kinds lists every server in display order.
var Kinds = []Kind{KindHTTP, KindFTP, KindSFTP, KindTFTP, KindTelnet, KindSyslog}

var kindNames = map[Kind]string{
	KindHTTP:   "HTTP file server",
	KindFTP:    "FTP server",
	KindSFTP:   "SSH / SFTP server",
	KindTFTP:   "TFTP server",
	KindTelnet: "Telnet server",
	KindSyslog: "Syslog server",
}

func parseKind(s string) (Kind, bool) {
	k := Kind(strings.ToLower(strings.TrimSpace(s)))
	_, ok := kindNames[k]
	return k, ok
}

// Limits.
const (
	maxUsers        = 100
	maxPublicKeys   = 50
	maxUsernameLen  = 64
	minPasswordLen  = 4
	maxStopAfterSec = 30 * 24 * 3600
	minStopAfterSec = 10
	maxPathLen      = 4096
	maxBufferSize   = 100_000
)

// User is an account of the FTP, SSH/SFTP, Telnet or HTTP (basic auth) server. PasswordHash (argon2id) is stored but
// never returned; Password is write-only input (nil = unchanged, "" = remove).
type User struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	PasswordHash string   `json:"passwordHash,omitempty"`
	Password     *string  `json:"password,omitempty"`
	PublicKeys   []string `json:"publicKeys,omitempty"`
	ReadOnly     bool     `json:"readOnly,omitempty"`
}

// Common holds the settings every server has.
type Common struct {
	// BindAddress is an IP literal ("127.0.0.1" by default, "0.0.0.0" / "::" = every interface).
	BindAddress string `json:"bindAddress"`
	Port        int    `json:"port"`
	// AutoStart starts the server when AstraTerm starts.
	AutoStart bool `json:"autoStart"`
	// StopAfterSec stops the server automatically this long after it was started (0 = never).
	StopAfterSec int `json:"stopAfterSec"`
}

// HTTPConfig configures the HTTP file server (SRV-4).
type HTTPConfig struct {
	Common
	Root string `json:"root"`
	// Listing renders directory listings (index.html is served when present either way).
	Listing bool `json:"listing"`
	// ReadOnly forbids every modification; otherwise PUT uploads are accepted.
	ReadOnly bool `json:"readOnly"`
	// Upload adds an upload / new-folder form to listings (effective when not read-only).
	Upload bool `json:"upload"`
	// MaxUploadMB caps one upload (0 = unlimited).
	MaxUploadMB int `json:"maxUploadMB"`
	// RequireAuth enables HTTP basic authentication with Users.
	RequireAuth bool   `json:"requireAuth"`
	Users       []User `json:"users"`
	// TLS serves HTTPS with a self-signed certificate generated (and kept) by AstraTerm.
	TLS bool `json:"tls"`
}

// FTPConfig configures the FTP server (SRV-3).
type FTPConfig struct {
	Common
	Root           string `json:"root"`
	ReadOnly       bool   `json:"readOnly"`
	Anonymous      bool   `json:"anonymous"`
	AnonymousWrite bool   `json:"anonymousWrite"`
	Users          []User `json:"users"`
	// PassivePortMin/Max restrict passive data ports (0/0 = any free port).
	PassivePortMin int `json:"passivePortMin"`
	PassivePortMax int `json:"passivePortMax"`
	// PublicHost is the IPv4 address announced in PASV replies (NAT); "" = the address the client connected to.
	PublicHost string `json:"publicHost"`
	// TLS: off | optional (explicit AUTH TLS allowed) | required (explicit TLS mandatory) | implicit (FTPS, port 990 style).
	TLS            string `json:"tls"`
	IdleTimeoutSec int    `json:"idleTimeoutSec"`
}

// SFTPConfig configures the SSH / SFTP server (SRV-5).
type SFTPConfig struct {
	Common
	// Root is the SFTP jail: clients see it as "/".
	Root     string `json:"root"`
	ReadOnly bool   `json:"readOnly"`
	Users    []User `json:"users"`
	// Shell allows interactive shells and remote commands (run as the AstraTerm OS user, not jailed). Off by default.
	Shell bool `json:"shell"`
	// ShellCommand overrides the shell program ("" = the user's default shell).
	ShellCommand   string `json:"shellCommand"`
	IdleTimeoutSec int    `json:"idleTimeoutSec"`
}

// TFTPConfig configures the TFTP server (SRV-2).
type TFTPConfig struct {
	Common
	Root     string `json:"root"`
	ReadOnly bool   `json:"readOnly"`
	// BlockSize caps the negotiated block size (0 = up to the interface MTU; 512…65464).
	BlockSize  int `json:"blockSize"`
	TimeoutSec int `json:"timeoutSec"`
	Retries    int `json:"retries"`
	// SinglePort serves every transfer from the listening port (firewall / NAT friendly, slower).
	SinglePort bool `json:"singlePort"`
}

// TelnetConfig configures the Telnet server (SRV-6).
type TelnetConfig struct {
	Common
	Users []User `json:"users"`
	// ShellCommand overrides the shell program ("" = the user's default shell).
	ShellCommand string `json:"shellCommand"`
	// WorkingDir is the shell's start directory ("" = the home directory).
	WorkingDir     string `json:"workingDir"`
	IdleTimeoutSec int    `json:"idleTimeoutSec"`
}

// SyslogConfig configures the syslog receiver (CC-5).
type SyslogConfig struct {
	Common
	UDP bool `json:"udp"`
	TCP bool `json:"tcp"`
	// BufferSize is the number of messages kept in memory.
	BufferSize int `json:"bufferSize"`
	// LogToFile appends every message to daily files in LogDir ("" = <data dir>/logs/syslog).
	LogToFile     bool   `json:"logToFile"`
	LogDir        string `json:"logDir"`
	RetentionDays int    `json:"retentionDays"`
	MaxFileMB     int    `json:"maxFileMB"`
}

// config is implemented by every *XConfig.
type config interface {
	base() *Common
	users() *[]User
	validate(e *env) error
}

func (c *HTTPConfig) base() *Common   { return &c.Common }
func (c *FTPConfig) base() *Common    { return &c.Common }
func (c *SFTPConfig) base() *Common   { return &c.Common }
func (c *TFTPConfig) base() *Common   { return &c.Common }
func (c *TelnetConfig) base() *Common { return &c.Common }
func (c *SyslogConfig) base() *Common { return &c.Common }

func (c *HTTPConfig) users() *[]User   { return &c.Users }
func (c *FTPConfig) users() *[]User    { return &c.Users }
func (c *SFTPConfig) users() *[]User   { return &c.Users }
func (c *TFTPConfig) users() *[]User   { return nil }
func (c *TelnetConfig) users() *[]User { return &c.Users }
func (c *SyslogConfig) users() *[]User { return nil }

// env is the host context configs are validated against.
type env struct {
	home        string
	dataDir     string
	defaultRoot string
	// nexHost / nexPort: AstraTerm's own listener (never bindable by an embedded server).
	nexHost string
	nexPort int
}

// defaultConfig returns the factory configuration of kind.
func defaultConfig(k Kind, e *env) config {
	loop := "127.0.0.1"
	switch k {
	case KindHTTP:
		return &HTTPConfig{Common: Common{BindAddress: loop, Port: 8080}, Root: e.defaultRoot, Listing: true,
			ReadOnly: true, Upload: true, Users: []User{}}
	case KindFTP:
		return &FTPConfig{Common: Common{BindAddress: loop, Port: 2121}, Root: e.defaultRoot, Users: []User{},
			TLS: "optional", IdleTimeoutSec: 900}
	case KindSFTP:
		return &SFTPConfig{Common: Common{BindAddress: loop, Port: 2222}, Root: e.defaultRoot, Users: []User{},
			IdleTimeoutSec: 1800}
	case KindTFTP:
		return &TFTPConfig{Common: Common{BindAddress: loop, Port: 69}, Root: e.defaultRoot, ReadOnly: true,
			TimeoutSec: 5, Retries: 5}
	case KindTelnet:
		return &TelnetConfig{Common: Common{BindAddress: loop, Port: 2323}, Users: []User{}, IdleTimeoutSec: 3600}
	case KindSyslog:
		return &SyslogConfig{Common: Common{BindAddress: loop, Port: 514}, UDP: true, TCP: true, BufferSize: 10_000,
			RetentionDays: 14, MaxFileMB: 256}
	}
	panic("servers: unknown kind " + string(k))
}

// cloneConfig deep-copies c (through JSON, which is exact for these plain structs).
func cloneConfig(k Kind, c config, e *env) config {
	out := defaultConfig(k, e)
	if b, err := json.Marshal(c); err == nil {
		_ = json.Unmarshal(b, out)
	}
	return out
}

// publicConfig renders c for the API: password hashes are replaced by hasPassword.
func publicConfig(c config) map[string]any {
	b, err := json.Marshal(c)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	if list, ok := m["users"].([]any); ok {
		for _, u := range list {
			um, ok := u.(map[string]any)
			if !ok {
				continue
			}
			h, _ := um["passwordHash"].(string)
			delete(um, "passwordHash")
			delete(um, "password")
			um["hasPassword"] = h != ""
			if _, ok := um["publicKeys"]; !ok {
				um["publicKeys"] = []any{}
			}
		}
	}
	return m
}

// ---- validation ---------------------------------------------------------------------------------------------------

// errInvalid marks a configuration error (HTTP 400).
type errInvalid struct{ msg string }

func (e *errInvalid) Error() string { return e.msg }

func invalidf(format string, a ...any) error { return &errInvalid{msg: fmt.Sprintf(format, a...)} }

func isInvalid(err error) bool {
	var e *errInvalid
	return errors.As(err, &e)
}

// normalizeBind validates and canonicalizes a bind address ("" and "localhost" → 127.0.0.1, "*" → 0.0.0.0).
func normalizeBind(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "localhost":
		return "127.0.0.1", nil
	case "*":
		return "0.0.0.0", nil
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", invalidf("bind address %q is not an IP address", s)
	}
	return a.Unmap().String(), nil
}

// isUnspecified reports whether host binds every interface.
func isUnspecified(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsUnspecified()
}

// isLoopback reports whether host binds loopback only.
func isLoopback(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// overlaps reports whether two bind hosts can conflict (same address, or either binds every interface).
func overlaps(a, b string) bool {
	return a == b || isUnspecified(a) || isUnspecified(b)
}

func (c *Common) validate(e *env, tcp bool) error {
	host, err := normalizeBind(c.BindAddress)
	if err != nil {
		return err
	}
	c.BindAddress = host
	if c.Port < 1 || c.Port > 65535 {
		return invalidf("port must be between 1 and 65535")
	}
	if tcp && e.nexPort != 0 && c.Port == e.nexPort && overlaps(host, e.nexHost) {
		return invalidf("port %d is AstraTerm's own port", c.Port)
	}
	if c.StopAfterSec < 0 || c.StopAfterSec > maxStopAfterSec {
		return invalidf("stopAfterSec must be between 0 and %d", maxStopAfterSec)
	}
	if c.StopAfterSec > 0 && c.StopAfterSec < minStopAfterSec {
		c.StopAfterSec = minStopAfterSec
	}
	return nil
}

// cleanRoot validates a shared folder: an absolute path ("~" expands to the home directory).
func cleanRoot(p string, e *env, what string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", invalidf("%s is required", what)
	}
	if len(p) > maxPathLen || strings.ContainsRune(p, 0) {
		return "", invalidf("%s is not a valid path", what)
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if e.home == "" {
			return "", invalidf("%s: the home directory is unknown", what)
		}
		p = filepath.Join(e.home, p[1:])
	}
	if !filepath.IsAbs(p) {
		return "", invalidf("%s must be an absolute path", what)
	}
	return filepath.Clean(p), nil
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)

// validateUsers checks user entries. Passwords must already have been resolved into hashes (applyUserInput).
func validateUsers(list *[]User, keys, needCred bool) error {
	if *list == nil {
		*list = []User{}
	}
	if len(*list) > maxUsers {
		return invalidf("at most %d users", maxUsers)
	}
	seen := map[string]bool{}
	for i := range *list {
		u := &(*list)[i]
		u.Username = strings.TrimSpace(u.Username)
		if u.Username == "" {
			return invalidf("user %d has no name", i+1)
		}
		if len(u.Username) > maxUsernameLen || !usernameRe.MatchString(u.Username) {
			return invalidf("user name %q may only contain letters, digits and . _ @ + -", u.Username)
		}
		lower := strings.ToLower(u.Username)
		if seen[lower] {
			return invalidf("duplicate user %q", u.Username)
		}
		seen[lower] = true
		if strings.EqualFold(u.Username, "anonymous") || strings.EqualFold(u.Username, "ftp") {
			return invalidf("%q is reserved for anonymous access", u.Username)
		}
		if !model.ValidID(u.ID) {
			u.ID = model.NewID()
		}
		u.Password = nil
		if !keys {
			u.PublicKeys = nil
		} else if err := normalizeKeys(u); err != nil {
			return err
		}
		if needCred && u.PasswordHash == "" && len(u.PublicKeys) == 0 {
			if keys {
				return invalidf("user %q needs a password or a public key", u.Username)
			}
			return invalidf("user %q needs a password", u.Username)
		}
	}
	return nil
}

// normalizeKeys parses the user's authorized keys (authorized_keys lines; options are not supported).
func normalizeKeys(u *User) error {
	if len(u.PublicKeys) > maxPublicKeys {
		return invalidf("user %q: at most %d public keys", u.Username, maxPublicKeys)
	}
	out := make([]string, 0, len(u.PublicKeys))
	seen := map[string]bool{}
	for _, line := range u.PublicKeys {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return invalidf("user %q: invalid public key %q", u.Username, truncate(line, 40))
		}
		s := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
		if comment != "" {
			s += " " + comment
		}
		fp := ssh.FingerprintSHA256(pk)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, s)
	}
	u.PublicKeys = out
	return nil
}

// applyUserInput resolves write-only passwords of next against prev (matched by id, then by name): a nil password
// keeps the stored hash, "" removes it, anything else is hashed.
func applyUserInput(next, prev []User) error {
	byID := map[string]*User{}
	byName := map[string]*User{}
	for i := range prev {
		p := &prev[i]
		if p.ID != "" {
			byID[p.ID] = p
		}
		byName[strings.ToLower(p.Username)] = p
	}
	for i := range next {
		u := &next[i]
		old := byID[u.ID]
		if old == nil && u.ID == "" {
			old = byName[strings.ToLower(strings.TrimSpace(u.Username))]
			if old != nil {
				u.ID = old.ID
			}
		}
		switch {
		case u.Password == nil:
			u.PasswordHash = ""
			if old != nil {
				u.PasswordHash = old.PasswordHash
			}
		case *u.Password == "":
			u.PasswordHash = ""
		default:
			pw := *u.Password
			if utf8.RuneCountInString(pw) < minPasswordLen {
				return invalidf("password of %q must be at least %d characters", strings.TrimSpace(u.Username), minPasswordLen)
			}
			if len(pw) > auth.MaxPasswordLen || strings.ContainsRune(pw, 0) {
				return invalidf("password of %q is too long", strings.TrimSpace(u.Username))
			}
			h, err := auth.HashPassword(pw)
			if err != nil {
				return err
			}
			u.PasswordHash = h
		}
		u.Password = nil
	}
	return nil
}

func (c *HTTPConfig) validate(e *env) error {
	if err := c.Common.validate(e, true); err != nil {
		return err
	}
	root, err := cleanRoot(c.Root, e, "root folder")
	if err != nil {
		return err
	}
	c.Root = root
	if c.MaxUploadMB < 0 || c.MaxUploadMB > 1<<20 {
		return invalidf("maxUploadMB must be between 0 and %d", 1<<20)
	}
	if err := validateUsers(&c.Users, false, true); err != nil {
		return err
	}
	if c.RequireAuth && len(c.Users) == 0 {
		return invalidf("add at least one user or turn off authentication")
	}
	return nil
}

var ftpTLSModes = map[string]bool{"off": true, "optional": true, "required": true, "implicit": true}

func (c *FTPConfig) validate(e *env) error {
	if err := c.Common.validate(e, true); err != nil {
		return err
	}
	root, err := cleanRoot(c.Root, e, "root folder")
	if err != nil {
		return err
	}
	c.Root = root
	if err := validateUsers(&c.Users, false, true); err != nil {
		return err
	}
	c.TLS = strings.ToLower(strings.TrimSpace(c.TLS))
	if c.TLS == "" {
		c.TLS = "off"
	}
	if !ftpTLSModes[c.TLS] {
		return invalidf("tls must be off, optional, required or implicit")
	}
	if c.PassivePortMin != 0 || c.PassivePortMax != 0 {
		if c.PassivePortMin < 1024 || c.PassivePortMax > 65535 || c.PassivePortMin > c.PassivePortMax {
			return invalidf("the passive port range must lie within 1024-65535 (min ≤ max)")
		}
	}
	c.PublicHost = strings.TrimSpace(c.PublicHost)
	if c.PublicHost != "" {
		a, err := netip.ParseAddr(c.PublicHost)
		if err != nil || !a.Unmap().Is4() {
			return invalidf("the passive (public) address must be an IPv4 address")
		}
		c.PublicHost = a.Unmap().String()
	}
	if c.IdleTimeoutSec <= 0 {
		c.IdleTimeoutSec = 900
	}
	if c.IdleTimeoutSec > 86400 {
		c.IdleTimeoutSec = 86400
	}
	if !c.Anonymous {
		c.AnonymousWrite = false
	}
	return nil
}

func (c *SFTPConfig) validate(e *env) error {
	if err := c.Common.validate(e, true); err != nil {
		return err
	}
	root, err := cleanRoot(c.Root, e, "root folder")
	if err != nil {
		return err
	}
	c.Root = root
	if err := validateUsers(&c.Users, true, true); err != nil {
		return err
	}
	c.ShellCommand = strings.TrimSpace(c.ShellCommand)
	if len(c.ShellCommand) > maxPathLen || strings.ContainsRune(c.ShellCommand, 0) {
		return invalidf("invalid shell command")
	}
	if c.IdleTimeoutSec <= 0 {
		c.IdleTimeoutSec = 1800
	}
	if c.IdleTimeoutSec > 7*86400 {
		c.IdleTimeoutSec = 7 * 86400
	}
	return nil
}

func (c *TFTPConfig) validate(e *env) error {
	if err := c.Common.validate(e, false); err != nil {
		return err
	}
	root, err := cleanRoot(c.Root, e, "root folder")
	if err != nil {
		return err
	}
	c.Root = root
	if c.BlockSize != 0 && (c.BlockSize < 512 || c.BlockSize > 65464) {
		return invalidf("block size must be 0 (automatic) or between 512 and 65464")
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = 5
	}
	if c.TimeoutSec > 255 {
		c.TimeoutSec = 255
	}
	if c.Retries <= 0 {
		c.Retries = 5
	}
	if c.Retries > 50 {
		c.Retries = 50
	}
	return nil
}

func (c *TelnetConfig) validate(e *env) error {
	if err := c.Common.validate(e, true); err != nil {
		return err
	}
	if err := validateUsers(&c.Users, false, true); err != nil {
		return err
	}
	c.ShellCommand = strings.TrimSpace(c.ShellCommand)
	if len(c.ShellCommand) > maxPathLen || strings.ContainsRune(c.ShellCommand, 0) {
		return invalidf("invalid shell command")
	}
	c.WorkingDir = strings.TrimSpace(c.WorkingDir)
	if c.WorkingDir != "" {
		wd, err := cleanRoot(c.WorkingDir, e, "working directory")
		if err != nil {
			return err
		}
		c.WorkingDir = wd
	}
	if c.IdleTimeoutSec <= 0 {
		c.IdleTimeoutSec = 3600
	}
	if c.IdleTimeoutSec > 7*86400 {
		c.IdleTimeoutSec = 7 * 86400
	}
	return nil
}

func (c *SyslogConfig) validate(e *env) error {
	if err := c.Common.validate(e, c.TCP); err != nil {
		return err
	}
	if !c.UDP && !c.TCP {
		return invalidf("enable UDP, TCP or both")
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 10_000
	}
	if c.BufferSize < 100 {
		c.BufferSize = 100
	}
	if c.BufferSize > maxBufferSize {
		return invalidf("the buffer holds at most %d messages", maxBufferSize)
	}
	c.LogDir = strings.TrimSpace(c.LogDir)
	if c.LogDir != "" {
		dir, err := cleanRoot(c.LogDir, e, "log folder")
		if err != nil {
			return err
		}
		c.LogDir = dir
	}
	if c.RetentionDays < 0 || c.RetentionDays > 3650 {
		return invalidf("retentionDays must be between 0 (keep forever) and 3650")
	}
	if c.MaxFileMB < 0 || c.MaxFileMB > 1<<20 {
		return invalidf("maxFileMB must be between 0 (unlimited) and %d", 1<<20)
	}
	return nil
}

// ---- helpers ------------------------------------------------------------------------------------------------------

// hostPort joins a bind host and port.
func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// rootOf returns the shared folder of a file server config ("" for others).
func rootOf(c config) string {
	switch t := c.(type) {
	case *HTTPConfig:
		return t.Root
	case *FTPConfig:
		return t.Root
	case *SFTPConfig:
		return t.Root
	case *TFTPConfig:
		return t.Root
	}
	return ""
}

// checkRoot verifies (at start) that root is an existing directory that neither contains nor lies inside AstraTerm's
// data directory (its database and vault key must never be served). The default root is created when missing.
func checkRoot(root string, e *env) error {
	st, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) && filepath.Clean(root) == filepath.Clean(e.defaultRoot) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("cannot create the shared folder %s: %w", root, err)
		}
		st, err = os.Stat(root)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return invalidf("the folder %s does not exist", root)
		}
		return invalidf("cannot access %s: %v", root, rootCause(err))
	}
	if !st.IsDir() {
		return invalidf("%s is not a folder", root)
	}
	if e.dataDir != "" && pathsOverlap(root, e.dataDir) {
		return invalidf("the folder %s overlaps AstraTerm's data directory (%s); choose another folder", root, e.dataDir)
	}
	return nil
}

// pathsOverlap reports whether a contains b or b contains a (after resolving symlinks where possible).
// macOS and Windows file systems are case-insensitive by default: compare folded there, so "/USERS/me/…" cannot
// sneak around the check.
func pathsOverlap(a, b string) bool {
	ra, rb := resolvePath(a), resolvePath(b)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		ra, rb = strings.ToLower(ra), strings.ToLower(rb)
	}
	return within(ra, rb) || within(rb, ra)
}

func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// within reports whether p is base or lies below it.
func within(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// rootCause strips *PathError wrappers so messages do not repeat the path.
func rootCause(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
