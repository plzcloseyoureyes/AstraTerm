// Package importer imports saved sessions from other clients (MobaXterm, PuTTY/KiTTY, OpenSSH ~/.ssh/config, Termius,
// mRemoteNG, Remmina, FileZilla, WinSCP, SecureCRT, generic CSV, NexTerm JSON and OpenSSH known_hosts), exports the
// user's own sessions (JSON / CSV / ssh_config, optionally passphrase-encrypted), and provides admin backup/restore.
//
// # Security
//
// Import is deliberately read-only with respect to credentials found in third-party files. MobaXterm keeps saved
// passwords in its own encrypted password store ([Passwords] / [Credentials] of MobaXterm.ini or the registry, never
// in the bookmark lines), PuTTY never stores them, and mRemoteNG (AES), FileZilla, WinSCP and Remmina protect or
// obfuscate theirs — none of it is decrypted or read. The only plaintext credential MobaXterm puts in a bookmark line
// (an SFTP session's proxy password) is dropped with a warning. Plaintext secrets and private keys are only ever read
// from a NexTerm JSON export that the user encrypted with a passphrase (see export.go / json.go). The importer never
// guesses, cracks or reverse-engineers another product's credential storage.
package importer

import (
	"strconv"
	"strings"

	"github.com/nexterm/nexterm/internal/model"
)

// Supported import formats (the `format` field of preview/commit; "auto" asks the server to detect it).
const (
	fmtAuto       = "auto"
	fmtMobaXterm  = "mobaxterm"
	fmtPuttyReg   = "putty_reg"
	fmtSSHConfig  = "ssh_config"
	fmtTermiusCSV = "termius_csv"
	fmtMRemoteNG  = "mremoteng"
	fmtRemmina    = "remmina"
	fmtFileZilla  = "filezilla"
	fmtWinSCP     = "winscp"
	fmtSecureCRT  = "securecrt"
	fmtCSV        = "csv"
	fmtJSON       = "json"
	fmtKnownHosts = "known_hosts"
)

// importFormats lists every format a preview/commit accepts, in the order the wizard offers them.
var importFormats = []string{
	fmtMobaXterm, fmtPuttyReg, fmtSSHConfig, fmtTermiusCSV, fmtMRemoteNG, fmtRemmina,
	fmtFileZilla, fmtWinSCP, fmtSecureCRT, fmtCSV, fmtJSON, fmtKnownHosts,
}

// parsed is the normalized, format-independent result of parsing one import source. Temp IDs are assigned
// deterministically in parse order (f0, f1, … / c0, c1, … / k0, …) so that a preview and a later commit of the very
// same content agree on which IDs the caller selected — the server keeps no per-preview state.
type parsed struct {
	format     string
	folders    []*pfolder
	conns      []*pconn
	keys       []*pkey
	knownHosts []*pknownHost
	identities []*pidentity // only from a native NexTerm JSON import
	snippets   []*psnippet  // only from a native NexTerm JSON import
	warnings   []string
	// unsupported counts entries recognised but not mappable (reported, not imported).
	unsupported int
}

// pfolder is a folder to (re)create.
type pfolder struct {
	tempID   string
	parentID string // temp id of the parent folder, or "" for a top-level folder (placed under the import target)
	name     string
	color    string
	icon     string
}

// pconn is one connection to import.
type pconn struct {
	tempID   string
	folderID string // temp folder id, or "" for the import target root
	conn     model.Connection
	// secrets holds plaintext secrets to store (only from an encrypted NexTerm export the user decrypted). Never
	// populated from third-party formats.
	secrets map[string]string
	// keyRef links to a pkey.tempID whose material should be imported (desktop mode) and referenced by keyId.
	keyRef string
	// identityRef links to a pidentity.tempID (native JSON import).
	identityRef string
	// keyPath is the raw IdentityFile / PublicKeyFile path when the key could not be read (recorded as a warning).
	keyPath string
	// hops is the SSH jump chain (first hop first). For SSH-family protocols it becomes options.jumpHosts; for other
	// network protocols the last hop becomes options.sshTunnelVia (a saved SSH connection, created when needed).
	hops []phop
	// refOpts maps single-connection-ID options (e.g. docker "viaConnectionId") to the temp id they reference.
	refOpts  map[string]string
	warnings []string
}

// phop is one hop of an SSH jump chain: a reference to another connection of the same import (ref = its temp id,
// e.g. an ssh_config alias named by ProxyJump) or an inline gateway spec (MobaXterm / WinSCP / Remmina gateways,
// ad-hoc ProxyJump entries).
type phop struct {
	ref        string
	existingID string // an id of an existing connection (NexTerm JSON re-import); verified at commit
	host       string
	port       int
	user       string
	keyPath    string // the gateway's own private key file (MobaXterm), imported in desktop mode
}

