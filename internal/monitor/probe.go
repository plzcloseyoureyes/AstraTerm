package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// hostInfo is what the one-shot probe learned about a monitored host (static for the lifetime of a transport).
type hostInfo struct {
	Platform string `json:"platform"` // linux, darwin, freebsd, openbsd, netbsd, dragonfly, windows
	OS       string `json:"os"`       // pretty name, e.g. "Ubuntu 24.04.1 LTS", "macOS 15.1", "Windows Server 2022"
	Kernel   string `json:"kernel"`
	Hostname string `json:"hostname"`
	Arch     string `json:"arch"`
	CPUModel string `json:"cpuModel,omitempty"`
	Cores    int    `json:"cores,omitempty"`
	Virt     string `json:"virt,omitempty"`
	Model    string `json:"model,omitempty"` // hardware model (device tree / Win32_ComputerSystem)
	// Tools lists the helpers found on the host (sudo, systemctl, journalctl, ss, lsof, sockstat, powershell). Written
	// once by the probe, read-only afterwards.
	Tools map[string]bool `json:"tools"`
}

func (h *hostInfo) has(tool string) bool { return h != nil && h.Tools[tool] }

// unavailableError means monitoring cannot work on the host (remote command execution refused, unsupported OS). The
// collector then retries rarely instead of every few seconds.
type unavailableError struct{ msg string }

func (e *unavailableError) Error() string { return e.msg }

