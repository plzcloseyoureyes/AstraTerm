package vnc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/vnc/anontls"
)

// The RFB client side of AstraTerm's Go-side security termination (SPEC §6.3, RESEARCH §3.12): AstraTerm connects to the
// VNC server, negotiates the protocol version, picks the strongest security type it implements (VeNCrypt with real
// TLS, Apple Remote Desktop, VNC Authentication, None), authenticates with vault credentials (prompting through the
// broker when missing or rejected), sends ClientInit and reads ServerInit. The browser (noVNC) is then offered RFB
// 3.8 with security type None and receives the server's ServerInit. Security types AstraTerm does not implement but
// noVNC does (RA2ne, Tight, XVP, MS-Logon II, …) are handed to noVNC unchanged ("pass-through").

// errCanceled is returned when the user dismissed a credentials or certificate prompt.
var errCanceled = errors.New("authentication canceled")

// authError is a SecurityResult failure (wrong password, account locked, …).
type authError struct {
	reason string
}

func (e *authError) Error() string {
	if e.reason != "" {
		return "authentication failed: " + e.reason
	}
	return "authentication failed"
}

// refusedError is a server that closed the handshake with a reason (e.g. TigerVNC's "Too many security failures").
type refusedError struct{ reason string }

func (e *refusedError) Error() string {
	if e.reason == "" {
		return "the VNC server refused the connection"
	}
	return "the VNC server refused the connection: " + e.reason
}

// unsupportedError reports that neither AstraTerm nor the browser can use any offered security type.
type unsupportedError struct{ offered string }

func (e *unsupportedError) Error() string {
	return "no supported security type (the server offers: " + e.offered + ")"
}

// retryError asks the caller to reconnect with a security choice excluded (e.g. anonymous TLS the server cannot
// negotiate with us). The policy already allowed what the next attempt may end up with; downgrade marks retries that
// give up encryption the server offered (reported in vnc-info).
type retryError struct {
	exclude   uint32
	cause     error
	downgrade bool
}

func (e *retryError) Error() string { return e.cause.Error() }
func (e *retryError) Unwrap() error { return e.cause }

// excludeAnonTLS is a pseudo security value used in handshakeConfig.exclude for the TLSNone/TLSVnc/TLSPlain family.
const excludeAnonTLS = 1 << 20

// credRequest describes the credentials a security type needs.
type credRequest struct {
	needUser bool   // Plain / ARD need a user name
	scheme   string // "VNC Authentication", "VeNCrypt Plain", "Apple Remote Desktop"
}

// credentialSource supplies credentials, prompting the user when needed.
type credentialSource interface {
	// available reports what is known without prompting.
	available() (hasUser, hasPassword bool)
	// get returns credentials for req (prompting when something is missing or was rejected).
	get(ctx context.Context, req credRequest) (username, password string, err error)
}

// certVerifier decides whether a VeNCrypt X.509 server certificate is trusted (system roots, saved fingerprint, or
// the user's answer). It returns how the certificate was trusted ("system", "saved", "accepted", "accepted-saved").
type certVerifier interface {
	verify(ctx context.Context, host string, port int, certs []*x509.Certificate) (string, error)
}

// handshakeConfig parameterizes clientHandshake.
type handshakeConfig struct {
	host             string
	port             int
	repeaterID       string
	shared           bool
	allowPassthrough bool
	policy           encryptionPolicy // effective encryption policy (connection option + confirmations)
	exclude          map[uint32]bool
	creds            credentialSource
	trust            certVerifier
	timeout          time.Duration // per read/write
	status           func(msg string)
	anonTLS          *anontls.Config // test hook (nil: defaults)
}

