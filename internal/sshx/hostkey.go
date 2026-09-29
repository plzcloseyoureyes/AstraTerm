package sshx

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Host-key verification (SSH-17/18/19) against the global known_hosts table. Unknown keys raise a TOFU prompt
// (accept once / accept and save / reject) showing SHA256 and MD5 fingerprints; a changed key raises a loud
// MISMATCH prompt. Stored keys use the OpenSSH authorized_keys format ("ssh-ed25519 AAAA…") in
// known_hosts.public_key; host names are stored lower-case with the port in its own column.

// hostKeyError is returned when the user (or policy) rejected the server's host key.
type hostKeyError struct{ msg string }

func (e *hostKeyError) Error() string { return e.msg }

type hostKeyChecker struct {
	p       *Pool
	ctx     context.Context
	user    *model.User
	host    string
	port    int
	conn    *model.Connection
	session *term.Session
	dc      *deadlineConn
	label   string

	accepted    ssh.PublicKey // key accepted during the initial key exchange (re-keys must present the same key)
	fingerprint string
}

// known returns the stored keys of the host.
func (h *hostKeyChecker) known() ([]*model.KnownHost, error) {
	if h.p.d == nil || h.p.d.Store == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 10*time.Second)
	defer cancel()
	return h.p.d.Store.KnownHosts.Find(ctx, h.host, h.port)
}

// ParseKnownHostKey parses a known_hosts.public_key value (authorized_keys format or bare base64 wire format).
func ParseKnownHostKey(s string) (ssh.PublicKey, error) {
	s = strings.TrimSpace(s)
	if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s)); err == nil {
		return k, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid host key encoding: %w", err)
	}
	return ssh.ParsePublicKey(raw)
}

