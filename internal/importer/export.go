package importer

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// export formats.
const (
	exportJSON      = "json"
	exportCSV       = "csv"
	exportSSHConfig = "ssh_config"
)

// exportParams control an export.
type exportParams struct {
	format         string
	includeSecrets bool
	passphrase     string
	folderID       string // "" = everything the user owns
	connectionIDs  []string
}

// exportResult is a rendered export file.
type exportResult struct {
	Data        []byte
	Filename    string
	ContentType string
}

// runExport gathers the user's data and renders it. includeSecrets requires a passphrase and always yields an
// encrypted envelope (secrets never leave the server in cleartext).
func runExport(ctx context.Context, d *app.Deps, user *model.User, pr exportParams) (exportResult, error) {
	if pr.includeSecrets {
		if pr.format != exportJSON {
			return exportResult{}, httpx.BadRequest("secrets can only be included in a JSON export")
		}
		if pr.passphrase == "" {
			return exportResult{}, httpx.BadRequest("a passphrase is required to export secrets")
		}
		if d.Vault != nil && d.Vault.Locked() {
			return exportResult{}, httpx.ErrLocked
		}
	}

	all, err := d.Store.Connections.ListByOwner(ctx, user.ID)
	if err != nil {
		return exportResult{}, err
	}
	folders, err := d.Store.Folders.ListByOwner(ctx, user.ID)
	if err != nil {
		return exportResult{}, err
	}
	conns, folders := filterExportScope(all, folders, pr)

	switch pr.format {
	case exportCSV:
		return exportCSVFile(conns, folders), nil
	case exportSSHConfig:
		return exportSSHConfigFile(conns, all), nil
	case exportJSON, "":
		return exportJSONFile(ctx, d, user, conns, folders, pr)
	default:
		return exportResult{}, httpx.BadRequest("unknown export format " + pr.format)
	}
}

// filterExportScope restricts to a folder subtree and/or an explicit connection id set.
func filterExportScope(conns []*model.Connection, folders []*model.Folder, pr exportParams) ([]*model.Connection, []*model.Folder) {
	if pr.folderID != "" {
		sub := folderSubtree(folders, pr.folderID)
		var fc []*model.Connection
		for _, c := range conns {
			if sub[c.FolderID] {
				fc = append(fc, c)
			}
		}
		var ff []*model.Folder
		for _, f := range folders {
			if sub[f.ID] {
				ff = append(ff, f)
			}
		}
		conns, folders = fc, ff
	}
	if len(pr.connectionIDs) > 0 {
		want := map[string]bool{}
		for _, id := range pr.connectionIDs {
			want[id] = true
		}
		var fc []*model.Connection
		for _, c := range conns {
			if want[c.ID] {
				fc = append(fc, c)
			}
		}
		conns = fc
	}
	return conns, folders
}

func folderSubtree(folders []*model.Folder, root string) map[string]bool {
	children := map[string][]string{}
	for _, f := range folders {
		children[f.ParentID] = append(children[f.ParentID], f.ID)
	}
	out := map[string]bool{root: true}
	var walk func(id string)
	walk = func(id string) {
		for _, c := range children[id] {
			if !out[c] {
				out[c] = true
				walk(c)
			}
		}
	}
	walk(root)
	return out
}

