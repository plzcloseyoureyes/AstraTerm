package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// SSHInfo is the SSH connection card (SSH-38 display, MON-4): the transport's negotiated parameters plus a latency
// measured now, route and session details. A JSON superset of model.SSHConnInfo.
type SSHInfo struct {
	model.SSHConnInfo
	// ProbeMs is the round trip of a keepalive request sent for this call (−1: no answer within 5 s, a stall).
	ProbeMs         float64    `json:"probeMs"`
	RemoteAddr      string     `json:"remoteAddr,omitempty"`
	LocalAddr       string     `json:"localAddr,omitempty"`
	User            string     `json:"user,omitempty"`
	Host            string     `json:"host,omitempty"`
	Port            int        `json:"port,omitempty"`
	PQ              bool       `json:"pq"` // post-quantum hybrid key exchange (ML-KEM / sntrup)
	JumpHosts       []string   `json:"jumpHosts,omitempty"`
	Proxy           string     `json:"proxy,omitempty"`
	AuthMethod      string     `json:"authMethod,omitempty"`
	Compression     bool       `json:"compression"`
	AgentForwarding bool       `json:"agentForwarding"`
	X11Forwarding   bool       `json:"x11Forwarding"`
	KeepAliveSec    int        `json:"keepAliveSec"`
	ConnectedAt     *time.Time `json:"connectedAt,omitempty"`
	MeasuredAt      time.Time  `json:"measuredAt"`
	// HostCert: the server authenticated with an OpenSSH host certificate (host key algorithm *-cert-v01@openssh.com).
	HostCert bool `json:"hostCert"`
	// Session history and terminal traffic (reconnects, first connect, last drop and its reason, bytes of the terminal
	// stream: output received / input sent).
	sessionHistory
}

func (s *Service) sshInfo(ctx context.Context, t *target) (*SSHInfo, error) {
	if t.client == nil {
		return nil, httpx.BadRequest("not an SSH session")
	}
	c := t.client
	info := &SSHInfo{SSHConnInfo: c.Info(), MeasuredAt: time.Now().UTC()}
	info.ProbeMs = probeLatency(ctx, func() error {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		return err
	}, 5*time.Second)
	if a := c.RemoteAddr(); a != nil {
		info.RemoteAddr = a.String()
	}
	if a := c.LocalAddr(); a != nil {
		info.LocalAddr = a.String()
	}
	info.User = c.User()
	kex := strings.ToLower(info.Kex)
	info.PQ = strings.Contains(kex, "mlkem") || strings.Contains(kex, "sntrup")
	info.HostCert = strings.Contains(strings.ToLower(info.HostKeyAlgo), "-cert-v01@openssh.com")
	if conn := c.Conn; conn != nil {
		info.Host, info.Port = conn.Host, conn.Port
		if info.Port == 0 {
			info.Port = 22
		}
		o := conn.Options
		info.AuthMethod = conn.AuthMethod
		info.Compression = o.Bool("compression", false)
		info.AgentForwarding = o.Bool("agentForwarding", false)
		info.X11Forwarding = o.Bool("x11Forwarding", false)
		info.KeepAliveSec = o.Int("keepAliveSec", 30)
		info.JumpHosts = s.describeHops(ctx, t.user, o.Strings("jumpHosts"))
		if pc := strings.TrimSpace(o.String("proxyCommand", "")); pc != "" {
			info.Proxy = "ProxyCommand"
		} else if p := o.Map("proxy"); p != nil {
			typ, _ := p["type"].(string)
			host, _ := p["host"].(string)
			if typ != "" && typ != "none" && host != "" {
				port := ""
				switch v := p["port"].(type) {
				case float64:
					port = strconv.Itoa(int(v))
				case string:
					port = v
				}
				info.Proxy = strings.TrimSpace(fmt.Sprintf("%s %s", typ, joinHostPort(host, port)))
			}
		}
	}
	if t.sess != nil {
		info.ConnectedAt = t.sess.Info().ConnectedAt
		info.sessionHistory = s.sessionHistory(t.sess.ID)
	}
	return info, nil
}

func joinHostPort(host, port string) string {
	if port == "" {
		return host
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// describeHops renders jump hosts: saved connections by name (when visible to the user), ad-hoc specs as written.
func (s *Service) describeHops(ctx context.Context, user *model.User, hops []string) []string {
	var out []string
	for _, h := range hops {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		label := h
		if s.d != nil && s.d.Store != nil && !strings.ContainsAny(h, "@:.") {
			if c, err := s.d.Store.Connections.Get(ctx, h); err == nil && app.Visible(user, c.OwnerID, c.Shared) {
				label = c.Name
				if c.Host != "" {
					label += " (" + c.Host + ")"
				}
			}
		}
		out = append(out, label)
	}
	return out
}

// probeLatency measures one round trip of fn in milliseconds (−1 on error or timeout).
func probeLatency(ctx context.Context, fn func() error, timeout time.Duration) float64 {
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			return -1
		}
		return round2(float64(time.Since(start).Microseconds()) / 1000)
	case <-t.C:
		return -1
	case <-ctx.Done():
		return -1
	}
}
