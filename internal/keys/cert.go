package keys

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// OpenSSH certificates: details of attached user certificates (SSH-5), validation of certificates attached to a
// stored key, and signing user / host certificates with a stored key acting as CA (TOOL-1 "sign certificates").

// Certificate statuses.
const (
	certValid       = "valid"
	certExpired     = "expired"
	certNotYetValid = "not_yet_valid"
)

// certInfo describes a certificate for the UI.
type certInfo struct {
	Type            string            `json:"type"` // user | host
	KeyID           string            `json:"keyId"`
	Serial          string            `json:"serial"` // decimal (uint64 does not fit a JSON number)
	Principals      []string          `json:"principals"`
	ValidAfter      *time.Time        `json:"validAfter,omitempty"`  // nil: valid since forever
	ValidBefore     *time.Time        `json:"validBefore,omitempty"` // nil: never expires
	CriticalOptions map[string]string `json:"criticalOptions"`
	Extensions      []string          `json:"extensions"`
	KeyType         string            `json:"keyType"`
	KeyFingerprint  string            `json:"keyFingerprint"`
	CAKeyType       string            `json:"caKeyType"`
	CAFingerprint   string            `json:"caFingerprint"`
	SignatureType   string            `json:"signatureType"`
	Status          string            `json:"status"`
}

func certTypeName(t uint32) string {
	if t == ssh.HostCert {
		return "host"
	}
	return "user"
}

// describeCert summarizes cert at time now.
func describeCert(cert *ssh.Certificate, now time.Time) *certInfo {
	info := &certInfo{
		Type:            certTypeName(cert.CertType),
		KeyID:           cert.KeyId,
		Serial:          strconv.FormatUint(cert.Serial, 10),
		Principals:      append([]string{}, cert.ValidPrincipals...),
		CriticalOptions: map[string]string{},
		Extensions:      []string{},
		KeyType:         cert.Key.Type(),
		KeyFingerprint:  ssh.FingerprintSHA256(cert.Key),
		Status:          certValid,
	}
	if cert.SignatureKey != nil {
		info.CAKeyType, info.CAFingerprint = cert.SignatureKey.Type(), ssh.FingerprintSHA256(cert.SignatureKey)
	}
	if cert.Signature != nil {
		info.SignatureType = cert.Signature.Format
	}
	for k, v := range cert.CriticalOptions {
		info.CriticalOptions[k] = v
	}
	for k := range cert.Extensions {
		info.Extensions = append(info.Extensions, k)
	}
	sort.Strings(info.Extensions)
	if cert.ValidAfter != 0 {
		t := certTime(cert.ValidAfter)
		info.ValidAfter = &t
	}
	if cert.ValidBefore != ssh.CertTimeInfinity {
		t := certTime(cert.ValidBefore)
		info.ValidBefore = &t
	}
	unix := now.Unix()
	switch {
	case unix < 0 || uint64(unix) < cert.ValidAfter:
		info.Status = certNotYetValid
	case cert.ValidBefore != ssh.CertTimeInfinity && uint64(unix) >= cert.ValidBefore:
		info.Status = certExpired
	}
	return info
}

// certTime converts a certificate timestamp (uint64 seconds) to a time, clamping absurd values.
func certTime(v uint64) time.Time {
	const maxUnix = 253402300799 // 9999-12-31T23:59:59Z
	if v > maxUnix {
		v = maxUnix
	}
	return time.Unix(int64(v), 0).UTC()
}

// parseCertificateText parses an OpenSSH certificate line ("<type>-cert-v01@openssh.com AAAA… comment").
func parseCertificateText(text string) (*ssh.Certificate, string, error) {
	pk, comment, err := parsePublicKeyText(text)
	if err != nil {
		return nil, "", invalidKey("not a valid OpenSSH certificate: " + err.Error())
	}
	cert, ok := pk.(*ssh.Certificate)
	if !ok {
		return nil, "", invalidKey("this is a plain public key, not a certificate (expected an id_*-cert.pub file)")
	}
	return cert, comment, nil
}