func exportJSONFile(ctx context.Context, d *app.Deps, user *model.User, conns []*model.Connection, folders []*model.Folder, pr exportParams) (exportResult, error) {
	doc := exportDoc{
		Format: jsonFormatMarker, Version: jsonExportVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339),
		IncludesSecrets: pr.includeSecrets,
		Folders:         make([]exportFolder, 0, len(folders)),
		Connections:     make([]exportConnection, 0, len(conns)),
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	for _, f := range folders {
		doc.Folders = append(doc.Folders, exportFolder{ID: f.ID, ParentID: f.ParentID, Name: f.Name, Color: f.Color, Icon: f.Icon, SortOrder: f.SortOrder})
	}
	usedIdentities := map[string]bool{}
	usedKeys := map[string]bool{}
	for _, c := range conns {
		ec := exportConnection{
			ID: c.ID, FolderID: c.FolderID, Name: c.Name, Protocol: c.Protocol, Host: c.Host, Port: c.Port,
			Username: c.Username, IdentityID: c.IdentityID, KeyID: c.KeyID, AuthMethod: c.AuthMethod, Color: c.Color,
			Icon: c.Icon, Tags: c.Tags, Notes: c.Notes, Favorite: c.Favorite, Options: c.Options, SecretKeys: c.SecretKeys,
		}
		if c.IdentityID != "" {
			usedIdentities[c.IdentityID] = true
		}
		if c.KeyID != "" {
			usedKeys[c.KeyID] = true
		}
		if pr.includeSecrets {
			if m, err := d.Vault.OpenJSON(c.SecretsEnc); err == nil && len(m) > 0 {
				ec.Secrets = m
			}
		}
		doc.Connections = append(doc.Connections, ec)
	}
	exportIdentitiesAndKeys(ctx, d, user, &doc, usedIdentities, usedKeys, pr.includeSecrets)
	if snips, err := d.Store.Snippets.ListByOwner(ctx, user.ID); err == nil {
		for _, s := range snips {
			doc.Snippets = append(doc.Snippets, exportSnippet{Name: s.Name, Folder: s.Folder, Description: s.Description,
				Content: s.Content, Tags: s.Tags, SendMode: s.SendMode, Shortcut: s.Shortcut})
		}
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return exportResult{}, err
	}
	ts := time.Now().Format("20060102-150405")
	if pr.includeSecrets {
		enc, err := encrypt(body, pr.passphrase, payloadExport)
		if err != nil {
			return exportResult{}, err
		}
		return exportResult{Data: enc, Filename: "termstead-sessions-" + ts + ".secret.json", ContentType: "application/json"}, nil
	}
	return exportResult{Data: body, Filename: "termstead-sessions-" + ts + ".json", ContentType: "application/json"}, nil
}

// exportIdentitiesAndKeys adds the referenced identities (secrets only in an encrypted export) and, for an encrypted
// export, the referenced keys with their private material.
func exportIdentitiesAndKeys(ctx context.Context, d *app.Deps, user *model.User, doc *exportDoc, idents, keys map[string]bool, withSecrets bool) {
	if idlist, err := d.Store.Identities.ListByOwner(ctx, user.ID); err == nil {
		for _, i := range idlist {
			if !idents[i.ID] {
				continue
			}
			ei := exportIdentity{ID: i.ID, Name: i.Name, Username: i.Username, KeyID: i.KeyID, SecretKeys: i.SecretKeys}
			if withSecrets {
				if m, err := d.Vault.OpenJSON(i.SecretsEnc); err == nil && len(m) > 0 {
					ei.Secrets = m
				}
			}
			doc.Identities = append(doc.Identities, ei)
			if i.KeyID != "" {
				keys[i.KeyID] = true
			}
		}
	}
	if !withSecrets {
		return
	}
	if keylist, err := d.Store.Keys.ListByOwner(ctx, user.ID); err == nil {
		for _, k := range keylist {
			if !keys[k.ID] {
				continue
			}
			ek := exportKey{ID: k.ID, Name: k.Name, Type: k.Type, Bits: k.Bits, PublicKey: k.PublicKey,
				Fingerprint: k.Fingerprint, Comment: k.Comment, HasPass: k.HasPassphrase, Certificate: k.Certificate}
			if pem, err := d.Vault.Open(k.PrivateKeyEnc); err == nil && len(pem) > 0 {
				ek.PrivateKey = string(pem)
			}
			if len(k.PassphraseEnc) > 0 {
				if pp, err := d.Vault.Open(k.PassphraseEnc); err == nil {
					ek.Passphrase = string(pp)
				}
			}
			doc.Keys = append(doc.Keys, ek)
		}
	}
}

