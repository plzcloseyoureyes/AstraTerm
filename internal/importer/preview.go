package importer

import (
	"context"
	"errors"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/sshx"
)

// buildPreview turns a parsed import into the JSON preview, marking connections that already exist for the user.
func buildPreview(ctx context.Context, d *app.Deps, user *model.User, p *parsed) (previewResponse, error) {
	resp := previewResponse{
		Format:      p.format,
		Folders:     make([]previewFolder, 0, len(p.folders)),
		Connections: make([]previewConnection, 0, len(p.conns)),
		Warnings:    append([]string{}, p.warnings...),
		Duplicates:  []duplicateRef{},
	}

	// Existing connections (for duplicate detection).
	dupByKey := map[string]*model.Connection{}
	conns, err := d.Store.Connections.ListByOwner(ctx, user.ID)
	if err != nil {
		return resp, err
	}
	for _, c := range conns {
		dupByKey[dupKey(c.Name, c.Protocol, c.Host, c.Port, c.Username)] = c
	}
	// Existing keys / known hosts (for duplicate flags).
	existingFP := map[string]bool{}
	if keys, err := d.Store.Keys.ListByOwner(ctx, user.ID); err == nil {
		for _, k := range keys {
			existingFP[k.Fingerprint] = true
		}
	}
	trusted := map[string]map[string]bool{}
	if khs, err := d.Store.KnownHosts.List(ctx); err == nil {
		for _, kh := range khs {
			slot := knownHostSlot(kh.Host, kh.Port, kh.KeyType)
			if trusted[slot] == nil {
				trusted[slot] = map[string]bool{}
			}
			trusted[slot][normalizePubKey(kh.PublicKey)] = true
		}
	}

	for _, f := range p.folders {
		resp.Folders = append(resp.Folders, previewFolder{
			ID: f.tempID, ParentID: f.parentID, Name: f.name, Icon: f.icon, Color: f.color,
		})
	}

	desktop := d.Cfg != nil && d.Cfg.IsDesktop()
	keyByTemp := map[string]*pkey{}
	for _, k := range p.keys {
		keyByTemp[k.tempID] = k
	}
	connByTemp := map[string]*pconn{}
	for _, pc := range p.conns {
		connByTemp[pc.tempID] = pc
	}

	for _, pc := range p.conns {
		c := pc.conn
		c.Normalize()
		pv := previewConnection{
			ID: pc.tempID, FolderID: pc.folderID, Name: c.Name, Protocol: c.Protocol, Host: c.Host, Port: c.Port,
			Username: c.Username, AuthMethod: c.AuthMethod, Options: c.Options, SecretKeys: sortedSecretKeys(pc.secrets),
			Icon: c.Icon, Color: c.Color, Tags: c.Tags, Notes: c.Notes, Warnings: append([]string{}, pc.warnings...),
			RunsLocalCommand: runsLocalCommand(&c),
		}
		if k := keyByTemp[pc.keyRef]; k != nil {
			pv.KeyName = k.name
		} else if pc.keyPath != "" && desktop {
			pv.KeyName = pc.keyPath
		}
		for _, h := range pc.hops {
			switch {
			case h.ref != "":
				if rc := connByTemp[h.ref]; rc != nil {
					pv.Via = append(pv.Via, rc.conn.Name)
				}
			case h.existingID != "":
				pv.Via = append(pv.Via, "saved connection "+h.existingID)
			default:
				pv.Via = append(pv.Via, h.spec())
			}
		}
		if len(pc.hops) > 0 && !isSSHFamily(c.Protocol) {
			pv.Warnings = append(pv.Warnings, "reached through an SSH gateway: a saved SSH connection is created for it (or an equivalent one reused)")
		}
		if existing, ok := dupByKey[dupKey(c.Name, c.Protocol, c.Host, c.Port, c.Username)]; ok {
			pv.Duplicate = true
			pv.DuplicateOf = existing.ID
			resp.Duplicates = append(resp.Duplicates, duplicateRef{
				ID: pc.tempID, Name: c.Name, ExistingID: existing.ID, ExistingName: existing.Name,
			})
		}
		resp.Connections = append(resp.Connections, pv)
	}

	// Keys (desktop mode only reads files; encrypted JSON exports carry material inline).
	for _, k := range p.keys {
		pk := previewKey{ID: k.tempID, Name: k.name}
		signer, err := sshx.ParsePrivateKey(k.text, k.pass)
		var pub ssh.PublicKey
		if err == nil {
			pub = signer.PublicKey()
		} else {
			var np *sshx.NeedsPassphraseError
			if errors.As(err, &np) {
				pk.Encrypted, pub = true, np.PublicKey
			}
		}
		if pub != nil {
			pk.Type, pk.Bits = keyTypeBits(pub)
			pk.Fingerprint = ssh.FingerprintSHA256(pub)
			pk.Duplicate = existingFP[pk.Fingerprint]
		}
		resp.Keys = append(resp.Keys, pk)
	}

	for _, kh := range p.knownHosts {
		slot := knownHostSlot(kh.host, kh.port, kh.keyType)
		dup := trusted[slot][normalizePubKey(kh.publicKey)]
		resp.KnownHosts = append(resp.KnownHosts, previewKnownHost{
			Host: kh.host, Port: kh.port, KeyType: kh.keyType, Fingerprint: kh.fingerprint,
			Duplicate: dup, Conflict: !dup && len(trusted[slot]) > 0,
		})
	}

	resp.Counts = previewCounts{
		Folders:     len(resp.Folders),
		Connections: len(resp.Connections),
		Keys:        len(resp.Keys),
		KnownHosts:  len(resp.KnownHosts),
		Duplicates:  len(resp.Duplicates),
		Unsupported: p.unsupported,
		Identities:  len(p.identities),
		Snippets:    len(p.snippets),
	}
	return resp, nil
}
