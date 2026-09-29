package keys

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// The built-in SSH agent (MobAgent, SSH-11) and the policy-aware keyrings served through agent forwarding (SSH-12).
//
// Every user has a keyring: the user's stored keys that are not excluded in Settings → SSH keys & agent (available
// while the vault is unlocked), keys added with ssh-add while the agent socket runs, decrypted keys cached in memory
// (optionally for a limited lifetime), the lock state and remembered confirm decisions. Views bind a keyring to a
// requester and implement agent.ExtendedAgent: a local program on the agent socket, a remote server using a forwarded
// agent, or AstraTerm itself authenticating a connection. Confirm-before-use and passphrase prompts go through the
// prompt broker and name the requester and — when the client sent session-bind@openssh.com — the destination host.

const (
	agentPromptTimeout = 2 * time.Minute
	maxAddedKeys       = 100
	ringIdleDrop       = 15 * time.Minute
)

var (
	errAgentLocked = errors.New("agent: locked")
	// errAgentLockedSign: a locked agent holds no usable key, so a merged agent (AstraTerm's logins and forwarding
	// combine the built-in agent with the host agent) goes on to the next agent instead of failing.
	errAgentLockedSign = fmt.Errorf("agent: locked (%w)", sshx.ErrAgentKeyNotFound)
	errAgentRefused    = errors.New("agent: the signature request was refused")
	errAgentReadOnly   = errors.New("agent: read-only (forwarded agent)")
)

// originKind classifies who asks the agent.
type originKind int

const (
	originLocal     originKind = iota // a program on the local agent socket
	originForwarded                   // a remote server using agent forwarding
	originAstraTerm                   // AstraTerm authenticating a connection
)

type agentOrigin struct {
	kind   originKind
	label  string // "ssh (pid 4242)", "admin@bastion.example.com:22", …
	scope  string // remembered decisions apply per key and scope
	connID string // saved connection involved, if any (shown by the prompt dialog)
}

func localOrigin(p peerInfo) agentOrigin {
	return agentOrigin{kind: originLocal, label: p.label(), scope: "local:" + p.name}
}

// requester is the subject of prompt sentences for local programs ("The local program ssh (pid 42)").
func (o agentOrigin) requester() string {
	if o.label == "" || o.label == "a local program" {
		return "A local program"
	}
	return "The local program " + o.label
}

func connOrigin(kind originKind, conn *model.Connection) agentOrigin {
	o := agentOrigin{kind: kind, label: "a remote server", scope: "fwd:?"}
	if conn != nil {
		who := conn.Host
		if conn.Username != "" {
			who = conn.Username + "@" + who
		}
		if conn.Port != 0 && conn.Port != 22 {
			who = fmt.Sprintf("%s:%d", who, conn.Port)
		}
		if conn.Name != "" && conn.Name != conn.Host {
			who = fmt.Sprintf("%s (%s)", conn.Name, who)
		}
		o.label, o.scope, o.connID = who, "fwd:"+strings.ToLower(conn.Host)+fmt.Sprintf(":%d", conn.Port), conn.ID
	}
	if kind == originAstraTerm {
		o.scope = "astraterm"
	}
	return o
}

// ---- service ------------------------------------------------------------------------------------------------------

type agentService struct {
	h   *handler
	log *slog.Logger

	lifeMu sync.Mutex // serializes start / stop

	mu    sync.Mutex
	rings map[string]*keyring
	sock  *agentSocket

	signs atomic.Int64
}

func newAgentService(h *handler) *agentService {
	return &agentService{h: h, log: h.log.With("component", "agent"), rings: map[string]*keyring{}}
}

// ring returns (creating) the keyring of userID.
func (s *agentService) ring(userID string) *keyring {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rings[userID]
	if r == nil {
		r = &keyring{svc: s, userID: userID, cache: map[string]*cachedSigner{}, unloaded: map[string]bool{},
			approvals: map[string]bool{}, touched: time.Now()}
		s.rings[userID] = r
	}
	r.touched = time.Now()
	return r
}

