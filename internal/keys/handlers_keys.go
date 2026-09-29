package keys

import (
	"context"
	"crypto/dsa" //nolint:staticcheck // legacy DSA keys are stored as traditional PEM
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/sshx"
)

const maxKeyRequestBody = 512 << 10

// keyUsage counts what references a key.
type keyUsage struct {
	Connections int `json:"connections"`
	Identities  int `json:"identities"`
}

// keyView is the JSON of a stored key: model.SSHKey (SPEC §5.2) plus module extensions.
type keyView struct {
	*model.SSHKey
	FingerprintMD5  string    `json:"fingerprintMd5"`
	HasPrivateKey   bool      `json:"hasPrivateKey"`
	PassphraseSaved bool      `json:"passphraseSaved"` // the passphrase of an encrypted key is remembered in the vault
	CertificateInfo *certInfo `json:"certificateInfo,omitempty"`
	UsedBy          keyUsage  `json:"usedBy"`
}

func (h *handler) view(k *model.SSHKey, usage keyUsage, t time.Time) keyView {
	v := keyView{SSHKey: k, HasPrivateKey: len(k.PrivateKeyEnc) > 0, PassphraseSaved: len(k.PassphraseEnc) > 0, UsedBy: usage}
	if pub, err := keyPublic(k); err == nil {
		v.FingerprintMD5 = fingerprintMD5(pub)
		if cert := storedCertificate(k.Certificate, pub); cert != nil {
			v.CertificateInfo = describeCert(cert, t)
		}
	}
	return v
}

// usage counts the owner's connections and identities using each key.
func (h *handler) usage(ctx context.Context, ownerID string) map[string]keyUsage {
	out := map[string]keyUsage{}
	if conns, err := h.d.Store.Connections.ListByOwner(ctx, ownerID); err == nil {
		for _, c := range conns {
			if c.KeyID != "" {
				u := out[c.KeyID]
				u.Connections++
				out[c.KeyID] = u
			}
		}
	}
	if ids, err := h.d.Store.Identities.ListByOwner(ctx, ownerID); err == nil {
		for _, i := range ids {
			if i.KeyID != "" {
				u := out[i.KeyID]
				u.Identities++
				out[i.KeyID] = u
			}
		}
	}
	return out
}

func (h *handler) listKeys(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	keys, err := h.d.Store.Keys.ListByOwner(ctx, u.ID)
	if err != nil {
		return err
	}
	usage, t := h.usage(ctx, u.ID), now()
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, h.view(k, usage[k.ID], t))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) getKey(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, h.view(k, h.usage(ctx, u.ID)[k.ID], now()))
}

// ---- create -------------------------------------------------------------------------------------------------------

type storeParams struct {
	name       string
	comment    string
	passphrase string // protects the stored text ("" = unencrypted)
	remember   bool   // keep the passphrase in the vault
	key        *parsedKey
	text       []byte // original text of imports (stored as is when sshx can read it)
	source     string // generate | import
}

// storageText returns the private key text to store: the imported OpenSSH / PEM / PKCS#8 text itself when sshx can
// load it with the passphrase, otherwise (PuTTY keys, PBES2 PKCS#8, OpenSSH keys with ciphers x/crypto lacks) an
// equivalent OpenSSH encoding (DSA: traditional PEM) protected by the same passphrase.
func storageText(pk *parsedKey, original []byte, comment, passphrase string) ([]byte, error) {
	if original != nil && pk.Format != formatPPK {
		clean := cleanKeyText(original)
		if s, err := sshx.ParsePrivateKey(clean, passphrase); err == nil && sameKey(s.PublicKey(), pk.Pub) {
			return append(append([]byte(nil), clean...), '\n'), nil
		}
	}
	if _, ok := pk.Raw.(*dsa.PrivateKey); ok {
		block, err := traditionalPEM(pk, []byte(passphrase))
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(block), nil
	}
	return marshalOpenSSHPrivateKey(pk.Raw, comment, []byte(passphrase))
}

