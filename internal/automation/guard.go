package automation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Dangerous-command guard (SEC-21). Every API that types user-provided text into sessions (snippet runs, macro
// replays, paced sends, batch runs, schedules) refuses text matching these rules with 409 {code:'dangerous_command',
// matches} unless the request carries confirmDangerous: true. The browser runs the same rules
// (web/src/features/automation/guard.ts) to ask before sending. It is a best-effort speed bump, not a sandbox.

// Severities: "danger" rules always ask; "warning" rules only in strict mode.
const (
	sevDanger  = "danger"
	sevWarning = "warning"
)

type guardRule struct {
	id, message, severity string
	re                    *regexp.Regexp
}

// Line-oriented rules (RE2). Keep in sync with guard.ts.
var guardRules = []guardRule{
	{"rm-root", "Recursively deletes the root, home or every file (rm -r on / ~ *).", sevDanger,
		regexp.MustCompile(`(?i)\brm\s+(?:-[-\w]+\s+)*-[a-z]*r[a-z]*\s+(?:-[-\w]+\s+)*(?:/\*?|~/?|\*|\$home/?|\.\.?/?\*?)(?:\s|$|[;&|)])|\brm\b[^\n]*--no-preserve-root`)},
	{"rm-rf", "Recursively force-deletes files (rm -rf).", sevWarning,
		regexp.MustCompile(`(?i)\brm\s+(?:-[-\w]+\s+)*-(?:[a-z]*r[a-z]*f|[a-z]*f[a-z]*r)[a-z]*\b`)},
	{"mkfs", "Creates a filesystem / wipes a device (mkfs, mkswap, wipefs).", sevDanger,
		regexp.MustCompile(`(?i)(?:^|[\s;&|(])(?:mkfs(?:\.\w+)?|mke2fs|mkswap|wipefs|mkfs_msdos)\s`)},
	{"dd-dev", "Writes raw data to a device (dd of=/dev/…).", sevDanger,
		regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof=/dev/(?:sd|hd|vd|xvd|nvme|mmcblk|disk|rdisk|md|dm-|mapper|loop)`)},
	{"redirect-dev", "Overwrites a block device (> /dev/sdX).", sevDanger,
		regexp.MustCompile(`(?i)>\s*/dev/(?:sd|hd|vd|xvd|nvme|mmcblk|disk|rdisk)\w*`)},
	{"partition", "Rewrites a partition table.", sevWarning,
		regexp.MustCompile(`(?i)(?:^|[\s;&|(])(?:fdisk|sfdisk|gdisk|sgdisk|parted)\s+(?:-[-\w]+\s+)*/dev/`)},
	{"fork-bomb", "Fork bomb.", sevDanger,
		regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)},
	{"power", "Shuts down or reboots the machine.", sevDanger,
		regexp.MustCompile(`(?i)(?:^|[;&|(]|\bsudo\s+|\bexec\s+|\bdoas\s+)\s*(?:shutdown|reboot|halt|poweroff|init\s+[06]|telinit\s+[06]|systemctl\s+(?:reboot|poweroff|halt|kexec|emergency|rescue))\b`)},
	{"chmod-root", "Recursively changes permissions / ownership from the root directory.", sevDanger,
		regexp.MustCompile(`(?i)\b(?:chmod|chown|chgrp)\s+(?:-[-\w]+\s+)*-[a-z]*R[a-z]*\s+\S+\s+/(?:\s|$)`)},
	{"kill-all", "Kills every process.", sevWarning,
		regexp.MustCompile(`(?i)\bkill\s+(?:-\S+\s+)*-1\b|\bkillall5\b|\bpkill\s+(?:-\S+\s+)*-u\s+root\b`)},
	{"firewall-flush", "Flushes / disables the firewall (may cut your own access).", sevWarning,
		regexp.MustCompile(`(?i)\b(?:iptables|ip6tables)\s+(?:-\S+\s+)*-F\b|\bnft\s+flush\s+ruleset\b|\bufw\s+disable\b`)},
	{"iface-down", "Takes a network interface down (may cut your own access).", sevWarning,
		regexp.MustCompile(`(?i)\bip\s+link\s+set\s+\S+\s+down\b|\bifconfig\s+\S+\s+down\b|\bifdown\s+\S+`)},
	{"sql-drop", "Drops a database or schema.", sevDanger,
		regexp.MustCompile(`(?i)\bdrop\s+(?:database|schema)\b`)},
	{"sql-table", "Drops or truncates a table.", sevWarning,
		regexp.MustCompile(`(?i)\bdrop\s+table\b|\btruncate\s+(?:table\s+)?\w`)},
	{"sql-delete-all", "DELETE without WHERE removes every row.", sevWarning,
		regexp.MustCompile(`(?i)\bdelete\s+from\s+[\w."\x60\[\]]+\s*(?:;|$)`)},
	{"cisco", "Reloads / erases a network device.", sevDanger,
		regexp.MustCompile(`(?i)^\s*(?:reload(?:\s+in\s+\d+)?|write\s+erase|erase\s+(?:startup-config|nvram:|flash:)|format\s+(?:flash|disk\d?|bootflash):)\s*$`)},
	{"windows-format", "Formats a drive / deletes recursively on Windows.", sevDanger,
		regexp.MustCompile(`(?i)\bformat\s+[a-z]:|\bRemove-Item\b[^\n]*-Recurse[^\n]*\s[a-z]:\\?(?:\s|$)|\brd\s+/s\s+/q\s+[a-z]:\\?(?:\s|$)`)},
	{"crontab-remove", "Removes the crontab.", sevWarning,
		regexp.MustCompile(`(?i)\bcrontab\s+(?:-u\s+\S+\s+)?-r\b`)},
	{"k8s-delete", "Deletes Kubernetes namespaces / everything.", sevWarning,
		regexp.MustCompile(`(?i)\bkubectl\s+delete\s+(?:ns|namespaces?|all)\b|\bkubectl\s+delete\b[^\n]*--all\b`)},
	{"terraform-destroy", "Destroys infrastructure.", sevWarning,
		regexp.MustCompile(`(?i)\bterraform\s+destroy\b|\bpulumi\s+destroy\b`)},
}

type customGuardRule struct {
	Pattern string `json:"pattern"`
	Message string `json:"message"`
}

// guardConfig is the server-side view of the "automation" settings section keys the guard uses.
type guardConfig struct {
	Enabled bool
	Strict  bool
	Custom  []guardRule
}

var customRuleCache sync.Map // pattern → *regexp.Regexp (nil when invalid)

func compileCustom(p string) *regexp.Regexp {
	if v, ok := customRuleCache.Load(p); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile("(?i)" + p)
	if err != nil {
		re = nil
	}
	customRuleCache.Store(p, re)
	return re
}

// loadGuardConfig reads the user's "automation" settings (user values over global ones).
func loadGuardConfig(ctx context.Context, st *store.Store, userID string) guardConfig {
	cfg := guardConfig{Enabled: true}
	if st == nil {
		return cfg
	}
	merged := map[string]json.RawMessage{}
	for _, scope := range []string{store.ScopeGlobal, userID} {
		var sec map[string]json.RawMessage
		if ok, err := st.Settings.GetJSON(ctx, scope, "automation", &sec); err == nil && ok {
			for k, v := range sec {
				merged[k] = v
			}
		}
	}
	var b bool
	if v, ok := merged["guardEnabled"]; ok && json.Unmarshal(v, &b) == nil {
		cfg.Enabled = b
	}
	if v, ok := merged["guardStrict"]; ok && json.Unmarshal(v, &b) == nil {
		cfg.Strict = b
	}
	var custom []customGuardRule
	if v, ok := merged["guardCustom"]; ok && json.Unmarshal(v, &custom) == nil {
		for i, c := range custom {
			if i >= 100 || strings.TrimSpace(c.Pattern) == "" || len(c.Pattern) > 1000 {
				continue
			}
			if re := compileCustom(c.Pattern); re != nil {
				msg := strings.TrimSpace(c.Message)
				if msg == "" {
					msg = "Matches a custom dangerous-command rule."
				}
				cfg.Custom = append(cfg.Custom, guardRule{id: "custom", message: msg, severity: sevDanger, re: re})
			}
		}
	}
	return cfg
}

// commandLines splits text into logical command lines (joining backslash continuations).
func commandLines(text string) []string {
	var out []string
	var cur strings.Builder
	for _, l := range splitLines(text) {
		if strings.HasSuffix(l, `\`) {
			cur.WriteString(strings.TrimSuffix(l, `\`))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(l)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// checkDangerous returns the guard hits of text (one per rule).
func checkDangerous(text string, cfg guardConfig) []DangerMatch {
	if !cfg.Enabled || strings.TrimSpace(text) == "" {
		return nil
	}
	var out []DangerMatch
	seen := map[string]bool{}
	rules := guardRules
	if len(cfg.Custom) > 0 {
		rules = append(append([]guardRule(nil), guardRules...), cfg.Custom...)
	}
	for _, line := range commandLines(text) {
		if len(line) > 8192 {
			line = line[:8192]
		}
		for _, r := range rules {
			if r.severity == sevWarning && !cfg.Strict {
				continue
			}
			key := r.id + "\x00" + r.message
			if seen[key] || !r.re.MatchString(line) {
				continue
			}
			seen[key] = true
			out = append(out, DangerMatch{Rule: r.id, Message: r.message, Severity: r.severity, Line: truncateUTF8(strings.TrimSpace(line), 300)})
		}
	}
	return out
}

// typedText interprets keystrokes (macro steps) as a line editor would: printable text accumulates, Backspace/DEL
// removes a character, Ctrl-U / Ctrl-C clear the line, escape sequences are ignored, CR/LF end a line.
func typedText(data string) string {
	var out strings.Builder
	var line []rune
	flush := func() {
		out.WriteString(string(line))
		out.WriteByte('\n')
		line = line[:0]
	}
	rs := []rune(data)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\r' || r == '\n':
			flush()
		case r == 0x7f || r == 0x08:
			if len(line) > 0 {
				line = line[:len(line)-1]
			}
		case r == 0x15 || r == 0x03:
			line = line[:0]
		case r == 0x1b:
			// Skip an escape sequence: ESC [ … final, ESC O x, ESC x.
			if i+1 < len(rs) && (rs[i+1] == '[' || rs[i+1] == 'O') {
				i += 2
				for i < len(rs) && !(rs[i] >= 0x40 && rs[i] <= 0x7e) {
					i++
				}
			} else {
				i++
			}
		case r == '\t':
			line = append(line, ' ')
		case r < 0x20:
		default:
			line = append(line, r)
		}
	}
	if len(line) > 0 {
		out.WriteString(string(line))
	}
	return out.String()
}

// dangerousResponse is the 409 body of a refused send.
type dangerousResponse struct {
	Error   string        `json:"error"`
	Code    string        `json:"code"`
	Matches []DangerMatch `json:"matches"`
}

// dangerError carries guard hits out of validation helpers; handlers answer it with refuseDangerous.
type dangerError struct{ matches []DangerMatch }

func (e *dangerError) Error() string { return "the text contains a potentially dangerous command" }

// asDanger answers a dangerError with the 409 body (ok = true when it did).
func asDanger(c *echo.Context, err error) (bool, error) {
	var de *dangerError
	if errors.As(err, &de) {
		return true, refuseDangerous(c, de.matches)
	}
	return false, err
}

func refuseDangerous(c *echo.Context, matches []DangerMatch) error {
	return c.JSON(http.StatusConflict, dangerousResponse{
		Error:   "the text contains a potentially dangerous command: " + matches[0].Message + " Confirm to send it anyway.",
		Code:    "dangerous_command",
		Matches: matches,
	})
}
