// Package config loads Termstead's runtime configuration from command-line flags and TERMSTEAD_* environment variables
// (flags win over env, env wins over defaults) and resolves the data directory (SPEC §8, RESEARCH CORE-8).
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Run modes.
const (
	ModeDesktop = "desktop"
	ModeServer  = "server"
)

// Defaults.
const (
	DefaultListen          = "127.0.0.1:7822"
	DefaultGuacd           = "127.0.0.1:4822"
	DefaultDetachedTTL     = 24 * time.Hour
	DefaultScrollbackBytes = 4 << 20
	// PortableDirName is the data directory created next to the executable in portable mode. Its presence (or the
	// PortableMarker file) next to the executable enables portable mode automatically.
	PortableDirName = "termstead-data"
	PortableMarker  = "termstead.portable"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Listen        string
	DataDir       string
	Portable      bool
	Mode          string // desktop | server
	TLSCert       string
	TLSKey        string
	TLSSelfSigned bool
	InsecureHTTP  bool
	Open          bool
	Guacd         string // host:port of guacd ("" = disabled)
	Dev           bool
	LogLevel      string

	// DetachedSessionTTL: detached runtime sessions are reaped after this long (0 = never).
	DetachedSessionTTL time.Duration
	// ScrollbackBytes is the per-session output ring buffer size.
	ScrollbackBytes int
	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers are honored.
	TrustedProxies []netip.Prefix
	// AllowedHosts are extra Host header names (lower case, without port) accepted by the DNS-rebinding guard of a
	// loopback listener — the public name of a reverse proxy on the same machine.
	AllowedHosts []string

	// Version of the running binary (set by cmd/termstead).
	Version string
}

// IsDesktop reports whether the server runs in single-user desktop mode.
func (c *Config) IsDesktop() bool { return c.Mode == ModeDesktop }

// IsServer reports whether the server runs in multi-user server mode.
func (c *Config) IsServer() bool { return c.Mode == ModeServer }

// TLSEnabled reports whether HTTPS is served.
func (c *Config) TLSEnabled() bool { return c.TLSSelfSigned || (c.TLSCert != "" && c.TLSKey != "") }

// Path joins elem onto the data directory.
func (c *Config) Path(elem ...string) string {
	return filepath.Join(append([]string{c.DataDir}, elem...)...)
}

// DBPath is the SQLite database file.
func (c *Config) DBPath() string { return c.Path("termstead.db") }

// RecordingsDir holds asciicast recordings and text logs.
func (c *Config) RecordingsDir() string { return c.Path("recordings") }

// LogsDir holds application logs.
func (c *Config) LogsDir() string { return c.Path("logs") }

// TmpDir holds temporary files (uploads in flight, archives).
func (c *Config) TmpDir() string { return c.Path("tmp") }

// ListenIsLoopback reports whether the listen address only binds loopback interfaces.
func (c *Config) ListenIsLoopback() bool { return IsLoopbackListen(c.Listen) }