// storeKey seals and saves a key for u.
func (h *handler) storeKey(ctx context.Context, u *model.User, p storeParams) (*model.SSHKey, error) {
	if h.d.Vault == nil || h.d.Vault.Locked() {
		return nil, httpx.ErrLocked
	}
	typ, bits := keyTypeBits(p.key.Pub)
	name, err := cleanName(p.name)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = p.comment
		if name == "" {
			name = strings.ToUpper(typ[:1]) + typ[1:] + " key"
		}
		name, _ = cleanName(name)
	}
	fp := ssh.FingerprintSHA256(p.key.Pub)
	existing, err := h.d.Store.Keys.ListByOwner(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.Fingerprint == fp {
			return nil, httpx.NewError(http.StatusConflict, "key_exists", fmt.Sprintf("this key is already stored as “%s”", e.Name))
		}
	}
	text, err := storageText(p.key, p.text, p.comment, p.passphrase)
	if err != nil {
		return nil, err
	}
	sealed, err := h.d.Vault.Seal(text)
	wipe(text)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return nil, httpx.ErrLocked
		}
		return nil, err
	}
	var passEnc []byte
	if p.passphrase != "" && p.remember {
		if passEnc, err = h.d.Vault.Seal([]byte(p.passphrase)); err != nil {
			return nil, err
		}
	}
	k := &model.SSHKey{
		OwnerID:       u.ID,
		Name:          name,
		Type:          typ,
		Bits:          bits,
		PublicKey:     authorizedKeyLine(p.key.Pub, p.comment),
		Fingerprint:   fp,
		Comment:       p.comment,
		HasPassphrase: p.passphrase != "",
		PrivateKeyEnc: sealed,
		PassphraseEnc: passEnc,
	}
	if err := h.d.Store.Keys.Create(ctx, k); err != nil {
		return nil, err
	}
	h.d.Audit.LogUser(ctx, u, "key.create", k.ID, map[string]any{"name": name, "type": typ, "bits": bits, "source": p.source, "fingerprint": fp})
	return k, nil
}

func checkPassphrase(p string) error {
	if len(p) > maxPassphraseSize {
		return httpx.BadRequest("the passphrase is too long")
	}
	return nil
}

