package tools

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// macRegexp matches a MAC address in the common colon/dash notations.
var macRegexp = regexp.MustCompile(`([0-9a-fA-F]{1,2}[:-]){5}[0-9a-fA-F]{1,2}`)

// arpTable returns a map of IP → MAC from the host's ARP / neighbor cache. It reads /proc/net/arp on Linux and falls
// back to parsing `ip neigh` or `arp -a` elsewhere. Incomplete or invalid entries are skipped. The tool warms this
// cache by connecting to on-link hosts before reading it.
func arpTable(ctx context.Context) map[string]string {
	if runtime.GOOS == "linux" {
		if t := arpFromProc(); len(t) > 0 {
			return t
		}
	}
	if runtime.GOOS == "windows" {
		return arpFromCommand(ctx, "arp", "-a") // -n means an interface argument on Windows
	}
	if t := arpFromCommand(ctx, "ip", "neigh"); len(t) > 0 {
		return t
	}
	return arpFromCommand(ctx, "arp", "-a", "-n")
}

func arpFromProc() map[string]string {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		// IP address, HW type, Flags, HW address, Mask, Device
		if len(fields) < 4 {
			continue
		}
		ip, mac := fields[0], fields[3]
		if isValidMAC(mac) {
			out[ip] = normalizeMAC(mac)
		}
	}
	return out
}

func arpFromCommand(ctx context.Context, name string, args ...string) map[string]string {
	if _, err := exec.LookPath(name); err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	outBytes, err := exec.CommandContext(cctx, name, args...).Output()
	if err != nil {
		return nil
	}
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(outBytes)))
	for sc.Scan() {
		line := sc.Text()
		mac := macRegexp.FindString(line)
		if !isValidMAC(mac) || strings.EqualFold(normalizeMAC(mac), "ff:ff:ff:ff:ff:ff") {
			continue
		}
		var ip string
		if m := ipInParens.FindStringSubmatch(line); m != nil { // `arp -a` (unix): host (1.2.3.4) at aa:bb:...
			ip = m[1]
		} else if f := strings.Fields(line); len(f) > 0 { // `ip neigh` / Windows `arp -a`: 1.2.3.4 … aa-bb-…
			ip = f[0]
		}
		if parsed := net.ParseIP(ip); parsed != nil {
			out[parsed.String()] = normalizeMAC(mac)
		}
	}
	return out
}

var ipInParens = regexp.MustCompile(`\(([0-9a-fA-F.:]+)\)`)

func isValidMAC(mac string) bool {
	mac = strings.TrimSpace(mac)
	if mac == "" || mac == "00:00:00:00:00:00" || strings.EqualFold(mac, "(incomplete)") {
		return false
	}
	return macRegexp.MatchString(mac)
}

// normalizeMAC lower-cases and zero-pads a MAC to aa:bb:cc:dd:ee:ff form.
func normalizeMAC(mac string) string {
	parts := strings.FieldsFunc(mac, func(r rune) bool { return r == ':' || r == '-' })
	if len(parts) != 6 {
		return strings.ToLower(mac)
	}
	for i, p := range parts {
		if len(p) == 1 {
			p = "0" + p
		}
		parts[i] = strings.ToLower(p)
	}
	return strings.Join(parts, ":")
}