// IsLoopbackListen reports whether addr (host:port) binds loopback only. An empty host (all interfaces) is not
// loopback; "localhost" is.
func IsLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// FlagSet builds the flag set used by Load. Defaults are taken from TERMSTEAD_* environment variables when set.
// The returned apply function must be called after parsing to produce the Config.
func FlagSet(name string, output io.Writer) (*flag.FlagSet, func() (*Config, error)) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)

	listen := fs.String("listen", env("LISTEN", DefaultListen), "address to listen on (host:port) [TERMSTEAD_LISTEN]")
	dataDir := fs.String("data-dir", env("DATA_DIR", ""), "data directory (default: per-user config dir, or ./termstead-data beside the executable in portable mode) [TERMSTEAD_DATA_DIR]")
	portable := fs.Bool("portable", envBool("PORTABLE", false), "portable mode: keep data in termstead-data/ beside the executable [TERMSTEAD_PORTABLE]")
	mode := fs.String("mode", env("MODE", ModeDesktop), "run mode: desktop | server [TERMSTEAD_MODE]")
	tlsCert := fs.String("tls-cert", env("TLS_CERT", ""), "TLS certificate file (PEM) [TERMSTEAD_TLS_CERT]")
	tlsKey := fs.String("tls-key", env("TLS_KEY", ""), "TLS private key file (PEM) [TERMSTEAD_TLS_KEY]")
	selfSigned := fs.Bool("tls-self-signed", envBool("TLS_SELF_SIGNED", false), "serve HTTPS with a generated self-signed certificate [TERMSTEAD_TLS_SELF_SIGNED]")
	insecure := fs.Bool("insecure-http", envBool("INSECURE_HTTP", false), "allow plain HTTP on a non-loopback address (NOT recommended) [TERMSTEAD_INSECURE_HTTP]")
	open := fs.Bool("open", envBool("OPEN", false), "open the UI in the default browser on start (default in desktop mode) [TERMSTEAD_OPEN]")
	noOpen := fs.Bool("no-open", envBool("NO_OPEN", false), "do not open the browser on start [TERMSTEAD_NO_OPEN]")
	guacd := fs.String("guacd", env("GUACD", DefaultGuacd), "guacd address host:port for the optional Guacamole RDP engine (\"\" or \"off\" disables) [TERMSTEAD_GUACD]")
	dev := fs.Bool("dev", envBool("DEV", false), "development mode: allow the Vite dev server origins (localhost:5173) [TERMSTEAD_DEV]")
	logLevel := fs.String("log-level", env("LOG_LEVEL", "info"), "log level: debug | info | warn | error [TERMSTEAD_LOG_LEVEL]")
	ttl := fs.String("detached-ttl", env("DETACHED_TTL", DefaultDetachedTTL.String()), "reap detached sessions after this duration (0 = never) [TERMSTEAD_DETACHED_TTL]")
	scrollback := fs.String("scrollback-bytes", env("SCROLLBACK_BYTES", "4MiB"), "per-session scrollback ring size (e.g. 4MiB) [TERMSTEAD_SCROLLBACK_BYTES]")
	proxies := fs.String("trusted-proxies", env("TRUSTED_PROXIES", ""), "comma-separated CIDRs/IPs of trusted reverse proxies [TERMSTEAD_TRUSTED_PROXIES]")
	allowedHosts := fs.String("allowed-hosts", env("ALLOWED_HOSTS", ""), "comma-separated extra host names accepted on a loopback listener, e.g. a same-machine reverse proxy's public name [TERMSTEAD_ALLOWED_HOSTS]")

	apply := func() (*Config, error) {
		c := &Config{
			Listen:        strings.TrimSpace(*listen),
			DataDir:       strings.TrimSpace(*dataDir),
			Portable:      *portable,
			Mode:          strings.ToLower(strings.TrimSpace(*mode)),
			TLSCert:       *tlsCert,
			TLSKey:        *tlsKey,
			TLSSelfSigned: *selfSigned,
			InsecureHTTP:  *insecure,
			Guacd:         strings.TrimSpace(*guacd),
			Dev:           *dev,
			LogLevel:      strings.ToLower(strings.TrimSpace(*logLevel)),
		}
		// --open defaults to true in desktop mode unless explicitly given; --no-open always wins.
		openSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "open" {
				openSet = true
			}
		})
		if _, ok := os.LookupEnv("TERMSTEAD_OPEN"); ok {
			openSet = true
		}
		c.Open = *open
		if !openSet {
			c.Open = c.Mode == ModeDesktop
		}
		if *noOpen {
			c.Open = false
		}
		if strings.EqualFold(c.Guacd, "off") || strings.EqualFold(c.Guacd, "none") {
			c.Guacd = ""
		}
		d, err := parseDuration(*ttl)
		if err != nil {
			return nil, fmt.Errorf("--detached-ttl: %w", err)
		}
		c.DetachedSessionTTL = d
		n, err := ParseSize(*scrollback)
		if err != nil {
			return nil, fmt.Errorf("--scrollback-bytes: %w", err)
		}
		c.ScrollbackBytes = int(n)
		if c.TrustedProxies, err = ParsePrefixes(*proxies); err != nil {
			return nil, fmt.Errorf("--trusted-proxies: %w", err)
		}
		if c.AllowedHosts, err = ParseHosts(*allowedHosts); err != nil {
			return nil, fmt.Errorf("--allowed-hosts: %w", err)
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if err := c.ResolveDataDir(); err != nil {
			return nil, err
		}
		return c, nil
	}
	return fs, apply
}

// Load parses args (without the program name) and resolves + creates the data directory.
func Load(args []string) (*Config, error) {
	fs, apply := FlagSet("termstead", io.Discard)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return apply()
}

