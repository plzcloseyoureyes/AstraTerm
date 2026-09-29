package ai

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Redaction (RESEARCH SEC-11): everything that leaves AstraTerm for an AI provider — context and the conversation —
// is passed through a redactor that removes the user's stored secrets (exact values) and anything that looks like a
// credential (private-key blocks, cloud/API tokens, JWTs, Authorization headers, password assignments, URL
// credentials, …).

const redactedMark = "[REDACTED]"

type redactRule struct {
	name string
	re   *regexp.Regexp
	// keep is the number of leading submatches kept verbatim (the rest of the match is replaced); 0 = replace all.
	keep int
	// tail keeps the last submatch too (e.g. the "@" of URL credentials).
	tail bool
}

var builtinRedactions = []redactRule{
	{name: "private-key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)},
	{name: "putty-key", re: regexp.MustCompile(`(?s)PuTTY-User-Key-File-\d+:.*?(?:Private-MAC: *[0-9a-fA-F]+|\z)`)},
	{name: "aws-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA)[A-Z0-9]{16}\b`)},
	{name: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,255}|github_pat_[A-Za-z0-9_]{22,255})\b`)},
	{name: "gitlab-token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`)},
	{name: "slack-token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{name: "api-key", re: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_\-]{20,}`)},
	{name: "google-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{name: "stripe-key", re: regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}\b`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{name: "auth-header", re: regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization\s*[:=]\s*"?(?:bearer|basic|token|digest)\s+)[A-Za-z0-9._~+/=\-]{6,}`), keep: 1},
	{name: "bearer", re: regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=\-]{16,}`), keep: 1},
	{name: "astraterm-token", re: regexp.MustCompile(`\bnxt_[A-Za-z0-9_\-]{16,}`)},
	{name: "cookie-header", re: regexp.MustCompile(`(?im)(\b(?:set-)?cookie\s*:\s*[^=\s;]+=)([^;\s]{6,})`), keep: 1},
	{name: "url-credentials", re: regexp.MustCompile(`(\b[a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@'"]+:)[^@\s/'"]+(@)`), keep: 1, tail: true},
	{name: "cli-password", re: regexp.MustCompile(`(?i)(--[a-z0-9-]*(?:password|passwd|pass|token|secret|api-?key|access-?key|secret-?key|client-?secret|auth-?token)(?:=|\s+))("[^"\n]*"|'[^'\n]*'|\S+)`), keep: 1},
	{name: "curl-user", re: regexp.MustCompile(`(\b(?:curl|wget|http|https)\b[^\n]*?\s(?:-u|--user|--proxy-user)\s*["']?[^\s:'"]+:)([^\s'"]+)`), keep: 1},
	{name: "mysql-password", re: regexp.MustCompile(`(\bmysql(?:dump|admin|sh)?\b[^\n]*?\s-p)([^\s-]\S*)`), keep: 1},
	{name: "sshpass", re: regexp.MustCompile(`(\bsshpass\s+-p\s*)(\S+)`), keep: 1},
	// sudo -S reads the password from stdin: `echo pw | sudo -S …`, `printf '%s\n' pw | sudo -S …`, `sudo -S … <<< pw`.
	{name: "sudo-stdin", re: regexp.MustCompile(`(\b(?:echo|printf)\s+(?:-[a-zA-Z]+\s+)*(?:'%s\\n'\s+|"%s\\n"\s+)?)("[^"\n]*"|'[^'\n]*'|[^\s|]+)(\s*\|\s*sudo\b[^\n|]*\s-[a-zA-Z]*S)`), keep: 1, tail: true},
	{name: "sudo-herestring", re: regexp.MustCompile(`(\bsudo\b[^\n]*\s-[a-zA-Z]*S\b[^\n]*<<<\s*)("[^"\n]*"|'[^'\n]*'|\S+)`), keep: 1},
	{name: "xml-password", re: regexp.MustCompile(`(?i)(<([A-Za-z0-9_.\-]*(?:password|passwd|secret|token|api[_\-]?key|apikey))\s*>)([^<\n]{3,})`), keep: 1},
	{name: "assignment", re: regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.\-]*(?:password|passwd|passphrase|secret|token|api[_\-]?key|apikey|access[_\-]?key|private[_\-]?key|client[_\-]?secret|credentials?)[A-Za-z0-9_.\-]*"?\s*[:=]\s*)("[^"\n]{3,}"|'[^'\n]{3,}'|[^\s"',;]{3,})`), keep: 1},
	// FOO_PASS=… / MYSQL_PWD=… / redis_pw=… (a separator before the short word keeps "bypass=", "PWD=/home" intact).
	{name: "short-assignment", re: regexp.MustCompile(`(?i)(\b[A-Za-z0-9]+(?:[_.\-][A-Za-z0-9]+)*[_.\-](?:pass|pwd|pw)"?\s*[:=]\s*)("[^"\n]{3,}"|'[^'\n]{3,}'|[^\s"',;]{3,})`), keep: 1},
}

// Redactor removes secrets from text.
type Redactor struct {
	secrets []string
	custom  []*regexp.Regexp
}

// newRedactor builds a redactor for the given exact secret values and extra patterns (invalid patterns are ignored;
// they were validated when saved).
func newRedactor(secrets []string, patterns []string) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if !redactableSecret(s) || seen[s] {
			continue
		}
		seen[s] = true
		r.secrets = append(r.secrets, s)
	}
	// Longest first, so a secret containing another one is replaced whole.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			r.custom = append(r.custom, re)
		}
	}
	return r
}

// redactableSecret decides whether an exact secret value is distinctive enough to be replaced wherever it appears:
// 8+ characters always; 5–7 characters unless it is a plain lowercase word (which would blank ordinary words such as
// "ubuntu" everywhere in the output).
func redactableSecret(s string) bool {
	n := len([]rune(s))
	if n >= 8 {
		return true
	}
	if n < 5 {
		return false
	}
	for _, c := range s {
		if !unicode.IsLower(c) {
			return true
		}
	}
	return false
}

// Redact returns text without secrets and the number of replacements.
func (r *Redactor) Redact(text string) (string, int) {
	if text == "" {
		return text, 0
	}
	n := 0
	for _, s := range r.secrets {
		if c := strings.Count(text, s); c > 0 {
			n += c
			text = strings.ReplaceAll(text, s, redactedMark)
		}
	}
	for _, rule := range builtinRedactions {
		text = applyRule(rule, text, &n)
	}
	for _, re := range r.custom {
		text = re.ReplaceAllStringFunc(text, func(string) string {
			n++
			return redactedMark
		})
	}
	return text, n
}

func applyRule(rule redactRule, text string, n *int) string {
	if rule.keep == 0 {
		return rule.re.ReplaceAllStringFunc(text, func(m string) string {
			if m == redactedMark {
				return m
			}
			*n++
			return redactedMark
		})
	}
	var b strings.Builder
	last := 0
	for _, loc := range rule.re.FindAllStringSubmatchIndex(text, -1) {
		keepEnd := loc[2*rule.keep+1]
		valStart, valEnd := keepEnd, loc[1]
		tail := ""
		if rule.tail {
			ts, te := loc[len(loc)-2], loc[len(loc)-1]
			tail, valEnd = text[ts:te], ts
		}
		val := strings.Trim(text[valStart:valEnd], `"'`)
		if val == redactedMark || strings.HasPrefix(val, "$") || val == "" {
			continue // already redacted, or a variable reference ($PASSWORD) — not a secret itself
		}
		b.WriteString(text[last:valStart])
		b.WriteString(redactedMark)
		b.WriteString(tail)
		last = loc[1]
		*n++
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// ---- secret collection --------------------------------------------------------------------------------------------

