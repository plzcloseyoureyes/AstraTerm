package importer

import (
	"context"
	"crypto/dsa" //nolint:staticcheck // legacy imported keys may still be DSA
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// maxKeyFileBytes bounds a key file read from disk (the keys module's limit for key text).
const maxKeyFileBytes = 64 << 10

// committer applies a parsed import to the store for one user.
type committer struct {
	ctx        context.Context
	d          *app.Deps
	user       *model.User
	req        commitRequest
	p          *parsed
	desktop    bool
	vaultOK    bool
	importKeys bool

	target       string // target folder id ("" = root)
	folderReal   map[string]string
	folderByTemp map[string]*pfolder
	connByTemp   map[string]*pconn
	connReal     map[string]string // temp connection id → real id (created, updated, or the duplicate it matched)
	keyReal      map[string]string
	identReal    map[string]string
	keyByPath    map[string]string
	gatewayByKey map[string]string // gateway spec + chain → saved SSH connection id

	existingFolderByKey map[string]string            // realParent+"\x00"+lower(name) → folder id
	existingKeyByFP     map[string]*model.SSHKey     // fingerprint → key
	dupByKey            map[string]*model.Connection // dupKey → existing connection
	sshByAddr           map[string][]*model.Connection

	res commitResponse
}

// commitImport creates the selected connections (and their folders/keys/identities/known-hosts) under targetFolderId.
func commitImport(ctx context.Context, d *app.Deps, user *model.User, p *parsed, req commitRequest) (commitResponse, error) {
	cm := &committer{
		ctx: ctx, d: d, user: user, req: req, p: p,
		desktop:      d.Cfg != nil && d.Cfg.IsDesktop(),
		vaultOK:      d.Vault != nil && !d.Vault.Locked(),
		importKeys:   boolOr(req.ImportKeys, true),
		target:       strings.TrimSpace(req.TargetFolderID),
		folderReal:   map[string]string{},
		folderByTemp: map[string]*pfolder{},
		connByTemp:   map[string]*pconn{},
		connReal:     map[string]string{},
		keyReal:      map[string]string{},
		identReal:    map[string]string{},
		keyByPath:    map[string]string{},
		gatewayByKey: map[string]string{},
		res:          commitResponse{ConnectionIDs: []string{}, FolderIDs: []string{}},
	}
	for _, f := range p.folders {
		cm.folderByTemp[f.tempID] = f
	}
	for _, pc := range p.conns {
		cm.connByTemp[pc.tempID] = pc
	}
	if err := cm.validateTarget(); err != nil {
		return commitResponse{}, err
	}
	if err := cm.loadExisting(); err != nil {
		return commitResponse{}, err
	}
	selected := cm.selection()

	// 1. Keys with inline material (encrypted native JSON). Desktop-mode key files are read lazily per connection.
	if cm.importKeys {
		for _, k := range p.keys {
			if len(k.text) == 0 {
				continue
			}
			id, warn := cm.importKeyText(k.name, k.text, k.pass)
			cm.warn(warn)
			if id != "" {
				cm.keyReal[k.tempID] = id
			}
		}
	}

	// 2. Identities (native JSON).
	for _, id := range p.identities {
		rid, err := cm.importIdentity(id)
		if err != nil {
			if errors.Is(err, httpx.ErrLocked) {
				return commitResponse{}, err
			}
			cm.warn(fmt.Sprintf("identity %q: %v", id.name, errMsg(err)))
			continue
		}
		cm.identReal[id.tempID] = rid
	}

	// 3. Connections, referenced ones first (jump hosts / gateways must exist before the connections using them).
	for _, pc := range cm.importOrder(selected) {
		if err := cm.importConnection(pc); err != nil {
			if errors.Is(err, httpx.ErrLocked) {
				return commitResponse{}, err
			}
			cm.warn(fmt.Sprintf("%q: %v", pc.conn.Name, errMsg(err)))
		}
	}

	// 4. Snippets (native JSON).
	for _, s := range p.snippets {
		if err := cm.importSnippet(s); err != nil {
			cm.warn(fmt.Sprintf("snippet %q: %v", s.name, errMsg(err)))
		} else {
			cm.res.SnippetsCreated++
		}
	}

	// 5. Known hosts.
	if boolOr(req.ImportKnown, true) && len(p.knownHosts) > 0 {
		if err := cm.importKnownHosts(); err != nil {
			cm.warn(errMsg(err))
		}
	}
	return cm.res, nil
}

func (cm *committer) warn(w string) {
	if w != "" && len(cm.res.Warnings) < 500 {
		cm.res.Warnings = append(cm.res.Warnings, w)
	}
}

// selection returns the selected temp ids: absent selectedIds = everything, an empty list = nothing.
func (cm *committer) selection() map[string]bool {
	sel := map[string]bool{}
	if cm.req.SelectedIDs == nil {
		for _, pc := range cm.p.conns {
			sel[pc.tempID] = true
		}
		return sel
	}
	for _, id := range cm.req.SelectedIDs {
		sel[id] = true
	}
	return sel
}

// importOrder returns the selected connections with every referenced (selected) connection before its users.
func (cm *committer) importOrder(selected map[string]bool) []*pconn {
	state := map[string]int{} // 1 visiting, 2 done
	out := make([]*pconn, 0, len(selected))
	var visit func(pc *pconn)
	visit = func(pc *pconn) {
		if state[pc.tempID] != 0 {
			return // done, or a reference cycle (the back-reference is inlined)
		}
		state[pc.tempID] = 1
		deps := make([]string, 0, len(pc.hops)+len(pc.refOpts))
		for _, h := range pc.hops {
			deps = append(deps, h.ref)
		}
		for _, r := range pc.refOpts {
			deps = append(deps, r)
		}
		for _, ref := range deps {
			if rc := cm.connByTemp[ref]; rc != nil && selected[ref] {
				visit(rc)
			}
		}
		state[pc.tempID] = 2
		out = append(out, pc)
	}
	for _, pc := range cm.p.conns {
		if selected[pc.tempID] {
			visit(pc)
		}
	}
	return out
}

func (cm *committer) validateTarget() error {
	if cm.target == "" {
		return nil
	}
	f, err := cm.d.Store.Folders.Get(cm.ctx, cm.target)
	if errors.Is(err, model.ErrNotFound) {
		return httpx.BadRequest("target folder not found")
	}
	if err != nil {
		return err
	}
	if !app.Visible(cm.user, f.OwnerID, f.Shared) || !app.CanModify(cm.user, f.OwnerID) {
		return httpx.BadRequest("target folder not found")
	}
	return nil
}

func (cm *committer) loadExisting() error {
	folders, err := cm.d.Store.Folders.ListByOwner(cm.ctx, cm.user.ID)
	if err != nil {
		return err
	}
	cm.existingFolderByKey = map[string]string{}
	for _, f := range folders {
		cm.existingFolderByKey[f.ParentID+"\x00"+strings.ToLower(f.Name)] = f.ID
	}
	conns, err := cm.d.Store.Connections.ListByOwner(cm.ctx, cm.user.ID)
	if err != nil {
		return err
	}
	cm.dupByKey = map[string]*model.Connection{}
	cm.sshByAddr = map[string][]*model.Connection{}
	for _, c := range conns {
		cm.dupByKey[dupKey(c.Name, c.Protocol, c.Host, c.Port, c.Username)] = c
		cm.indexSSH(c)
	}
	keys, err := cm.d.Store.Keys.ListByOwner(cm.ctx, cm.user.ID)
	if err != nil {
		return err
	}
	cm.existingKeyByFP = map[string]*model.SSHKey{}
	for _, k := range keys {
		if k.Fingerprint != "" {
			cm.existingKeyByFP[k.Fingerprint] = k
		}
	}
	return nil
}

func addrKey(host string, port int, user string) string {
	if port == 0 {
		port = 22
	}
	return strings.ToLower(host) + "\x00" + strconv.Itoa(port) + "\x00" + strings.ToLower(user)
}

func (cm *committer) indexSSH(c *model.Connection) {
	if c.Protocol == model.ProtoSSH {
		k := addrKey(c.Host, c.Port, c.Username)
		cm.sshByAddr[k] = append(cm.sshByAddr[k], c)
	}
}

// realFolder returns the real folder id for a temp folder id, creating the folder chain (deduped by name) on demand.
func (cm *committer) realFolder(tempID string) (string, error) {
	return cm.realFolderDepth(tempID, 0)
}

func (cm *committer) realFolderDepth(tempID string, depth int) (string, error) {
	if tempID == "" || depth > 256 {
		return cm.target, nil
	}
	if id, ok := cm.folderReal[tempID]; ok {
		return id, nil
	}
	pf := cm.folderByTemp[tempID]
	if pf == nil {
		return cm.target, nil
	}
	parentReal, err := cm.realFolderDepth(pf.parentID, depth+1)
	if err != nil {
		return "", err
	}
	dk := parentReal + "\x00" + strings.ToLower(pf.name)
	if existing, ok := cm.existingFolderByKey[dk]; ok {
		cm.folderReal[tempID] = existing
		return existing, nil
	}
	f := &model.Folder{OwnerID: cm.user.ID, ParentID: parentReal, Name: truncate(pf.name, maxNameRunes),
		Icon: cleanIcon(pf.icon), Color: cleanColor(pf.color)}
	if err := cm.d.Store.Folders.Create(cm.ctx, f); err != nil {
		return "", err
	}
	cm.res.FoldersCreated++
	cm.res.FolderIDs = append(cm.res.FolderIDs, f.ID)
	cm.folderReal[tempID] = f.ID
	cm.existingFolderByKey[dk] = f.ID
	return f.ID, nil
}

func (cm *committer) importConnection(pc *pconn) error {
	dk := dupKey(pc.conn.Name, pc.conn.Protocol, pc.conn.Host, pc.conn.Port, pc.conn.Username)
	existing, isDup := cm.dupByKey[dk]
	if isDup && cm.dedupe() == dedupeSkip {
		cm.res.Skipped++
		cm.connReal[pc.tempID] = existing.ID // references to it (jump hosts) use the existing connection
		return nil
	}
	folderID, err := cm.realFolder(pc.folderID)
	if err != nil {
		return err
	}
	c := pc.conn
	c.Options = pc.conn.Options.Clone()
	if c.Options == nil {
		c.Options = model.Options{}
	}
	c.Tags = slices.Clone(pc.conn.Tags)
	// Resolve a key reference / key file into a real key id (best effort).
	keyID := ""
	if cm.importKeys {
		if pc.keyRef != "" {
			keyID = cm.keyReal[pc.keyRef]
		} else if pc.keyPath != "" && cm.desktop {
			keyID = cm.importKeyFromPath(pc.keyPath)
		}
	}
	identityID := ""
	if pc.identityRef != "" {
		identityID = cm.identReal[pc.identityRef]
	}
	if keyID == "" && identityID == "" && c.AuthMethod == model.AuthKey {
		// The key the source used is not available here: "key" alone would fail with "no private key is configured",
		// "auto" still tries the agent, then password / keyboard-interactive.
		c.AuthMethod = model.AuthAuto
	}
	for k, ref := range pc.refOpts {
		if id := cm.resolveRef(ref); id != "" {
			c.Options[k] = id
		} else {
			cm.warn(fmt.Sprintf("%q: the connection referenced by %s is not available", c.Name, k))
		}
	}
	cm.applyHops(pc, &c, folderID)
	if err := sanitizeConn(&c); err != nil {
		return err
	}

	if isDup {
		switch cm.dedupe() {
		case dedupeUpdate:
			return cm.updateExisting(existing, pc, &c, keyID, identityID)
		case dedupeDuplicate:
			c.Name = truncate(c.Name+" (imported)", maxNameRunes)
			dk = dupKey(c.Name, c.Protocol, c.Host, c.Port, c.Username)
		}
	}
	c.ID = ""
	c.OwnerID = cm.user.ID
	c.FolderID = folderID
	c.KeyID = keyID
	c.IdentityID = identityID
	c.Shared = false
	c.Secrets = nil
	c.SecretsEnc = nil
	c.SecretKeys = nil
	c.SortOrder = 0
	if keyID != "" && c.AuthMethod == model.AuthAuto {
		c.AuthMethod = model.AuthKey
	}
	c.Normalize()
	if err := cm.sealSecrets(&c, pc.secrets); err != nil {
		return err
	}
	if err := cm.d.Store.Connections.Create(cm.ctx, &c); err != nil {
		return err
	}
	cm.res.Created++
	cm.res.ConnectionIDs = append(cm.res.ConnectionIDs, c.ID)
	cm.connReal[pc.tempID] = c.ID
	cm.dupByKey[dk] = &c
	cm.indexSSH(&c)
	return nil
}

// resolveRef maps a reference (temp id of this import, or "id:<existing id>") to a real connection id visible to
// the user ("" when unavailable).
func (cm *committer) resolveRef(ref string) string {
	if id, ok := strings.CutPrefix(ref, "id:"); ok {
		if cm.visibleConnection(id) {
			return id
		}
		return ""
	}
	return cm.connReal[ref]
}

func (cm *committer) visibleConnection(id string) bool {
	c, err := cm.d.Store.Connections.Get(cm.ctx, id)
	return err == nil && app.Visible(cm.user, c.OwnerID, c.Shared)
}

// applyHops turns the parsed jump chain into options: SSH-family protocols get options.jumpHosts (connection ids for
// hops that are, or need to be, saved connections — referenced aliases, hops with their own key — and ad-hoc
// "[user@]host[:port]" specs otherwise); other protocols reach the host through options.sshTunnelVia, a saved SSH
// connection for the last hop (reused when the user already has an equivalent one, else created next to the
// connection), whose own jumpHosts carry the earlier hops.
func (cm *committer) applyHops(pc *pconn, c *model.Connection, folderID string) {
	if len(pc.hops) == 0 {
		return
	}
	sshFamily := isSSHFamily(c.Protocol)
	var entries []string
	for i, h := range pc.hops {
		last := i == len(pc.hops)-1
		switch {
		case h.existingID != "":
			if cm.visibleConnection(h.existingID) {
				entries = append(entries, h.existingID)
			} else {
				cm.warn(fmt.Sprintf("%q: a jump host / gateway it used no longer exists and was dropped", c.Name))
			}
			continue
		case h.ref != "":
			if id := cm.connReal[h.ref]; id != "" {
				entries = append(entries, id)
				continue
			}
			rc := cm.connByTemp[h.ref]
			if rc == nil {
				continue
			}
			// The referenced connection was not imported (deselected or failed): use its address inline.
			h = phop{host: rc.conn.Host, port: rc.conn.Port, user: rc.conn.Username, keyPath: rc.keyPath}
		}
		if h.host == "" {
			continue
		}
		// A hop with its own private key needs a saved connection to hold that key; only worth it when the key file
		// can actually be imported (desktop mode, readable).
		keyID := ""
		if h.keyPath != "" && cm.desktop && cm.importKeys {
			keyID = cm.importKeyFromPath(h.keyPath)
		}
		if h.keyPath != "" && keyID == "" {
			cm.warn(fmt.Sprintf("%q: the private key of jump host %s (%s) was not imported", c.Name, h.spec(), h.keyPath))
		}
		if (!sshFamily && last) || keyID != "" {
			chain := []string(nil)
			if !sshFamily && last {
				chain = slices.Clone(entries)
			}
			id, err := cm.gatewayConnection(h, chain, keyID, folderID)
			if err == nil {
				entries = append(entries, id)
				continue
			}
			cm.warn(fmt.Sprintf("%q: could not save its SSH gateway %s: %v", c.Name, h.spec(), errMsg(err)))
			if !sshFamily {
				return
			}
		}
		entries = append(entries, h.spec())
	}
	if len(entries) == 0 {
		return
	}
	if sshFamily {
		c.Options["jumpHosts"] = entries
		return
	}
	c.Options["sshTunnelVia"] = entries[len(entries)-1]
}

// gatewayConnection returns a saved SSH connection for hop h (with the given earlier hops as its own jump chain):
// one created earlier in this import, an equivalent existing connection of the user, or a new one.
func (cm *committer) gatewayConnection(h phop, chain []string, keyID, folderID string) (string, error) {
	port := h.port
	if port <= 0 {
		port = 22
	}
	key := strings.ToLower(h.spec()) + "\x00" + strings.Join(chain, ",")
	if id := cm.gatewayByKey[key]; id != "" {
		return id, nil
	}
	for _, ex := range cm.sshByAddr[addrKey(h.host, port, h.user)] {
		if slices.Equal(ex.Options.Strings("jumpHosts"), chain) || len(chain) == 0 && len(ex.Options.Strings("jumpHosts")) == 0 {
			if keyID != "" && ex.KeyID == "" && ex.OwnerID == cm.user.ID && ex.AuthMethod == model.AuthAuto {
				upd := ex.Clone()
				upd.KeyID, upd.AuthMethod = keyID, model.AuthKey
				if err := cm.d.Store.Connections.Update(cm.ctx, upd); err == nil {
					*ex = *upd
				}
			}
			cm.gatewayByKey[key] = ex.ID
			return ex.ID, nil
		}
	}
	c := model.Connection{
		Name:     cleanName(h.spec()+" (SSH gateway)", h.host),
		Protocol: model.ProtoSSH,
		Host:     h.host,
		Port:     port,
		Username: h.user,
		Options:  model.Options{},
		OwnerID:  cm.user.ID,
		FolderID: folderID,
		Notes:    "SSH gateway created by an import.",
	}
	if len(chain) > 0 {
		c.Options["jumpHosts"] = chain
	}
	if keyID != "" {
		c.KeyID, c.AuthMethod = keyID, model.AuthKey
	}
	if err := sanitizeConn(&c); err != nil {
		return "", err
	}
	if err := cm.d.Store.Connections.Create(cm.ctx, &c); err != nil {
		return "", err
	}
	cm.res.GatewaysCreated++
	cm.res.ConnectionIDs = append(cm.res.ConnectionIDs, c.ID)
	cm.gatewayByKey[key] = c.ID
	cm.dupByKey[dupKey(c.Name, c.Protocol, c.Host, c.Port, c.Username)] = &c
	cm.indexSSH(&c)
	return c.ID, nil
}

// updateExisting refreshes an existing connection from the import (dedupe "update"): address, login, protocol
// options and the key/identity/secrets it brings; the user's own organisation (folder, name, colour, icon, favourite,
// order, tags) is kept.
func (cm *committer) updateExisting(existing *model.Connection, pc *pconn, c *model.Connection, keyID, identityID string) error {
	if !app.CanModify(cm.user, existing.OwnerID) {
		cm.res.Skipped++
		cm.connReal[pc.tempID] = existing.ID
		return nil
	}
	upd := existing.Clone()
	upd.Host, upd.Port, upd.Username, upd.Protocol = c.Host, c.Port, c.Username, c.Protocol
	if c.Notes != "" {
		upd.Notes = c.Notes
	}
	if keyID != "" {
		upd.KeyID = keyID
	}
	if identityID != "" {
		upd.IdentityID = identityID
	}
	if upd.Options == nil {
		upd.Options = model.Options{}
	}
	// imported keys win; the user's other options stay
	maps.Copy(upd.Options, c.Options)
	if c.AuthMethod != "" && c.AuthMethod != model.AuthAuto {
		upd.AuthMethod = c.AuthMethod
	}
	if err := sanitizeConn(upd); err != nil {
		return err
	}
	if err := cm.sealSecrets(upd, pc.secrets); err != nil {
		return err
	}
	if err := cm.d.Store.Connections.Update(cm.ctx, upd); err != nil {
		return err
	}
	cm.res.Updated++
	cm.res.ConnectionIDs = append(cm.res.ConnectionIDs, upd.ID)
	cm.connReal[pc.tempID] = upd.ID
	return nil
}

// sealSecrets merges plaintext secrets into the connection's sealed storage (vault must be unlocked). With no
// secrets, it is a no-op. When the vault is locked and there are secrets, it records a warning and drops them.
func (cm *committer) sealSecrets(c *model.Connection, secrets map[string]string) error {
	secrets = cleanSecrets(secrets)
	if len(secrets) == 0 {
		return nil
	}
	if !cm.vaultOK {
		cm.warn(fmt.Sprintf("%q: the vault is locked — imported without its stored secrets", c.Name))
		return nil
	}
	current := map[string]string{}
	if len(c.SecretsEnc) > 0 {
		m, err := cm.d.Vault.OpenJSON(c.SecretsEnc)
		if err != nil {
			return err
		}
		current = m
	}
	maps.Copy(current, secrets)
	enc, err := cm.d.Vault.SealJSON(current)
	if err != nil {
		return err
	}
	c.SecretsEnc = enc
	c.SecretKeys = model.SortedKeys(current)
	return nil
}

func (cm *committer) dedupe() string {
	switch cm.req.Dedupe {
	case dedupeUpdate, dedupeDuplicate:
		return cm.req.Dedupe
	default:
		return dedupeSkip
	}
}

// importIdentity creates an identity (native JSON import) and returns its id.
func (cm *committer) importIdentity(pi *pidentity) (string, error) {
	i := &model.Identity{OwnerID: cm.user.ID, Name: cleanName(pi.name, pi.username), Username: pi.username}
	if pi.keyRef != "" {
		i.KeyID = cm.keyReal[pi.keyRef]
	}
	if secrets := cleanSecrets(pi.secrets); len(secrets) > 0 {
		if !cm.vaultOK {
			cm.warn(fmt.Sprintf("identity %q imported without its secrets (vault locked)", pi.name))
		} else {
			enc, err := cm.d.Vault.SealJSON(secrets)
			if err != nil {
				return "", err
			}
			i.SecretsEnc = enc
			i.SecretKeys = model.SortedKeys(secrets)
		}
	}
	i.Normalize()
	if err := cm.d.Store.Identities.Create(cm.ctx, i); err != nil {
		return "", err
	}
	cm.res.IdentitiesCreated++
	return i.ID, nil
}

func (cm *committer) importSnippet(s *psnippet) error {
	sn := &model.Snippet{
		OwnerID: cm.user.ID, Name: cleanName(s.name, "Snippet"), Folder: s.folder, Description: s.description,
		Content: s.content, Tags: cleanTags(s.tags), Shortcut: s.shortcut, SendMode: s.sendMode,
	}
	if sn.SendMode != model.SendModeExecute {
		sn.SendMode = model.SendModePaste
	}
	if sn.Tags == nil {
		sn.Tags = []string{}
	}
	return cm.d.Store.Snippets.Create(cm.ctx, sn)
}

// importKeyFromPath reads and imports a key file (desktop mode), caching by path. Returns "" on any failure.
func (cm *committer) importKeyFromPath(path string) string {
	path = expandTilde(path)
	if id, ok := cm.keyByPath[path]; ok {
		return id
	}
	cm.keyByPath[path] = "" // negative cache by default
	data, err := readFileLimited(path, maxKeyFileBytes)
	if err != nil {
		cm.warn(fmt.Sprintf("could not read key file %s", path))
		return ""
	}
	name := path
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		name = path[i+1:]
	}
	id, warn := cm.importKeyText(name, data, "")
	cm.warn(warn)
	cm.keyByPath[path] = id
	return id
}