func (h *handler) generate(c *echo.Context) error {
	var req struct {
		Name               string `json:"name"`
		Type               string `json:"type"`
		Bits               int    `json:"bits"`
		Comment            string `json:"comment"`
		Passphrase         string `json:"passphrase"`
		RememberPassphrase *bool  `json:"rememberPassphrase"`
		Store              *bool  `json:"store"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if err := checkPassphrase(req.Passphrase); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	raw, err := generateKey(req.Type, req.Bits)
	if err != nil {
		return httpKeyError(err)
	}
	pk, err := newParsedKey(raw, "", formatOpenSSH, false)
	if err != nil {
		return httpKeyError(err)
	}
	typ, _ := keyTypeBits(pk.Pub)
	comment := cleanComment(req.Comment)
	if comment == "" {
		comment = defaultComment(typ, time.Now())
	}
	if req.Store != nil && !*req.Store {
		d := newDraft(u.ID, pk, comment, req.Passphrase)
		h.drafts.put(d)
		return c.JSON(http.StatusOK, d.view())
	}
	k, err := h.storeKey(ctx, u, storeParams{name: req.Name, comment: comment, passphrase: req.Passphrase,
		remember: boolOr(req.RememberPassphrase, true), key: pk, source: "generate"})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, h.view(k, keyUsage{}, now()))
}

func (h *handler) importKey(c *echo.Context) error {
	var req struct {
		Name               string `json:"name"`
		PrivateKey         string `json:"privateKey"`
		Passphrase         string `json:"passphrase"`
		Comment            string `json:"comment"`
		RememberPassphrase *bool  `json:"rememberPassphrase"`
		Certificate        string `json:"certificate"`
	}
	if err := httpx.BindLimit(c, &req, maxKeyRequestBody); err != nil {
		return err
	}
	if err := checkPassphrase(req.Passphrase); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	pk, err := parseKey([]byte(req.PrivateKey), req.Passphrase)
	if err != nil {
		return httpKeyError(err)
	}
	certLine := ""
	if strings.TrimSpace(req.Certificate) != "" {
		_, line, err := validateUserCert(req.Certificate, pk.Pub)
		if err != nil {
			return httpKeyError(err)
		}
		certLine = line
	}
	comment := cleanComment(req.Comment)
	if comment == "" {
		comment = cleanComment(pk.Comment)
	}
	pass := ""
	if pk.Encrypted {
		pass = req.Passphrase
	}
	k, err := h.storeKey(ctx, u, storeParams{name: req.Name, comment: comment, passphrase: pass,
		remember: boolOr(req.RememberPassphrase, true), key: pk, text: []byte(req.PrivateKey), source: "import"})
	if err != nil {
		return err
	}
	if certLine != "" {
		k.Certificate = certLine
		if err := h.d.Store.Keys.Update(ctx, k); err != nil {
			return err
		}
	}
	return c.JSON(http.StatusCreated, h.view(k, keyUsage{}, now()))
}

// inspectResult describes pasted key text (import dialog, certificate dialog) without storing anything.
type inspectResult struct {
	Kind            string    `json:"kind"` // private | public | certificate
	Format          string    `json:"format,omitempty"`
	PPKVersion      int       `json:"ppkVersion,omitempty"`
	Encrypted       bool      `json:"encrypted"`
	NeedsPassphrase bool      `json:"needsPassphrase"`
	WrongPassphrase bool      `json:"wrongPassphrase"`
	Type            string    `json:"type,omitempty"`
	Bits            int       `json:"bits,omitempty"`
	Fingerprint     string    `json:"fingerprint,omitempty"`
	FingerprintMD5  string    `json:"fingerprintMd5,omitempty"`
	PublicKey       string    `json:"publicKey,omitempty"`
	Comment         string    `json:"comment,omitempty"`
	Certificate     *certInfo `json:"certificate,omitempty"`
	ExistingKeyID   string    `json:"existingKeyId,omitempty"`
	ExistingKeyName string    `json:"existingKeyName,omitempty"`
}

func (h *handler) inspect(c *echo.Context) error {
	var req struct {
		Text       string `json:"text"`
		Passphrase string `json:"passphrase"`
	}
	if err := httpx.BindLimit(c, &req, maxKeyRequestBody); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	res := inspectResult{Kind: "private"}
	var pub ssh.PublicKey
	pk, err := parseKey([]byte(req.Text), req.Passphrase)
	var pe *passphraseError
	var ke *keyError
	switch {
	case err == nil:
		pub = pk.Pub
		res.Format, res.PPKVersion, res.Encrypted, res.Comment = pk.Format, pk.PPKVersion, pk.Encrypted, pk.Comment
	case errors.As(err, &pe):
		res.Encrypted, res.NeedsPassphrase, res.WrongPassphrase = true, true, pe.wrong
		pub = pe.pub
	case errors.As(err, &ke) && ke.code == "public_key_only":
		pk, comment, perr := parsePublicKeyText(req.Text)
		if perr != nil {
			return httpKeyError(err)
		}
		res.Kind, res.Comment, pub = "public", comment, pk
		if cert, ok := pk.(*ssh.Certificate); ok {
			res.Kind, res.Certificate = "certificate", describeCert(cert, now())
		}
	default:
		return httpKeyError(err)
	}
	if pub != nil {
		res.Type, res.Bits = keyTypeBits(pub)
		res.Fingerprint, res.FingerprintMD5 = ssh.FingerprintSHA256(pub), fingerprintMD5(pub)
		res.PublicKey = authorizedKeyLine(pub, res.Comment)
		if cert, ok := pub.(*ssh.Certificate); ok {
			res.Fingerprint = ssh.FingerprintSHA256(cert.Key)
		}
		if keys, err := h.d.Store.Keys.ListByOwner(ctx, u.ID); err == nil {
			for _, k := range keys {
				if k.Fingerprint == res.Fingerprint {
					res.ExistingKeyID, res.ExistingKeyName = k.ID, k.Name
					break
				}
			}
		}
	}
	return c.JSON(http.StatusOK, res)
}

// convert converts key text between formats without storing it (MobaKeyGen "Load" + "Save").
func (h *handler) convert(c *echo.Context) error {
	var req struct {
		PrivateKey    string  `json:"privateKey"`
		Passphrase    string  `json:"passphrase"`
		Format        string  `json:"format"`
		NewPassphrase string  `json:"newPassphrase"`
		Comment       *string `json:"comment"`
		PPKVersion    int     `json:"ppkVersion"`
		Name          string  `json:"name"`
	}
	if err := httpx.BindLimit(c, &req, maxKeyRequestBody); err != nil {
		return err
	}
	if err := checkPassphrase(req.NewPassphrase); err != nil {
		return err
	}
	pk, err := parseKey([]byte(req.PrivateKey), req.Passphrase)
	if err != nil {
		return httpKeyError(err)
	}
	comment := pk.Comment
	if req.Comment != nil {
		comment = cleanComment(*req.Comment)
	}
	typ, _ := keyTypeBits(pk.Pub)
	base := fileBase(req.Name, typ)
	var res *exportResult
	switch {
	case isPublicFormat(req.Format):
		res, err = exportPublicKey(pk.Pub, req.Format, comment, base)
	case isPrivateFormat(req.Format):
		res, err = exportPrivateKey(pk, req.Format, comment, req.NewPassphrase, req.PPKVersion, base)
	default:
		return httpx.BadRequest("unknown format")
	}
	if err != nil {
		return httpKeyError(err)
	}
	return c.JSON(http.StatusOK, res)
}

// ---- drafts -------------------------------------------------------------------------------------------------------

func (h *handler) draft(c *echo.Context) (*keyDraft, error) {
	d := h.drafts.get(httpx.UserFrom(c).ID, c.Param("id"))
	if d == nil {
		return nil, httpx.NotFound("the generated key expired; generate a new one")
	}
	return d, nil
}

func (h *handler) storeDraft(c *echo.Context) error {
	var req struct {
		Name               string `json:"name"`
		RememberPassphrase *bool  `json:"rememberPassphrase"`
	}
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	d, err := h.draft(c)
	if err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.storeKey(ctx, u, storeParams{name: req.Name, comment: d.Comment, passphrase: d.Passphrase,
		remember: boolOr(req.RememberPassphrase, true), key: d.Key, source: "generate"})
	if err != nil {
		return err
	}
	h.drafts.remove(u.ID, d.ID)
	return c.JSON(http.StatusCreated, h.view(k, keyUsage{}, now()))
}

func (h *handler) exportDraft(c *echo.Context) error {
	var req struct {
		Format     string `json:"format"`
		PPKVersion int    `json:"ppkVersion"`
		Name       string `json:"name"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	d, err := h.draft(c)
	if err != nil {
		return err
	}
	typ, _ := keyTypeBits(d.Key.Pub)
	base := fileBase(req.Name, typ)
	var res *exportResult
	switch {
	case isPublicFormat(req.Format):
		res, err = exportPublicKey(d.Key.Pub, req.Format, d.Comment, base)
	case isPrivateFormat(req.Format):
		res, err = exportPrivateKey(d.Key, req.Format, d.Comment, d.Passphrase, req.PPKVersion, base)
		if err == nil {
			h.d.Audit.Log(c, "key.draft.export", d.ID, map[string]any{"format": req.Format, "encrypted": res.Encrypted})
		}
	default:
		return httpx.BadRequest("unknown format")
	}
	if err != nil {
		return httpKeyError(err)
	}
	return c.JSON(http.StatusOK, res)
}

func (h *handler) discardDraft(c *echo.Context) error {
	h.drafts.remove(httpx.UserFrom(c).ID, c.Param("id"))
	return httpx.OK(c)
}

// ---- update / delete ----------------------------------------------------------------------------------------------

func (h *handler) updateKey(c *echo.Context) error {
	var req struct {
		Name               *string `json:"name"`
		Comment            *string `json:"comment"`
		Certificate        *string `json:"certificate"`
		Passphrase         *string `json:"passphrase"`    // current passphrase: remember it ("" forgets the remembered one)
		NewPassphrase      *string `json:"newPassphrase"` // re-encrypt the stored key ("" removes the passphrase)
		RememberPassphrase *bool   `json:"rememberPassphrase"`
	}
	if err := httpx.BindLimit(c, &req, maxKeyRequestBody); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	pub, err := keyPublic(k)
	if err != nil {
		return err
	}
	var events []string
	if req.Name != nil {
		name, err := cleanName(*req.Name)
		if err != nil {
			return err
		}
		if name == "" {
			return httpx.BadRequest("the name is required")
		}
		k.Name = name
	}
	if req.Comment != nil {
		k.Comment = cleanComment(*req.Comment)
		k.PublicKey = authorizedKeyLine(pub, k.Comment)
	}
	if req.Certificate != nil {
		if strings.TrimSpace(*req.Certificate) == "" {
			if k.Certificate != "" {
				events = append(events, "key.certificate.detach")
			}
			k.Certificate = ""
		} else {
			_, line, err := validateUserCert(*req.Certificate, pub)
			if err != nil {
				return httpKeyError(err)
			}
			k.Certificate = line
			events = append(events, "key.certificate.attach")
		}
	}
	current := ""
	if req.Passphrase != nil {
		current = *req.Passphrase
	}
	switch {
	case req.NewPassphrase != nil:
		np := *req.NewPassphrase
		if err := checkPassphrase(np); err != nil {
			return err
		}
		pk, err := h.material(ctx, u, k, current)
		if err != nil {
			return err
		}
		text, err := storageText(pk, nil, k.Comment, np)
		if err != nil {
			return err
		}
		sealed, err := h.d.Vault.Seal(text)
		wipe(text)
		if err != nil {
			return err
		}
		k.PrivateKeyEnc, k.HasPassphrase, k.PassphraseEnc = sealed, np != "", nil
		if np != "" && boolOr(req.RememberPassphrase, true) {
			if k.PassphraseEnc, err = h.d.Vault.Seal([]byte(np)); err != nil {
				return err
			}
		}
		events = append(events, "key.passphrase.change")
	case req.Passphrase != nil && current == "":
		k.PassphraseEnc = nil
		events = append(events, "key.passphrase.forget")
	case req.Passphrase != nil:
		if !k.HasPassphrase {
			return httpx.BadRequest("this key is not protected by a passphrase")
		}
		if err := checkPassphrase(current); err != nil {
			return err
		}
		if _, err := h.material(ctx, u, k, current); err != nil {
			return err
		}
		if k.PassphraseEnc, err = h.d.Vault.Seal([]byte(current)); err != nil {
			return err
		}
		events = append(events, "key.passphrase.remember")
	}
	if err := h.d.Store.Keys.Update(ctx, k); err != nil {
		return err
	}
	h.agent.ring(u.ID).forget(k.ID)
	if len(events) == 0 {
		events = append(events, "key.update")
	}
	for _, ev := range events {
		h.d.Audit.Log(c, ev, k.ID, map[string]any{"name": k.Name})
	}
	return c.JSON(http.StatusOK, h.view(k, h.usage(ctx, u.ID)[k.ID], now()))
}

func (h *handler) deleteKey(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	if err := h.d.Store.Keys.Delete(ctx, k.ID); err != nil {
		return err
	}
	h.agent.ring(u.ID).forget(k.ID)
	h.d.Audit.Log(c, "key.delete", k.ID, map[string]any{"name": k.Name, "fingerprint": k.Fingerprint})
	return httpx.OK(c)
}

// ---- export -------------------------------------------------------------------------------------------------------

// exportKeyGET implements GET /api/keys/{id}/export?format=openssh|ppk|pem|pkcs8|public|rfc4716[&ppkVersion=2]: the
// file as text. Private formats keep the key's current protection (its remembered passphrase); POST chooses another.
func (h *handler) exportKeyGET(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	format := c.QueryParam("format")
	if format == "" {
		format = exportPublic
	}
	ppk := 0
	if v := c.QueryParam("ppkVersion"); v == "2" || v == "3" {
		ppk = int(v[0] - '0')
	}
	res, err := h.exportStored(c, u, k, format, true, "", "", ppk)
	if err != nil {
		return err
	}
	hdr := c.Response().Header()
	hdr.Set(echo.HeaderContentDisposition, mime.FormatMediaType("attachment", map[string]string{"filename": res.Filename}))
	hdr.Set(echo.HeaderCacheControl, "no-store")
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(res.Content))
}

