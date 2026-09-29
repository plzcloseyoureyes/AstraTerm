package tools

import "strings"

// The rating tables below encode the built-in ssh-audit-style policy (RESEARCH CC-17). They are intentionally
// conservative and self-contained (no external MIB/rules file), covering the algorithms OpenSSH and common network
// gear negotiate.

func rateKex(a string) (string, string) {
	switch {
	case strings.Contains(a, "sntrup761") || strings.Contains(a, "mlkem") || strings.Contains(a, "kyber"):
		return ratePQ, "post-quantum hybrid key exchange"
	case a == "curve25519-sha256" || a == "curve25519-sha256@libssh.org":
		return rateOK, ""
	case a == "diffie-hellman-group16-sha512" || a == "diffie-hellman-group18-sha512" || a == "diffie-hellman-group15-sha512":
		return rateOK, ""
	case a == "ecdh-sha2-nistp256" || a == "ecdh-sha2-nistp384" || a == "ecdh-sha2-nistp521":
		return rateOK, "NIST curve (some operators avoid these)"
	case a == "diffie-hellman-group14-sha256":
		return rateWeak, "2048-bit group; prefer group16/18 or curve25519"
	case a == "diffie-hellman-group-exchange-sha256":
		return rateOK, "ensure the server uses ≥ 3072-bit moduli"
	case a == "diffie-hellman-group14-sha1", a == "diffie-hellman-group1-sha1",
		a == "diffie-hellman-group-exchange-sha1", strings.HasSuffix(a, "sha1"):
		return rateLegacy, "SHA-1 based; disable"
	case strings.HasPrefix(a, "gss-"):
		return rateWeak, "GSSAPI key exchange"
	case a == "ext-info-s", a == "ext-info-c", strings.HasPrefix(a, "kex-strict-"):
		return rateOK, ""
	default:
		return rateUnknown, ""
	}
}

func rateHostKey(a string) (string, string) {
	switch a {
	case "ssh-ed25519", "ssh-ed25519-cert-v01@openssh.com",
		"sk-ssh-ed25519@openssh.com", "sk-ssh-ed25519-cert-v01@openssh.com":
		return rateOK, ""
	case "rsa-sha2-512", "rsa-sha2-256", "rsa-sha2-512-cert-v01@openssh.com", "rsa-sha2-256-cert-v01@openssh.com":
		return rateOK, ""
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"sk-ecdsa-sha2-nistp256@openssh.com":
		return rateOK, "NIST curve"
	case "ssh-rsa", "ssh-rsa-cert-v01@openssh.com":
		return rateLegacy, "SHA-1 RSA signature; disable (OpenSSH ≥ 8.8 defaults off)"
	case "ssh-dss", "ssh-dss-cert-v01@openssh.com":
		return rateLegacy, "DSA; broken, disable"
	default:
		return rateUnknown, ""
	}
}

func rateCipher(a string) (string, string) {
	switch a {
	case "chacha20-poly1305@openssh.com":
		return rateOK, "AEAD (Terrapin-relevant without strict-kex)"
	case "aes256-gcm@openssh.com", "aes128-gcm@openssh.com":
		return rateOK, "AEAD"
	case "aes256-ctr", "aes192-ctr", "aes128-ctr":
		return rateOK, ""
	case "aes256-cbc", "aes192-cbc", "aes128-cbc", "rijndael-cbc@lysator.liu.se":
		return rateWeak, "CBC mode; disable"
	case "3des-cbc", "3des-ctr":
		return rateLegacy, "3DES; disable"
	case "blowfish-cbc", "cast128-cbc", "cast128-ctr":
		return rateLegacy, "obsolete cipher; disable"
	case "arcfour", "arcfour128", "arcfour256":
		return rateLegacy, "RC4; broken, disable"
	case "none":
		return rateLegacy, "no encryption"
	default:
		if strings.HasSuffix(a, "-cbc") {
			return rateWeak, "CBC mode"
		}
		return rateUnknown, ""
	}
}

