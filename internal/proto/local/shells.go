package local

import (
	"strings"
	"unicode/utf16"
)

// shellNames maps executable base names to display names.
var shellNames = map[string]string{
	"bash": "Bash", "zsh": "Zsh", "fish": "Fish", "sh": "sh", "dash": "Dash", "ksh": "Korn shell",
	"mksh": "MirBSD Korn shell", "tcsh": "tcsh", "csh": "C shell", "pwsh": "PowerShell", "nu": "Nushell",
	"xonsh": "Xonsh", "elvish": "Elvish", "rbash": "Restricted Bash", "busybox": "BusyBox",
}

func shellName(base string) string {
	if n, ok := shellNames[strings.TrimSuffix(strings.ToLower(base), ".exe")]; ok {
		return n
	}
	return base
}

// nonInteractive lists /etc/shells entries that are not usable as interactive shells.
func nonInteractive(base string) bool {
	switch strings.ToLower(base) {
	case "nologin", "false", "true", "git-shell", "scponly", "rssh", "sync", "shutdown", "halt":
		return true
	}
	return false
}

// decodeWSLList decodes the output of `wsl.exe -l -q`, which is UTF-16LE (with or without BOM) on most Windows
// builds and UTF-8 when WSL_UTF8=1, into distribution names.
func decodeWSLList(out []byte) []string {
	text := string(out)
	if isUTF16LE(out) {
		b := out
		if len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe {
			b = b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		text = string(utf16.Decode(u))
	}
	text = strings.TrimPrefix(text, "\ufeff")
	var names []string
	for _, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(strings.Trim(line, "\x00\r"))
		if name == "" || strings.ContainsAny(name, "\x00") {
			continue
		}
		// Docker Desktop's internal distributions are not interactive environments.
		if strings.HasPrefix(strings.ToLower(name), "docker-desktop") {
			continue
		}
		names = append(names, name)
	}
	return names
}

func isUTF16LE(b []byte) bool {
	if len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe {
		return true
	}
	if len(b) < 2 || len(b)%2 != 0 {
		return false
	}
	zeros := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros*2 >= len(b)/2 // most high bytes are zero for ASCII names
}