func (h *handler) exportKeyPOST(c *echo.Context) error {
	var req struct {
		Format            string `json:"format"`
		KeepPassphrase    bool   `json:"keepPassphrase"`    // protect the output with the key's current passphrase
		Passphrase        string `json:"passphrase"`        // else: protect it with this one ("" = unencrypted)
		CurrentPassphrase string `json:"currentPassphrase"` // needed when the passphrase is not remembered
		PPKVersion        int    `json:"ppkVersion"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if err := checkPassphrase(req.Passphrase); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	res, err := h.exportStored(c, u, k, req.Format, req.KeepPassphrase, req.Passphrase, req.CurrentPassphrase, req.PPKVersion)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}

func (h *handler) exportStored(c *echo.Context, u *model.User, k *model.SSHKey, format string, keep bool, pass, current string, ppk int) (*exportResult, error) {
	pub, err := keyPublic(k)
	if err != nil {
		return nil, err
	}
	base := fileBase(k.Name, k.Type)
	if isPublicFormat(format) {
		res, err := exportPublicKey(pub, format, k.Comment, base)
		return res, httpKeyError(err)
	}
	if !isPrivateFormat(format) {
		return nil, httpx.BadRequest("unknown format")
	}
	ctx := c.Request().Context()
	pk, err := h.material(ctx, u, k, current)
	if err != nil {
		return nil, err
	}
	if keep {
		pass = ""
		if k.HasPassphrase {
			pass = current
			if pass == "" && len(k.PassphraseEnc) > 0 {
				pp, err := h.d.Vault.Open(k.PassphraseEnc)
				if err != nil {
					if errors.Is(err, model.ErrLocked) {
						return nil, httpx.ErrLocked
					}
					return nil, err
				}
				pass = string(pp)
			}
		}
	}
	res, err := exportPrivateKey(pk, format, k.Comment, pass, ppk, base)
	if err != nil {
		return nil, httpKeyError(err)
	}
	h.d.Audit.Log(c, "key.export", k.ID, map[string]any{"name": k.Name, "format": format, "encrypted": res.Encrypted})
	return res, nil
}

// ---- install ------------------------------------------------------------------------------------------------------

func (h *handler) install(c *echo.Context) error {
	var req struct {
		ConnectionID string `json:"connectionId"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if !model.ValidID(req.ConnectionID) {
		return httpx.BadRequest("connectionId is required")
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	k, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	pub, err := keyPublic(k)
	if err != nil {
		return err
	}
	if h.c == nil || h.c.SSH == nil {
		return httpx.NewError(http.StatusServiceUnavailable, "unavailable", "SSH is not available")
	}
	cl, release, err := h.c.SSH.Get(ctx, u, req.ConnectionID)
	if err != nil {
		var he *httpx.HTTPError
		if errors.As(err, &he) || errors.Is(err, context.Canceled) {
			return err
		}
		// 422, not 502: the router hides the message of 5xx errors, and the user needs the reason (SPEC §9 keys).
		return httpx.NewError(http.StatusUnprocessableEntity, "connect_failed", err.Error())
	}
	defer release()
	comment := k.Comment
	if comment == "" {
		comment = k.Name
	}
	res, err := installPublicKey(ctx, cl, pub, authorizedKeyLine(pub, comment))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return httpx.NewError(http.StatusUnprocessableEntity, "install_failed", err.Error())
	}
	res.Target = fmt.Sprintf("%s@%s:%d", cl.Conn.Username, cl.Conn.Host, cl.Conn.Port)
	h.d.Audit.Log(c, "key.install", k.ID, map[string]any{"name": k.Name, "connectionId": req.ConnectionID,
		"target": res.Target, "alreadyPresent": res.AlreadyPresent})
	return c.JSON(http.StatusOK, res)
}

// ---- sign certificates ------------------------------------------------------------------------------------------

func (h *handler) signCertificate(c *echo.Context) error {
	var req signRequest
	if err := httpx.BindLimit(c, &req, maxKeyRequestBody); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	ca, err := h.ownKey(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	var (
		subjectPub     ssh.PublicKey
		subjectComment string
		subjectKey     *model.SSHKey
	)
	if req.SubjectKeyID != "" {
		if subjectKey, err = h.ownKey(ctx, u, req.SubjectKeyID); err != nil {
			return httpx.BadRequest("subject key not found")
		}
		if subjectPub, err = keyPublic(subjectKey); err != nil {
			return err
		}
		subjectComment = subjectKey.Comment
	} else {
		if subjectPub, subjectComment, err = parsePublicKeyText(req.PublicKey); err != nil {
			return httpx.BadRequest("invalid subject public key: " + err.Error())
		}
	}
	if sameKey(subjectPub, mustPublic(ca)) {
		return httpx.BadRequest("a key cannot certify itself")
	}
	t := now()
	cert, err := buildCertificate(&req, subjectPub, t)
	if err != nil {
		return httpKeyError(err)
	}
	if req.Attach && (subjectKey == nil || cert.CertType != ssh.UserCert) {
		return httpx.BadRequest("only user certificates of stored keys can be attached")
	}
	pk, err := h.material(ctx, u, ca, req.CAPassphrase)
	if err != nil {
		return err
	}
	signer, err := caSigner(pk)
	if err != nil {
		return httpKeyError(err)
	}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		return err
	}
	line := authorizedKeyLine(cert, subjectComment)
	res := signResult{Certificate: line + "\n", Info: describeCert(cert, t), Filename: "id_" + strings.ToLower(keyTypeName(subjectPub)) + "-cert.pub"}
	if subjectKey != nil {
		res.Filename = fileBase(subjectKey.Name, subjectKey.Type) + "-cert.pub"
	}
	if req.Attach {
		subjectKey.Certificate = line
		if err := h.d.Store.Keys.Update(ctx, subjectKey); err != nil {
			return err
		}
		res.Attached = true
	}
	h.d.Audit.Log(c, "key.sign", ca.ID, map[string]any{"ca": ca.Name, "type": certTypeName(cert.CertType),
		"identity": cert.KeyId, "principals": cert.ValidPrincipals, "serial": fmt.Sprint(cert.Serial),
		"subject": ssh.FingerprintSHA256(subjectPub), "attached": res.Attached})
	return c.JSON(http.StatusOK, res)
}

func mustPublic(k *model.SSHKey) ssh.PublicKey {
	pub, err := keyPublic(k)
	if err != nil {
		return nil
	}
	return pub
}

func keyTypeName(pub ssh.PublicKey) string {
	t, _ := keyTypeBits(pub)
	return t
}