// running returns the socket owned by userID, if the agent runs for that user.
func (s *agentService) running(userID string) *agentSocket {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sock != nil && s.sock.owner.ID == userID {
		return s.sock
	}
	return nil
}

// Agent implements sshx.BuiltinAgent: a view of the user's running agent.
func (s *agentService) Agent(ctx context.Context, user *model.User, conn *model.Connection, forwarding bool) agent.ExtendedAgent {
	if user == nil || s.running(user.ID) == nil {
		return nil
	}
	if forwarding {
		return s.ring(user.ID).view(ctx, connOrigin(originForwarded, conn), true, "")
	}
	exclude := ""
	if conn != nil {
		exclude = conn.KeyID
	}
	return s.ring(user.ID).view(ctx, connOrigin(originAstraTerm, conn), true, exclude)
}

// Keyring implements sshx.BuiltinAgent: the forwarding keyring of the user's stored keys.
func (s *agentService) Keyring(ctx context.Context, user *model.User, conn *model.Connection) agent.Agent {
	if user == nil {
		return nil
	}
	return s.ring(user.ID).view(ctx, connOrigin(originForwarded, conn), true, "")
}

// run maintains the keyrings until ctx ends (then the agent socket is stopped).
func (s *agentService) run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stop()
			return
		case <-t.C:
			s.maintain(ctx)
		}
	}
}

func (s *agentService) maintain(ctx context.Context) {
	vaultLocked := s.h.d.Vault == nil || s.h.d.Vault.Locked()
	s.mu.Lock()
	rings := make([]*keyring, 0, len(s.rings))
	for _, r := range s.rings {
		rings = append(rings, r)
	}
	owner, since := "", time.Time{}
	if s.sock != nil {
		owner, since = s.sock.owner.ID, s.sock.started
	}
	s.mu.Unlock()
	now := time.Now()
	for _, r := range rings {
		isOwner := r.userID == owner
		var cfg keysSettings
		if isOwner {
			cfg = loadSettings(ctx, s.h.d.Store, r.userID)
		}
		idle := r.maintain(now, vaultLocked, isOwner, cfg, since)
		if idle && !isOwner {
			s.mu.Lock()
			if s.rings[r.userID] == r && now.Sub(r.touched) > ringIdleDrop {
				delete(s.rings, r.userID)
			}
			s.mu.Unlock()
		}
	}
}

// ---- keyring ------------------------------------------------------------------------------------------------------

type keyring struct {
	svc    *agentService
	userID string

	mu            sync.Mutex
	added         []*addedKey
	cache         map[string]*cachedSigner // stored key ID → decrypted signer
	unloaded      map[string]bool          // stored keys removed for this agent run (ssh-add -d / -D, AstraTerm UI)
	locked        bool
	lockPass      []byte // passphrase of an ssh-add -x lock; nil when locked from AstraTerm (or by auto-lock)
	failedUnlocks int    // consecutive wrong ssh-add -X passphrases (throttling)
	approvals     map[string]bool
	lastUse       time.Time
	touched       time.Time // guarded by svc.mu

	unlockMu       sync.Mutex // one passphrase prompt at a time
	unlockAttempts sync.Mutex // one ssh-add -X attempt at a time
}

type addedKey struct {
	signer  ssh.Signer
	pub     ssh.PublicKey // the certificate when one was added with the key
	comment string
	confirm bool
	expires time.Time
	addedAt time.Time
}

type cachedSigner struct {
	signer   ssh.Signer
	stamp    string
	expires  time.Time
	unlocked time.Time
}

