//go:build !linux && !windows

// On other platforms (notably darwin under CGO_ENABLED=0, where go.bug.st/serial's enumerator requires cgo) only
// port names are available; USB details are omitted.
package serial

import (
	"runtime"
	"sort"
	"strings"

	goserial "go.bug.st/serial"
)

// listPorts returns the host's serial port names (no USB details on this platform). On macOS every device appears
// twice: /dev/cu.* (call-out, what a console wants) first, /dev/tty.* (dial-in, waits for carrier) after it.
func listPorts() []PortInfo {
	names, err := goserial.GetPortsList()
	if err != nil {
		return []PortInfo{}
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		p := PortInfo{Name: n}
		if runtime.GOOS == "darwin" && strings.HasPrefix(n, "/dev/tty.") {
			p.Description = "dial-in device — prefer /dev/cu." + strings.TrimPrefix(n, "/dev/tty.")
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := strings.HasPrefix(out[i].Name, "/dev/cu."), strings.HasPrefix(out[j].Name, "/dev/cu.")
		if ci != cj {
			return ci
		}
		return out[i].Name < out[j].Name
	})
	return out
}
