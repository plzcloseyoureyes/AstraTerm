package keys

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/sshx"
)

// Known hosts manager (SSH-19/20). Entries live in the core known_hosts table (global, shared by every user and
// consulted by sshx); @cert-authority / @revoked markers in the module's table. Reads are open to every user;
// changes need desktop mode or an administrator (a trusted key affects everyone).

const maxKnownHostsBody = 8 << 20

func (h *handler) listKnownHosts(c *echo.Context) error {
	list, err := h.d.Store.KnownHosts.List(c.Request().Context())
	if err != nil {
		return err
	}
	if q := strings.ToLower(strings.TrimSpace(c.QueryParam("q"))); q != "" {
		out := list[:0]
		for _, kh := range list {
			if strings.Contains(strings.ToLower(knownHostName(kh.Host, kh.Port)), q) || strings.Contains(strings.ToLower(kh.KeyType), q) ||
				strings.Contains(strings.ToLower(kh.Fingerprint), q) || strings.Contains(strings.ToLower(kh.Comment), q) {
				out = append(out, kh)
			}
		}
		list = out
	}
	return c.JSON(http.StatusOK, list)
}

// hostPort parses the host (optionally "[host]:port") and port of a request.
func hostPort(host string, port int) (string, int, error) {
	h, p, kind := splitKnownHost(strings.TrimSpace(host))
	switch kind {
	case hostPattern:
		return "", 0, httpx.BadRequest("host patterns are only allowed in certificate authority and revocation entries")
	case hostHashed, hostInvalid:
		return "", 0, httpx.BadRequest("invalid host name")
	}
	if port != 0 {
		if port < 1 || port > 65535 {
			return "", 0, httpx.BadRequest("invalid port")
		}
		p = port
	}
	return h, p, nil
}

func (h *handler) addKnownHost(c *echo.Context) error {
	var req struct {
		Host      string `json:"host"`
		Port      int    `json:"port"`
		PublicKey string `json:"publicKey"`
		Comment   string `json:"comment"`
		Replace   bool   `json:"replace"`
	}
	if err := httpx.BindLimit(c, &req, 256<<10); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if err := h.requireKnownHostsAdmin(u); err != nil {
		return err
	}
	host, port, err := hostPort(req.Host, req.Port)
	if err != nil {
		return err
	}
	pub, comment, err := parsePublicKeyText(req.PublicKey)
	if err != nil {
		return httpx.BadRequest("invalid host key: " + err.Error())
	}
	if _, ok := pub.(*ssh.Certificate); ok {
		return httpx.BadRequest("a certificate cannot be trusted directly; trust its CA instead")
	}
	if c := cleanComment(req.Comment); c != "" {
		comment = c
	}
	if comment == "" {
		comment = "added by " + u.Username
	}
	existing, err := h.d.Store.KnownHosts.Find(ctx, host, port)
	if err != nil {
		return err
	}
	sameType := false
	for _, e := range existing {
		pk, err := sshx.ParseKnownHostKey(e.PublicKey)
		if err != nil {
			continue
		}
		if sameKey(pk, pub) {
			return httpx.NewError(http.StatusConflict, "already_known", "this host key is already trusted")
		}
		if pk.Type() == pub.Type() {
			sameType = true
		}
	}
	if sameType && !req.Replace {
		return httpx.NewError(http.StatusConflict, "host_key_conflict",
			fmt.Sprintf("another %s key is already trusted for %s; replace it?", pub.Type(), knownHostName(host, port)))
	}
	kh := &model.KnownHost{Host: host, Port: port, KeyType: pub.Type(), PublicKey: sshx.FormatKnownHostKey(pub),
		Fingerprint: ssh.FingerprintSHA256(pub), Comment: comment}
	action := "known_host.add"
	if sameType {
		action = "known_host.replace"
		err = h.d.Store.KnownHosts.Replace(ctx, kh)
	} else {
		err = h.d.Store.KnownHosts.Add(ctx, kh)
	}
	if err != nil {
		return err
	}
	h.d.Audit.Log(c, action, knownHostName(host, port), map[string]any{"keyType": kh.KeyType, "fingerprint": kh.Fingerprint})
	return c.JSON(http.StatusCreated, kh)
}