// validateUserCert checks that cert certifies keyPub as a user certificate and returns the line to store.
func validateUserCert(text string, keyPub ssh.PublicKey) (*ssh.Certificate, string, error) {
	cert, comment, err := parseCertificateText(text)
	if err != nil {
		return nil, "", err
	}
	if cert.CertType != ssh.UserCert {
		return nil, "", invalidKey("this is a host certificate; only user certificates can be attached to a key")
	}
	if !sameKey(cert.Key, keyPub) {
		return nil, "", &keyError{"certificate_mismatch", fmt.Sprintf("the certificate is for another key (%s)", ssh.FingerprintSHA256(cert.Key))}
	}
	if cert.SignatureKey == nil || cert.Signature == nil {
		return nil, "", invalidKey("the certificate is not signed")
	}
	if err := cert.SignatureKey.Verify(certSignedBytes(cert), cert.Signature); err != nil {
		return nil, "", invalidKey("the certificate signature does not verify")
	}
	return cert, authorizedKeyLine(cert, comment), nil
}

// certSignedBytes returns the part of the certificate covered by its CA signature: the marshalled certificate
// without its trailing signature field (as x/crypto's unexported bytesForSigning).
func certSignedBytes(cert *ssh.Certificate) []byte {
	c := *cert
	c.Signature = nil
	b := c.Marshal()
	return b[:len(b)-4] // drop the empty signature's length
}

// storedCertificate parses a certificate stored with a key (nil when absent or invalid).
func storedCertificate(line string, keyPub ssh.PublicKey) *ssh.Certificate {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	pk, _, err := parsePublicKeyText(line)
	if err != nil {
		return nil
	}
	cert, ok := pk.(*ssh.Certificate)
	if !ok || (keyPub != nil && !sameKey(cert.Key, keyPub)) {
		return nil
	}
	return cert
}

// ---- signing ------------------------------------------------------------------------------------------------------

// Default extensions of user certificates (ssh-keygen's defaults).
var defaultUserExtensions = []string{"permit-X11-forwarding", "permit-agent-forwarding", "permit-port-forwarding",
	"permit-pty", "permit-user-rc"}

var knownUserExtensions = append([]string{"no-touch-required"}, defaultUserExtensions...)

