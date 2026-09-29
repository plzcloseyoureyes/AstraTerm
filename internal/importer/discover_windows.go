//go:build windows

package importer

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Windows registry sources (desktop mode): PuTTY, KiTTY and WinSCP keep their sessions under HKCU by default. They
// are offered as pseudo paths "registry:<id>" and rendered to the .reg / WinSCP.ini text the importers already read.

type regSource struct {
	id, key, hostKeys, format, label string
}

var regSources = []regSource{
	{"putty", `Software\SimonTatham\PuTTY\Sessions`, `Software\SimonTatham\PuTTY\SshHostKeys`, fmtPuttyReg, "PuTTY sessions (registry)"},
	{"kitty", `Software\9bis.com\KiTTY\Sessions`, `Software\9bis.com\KiTTY\SshHostKeys`, fmtPuttyReg, "KiTTY sessions (registry)"},
	{"winscp", `Software\Martin Prikryl\WinSCP 2\Sessions`, "", fmtWinSCP, "WinSCP sessions (registry)"},
}

const maxRegistrySessions = 5000

func registrySources() []discoverEntry {
	var out []discoverEntry
	for _, rs := range regSources {
		k, err := registry.OpenKey(registry.CURRENT_USER, rs.key, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		info, err := k.Stat()
		k.Close()
		if err != nil || info.SubKeyCount == 0 {
			continue
		}
		out = append(out, discoverEntry{Path: registryPrefix + rs.id, Format: rs.format,
			Label: fmt.Sprintf("%s — %d session(s)", rs.label, info.SubKeyCount)})
	}
	return out
}

func readRegistrySource(id string) ([]byte, string, bool) {
	for _, rs := range regSources {
		if rs.id != id {
			continue
		}
		var b strings.Builder
		if rs.format == fmtPuttyReg {
			b.WriteString("Windows Registry Editor Version 5.00\r\n")
		}
		root, err := registry.OpenKey(registry.CURRENT_USER, rs.key, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			return nil, "", false
		}
		names, _ := root.ReadSubKeyNames(maxRegistrySessions)
		root.Close()
		for _, name := range names {
			k, err := registry.OpenKey(registry.CURRENT_USER, rs.key+`\`+name, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			if rs.format == fmtPuttyReg {
				fmt.Fprintf(&b, "\r\n[HKEY_CURRENT_USER\\%s\\%s]\r\n", rs.key, name)
			} else {
				fmt.Fprintf(&b, "\r\n[Sessions\\%s]\r\n", name)
			}
			writeRegistryValues(&b, k, rs.format == fmtPuttyReg)
			k.Close()
		}
		if rs.hostKeys != "" {
			if k, err := registry.OpenKey(registry.CURRENT_USER, rs.hostKeys, registry.QUERY_VALUE); err == nil {
				fmt.Fprintf(&b, "\r\n[HKEY_CURRENT_USER\\%s]\r\n", rs.hostKeys)
				writeRegistryValues(&b, k, true)
				k.Close()
			}
		}
		return []byte(b.String()), rs.format, true
	}
	return nil, "", false
}

// writeRegistryValues renders a key's string / DWORD values as .reg lines (asReg) or INI lines (WinSCP).
func writeRegistryValues(b *strings.Builder, k registry.Key, asReg bool) {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	names, _ := k.ReadValueNames(4096)
	for _, n := range names {
		if strings.ContainsAny(n, "\r\n") || !asReg && strings.Contains(n, "=") {
			continue
		}
		if s, _, err := k.GetStringValue(n); err == nil {
			if len(s) > 64<<10 || strings.ContainsAny(s, "\r\n") {
				continue
			}
			if asReg {
				fmt.Fprintf(b, "\"%s\"=\"%s\"\r\n", esc.Replace(n), esc.Replace(s))
			} else {
				fmt.Fprintf(b, "%s=%s\r\n", n, s)
			}
			continue
		}
		if v, _, err := k.GetIntegerValue(n); err == nil {
			if asReg {
				fmt.Fprintf(b, "\"%s\"=dword:%08x\r\n", esc.Replace(n), uint32(v))
			} else {
				fmt.Fprintf(b, "%s=%d\r\n", n, v)
			}
		}
	}
}