func (h *handler) deleteKnownHost(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if err := h.requireKnownHostsAdmin(u); err != nil {
		return err
	}
	kh, err := h.d.Store.KnownHosts.Get(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if err := h.d.Store.KnownHosts.Delete(ctx, kh.ID); err != nil {
		return err
	}
	h.d.Audit.Log(c, "known_host.delete", knownHostName(kh.Host, kh.Port), map[string]any{"keyType": kh.KeyType, "fingerprint": kh.Fingerprint})
	return httpx.OK(c)
}

func (h *handler) bulkDeleteKnownHosts(c *echo.Context) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if err := h.requireKnownHostsAdmin(u); err != nil {
		return err
	}
	if len(req.IDs) > 10000 {
		return httpx.BadRequest("too many ids")
	}
	deleted := 0
	var hosts []string
	for _, id := range req.IDs {
		kh, err := h.d.Store.KnownHosts.Get(ctx, id)
		if err != nil {
			continue
		}
		if err := h.d.Store.KnownHosts.Delete(ctx, id); err == nil {
			deleted++
			if len(hosts) < 50 {
				hosts = append(hosts, knownHostName(kh.Host, kh.Port))
			}
		}
	}
	h.d.Audit.Log(c, "known_host.bulk_delete", "", map[string]any{"deleted": deleted, "hosts": hosts})
	return c.JSON(http.StatusOK, map[string]int{"deleted": deleted})
}

// knownHostsImportResult is the response of POST /api/known-hosts/import.
type knownHostsImportResult struct {
	Format    string        `json:"format"`    // openssh | putty
	Added     int           `json:"added"`     // new trusted keys
	Replaced  int           `json:"replaced"`  // keys replaced (onConflict=replace)
	Skipped   int           `json:"skipped"`   // already trusted
	Conflicts int           `json:"conflicts"` // another key of the same type is trusted (not imported)
	Hashed    int           `json:"hashed"`    // hashed host names cannot be imported
	Patterns  int           `json:"patterns"`  // wildcard / negated host patterns of plain entries (not supported)
	Markers   int           `json:"markers"`   // @cert-authority / @revoked entries added
	Invalid   int           `json:"invalid"`   // unreadable lines
	Errors    []khLineError `json:"errors"`    // the first problems, with line numbers
}