// importKeyText validates and stores a private key, deduping by fingerprint. Returns the key id (or "") and a warning.
func (cm *committer) importKeyText(name string, text []byte, passphrase string) (string, string) {
	if len(text) > maxKeyFileBytes {
		return "", fmt.Sprintf("key %q is too large to be a private key", name)
	}
	signer, err := sshx.ParsePrivateKey(text, passphrase)
	var pub ssh.PublicKey
	encrypted := false
	if err != nil {
		if np, ok := errors.AsType[*sshx.NeedsPassphraseError](err); ok {
			encrypted = true
			pub = np.PublicKey
			if pub == nil {
				return "", fmt.Sprintf("key %q is encrypted and could not be stored (unknown public key)", name)
			}
		} else {
			return "", fmt.Sprintf("key %q could not be parsed: %v", name, errMsg(err))
		}
	} else {
		pub = signer.PublicKey()
	}
	fp := ssh.FingerprintSHA256(pub)
	if existing, ok := cm.existingKeyByFP[fp]; ok {
		return existing.ID, "" // already stored — reuse
	}
	if !cm.vaultOK {
		return "", fmt.Sprintf("key %q not imported (vault is locked)", name)
	}
	sealed, err := cm.d.Vault.Seal(append([]byte(nil), text...))
	if err != nil {
		return "", fmt.Sprintf("key %q could not be stored: %v", name, errMsg(err))
	}
	typ, bits := keyTypeBits(pub)
	k := &model.SSHKey{
		OwnerID:       cm.user.ID,
		Name:          cleanName(name, typ+" key"),
		Type:          typ,
		Bits:          bits,
		PublicKey:     strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint:   fp,
		HasPassphrase: encrypted || passphrase != "",
		PrivateKeyEnc: sealed,
	}
	if passphrase != "" {
		if penc, err := cm.d.Vault.Seal([]byte(passphrase)); err == nil {
			k.PassphraseEnc = penc
		}
	}
	if err := cm.d.Store.Keys.Create(cm.ctx, k); err != nil {
		return "", fmt.Sprintf("key %q could not be stored: %v", name, errMsg(err))
	}
	cm.existingKeyByFP[fp] = k
	cm.res.KeysImported++
	return k.ID, ""
}