// spec renders an inline hop as the ad-hoc "[user@]host[:port]" form the SSH dialer accepts.
func (h phop) spec() string {
	s := h.host
	if strings.Contains(s, ":") && !strings.HasPrefix(s, "[") {
		s = "[" + s + "]" // IPv6 literal
	}
	if h.user != "" {
		s = h.user + "@" + s
	}
	if h.port > 0 && h.port != 22 {
		s += ":" + strconv.Itoa(h.port)
	}
	return s
}

// parseHopSpec parses an ad-hoc "[user@]host[:port]" hop ("[v6]:port" accepted).
func parseHopSpec(s string) (phop, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return phop{}, false
	}
	var h phop
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		h.user, s = s[:at], s[at+1:]
	}
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return phop{}, false
		}
		h.host = s[1:end]
		if rest := s[end+1:]; strings.HasPrefix(rest, ":") {
			h.port = atoiSafe(rest[1:], 0)
		}
	case strings.Count(s, ":") == 1:
		i := strings.IndexByte(s, ':')
		h.host, h.port = s[:i], atoiSafe(s[i+1:], 0)
	default:
		h.host = s
	}
	if h.host == "" || h.port < 0 || h.port > 65535 {
		return phop{}, false
	}
	return h, true
}

// pidentity is a reusable credential set (native JSON import only).
type pidentity struct {
	tempID   string
	name     string
	username string
	keyRef   string
	secrets  map[string]string
}

// psnippet is a snippet to import (native JSON import only).
type psnippet struct {
	name        string
	folder      string
	description string
	content     string
	sendMode    string
	shortcut    string
	tags        []string
}

// pkey is SSH key material discovered during import (desktop mode: readable IdentityFile, or a decrypted JSON export).
type pkey struct {
	tempID string
	name   string
	text   []byte // raw private-key text (OpenSSH / PEM / PPK)
	pass   string // passphrase to remember (native JSON export only)
	path   string // source path (for the audit trail)
}

// pknownHost is a trusted host key to add to the global known_hosts store.
type pknownHost struct {
	host        string
	port        int
	keyType     string
	publicKey   string // authorized_keys format
	fingerprint string
	comment     string
}

// ---- JSON contract (preview / commit) -----------------------------------------------------------------------------

// previewRequest is the body of POST /api/import/preview.
type previewRequest struct {
	Format  string         `json:"format"`
	Content string         `json:"content"`          // text, or base64 when Base64 is true
	Base64  bool           `json:"base64,omitempty"` // Content is base64-encoded
	Path    string         `json:"path,omitempty"`   // desktop mode: read a file returned by /import/discover instead of Content
	Options previewOptions `json:"options,omitempty"`
}

// previewOptions carry format hints (the CSV column mapping, a passphrase for encrypted JSON, a legacy charset).
type previewOptions struct {
	Passphrase string            `json:"passphrase,omitempty"` // decrypt an encrypted NexTerm JSON export
	CSVMapping map[string]string `json:"csvMapping,omitempty"` // generic CSV: header name → field (name/host/port/username/protocol/folder/notes/tags)
	CSVDelim   string            `json:"csvDelim,omitempty"`   // generic CSV delimiter override ("," ";" "\t" "|")
	// Charset decodes legacy 8-bit text files ("" / "auto" = UTF-16 by BOM, UTF-8 when valid, else Windows-1252 —
	// MobaXterm.ini's charset on Western systems). Any WHATWG label works: "windows-1255", "gbk", "shift_jis", …
	Charset string `json:"charset,omitempty"`
}

// previewFolder is a folder node of the preview tree.
type previewFolder struct {
	ID       string `json:"id"`       // temp id
	ParentID string `json:"parentId"` // temp id of parent, or ""
	Name     string `json:"name"`
	Icon     string `json:"icon,omitempty"`
	Color    string `json:"color,omitempty"`
}