func exportCSVFile(conns []*model.Connection, folders []*model.Folder) exportResult {
	path := folderPaths(folders)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Groups", "Label", "Tags", "Hostname/IP", "Protocol", "Port", "Username"})
	for _, c := range conns {
		_ = w.Write([]string{
			csvCell(path[c.FolderID]), csvCell(c.Name), csvCell(strings.Join(c.Tags, " ")), csvCell(c.Host),
			string(c.Protocol), strconv.Itoa(c.Port), csvCell(c.Username),
		})
	}
	w.Flush()
	ts := time.Now().Format("20060102-150405")
	return exportResult{Data: buf.Bytes(), Filename: "termstead-sessions-" + ts + ".csv", ContentType: "text/csv"}
}

// folderPaths maps folder id → "A/B/C" path.
func folderPaths(folders []*model.Folder) map[string]string {
	byID := map[string]*model.Folder{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	cache := map[string]string{}
	var path func(id string, depth int) string
	path = func(id string, depth int) string {
		if id == "" || depth > 64 {
			return ""
		}
		if p, ok := cache[id]; ok {
			return p
		}
		f, ok := byID[id]
		if !ok {
			return ""
		}
		parent := path(f.ParentID, depth+1)
		p := f.Name
		if parent != "" {
			p = parent + "/" + f.Name
		}
		cache[id] = p
		return p
	}
	out := map[string]string{}
	for _, f := range folders {
		out[f.ID] = path(f.ID, 0)
	}
	return out
}

func exportSSHConfigFile(conns, all []*model.Connection) exportResult {
	var buf bytes.Buffer
	buf.WriteString("# Termstead ssh_config export — generated " + time.Now().UTC().Format(time.RFC3339) + "\n")
	buf.WriteString("# Only ssh/sftp/mosh connections are represented; keys and passwords are not exported.\n\n")
	byID := map[string]*model.Connection{}
	for _, c := range all {
		byID[c.ID] = c
	}
	// Aliases first, so ProxyJump can name the alias of an exported jump host.
	alias := map[string]string{}
	seen := map[string]int{}
	for _, c := range conns {
		if !isSSHFamily(c.Protocol) {
			continue
		}
		a := sshConfigAlias(c.Name)
		seen[strings.ToLower(a)]++
		if n := seen[strings.ToLower(a)]; n > 1 {
			a = fmt.Sprintf("%s-%d", a, n)
		}
		alias[c.ID] = a
	}
	for _, c := range conns {
		a, ok := alias[c.ID]
		if !ok {
			continue
		}
		host, ok := sshConfigToken(c.Host)
		if !ok {
			fmt.Fprintf(&buf, "# skipped %s: its host name cannot be written to ssh_config\n\n", a)
			continue
		}
		fmt.Fprintf(&buf, "Host %s\n", a)
		fmt.Fprintf(&buf, "    HostName %s\n", host)
		if c.Port != 0 && c.Port != 22 {
			fmt.Fprintf(&buf, "    Port %d\n", c.Port)
		}
		if u, ok := sshConfigValue(c.Username); ok {
			fmt.Fprintf(&buf, "    User %s\n", u)
		}
		var hops []string
		for _, j := range c.Options.Strings("jumpHosts") {
			if ja, ok := alias[j]; ok {
				hops = append(hops, ja)
			} else if jc, ok := byID[j]; ok {
				if h, ok := sshConfigToken(phop{host: jc.Host, port: jc.Port, user: jc.Username}.spec()); ok {
					hops = append(hops, h)
				}
			} else if h, ok := sshConfigToken(j); ok && !reConnID.MatchString(j) {
				hops = append(hops, h)
			}
		}
		if len(hops) > 0 { // one directive: OpenSSH only honours the first ProxyJump line
			fmt.Fprintf(&buf, "    ProxyJump %s\n", strings.Join(hops, ","))
		} else if pcmd, ok := sshConfigLine(c.Options.String("proxyCommand", "")); ok {
			fmt.Fprintf(&buf, "    ProxyCommand %s\n", pcmd)
		}
		if c.Options.Bool("compression") {
			buf.WriteString("    Compression yes\n")
		}
		if c.Options.Bool("agentForwarding") {
			buf.WriteString("    ForwardAgent yes\n")
		}
		if c.Options.Bool("x11Forwarding") {
			buf.WriteString("    ForwardX11 yes\n")
		}
		if c.Options.Has("keepAliveSec") {
			fmt.Fprintf(&buf, "    ServerAliveInterval %d\n", c.Options.Int("keepAliveSec"))
		}
		if t := c.Options.Int("connectTimeoutSec"); t > 0 {
			fmt.Fprintf(&buf, "    ConnectTimeout %d\n", t)
		}
		if rc, ok := sshConfigLine(c.Options.String("remoteCommand", "")); ok {
			fmt.Fprintf(&buf, "    RemoteCommand %s\n", rc)
		}
		for _, f := range exportForwards(c.Options) {
			buf.WriteString("    " + f + "\n")
		}
		buf.WriteString("\n")
	}
	ts := time.Now().Format("20060102-150405")
	return exportResult{Data: buf.Bytes(), Filename: "termstead-ssh-config-" + ts + ".txt", ContentType: "text/plain; charset=utf-8"}
}

// exportForwards renders options.forwards as Local/Remote/DynamicForward directives (TCP forwards only).
func exportForwards(o model.Options) []string {
	var list []struct {
		Type     string `json:"type"`
		BindHost string `json:"bindHost"`
		BindPort int    `json:"bindPort"`
		DestHost string `json:"destHost"`
		DestPort int    `json:"destPort"`
		Reverse  bool   `json:"reverse"`
		Disabled bool   `json:"disabled"`
	}
	if o.Decode("forwards", &list) != nil {
		return nil
	}
	bind := func(h string, p int) string {
		if h == "" {
			return strconv.Itoa(p)
		}
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		return h + ":" + strconv.Itoa(p)
	}
	var out []string
	for _, f := range list {
		if f.Disabled || f.BindPort <= 0 || f.BindPort > 65535 {
			continue
		}
		if _, ok := sshConfigToken(f.BindHost + "x"); !ok || strings.ContainsAny(f.DestHost, " \t\r\n\"") {
			continue
		}
		switch {
		case f.Type == "dynamic" && f.Reverse:
			out = append(out, "RemoteForward "+bind(f.BindHost, f.BindPort))
		case f.Type == "dynamic":
			out = append(out, "DynamicForward "+bind(f.BindHost, f.BindPort))
		case (f.Type == "local" || f.Type == "remote") && f.DestHost != "" && f.DestPort > 0 && f.DestPort <= 65535:
			kw := map[string]string{"local": "LocalForward", "remote": "RemoteForward"}[f.Type]
			out = append(out, kw+" "+bind(f.BindHost, f.BindPort)+" "+bind(f.DestHost, f.DestPort))
		}
	}
	return out
}

// sshConfigToken accepts a value that must be a single ssh_config token (host names, hop specs).
func sshConfigToken(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, " \t\r\n\x00\"'#") {
		return "", false
	}
	return v, true
}

// sshConfigValue renders a single-line value, quoting it when it contains spaces; values that cannot be represented
// (line breaks, quotes) are refused so an exported file can never gain extra directives.
func sshConfigValue(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "\r\n\x00\"") {
		return "", false
	}
	if strings.ContainsAny(v, " \t#") {
		return `"` + v + `"`, true
	}
	return v, true
}

// sshConfigLine accepts a free-form single-line value (commands): anything without line breaks.
func sshConfigLine(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return "", false
	}
	return v, true
}

// csvCell neutralises spreadsheet formula injection (a cell starting with = + - @ or a control character is prefixed
// with an apostrophe), leaving plain numbers alone.
func csvCell(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return "'" + v
		}
	}
	return v
}

// sshConfigAlias turns a connection name into a safe Host alias token.
func sshConfigAlias(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "host"
	}
	return truncate(s, 64)
}
