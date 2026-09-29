package tools

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	g "github.com/gosnmp/gosnmp"

	"github.com/termstead/termstead/internal/httpx"
)

type snmpRequest struct {
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Version   string   `json:"version"`   // "1" | "2c" | "3"
	Operation string   `json:"operation"` // get | getnext | walk | bulkwalk
	Community string   `json:"community"`
	OIDs      []string `json:"oids"`
	OID       string   `json:"oid"` // one OID or several separated by spaces / commas
	TimeoutMs int      `json:"timeoutMs"`
	Retries   int      `json:"retries"`

	// SNMPv3 (USM)
	SecLevel    string `json:"secLevel"` // noAuthNoPriv | authNoPriv | authPriv
	Username    string `json:"username"`
	AuthProto   string `json:"authProto"` // MD5 | SHA | SHA224 | SHA256 | SHA384 | SHA512
	AuthPass    string `json:"authPass"`
	PrivProto   string `json:"privProto"` // DES | AES | AES192 | AES256 | AES192C | AES256C
	PrivPass    string `json:"privPass"`
	ContextName string `json:"contextName"`
}

// maxSNMPVars bounds a walk (a full-tree walk of a big switch can return hundreds of thousands of variables).
const maxSNMPVars = 50000

var errWalkLimit = errors.New("walk limit reached")

func prepareSNMP(ctx context.Context, cl *call) (runner, error) {
	var req snmpRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	req.Host = strings.Trim(strings.TrimSpace(req.Host), "[]")
	if req.Host == "" {
		return nil, httpx.BadRequest("host is required")
	}
	if _, err := safeHostArg(req.Host); err != nil {
		return nil, err
	}
	if req.Port == 0 {
		req.Port = 161
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, httpx.BadRequest("port must be between 1 and 65535")
	}
	op := strings.ToLower(strings.TrimSpace(req.Operation))
	if op == "" {
		op = "get"
	}
	switch op {
	case "get", "getnext", "walk", "bulkwalk":
	default:
		return nil, httpx.BadRequest("unsupported operation: " + op)
	}
	req.Operation = op
	raw := append([]string(nil), req.OIDs...)
	raw = append(raw, strings.FieldsFunc(req.OID, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })...)
	var oids []string
	for _, o := range raw {
		if strings.TrimSpace(o) == "" {
			continue
		}
		num, ok := resolveOIDName(o)
		if !ok {
			return nil, httpx.BadRequest("unknown OID or MIB name: " + strings.TrimSpace(o))
		}
		oids = append(oids, "."+num)
	}
	if len(oids) > 64 {
		return nil, httpx.BadRequest("too many OIDs (max 64)")
	}
	if len(oids) == 0 {
		if op == "walk" || op == "bulkwalk" {
			oids = []string{".1.3.6.1.2.1.1"} // system subtree
		} else {
			oids = []string{".1.3.6.1.2.1.1.1.0"} // sysDescr.0
		}
	}
	client, err := buildSNMP(&req)
	if err != nil {
		return nil, err
	}
	cl.target = req.Host
	cl.details = map[string]any{"operation": op, "version": snmpVersionLabel(client.Version)}
	guard := cl.guard
	return func(ctx context.Context, out *sink) error { return runSNMP(ctx, guard, &req, client, oids, out) }, nil
}

func runSNMP(ctx context.Context, guard *netGuard, req *snmpRequest, client *g.GoSNMP, oids []string, out *sink) error {
	ip, err := guard.resolve(ctx, req.Host, false)
	if err != nil {
		return err
	}
	client.Target = ip.String()
	client.Context = ctx
	if err := client.Connect(); err != nil {
		return fmt.Errorf("SNMP connect failed: %w", err)
	}
	defer client.Conn.Close()

	op := req.Operation
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("SNMP %s %s (v%s) %s", strings.ToUpper(op), net.JoinHostPort(req.Host, fmt.Sprint(req.Port)), snmpVersionLabel(client.Version), strings.Join(oids, " "))})
	start := time.Now()
	count := 0
	truncated := false

	switch op {
	case "get", "getnext":
		fn := client.Get
		if op == "getnext" {
			fn = client.GetNext
		}
		pkt, err := fn(oids)
		if err != nil {
			return snmpErr(ctx, err)
		}
		if pkt.Error != g.NoError {
			out.add(row{"kind": "info", "message": fmt.Sprintf("agent error: %v (index %d)", pkt.Error, pkt.ErrorIndex)})
		}
		for _, v := range pkt.Variables {
			out.add(pduRow(v))
			count++
		}
	case "walk", "bulkwalk":
		walk := client.Walk
		if op == "bulkwalk" && client.Version != g.Version1 {
			walk = client.BulkWalk
		}
		for _, root := range oids {
			err := walk(root, func(v g.SnmpPDU) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if count >= maxSNMPVars {
					return errWalkLimit
				}
				out.add(pduRow(v))
				count++
				return nil
			})
			if errors.Is(err, errWalkLimit) {
				truncated = true
				out.add(row{"kind": "info", "message": fmt.Sprintf("Stopped after %d variables; walk a narrower subtree", maxSNMPVars)})
				break
			}
			if err != nil {
				return snmpErr(ctx, err)
			}
		}
	}
	out.emitNow(row{"kind": "summary", "host": req.Host, "operation": op, "count": count, "truncated": truncated, "rttMs": ms(time.Since(start))})
	return nil
}