// previewConnection is a connection of the preview tree (write-only secrets are never echoed; only their names).
type previewConnection struct {
	ID          string         `json:"id"`       // temp id (pass in commit's selectedIds)
	FolderID    string         `json:"folderId"` // temp folder id, or ""
	Name        string         `json:"name"`
	Protocol    model.Protocol `json:"protocol"`
	Host        string         `json:"host"`
	Port        int            `json:"port"`
	Username    string         `json:"username,omitempty"`
	AuthMethod  string         `json:"authMethod"`
	Options     model.Options  `json:"options"`
	SecretKeys  []string       `json:"secretKeys"` // names of secrets that would be imported
	Icon        string         `json:"icon,omitempty"`
	Color       string         `json:"color,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Notes       string         `json:"notes,omitempty"`
	Duplicate   bool           `json:"duplicate"`             // an equivalent connection already exists
	DuplicateOf string         `json:"duplicateOf,omitempty"` // id of the existing connection
	KeyName     string         `json:"keyName,omitempty"`     // an SSH key that would be imported alongside
	Via         []string       `json:"via,omitempty"`         // SSH jump chain / gateway, first hop first (display)
	// RunsLocalCommand flags connections that execute a program on the NexTerm host when opened (ProxyCommand,
	// local shell) — the wizard highlights them because an import file could carry a malicious one.
	RunsLocalCommand bool     `json:"runsLocalCommand,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

// previewKey is an SSH key that would be imported (desktop mode).
type previewKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Bits        int    `json:"bits,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Encrypted   bool   `json:"encrypted"`
	Duplicate   bool   `json:"duplicate"`
}

// previewKnownHost is a host key that would be trusted.
type previewKnownHost struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	Duplicate   bool   `json:"duplicate"` // this exact key is already trusted
	Conflict    bool   `json:"conflict"`  // a different key of this type is trusted for the host (not replaced)
}

// duplicateRef flags a preview connection whose equivalent already exists.
type duplicateRef struct {
	ID           string `json:"id"` // temp id in this preview
	Name         string `json:"name"`
	ExistingID   string `json:"existingId"` // id of the existing connection
	ExistingName string `json:"existingName"`
}

// previewResponse is returned by POST /api/import/preview.
type previewResponse struct {
	Format      string              `json:"format"`
	Folders     []previewFolder     `json:"folders"`
	Connections []previewConnection `json:"connections"`
	Keys        []previewKey        `json:"keys,omitempty"`
	KnownHosts  []previewKnownHost  `json:"knownHosts,omitempty"`
	Warnings    []string            `json:"warnings"`
	Duplicates  []duplicateRef      `json:"duplicates"`
	// Counts is a small summary for the wizard header.
	Counts previewCounts `json:"counts"`
}

type previewCounts struct {
	Folders     int `json:"folders"`
	Connections int `json:"connections"`
	Keys        int `json:"keys"`
	KnownHosts  int `json:"knownHosts"`
	Duplicates  int `json:"duplicates"`
	Unsupported int `json:"unsupported"`
	Identities  int `json:"identities"`
	Snippets    int `json:"snippets"`
}

// commitRequest is the body of POST /api/import/commit.
type commitRequest struct {
	Format         string `json:"format"`
	Content        string `json:"content"`
	Base64         bool   `json:"base64,omitempty"`
	Path           string `json:"path,omitempty"`
	TargetFolderID string `json:"targetFolderId,omitempty"` // import under this existing folder ("" = root)
	// SelectedIDs are the temp connection ids to import: absent/null = all, [] = none (keys / known hosts /
	// snippets only).
	SelectedIDs []string       `json:"selectedIds,omitempty"`
	Dedupe      string         `json:"dedupe,omitempty"` // skip (default) | update | duplicate
	Options     previewOptions `json:"options,omitempty"`
	ImportKeys  *bool          `json:"importKeys,omitempty"`       // desktop mode: import referenced key files (default true)
	ImportKnown *bool          `json:"importKnownHosts,omitempty"` // import parsed host keys (default true)
}

// Dedupe strategies.
const (
	dedupeSkip      = "skip"
	dedupeUpdate    = "update"
	dedupeDuplicate = "duplicate"
)

// commitResponse is returned by POST /api/import/commit.
type commitResponse struct {
	Created             int      `json:"created"`
	Updated             int      `json:"updated"`
	Skipped             int      `json:"skipped"`
	FoldersCreated      int      `json:"foldersCreated"`
	KeysImported        int      `json:"keysImported"`
	KnownHostsAdded     int      `json:"knownHostsAdded"`
	KnownHostsConflicts int      `json:"knownHostsConflicts,omitempty"`
	GatewaysCreated     int      `json:"gatewaysCreated,omitempty"` // saved SSH connections created for gateways
	IdentitiesCreated   int      `json:"identitiesCreated,omitempty"`
	SnippetsCreated     int      `json:"snippetsCreated,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
	// ConnectionIDs lists the created or updated connections (import order) so the UI can reveal them.
	ConnectionIDs []string `json:"connectionIds"`
	// FolderIDs lists the folders created by this import.
	FolderIDs []string `json:"folderIds"`
}

// discoverEntry is one local file the importer offers to read (desktop mode, GET /api/import/discover).
type discoverEntry struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	Label  string `json:"label"`
	Size   int64  `json:"size"`
}
