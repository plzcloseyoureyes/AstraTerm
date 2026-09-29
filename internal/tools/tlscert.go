package tools

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // certificate fingerprints, not security
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/httpx"
)

type tlsCertRequest struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ServerName    string `json:"serverName"` // SNI override
	StartTLS      string `json:"startTls"`   // "", "smtp", "imap", "pop3", "ftp", "postgres", "ldap"
	TimeoutMs     int    `json:"timeoutMs"`
	ProbeVersions *bool  `json:"probeVersions"` // probe TLS 1.0–1.3 support separately (default true)
}

var startTLSPorts = map[string]int{"smtp": 25, "imap": 143, "pop3": 110, "ftp": 21, "postgres": 5432, "ldap": 389}

func prepareTLSCert(_ context.Context, cl *call) (runner, error) {
	var req tlsCertRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	host := strings.TrimSpace(req.Host)
	if host == "" {
		return nil, httpx.BadRequest("host is required")
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	// Allow a host:port in the host field.
	if h2, p2, err := net.SplitHostPort(host); err == nil {
		host = h2
		if req.Port == 0 {
			req.Port, _ = strconv.Atoi(p2)
		}
	}
	host = strings.Trim(host, "[]")
	if _, err := safeHostArg(host); err != nil {
		return nil, err
	}
	req.StartTLS = strings.ToLower(strings.TrimSpace(req.StartTLS))
	if req.StartTLS != "" {
		if _, ok := startTLSPorts[req.StartTLS]; !ok {
			return nil, httpx.BadRequest("unsupported STARTTLS protocol: " + req.StartTLS)
		}
	}
	if req.Port == 0 {
		req.Port = 443
		if p, ok := startTLSPorts[req.StartTLS]; ok {
			req.Port = p
		}
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, httpx.BadRequest("port must be between 1 and 65535")
	}
	req.ServerName = strings.TrimSpace(req.ServerName)
	if req.ServerName != "" {
		if _, err := safeHostArg(req.ServerName); err != nil {
			return nil, httpx.BadRequest("invalid server name")
		}
	}
	req.Host = host
	req.TimeoutMs = clampInt(orDefault(req.TimeoutMs, 10000), 500, 60000)
	cl.target = net.JoinHostPort(host, strconv.Itoa(req.Port))
	guard := cl.guard
	return func(ctx context.Context, out *sink) error { return runTLSCert(ctx, guard, &req, out) }, nil
}

// inspectCipherSuites lets the inspector talk to legacy servers too (the negotiated suite is reported, not trusted).
func inspectCipherSuites() []uint16 {
	var ids []uint16
	for _, s := range tls.CipherSuites() {
		ids = append(ids, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		ids = append(ids, s.ID)
	}
	return ids
}

// dialTLS connects, runs the STARTTLS prelude when requested and performs a handshake limited to [minV, maxV].
func dialTLS(ctx context.Context, guard *netGuard, req *tlsCertRequest, sni string, minV, maxV uint16) (*tls.Conn, error) {
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := guard.dialer(timeout).DialContext(dctx, "tcp", net.JoinHostPort(req.Host, strconv.Itoa(req.Port)))
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	_ = raw.SetDeadline(time.Now().Add(timeout))
	if req.StartTLS != "" {
		if err := doStartTLS(raw, req.StartTLS, sni); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("STARTTLS failed: %w", err)
		}
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, //nolint:gosec // inspection: the chain is verified separately and reported
		MinVersion:         minV,
		MaxVersion:         maxV,
		CipherSuites:       inspectCipherSuites(),
	})
	if err := conn.HandshakeContext(dctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func runTLSCert(ctx context.Context, guard *netGuard, req *tlsCertRequest, out *sink) error {
	sni := req.ServerName
	if sni == "" && net.ParseIP(req.Host) == nil {
		sni = req.Host
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	prelude := ""
	if req.StartTLS != "" {
		prelude = " via " + strings.ToUpper(req.StartTLS) + " STARTTLS"
	}
	out.emitNow(row{"kind": "info", "message": "Connecting to " + addr + prelude + ternary(sni != "", " (SNI "+sni+")", " (no SNI)")})

	conn, err := dialTLS(ctx, guard, req, sni, tls.VersionTLS10, tls.VersionTLS13)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("TLS handshake failed: %w", err)
	}
	st := conn.ConnectionState()
	_ = conn.Close()

	// Verify the chain independently (the handshake skips verification so a misconfigured server can still be
	// inspected) and report the result rather than failing.
	verifyName := req.ServerName
	if verifyName == "" {
		verifyName = req.Host
	}
	verifyErr := verifyChain(st, verifyName)
	suite := tls.CipherSuiteName(st.CipherSuite)
	insecureSuite := false
	for _, s := range tls.InsecureCipherSuites() {
		if s.ID == st.CipherSuite {
			insecureSuite = true
		}
	}
	out.emitNow(row{
		"kind": "connection", "host": req.Host, "port": req.Port, "serverName": sni,
		"version": tlsVersionName(st.Version), "cipher": suite, "weakCipher": insecureSuite,
		"alpn": st.NegotiatedProtocol, "ocspStapled": len(st.OCSPResponse) > 0, "scts": len(st.SignedCertificateTimestamps),
		"verified": verifyErr == nil, "verifyError": errString(verifyErr),
	})

	now := time.Now()
	for i, cert := range st.PeerCertificates {
		out.add(certRow(i, cert, now))
	}
	if len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		days := int(time.Until(leaf.NotAfter).Hours() / 24)
		out.emitNow(row{
			"kind": "summary", "subject": leaf.Subject.CommonName, "sans": leaf.DNSNames,
			"notBefore": leaf.NotBefore.UTC().Format(time.RFC3339), "notAfter": leaf.NotAfter.UTC().Format(time.RFC3339),
			"daysRemaining": days, "expired": now.After(leaf.NotAfter), "chainLength": len(st.PeerCertificates),
			"hostnameMatch": leaf.VerifyHostname(verifyName) == nil,
		})
	}

	if req.ProbeVersions == nil || *req.ProbeVersions {
		for _, v := range []uint16{tls.VersionTLS13, tls.VersionTLS12, tls.VersionTLS11, tls.VersionTLS10} {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r := row{"kind": "version", "version": tlsVersionName(v)}
			c, err := dialTLS(ctx, guard, req, sni, v, v)
			if err == nil {
				cs := c.ConnectionState()
				r["supported"], r["cipher"] = true, tls.CipherSuiteName(cs.CipherSuite)
				_ = c.Close()
			} else {
				r["supported"] = false
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					r["error"] = trimTLSError(err)
				}
			}
			r["deprecated"] = v < tls.VersionTLS12
			out.emitNow(r)
		}
	}
	return nil
}