// maintain expires keys and caches, forgets decrypted keys while the vault is locked and auto-locks the agent. It
// reports whether the ring holds no state.
func (r *keyring) maintain(now time.Time, vaultLocked, owner bool, cfg keysSettings, since time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	if vaultLocked {
		clear(r.cache)
	}
	for id, c := range r.cache {
		if !c.expires.IsZero() && now.After(c.expires) {
			delete(r.cache, id)
		}
	}
	if owner && cfg.AgentAutoLockMin > 0 && !r.locked {
		last := r.lastUse
		if last.Before(since) {
			last = since
		}
		if now.Sub(last) > time.Duration(cfg.AgentAutoLockMin)*time.Minute {
			r.locked, r.lockPass = true, nil
			clear(r.cache)
			r.svc.log.Info("built-in agent locked after inactivity")
		}
	}
	return len(r.added) == 0 && len(r.cache) == 0 && !r.locked && len(r.unloaded) == 0 && len(r.approvals) == 0
}

func (r *keyring) expireLocked(now time.Time) {
	out := r.added[:0]
	for _, k := range r.added {
		if k.expires.IsZero() || now.Before(k.expires) {
			out = append(out, k)
		}
	}
	clear(r.added[len(out):])
	r.added = out
}

// reset forgets everything (the agent socket stopped).
func (r *keyring) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.lockPass)
	r.added, r.locked, r.lockPass, r.failedUnlocks = nil, false, nil, 0
	clear(r.cache)
	clear(r.unloaded)
	clear(r.approvals)
}

func (r *keyring) isLocked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.locked
}

func (r *keyring) addedKeys() []*addedKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(time.Now())
	return append([]*addedKey(nil), r.added...)
}

func (r *keyring) touch() {
	r.mu.Lock()
	r.lastUse = time.Now()
	r.mu.Unlock()
}

// forget drops the decrypted copy of a stored key (it changed or was deleted).
func (r *keyring) forget(keyID string) {
	r.mu.Lock()
	delete(r.cache, keyID)
	r.mu.Unlock()
}

func (r *keyring) cached(keyID, stamp string) ssh.Signer {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cache[keyID]
	if c == nil || c.stamp != stamp || (!c.expires.IsZero() && time.Now().After(c.expires)) {
		return nil
	}
	return c.signer
}

// storedIdentity is a stored key offered by the agent.
type storedIdentity struct {
	key  *model.SSHKey
	pub  ssh.PublicKey
	cert *ssh.Certificate // valid attached certificate, listed before the plain key
}

// storedIdentities returns the stored keys the agent offers (none while the vault is locked).
func (r *keyring) storedIdentities(ctx context.Context, cfg keysSettings, exclude string) []storedIdentity {
	d := r.svc.h.d
	if d.Vault == nil || d.Vault.Locked() {
		r.mu.Lock()
		clear(r.cache) // decrypted keys go with the vault (maintain also purges them every few seconds)
		r.mu.Unlock()
		return nil
	}
	keys, err := d.Store.Keys.ListByOwner(ctx, r.userID)
	if err != nil {
		r.svc.log.Warn("agent: listing stored keys failed", "err", err)
		return nil
	}
	r.mu.Lock()
	unloaded := maps.Clone(r.unloaded)
	r.mu.Unlock()
	now := time.Now()
	out := make([]storedIdentity, 0, len(keys))
	for _, k := range keys {
		if len(k.PrivateKeyEnc) == 0 || k.ID == exclude || cfg.excluded(k.ID) || unloaded[k.ID] {
			continue
		}
		pub, _, err := parsePublicKeyText(k.PublicKey)
		if err != nil {
			continue
		}
		si := storedIdentity{key: k, pub: pub}
		if c := storedCertificate(k.Certificate, pub); c != nil && describeCert(c, now).Status == certValid {
			si.cert = c
		}
		out = append(out, si)
	}
	return out
}

// materialStamp identifies the stored private material (a changed key invalidates cached decryptions).
func materialStamp(k *model.SSHKey) string {
	sum := sha256.Sum256(k.PrivateKeyEnc)
	return k.Fingerprint + ":" + hex.EncodeToString(sum[:8])
}

// ---- views --------------------------------------------------------------------------------------------------------