func buildSNMP(req *snmpRequest) (*g.GoSNMP, error) {
	timeout := time.Duration(clampInt(orDefault(req.TimeoutMs, 3000), 200, 30000)) * time.Millisecond
	retries := clampInt(req.Retries, 0, 5)
	if req.Retries == 0 {
		retries = 1
	}
	client := &g.GoSNMP{
		Target:         req.Host,
		Port:           uint16(req.Port),
		Transport:      "udp",
		Timeout:        timeout,
		Retries:        retries,
		MaxOids:        g.MaxOids,
		MaxRepetitions: 25,
	}
	switch strings.ToLower(strings.TrimSpace(req.Version)) {
	case "", "2c", "2", "v2c":
		client.Version = g.Version2c
		client.Community = orString(req.Community, "public")
	case "1", "v1":
		client.Version = g.Version1
		client.Community = orString(req.Community, "public")
	case "3", "v3":
		client.Version = g.Version3
		if err := configureV3(client, req); err != nil {
			return nil, err
		}
	default:
		return nil, httpx.BadRequest("unsupported SNMP version: " + req.Version)
	}
	return client, nil
}

func configureV3(client *g.GoSNMP, req *snmpRequest) error {
	if strings.TrimSpace(req.Username) == "" {
		return httpx.BadRequest("SNMPv3 needs a user name")
	}
	client.SecurityModel = g.UserSecurityModel
	client.ContextName = req.ContextName
	usm := &g.UsmSecurityParameters{UserName: req.Username}
	switch strings.ToLower(strings.TrimSpace(req.SecLevel)) {
	case "", "noauthnopriv":
		client.MsgFlags = g.NoAuthNoPriv
	case "authnopriv":
		client.MsgFlags = g.AuthNoPriv
	case "authpriv":
		client.MsgFlags = g.AuthPriv
	default:
		return httpx.BadRequest("invalid secLevel")
	}
	if client.MsgFlags != g.NoAuthNoPriv {
		ap, ok := snmpAuthProtos[strings.ToUpper(req.AuthProto)]
		if !ok {
			return httpx.BadRequest("invalid authProto")
		}
		if len(req.AuthPass) < 8 {
			return httpx.BadRequest("the SNMPv3 authentication passphrase must have at least 8 characters")
		}
		usm.AuthenticationProtocol = ap
		usm.AuthenticationPassphrase = req.AuthPass
	}
	if client.MsgFlags == g.AuthPriv {
		pp, ok := snmpPrivProtos[strings.ToUpper(req.PrivProto)]
		if !ok {
			return httpx.BadRequest("invalid privProto")
		}
		if len(req.PrivPass) < 8 {
			return httpx.BadRequest("the SNMPv3 privacy passphrase must have at least 8 characters")
		}
		usm.PrivacyProtocol = pp
		usm.PrivacyPassphrase = req.PrivPass
	}
	client.SecurityParameters = usm
	return nil
}

var snmpAuthProtos = map[string]g.SnmpV3AuthProtocol{
	"MD5": g.MD5, "SHA": g.SHA, "SHA1": g.SHA, "SHA224": g.SHA224, "SHA256": g.SHA256, "SHA384": g.SHA384, "SHA512": g.SHA512,
}

var snmpPrivProtos = map[string]g.SnmpV3PrivProtocol{
	"DES": g.DES, "AES": g.AES, "AES128": g.AES, "AES192": g.AES192, "AES256": g.AES256, "AES192C": g.AES192C, "AES256C": g.AES256C,
}

// pduRow renders one SNMP variable binding into a row with a human value, the ASN.1 type and a MIB label.
func pduRow(v g.SnmpPDU) row {
	oid := strings.TrimPrefix(v.Name, ".")
	r := row{"kind": "var", "oid": oid, "type": snmpType(v.Type), "value": snmpValue(v)}
	if l := oidLabel(oid); l != "" {
		r["name"] = l
	}
	return r
}

func snmpValue(v g.SnmpPDU) any {
	switch v.Type {
	case g.OctetString:
		b, _ := v.Value.([]byte)
		if isPrintable(b) {
			return sanitizeText(string(b))
		}
		if len(b) == 6 { // most likely a MAC address (ifPhysAddress)
			return formatMAC(b)
		}
		return "0x" + hex.EncodeToString(b)
	case g.ObjectIdentifier:
		s := strings.TrimPrefix(fmt.Sprint(v.Value), ".")
		if l := oidLabel(s); l != "" {
			return s + " (" + l + ")"
		}
		return s
	case g.IPAddress:
		return v.Value
	case g.TimeTicks:
		ticks := g.ToBigInt(v.Value).Int64()
		return fmt.Sprintf("%d (%s)", ticks, time.Duration(ticks)*10*time.Millisecond)
	case g.Null, g.NoSuchObject, g.NoSuchInstance, g.EndOfMibView:
		return snmpType(v.Type)
	default:
		return fmt.Sprintf("%v", v.Value)
	}
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if (c < 0x20 || c > 0x7e) && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

func snmpType(t g.Asn1BER) string {
	switch t {
	case g.OctetString:
		return "OctetString"
	case g.Integer:
		return "Integer"
	case g.Counter32:
		return "Counter32"
	case g.Counter64:
		return "Counter64"
	case g.Gauge32:
		return "Gauge32"
	case g.TimeTicks:
		return "TimeTicks"
	case g.ObjectIdentifier:
		return "OID"
	case g.IPAddress:
		return "IPAddress"
	case g.Opaque:
		return "Opaque"
	case g.Null:
		return "Null"
	case g.NoSuchObject:
		return "NoSuchObject"
	case g.NoSuchInstance:
		return "NoSuchInstance"
	case g.EndOfMibView:
		return "EndOfMibView"
	default:
		return fmt.Sprintf("0x%02x", byte(t))
	}
}

func snmpVersionLabel(v g.SnmpVersion) string {
	switch v {
	case g.Version1:
		return "1"
	case g.Version2c:
		return "2c"
	case g.Version3:
		return "3"
	default:
		return "?"
	}
}

func snmpErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("SNMP request failed: %w", err)
}