func trimTLSError(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, "tls: "); i >= 0 {
		return s[i:]
	}
	return s
}

func certRow(index int, cert *x509.Certificate, now time.Time) row {
	sha1sum := sha1.Sum(cert.Raw) //nolint:gosec // fingerprint
	sha256sum := sha256.Sum256(cert.Raw)
	keyAlg, keyBits := publicKeyInfo(cert)
	r := row{
		"kind": "cert", "index": index,
		"subject": cert.Subject.String(), "issuer": cert.Issuer.String(),
		"commonName": cert.Subject.CommonName, "sans": cert.DNSNames, "ipSans": ipStrings(cert.IPAddresses),
		"serial":     cert.SerialNumber.Text(16),
		"notBefore":  cert.NotBefore.UTC().Format(time.RFC3339),
		"notAfter":   cert.NotAfter.UTC().Format(time.RFC3339),
		"expired":    now.After(cert.NotAfter),
		"notYet":     now.Before(cert.NotBefore),
		"selfSigned": cert.Subject.String() == cert.Issuer.String(),
		"isCA":       cert.IsCA,
		"sigAlg":     cert.SignatureAlgorithm.String(),
		"keyAlg":     keyAlg,
		"keyBits":    keyBits,
		"sha1":       colonHex(sha1sum[:]),
		"sha256":     colonHex(sha256sum[:]),
	}
	var weak []string
	switch cert.SignatureAlgorithm {
	case x509.SHA1WithRSA, x509.ECDSAWithSHA1, x509.MD5WithRSA, x509.DSAWithSHA1: //nolint:staticcheck // detection
		weak = append(weak, "weak signature ("+cert.SignatureAlgorithm.String()+")")
	}
	if keyAlg == "RSA" && keyBits < 2048 {
		weak = append(weak, fmt.Sprintf("short RSA key (%d bits)", keyBits))
	}
	if len(weak) > 0 {
		r["warnings"] = weak
	}
	return r
}

