package tools

import (
	"encoding/binary"
	"testing"
)

// buildKexInitPayload marshals a synthetic SSH_MSG_KEXINIT payload from the given name-lists.
func buildKexInitPayload(lists [10][]string) []byte {
	var b []byte
	b = append(b, 20)                  // SSH_MSG_KEXINIT
	b = append(b, make([]byte, 16)...) // cookie
	for _, l := range lists {
		s := ""
		for i, e := range l {
			if i > 0 {
				s += ","
			}
			s += e
		}
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		b = append(b, n[:]...)
		b = append(b, []byte(s)...)
	}
	b = append(b, 0)                  // first_kex_packet_follows
	b = append(b, make([]byte, 4)...) // reserved
	return b
}

func TestParseKexInit(t *testing.T) {
	lists := [10][]string{
		{"curve25519-sha256", "kex-strict-s-v00@openssh.com"},
		{"ssh-ed25519", "rsa-sha2-512"},
		{"chacha20-poly1305@openssh.com", "aes256-cbc"}, // c2s ciphers
		{"chacha20-poly1305@openssh.com", "aes256-cbc"}, // s2c ciphers
		{"hmac-sha2-256-etm@openssh.com"},               // c2s macs
		{"hmac-sha2-256-etm@openssh.com", "hmac-sha1"},  // s2c macs
		{"none", "zlib@openssh.com"},                    // c2s compression
		{"none"},                                        // s2c compression
		{},                                              // langs c2s
		{},                                              // langs s2c
	}
	kex, err := parseKexInit(buildKexInitPayload(lists))
	if err != nil {
		t.Fatal(err)
	}
	if len(kex.KexAlgos) != 2 || kex.KexAlgos[0] != "curve25519-sha256" {
		t.Fatalf("kex = %v", kex.KexAlgos)
	}
	if kex.HostKeyAlgos[0] != "ssh-ed25519" {
		t.Fatalf("hostkeys = %v", kex.HostKeyAlgos)
	}
	if kex.CiphersS2C[1] != "aes256-cbc" || kex.MACsS2C[1] != "hmac-sha1" {
		t.Fatalf("s2c = %v / %v", kex.CiphersS2C, kex.MACsS2C)
	}
}

func TestRatings(t *testing.T) {
	if r, _ := rateKex("sntrup761x25519-sha512@openssh.com"); r != ratePQ {
		t.Errorf("sntrup should be pq, got %s", r)
	}
	if r, _ := rateKex("diffie-hellman-group14-sha1"); r != rateLegacy {
		t.Errorf("group14-sha1 should be legacy, got %s", r)
	}
	if r, _ := rateHostKey("ssh-rsa"); r != rateLegacy {
		t.Errorf("ssh-rsa should be legacy, got %s", r)
	}
	if r, _ := rateCipher("aes256-cbc"); r != rateWeak {
		t.Errorf("aes256-cbc should be weak, got %s", r)
	}
	if r, _ := rateCipher("arcfour"); r != rateLegacy {
		t.Errorf("arcfour should be legacy, got %s", r)
	}
	if r, _ := rateMAC("hmac-sha2-256-etm@openssh.com"); r != rateOK {
		t.Errorf("etm mac should be ok, got %s", r)
	}
	if r, _ := rateMAC("hmac-md5"); r != rateLegacy {
		t.Errorf("hmac-md5 should be legacy, got %s", r)
	}
	if r, _ := rateMAC("hmac-sha1-etm@openssh.com"); r != rateWeak {
		t.Errorf("hmac-sha1-etm should be weak (not broken), got %s", r)
	}
}

func TestTerrapinExposure(t *testing.T) {
	// chacha20 without strict-kex → exposed.
	kex := &kexInit{CiphersS2C: []string{"chacha20-poly1305@openssh.com"}, CiphersC2S: []string{"chacha20-poly1305@openssh.com"}}
	if vuln, _ := terrapinExposure(kex, false); !vuln {
		t.Error("chacha20 without strict-kex should be vulnerable")
	}
	// With strict-kex → mitigated.
	if vuln, _ := terrapinExposure(kex, true); vuln {
		t.Error("strict-kex should mitigate Terrapin")
	}
	// CBC + ETM without strict-kex → exposed.
	kex2 := &kexInit{CiphersS2C: []string{"aes256-cbc"}, MACsS2C: []string{"hmac-sha2-256-etm@openssh.com"}}
	if vuln, _ := terrapinExposure(kex2, false); !vuln {
		t.Error("cbc+etm without strict-kex should be vulnerable")
	}
	// Only CTR + non-ETM → safe.
	kex3 := &kexInit{CiphersS2C: []string{"aes256-ctr"}, MACsS2C: []string{"hmac-sha2-256"}}
	if vuln, _ := terrapinExposure(kex3, false); vuln {
		t.Error("ctr + non-etm should be safe")
	}
}

func TestAuditFindingsAndGrade(t *testing.T) {
	weak := &kexInit{
		KexAlgos:     []string{"diffie-hellman-group1-sha1"},
		HostKeyAlgos: []string{"ssh-rsa"},
		CiphersS2C:   []string{"aes256-cbc", "3des-cbc"},
		MACsS2C:      []string{"hmac-md5"},
	}
	findings := auditFindings(weak, false, true)
	if len(findings) == 0 {
		t.Fatal("expected findings for a weak server")
	}
	if g := overallGrade(weak, true); g != "D" {
		t.Errorf("weak server grade = %s want D", g)
	}
	strong := &kexInit{
		KexAlgos:     []string{"curve25519-sha256", "kex-strict-s-v00@openssh.com", "sntrup761x25519-sha512@openssh.com"},
		HostKeyAlgos: []string{"ssh-ed25519"},
		CiphersS2C:   []string{"aes256-gcm@openssh.com"},
		MACsS2C:      []string{"hmac-sha2-256-etm@openssh.com"},
	}
	if g := overallGrade(strong, false); g != "A" {
		t.Errorf("strong server grade = %s want A", g)
	}
	// A stock modern OpenSSH also offers SHA-1 HMACs: weak (B), not broken (D).
	stock := *strong
	stock.MACsS2C = []string{"umac-128-etm@openssh.com", "hmac-sha2-256-etm@openssh.com", "hmac-sha1-etm@openssh.com", "hmac-sha1"}
	if g := overallGrade(&stock, false); g != "B" {
		t.Errorf("stock OpenSSH grade = %s want B", g)
	}
	cfg := recommendedSSHDConfig(strong)
	if cfg == "" || !contains(cfg, "KexAlgorithms") {
		t.Errorf("recommended config missing: %q", cfg)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
