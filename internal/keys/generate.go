package keys

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/model"
)

// Key generation (TOOL-1, MobaKeyGen): Ed25519, RSA 2048/3072/4096 and ECDSA P-256/384/521. The generator dialog
// first creates a *draft* kept in server memory (never written to disk), shows its public key, fingerprints and
// randomart, and then stores it in the vault and/or exports the private key — so private material only reaches the
// browser when the user explicitly saves it to a file.

const (
	draftTTL          = 30 * time.Minute
	maxDraftsPerUser  = 10
	defaultRSABits    = 3072 // ssh-keygen's default
	defaultECDSABits  = 256
	maxPassphraseSize = 1024
)

// generateKey creates a private key of the given type and size (0 = default).
func generateKey(typ string, bits int) (crypto.PrivateKey, error) {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "", "ed25519":
		if bits != 0 && bits != 256 {
			return nil, invalidKey("Ed25519 keys are always 256 bits")
		}
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	case "rsa":
		if bits == 0 {
			bits = defaultRSABits
		}
		if bits != 2048 && bits != 3072 && bits != 4096 {
			return nil, invalidKey("RSA keys must be 2048, 3072 or 4096 bits")
		}
		var k *rsa.PrivateKey
		err := withKDF(func() (err error) { // seconds of CPU for 4096 bits: shares the slots of the key derivations
			k, err = rsa.GenerateKey(rand.Reader, bits)
			return err
		})
		if err != nil {
			return nil, err
		}
		return k, nil
	case "ecdsa":
		var c elliptic.Curve
		switch bits {
		case 0, 256:
			c = elliptic.P256()
		case 384:
			c = elliptic.P384()
		case 521:
			c = elliptic.P521()
		default:
			return nil, invalidKey("ECDSA keys must be 256, 384 or 521 bits")
		}
		return ecdsa.GenerateKey(c, rand.Reader)
	case "dsa":
		return nil, unsupportedKey("DSA keys are obsolete and cannot be generated (existing ones can be imported)")
	}
	return nil, invalidKey(fmt.Sprintf("unknown key type %q (use ed25519, rsa or ecdsa)", typ))
}

// defaultComment is the comment of generated keys without one, like PuTTYgen ("ed25519-key-20260927").
func defaultComment(typ string, now time.Time) string {
	return fmt.Sprintf("%s-key-%s", typ, now.Format("20060102"))
}

// keyDraft is a generated key awaiting "store in vault" / export.
type keyDraft struct {
	ID         string
	UserID     string
	Key        *parsedKey
	Comment    string
	Passphrase string
	Created    time.Time
}

// draftView is the JSON description of a draft.
type draftView struct {
	DraftID        string    `json:"draftId"`
	Type           string    `json:"type"`
	Bits           int       `json:"bits"`
	PublicKey      string    `json:"publicKey"`
	Fingerprint    string    `json:"fingerprint"`
	FingerprintMD5 string    `json:"fingerprintMd5"`
	Comment        string    `json:"comment"`
	HasPassphrase  bool      `json:"hasPassphrase"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

func (d *keyDraft) view() draftView {
	typ, bits := keyTypeBits(d.Key.Pub)
	return draftView{
		DraftID:        d.ID,
		Type:           typ,
		Bits:           bits,
		PublicKey:      authorizedKeyLine(d.Key.Pub, d.Comment),
		Fingerprint:    ssh.FingerprintSHA256(d.Key.Pub),
		FingerprintMD5: fingerprintMD5(d.Key.Pub),
		Comment:        d.Comment,
		HasPassphrase:  d.Passphrase != "",
		ExpiresAt:      d.Created.Add(draftTTL).UTC(),
	}
}

type draftStore struct {
	mu    sync.Mutex
	items map[string]*keyDraft
}

func newDraftStore() *draftStore { return &draftStore{items: map[string]*keyDraft{}} }

// put adds a draft, evicting the user's oldest ones beyond maxDraftsPerUser.
func (s *draftStore) put(d *keyDraft) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(time.Now())
	var oldest *keyDraft
	n := 0
	for _, x := range s.items {
		if x.UserID == d.UserID {
			n++
			if oldest == nil || x.Created.Before(oldest.Created) {
				oldest = x
			}
		}
	}
	if n >= maxDraftsPerUser && oldest != nil {
		delete(s.items, oldest.ID)
	}
	s.items[d.ID] = d
}

// get returns a live draft of userID.
func (s *draftStore) get(userID, id string) *keyDraft {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(time.Now())
	if d := s.items[id]; d != nil && d.UserID == userID {
		return d
	}
	return nil
}

func (s *draftStore) remove(userID, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.items[id]; d != nil && d.UserID == userID {
		delete(s.items, id)
		return true
	}
	return false
}

func (s *draftStore) expireLocked(now time.Time) {
	for id, d := range s.items {
		if now.Sub(d.Created) > draftTTL {
			delete(s.items, id)
		}
	}
}

// janitor drops expired drafts until ctx ends.
func (s *draftStore) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			clear(s.items)
			s.mu.Unlock()
			return
		case now := <-t.C:
			s.mu.Lock()
			s.expireLocked(now)
			s.mu.Unlock()
		}
	}
}

func newDraft(userID string, pk *parsedKey, comment, passphrase string) *keyDraft {
	return &keyDraft{ID: model.NewID(), UserID: userID, Key: pk, Comment: comment, Passphrase: passphrase, Created: time.Now()}
}