// agentView is a keyring seen by one requester.
type agentView struct {
	ring     *keyring
	ctx      context.Context
	origin   agentOrigin
	readOnly bool
	exclude  string

	mu    sync.Mutex
	bound []ssh.PublicKey // destination host keys announced with session-bind@openssh.com
}

func (r *keyring) view(ctx context.Context, origin agentOrigin, readOnly bool, exclude string) *agentView {
	if ctx == nil {
		ctx = context.Background()
	}
	return &agentView{ring: r, ctx: ctx, origin: origin, readOnly: readOnly, exclude: exclude}
}

var _ agent.ExtendedAgent = (*agentView)(nil)

func (v *agentView) settings() keysSettings {
	return loadSettings(v.ctx, v.ring.svc.h.d.Store, v.ring.userID)
}

// List implements agent.Agent.
func (v *agentView) List() ([]*agent.Key, error) {
	r := v.ring
	if r.isLocked() {
		return nil, nil // PROTOCOL.agent: a locked agent lists no keys
	}
	var out []*agent.Key
	seen := map[string]bool{}
	add := func(pub ssh.PublicKey, comment string) {
		blob := pub.Marshal()
		if !seen[string(blob)] {
			seen[string(blob)] = true
			out = append(out, &agent.Key{Format: pub.Type(), Blob: blob, Comment: comment})
		}
	}
	for _, si := range r.storedIdentities(v.ctx, v.settings(), v.exclude) {
		if si.cert != nil {
			add(si.cert, si.key.Name)
		}
		add(si.pub, si.key.Name)
	}
	for _, ak := range r.addedKeys() {
		add(ak.pub, ak.comment)
	}
	return out, nil
}

// agentTarget is the key a signature request refers to.
type agentTarget struct {
	added  *addedKey
	stored *storedIdentity
}

func (t *agentTarget) name() string {
	if t.added != nil {
		if t.added.comment != "" {
			return t.added.comment
		}
		return "added key"
	}
	return t.stored.key.Name
}

func (t *agentTarget) publicKey() ssh.PublicKey {
	if t.added != nil {
		if c, ok := t.added.pub.(*ssh.Certificate); ok {
			return c.Key
		}
		return t.added.pub
	}
	return t.stored.pub
}

func (v *agentView) find(key ssh.PublicKey, cfg keysSettings) (*agentTarget, error) {
	blob := key.Marshal()
	for _, ak := range v.ring.addedKeys() {
		if bytes.Equal(ak.pub.Marshal(), blob) {
			return &agentTarget{added: ak}, nil
		}
	}
	for _, si := range v.ring.storedIdentities(v.ctx, cfg, v.exclude) {
		if bytes.Equal(si.pub.Marshal(), blob) || (si.cert != nil && bytes.Equal(si.cert.Marshal(), blob)) {
			return &agentTarget{stored: &si}, nil
		}
	}
	return nil, sshx.ErrAgentKeyNotFound
}

// Sign implements agent.Agent.
func (v *agentView) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return v.SignWithFlags(key, data, 0)
}

// SignWithFlags implements agent.ExtendedAgent.
func (v *agentView) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	r := v.ring
	if r.isLocked() {
		return nil, errAgentLockedSign
	}
	cfg := v.settings()
	t, err := v.find(key, cfg)
	if err != nil {
		return nil, err
	}
	if err := v.confirm(t, cfg); err != nil {
		return nil, err
	}
	var signer ssh.Signer
	if t.added != nil {
		signer = t.added.signer
	} else if signer, err = r.storedSigner(v, t.stored, cfg); err != nil {
		return nil, err
	}
	algorithm := ""
	switch {
	case flags&agent.SignatureFlagRsaSha512 != 0:
		algorithm = ssh.KeyAlgoRSASHA512
	case flags&agent.SignatureFlagRsaSha256 != 0:
		algorithm = ssh.KeyAlgoRSASHA256
	}
	var sig *ssh.Signature
	if algorithm != "" {
		as, ok := signer.(ssh.AlgorithmSigner)
		if !ok {
			return nil, fmt.Errorf("agent: %s signatures are not supported by this key", algorithm)
		}
		sig, err = as.SignWithAlgorithm(rand.Reader, data, algorithm)
	} else {
		sig, err = signer.Sign(rand.Reader, data)
	}
	if err != nil {
		return nil, err
	}
	r.touch()
	r.svc.signs.Add(1)
	if v.origin.kind == originForwarded {
		r.svc.audit(v.ctx, r.userID, "agent.forwarded.sign", ssh.FingerprintSHA256(t.publicKey()),
			map[string]any{"key": t.name(), "via": v.origin.label, "for": v.boundText()})
	}
	return sig, nil
}