// importKnownHosts adds the parsed host keys to the global store. A host whose key of that type is already trusted
// with a DIFFERENT key is left alone (counted as a conflict): an import must never silently replace or add a
// competing trusted key — changed keys are handled by the host-key prompt or Keys → Known hosts.
func (cm *committer) importKnownHosts() error {
	// Known hosts are global; in server mode only admins may change them (matches the keys module).
	if cm.d.Cfg != nil && cm.d.Cfg.IsServer() && !cm.user.IsAdmin() {
		return errors.New("only administrators can import trusted host keys in server mode")
	}
	existing, err := cm.d.Store.KnownHosts.List(cm.ctx)
	if err != nil {
		return err
	}
	have := map[string]map[string]bool{} // host/port/type → normalized keys
	for _, kh := range existing {
		k := knownHostSlot(kh.Host, kh.Port, kh.KeyType)
		if have[k] == nil {
			have[k] = map[string]bool{}
		}
		have[k][normalizePubKey(kh.PublicKey)] = true
	}
	var conflicts []string
	for _, kh := range cm.p.knownHosts {
		slot := knownHostSlot(kh.host, kh.port, kh.keyType)
		norm := normalizePubKey(kh.publicKey)
		if have[slot][norm] {
			continue // already trusted
		}
		if len(have[slot]) > 0 {
			cm.res.KnownHostsConflicts++
			if len(conflicts) < 5 {
				conflicts = append(conflicts, knownHostLabel(kh.host, kh.port))
			}
			continue
		}
		row := &model.KnownHost{Host: kh.host, Port: kh.port, KeyType: kh.keyType, PublicKey: kh.publicKey,
			Fingerprint: kh.fingerprint, Comment: kh.comment}
		if err := cm.d.Store.KnownHosts.Add(cm.ctx, row); err != nil {
			continue
		}
		have[slot] = map[string]bool{norm: true}
		cm.res.KnownHostsAdded++
	}
	if cm.res.KnownHostsConflicts > 0 {
		cm.warn(fmt.Sprintf("%d host key(s) differ from the key already trusted for that host and were not imported (%s) — review them in Keys → Known hosts",
			cm.res.KnownHostsConflicts, strings.Join(conflicts, ", ")))
	}
	if cm.res.KnownHostsAdded > 0 {
		cm.d.Audit.Log(cm.ctx, "known_hosts.import", "", map[string]any{"added": cm.res.KnownHostsAdded,
			"conflicts": cm.res.KnownHostsConflicts, "source": "importer"})
	}
	return nil
}