func publicKeyInfo(cert *x509.Certificate) (string, int) {
	switch k := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return "RSA", k.N.BitLen()
	case *ecdsa.PublicKey:
		return "ECDSA", k.Curve.Params().BitSize
	case ed25519.PublicKey:
		return "Ed25519", 256
	default:
		return cert.PublicKeyAlgorithm.String(), 0
	}
}

func verifyChain(st tls.ConnectionState, name string) error {
	if len(st.PeerCertificates) == 0 {
		return fmt.Errorf("no certificates presented")
	}
	roots, _ := x509.SystemCertPool()
	inter := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := st.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: inter})
	return err
}

// ---- STARTTLS preludes ---------------------------------------------------------------------------------------------

// replyReader reads line-oriented protocol replies with bounded line length and count.
type replyReader struct{ r *bufio.Reader }

func (rr replyReader) line() (string, error) {
	var sb strings.Builder
	for {
		b, err := rr.r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimRight(sb.String(), "\r"), nil
		}
		if sb.Len() >= 4096 {
			return "", errors.New("reply line too long")
		}
		sb.WriteByte(b)
	}
}

// codeReply reads an SMTP/FTP style reply ("250-…" continuation lines, "250 …" final) and returns the code and all
// lines.
func (rr replyReader) codeReply() (int, []string, error) {
	var lines []string
	for i := 0; i < 200; i++ {
		l, err := rr.line()
		if err != nil {
			return 0, lines, err
		}
		lines = append(lines, l)
		if len(l) >= 4 && l[3] == ' ' || len(l) == 3 {
			code, err := strconv.Atoi(l[:3])
			if err != nil {
				return 0, lines, fmt.Errorf("unexpected reply %q", l)
			}
			return code, lines, nil
		}
		if len(l) < 4 || l[3] != '-' {
			return 0, lines, fmt.Errorf("unexpected reply %q", l)
		}
	}
	return 0, lines, errors.New("reply too long")
}