// probeHost identifies the host: the POSIX probe first (Linux, macOS, BSD), then PowerShell (Windows OpenSSH).
func probeHost(ctx context.Context, r runner) (*hostInfo, error) {
	res, err := r.run(ctx, command{sh: script(scriptProbe)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil && bytes.Contains(res.Stdout, []byte("@@OS ")) {
		if info := parseProbe(res.Stdout); info.Platform != "" {
			if info.Platform != platWindows {
				return info, nil
			}
			// Cygwin / MSYS login shell on Windows: the PowerShell probe gives the real details.
			if w, werr := r.run(ctx, command{ps: script(scriptProbePS)}); werr == nil {
				if wi, ok := parseWindowsProbe(w.Stdout); ok {
					return wi, nil
				}
			}
			return info, nil
		}
	}
	unixErr := err
	unixText := ""
	if res != nil {
		unixText = res.errText()
	}
	w, werr := r.run(ctx, command{ps: script(scriptProbePS)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if werr == nil {
		if info, ok := parseWindowsProbe(w.Stdout); ok {
			return info, nil
		}
	}
	// Transport problems (channel refused, connection lost) are retried; anything else means exec is not usable.
	if unixErr != nil && werr != nil {
		return nil, fmt.Errorf("remote command execution failed: %w", unixErr)
	}
	msg := "the host does not allow remote command execution or runs an unsupported system"
	if unixText != "" {
		msg += ": " + unixText
	}
	return nil, &unavailableError{msg: msg}
}

// parseProbe parses the POSIX probe output.
func parseProbe(out []byte) *hostInfo {
	sec := sections(splitLines(out))
	uname := sec["OS"].first()
	h := &hostInfo{
		Hostname: sec["HOSTNAME"].first(),
		Kernel:   sec["KERNEL"].first(),
		Arch:     sec["ARCH"].first(),
		Tools:    map[string]bool{},
	}
	switch {
	case uname == "Linux":
		h.Platform = platLinux
	case uname == "Darwin":
		h.Platform = platDarwin
	case uname == "FreeBSD":
		h.Platform = platFreeBSD
	case uname == "OpenBSD":
		h.Platform = platOpenBSD
	case uname == "NetBSD":
		h.Platform = platNetBSD
	case uname == "DragonFly":
		h.Platform = platDragon
	case strings.HasPrefix(uname, "CYGWIN") || strings.HasPrefix(uname, "MINGW") || strings.HasPrefix(uname, "MSYS"):
		h.Platform = platWindows
	case uname != "":
		h.Platform = strings.ToLower(uname)
	}
	for kv := range strings.FieldsSeq(sec["TOOLS"].first()) {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			h.Tools[k] = true
		}
	}
	h.Cores = sec["NCPU"].int()
	h.Virt = sec["VIRT"].first()
	if h.Virt == "none" {
		h.Virt = ""
	}
	h.Model = strings.TrimSpace(strings.Trim(sec["DTMODEL"].text(), "\x00"))

	rel := sec["RELEASE"]
	switch h.Platform {
	case platLinux:
		kv := parseKV(rel)
		h.OS = kv["PRETTY_NAME"]
		if h.OS == "" {
			h.OS = strings.TrimSpace(kv["NAME"] + " " + kv["VERSION"])
		}
		if h.OS == "" {
			h.OS = "Linux"
		}
		h.CPUModel = linuxCPUModel(sec["CPUINFO"])
	case platDarwin:
		kv := parseColonKV(rel)
		name := kv["ProductName"]
		if name == "" || name == "Mac OS X" {
			name = "macOS"
		}
		h.OS = strings.TrimSpace(name + " " + kv["ProductVersion"])
		h.CPUModel = sec["CPUMODEL"].first()
	default:
		ver := rel.first()
		if ver == "" {
			ver = h.Kernel
		}
		h.OS = strings.TrimSpace(uname + " " + ver)
		h.CPUModel = sec["CPUMODEL"].first()
	}
	h.Kernel = strings.TrimSpace(h.Kernel)
	return h
}

// parseKV parses os-release style KEY="value" lines.
func parseKV(sec *section) map[string]string {
	out := map[string]string{}
	if sec == nil {
		return out
	}
	for _, l := range sec.lines {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `"'`)
		}
		out[k] = v
	}
	return out
}

// parseColonKV parses "Key: value" lines (sw_vers).
func parseColonKV(sec *section) map[string]string {
	out := map[string]string{}
	if sec == nil {
		return out
	}
	for _, l := range sec.lines {
		if k, v, ok := strings.Cut(l, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

var armImplementers = map[string]string{
	"0x41": "ARM", "0x42": "Broadcom", "0x43": "Cavium", "0x46": "Fujitsu", "0x48": "HiSilicon", "0x4e": "NVIDIA",
	"0x50": "APM", "0x51": "Qualcomm", "0x53": "Samsung", "0x56": "Marvell", "0x61": "Apple", "0x69": "Intel",
	"0x6d": "Microsoft", "0xc0": "Ampere",
}

var armParts = map[string]string{
	"0xd03": "Cortex-A53", "0xd04": "Cortex-A35", "0xd05": "Cortex-A55", "0xd07": "Cortex-A57", "0xd08": "Cortex-A72",
	"0xd09": "Cortex-A73", "0xd0a": "Cortex-A75", "0xd0b": "Cortex-A76", "0xd0c": "Neoverse-N1", "0xd0d": "Cortex-A77",
	"0xd40": "Neoverse-V1", "0xd41": "Cortex-A78", "0xd44": "Cortex-X1", "0xd46": "Cortex-A510", "0xd47": "Cortex-A710",
	"0xd48": "Cortex-X2", "0xd49": "Neoverse-N2", "0xd4b": "Cortex-A78C", "0xd4f": "Neoverse-V2", "0xd80": "Cortex-A520",
	"0xd81": "Cortex-A720", "0xd82": "Cortex-X4", "0xd84": "Neoverse-V3", "0xd8e": "Neoverse-N3",
}

// linuxCPUModel picks a CPU name from /proc/cpuinfo keys (x86 "model name", ARM implementer/part, s390 "machine").
func linuxCPUModel(sec *section) string {
	kv := parseColonKV(sec)
	for _, k := range []string{"model name", "cpu model", "Model", "Hardware", "machine"} {
		if v := kv[k]; v != "" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	if impl := strings.ToLower(kv["CPU implementer"]); impl != "" {
		vendor := armImplementers[impl]
		if vendor == "" {
			vendor = "ARM"
		}
		if part := armParts[strings.ToLower(kv["CPU part"])]; part != "" && vendor == "ARM" {
			return "ARM " + part
		}
		return vendor + " (ARM64)"
	}
	return kv["vendor_id"]
}

type winProbe struct {
	OS    string `json:"os"`
	H     string `json:"h"`
	Cap   string `json:"cap"`
	Ver   string `json:"ver"`
	Arch  string `json:"arch"`
	CPU   string `json:"cpu"`
	N     int    `json:"n"`
	Model string `json:"model"`
	Maker string `json:"maker"`
}

// parseWindowsProbe parses the PowerShell probe's JSON line.
func parseWindowsProbe(out []byte) (*hostInfo, bool) {
	for _, l := range splitLines(out) {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var p winProbe
		if err := json.Unmarshal([]byte(l), &p); err != nil || p.OS != "Windows" {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(p.Cap, "Microsoft "))
		if name == "" {
			name = "Windows"
		}
		model := strings.TrimSpace(p.Maker + " " + p.Model)
		return &hostInfo{Platform: platWindows, OS: name, Kernel: p.Ver, Hostname: p.H, Arch: strings.ToLower(p.Arch),
			CPUModel: strings.Join(strings.Fields(p.CPU), " "), Cores: p.N, Model: model, Tools: map[string]bool{"powershell": true}}, true
	}
	return nil, false
}