func rateMAC(a string) (string, string) {
	switch a {
	case "hmac-sha2-256-etm@openssh.com", "hmac-sha2-512-etm@openssh.com", "umac-128-etm@openssh.com":
		return rateOK, "encrypt-then-MAC"
	case "hmac-sha2-256", "hmac-sha2-512", "umac-128@openssh.com":
		return rateWeak, "encrypt-and-MAC; prefer the -etm variant"
	case "umac-64-etm@openssh.com", "umac-64@openssh.com":
		return rateWeak, "64-bit tag"
	case "hmac-sha1-etm@openssh.com":
		return rateWeak, "SHA-1 based (HMAC is not broken, but prefer SHA-2)"
	case "hmac-sha1":
		return rateWeak, "SHA-1 based and encrypt-and-MAC; prefer SHA-2 -etm"
	case "hmac-sha1-96", "hmac-sha1-96-etm@openssh.com", "hmac-md5", "hmac-md5-96",
		"hmac-md5-etm@openssh.com", "hmac-md5-96-etm@openssh.com", "hmac-ripemd160":
		return rateLegacy, "broken/truncated MAC; disable"
	case "none":
		return rateLegacy, "no integrity protection"
	default:
		return rateUnknown, ""
	}
}

func rateCompression(a string) (string, string) {
	switch a {
	case "none", "zlib@openssh.com":
		return rateOK, ""
	case "zlib":
		return rateWeak, "pre-auth compression"
	default:
		return rateUnknown, ""
	}
}

// terrapinExposure reports whether the server's offered algorithms leave it exposed to the Terrapin prefix-truncation
// attack (CVE-2023-48795): a ChaCha20-Poly1305 cipher or any encrypt-then-MAC combined with a CBC cipher, without the
// strict-kex countermeasure.
func terrapinExposure(kex *kexInit, strictKex bool) (bool, string) {
	if strictKex {
		return false, "server supports strict key exchange (kex-strict), which mitigates Terrapin"
	}
	chacha := containsAny(kex.CiphersS2C, "chacha20-poly1305@openssh.com") || containsAny(kex.CiphersC2S, "chacha20-poly1305@openssh.com")
	hasETM := anyHasSuffix(kex.MACsS2C, "-etm@openssh.com") || anyHasSuffix(kex.MACsC2S, "-etm@openssh.com")
	hasCBC := anyHasSuffix(kex.CiphersS2C, "-cbc") || anyHasSuffix(kex.CiphersC2S, "-cbc")
	switch {
	case chacha && hasETM && hasCBC:
		return true, "offers ChaCha20-Poly1305, ETM MACs and CBC ciphers without strict-kex"
	case chacha:
		return true, "offers ChaCha20-Poly1305 without strict key exchange"
	case hasETM && hasCBC:
		return true, "offers CBC ciphers with encrypt-then-MAC without strict key exchange"
	default:
		return false, "no Terrapin-vulnerable cipher/MAC combination offered"
	}
}