// confirm asks the user before the key is used, when the key or the settings require it.
func (v *agentView) confirm(t *agentTarget, cfg keysSettings) error {
	need := t.added != nil && t.added.confirm
	switch v.origin.kind {
	case originLocal:
		need = need || cfg.AgentConfirm
	case originForwarded:
		need = need || cfg.AgentForwardConfirm
	}
	if !need {
		return nil
	}
	r := v.ring
	fp := ssh.FingerprintSHA256(t.publicKey())
	scope := fp + "\x00" + v.origin.scope
	r.mu.Lock()
	decision, known := r.approvals[scope]
	r.mu.Unlock()
	if known {
		if decision {
			return nil
		}
		return errAgentRefused
	}
	resp, err := v.prompt(model.Prompt{
		Kind:      model.PromptConfirm,
		Title:     fmt.Sprintf("Allow use of SSH key “%s”?", t.name()),
		Message:   v.requestText(t) + "\n\nChoosing “Remember my choice” applies to this key and requester until the agent stops.",
		AllowSave: true,
	})
	if err != nil {
		return fmt.Errorf("agent: confirmation unavailable: %w", err)
	}
	if resp.Save {
		r.mu.Lock()
		r.approvals[scope] = resp.Accept
		r.mu.Unlock()
	}
	if !resp.Accept {
		r.svc.audit(v.ctx, r.userID, "agent.sign.denied", fp, map[string]any{"key": t.name(), "requester": v.origin.label})
		return errAgentRefused
	}
	return nil
}

// requestText describes a signature request for prompts.
func (v *agentView) requestText(t *agentTarget) string {
	typ, bits := keyTypeBits(t.publicKey())
	key := fmt.Sprintf("“%s” (%s %d, %s)", t.name(), strings.ToUpper(typ), bits, ssh.FingerprintSHA256(t.publicKey()))
	dest := v.boundText()
	switch v.origin.kind {
	case originForwarded:
		s := fmt.Sprintf("The server %s is using your forwarded agent to sign with the key %s", v.origin.label, key)
		if dest != "" {
			s += " to log in to " + dest
		}
		return s + "."
	case originAstraTerm:
		return fmt.Sprintf("AstraTerm is logging in to %s with the key %s.", v.origin.label, key)
	}
	s := fmt.Sprintf("%s wants to sign with the key %s", v.origin.requester(), key)
	if dest != "" {
		s += " to log in to " + dest
	}
	return s + "."
}

// boundText names the destination hosts the client announced (known hosts names, else fingerprints).
func (v *agentView) boundText() string {
	v.mu.Lock()
	bound := append([]ssh.PublicKey(nil), v.bound...)
	v.mu.Unlock()
	if len(bound) == 0 {
		return ""
	}
	hk := bound[len(bound)-1]
	if names := v.ring.svc.h.knownHostNames(v.ctx, hk); len(names) > 0 {
		return strings.Join(names, ", ")
	}
	return "a host with key " + ssh.FingerprintSHA256(hk)
}

func (v *agentView) prompt(p model.Prompt) (model.PromptResponse, error) {
	if v.origin.connID != "" {
		p.ConnectionID = v.origin.connID
	}
	ctx, cancel := context.WithTimeout(v.ctx, agentPromptTimeout)
	defer cancel()
	return v.ring.svc.h.d.Events.Prompt(ctx, v.ring.userID, p)
}

