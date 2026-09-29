package monitor

import (
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Per-OS helper scripts (MON-2), embedded with go:embed. POSIX scripts run as `/bin/sh -s` with the script on stdin, so
// they never depend on the remote user's login shell (bash, zsh, fish, csh...); PowerShell scripts run with
// -EncodedCommand.
//
//go:embed scripts/*.sh scripts/*.ps1
var scriptFS embed.FS

// Embedded script names.
const (
	scriptProbe           = "probe.sh"
	scriptProbePS         = "probe.ps1"
	scriptLinuxLoop       = "linux_loop.sh"
	scriptDarwinLoop      = "darwin_loop.sh"
	scriptBSDLoop         = "bsd_loop.sh"
	scriptWindowsLoop     = "windows_loop.ps1"
	scriptLinuxProcs      = "linux_procs.sh"
	scriptUnixProcs       = "unix_procs.sh"
	scriptWindowsProcs    = "windows_procs.ps1"
	scriptLinuxPorts      = "linux_ports.sh"
	scriptUnixPorts       = "unix_ports.sh"
	scriptWindowsPorts    = "windows_ports.ps1"
	scriptSystemdServices = "systemd_services.sh"
	scriptWindowsServices = "windows_services.ps1"
)

func script(name string) string {
	b, err := scriptFS.ReadFile("scripts/" + name)
	if err != nil {
		panic("monitor: missing embedded script " + name)
	}
	return string(b)
}

// Platforms reported by the probe.
const (
	platLinux   = "linux"
	platDarwin  = "darwin"
	platFreeBSD = "freebsd"
	platOpenBSD = "openbsd"
	platNetBSD  = "netbsd"
	platDragon  = "dragonfly"
	platWindows = "windows"
)

func isBSD(p string) bool {
	return p == platFreeBSD || p == platOpenBSD || p == platNetBSD || p == platDragon
}

// loopCommand renders the sampler of a platform: one sample every interval, count samples (0 = forever).
func loopCommand(platform string, interval time.Duration, count int) (command, error) {
	sec := max(int(interval.Round(time.Second)/time.Second), 1)
	count = max(count, 0)
	repl := strings.NewReplacer("__INTERVAL__", strconv.Itoa(sec), "__COUNT__", strconv.Itoa(count),
		"__SLEEP__", strconv.Itoa(max(sec-1, 1)))
	switch {
	case platform == platLinux:
		return command{sh: repl.Replace(script(scriptLinuxLoop))}, nil
	case platform == platDarwin:
		return command{sh: repl.Replace(script(scriptDarwinLoop))}, nil
	case isBSD(platform):
		return command{sh: repl.Replace(script(scriptBSDLoop))}, nil
	case platform == platWindows:
		return command{ps: repl.Replace(script(scriptWindowsLoop))}, nil
	}
	return command{}, fmt.Errorf("monitoring is not supported on %s hosts", platform)
}

// ---- quoting --------------------------------------------------------------------------------------------------------

// bareSafe reports whether s needs no quoting in any common shell (a leading "=" is special in zsh).
func bareSafe(s string) bool {
	if s == "" || s[0] == '=' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-./:@=+,", r):
		default:
			return false
		}
	}
	return true
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	if bareSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shJoin renders argv as a POSIX shell command line.
func shJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shQuote(a)
	}
	return strings.Join(parts, " ")
}

// psQuote renders s as a PowerShell single-quoted string literal (PowerShell also treats the typographic single
// quotes as quote characters, so they are doubled as well).
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '‘', '’', '‚', '‛':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// psEncode minifies a PowerShell script (comment and blank lines dropped) and encodes it for -EncodedCommand
// (base64 of UTF-16LE).
func psEncode(src string) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lines = append(lines, t)
	}
	u := utf16.Encode([]rune(strings.Join(lines, "\n")))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// powershellLine is the remote command line that runs a PowerShell script (works from cmd.exe and PowerShell as the
// OpenSSH default shell).
func powershellLine(src string) string {
	return "powershell -NoProfile -NonInteractive -EncodedCommand " + psEncode(src)
}