// doStartTLS performs the plaintext prelude that upgrades a protocol connection to TLS (opportunistic TLS).
func doStartTLS(conn net.Conn, proto, sni string) error {
	rr := replyReader{bufio.NewReaderSize(conn, 4096)}
	write := func(s string) error { _, err := io.WriteString(conn, s); return err }
	expect := func(code int, want int, lines []string, err error) error {
		if err != nil {
			return err
		}
		if code != want {
			return fmt.Errorf("server replied %q", strings.Join(lines, " / "))
		}
		return nil
	}
	switch proto {
	case "smtp":
		code, lines, err := rr.codeReply() // 220 greeting (possibly multi-line)
		if err := expect(code, 220, lines, err); err != nil {
			return err
		}
		helo := orString(sni, "termstead.localdomain")
		if err := write("EHLO " + helo + "\r\n"); err != nil {
			return err
		}
		code, lines, err = rr.codeReply()
		if err := expect(code, 250, lines, err); err != nil {
			return err
		}
		if !strings.Contains(strings.ToUpper(strings.Join(lines, "\n")), "STARTTLS") {
			return errors.New("the server does not offer STARTTLS")
		}
		if err := write("STARTTLS\r\n"); err != nil {
			return err
		}
		code, lines, err = rr.codeReply()
		return expect(code, 220, lines, err)
	case "ftp":
		if _, _, err := rr.codeReply(); err != nil {
			return err
		}
		if err := write("AUTH TLS\r\n"); err != nil {
			return err
		}
		code, lines, err := rr.codeReply()
		return expect(code, 234, lines, err)
	case "imap":
		if _, err := rr.line(); err != nil { // "* OK …" greeting
			return err
		}
		if err := write("a1 STARTTLS\r\n"); err != nil {
			return err
		}
		for i := 0; i < 50; i++ {
			l, err := rr.line()
			if err != nil {
				return err
			}
			if strings.HasPrefix(l, "a1 ") {
				if !strings.HasPrefix(strings.ToUpper(l), "A1 OK") {
					return fmt.Errorf("server replied %q", l)
				}
				return nil
			}
		}
		return errors.New("no STARTTLS reply")
	case "pop3":
		if _, err := rr.line(); err != nil {
			return err
		}
		if err := write("STLS\r\n"); err != nil {
			return err
		}
		l, err := rr.line()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(l, "+OK") {
			return fmt.Errorf("server replied %q", l)
		}
		return nil
	case "postgres":
		// SSLRequest: length 8, code 80877103; the server answers 'S' (proceed) or 'N'.
		msg := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), 80877103)
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return err
		}
		if b[0] != 'S' {
			return errors.New("the server does not accept SSL connections")
		}
		return nil
	case "ldap":
		// ExtendedRequest StartTLS (OID 1.3.6.1.4.1.1466.20037), message ID 1.
		oid := "1.3.6.1.4.1.1466.20037"
		ext := append([]byte{0x80, byte(len(oid))}, oid...)
		op := append([]byte{0x77, byte(len(ext))}, ext...)
		body := append([]byte{0x02, 0x01, 0x01}, op...)
		msg := append([]byte{0x30, byte(len(body))}, body...)
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		return readLDAPExtendedResult(conn)
	default:
		return fmt.Errorf("unsupported STARTTLS protocol %q", proto)
	}
}

// readLDAPExtendedResult reads one LDAPMessage and checks the ExtendedResponse result code (0 = success).
func readLDAPExtendedResult(r io.Reader) error {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x30 {
		return errors.New("unexpected LDAP response")
	}
	n := int(hdr[1])
	if n&0x80 != 0 { // long form length
		k := n & 0x7f
		if k < 1 || k > 3 {
			return errors.New("unexpected LDAP response length")
		}
		lb := make([]byte, k)
		if _, err := io.ReadFull(r, lb); err != nil {
			return err
		}
		n = 0
		for _, b := range lb {
			n = n<<8 | int(b)
		}
	}
	if n > 64*1024 {
		return errors.New("LDAP response too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	// messageID INTEGER, then [APPLICATION 24] ExtendedResponse whose first element is resultCode ENUMERATED.
	for i := 0; i+2 < len(body); i++ {
		if body[i] == 0x0a && body[i+1] == 0x01 {
			if body[i+2] != 0 {
				return fmt.Errorf("the server refused StartTLS (result code %d)", body[i+2])
			}
			return nil
		}
	}
	return errors.New("unexpected LDAP response")
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionSSL30: //nolint:staticcheck // display only
		return "SSL 3.0"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func colonHex(b []byte) string {
	s := hex.EncodeToString(b)
	var sb strings.Builder
	for i := 0; i < len(s); i += 2 {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(s[i : i+2])
	}
	return strings.ToUpper(sb.String())
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}
