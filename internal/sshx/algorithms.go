package sshx

import (
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Algorithm selection (SSH-23/24). options.kex / ciphers / macs / hostKeyAlgorithms are ordered lists; entries may be
// comma-separated and may use ssh_config modifiers applied to the defaults: "+alg" appends, "-alg" removes, "^alg"
// prepends. options.legacyAlgorithms adds x/crypto's InsecureAlgorithms() (DH group1/group14-sha1/GEX-sha1, CBC and
// RC4 ciphers, hmac-sha1-96, ssh-rsa, ssh-dss) for old devices.

// algorithmLists returns the KEX, cipher and MAC lists for a connection (nil = library defaults).
func algorithmLists(opts model.Options) (kex, ciphers, macs []string) {
	sup, ins := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	return configuredList(opts, "kex", sup.KeyExchanges, ins.KeyExchanges),
		configuredList(opts, "ciphers", sup.Ciphers, ins.Ciphers),
		configuredList(opts, "macs", sup.MACs, ins.MACs)
}

// configuredList computes one algorithm list from options[key], options.legacyAlgorithms and the library lists.
func configuredList(opts model.Options, key string, supported, insecure []string) []string {
	legacy := opts.Bool("legacyAlgorithms")
	defaults := slices.Clone(supported)
	if legacy {
		defaults = append(defaults, insecure...)
	}
	var entries []string
	for _, e := range opts.Strings(key) {
		for part := range strings.SplitSeq(e, ",") {
			if part = strings.TrimSpace(part); part != "" {
				entries = append(entries, part)
			}
		}
	}
	if len(entries) == 0 {
		if legacy {
			return defaults
		}
		return nil
	}
	known := func(a string) bool { return slices.Contains(supported, a) || slices.Contains(insecure, a) }

	modifiers := true
	for _, e := range entries {
		if !strings.ContainsAny(e[:1], "+-^") {
			modifiers = false
			break
		}
	}
	var out []string
	if modifiers {
		out = defaults
		for _, e := range entries {
			name := e[1:]
			switch e[0] {
			case '+':
				if known(name) && !slices.Contains(out, name) {
					out = append(out, name)
				}
			case '-':
				out = slices.DeleteFunc(out, func(a string) bool { return a == name })
			case '^':
				if known(name) {
					out = slices.DeleteFunc(out, func(a string) bool { return a == name })
					out = append([]string{name}, out...)
				}
			}
		}
	} else {
		for _, e := range entries {
			if known(e) && !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AlgorithmCatalog lists the algorithms the UI can offer (GET /api/ssh/algorithms).
type AlgorithmCatalog struct {
	Supported AlgorithmSet `json:"supported"`
	Insecure  AlgorithmSet `json:"insecure"`
}

// AlgorithmSet is one group of algorithm names.
type AlgorithmSet struct {
	Kex            []string `json:"kex"`
	Ciphers        []string `json:"ciphers"`
	MACs           []string `json:"macs"`
	HostKeys       []string `json:"hostKeys"`
	PublicKeyAuths []string `json:"publicKeyAuths"`
}

// Catalog returns x/crypto's supported and insecure algorithm lists (preference order).
func Catalog() AlgorithmCatalog {
	conv := func(a ssh.Algorithms) AlgorithmSet {
		return AlgorithmSet{Kex: a.KeyExchanges, Ciphers: a.Ciphers, MACs: a.MACs, HostKeys: a.HostKeys,
			PublicKeyAuths: a.PublicKeyAuths}
	}
	return AlgorithmCatalog{Supported: conv(ssh.SupportedAlgorithms()), Insecure: conv(ssh.InsecureAlgorithms())}
}