func (h *handler) importKnownHosts(c *echo.Context) error {
	var req struct {
		Text       string `json:"text"`
		Source     string `json:"source"`     // "" (text) | "system" (~/.ssh/known_hosts of the host)
		Format     string `json:"format"`     // auto | openssh | putty
		OnConflict string `json:"onConflict"` // skip (default) | replace | add
	}
	if err := httpx.BindLimit(c, &req, maxKnownHostsBody); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if err := h.requireKnownHostsAdmin(u); err != nil {
		return err
	}
	text := req.Text
	if req.Source == "system" {
		home, err := os.UserHomeDir()
		if err != nil {
			return httpx.BadRequest("the home directory of the Termstead host is unknown")
		}
		b, err := readLimited(filepath.Join(home, ".ssh", "known_hosts"), maxKnownHostsBody)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return httpx.NotFound("~/.ssh/known_hosts does not exist on the Termstead host")
			}
			return httpx.BadRequest("cannot read ~/.ssh/known_hosts: " + err.Error())
		}
		text = string(b)
	} else if req.Source != "" {
		return httpx.BadRequest("unknown source")
	}
	onConflict := req.OnConflict
	switch onConflict {
	case "":
		onConflict = "skip"
	case "skip", "replace", "add":
	default:
		return httpx.BadRequest("onConflict must be skip, replace or add")
	}
	format := req.Format
	if format == "" || format == "auto" {
		format = "openssh"
		if looksLikePuTTYHostKeys(text) {
			format = "putty"
		}
	}
	var (
		entries []khEntry
		errs    []khLineError
	)
	switch format {
	case "openssh":
		entries, errs = parseOpenSSHKnownHosts(text)
	case "putty":
		entries, errs = parsePuTTYHostKeys(text)
	default:
		return httpx.BadRequest("format must be auto, openssh or putty")
	}
	res := knownHostsImportResult{Format: format, Invalid: len(errs), Errors: []khLineError{}}
	addErr := func(e khLineError) {
		if len(res.Errors) < 25 {
			res.Errors = append(res.Errors, e)
		}
	}
	for _, e := range errs {
		addErr(e)
	}

	all, err := h.d.Store.KnownHosts.List(ctx)
	if err != nil {
		return err
	}
	type known struct {
		kh  *model.KnownHost
		key ssh.PublicKey
	}
	byHost := map[string][]known{}
	for _, kh := range all {
		if pk, err := sshx.ParseKnownHostKey(kh.PublicKey); err == nil {
			k := strings.ToLower(kh.Host) + "\x00" + strconv.Itoa(kh.Port)
			byHost[k] = append(byHost[k], known{kh, pk})
		}
	}
	who := "imported by " + u.Username
	for _, e := range entries {
		if e.Marker != "" {
			comment := e.Comment
			if comment == "" {
				comment = who
			}
			if _, err := h.markers.add(ctx, e.Marker, strings.Join(e.Hosts, ","), e.Key, comment); err != nil {
				if errors.Is(err, errMarkerExists) {
					res.Skipped++
				} else {
					res.Invalid++
					addErr(khLineError{Line: e.Line, Error: err.Error()})
				}
				continue
			}
			res.Markers++
			continue
		}
		for _, hostField := range e.Hosts {
			host, port, kind := splitKnownHost(hostField)
			switch kind {
			case hostHashed:
				res.Hashed++
				continue
			case hostPattern:
				res.Patterns++
				continue
			case hostInvalid:
				res.Invalid++
				addErr(khLineError{Line: e.Line, Error: fmt.Sprintf("invalid host %q", hostField)})
				continue
			}
			key := host + "\x00" + strconv.Itoa(port)
			dup, conflict := false, false
			for _, k := range byHost[key] {
				if sameKey(k.key, e.Key) {
					dup = true
				} else if k.key.Type() == e.Key.Type() {
					conflict = true
				}
			}
			if dup {
				res.Skipped++
				continue
			}
			comment := e.Comment
			if comment == "" {
				comment = who
			}
			kh := &model.KnownHost{Host: host, Port: port, KeyType: e.Key.Type(), PublicKey: sshx.FormatKnownHostKey(e.Key),
				Fingerprint: ssh.FingerprintSHA256(e.Key), Comment: comment}
			switch {
			case conflict && onConflict == "skip":
				res.Conflicts++
				continue
			case conflict && onConflict == "replace":
				if err := h.d.Store.KnownHosts.Replace(ctx, kh); err != nil {
					return err
				}
				kept := byHost[key][:0]
				for _, k := range byHost[key] {
					if k.key.Type() != e.Key.Type() {
						kept = append(kept, k)
					}
				}
				byHost[key] = kept
				res.Replaced++
			default:
				if err := h.d.Store.KnownHosts.Add(ctx, kh); err != nil {
					return err
				}
				res.Added++
			}
			byHost[key] = append(byHost[key], known{kh, e.Key})
		}
	}
	source := "text"
	if req.Source == "system" {
		source = "~/.ssh/known_hosts"
	}
	h.d.Audit.Log(c, "known_hosts.import", "", map[string]any{"source": source, "format": format, "added": res.Added,
		"replaced": res.Replaced, "markers": res.Markers})
	return c.JSON(http.StatusOK, res)
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("the file is too large")
	}
	return b, nil
}