// FormatKnownHostKey renders a key the way it is stored in known_hosts.public_key.
func FormatKnownHostKey(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// FingerprintMD5 returns the legacy "MD5:aa:bb:…" fingerprint.
func FingerprintMD5(k ssh.PublicKey) string { return "MD5:" + ssh.FingerprintLegacyMD5(k) }

func (h *hostKeyChecker) check(_ string, _ net.Addr, key ssh.PublicKey) error {
	if done, err := h.checkMarkers(&key); done { // @revoked / @cert-authority / host certificates (cert_hostca.go)
		return err
	}
	if h.accepted != nil {
		// Re-key: the server must present the key accepted at connect time.
		if bytes.Equal(h.accepted.Marshal(), key.Marshal()) {
			return nil
		}
		return &hostKeyError{msg: "the server presented a different host key during re-keying"}
	}
	known, err := h.known()
	if err != nil {
		return fmt.Errorf("known hosts lookup failed: %w", err)
	}
	fp := ssh.FingerprintSHA256(key)
	var sameType []*model.KnownHost
	var otherTypes []string
	for _, k := range known {
		pk, err := ParseKnownHostKey(k.PublicKey)
		if err != nil {
			continue
		}
		if pk.Type() != key.Type() {
			if !slices.Contains(otherTypes, pk.Type()) {
				otherTypes = append(otherTypes, pk.Type())
			}
			continue
		}
		if bytes.Equal(pk.Marshal(), key.Marshal()) {
			h.accept(key, fp)
			return nil
		}
		sameType = append(sameType, k)
	}

	info := &model.HostKeyInfo{Host: h.host, Port: h.port, KeyType: key.Type(), Fingerprint: fp,
		FingerprintMD5: FingerprintMD5(key), Status: model.HostKeyUnknown}
	af := &authFlow{p: h.p, ctx: h.ctx, user: h.user, conn: h.conn, session: h.session, dc: h.dc, label: h.label}
	target := h.target()

	if len(sameType) > 0 {
		info.Status, info.KnownFingerprint = model.HostKeyMismatch, sameType[0].Fingerprint
		if info.KnownFingerprint == "" {
			if pk, err := ParseKnownHostKey(sameType[0].PublicKey); err == nil {
				info.KnownFingerprint = ssh.FingerprintSHA256(pk)
			}
		}
		// Replacing a trusted key affects every user, so in server mode only administrators may save it.
		allowSave := h.p.allowLocalExec(h.user)
		resp, err := af.prompt(model.Prompt{
			Kind:  model.PromptHostKey,
			Title: "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED",
			Message: fmt.Sprintf("The %s host key of %s does not match the trusted key. Someone could be eavesdropping "+
				"on you right now (man-in-the-middle attack), or the host key has just been changed. Only continue if "+
				"you know the key was legitimately replaced.\nTrusted: %s\nOffered: %s", key.Type(), target,
				info.KnownFingerprint, fp),
			HostKey:   info,
			AllowSave: allowSave,
		})
		if err != nil {
			return &hostKeyError{msg: fmt.Sprintf("host key of %s changed and could not be confirmed: %v", target, err)}
		}
		if !resp.Accept {
			return &hostKeyError{msg: fmt.Sprintf("host key verification failed: the host key of %s has changed", target)}
		}
		if resp.Save && allowSave {
			h.save(key, fp, true)
		}
		h.p.audit(h.ctx, h.user, "ssh.hostkey.mismatch_accepted", target, map[string]any{"fingerprint": fp,
			"knownFingerprint": info.KnownFingerprint, "saved": resp.Save && allowSave})
		h.accept(key, fp)
		return nil
	}

	msg := fmt.Sprintf("The authenticity of %s can't be established.\n%s key fingerprint is %s (%s).", target,
		key.Type(), fp, info.FingerprintMD5)
	if len(otherTypes) > 0 {
		msg += fmt.Sprintf("\nNote: this host already has a trusted %s key, but offered a different key type.",
			strings.Join(otherTypes, ", "))
	}
	resp, err := af.prompt(model.Prompt{
		Kind:      model.PromptHostKey,
		Title:     "Unknown host key",
		Message:   msg,
		HostKey:   info,
		AllowSave: true,
	})
	if err != nil {
		return &hostKeyError{msg: fmt.Sprintf("host key of %s is not trusted and could not be confirmed: %v", target, err)}
	}
	if !resp.Accept {
		return &hostKeyError{msg: fmt.Sprintf("host key of %s was rejected", target)}
	}
	if resp.Save {
		h.save(key, fp, false)
	}
	h.accept(key, fp)
	return nil
}

func (h *hostKeyChecker) accept(key ssh.PublicKey, fp string) {
	h.accepted, h.fingerprint = key, fp
}

func (h *hostKeyChecker) target() string {
	if h.port == 22 {
		return h.host
	}
	return fmt.Sprintf("[%s]:%d", h.host, h.port)
}

func (h *hostKeyChecker) save(key ssh.PublicKey, fp string, replace bool) {
	if h.p.d == nil || h.p.d.Store == nil {
		return
	}
	who := ""
	if h.user != nil {
		who = h.user.Username
	}
	kh := &model.KnownHost{
		Host:        h.host,
		Port:        h.port,
		KeyType:     key.Type(),
		PublicKey:   FormatKnownHostKey(key),
		Fingerprint: fp,
		Comment:     fmt.Sprintf("accepted by %s on %s", who, time.Now().UTC().Format("2006-01-02")),
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 10*time.Second)
	defer cancel()
	var err error
	if replace {
		err = h.p.d.Store.KnownHosts.Replace(ctx, kh)
	} else {
		err = h.p.d.Store.KnownHosts.Add(ctx, kh)
	}
	if err != nil {
		h.p.log.Warn("ssh: saving host key failed", "host", h.host, "port", h.port, "err", err)
		return
	}
	action := "known_host.add"
	if replace {
		action = "known_host.replace"
	}
	h.p.audit(h.ctx, h.user, action, h.target(), map[string]any{"keyType": key.Type(), "fingerprint": fp})
}

// hostKeyAlgorithms orders the host-key algorithms so the server presents a key type we already trust (avoids false
// "changed key" alarms, SSH-18), honoring options.hostKeyAlgorithms and options.legacyAlgorithms.
func hostKeyAlgorithms(known []*model.KnownHost, opts model.Options) []string {
	base := configuredList(opts, "hostKeyAlgorithms", ssh.SupportedAlgorithms().HostKeys, ssh.InsecureAlgorithms().HostKeys)
	if len(known) == 0 {
		return base // nil = library defaults
	}
	if base == nil {
		base = ssh.SupportedAlgorithms().HostKeys
	}
	var preferred []string
	for _, k := range known {
		t := k.KeyType
		if pk, err := ParseKnownHostKey(k.PublicKey); err == nil {
			t = pk.Type()
		}
		for _, a := range algorithmsForKeyType(t) {
			if slices.Contains(base, a) && !slices.Contains(preferred, a) {
				preferred = append(preferred, a)
			}
		}
	}
	out := append([]string(nil), preferred...)
	for _, a := range base {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func algorithmsForKeyType(t string) []string {
	switch t {
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	case ssh.CertAlgoRSAv01:
		return []string{ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSAv01}
	}
	return []string{t}
}
