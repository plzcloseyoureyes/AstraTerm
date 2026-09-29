//go:build !windows

package local

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// detectShells lists $SHELL first, then /etc/shells entries, then well-known shells found on PATH.
func detectShells(_ context.Context) []Shell {
	var out []Shell
	seen := map[string]bool{}
	ids := map[string]bool{}
	add := func(path string) {
		if path == "" || !filepath.IsAbs(path) || nonInteractive(filepath.Base(path)) {
			return
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			return
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			real = path
		}
		if seen[path] || seen[real] {
			return
		}
		seen[path], seen[real] = true, true
		base := filepath.Base(path)
		id := base
		if ids[id] {
			id = path
		}
		ids[id] = true
		out = append(out, Shell{ID: id, Name: shellName(base), Path: path, Args: []string{}})
	}
	add(os.Getenv("SHELL"))
	if f, err := os.Open("/etc/shells"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			add(line)
		}
		f.Close()
	}
	for _, name := range []string{"zsh", "bash", "fish", "sh", "pwsh", "nu", "xonsh", "elvish", "ksh", "tcsh", "dash"} {
		if p, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				add(abs)
			}
		}
	}
	if len(out) == 0 {
		add("/bin/sh")
	}
	return out
}