// exportKnownHosts renders every trusted key and marker in OpenSSH known_hosts format (?hashed=1 hashes host names).
func (h *handler) exportKnownHosts(c *echo.Context) error {
	list, err := h.d.Store.KnownHosts.List(c.Request().Context())
	if err != nil {
		return err
	}
	hashed := c.QueryParam("hashed") == "1" || c.QueryParam("hashed") == "true"
	var b strings.Builder
	fmt.Fprintf(&b, "# Termstead known hosts, exported %s\n", now().Format("2006-01-02T15:04:05Z"))
	for _, m := range h.markers.list() {
		line := "@" + m.Marker + " " + m.Hosts + " " + m.PublicKey
		if m.Comment != "" {
			line += " " + m.Comment
		}
		b.WriteString(line + "\n")
	}
	for _, kh := range list {
		name := knownHostName(kh.Host, kh.Port)
		if hashed {
			name = hashHostName(name)
		}
		pk, err := sshx.ParseKnownHostKey(kh.PublicKey)
		if err != nil {
			continue
		}
		line := name + " " + sshx.FormatKnownHostKey(pk)
		if kh.Comment != "" && !hashed {
			line += " " + cleanComment(kh.Comment)
		}
		b.WriteString(line + "\n")
	}
	hdr := c.Response().Header()
	hdr.Set(echo.HeaderContentDisposition, mime.FormatMediaType("attachment", map[string]string{"filename": "known_hosts"}))
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(b.String()))
}

// ---- markers ------------------------------------------------------------------------------------------------------

func (h *handler) listMarkers(c *echo.Context) error {
	list := h.markers.list()
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Marker != list[j].Marker {
			return list[i].Marker == markerCertAuthority
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return c.JSON(http.StatusOK, list)
}

func (h *handler) addMarker(c *echo.Context) error {
	var req struct {
		Marker    string `json:"marker"`
		Hosts     string `json:"hosts"`
		PublicKey string `json:"publicKey"`
		KeyID     string `json:"keyId"` // or: one of the caller's stored keys (e.g. a CA key made in Termstead)
		Comment   string `json:"comment"`
	}
	if err := httpx.BindLimit(c, &req, 256<<10); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if err := h.requireKnownHostsAdmin(u); err != nil {
		return err
	}
	var (
		pub     ssh.PublicKey
		comment string
		err     error
	)
	if req.KeyID != "" {
		k, err := h.ownKey(ctx, u, req.KeyID)
		if err != nil {
			return err
		}
		if pub, err = keyPublic(k); err != nil {
			return err
		}
		comment = k.Name
	} else if pub, comment, err = parsePublicKeyText(req.PublicKey); err != nil {
		return httpx.BadRequest("invalid public key: " + err.Error())
	}
	if c := cleanComment(req.Comment); c != "" {
		comment = c
	}
	marker := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.Marker)), "@")
	m, err := h.markers.add(ctx, marker, req.Hosts, pub, comment)
	if err != nil {
		if errors.Is(err, errMarkerExists) {
			return httpx.NewError(http.StatusConflict, "already_known", "an identical entry already exists")
		}
		return httpx.BadRequest(err.Error())
	}
	h.d.Audit.Log(c, "known_host.marker.add", m.Hosts, map[string]any{"marker": m.Marker, "fingerprint": m.Fingerprint})
	return c.JSON(http.StatusCreated, m)
}

func (h *handler) deleteMarker(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if err := h.requireKnownHostsAdmin(u); err != nil {
		return err
	}
	m, ok := h.markers.get(c.Param("id"))
	if !ok {
		return httpx.ErrNotFound
	}
	if err := h.markers.remove(ctx, m.ID); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return httpx.ErrNotFound
		}
		return err
	}
	h.d.Audit.Log(c, "known_host.marker.delete", m.Hosts, map[string]any{"marker": m.Marker, "fingerprint": m.Fingerprint})
	return httpx.OK(c)
}
