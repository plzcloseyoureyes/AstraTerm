package importer

import (
	"crypto/dsa" //nolint:staticcheck // PuTTY caches may still hold DSA host keys
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// PuTTY-family host key caches (SSH-19 import path of IMP-1/IMP-2): PuTTY/KiTTY keep them under
// HKCU\Software\SimonTatham\PuTTY\SshHostKeys (exported as `"ssh-ed25519@22:host"="0x…,0x…"`), MobaXterm under
// [SSH_Hostkeys] of MobaXterm.ini (`ssh-ed25519@22:host=0x…,0x…`) and Unix PuTTY in ~/.putty/sshhostkeys
// (`ssh-ed25519@22:host 0x…,0x…`). The values are the key's numeric parameters in hex, converted here to SSH wire keys.

// rePuttyHostKeyLine matches one cache entry: kind, port, host, value.
var rePuttyHostKeyLine = regexp.MustCompile(`^"?([A-Za-z0-9-]+)@(\d{1,5}):([^"=\s]+)"?(?:\s*=\s*|\s+)"?([^"]*)"?\s*$`)

// parsePuttyHostKeyLine converts one cache line; ok is false when the line is not a cache entry at all.
func parsePuttyHostKeyLine(line string) (*pknownHost, bool, error) {
	m := rePuttyHostKeyLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return nil, false, nil
	}
	kh, err := puttyHostKey(m[1], m[2], m[3], m[4])
	return kh, true, err
}

// puttyHostKey converts a cache entry (kind "rsa2" | "dss" | "ssh-ed25519" | "ecdsa-sha2-nistpNNN", port, host and the
// comma-separated hex parameters) to a known host.
func puttyHostKey(kind, portStr, host, value string) (*pknownHost, error) {
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid port")
	}
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t/\\@\"'*?!,") {
		return nil, errors.New("invalid host name")
	}
	var nums []*big.Int
	curve := ""
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "nistp") {
			curve = part
			continue
		}
		h := strings.TrimPrefix(strings.TrimPrefix(part, "0x"), "0X")
		if h == "" || len(h) > 4096 {
			return nil, errors.New("invalid key value")
		}
		v, ok := new(big.Int).SetString(h, 16)
		if !ok {
			return nil, errors.New("invalid hex number in the key value")
		}
		nums = append(nums, v)
	}
	var pub ssh.PublicKey
	switch kind {
	case "rsa2":
		if len(nums) != 2 || nums[0].BitLen() > 32 || nums[0].Int64() < 3 || nums[1].BitLen() < 512 || nums[1].BitLen() > 16384 {
			return nil, errors.New("invalid RSA host key")
		}
		pub, err = ssh.NewPublicKey(&rsa.PublicKey{E: int(nums[0].Int64()), N: nums[1]})
	case "dss":
		if len(nums) != 4 || nums[0].BitLen() > 4096 {
			return nil, errors.New("invalid DSA host key")
		}
		pub, err = ssh.NewPublicKey(&dsa.PublicKey{Parameters: dsa.Parameters{P: nums[0], Q: nums[1], G: nums[2]}, Y: nums[3]})
	case "ssh-ed25519":
		if len(nums) != 2 || nums[1].BitLen() > 255 {
			return nil, errors.New("invalid Ed25519 host key")
		}
		// RFC 8032 encoding: y little-endian, the sign (low bit) of x in the top bit.
		enc := nums[1].FillBytes(make([]byte, ed25519.PublicKeySize))
		for i, j := 0, len(enc)-1; i < j; i, j = i+1, j-1 {
			enc[i], enc[j] = enc[j], enc[i]
		}
		if nums[0].Bit(0) == 1 {
			enc[31] |= 0x80
		}
		pub, err = ssh.NewPublicKey(ed25519.PublicKey(enc))
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		name := strings.TrimPrefix(kind, "ecdsa-sha2-")
		if curve != "" && curve != name {
			return nil, errors.New("curve mismatch in ECDSA host key")
		}
		var c elliptic.Curve
		switch name {
		case "nistp256":
			c = elliptic.P256()
		case "nistp384":
			c = elliptic.P384()
		default:
			c = elliptic.P521()
		}
		size := (c.Params().BitSize + 7) / 8
		if len(nums) != 2 || nums[0].BitLen() > size*8 || nums[1].BitLen() > size*8 {
			return nil, errors.New("invalid ECDSA host key")
		}
		point := append([]byte{4}, nums[0].FillBytes(make([]byte, size))...)
		point = append(point, nums[1].FillBytes(make([]byte, size))...)
		pk, perr := ecdsa.ParseUncompressedPublicKey(c, point)
		if perr != nil {
			return nil, fmt.Errorf("invalid ECDSA host key: %v", perr)
		}
		pub, err = ssh.NewPublicKey(pk)
	default:
		return nil, fmt.Errorf("unsupported host key type %q", truncate(kind, 32))
	}
	if err != nil {
		return nil, err
	}
	return &pknownHost{
		host: host, port: port, keyType: pub.Type(), publicKey: sshx.FormatKnownHostKey(pub),
		fingerprint: ssh.FingerprintSHA256(pub), comment: "imported from a PuTTY-style host key cache",
	}, nil
}

// addPuttyHostKeys parses cache lines ("kind@port:host" = value) into b, counting unusable ones in a warning.
func addPuttyHostKeys(b *builder, lines []string, source string) {
	bad := 0
	for _, ln := range lines {
		kh, ok, err := parsePuttyHostKeyLine(ln)
		if !ok {
			continue
		}
		if err != nil {
			bad++
			continue
		}
		b.knownHost(kh)
	}
	if bad > 0 {
		b.warn("%d host key(s) in %s could not be converted (unsupported type or malformed)", bad, source)
	}
}