// Validate checks option consistency (it does not touch the filesystem).
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeDesktop, ModeServer:
	default:
		return fmt.Errorf("--mode must be desktop or server, got %q", c.Mode)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("--listen %q: %w", c.Listen, err)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("--tls-cert and --tls-key must be given together")
	}
	if c.TLSSelfSigned && c.TLSCert != "" {
		return errors.New("--tls-self-signed cannot be combined with --tls-cert/--tls-key")
	}
	if !c.ListenIsLoopback() && !c.TLSEnabled() && !c.InsecureHTTP {
		return fmt.Errorf("listening on non-loopback address %q requires TLS (--tls-cert/--tls-key or --tls-self-signed) or --insecure-http", c.Listen)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("--log-level must be debug, info, warn or error, got %q", c.LogLevel)
	}
	if c.ScrollbackBytes < 64<<10 || c.ScrollbackBytes > 1<<30 {
		return fmt.Errorf("--scrollback-bytes must be between 64KiB and 1GiB")
	}
	if c.DetachedSessionTTL < 0 {
		return errors.New("--detached-ttl must not be negative")
	}
	return nil
}

// ResolveDataDir determines the data directory (flag → portable flag/marker → os.UserConfigDir) and creates it with
// its subdirectories (0700).
func (c *Config) ResolveDataDir() error {
	dir, portable, err := ResolveDataDir(c.DataDir, c.Portable)
	if err != nil {
		return err
	}
	c.DataDir, c.Portable = dir, portable
	return EnsureDataDir(dir)
}

// ResolveDataDir returns the absolute data directory for the given flag value and portable flag.
func ResolveDataDir(flagDir string, portable bool) (dir string, isPortable bool, err error) {
	if flagDir != "" {
		abs, err := filepath.Abs(flagDir)
		return abs, portable, err
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		exeDir = filepath.Dir(exe)
	}
	if exeDir != "" {
		if portable {
			return filepath.Join(exeDir, PortableDirName), true, nil
		}
		if fi, err := os.Stat(filepath.Join(exeDir, PortableMarker)); err == nil && !fi.IsDir() {
			return filepath.Join(exeDir, PortableDirName), true, nil
		}
		if fi, err := os.Stat(filepath.Join(exeDir, PortableDirName)); err == nil && fi.IsDir() {
			return filepath.Join(exeDir, PortableDirName), true, nil
		}
	} else if portable {
		abs, err := filepath.Abs(PortableDirName)
		return abs, true, err
	}
	base, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", false, fmt.Errorf("cannot determine a data directory (use --data-dir): %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "termstead"), false, nil
}

// EnsureDataDir creates dir and its standard subdirectories with mode 0700.
func EnsureDataDir(dir string) error {
	if strings.ContainsRune(dir, '?') {
		return fmt.Errorf("data directory %q must not contain '?'", dir)
	}
	for _, d := range []string{dir, filepath.Join(dir, "recordings"), filepath.Join(dir, "logs"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create data directory: %w", err)
		}
	}
	// Tighten permissions of a pre-existing root (best effort; no-op on Windows).
	_ = os.Chmod(dir, 0o700)
	return nil
}

// ParseSize parses byte sizes such as "4194304", "4MiB", "4M", "512k", "1GB" (binary multiples).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := float64(1)
	switch unit {
	case "", "b":
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	v := f * mult
	if v > 1<<40 {
		return 0, fmt.Errorf("size %q too large", s)
	}
	return int64(v), nil
}

// ParsePrefixes parses a comma/space separated list of CIDRs or bare IPs.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, err
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// ParseHosts parses a comma/space separated list of host names or IP addresses as they appear in a Host header. An
// optional port is dropped (the guard compares names only); entries are lower-cased and de-duplicated.
func ParseHosts(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		h := strings.ToLower(f)
		if strings.Contains(h, "://") || strings.ContainsAny(h, "/*@") {
			return nil, fmt.Errorf("%q: give a bare host name such as termstead.example.com (no scheme, path or wildcard)", f)
		}
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
		if !validHostName(h) {
			return nil, fmt.Errorf("invalid host name %q", f)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out, nil
}

// validHostName accepts DNS names (letters, digits, '-', '_', '.') and IP literals.
func validHostName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil { // bare number = seconds
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv("TERMSTEAD_" + key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv("TERMSTEAD_" + key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}
