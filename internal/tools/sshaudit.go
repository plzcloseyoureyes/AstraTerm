package tools

import (
	"bufio"
	"context"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

type sshAuditRequest struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	TimeoutMs int    `json:"timeoutMs"`
}

// rating classifies an algorithm. Order of severity: legacy (broken) < weak (deprecated) < ok < pq (quantum-safe).
const (
	rateLegacy  = "legacy"
	rateWeak    = "weak"
	rateOK      = "ok"
	ratePQ      = "pq"
	rateUnknown = "unknown"
)

func prepareSSHAudit(_ context.Context, cl *call) (runner, error) {
	var req sshAuditRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		return nil, httpx.BadRequest("host is required")
	}
	if hh, pp, err := net.SplitHostPort(req.Host); err == nil {
		req.Host = hh
		if req.Port == 0 {
			req.Port, _ = strconv.Atoi(pp)
		}
	}
	req.Host = strings.Trim(req.Host, "[]")
	if _, err := safeHostArg(req.Host); err != nil {
		return nil, err
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, httpx.BadRequest("port must be between 1 and 65535")
	}
	req.TimeoutMs = min(max(orDefault(req.TimeoutMs, 8000), 500), 30000)
	cl.target = net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	guard := cl.guard
	return func(ctx context.Context, out *sink) error { return runSSHAudit(ctx, guard, &req, out) }, nil
}

