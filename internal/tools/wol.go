package tools

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nexterm/nexterm/internal/httpx"
)

type wolRequest struct {
	MAC             string `json:"mac"`
	Broadcast       string `json:"broadcast"` // broadcast / subnet-directed / unicast address (default 255.255.255.255)
	Port            int    `json:"port"`      // UDP 9 (default) or 7 (any port is accepted)
	SecureOn        string `json:"secureOn"`  // optional 6-byte SecureOn password (MAC-like)
	Count           int    `json:"count"`     // number of packets to send (default 1)
	ViaConnectionID string `json:"viaConnectionId"`
}

var macClean = regexp.MustCompile(`[^0-9a-fA-F]`)

// parseMAC parses a MAC address in any common separator style (aa:bb…, aa-bb…, aabb.ccdd.eeff, aabbccddeeff).
func parseMAC(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return nil, fmt.Errorf("invalid MAC address")
	}
	hexStr := macClean.ReplaceAllString(s, "")
	if len(hexStr) != 12 || len(s)-len(hexStr) > 5 {
		return nil, fmt.Errorf("invalid MAC address")
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC address")
	}
	return b, nil
}

// buildMagicPacket returns the Wake-on-LAN magic packet: 6×0xFF followed by 16 repetitions of the MAC, plus an
// optional 6-byte SecureOn password.
func buildMagicPacket(mac, secureOn []byte) []byte {
	pkt := make([]byte, 0, 6+16*6+len(secureOn))
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xFF)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, mac...)
	}
	return append(pkt, secureOn...)
}

func prepareWOL(ctx context.Context, cl *call) (runner, error) {
	var req wolRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	mac, err := parseMAC(req.MAC)
	if err != nil {
		return nil, httpx.BadRequest(err.Error())
	}
	var secureOn []byte
	if strings.TrimSpace(req.SecureOn) != "" {
		if secureOn, err = parseMAC(req.SecureOn); err != nil {
			return nil, httpx.BadRequest("invalid SecureOn password (expected 6 bytes, e.g. aa:bb:cc:dd:ee:ff)")
		}
	}
	if req.Port == 0 {
		req.Port = 9
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, httpx.BadRequest("invalid port")
	}
	req.Count = clampInt(orDefault(req.Count, 1), 1, 20)
	bcast := strings.TrimSpace(req.Broadcast)
	if bcast == "" {
		bcast = "255.255.255.255"
	}
	cl.target = formatMAC(mac)
	if req.ViaConnectionID != "" {
		if ip := net.ParseIP(bcast); ip == nil || ip.To4() == nil {
			return nil, httpx.BadRequest("the broadcast address must be an IPv4 address when sending from an SSH host")
		}
		if err := cl.checkSSHConnection(ctx, req.ViaConnectionID); err != nil {
			return nil, err
		}
		cl.details = map[string]any{"broadcast": bcast, "port": req.Port, "via": req.ViaConnectionID}
		return func(ctx context.Context, out *sink) error {
			return runWOLViaSSH(ctx, cl, &req, mac, secureOn, bcast, out)
		}, nil
	}
	if net.ParseIP(bcast) == nil {
		// Allow a host name (e.g. a subnet-directed broadcast name); resolve it now for a synchronous error.
		ip, rerr := resolveOne(ctx, bcast, false)
		if rerr != nil {
			return nil, httpx.BadRequest("invalid broadcast address")
		}
		bcast = ip.String()
	}
	cl.details = map[string]any{"broadcast": bcast, "port": req.Port}
	return func(ctx context.Context, out *sink) error {
		return runWOL(ctx, &req, mac, secureOn, bcast, out)
	}, nil
}

func runWOL(ctx context.Context, req *wolRequest, mac, secureOn []byte, bcast string, out *sink) error {
	pkt := buildMagicPacket(mac, secureOn)
	conn, err := net.Dial("udp", net.JoinHostPort(bcast, strconv.Itoa(req.Port)))
	if err != nil {
		return fmt.Errorf("open UDP socket: %w", err)
	}
	defer conn.Close()
	sent := 0
	for i := 0; i < req.Count; i++ {
		if i > 0 && !sleep(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
		if _, werr := conn.Write(pkt); werr != nil {
			return fmt.Errorf("send magic packet: %w", werr)
		}
		sent++
	}
	out.emitNow(row{
		"kind": "summary", "mac": formatMAC(mac), "broadcast": bcast, "port": req.Port,
		"packets": sent, "bytes": len(pkt), "secureOn": len(secureOn) > 0,
	})
	return nil
}

// runWOLViaSSH sends the magic packet from a saved SSH host (waking a machine on that host's subnet): wakeonlan when
// installed (and no SecureOn is needed), else a python3 / python one-liner with a broadcast UDP socket.
func runWOLViaSSH(ctx context.Context, cl *call, req *wolRequest, mac, secureOn []byte, bcast string, out *sink) error {
	client, release, err := cl.sshClientFor(ctx, req.ViaConnectionID)
	if err != nil {
		return err
	}
	defer release()
	macHex, secHex := hex.EncodeToString(mac), hex.EncodeToString(secureOn)
	py := `import socket,sys
m=bytes.fromhex(sys.argv[1]);p=b"\xff"*6+m*16+bytes.fromhex(sys.argv[4])
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.setsockopt(socket.SOL_SOCKET,socket.SO_BROADCAST,1)
for _ in range(int(sys.argv[5])): s.sendto(p,(sys.argv[2],int(sys.argv[3])))`
	args := fmt.Sprintf("%s %s %d %s %d", macHex, shellQuote(bcast), req.Port, shellQuote(secHex), req.Count)
	script := ""
	if len(secureOn) == 0 && req.Count == 1 {
		script = fmt.Sprintf("if command -v wakeonlan >/dev/null 2>&1; then exec wakeonlan -i %s -p %d %s; fi; ", shellQuote(bcast), req.Port, formatMAC(mac))
	}
	script += fmt.Sprintf(`for P in python3 python; do if command -v $P >/dev/null 2>&1; then exec $P -c %s %s; fi; done; echo "neither wakeonlan nor python3 is available on this host" >&2; exit 127`, shellQuote(py), args)
	out.emitNow(row{"kind": "info", "message": "Sending from " + client.Conn.Name})
	var lastErr string
	code, err := streamExec(ctx, client, shScript(script), func(text string, isErr bool) {
		if t := strings.TrimSpace(text); t != "" {
			if isErr {
				lastErr = t
			}
			out.add(row{"kind": "info", "message": t})
		}
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New(orString(lastErr, fmt.Sprintf("remote sender failed (exit %d)", code)))
	}
	out.emitNow(row{
		"kind": "summary", "mac": formatMAC(mac), "broadcast": bcast, "port": req.Port, "packets": req.Count,
		"bytes": 102 + len(secureOn), "secureOn": len(secureOn) > 0, "via": client.Conn.Name,
	})
	return nil
}

func formatMAC(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(parts, ":")
}
