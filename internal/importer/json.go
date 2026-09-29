package importer

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// Termstead native JSON export/import (IMP-2/IMP-3). The document carries folders, connections, identities, keys and
// snippets. Secrets (connection/identity secret maps, private-key material and passphrases) are present only when the
// export was passphrase-encrypted (see crypto.go); a plaintext export omits them, keeping only secretKeys as hints.

const (
	jsonFormatMarker    = "termstead-export"
	jsonFormatEncrypted = "termstead-encrypted" // the crypto envelope's Envelope value
	jsonExportVersion   = 1
	payloadExport       = "termstead-export"
)

// exportDoc is the plaintext export document.
type exportDoc struct {
	Format          string             `json:"format"`
	Version         int                `json:"version"`
	ExportedAt      string             `json:"exportedAt,omitempty"`
	IncludesSecrets bool               `json:"includesSecrets"`
	Folders         []exportFolder     `json:"folders"`
	Connections     []exportConnection `json:"connections"`
	Identities      []exportIdentity   `json:"identities,omitempty"`
	Keys            []exportKey        `json:"keys,omitempty"`
	Snippets        []exportSnippet    `json:"snippets,omitempty"`
}

type exportFolder struct {
	ID        string `json:"id"`
	ParentID  string `json:"parentId,omitempty"`
	Name      string `json:"name"`
	Color     string `json:"color,omitempty"`
	Icon      string `json:"icon,omitempty"`
	SortOrder int    `json:"sortOrder,omitempty"`
}

type exportConnection struct {
	ID         string            `json:"id"`
	FolderID   string            `json:"folderId,omitempty"`
	Name       string            `json:"name"`
	Protocol   model.Protocol    `json:"protocol"`
	Host       string            `json:"host"`
	Port       int               `json:"port"`
	Username   string            `json:"username,omitempty"`
	IdentityID string            `json:"identityId,omitempty"`
	KeyID      string            `json:"keyId,omitempty"`
	AuthMethod string            `json:"authMethod,omitempty"`
	Color      string            `json:"color,omitempty"`
	Icon       string            `json:"icon,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Notes      string            `json:"notes,omitempty"`
	Favorite   bool              `json:"favorite,omitempty"`
	Options    model.Options     `json:"options,omitempty"`
	SecretKeys []string          `json:"secretKeys,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

type exportIdentity struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Username   string            `json:"username,omitempty"`
	KeyID      string            `json:"keyId,omitempty"`
	SecretKeys []string          `json:"secretKeys,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

type exportKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Bits        int    `json:"bits,omitempty"`
	PublicKey   string `json:"publicKey,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Comment     string `json:"comment,omitempty"`
	HasPass     bool   `json:"hasPassphrase,omitempty"`
	Certificate string `json:"certificate,omitempty"`
	PrivateKey  string `json:"privateKey,omitempty"` // present only in an encrypted export
	Passphrase  string `json:"passphrase,omitempty"` // present only in an encrypted export
}

type exportSnippet struct {
	Name        string   `json:"name"`
	Folder      string   `json:"folder,omitempty"`
	Description string   `json:"description,omitempty"`
	Content     string   `json:"content"`
	Tags        []string `json:"tags,omitempty"`
	SendMode    string   `json:"sendMode,omitempty"`
	Shortcut    string   `json:"shortcut,omitempty"`
}

