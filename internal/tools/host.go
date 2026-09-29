package tools

import (
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// interfaceInfo describes one local network interface (GET /api/tools/interfaces).
type interfaceInfo struct {
	Name  string   `json:"name"`
	MTU   int      `json:"mtu"`
	MAC   string   `json:"mac"`
	Flags []string `json:"flags"`
	Addrs []string `json:"addrs"`
	Up    bool     `json:"up"`
}

func (h *handler) interfaces(c *echo.Context) error {
	if err := h.gate(httpx.UserFrom(c), true); err != nil { // host inventory: admin-only in server mode
		return err
	}
	ifaces, err := gnet.InterfacesWithContext(c.Request().Context())
	if err != nil {
		return httpx.Internal(err)
	}
	out := make([]interfaceInfo, 0, len(ifaces))
	for _, ifi := range ifaces {
		addrs := make([]string, 0, len(ifi.Addrs))
		for _, a := range ifi.Addrs {
			addrs = append(addrs, a.Addr)
		}
		up := false
		flags := make([]string, 0, len(ifi.Flags)) // never null in JSON (gopsutil returns nil for some interfaces)
		for _, f := range ifi.Flags {
			flags = append(flags, f)
			if f == "up" {
				up = true
			}
		}
		out = append(out, interfaceInfo{Name: ifi.Name, MTU: ifi.MTU, MAC: ifi.HardwareAddr, Flags: flags, Addrs: addrs, Up: up})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return c.JSON(http.StatusOK, out)
}

// socketInfo is one local socket (GET /api/tools/listening; TOOL-6 MobaListPorts).
type socketInfo struct {
	Proto      string `json:"proto"`
	LocalAddr  string `json:"localAddr"`
	LocalPort  int    `json:"localPort"`
	RemoteAddr string `json:"remoteAddr,omitempty"`
	RemotePort int    `json:"remotePort,omitempty"`
	State      string `json:"state"`
	PID        int32  `json:"pid"`
	Process    string `json:"process,omitempty"`
	User       string `json:"user,omitempty"`
	Service    string `json:"service,omitempty"`
}

func (h *handler) listening(c *echo.Context) error {
	if err := h.gate(httpx.UserFrom(c), true); err != nil { // exposes host processes: admin-only in server mode
		return err
	}
	ctx := c.Request().Context()
	includeAll := c.QueryParam("all") == "1" || c.QueryParam("all") == "true"

	conns, err := gnet.ConnectionsWithContext(ctx, "inet")
	if err != nil {
		return httpx.Internal(err)
	}
	nameCache := map[int32]*process.Process{}
	procName := func(pid int32) (string, string) {
		if pid <= 0 {
			return "", ""
		}
		p, ok := nameCache[pid]
		if !ok {
			p, _ = process.NewProcessWithContext(ctx, pid)
			nameCache[pid] = p
		}
		if p == nil {
			return "", ""
		}
		name, _ := p.NameWithContext(ctx)
		user, _ := p.UsernameWithContext(ctx)
		return name, user
	}

	out := make([]socketInfo, 0, len(conns))
	for _, cn := range conns {
		udp := cn.Type == 2 // SOCK_DGRAM
		listening := cn.Status == "LISTEN" || (udp && cn.Raddr.IP == "" && cn.Laddr.Port != 0)
		if !listening && !includeAll {
			continue
		}
		if cn.Laddr.IP == "" && cn.Laddr.Port == 0 {
			continue
		}
		proto := "tcp"
		if udp {
			proto = "udp"
		}
		if cn.Family == 10 || cn.Family == 23 || cn.Family == 28 || cn.Family == 30 || strings.Contains(cn.Laddr.IP, ":") { // AF_INET6 (linux, windows, freebsd, darwin)
			proto += "6"
		}
		name, user := procName(cn.Pid)
		state := cn.Status
		if state == "" {
			state = "-"
		}
		out = append(out, socketInfo{
			Proto: proto, LocalAddr: cn.Laddr.IP, LocalPort: int(cn.Laddr.Port),
			RemoteAddr: cn.Raddr.IP, RemotePort: int(cn.Raddr.Port), State: state,
			PID: cn.Pid, Process: name, User: user, Service: serviceName(int(cn.Laddr.Port), udp),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LocalPort != out[j].LocalPort {
			return out[i].LocalPort < out[j].LocalPort
		}
		return out[i].Proto < out[j].Proto
	})
	return c.JSON(http.StatusOK, out)
}

type killRequest struct {
	PID    int32  `json:"pid"`
	Signal string `json:"signal"` // TERM (default) | KILL
}

func (h *handler) killListener(c *echo.Context) error {
	if err := h.gate(httpx.UserFrom(c), true); err != nil {
		return err
	}
	var req killRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.PID <= 1 {
		return httpx.BadRequest("invalid pid")
	}
	if int(req.PID) == os.Getpid() {
		return httpx.BadRequest("refusing to terminate AstraTerm itself")
	}
	sig := "TERM"
	switch strings.ToUpper(strings.TrimSpace(req.Signal)) {
	case "", "TERM", "SIGTERM", "15":
	case "KILL", "SIGKILL", "9":
		sig = "KILL"
	default:
		return httpx.BadRequest("signal must be TERM or KILL")
	}
	ctx := c.Request().Context()
	p, err := process.NewProcessWithContext(ctx, req.PID)
	if err != nil {
		return httpx.NotFound("process not found")
	}
	name, _ := p.NameWithContext(ctx)
	if sig == "KILL" {
		err = p.KillWithContext(ctx)
	} else {
		err = p.TerminateWithContext(ctx)
	}
	h.auditLog(c, "tools.listening.kill", strconv.Itoa(int(req.PID)), map[string]any{"signal": sig, "process": name, "ok": err == nil})
	if err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "not permitted") ||
			strings.Contains(strings.ToLower(err.Error()), "access is denied") {
			return httpx.Forbidden("not permitted to signal process " + strconv.Itoa(int(req.PID)) + " (" + name + ")")
		}
		return httpx.Internal(err)
	}
	return httpx.OK(c)
}
