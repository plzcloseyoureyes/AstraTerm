package tools

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

type dnsRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`     // A, AAAA, CNAME, MX, NS, TXT, SOA, SRV, CAA, PTR, ANY, …
	Resolver string `json:"resolver"` // host or host:port; default the system resolver
	DoT      bool   `json:"dot"`      // DNS over TLS (port 853)
	TCP      bool   `json:"tcp"`
	DNSSEC   bool   `json:"dnssec"` // set the DO bit (show RRSIGs)
}

// dnsTypes maps the UI names to miekg/dns query types.
var dnsTypes = map[string]uint16{
	"A": dns.TypeA, "AAAA": dns.TypeAAAA, "CNAME": dns.TypeCNAME, "MX": dns.TypeMX, "NS": dns.TypeNS,
	"TXT": dns.TypeTXT, "SOA": dns.TypeSOA, "SRV": dns.TypeSRV, "CAA": dns.TypeCAA, "PTR": dns.TypePTR,
	"ANY": dns.TypeANY, "DNSKEY": dns.TypeDNSKEY, "DS": dns.TypeDS, "NAPTR": dns.TypeNAPTR, "SVCB": dns.TypeSVCB,
	"HTTPS": dns.TypeHTTPS, "TLSA": dns.TypeTLSA, "SSHFP": dns.TypeSSHFP,
}

func prepareDNS(ctx context.Context, cl *call) (runner, error) {
	var req dnsRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return nil, httpx.BadRequest("name is required")
	}
	if len(req.Name) > 253 || strings.ContainsAny(req.Name, " \t\r\n") {
		return nil, httpx.BadRequest("invalid name")
	}
	qtypeName := strings.ToUpper(strings.TrimSpace(req.Type))
	if qtypeName == "" {
		qtypeName = "A"
	}
	qtype, ok := dnsTypes[qtypeName]
	if !ok {
		return nil, httpx.BadRequest("unsupported record type: " + qtypeName)
	}
	qname := req.Name
	if qtype == dns.TypePTR {
		if arpa, err := dns.ReverseAddr(req.Name); err == nil {
			qname = arpa
		}
	}
	qname = dns.Fqdn(qname)
	if _, ok := dns.IsDomainName(qname); !ok {
		return nil, httpx.BadRequest("invalid name")
	}
	server, err := resolverAddr(req.Resolver, req.DoT)
	if err != nil {
		return nil, err
	}
	cl.target = req.Name
	guard := cl.guard
	return func(ctx context.Context, out *sink) error {
		return runDNS(ctx, guard, &req, qtypeName, qtype, qname, server, out)
	}, nil
}

func runDNS(ctx context.Context, guard *netGuard, req *dnsRequest, qtypeName string, qtype uint16, qname, server string, out *sink) error {
	client := &dns.Client{Timeout: 5 * time.Second, Dialer: guard.dialer(5 * time.Second)}
	switch {
	case req.DoT:
		client.Net = "tcp-tls"
		host, _, _ := net.SplitHostPort(server)
		client.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	case req.TCP:
		client.Net = "tcp"
	}
	m := new(dns.Msg)
	m.SetQuestion(qname, qtype)
	m.RecursionDesired = true
	if req.DNSSEC {
		m.SetEdns0(4096, true)
	}

	transport := "UDP"
	switch {
	case req.DoT:
		transport = "TLS"
	case req.TCP:
		transport = "TCP"
	}
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("Querying %s %s @ %s (%s)", qtypeName, req.Name, server, transport)})
	resp, rtt, err := client.ExchangeContext(ctx, m, server)
	if err == nil && resp != nil && resp.Truncated && client.Net == "" {
		// Retry over TCP like dig does when the UDP answer was truncated.
		out.add(row{"kind": "info", "message": "Answer truncated; retrying over TCP"})
		client.Net = "tcp"
		resp, rtt, err = client.ExchangeContext(ctx, m, server)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("query failed: %w", err)
	}

	for _, rr := range resp.Answer {
		out.add(rrRow("answer", rr))
	}
	for _, rr := range resp.Ns {
		out.add(rrRow("authority", rr))
	}
	for _, rr := range resp.Extra {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		out.add(rrRow("additional", rr))
	}
	out.emitNow(row{
		"kind": "summary", "server": server, "transport": transport, "rcode": dns.RcodeToString[resp.Rcode],
		"rttMs": ms(rtt), "answers": len(resp.Answer), "authoritative": resp.Authoritative, "truncated": resp.Truncated,
		"authenticated": resp.AuthenticatedData,
	})
	return nil
}

// rrRow renders a resource record into a structured row (name, type, ttl, and the record-specific data string).
func rrRow(section string, rr dns.RR) row {
	hdr := rr.Header()
	full := rr.String()
	// The value is everything after the header ("name ttl class type" prefix) in the presentation form.
	data := full
	if parts := strings.SplitN(full, "\t", 5); len(parts) == 5 {
		data = parts[4]
	}
	return row{
		"kind": "record", "section": section, "name": hdr.Name, "ttl": int(hdr.Ttl),
		"type": dns.TypeToString[hdr.Rrtype], "data": strings.TrimSpace(data),
	}
}

// resolverAddr normalizes the resolver into host:port. An empty resolver uses the first system nameserver (falling
// back to 1.1.1.1 where the OS has no resolv.conf, e.g. Windows). DoT defaults to port 853, plain DNS to 53.
func resolverAddr(resolver string, dot bool) (string, error) {
	defPort := ternary(dot, "853", "53")
	resolver = strings.TrimSpace(resolver)
	if resolver == "" {
		if !dot {
			if s := systemResolver(); s != "" {
				return net.JoinHostPort(s, defPort), nil
			}
		}
		return net.JoinHostPort("1.1.1.1", defPort), nil
	}
	if strings.ContainsAny(resolver, " /\\@") {
		return "", httpx.BadRequest("invalid resolver")
	}
	if host, port, err := net.SplitHostPort(resolver); err == nil {
		if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 || host == "" {
			return "", httpx.BadRequest("invalid resolver port")
		}
		return net.JoinHostPort(host, port), nil
	}
	return net.JoinHostPort(strings.Trim(resolver, "[]"), defPort), nil
}

// systemResolver returns the first nameserver from the OS resolver config, or "".
func systemResolver() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(conf.Servers) == 0 {
		return ""
	}
	return conf.Servers[0]
}