func runSSHAudit(ctx context.Context, guard *netGuard, req *sshAuditRequest, out *sink) error {
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	out.emitNow(row{"kind": "info", "message": "Connecting to " + addr})
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := guard.dialer(timeout).DialContext(dctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("connect failed: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // Stop aborts a slow server immediately
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	banner, kex, err := sshHandshake(conn)
	_ = conn.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("SSH audit failed: %w", err)
	}

	out.emitNow(row{"kind": "server", "banner": banner, "software": bannerSoftware(banner)})

	emitCategory(out, "kex", "Key exchange", kex.KexAlgos, rateKex)
	emitCategory(out, "hostkey", "Host key", kex.HostKeyAlgos, rateHostKey)
	emitCategory(out, "cipher", "Encryption (server→client)", kex.CiphersS2C, rateCipher)
	emitCategory(out, "mac", "MAC (server→client)", kex.MACsS2C, rateMAC)
	emitCategory(out, "compression", "Compression", kex.CompressionS2C, rateCompression)

	strictKex := containsAny(kex.KexAlgos, "kex-strict-s-v00@openssh.com")
	terrapin, reason := terrapinExposure(kex, strictKex)
	out.emitNow(row{"kind": "terrapin", "vulnerable": terrapin, "reason": reason, "strictKex": strictKex})

	for _, f := range auditFindings(kex, strictKex, terrapin) {
		out.add(f)
	}
	for _, hk := range hostKeyFingerprints(ctx, guard, addr, kex.HostKeyAlgos, timeout) {
		out.add(hk)
	}
	out.emitNow(row{
		"kind": "summary", "grade": overallGrade(kex, terrapin),
		"recommendedConfig": recommendedSSHDConfig(kex),
	})
	return nil
}

// hostKeyFingerprints completes one key exchange per offered host-key type (ed25519, RSA, ECDSA…) with x/crypto to
// capture the server's host keys; authentication is never attempted (the callback aborts right after the key is seen).
func hostKeyFingerprints(ctx context.Context, guard *netGuard, addr string, offered []string, timeout time.Duration) []row {
	type probe struct{ algo, label string }
	var probes []probe
	seen := map[string]bool{}
	for _, a := range offered {
		label := ""
		switch {
		case a == "ssh-ed25519":
			label = "ED25519"
		case a == "rsa-sha2-512" || a == "rsa-sha2-256" || a == "ssh-rsa":
			label = "RSA"
		case strings.HasPrefix(a, "ecdsa-sha2-nistp"):
			label = "ECDSA " + strings.TrimPrefix(a, "ecdsa-sha2-")
		default:
			continue
		}
		if !seen[label] {
			seen[label] = true
			probes = append(probes, probe{a, label})
		}
	}
	all, insecure := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	kexAlgos := append(slices.Clone(all.KeyExchanges), insecure.KeyExchanges...)
	ciphers := append(slices.Clone(all.Ciphers), insecure.Ciphers...)
	macs := append(slices.Clone(all.MACs), insecure.MACs...)
	errSeen := errors.New("host key captured")
	var rows []row
	for _, p := range probes {
		if ctx.Err() != nil {
			break
		}
		var key ssh.PublicKey
		cfg := &ssh.ClientConfig{
			User:              "astraterm-audit",
			HostKeyAlgorithms: []string{p.algo},
			Timeout:           timeout,
			HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
				key = k
				return errSeen
			},
		}
		cfg.KeyExchanges, cfg.Ciphers, cfg.MACs = kexAlgos, ciphers, macs
		dctx, cancel := context.WithTimeout(ctx, timeout)
		c, err := guard.dialer(timeout).DialContext(dctx, "tcp", addr)
		cancel()
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(timeout))
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		_, _, _, _ = ssh.NewClientConn(c, addr, cfg)
		stop()
		_ = c.Close()
		if key == nil {
			continue
		}
		r := row{"kind": "hostkey", "type": p.label, "algorithm": p.algo, "sha256": ssh.FingerprintSHA256(key), "md5": "MD5:" + ssh.FingerprintLegacyMD5(key)}
		if ck, ok := key.(ssh.CryptoPublicKey); ok {
			if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok {
				bits := rk.N.BitLen()
				r["bits"] = bits
				if bits < 2048 {
					r["warning"] = fmt.Sprintf("RSA host key is only %d bits", bits)
				} else if bits < 3072 {
					r["note"] = "RSA keys below 3072 bits are considered weak by current guidance"
				}
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// kexInit holds the server's offered algorithm name-lists.
type kexInit struct {
	KexAlgos       []string
	HostKeyAlgos   []string
	CiphersC2S     []string
	CiphersS2C     []string
	MACsC2S        []string
	MACsS2C        []string
	CompressionC2S []string
	CompressionS2C []string
}

const (
	maxSSHPacket     = 256 * 1024
	maxSSHBannerLine = 1024 // RFC 4253 §4.2 allows 255 bytes; be lenient but bounded
)

// sshHandshake exchanges identification strings and reads the server's SSH_MSG_KEXINIT, returning the banner and the
// offered algorithms (a small parser run before x/crypto would take over, per RESEARCH CC-17).
func sshHandshake(conn net.Conn) (string, *kexInit, error) {
	r := bufio.NewReaderSize(conn, 4096)
	// Read the server identification line (RFC 4253 §4.2: other lines may precede it).
	var banner string
	for range 50 {
		line, err := readLineCRLF(r, maxSSHBannerLine)
		if err != nil {
			return "", nil, fmt.Errorf("reading banner: %w", err)
		}
		if strings.HasPrefix(line, "SSH-") {
			banner = sanitizeBanner([]byte(line))
			break
		}
	}
	if banner == "" {
		return "", nil, fmt.Errorf("no SSH identification string received")
	}
	if !strings.HasPrefix(banner, "SSH-2.0") && !strings.HasPrefix(banner, "SSH-1.99") {
		return banner, nil, fmt.Errorf("unsupported protocol: %s", banner)
	}
	if _, err := conn.Write([]byte("SSH-2.0-AstraTerm_audit\r\n")); err != nil {
		return banner, nil, err
	}
	// Read binary packets until we see KEXINIT (msg type 20), skipping others.
	for range 5 {
		payload, err := readPacket(r)
		if err != nil {
			return banner, nil, err
		}
		if len(payload) > 0 && payload[0] == 20 {
			kex, perr := parseKexInit(payload)
			return banner, kex, perr
		}
	}
	return banner, nil, fmt.Errorf("no KEXINIT received")
}

// readLineCRLF reads one line (without CR/LF) of at most limit bytes.
func readLineCRLF(r *bufio.Reader, limit int) (string, error) {
	var sb strings.Builder
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimRight(sb.String(), "\r"), nil
		}
		if sb.Len() >= limit {
			return "", errors.New("identification line too long")
		}
		sb.WriteByte(b)
	}
}

// readPacket reads one unencrypted SSH binary packet and returns its payload (RFC 4253 §6).
func readPacket(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	pktLen := binary.BigEndian.Uint32(lenBuf[:])
	if pktLen < 2 || pktLen > maxSSHPacket {
		return nil, fmt.Errorf("invalid packet length %d", pktLen)
	}
	buf := make([]byte, pktLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	padLen := int(buf[0])
	if padLen+1 > len(buf) {
		return nil, fmt.Errorf("invalid padding")
	}
	return buf[1 : len(buf)-padLen], nil
}

// parseKexInit parses the 10 name-lists of an SSH_MSG_KEXINIT payload.
func parseKexInit(payload []byte) (*kexInit, error) {
	if len(payload) < 17 {
		return nil, fmt.Errorf("short KEXINIT")
	}
	p := payload[17:] // skip msg type (1) + cookie (16)
	lists := make([][]string, 0, 10)
	for range 10 {
		nl, rest, err := readNameList(p)
		if err != nil {
			return nil, err
		}
		lists = append(lists, nl)
		p = rest
	}
	return &kexInit{
		KexAlgos: lists[0], HostKeyAlgos: lists[1],
		CiphersC2S: lists[2], CiphersS2C: lists[3],
		MACsC2S: lists[4], MACsS2C: lists[5],
		CompressionC2S: lists[6], CompressionS2C: lists[7],
	}, nil
}

func readNameList(p []byte) ([]string, []byte, error) {
	if len(p) < 4 {
		return nil, nil, fmt.Errorf("truncated name-list")
	}
	n := binary.BigEndian.Uint32(p[:4])
	p = p[4:]
	if uint32(len(p)) < n {
		return nil, nil, fmt.Errorf("truncated name-list body")
	}
	s := string(p[:n])
	rest := p[n:]
	if s == "" {
		return nil, rest, nil
	}
	var out []string
	for name := range strings.SplitSeq(s, ",") {
		if name = sanitizeBanner([]byte(name)); name != "" && len(out) < 256 {
			out = append(out, name)
		}
	}
	return out, rest, nil
}

func emitCategory(out *sink, id, label string, algos []string, rate func(string) (string, string)) {
	items := make([]row, 0, len(algos))
	for _, a := range algos {
		if a == "" || strings.HasPrefix(a, "kex-strict-") || a == "ext-info-s" || a == "ext-info-c" {
			continue
		}
		rating, note := rate(a)
		item := row{"name": a, "rating": rating}
		if note != "" {
			item["note"] = note
		}
		items = append(items, item)
	}
	out.emitNow(row{"kind": "category", "id": id, "label": label, "algorithms": items})
}

// bannerSoftware extracts the software/version portion of an SSH identification string.
func bannerSoftware(banner string) string {
	parts := strings.SplitN(banner, "-", 3)
	if len(parts) == 3 {
		if i := strings.IndexByte(parts[2], ' '); i >= 0 {
			return parts[2][:i]
		}
		return parts[2]
	}
	return banner
}

func containsAny(list []string, want ...string) bool {
	set := map[string]bool{}
	for _, w := range want {
		set[w] = true
	}
	for _, l := range list {
		if set[l] {
			return true
		}
	}
	return false
}
