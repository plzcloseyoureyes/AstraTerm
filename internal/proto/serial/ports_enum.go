//go:build linux || windows

// On Linux and Windows go.bug.st/serial's enumerator builds without cgo and provides USB VID/PID/serial details.
package serial

import (
	goserial "go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// listPorts returns the host's serial ports with USB details when available, falling back to bare names.
func listPorts() []PortInfo {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil || details == nil {
		return basicPorts()
	}
	out := make([]PortInfo, 0, len(details))
	for _, d := range details {
		if d == nil {
			continue
		}
		desc := d.Product
		if desc == "" && d.Manufacturer != "" {
			desc = d.Manufacturer
		}
		out = append(out, PortInfo{
			Name:        d.Name,
			Description: desc,
			VID:         d.VID,
			PID:         d.PID,
			Serial:      d.SerialNumber,
		})
	}
	if len(out) == 0 {
		return basicPorts()
	}
	return out
}

func basicPorts() []PortInfo {
	names, err := goserial.GetPortsList()
	if err != nil {
		return []PortInfo{}
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		out = append(out, PortInfo{Name: n})
	}
	return out
}