// storedSigner returns the decrypted signer of a stored key, asking for its passphrase when it is not remembered.
func (r *keyring) storedSigner(v *agentView, si *storedIdentity, cfg keysSettings) (ssh.Signer, error) {
	k := si.key
	stamp := materialStamp(k)
	if s := r.cached(k.ID, stamp); s != nil {
		return s, nil
	}
	r.unlockMu.Lock()
	defer r.unlockMu.Unlock()
	if s := r.cached(k.ID, stamp); s != nil {
		return s, nil
	}
	d := r.svc.h.d
	user, err := d.Store.Users.Get(v.ctx, r.userID)
	if err != nil {
		return nil, err
	}
	pemBytes, pass, err := d.KeyMaterial(v.ctx, user, k.ID)
	if err != nil {
		return nil, err
	}
	defer clear(pemBytes)
	signer, err := sshx.ParsePrivateKey(pemBytes, pass)
	var np *sshx.NeedsPassphraseError
	if errors.As(err, &np) {
		signer, err = v.askPassphrase(si, pemBytes)
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	c := &cachedSigner{signer: signer, stamp: stamp, unlocked: now}
	if cfg.AgentKeyLifetimeMin > 0 {
		c.expires = now.Add(time.Duration(cfg.AgentKeyLifetimeMin) * time.Minute)
	}
	r.mu.Lock()
	r.cache[k.ID] = c
	r.mu.Unlock()
	return signer, nil
}

func (v *agentView) askPassphrase(si *storedIdentity, pemBytes []byte) (ssh.Signer, error) {
	k := si.key
	canSave := !v.ring.svc.h.d.Vault.Locked()
	msg := fmt.Sprintf("The key “%s” is encrypted. %s", k.Name, v.requestText(&agentTarget{stored: si}))
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			msg = "Incorrect passphrase, please try again."
		}
		resp, err := v.prompt(model.Prompt{
			Kind:      model.PromptPassphrase,
			Title:     fmt.Sprintf("Passphrase for key “%s”", k.Name),
			Message:   msg,
			Fields:    []model.PromptField{{Label: "Passphrase", Echo: false}},
			AllowSave: canSave,
		})
		if err != nil {
			return nil, fmt.Errorf("agent: passphrase unavailable: %w", err)
		}
		if !resp.Accept || len(resp.Values) == 0 {
			return nil, errAgentRefused
		}
		s, err := sshx.ParsePrivateKey(pemBytes, resp.Values[0])
		var np *sshx.NeedsPassphraseError
		if errors.As(err, &np) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if resp.Save && canSave {
			if err := v.ring.svc.h.rememberPassphrase(v.ctx, v.ring.userID, k.ID, resp.Values[0]); err != nil {
				v.ring.svc.log.Warn("agent: remembering the passphrase failed", "key", k.ID, "err", err)
			}
		}
		return s, nil
	}
	return nil, errors.New("agent: incorrect passphrase")
}

// Signers implements agent.Agent (in-process use: AstraTerm authenticating a connection).
func (v *agentView) Signers() ([]ssh.Signer, error) {
	keys, err := v.List()
	if err != nil {
		return nil, err
	}
	out := make([]ssh.Signer, 0, len(keys))
	for _, k := range keys {
		pub, err := ssh.ParsePublicKey(k.Blob)
		if err != nil {
			continue
		}
		out = append(out, &viewSigner{v: v, pub: pub})
	}
	return out, nil
}

