package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Local file discovery (desktop mode). The importer offers to read well-known config files from the NexTerm host so
// the user can "Import from ~/.ssh/config" or "Scan for PuTTY/MobaXterm/FileZilla files" without hunting for paths.
// Only sources returned by discovery may later be read by preview/commit (a supplied path is re-validated against the
// candidate set), so a client cannot point the server at an arbitrary file. On Windows, PuTTY / KiTTY / WinSCP
// sessions are also read from the registry (pseudo paths "registry:<source>", see discover_windows.go).

const (
	maxDiscoverFileBytes = 8 << 20
	maxDiscoverEntries   = 200
	registryPrefix       = "registry:"
)

// discoverLocalFiles returns the known config sources that exist on the host.
func discoverLocalFiles() []discoverEntry {
	var out []discoverEntry
	seen := map[string]bool{}
	add := func(e discoverEntry) {
		if !seen[e.Path] && len(out) < maxDiscoverEntries {
			seen[e.Path] = true
			out = append(out, e)
		}
	}
	for _, cand := range candidatePaths() {
		if fi, ok := regularFile(cand.path); ok {
			add(discoverEntry{Path: cand.path, Format: cand.format, Label: cand.label, Size: fi.Size()})
		}
	}
	for _, e := range discoverDirFiles() {
		add(e)
	}
	for _, e := range registrySources() {
		add(e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Format != out[j].Format {
			return formatRank(out[i].Format) < formatRank(out[j].Format)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// formatRank orders the discovery list: ssh_config first, then MobaXterm, then the rest in wizard order.
func formatRank(f string) int {
	switch f {
	case fmtSSHConfig:
		return -2
	case fmtMobaXterm:
		return -1
	}
	for i, x := range importFormats {
		if x == f {
			return i
		}
	}
	return len(importFormats)
}

// regularFile stats path (following symlinks) and accepts non-empty regular files within the discovery size limit.
func regularFile(path string) (os.FileInfo, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 || fi.Size() > maxDiscoverFileBytes {
		return nil, false
	}
	return fi, true
}

type candidate struct {
	path   string
	format string
	label  string
}

func candidatePaths() []candidate {
	home := userHomeDir()
	var c []candidate
	add := func(path, format, label string) {
		if path != "" {
			c = append(c, candidate{path: filepath.Clean(path), format: format, label: label})
		}
	}
	if home != "" {
		add(filepath.Join(home, ".ssh", "config"), fmtSSHConfig, "OpenSSH config (~/.ssh/config)")
		add(filepath.Join(home, ".ssh", "known_hosts"), fmtKnownHosts, "OpenSSH known_hosts (~/.ssh/known_hosts)")
		// MobaXterm.ini: installed edition (My Documents, also when redirected to OneDrive) and portable next to the exe.
		add(filepath.Join(home, "Documents", "MobaXterm", "MobaXterm.ini"), fmtMobaXterm, "MobaXterm.ini (Documents)")
		add(filepath.Join(home, "OneDrive", "Documents", "MobaXterm", "MobaXterm.ini"), fmtMobaXterm, "MobaXterm.ini (OneDrive Documents)")
		add(filepath.Join(home, "MobaXterm", "MobaXterm.ini"), fmtMobaXterm, "MobaXterm.ini (portable)")
		add(filepath.Join(home, ".putty", "sshhostkeys"), fmtKnownHosts, "PuTTY host keys (~/.putty/sshhostkeys)")
		add(filepath.Join(home, ".config", "filezilla", "sitemanager.xml"), fmtFileZilla, "FileZilla site manager")
		add(filepath.Join(home, ".filezilla", "sitemanager.xml"), fmtFileZilla, "FileZilla site manager (legacy)")
		add(filepath.Join(home, ".config", "mRemoteNG", "confCons.xml"), fmtMRemoteNG, "mRemoteNG connections")
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		add(filepath.Join(appData, "MobaXterm", "MobaXterm.ini"), fmtMobaXterm, "MobaXterm.ini (AppData)")
		add(filepath.Join(appData, "FileZilla", "sitemanager.xml"), fmtFileZilla, "FileZilla site manager")
		add(filepath.Join(appData, "mRemoteNG", "confCons.xml"), fmtMRemoteNG, "mRemoteNG connections")
		add(filepath.Join(appData, "WinSCP.ini"), fmtWinSCP, "WinSCP.ini")
	}
	return c
}

// discoverDirFiles lists sources that live one-per-file: Remmina profiles, Unix PuTTY sessions (a directory rendered
// as one source) and exported MobaXterm session files in the usual download/document folders.
func discoverDirFiles() []discoverEntry {
	home := userHomeDir()
	if home == "" {
		return nil
	}
	var out []discoverEntry
	list := func(dir, suffix string, max int, fn func(path, name string, fi os.FileInfo)) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		n := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), suffix) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if fi, ok := regularFile(p); ok {
				fn(p, e.Name(), fi)
				if n++; n >= max {
					return
				}
			}
		}
	}
	remmina := filepath.Join(home, ".local", "share", "remmina")
	list(remmina, ".remmina", 100, func(p, name string, fi os.FileInfo) {
		out = append(out, discoverEntry{Path: p, Format: fmtRemmina, Label: "Remmina profile: " + name, Size: fi.Size()})
	})
	for _, dir := range []string{"Documents", "Desktop", "Downloads", filepath.Join("Documents", "MobaXterm"),
		filepath.Join("OneDrive", "Documents"), filepath.Join("OneDrive", "Desktop")} {
		list(filepath.Join(home, dir), ".mxtsessions", 20, func(p, name string, fi os.FileInfo) {
			out = append(out, discoverEntry{Path: p, Format: fmtMobaXterm, Label: "MobaXterm sessions: " + filepath.Join(dir, name), Size: fi.Size()})
		})
	}
	if dir := filepath.Join(home, ".putty", "sessions"); dirHasFiles(dir) {
		out = append(out, discoverEntry{Path: dir, Format: fmtPuttyReg, Label: "PuTTY sessions (~/.putty/sessions)"})
	}
	return out
}

func dirHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

// resolveDiscoverPath returns the content of a discovered source, re-validating that path is one the discovery would
// return (defence against a client pointing preview/commit at an arbitrary file). It also expands ssh_config Include
// directives so an imported config is complete. Returns the (possibly detected) format.
func resolveDiscoverPath(path string) ([]byte, string, error) {
	clean := path
	if !strings.HasPrefix(path, registryPrefix) {
		clean = filepath.Clean(path)
	}
	format, allowed := "", false
	for _, e := range discoverLocalFiles() {
		if e.Path == clean {
			allowed, format = true, e.Format
			break
		}
	}
	if !allowed {
		return nil, "", badRequest("that file is not one of the discovered import sources")
	}
	if strings.HasPrefix(clean, registryPrefix) {
		data, f, ok := readRegistrySource(strings.TrimPrefix(clean, registryPrefix))
		if !ok {
			return nil, "", badRequest("could not read " + clean)
		}
		return data, f, nil
	}
	if fi, err := os.Stat(clean); err == nil && fi.IsDir() {
		return puttySessionDirAsReg(clean), format, nil
	}
	data, err := readFileLimited(clean, maxDiscoverFileBytes)
	if err != nil {
		return nil, "", badRequest("could not read " + clean)
	}
	if format == fmtSSHConfig {
		data = []byte(expandSSHConfigIncludes(string(data), filepath.Dir(clean), 0))
	}
	return data, format, nil
}

// puttySessionDirAsReg renders Unix PuTTY's per-session files ("Key=Value" lines; numbers are DWORDs) as a .reg
// export so the PuTTY importer reads them.
func puttySessionDirAsReg(dir string) []byte {
	var b strings.Builder
	b.WriteString("Windows Registry Editor Version 5.00\r\n")
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if e.IsDir() || n >= maxImportItems {
			continue
		}
		data, err := readFileLimited(filepath.Join(dir, e.Name()), 1<<20)
		if err != nil {
			continue
		}
		n++
		fmt.Fprintf(&b, "\r\n[HKEY_CURRENT_USER\\Software\\SimonTatham\\PuTTY\\Sessions\\%s]\r\n", e.Name())
		forEachLine(decodeMaybeCP1252(data), func(line string) {
			line = strings.TrimRight(line, "\r")
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				return
			}
			b.WriteString(regLine(line[:eq], line[eq+1:]))
		})
	}
	return []byte(b.String())
}

// regLine renders one .reg value line: digits-only values as DWORDs, everything else as an escaped string.
func regLine(key, val string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	if val != "" && len(val) <= 10 && strings.Trim(val, "0123456789") == "" {
		if n, err := strconv.ParseUint(val, 10, 32); err == nil {
			return fmt.Sprintf("\"%s\"=dword:%08x\r\n", esc.Replace(key), n)
		}
	}
	return fmt.Sprintf("\"%s\"=\"%s\"\r\n", esc.Replace(key), esc.Replace(val))
}

// expandSSHConfigIncludes inlines `Include` directives (relative paths resolve against ~/.ssh, as OpenSSH does) up to
// a small recursion depth. After an Include inside a Host block the block's Host line is repeated, so directives that
// follow the Include still belong to that block (OpenSSH semantics) rather than to the included file's last block.
func expandSSHConfigIncludes(text, baseDir string, depth int) string {
	if depth > 8 {
		return text
	}
	var out strings.Builder
	currentHost := ""
	forEachLine(text, func(line string) {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		fields := strings.Fields(strings.Replace(trimmed, "=", " ", 1))
		if len(fields) > 0 {
			switch strings.ToLower(fields[0]) {
			case "host", "match":
				currentHost = trimmed
			case "include":
				for _, pat := range fields[1:] {
					out.WriteString(expandOneInclude(strings.Trim(pat, `"`), baseDir, depth))
				}
				if currentHost != "" {
					out.WriteString(currentHost)
					out.WriteString("\n")
				}
				return
			}
		}
		out.WriteString(line)
		out.WriteString("\n")
	})
	return out.String()
}

func expandOneInclude(pat, baseDir string, depth int) string {
	pat = expandTilde(pat)
	if !filepath.IsAbs(pat) {
		// OpenSSH resolves relative includes of the user config against ~/.ssh.
		root := baseDir
		if home := userHomeDir(); home != "" {
			root = filepath.Join(home, ".ssh")
		}
		pat = filepath.Join(root, pat)
	}
	matches, err := filepath.Glob(pat)
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	var b strings.Builder
	for i, m := range matches {
		if i >= 64 {
			break
		}
		data, err := readFileLimited(m, maxDiscoverFileBytes)
		if err != nil {
			continue
		}
		b.WriteString(expandSSHConfigIncludes(string(data), filepath.Dir(m), depth+1))
		b.WriteString("\n")
	}
	return b.String()
}