// signRequest is the body of POST /api/keys/{id}/sign: the stored key {id} signs a certificate for another key.
type signRequest struct {
	// Subject: a public key (any supported text form) or one of the caller's stored keys.
	PublicKey    string `json:"publicKey"`
	SubjectKeyID string `json:"subjectKeyId"`

	CertType   string   `json:"certType"` // user (default) | host
	Identity   string   `json:"identity"` // certificate key ID (logged by sshd)
	Principals []string `json:"principals"`
	// Validity window as RFC 3339 times; empty ValidBefore means "forever", empty ValidAfter "5 minutes ago" when
	// ValidBefore is set (clock skew), else "always".
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`

	CriticalOptions map[string]string `json:"criticalOptions"`
	// Extensions of user certificates; nil selects ssh-keygen's defaults.
	Extensions []string `json:"extensions"`
	Serial     *uint64  `json:"serial"` // nil: random

	CAPassphrase string `json:"caPassphrase"`
	// Attach the certificate to the subject key (user certificates of stored keys only).
	Attach bool `json:"attach"`
}

// signResult is the response of the sign endpoint.
type signResult struct {
	Certificate string    `json:"certificate"`
	Info        *certInfo `json:"info"`
	Filename    string    `json:"filename"`
	Attached    bool      `json:"attached"`
}

// buildCertificate validates req and returns the unsigned certificate for subject.
func buildCertificate(req *signRequest, subject ssh.PublicKey, now time.Time) (*ssh.Certificate, error) {
	if _, ok := subject.(*ssh.Certificate); ok {
		return nil, invalidKey("the subject is already a certificate; give its plain public key")
	}
	cert := &ssh.Certificate{Key: subject, CertType: ssh.UserCert}
	switch strings.ToLower(strings.TrimSpace(req.CertType)) {
	case "", "user":
	case "host":
		cert.CertType = ssh.HostCert
	default:
		return nil, invalidKey("certificate type must be user or host")
	}
	cert.KeyId = strings.TrimSpace(req.Identity)
	if len(cert.KeyId) > 256 || strings.ContainsFunc(cert.KeyId, isControl) {
		return nil, invalidKey("invalid certificate identity")
	}
	seen := map[string]bool{}
	for _, p := range req.Principals {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if len(p) > 256 || strings.ContainsFunc(p, func(r rune) bool { return isControl(r) || r == ',' || r == ' ' }) {
			return nil, invalidKey(fmt.Sprintf("invalid principal %q", p))
		}
		seen[p] = true
		cert.ValidPrincipals = append(cert.ValidPrincipals, p)
	}
	if len(cert.ValidPrincipals) == 0 {
		return nil, invalidKey("at least one principal (user name or host name) is required")
	}
	if len(cert.ValidPrincipals) > 256 {
		return nil, invalidKey("too many principals")
	}

	cert.ValidAfter, cert.ValidBefore = 0, ssh.CertTimeInfinity
	var after, before time.Time
	var err error
	if s := strings.TrimSpace(req.ValidBefore); s != "" {
		if before, err = time.Parse(time.RFC3339, s); err != nil {
			return nil, invalidKey("validBefore must be an RFC 3339 time")
		}
		if !before.After(now) {
			return nil, invalidKey("the certificate would already be expired")
		}
		cert.ValidBefore = uint64(before.Unix())
		cert.ValidAfter = uint64(max(now.Add(-5*time.Minute).Unix(), 0))
	}
	if s := strings.TrimSpace(req.ValidAfter); s != "" {
		if after, err = time.Parse(time.RFC3339, s); err != nil {
			return nil, invalidKey("validAfter must be an RFC 3339 time")
		}
		if after.Unix() < 0 {
			return nil, invalidKey("validAfter is out of range")
		}
		cert.ValidAfter = uint64(after.Unix())
	}
	if cert.ValidBefore != ssh.CertTimeInfinity && cert.ValidAfter >= cert.ValidBefore {
		return nil, invalidKey("the validity period is empty")
	}

	if req.Serial != nil {
		cert.Serial = *req.Serial
	} else {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		cert.Serial = binary.BigEndian.Uint64(b[:]) >> 1
	}

	cert.CriticalOptions, cert.Extensions = map[string]string{}, map[string]string{}
	if cert.CertType == ssh.HostCert {
		if len(req.CriticalOptions) > 0 || len(req.Extensions) > 0 {
			return nil, invalidKey("host certificates have no critical options or extensions")
		}
		return cert, nil
	}
	for k, v := range req.CriticalOptions {
		v = strings.TrimSpace(v)
		switch k {
		case "force-command":
			if v == "" {
				continue
			}
			if len(v) > 4096 || strings.ContainsFunc(v, isControl) {
				return nil, invalidKey("invalid force-command")
			}
		case "source-address":
			if v == "" {
				continue
			}
			for _, part := range strings.Split(v, ",") {
				part = strings.TrimSpace(part)
				if _, err := netip.ParsePrefix(part); err != nil {
					if _, err := netip.ParseAddr(part); err != nil {
						return nil, invalidKey(fmt.Sprintf("invalid source-address entry %q (expected IP addresses or CIDRs)", part))
					}
				}
			}
		case "verify-required":
			v = ""
		default:
			return nil, invalidKey(fmt.Sprintf("unsupported critical option %q", k))
		}
		cert.CriticalOptions[k] = v
	}
	exts := req.Extensions
	if exts == nil {
		exts = defaultUserExtensions
	}
	for _, e := range exts {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !slices.Contains(knownUserExtensions, e) && !strings.Contains(e, "@") {
			return nil, invalidKey(fmt.Sprintf("unknown extension %q (custom extensions must be name@domain)", e))
		}
		if len(e) > 128 || strings.ContainsFunc(e, isControl) {
			return nil, invalidKey("invalid extension name")
		}
		cert.Extensions[e] = ""
	}
	return cert, nil
}

// caSigner prepares the signer of a CA key: RSA CAs sign with rsa-sha2-512 (ssh-keygen's default).
func caSigner(p *parsedKey) (ssh.Signer, error) {
	if p.Pub.Type() == ssh.InsecureKeyAlgoDSA {
		return nil, unsupportedKey("DSA keys cannot sign certificates")
	}
	s, err := p.Signer()
	if err != nil {
		return nil, err
	}
	if p.Pub.Type() == ssh.KeyAlgoRSA {
		if as, ok := s.(ssh.AlgorithmSigner); ok {
			return ssh.NewSignerWithAlgorithms(as, []string{ssh.KeyAlgoRSASHA512})
		}
	}
	return s, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }
