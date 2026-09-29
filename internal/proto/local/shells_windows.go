//go:build windows

package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// detectShells lists PowerShell 7, Windows PowerShell, cmd, Git Bash and every WSL distribution.
func detectShells(ctx context.Context) []Shell {
	var out []Shell
	add := func(id, name, path string, args ...string) {
		if path == "" {
			return
		}
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			return
		}
		if args == nil {
			args = []string{}
		}
		out = append(out, Shell{ID: id, Name: name, Path: path, Args: args})
	}
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		add("pwsh", "PowerShell", p, "-NoLogo")
	} else {
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432")} {
			if base != "" {
				if p := filepath.Join(base, "PowerShell", "7", "pwsh.exe"); fileExists(p) {
					add("pwsh", "PowerShell", p, "-NoLogo")
					break
				}
			}
		}
	}
	add("powershell", "Windows PowerShell", filepath.Join(sysRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo")
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(sysRoot, "System32", "cmd.exe")
	}
	add("cmd", "Command Prompt", comspec)
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"),
		filepath.Join(os.Getenv("LocalAppData"), "Programs")} {
		if base == "" {
			continue
		}
		if p := filepath.Join(base, "Git", "bin", "bash.exe"); fileExists(p) {
			add("git-bash", "Git Bash", p, "--login", "-i")
			break
		}
	}
	wsl := filepath.Join(sysRoot, "System32", "wsl.exe")
	if fileExists(wsl) {
		for _, d := range wslDistros(ctx, wsl) {
			add("wsl:"+d, "WSL: "+d, wsl, "-d", d, "--cd", "~")
		}
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// wslDistros runs `wsl.exe -l -q` (UTF-16LE output) and returns the installed distributions.
func wslDistros(ctx context.Context, wsl string) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, wsl, "-l", "-q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return decodeWSLList(out)
}
