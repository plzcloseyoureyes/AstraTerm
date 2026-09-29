package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nexterm/nexterm/internal/httpx"
)

// Unit is one system service (systemd unit or Windows service).
type Unit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Load        string `json:"load"`              // systemd: loaded | not-found | masked | …; Windows: "loaded"
	Active      string `json:"active"`            // active | inactive | failed | activating | deactivating | reloading
	Sub         string `json:"sub"`               // running | exited | dead | failed | …; Windows: running | stopped | …
	Enabled     string `json:"enabled,omitempty"` // enabled | disabled | static | masked | …; Windows: auto | manual | disabled
	PID         int    `json:"pid,omitempty"`
}

// ServiceList is the answer of GET /api/monitor/{id}/services.
type ServiceList struct {
	Manager  string `json:"manager"` // "systemd" | "windows" | "" (unsupported)
	Services []Unit `json:"services"`
	Message  string `json:"message,omitempty"`
}

// Service actions.
var serviceActions = map[string]bool{"start": true, "stop": true, "restart": true, "reload": true, "enable": true, "disable": true}

// unitNameRE accepts systemd unit and Windows service names: no path separators, quotes, whitespace or shell syntax.
// Backslashes are allowed for systemd's escapes (e.g. systemd-fsck@dev-disk-by\x2duuid-….service); names are always
// passed as single arguments (shQuote / psQuote), where a backslash is literal.
var unitNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:+\\\-]{0,254}$`)

func validUnit(name string) error {
	if !unitNameRE.MatchString(name) {
		return httpx.BadRequest("invalid service name")
	}
	return nil
}

func (s *Service) listServices(ctx context.Context, t *target) (*ServiceList, error) {
	switch {
	case t.host.Platform == platLinux && t.host.has("systemctl"):
		res, err := s.run(ctx, t, command{sh: script(scriptSystemdServices)})
		if err != nil {
			return nil, err
		}
		units := parseSystemdUnits(res.Stdout)
		if len(units) == 0 && res.errText() != "" {
			return &ServiceList{Manager: "systemd", Services: []Unit{}, Message: res.errText()}, nil
		}
		return &ServiceList{Manager: "systemd", Services: units}, nil
	case t.host.Platform == platWindows:
		res, err := s.run(ctx, t, command{ps: script(scriptWindowsServices)})
		if err != nil {
			return nil, err
		}
		units, perr := parseWindowsServices(res.Stdout)
		if perr != nil {
			return nil, httpx.NewError(422, "command_failed", orDefault(res.errText(), perr.Error()))
		}
		return &ServiceList{Manager: "windows", Services: units}, nil
	}
	return &ServiceList{Manager: "", Services: []Unit{},
		Message: "Service management needs systemd (Linux) or Windows; " + orDefault(t.host.OS, t.host.Platform) + " is not supported."}, nil
}

// parseSystemdUnits merges `systemctl list-units --all --plain` (runtime state) with `list-unit-files` (enablement,
// including units that were never loaded).
func parseSystemdUnits(out []byte) []Unit {
	sec := sections(splitLines(out))
	byName := map[string]*Unit{}
	if u := sec["UNITS"]; u != nil {
		for _, l := range u.lines {
			f, desc := fieldsN(strings.TrimLeft(l, "●* "), 4)
			if len(f) < 4 || !strings.Contains(f[0], ".") {
				continue
			}
			byName[f[0]] = &Unit{Name: f[0], Load: f[1], Active: f[2], Sub: f[3], Description: strings.TrimSpace(desc)}
		}
	}
	if fs := sec["FILES"]; fs != nil {
		for _, l := range fs.lines {
			f := strings.Fields(l)
			if len(f) < 2 || !strings.Contains(f[0], ".") {
				continue
			}
			name := f[0]
			if strings.HasSuffix(name, "@.service") {
				continue // templates are not runnable by themselves
			}
			if u := byName[name]; u != nil {
				u.Enabled = f[1]
				continue
			}
			byName[name] = &Unit{Name: name, Load: "not-loaded", Active: "inactive", Sub: "dead", Enabled: f[1]}
		}
	}
	out2 := make([]Unit, 0, len(byName))
	for _, u := range byName {
		out2 = append(out2, *u)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Name < out2[j].Name })
	return out2
}

type winService struct {
	N string  `json:"n"`
	D string  `json:"d"`
	S string  `json:"s"`
	M string  `json:"m"`
	I int     `json:"i"`
	X *string `json:"x"`
}

func parseWindowsServices(out []byte) ([]Unit, error) {
	var list []winService
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, fmt.Errorf("no service data")
	}
	if strings.HasPrefix(raw, "{") { // a single service is not wrapped in an array by older PowerShell
		raw = "[" + raw + "]"
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	units := make([]Unit, 0, len(list))
	for _, w := range list {
		u := Unit{Name: w.N, Description: w.D, Load: "loaded", Sub: strings.ToLower(w.S), PID: w.I}
		switch strings.ToLower(w.S) {
		case "running":
			u.Active = "active"
		case "start pending", "continue pending":
			u.Active = "activating"
		case "stop pending", "pause pending":
			u.Active = "deactivating"
		default:
			u.Active = "inactive"
		}
		switch strings.ToLower(w.M) {
		case "auto":
			u.Enabled = "enabled"
		case "manual":
			u.Enabled = "manual"
		case "disabled":
			u.Enabled = "disabled"
		default:
			u.Enabled = strings.ToLower(w.M)
		}
		if w.X != nil && u.Description == "" {
			u.Description = *w.X
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return strings.ToLower(units[i].Name) < strings.ToLower(units[j].Name) })
	return units, nil
}

// serviceAction starts / stops / restarts / reloads / enables / disables a service.
func (s *Service) serviceAction(ctx context.Context, t *target, name, action string, sudo bool) error {
	if err := validUnit(name); err != nil {
		return err
	}
	if !serviceActions[action] {
		return httpx.BadRequest("unknown action (start, stop, restart, reload, enable, disable)")
	}
	switch {
	case t.host.Platform == platLinux && t.host.has("systemctl"):
		argv := []string{"systemctl"}
		if !sudo {
			argv = append(argv, "--no-ask-password")
		}
		argv = append(argv, action, "--", name)
		res, err := s.execArgv(ctx, t, argv, sudo)
		if err != nil {
			return err
		}
		return actionError(res, name)
	case t.host.Platform == platWindows:
		if sudo {
			return httpx.BadRequest("sudo is not available on Windows hosts")
		}
		var cmd string
		q := psQuote(name)
		switch action {
		case "start":
			cmd = "Start-Service -Name " + q
		case "stop":
			cmd = "Stop-Service -Force -Name " + q
		case "restart":
			cmd = "Restart-Service -Force -Name " + q
		case "enable":
			cmd = "Set-Service -StartupType Automatic -Name " + q
		case "disable":
			cmd = "Set-Service -StartupType Disabled -Name " + q
		default:
			return httpx.BadRequest("Windows services cannot be reloaded")
		}
		res, err := s.run(ctx, t, command{ps: "$ErrorActionPreference='Stop';try{" + cmd + "}catch{[Console]::Error.WriteLine($_.Exception.Message);exit 1}"})
		if err != nil {
			return err
		}
		return actionError(res, name)
	}
	return httpx.NewError(422, "monitor_unavailable", "service management is not supported on this host")
}

// serviceLogs returns the last lines of a unit's journal.
func (s *Service) serviceLogs(ctx context.Context, t *target, name string, lines int, sudo bool) ([]string, error) {
	if err := validUnit(name); err != nil {
		return nil, err
	}
	if t.host.Platform != platLinux || !t.host.has("journalctl") {
		return nil, httpx.NewError(422, "monitor_unavailable", "service logs need systemd's journalctl")
	}
	lines = min(max(lines, 1), 5000)
	res, err := s.execArgv(ctx, t, []string{"journalctl", "-u", name, "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso"}, sudo)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, l := range splitLines(res.Stdout) {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, clip(l, 8192))
		}
	}
	if !res.ok() && len(out) == 0 {
		return nil, actionError(res, name)
	}
	return out, nil
}