func anyHasSuffix(list []string, suffix string) bool {
	for _, s := range list {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// auditFindings turns the ratings into actionable findings (severity: critical|high|medium|low|info).
func auditFindings(kex *kexInit, strictKex, terrapin bool) []row {
	var findings []row
	add := func(severity, message string) {
		findings = append(findings, row{"kind": "finding", "severity": severity, "message": message})
	}

	if terrapin {
		add("high", "Vulnerable to the Terrapin attack (CVE-2023-48795). Update OpenSSH to ≥ 9.6 so strict key exchange is negotiated, or restrict ciphers/MACs.")
	}
	if !strictKex {
		add("medium", "Strict key exchange (kex-strict-s-v00@openssh.com) is not offered; upgrade the SSH server to enable it.")
	}
	reportLegacy := func(category string, algos []string, rate func(string) (string, string)) {
		var bad []string
		for _, a := range algos {
			if r, _ := rate(a); r == rateLegacy {
				bad = append(bad, a)
			}
		}
		if len(bad) > 0 {
			add("high", "Insecure "+category+" offered: "+strings.Join(bad, ", ")+". Disable these.")
		}
	}
	reportLegacy("key exchange algorithms", kex.KexAlgos, rateKex)
	reportLegacy("host key algorithms", kex.HostKeyAlgos, rateHostKey)
	reportLegacy("ciphers", kex.CiphersS2C, rateCipher)
	reportLegacy("MAC algorithms", kex.MACsS2C, rateMAC)
	if sha1 := filterPrefix(kex.MACsS2C, "hmac-sha1"); len(sha1) > 0 && !anyRated(sha1, rateMAC, rateLegacy) {
		add("low", "SHA-1 MACs are offered ("+strings.Join(sha1, ", ")+"); prefer hmac-sha2-256-etm@openssh.com / hmac-sha2-512-etm@openssh.com.")
	}

	if containsAny(kex.HostKeyAlgos, "ssh-rsa") {
		add("medium", "The ssh-rsa (SHA-1) host key algorithm is offered; remove it in favour of rsa-sha2-256/512 or ssh-ed25519.")
	}
	if anyHasSuffix(kex.CiphersS2C, "-cbc") {
		add("medium", "CBC-mode ciphers are offered; prefer CTR or AEAD (GCM / ChaCha20-Poly1305) ciphers.")
	}
	if !containsAny(kex.KexAlgos, "sntrup761x25519-sha512@openssh.com", "sntrup761x25519-sha512", "mlkem768x25519-sha256") {
		add("info", "No post-quantum key exchange offered; consider enabling sntrup761x25519-sha512@openssh.com or mlkem768x25519-sha256 (OpenSSH ≥ 9.9).")
	}
	if len(findings) == 0 {
		add("info", "No weak algorithms detected in the server's offer.")
	}
	return findings
}

func filterPrefix(list []string, prefix string) []string {
	var out []string
	for _, v := range list {
		if strings.HasPrefix(v, prefix) {
			out = append(out, v)
		}
	}
	return out
}

func anyRated(list []string, rate func(string) (string, string), want string) bool {
	for _, a := range list {
		if r, _ := rate(a); r == want {
			return true
		}
	}
	return false
}

// overallGrade is a coarse letter grade: D = broken algorithms or Terrapin, C = CBC ciphers or SHA-1 RSA
// signatures, B = no strict-kex or other weak algorithms, A = clean.
func overallGrade(kex *kexInit, terrapin bool) string {
	// Count legacy algorithms across categories with the appropriate raters.
	count := func(algos []string, rate func(string) (string, string)) int {
		n := 0
		for _, a := range algos {
			if r, _ := rate(a); r == rateLegacy {
				n++
			}
		}
		return n
	}
	legacy := count(kex.KexAlgos, rateKex) + count(kex.HostKeyAlgos, rateHostKey) + count(kex.CiphersS2C, rateCipher) + count(kex.MACsS2C, rateMAC)
	switch {
	case terrapin || legacy > 0:
		return "D"
	case anyHasSuffix(kex.CiphersS2C, "-cbc") || containsAny(kex.HostKeyAlgos, "ssh-rsa"):
		return "C"
	case !containsAny(kex.KexAlgos, "kex-strict-s-v00@openssh.com"),
		anyRated(kex.KexAlgos, rateKex, rateWeak), anyRated(kex.MACsS2C, rateMAC, rateWeak),
		anyRated(kex.CiphersS2C, rateCipher, rateWeak):
		return "B"
	default:
		return "A"
	}
}

// recommendedSSHDConfig returns a hardened sshd_config snippet (only algorithms the server already supports are
// listed, so the operator can paste it without breaking connectivity, plus commented modern additions).
func recommendedSSHDConfig(kex *kexInit) string {
	keep := func(offered []string, preferred []string) []string {
		var out []string
		for _, p := range preferred {
			if containsAny(offered, p) {
				out = append(out, p)
			}
		}
		return out
	}
	kexList := keep(kex.KexAlgos, []string{"sntrup761x25519-sha512@openssh.com", "mlkem768x25519-sha256", "curve25519-sha256", "curve25519-sha256@libssh.org", "diffie-hellman-group16-sha512", "diffie-hellman-group18-sha512"})
	hostList := keep(kex.HostKeyAlgos, []string{"ssh-ed25519", "rsa-sha2-512", "rsa-sha2-256", "ecdsa-sha2-nistp256"})
	cipherList := keep(kex.CiphersS2C, []string{"chacha20-poly1305@openssh.com", "aes256-gcm@openssh.com", "aes128-gcm@openssh.com", "aes256-ctr", "aes192-ctr", "aes128-ctr"})
	macList := keep(kex.MACsS2C, []string{"hmac-sha2-256-etm@openssh.com", "hmac-sha2-512-etm@openssh.com", "umac-128-etm@openssh.com"})

	fallback := func(list, def []string) []string {
		if len(list) == 0 {
			return def
		}
		return list
	}
	var b strings.Builder
	b.WriteString("# Hardened sshd_config (generated by Termstead ssh-audit)\n")
	b.WriteString("KexAlgorithms " + strings.Join(fallback(kexList, []string{"curve25519-sha256"}), ",") + "\n")
	b.WriteString("HostKeyAlgorithms " + strings.Join(fallback(hostList, []string{"ssh-ed25519", "rsa-sha2-512"}), ",") + "\n")
	b.WriteString("Ciphers " + strings.Join(fallback(cipherList, []string{"aes256-gcm@openssh.com"}), ",") + "\n")
	b.WriteString("MACs " + strings.Join(fallback(macList, []string{"hmac-sha2-256-etm@openssh.com"}), ",") + "\n")
	return b.String()
}