type secretCacheEntry struct {
	fp      [32]byte // hash of the sealed blobs the values were decrypted from
	values  []string
	expires time.Time
}

// secretCache keeps each user's decrypted stored secret values in memory. The sealed blobs are re-read on every
// request (cheap) and only decrypted again when they changed, so a secret saved a moment ago is redacted at once.
type secretCache struct {
	mu sync.Mutex
	m  map[string]secretCacheEntry
}

const secretCacheTTL = 10 * time.Minute

// userSecrets returns the values of the user's stored connection and identity secrets and of their stored SSH keys
// (private key + passphrase); empty while the vault is locked.
func (h *handler) userSecrets(ctx context.Context, user *model.User) []string {
	if h.d.Vault.Locked() {
		return nil
	}
	var blobs [][]byte
	var kinds []byte // 'j' = sealed JSON map, 'r' = sealed raw value
	addBlob := func(enc []byte, kind byte) {
		if len(enc) > 0 {
			blobs = append(blobs, enc)
			kinds = append(kinds, kind)
		}
	}
	if conns, err := h.d.Store.Connections.ListVisible(ctx, user.ID); err == nil {
		for _, c := range conns {
			addBlob(c.SecretsEnc, 'j')
		}
	}
	if ids, err := h.d.Store.Identities.ListByOwner(ctx, user.ID); err == nil {
		for _, i := range ids {
			addBlob(i.SecretsEnc, 'j')
		}
	}
	if keys, err := h.d.Store.Keys.ListByOwner(ctx, user.ID); err == nil {
		for _, k := range keys {
			addBlob(k.PrivateKeyEnc, 'r')
			addBlob(k.PassphraseEnc, 'r')
		}
	}
	sum := sha256.New()
	for i, b := range blobs {
		var n [9]byte
		n[0] = kinds[i]
		binary.BigEndian.PutUint64(n[1:], uint64(len(b)))
		sum.Write(n[:])
		sum.Write(b)
	}
	var fp [32]byte
	copy(fp[:], sum.Sum(nil))

	now := time.Now()
	h.secrets.mu.Lock()
	if e, ok := h.secrets.m[user.ID]; ok && e.fp == fp && now.Before(e.expires) {
		h.secrets.mu.Unlock()
		return e.values
	}
	h.secrets.mu.Unlock()

	var out []string
	for i, enc := range blobs {
		if kinds[i] == 'r' {
			if pt, err := h.d.Vault.Open(enc); err == nil && len(pt) > 0 {
				out = append(out, string(pt))
			}
			continue
		}
		m, err := h.d.Vault.OpenJSON(enc)
		if err != nil {
			continue
		}
		for _, v := range m {
			out = append(out, v)
		}
	}
	h.secrets.mu.Lock()
	if h.secrets.m == nil {
		h.secrets.m = map[string]secretCacheEntry{}
	}
	// Bound the cache (one entry per active user; expired entries are dropped opportunistically).
	for k, e := range h.secrets.m {
		if now.After(e.expires) {
			delete(h.secrets.m, k)
		}
	}
	h.secrets.m[user.ID] = secretCacheEntry{fp: fp, values: out, expires: now.Add(secretCacheTTL)}
	h.secrets.mu.Unlock()
	return out
}