// Add implements agent.Agent (ssh-add on the local socket).
func (v *agentView) Add(key agent.AddedKey) error {
	if v.readOnly {
		return errAgentReadOnly
	}
	if len(key.ConstraintExtensions) > 0 {
		return errors.New("agent: key constraints such as destination restrictions (ssh-add -h) are not supported")
	}
	signer, err := ssh.NewSignerFromKey(key.PrivateKey)
	if err != nil {
		return err
	}
	pub := signer.PublicKey()
	if key.Certificate != nil {
		if !sameKey(key.Certificate.Key, pub) {
			return errors.New("agent: the certificate does not match the key")
		}
		pub = key.Certificate
	}
	now := time.Now()
	ak := &addedKey{signer: signer, pub: pub, comment: cleanComment(key.Comment), confirm: key.ConfirmBeforeUse, addedAt: now}
	if key.LifetimeSecs > 0 {
		ak.expires = now.Add(time.Duration(key.LifetimeSecs) * time.Second)
	}
	r := v.ring
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked {
		return errAgentLocked
	}
	r.expireLocked(now)
	for i, k := range r.added {
		if bytes.Equal(k.pub.Marshal(), pub.Marshal()) {
			r.added[i] = ak
			return nil
		}
	}
	if len(r.added) >= maxAddedKeys {
		return errors.New("agent: too many keys")
	}
	r.added = append(r.added, ak)
	return nil
}

// Remove implements agent.Agent: added keys are deleted, stored keys unloaded until the agent restarts.
func (v *agentView) Remove(key ssh.PublicKey) error {
	if v.readOnly {
		return errAgentReadOnly
	}
	r := v.ring
	if r.isLocked() {
		return errAgentLocked
	}
	blob := key.Marshal()
	r.mu.Lock()
	for i, k := range r.added {
		if bytes.Equal(k.pub.Marshal(), blob) || bytes.Equal(k.signer.PublicKey().Marshal(), blob) {
			r.added = append(r.added[:i:i], r.added[i+1:]...)
			r.mu.Unlock()
			return nil
		}
	}
	r.mu.Unlock()
	for _, si := range r.storedIdentities(v.ctx, v.settings(), "") {
		if bytes.Equal(si.pub.Marshal(), blob) || (si.cert != nil && bytes.Equal(si.cert.Marshal(), blob)) {
			r.unload(si.key.ID)
			return nil
		}
	}
	return errors.New("agent: key not found")
}

func (r *keyring) unload(keyID string) {
	r.mu.Lock()
	r.unloaded[keyID] = true
	delete(r.cache, keyID)
	r.mu.Unlock()
}

// RemoveAll implements agent.Agent.
func (v *agentView) RemoveAll() error {
	if v.readOnly {
		return errAgentReadOnly
	}
	r := v.ring
	if r.isLocked() {
		return errAgentLocked
	}
	ids := r.storedIdentities(v.ctx, v.settings(), "")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = nil
	for _, si := range ids {
		r.unloaded[si.key.ID] = true
		delete(r.cache, si.key.ID)
	}
	return nil
}

// Lock implements agent.Agent (ssh-add -x).
func (v *agentView) Lock(passphrase []byte) error {
	if v.readOnly {
		return errAgentReadOnly
	}
	r := v.ring
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked {
		return errAgentLocked
	}
	r.locked, r.lockPass = true, append([]byte(nil), passphrase...)
	clear(r.cache)
	return nil
}

// Unlock implements agent.Agent (ssh-add -X). Like ssh-agent, attempts are serialized and every failure delays the
// next one a little longer (100 ms per failure, at most 10 s), so the lock passphrase cannot be brute-forced quickly.
func (v *agentView) Unlock(passphrase []byte) error {
	if v.readOnly {
		return errAgentReadOnly
	}
	r := v.ring
	r.unlockAttempts.Lock()
	defer r.unlockAttempts.Unlock()
	r.mu.Lock()
	if !r.locked {
		r.mu.Unlock()
		return errors.New("agent: not locked")
	}
	if r.lockPass == nil {
		r.mu.Unlock()
		return errors.New("agent: locked from AstraTerm; unlock it there")
	}
	if subtle.ConstantTimeCompare(passphrase, r.lockPass) != 1 {
		r.failedUnlocks = min(r.failedUnlocks+1, 100)
		delay := time.Duration(r.failedUnlocks) * 100 * time.Millisecond
		r.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-v.ctx.Done():
		}
		return errors.New("agent: incorrect passphrase")
	}
	clear(r.lockPass)
	r.locked, r.lockPass, r.failedUnlocks = false, nil, 0
	r.mu.Unlock()
	return nil
}