// tlsInfo describes the TLS layer of a VeNCrypt connection.
type tlsInfo struct {
	Version     string `json:"version"`
	CipherSuite string `json:"cipherSuite"`
	Anonymous   bool   `json:"anonymous"`
	Group       string `json:"group,omitempty"`
	// DHBits is the finite-field Diffie-Hellman modulus size of anonymous TLS (0 for ECDH); Weak marks groups below
	// 2048 bits, accepted only with the user's confirmation or options.encryption allow-weak / allow-unencrypted.
	DHBits int  `json:"dhBits,omitempty"`
	Weak   bool `json:"weak,omitempty"`
	// EncryptThenMAC reports RFC 7366 record protection (anonymous TLS with CBC suites).
	EncryptThenMAC bool   `json:"encryptThenMac,omitempty"`
	Subject        string `json:"subject,omitempty"`
	Issuer         string `json:"issuer,omitempty"`
	NotAfter       string `json:"notAfter,omitempty"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	Trust          string `json:"trust,omitempty"`
}

// passthrough carries the pre-authentication messages to replay to the browser.
type passthrough struct {
	version  rfbVersion
	security []byte // the security types message as received (3.7+) or the u32 type (3.3)
	offered  string
}

// handshakeResult is an authenticated upstream connection.
type handshakeResult struct {
	conn          net.Conn // RFB stream (inside TLS where applicable), deadlines cleared
	serverVersion string   // as sent by the server, e.g. "RFB 003.008"
	version       rfbVersion
	secType       uint32
	subType       uint32
	tls           *tlsInfo
	init          *serverInit  // termination mode
	pass          *passthrough // pass-through mode
	username      string       // credentials actually used (for remembering / saving)
	password      string
	usedPassword  bool
	policy        encryptionPolicy // effective encryption policy of the attempt (set by the caller)
	downgrade     string           // why the connection is weaker than what the server offered (set by the caller)
}

func (r *handshakeResult) securityName() string {
	if r.pass != nil {
		return "Browser-side authentication (" + r.pass.offered + ")"
	}
	name := securityTypeName(r.secType)
	if r.subType != 0 {
		name += " " + securityTypeName(r.subType)
	}
	return name
}

func (r *handshakeResult) encrypted() bool { return r.tls != nil }

// stepConn refreshes the I/O deadline before every read and write, so each protocol step gets the full timeout even
// when the user spent minutes in a prompt between two steps.
type stepConn struct {
	net.Conn
	timeout time.Duration
}

func (s *stepConn) Read(b []byte) (int, error) {
	_ = s.Conn.SetReadDeadline(time.Now().Add(s.timeout))
	return s.Conn.Read(b)
}

func (s *stepConn) Write(b []byte) (int, error) {
	_ = s.Conn.SetWriteDeadline(time.Now().Add(s.timeout))
	return s.Conn.Write(b)
}

type handshake struct {
	cfg  *handshakeConfig
	ctx  context.Context
	raw  net.Conn // TCP (or gateway channel)
	conn net.Conn // current stream (raw or TLS)
	rw   *stepConn
	res  *handshakeResult
}

func (h *handshake) status(msg string) {
	if h.cfg.status != nil {
		h.cfg.status(msg)
	}
}

func (h *handshake) setConn(c net.Conn) {
	h.conn = c
	h.rw = &stepConn{Conn: c, timeout: h.cfg.timeout}
}

func (h *handshake) write(b []byte) error {
	_, err := h.rw.Write(b)
	return err
}

// clientHandshake runs the RFB handshake with a VNC server over conn. On success the returned result's conn is
// positioned after ServerInit (termination mode) or after the security types message (pass-through mode). On error
// the caller closes conn.
func clientHandshake(ctx context.Context, conn net.Conn, cfg *handshakeConfig) (*handshakeResult, error) {
	if cfg.timeout <= 0 {
		cfg.timeout = 30 * time.Second
	}
	h := &handshake{cfg: cfg, ctx: ctx, raw: conn, res: &handshakeResult{}}
	h.setConn(conn)
	if err := h.negotiateVersion(); err != nil {
		return nil, err
	}
	if err := h.negotiateSecurity(); err != nil {
		return nil, err
	}
	if h.res.pass == nil {
		if err := h.write([]byte{boolByte(cfg.shared)}); err != nil { // ClientInit
			return nil, err
		}
		si, err := readServerInit(h.rw)
		if err != nil {
			return nil, fmt.Errorf("reading ServerInit: %w", err)
		}
		h.res.init = si
	}
	_ = h.conn.SetDeadline(time.Time{})
	h.res.conn = h.conn
	return h.res, nil
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func (h *handshake) negotiateVersion() error {
	h.status("Waiting for the VNC server")
	for attempt := 0; ; attempt++ {
		b, err := readFull(h.rw, versionLen)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New("the server closed the connection before the RFB greeting (not a VNC server, or it refused this client)")
			}
			return err
		}
		v, repeater, err := parseServerVersion(b)
		if err != nil {
			return err
		}
		if repeater {
			if attempt > 0 {
				return errors.New("the VNC repeater answered twice with its greeting")
			}
			if h.cfg.repeaterID == "" {
				return errRepeater
			}
			h.status("Asking the repeater for ID " + h.cfg.repeaterID)
			if err := h.write(repeaterMessage(h.cfg.repeaterID)); err != nil {
				return err
			}
			continue
		}
		h.res.serverVersion = strings.TrimSuffix(string(b), "\n")
		h.res.version = v
		return h.write(v.line())
	}
}

// repeaterMessage is the 250-byte UltraVNC repeater request ("ID:<id>" padded with NULs, like noVNC).
func repeaterMessage(id string) []byte {
	msg := make([]byte, 250)
	s := id
	if !strings.HasPrefix(strings.ToUpper(s), "ID:") {
		s = "ID:" + s
	}
	copy(msg, s)
	return msg
}

// Security types noVNC can negotiate itself (pass-through candidates).
var browserSecurityTypes = []byte{secNone, secVNCAuth, secRA2ne, secTight, secVeNCrypt, secXVP, secARD, secMSLogonII}

func (h *handshake) negotiateSecurity() error {
	h.status("Negotiating security")
	if h.res.version == rfb33 {
		t, err := readU32(h.rw)
		if err != nil {
			return err
		}
		if t == secInvalid {
			reason, _ := readReason(h.rw)
			return &refusedError{reason: reason}
		}
		if h.cfg.policy == encRequire {
			return &encryptionRequiredError{reason: "the server (RFB 3.3) only offers " + securityTypeName(t) + " without encryption"}
		}
		switch t {
		case secNone:
			h.res.secType = secNone
			return nil // RFB 3.3: no SecurityResult for None
		case secVNCAuth:
			h.res.secType = secVNCAuth
			if err := h.vncAuth(); err != nil {
				return err
			}
			return h.securityResult()
		}
		if h.cfg.allowPassthrough && t < 256 && slices.Contains(browserSecurityTypes, byte(t)) {
			h.res.pass = &passthrough{version: h.res.version, security: u32(t), offered: securityTypeName(t)}
			return nil
		}
		return &unsupportedError{offered: securityTypeName(t)}
	}

	n, err := readU8(h.rw)
	if err != nil {
		return err
	}
	if n == 0 {
		reason, _ := readReason(h.rw)
		return &refusedError{reason: reason}
	}
	types, err := readFull(h.rw, int(n))
	if err != nil {
		return err
	}
	choice := h.chooseType(types)
	if choice == secInvalid {
		if h.cfg.policy == encRequire {
			return &encryptionRequiredError{reason: "the server offers no encrypted security type (" + typeList(types) + ")"}
		}
		if h.cfg.allowPassthrough {
			for _, t := range types {
				if slices.Contains(browserSecurityTypes, t) {
					msg := append([]byte{n}, types...)
					h.res.pass = &passthrough{version: h.res.version, security: msg, offered: typeList(types)}
					return nil
				}
			}
		}
		return &unsupportedError{offered: typeList(types)}
	}
	if err := h.write([]byte{choice}); err != nil {
		return err
	}
	h.res.secType = uint32(choice)
	switch choice {
	case secNone:
		if h.res.version >= rfb38 {
			return h.securityResult()
		}
		return nil
	case secVNCAuth:
		if err := h.vncAuth(); err != nil {
			return err
		}
	case secARD:
		if err := h.ardAuth(); err != nil {
			return err
		}
	case secVeNCrypt:
		if err := h.veNCrypt(types); err != nil {
			return err
		}
	}
	return h.securityResult()
}

// chooseType picks the top-level security type: VeNCrypt (encryption) first, then Apple Remote Desktop when a user
// name is known (account login), VNC Authentication, Apple Remote Desktop, None. Without a stored password, None is
// preferred over types that would prompt. Policy `require` accepts VeNCrypt only.
func (h *handshake) chooseType(types []byte) byte {
	if h.cfg.policy == encRequire {
		if slices.Contains(types, secVeNCrypt) && !h.cfg.exclude[secVeNCrypt] {
			return secVeNCrypt
		}
		return secInvalid
	}
	hasUser, hasPass := h.cfg.creds.available()
	var order []byte
	order = append(order, secVeNCrypt)
	if !hasPass {
		order = append(order, secNone)
	}
	if hasUser {
		order = append(order, secARD)
	}
	order = append(order, secVNCAuth, secARD, secNone)
	for _, t := range order {
		if slices.Contains(types, t) && !h.cfg.exclude[uint32(t)] {
			return t
		}
	}
	return secInvalid
}

func (h *handshake) securityResult() error {
	status, err := readU32(h.rw)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return &authError{reason: "the server closed the connection"}
		}
		return err
	}
	if status == 0 {
		return nil
	}
	reason := ""
	if h.res.version >= rfb38 {
		reason, _ = readReason(h.rw)
	}
	if status == 2 && reason == "" {
		reason = "too many attempts"
	}
	return &authError{reason: reason}
}

// ---- authentication schemes ---------------------------------------------------------------------------------------

func (h *handshake) vncAuth() error {
	challenge, err := readFull(h.rw, 16)
	if err != nil {
		return err
	}
	_, pw, err := h.cfg.creds.get(h.ctx, credRequest{scheme: "VNC Authentication"})
	if err != nil {
		return err
	}
	h.res.password, h.res.usedPassword = pw, true
	h.status("Authenticating")
	resp, err := vncAuthResponse(pw, challenge)
	if err != nil {
		return err
	}
	return h.write(resp)
}

func (h *handshake) ardAuth() error {
	params, err := readARDParams(h.rw)
	if err != nil {
		return err
	}
	user, pw, err := h.cfg.creds.get(h.ctx, credRequest{needUser: true, scheme: "Apple Remote Desktop"})
	if err != nil {
		return err
	}
	h.res.username, h.res.password, h.res.usedPassword = user, pw, true
	h.status("Authenticating")
	resp, err := ardResponse(params, user, pw, nil)
	if err != nil {
		return err
	}
	return h.write(resp)
}

func (h *handshake) plainAuth(scheme string) error {
	user, pw, err := h.cfg.creds.get(h.ctx, credRequest{needUser: true, scheme: scheme})
	if err != nil {
		return err
	}
	h.res.username, h.res.password, h.res.usedPassword = user, pw, true
	h.status("Authenticating")
	msg := make([]byte, 0, 8+len(user)+len(pw))
	defer clear(msg[:cap(msg)])
	msg = append(msg, u32(uint32(len(user)))...)
	msg = append(msg, u32(uint32(len(pw)))...)
	msg = append(msg, user...)
	msg = append(msg, pw...)
	return h.write(msg)
}

// ---- VeNCrypt (version 0.2) ----------------------------------------------------------------------------------------

func isAnonTLS(t uint32) bool { return t == vcTLSNone || t == vcTLSVnc || t == vcTLSPlain }
func isX509(t uint32) bool    { return t == vcX509None || t == vcX509Vnc || t == vcX509Plain }

// chooseSubtype orders VeNCrypt subtypes: certificate TLS, then anonymous TLS, then unencrypted; within a TLS tier
// the user/password variant first when a user name is configured, the variant without authentication first when no
// password is stored. Without TLS, VNC Authentication (challenge-response) comes before Plain, which would send the
// password in clear text. Policy `require` stops after the TLS tiers.
func (h *handshake) chooseSubtype(subtypes []uint32) uint32 {
	hasUser, hasPass := h.cfg.creds.available()
	tier := func(plain, vnc, none uint32) []uint32 {
		var o []uint32
		if !hasPass {
			o = append(o, none)
		}
		if hasUser {
			o = append(o, plain)
		}
		return append(o, vnc, plain, none)
	}
	var order []uint32
	order = append(order, tier(vcX509Plain, vcX509Vnc, vcX509None)...)
	if !h.cfg.exclude[excludeAnonTLS] {
		order = append(order, tier(vcTLSPlain, vcTLSVnc, vcTLSNone)...)
	}
	if h.cfg.policy != encRequire {
		if !hasPass {
			order = append(order, secNone)
		}
		order = append(order, secVNCAuth, vcPlain, secNone)
	}
	for _, t := range order {
		if slices.Contains(subtypes, t) && !h.cfg.exclude[t] {
			return t
		}
	}
	return 0
}

// unencryptedAlternatives reports what the server offers besides anonymous TLS: unencrypted VeNCrypt subtypes, and
// other top-level security types AstraTerm (or the viewer) can use.
func (h *handshake) unencryptedAlternatives(subtypes []uint32, topTypes []byte) (inVeNCrypt, topLevel bool) {
	for _, t := range subtypes {
		if t == secNone || t == secVNCAuth || t == vcPlain {
			inVeNCrypt = true
		}
	}
	for _, alt := range topTypes {
		if alt != secVeNCrypt && !h.cfg.exclude[uint32(alt)] && (alt == secNone || alt == secVNCAuth || alt == secARD ||
			(h.cfg.allowPassthrough && slices.Contains(browserSecurityTypes, alt))) {
			topLevel = true
		}
	}
	return inVeNCrypt, topLevel
}

func (h *handshake) veNCrypt(topTypes []byte) error {
	ver, err := readFull(h.rw, 2)
	if err != nil {
		return err
	}
	if ver[0] != 0 || ver[1] < 2 {
		return h.retryWithout(secVeNCrypt, topTypes, fmt.Errorf("unsupported VeNCrypt version %d.%d", ver[0], ver[1]))
	}
	if err := h.write([]byte{0, 2}); err != nil {
		return err
	}
	ack, err := readU8(h.rw)
	if err != nil {
		return err
	}
	if ack != 0 {
		return h.retryWithout(secVeNCrypt, topTypes, errors.New("the server rejected VeNCrypt version 0.2"))
	}
	n, err := readU8(h.rw)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("the server offers no VeNCrypt subtypes")
	}
	raw, err := readFull(h.rw, 4*int(n))
	if err != nil {
		return err
	}
	subtypes := make([]uint32, n)
	names := make([]string, n)
	for i := range subtypes {
		subtypes[i] = uint32(raw[4*i])<<24 | uint32(raw[4*i+1])<<16 | uint32(raw[4*i+2])<<8 | uint32(raw[4*i+3])
		names[i] = securityTypeName(subtypes[i])
	}
	sub := h.chooseSubtype(subtypes)
	if sub == 0 {
		if h.cfg.policy == encRequire {
			return &encryptionRequiredError{reason: "the server's VeNCrypt offers no usable TLS subtype (" + strings.Join(names, ", ") + ")"}
		}
		return h.retryWithout(secVeNCrypt, topTypes, fmt.Errorf("no supported VeNCrypt subtype (the server offers: %s)",
			strings.Join(names, ", ")))
	}
	if sub == vcPlain && h.cfg.policy < encAllowUnencrypted {
		// The user name and password would cross the network in clear text.
		return &insecureError{reason: "the server only accepts a password login in clear text (VeNCrypt Plain without TLS)",
			unencrypted: true, cleartext: true}
	}
	if err := h.write(u32(sub)); err != nil {
		return err
	}
	h.res.subType = sub

	if isAnonTLS(sub) || isX509(sub) {
		ok, err := readU8(h.rw)
		if err != nil {
			return err
		}
		if ok != 1 {
			return errors.New("the server failed to initialize TLS")
		}
		if err := h.startTLS(sub, subtypes, topTypes); err != nil {
			return err
		}
	}
	switch sub {
	case vcTLSVnc, vcX509Vnc, secVNCAuth:
		return h.vncAuth()
	case vcTLSPlain, vcX509Plain, vcPlain:
		return h.plainAuth("VeNCrypt " + securityTypeName(sub))
	}
	return nil // None variants: SecurityResult follows
}

// retryWithout turns a dead end inside VeNCrypt into a reconnect that avoids it, when the server offered an
// alternative the client supports. The alternatives are unencrypted, so the policy decides: `require` fails,
// `allow-unencrypted` (or the user's confirmation) retries, otherwise the user is asked first.
func (h *handshake) retryWithout(t uint32, topTypes []byte, cause error) error {
	if _, alt := h.unencryptedAlternatives(nil, topTypes); !alt {
		return cause
	}
	switch {
	case h.cfg.policy == encRequire:
		return &encryptionRequiredError{reason: cause.Error()}
	case h.cfg.policy >= encAllowUnencrypted:
		return &retryError{exclude: t, cause: cause, downgrade: true}
	}
	return &insecureError{reason: capitalize(cause.Error()), unencrypted: true}
}

func (h *handshake) startTLS(sub uint32, offered []uint32, topTypes []byte) error {
	h.status("Negotiating TLS")
	_ = h.raw.SetDeadline(time.Now().Add(h.cfg.timeout))
	ctx, cancel := context.WithTimeout(h.ctx, h.cfg.timeout)
	defer cancel()
	if isAnonTLS(sub) {
		acfg := anontls.Config{}
		if h.cfg.anonTLS != nil {
			acfg = *h.cfg.anonTLS
		}
		if h.cfg.policy >= encAllowWeak && (acfg.MinDHBits == 0 || acfg.MinDHBits > weakDHFloor) {
			acfg.MinDHBits = weakDHFloor
		}
		tc := anontls.Client(h.conn, &acfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			if anontls.IsHandshakeFailure(err) {
				return h.anonTLSUnavailable(err, offered, topTypes)
			}
			return fmt.Errorf("TLS handshake failed: %w", err)
		}
		st := tc.ConnectionState()
		h.res.tls = &tlsInfo{Version: "TLS 1.2", CipherSuite: st.CipherSuiteName, Anonymous: true, Group: st.Group,
			DHBits: st.DHBits, Weak: st.DHBits != 0 && st.DHBits < 2048, EncryptThenMAC: st.EncryptThenMAC, Trust: "none"}
		_ = h.raw.SetDeadline(time.Time{})
		h.setConn(tc)
		return nil
	}

	cfg := &tls.Config{
		InsecureSkipVerify: true, // verified below against system roots, saved fingerprints or the user's decision
		MinVersion:         tls.VersionTLS12,
	}
	if net.ParseIP(h.cfg.host) == nil {
		cfg.ServerName = h.cfg.host
	}
	tc := tls.Client(h.conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS handshake failed: %w", err)
	}
	_ = h.raw.SetDeadline(time.Time{})
	st := tc.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return errors.New("the server presented no TLS certificate")
	}
	leaf := st.PeerCertificates[0]
	info := &tlsInfo{
		Version:     tls.VersionName(st.Version),
		CipherSuite: tls.CipherSuiteName(st.CipherSuite),
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		NotAfter:    leaf.NotAfter.UTC().Format(time.RFC3339),
		Fingerprint: certFingerprint(leaf),
	}
	h.status("Verifying the server certificate")
	trust, err := h.cfg.trust.verify(h.ctx, h.cfg.host, h.cfg.port, st.PeerCertificates)
	if err != nil {
		return err
	}
	info.Trust = trust
	h.res.tls = info
	h.setConn(tc)
	return nil
}

// anonTLSUnavailable decides what happens when the server's anonymous TLS cannot be used: the server refused
// AstraTerm's offer, or its Diffie-Hellman group is too weak. Depending on the policy the connection fails, retries
// (weak TLS allowed: the handshake already accepted it; unencrypted allowed: without anonymous TLS), or asks.
func (h *handshake) anonTLSUnavailable(err error, subtypes []uint32, topTypes []byte) error {
	var weak *anontls.WeakGroupError
	isWeak := errors.As(err, &weak) && weak.Bits >= weakDHFloor
	var cause error
	var ae *anontls.AlertError
	switch {
	case isWeak:
		cause = fmt.Errorf("the server's anonymous TLS uses a weak %d-bit Diffie-Hellman group", weak.Bits)
	case errors.As(err, &ae) && ae.Remote:
		cause = fmt.Errorf("the server refused the anonymous TLS handshake (%s)",
			strings.TrimPrefix(ae.Error(), "anontls: remote error: "))
	default:
		cause = fmt.Errorf("anonymous TLS failed: %s", strings.TrimPrefix(err.Error(), "anontls: "))
	}
	inVeNCrypt, topLevel := h.unencryptedAlternatives(subtypes, topTypes)
	if h.cfg.policy == encRequire {
		return &encryptionRequiredError{reason: cause.Error()}
	}
	if !inVeNCrypt && !topLevel && !isWeak {
		return cause
	}
	if h.cfg.policy >= encAllowUnencrypted && (inVeNCrypt || topLevel) {
		exclude := uint32(excludeAnonTLS)
		if !inVeNCrypt {
			exclude = secVeNCrypt // nothing else to use inside VeNCrypt: go straight to the other types
		}
		return &retryError{exclude: exclude, cause: cause, downgrade: true}
	}
	ie := &insecureError{reason: capitalize(cause.Error()), unencrypted: inVeNCrypt || topLevel}
	if isWeak {
		ie.weakTLS, ie.dhBits = true, weak.Bits
	}
	return ie
}

// hostPort formats host:port for messages.
func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