func knownHostSlot(host string, port int, keyType string) string {
	return strings.ToLower(host) + "\x00" + strconv.Itoa(port) + "\x00" + keyType
}

func knownHostLabel(host string, port int) string {
	if port == 22 {
		return host
	}
	return "[" + host + "]:" + strconv.Itoa(port)
}

// normalizePubKey reduces an authorized_keys line to its base64 blob for comparison.
func normalizePubKey(s string) string {
	fields := strings.Fields(s)
	if len(fields) >= 2 {
		return fields[1]
	}
	return strings.TrimSpace(s)
}

// keyTypeBits derives a human key type and bit size from a public key.
func keyTypeBits(pub ssh.PublicKey) (string, int) {
	if pub == nil {
		return "", 0
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519:
		return "ed25519", 256
	case ssh.KeyAlgoSKED25519:
		return "ed25519-sk", 256
	case ssh.KeyAlgoSKECDSA256:
		return "ecdsa-sk", 256
	}
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return pub.Type(), 0
	}
	switch k := cpk.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		return "rsa", k.N.BitLen()
	case *ecdsa.PublicKey:
		return "ecdsa", k.Curve.Params().BitSize
	case *dsa.PublicKey:
		return "dsa", k.P.BitLen()
	case ed25519.PublicKey:
		return "ed25519", 256
	}
	return pub.Type(), 0
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