func parseTermsteadJSON(content []byte, opts previewOptions) (*parsed, error) {
	decrypted := false
	if isEncryptedEnvelope(content) {
		if opts.Passphrase == "" {
			return nil, needPassphrase()
		}
		pt, payload, err := decrypt(content, opts.Passphrase)
		if err != nil {
			return nil, err
		}
		if payload != payloadExport {
			return nil, badRequest("this encrypted file is not a Termstead sessions export")
		}
		content, decrypted = pt, true
	}
	var doc exportDoc
	dec := json.NewDecoder(strings.NewReader(string(content)))
	if err := dec.Decode(&doc); err != nil {
		return nil, badRequest("invalid Termstead JSON export: " + truncate(err.Error(), 200))
	}
	if doc.Format != "" && doc.Format != jsonFormatMarker {
		return nil, badRequest("unrecognised export format " + truncate(doc.Format, 40))
	}
	b := newBuilder(fmtJSON)
	// Secrets and private keys are only honoured from an export the user encrypted (a plaintext file claiming to carry
	// them was edited by hand or produced elsewhere).
	droppedPlainSecrets := false

	// Folders: rebuild the hierarchy, mapping export folder ids → temp ids (topologically, tolerant of order).
	folderTemp := map[string]string{}
	byID := map[string]exportFolder{}
	for _, f := range doc.Folders {
		byID[f.ID] = f
	}
	var resolveFolder func(id string, seen map[string]bool) string
	resolveFolder = func(id string, seen map[string]bool) string {
		if id == "" {
			return ""
		}
		if t, ok := folderTemp[id]; ok {
			return t
		}
		f, ok := byID[id]
		if !ok || seen[id] {
			return ""
		}
		seen[id] = true
		parent := resolveFolder(f.ParentID, seen)
		t := b.folder(parent, f.Name, f.Icon, f.Color)
		folderTemp[id] = t
		return t
	}
	for _, f := range doc.Folders {
		resolveFolder(f.ID, map[string]bool{})
	}

	// Keys (material only in an encrypted export).
	keyTemp := map[string]string{}
	for _, k := range doc.Keys {
		if strings.TrimSpace(k.PrivateKey) == "" {
			continue // no material to import (plaintext export)
		}
		if !decrypted {
			droppedPlainSecrets = true
			continue
		}
		tid := b.key(cleanName(k.Name, k.Type+" key"), []byte(k.PrivateKey), "")
		keyTemp[k.ID] = tid
		b.p.keys[len(b.p.keys)-1].pass = k.Passphrase
	}

	// Identities.
	identTemp := map[string]string{}
	for _, id := range doc.Identities {
		pi := &pidentity{
			tempID:   "i" + strconv.Itoa(len(b.p.identities)),
			name:     cleanName(id.Name, id.Username),
			username: truncate(strings.TrimSpace(stripControl(id.Username)), maxUserRunes),
			keyRef:   keyTemp[id.KeyID],
		}
		if len(id.Secrets) > 0 {
			if decrypted {
				pi.secrets = cleanSecrets(id.Secrets)
			} else {
				droppedPlainSecrets = true
			}
		}
		b.p.identities = append(b.p.identities, pi)
		identTemp[id.ID] = pi.tempID
	}

	// Connections. Connection references inside options (jump chains, SSH gateways, Docker-over-SSH) name ids of the
	// exporting instance: they are remapped to the imported connections, or verified at commit time.
	type refs struct {
		pc        *pconn
		jump      []string
		via       string
		dockerVia string
	}
	var pending []refs
	connTemp := map[string]string{}
	droppedSecretHint := false
	for _, ec := range doc.Connections {
		host := strings.TrimSpace(ec.Host)
		proto := ec.Protocol
		if proto == "" {
			proto = model.ProtoSSH
		}
		o := ec.Options.Clone()
		if o == nil {
			o = model.Options{}
		}
		r := refs{jump: o.Strings("jumpHosts"), via: o.String("sshTunnelVia", ""), dockerVia: o.String("viaConnectionId", "")}
		delete(o, "jumpHosts")
		delete(o, "sshTunnelVia")
		delete(o, "viaConnectionId")
		delete(o, syncManagedKey)
		delete(o, syncAliasKey)
		c := model.Connection{
			Name:       cleanName(ec.Name, host),
			Protocol:   proto,
			Host:       host,
			Port:       ec.Port,
			Username:   ec.Username,
			AuthMethod: ec.AuthMethod,
			Color:      ec.Color,
			Icon:       ec.Icon,
			Tags:       ec.Tags,
			Notes:      ec.Notes,
			Favorite:   ec.Favorite,
			Options:    o,
		}
		pc := b.conn(resolveFolderTemp(folderTemp, ec.FolderID), c)
		if pc.tempID == "" {
			continue
		}
		if len(ec.Secrets) > 0 {
			if decrypted {
				pc.secrets = cleanSecrets(ec.Secrets)
			} else {
				droppedPlainSecrets = true
			}
		}
		if len(ec.SecretKeys) > 0 && len(pc.secrets) == 0 {
			droppedSecretHint = true
		}
		pc.identityRef = identTemp[ec.IdentityID]
		pc.keyRef = keyTemp[ec.KeyID]
		if ec.ID != "" {
			connTemp[ec.ID] = pc.tempID
		}
		r.pc = pc
		pending = append(pending, r)
	}
	for _, r := range pending {
		for _, j := range r.jump {
			switch {
			case connTemp[j] != "":
				r.pc.hops = append(r.pc.hops, phop{ref: connTemp[j]})
			case reConnID.MatchString(j):
				r.pc.hops = append(r.pc.hops, phop{existingID: j})
			default:
				if h, ok := parseHopSpec(j); ok {
					r.pc.hops = append(r.pc.hops, h)
				}
			}
		}
		if r.via != "" {
			if t := connTemp[r.via]; t != "" {
				r.pc.hops = append(r.pc.hops, phop{ref: t})
			} else if reConnID.MatchString(r.via) {
				r.pc.hops = append(r.pc.hops, phop{existingID: r.via})
			}
		}
		if r.dockerVia != "" {
			if r.pc.refOpts == nil {
				r.pc.refOpts = map[string]string{}
			}
			if t := connTemp[r.dockerVia]; t != "" {
				r.pc.refOpts["viaConnectionId"] = t
			} else if reConnID.MatchString(r.dockerVia) {
				r.pc.refOpts["viaConnectionId"] = "id:" + r.dockerVia
			}
		}
	}

	// Snippets.
	for _, s := range doc.Snippets {
		if strings.TrimSpace(s.Content) == "" && strings.TrimSpace(s.Name) == "" {
			continue
		}
		if len(b.p.snippets) >= maxImportItems {
			b.overflow = true
			break
		}
		b.p.snippets = append(b.p.snippets, &psnippet{
			name: cleanName(stripControl(s.Name), "Snippet"), folder: truncate(stripControl(s.Folder), maxNameRunes),
			description: truncateBytes(s.Description, maxNotesBytes), content: truncateBytes(s.Content, 1<<20),
			sendMode: s.SendMode, shortcut: truncate(stripControl(s.Shortcut), 64), tags: cleanTags(s.Tags),
		})
	}

	if len(b.p.conns) == 0 && len(b.p.folders) == 0 && len(b.p.snippets) == 0 && len(b.p.identities) == 0 {
		return nil, badRequest("the export contains no connections")
	}
	if droppedPlainSecrets {
		b.warn("Secrets and private keys found in this unencrypted file were ignored — only passphrase-encrypted Termstead exports carry secrets.")
	} else if droppedSecretHint {
		b.warn("This export was not encrypted, so stored passwords/keys were not included. Re-export with a passphrase to carry secrets.")
	}
	return b.finish()
}

// reConnID matches a Termstead id (model.NewID: 20 lowercase base32 characters).
var reConnID = regexp.MustCompile(`^[a-z2-7]{20}$`)

func resolveFolderTemp(m map[string]string, id string) string {
	if id == "" {
		return ""
	}
	return m[id]
}

// needPassphrase is the typed error (422 passphrase_required) asking the wizard to prompt for a decryption passphrase.
func needPassphrase() error {
	return httpx.NewError(http.StatusUnprocessableEntity, "passphrase_required", "this export is encrypted; a passphrase is required")
}