// Extension implements agent.ExtendedAgent: session-bind@openssh.com (the client announces the host it
// authenticates to, which prompts then name). Other extensions are unsupported; a locked agent refuses them all.
func (v *agentView) Extension(extensionType string, contents []byte) ([]byte, error) {
	if extensionType != "session-bind@openssh.com" {
		return nil, agent.ErrExtensionUnsupported
	}
	if v.ring.isLocked() {
		return nil, errAgentLocked
	}
	var msg struct {
		HostKey      []byte
		SessionID    []byte
		Signature    []byte
		IsForwarding bool
	}
	if err := ssh.Unmarshal(contents, &msg); err != nil {
		return nil, errors.New("agent: malformed session-bind request")
	}
	hk, err := ssh.ParsePublicKey(msg.HostKey)
	if err != nil {
		return nil, errors.New("agent: malformed session-bind host key")
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(msg.Signature, &sig); err != nil {
		return nil, errors.New("agent: malformed session-bind signature")
	}
	if err := hk.Verify(msg.SessionID, &sig); err != nil {
		return nil, errors.New("agent: session-bind signature does not verify")
	}
	v.mu.Lock()
	if len(v.bound) < 16 {
		v.bound = append(v.bound, hk)
	}
	v.mu.Unlock()
	return nil, nil
}

// viewSigner signs through a view (confirmations and passphrase prompts included).
type viewSigner struct {
	v   *agentView
	pub ssh.PublicKey
}

func (s *viewSigner) PublicKey() ssh.PublicKey { return s.pub }

func (s *viewSigner) Sign(_ io.Reader, data []byte) (*ssh.Signature, error) {
	return s.v.SignWithFlags(s.pub, data, 0)
}

func (s *viewSigner) SignWithAlgorithm(r io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	if algorithm == "" || algorithm == underlyingAlgo(s.pub.Type()) {
		return s.Sign(r, data)
	}
	switch algorithm {
	case ssh.KeyAlgoRSASHA256:
		return s.v.SignWithFlags(s.pub, data, agent.SignatureFlagRsaSha256)
	case ssh.KeyAlgoRSASHA512:
		return s.v.SignWithFlags(s.pub, data, agent.SignatureFlagRsaSha512)
	}
	return nil, fmt.Errorf("agent: unsupported algorithm %q", algorithm)
}

// underlyingAlgo maps certificate key types to their key algorithm.
func underlyingAlgo(t string) string {
	switch t {
	case ssh.CertAlgoRSAv01:
		return ssh.KeyAlgoRSA
	case ssh.CertAlgoRSASHA256v01:
		return ssh.KeyAlgoRSASHA256
	case ssh.CertAlgoRSASHA512v01:
		return ssh.KeyAlgoRSASHA512
	case ssh.InsecureCertAlgoDSAv01:
		return ssh.InsecureKeyAlgoDSA
	case ssh.CertAlgoECDSA256v01:
		return ssh.KeyAlgoECDSA256
	case ssh.CertAlgoECDSA384v01:
		return ssh.KeyAlgoECDSA384
	case ssh.CertAlgoECDSA521v01:
		return ssh.KeyAlgoECDSA521
	case ssh.CertAlgoED25519v01:
		return ssh.KeyAlgoED25519
	case ssh.CertAlgoSKECDSA256v01:
		return ssh.KeyAlgoSKECDSA256
	case ssh.CertAlgoSKED25519v01:
		return ssh.KeyAlgoSKED25519
	}
	return t
}

// audit records an agent event for userID.
func (s *agentService) audit(ctx context.Context, userID, action, target string, details any) {
	if s.h.d.Audit == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if u, err := s.h.d.Store.Users.Get(ctx, userID); err == nil {
		s.h.d.Audit.LogUser(ctx, u, action, target, details)
	}
}
