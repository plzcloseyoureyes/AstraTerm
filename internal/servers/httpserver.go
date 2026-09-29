package servers

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// httpService is the HTTP(S) file server (SRV-4): downloads with Range support, directory listings, optional
// browser / PUT uploads, optional basic authentication.
type httpService struct {
	m   *Manager
	in  *instance
	cfg *HTTPConfig
	fs  *rootFS
	db  *userDB
	tls *tls.Config
	srv *http.Server
	wg  sync.WaitGroup
}

type clientCtxKey struct{}

func newHTTPService(ctx context.Context, m *Manager, in *instance, cfg *HTTPConfig) (service, error) {
	s := &httpService{m: m, in: in, cfg: cfg, fs: in.root.withReadOnly(cfg.ReadOnly)}
	if cfg.RequireAuth {
		s.db = newUserDB(cfg.Users, &in.stats)
	}
	if cfg.TLS {
		cert, fp, err := m.secrets.certificate(ctx)
		if err != nil {
			return nil, fmt.Errorf("TLS certificate: %w", err)
		}
		s.tls = tlsConfig(cert)
		in.fp = fp
	}
	return s, nil
}

func (s *httpService) start() error {
	c := s.cfg
	raw, err := net.Listen("tcp", hostPort(c.BindAddress, c.Port))
	if err != nil {
		return err
	}
	s.in.addrs = []string{raw.Addr().String()}
	var ln net.Listener = &trackingListener{Listener: raw, set: s.in.clients}
	scheme := "http"
	if s.tls != nil {
		ln = tls.NewListener(ln, s.tls)
		scheme = "https"
	}
	s.in.url = serverURL(scheme, c.BindAddress, raw.Addr().(*net.TCPAddr).Port, "/")
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext:       func(net.Listener) context.Context { return s.in.ctx },
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if cl := clientOf(conn); cl != nil {
				return context.WithValue(ctx, clientCtxKey{}, cl)
			}
			return ctx
		},
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.in.failed(err)
		}
	}()
	return nil
}

func (s *httpService) stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	s.in.clients.closeAll()
	s.wg.Wait()
}

// httpStallTimeout bounds how long one read of a request body or one write of a response may stall. The server has
// no overall read / write timeout (large transfers over slow links are legitimate), so without it a client could hold
// a connection slot forever by trickling a body or by not reading a download (slowloris / slow read).
var httpStallTimeout = 60 * time.Second

// statusWriter records the status code and body size of a response and extends the write deadline per write.
type statusWriter struct {
	http.ResponseWriter
	rc     *http.ResponseController
	status int
	n      int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.rc != nil {
		_ = w.rc.SetWriteDeadline(time.Now().Add(httpStallTimeout))
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the connection.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *httpService) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(rw)
	// Refresh the write deadline for this request (a keep-alive connection keeps the previous one).
	_ = rc.SetWriteDeadline(time.Now().Add(httpStallTimeout))
	w := &statusWriter{ResponseWriter: rw, rc: rc}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &stallReader{ReadCloser: r.Body, rc: rc}
	}
	h := w.Header()
	h.Set("Server", "NexTerm")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	cl, _ := r.Context().Value(clientCtxKey{}).(*client)
	remote := r.RemoteAddr
	if cl != nil {
		cl.setActivity(r.Method + " " + r.URL.Path)
	}
	// Canonical paths only ("/a/../b", "//x" → redirect), so listings, breadcrumbs and logs show what is served.
	if p := r.URL.Path; p == "" || p[0] != '/' {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	} else if clean := canonicalPath(p); clean != p {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Bad request: non-canonical path", http.StatusBadRequest)
			return
		}
		target := (&url.URL{Path: clean}).EscapedPath()
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	user := ""
	readOnly := s.cfg.ReadOnly
	if s.db != nil {
		name, pass, ok := r.BasicAuth()
		if !ok {
			s.challenge(w)
			return
		}
		u, err := s.db.checkPassword(remoteIP(remote), name, pass)
		if err != nil {
			s.in.logf(levelWarn, remote, name, "Authentication failed: %v", err)
			if errors.Is(err, errTooManyFails) {
				http.Error(w, "Too many failed logins. Try again later.", http.StatusTooManyRequests)
				return
			}
			s.challenge(w)
			return
		}
		user = u.Username
		readOnly = readOnly || u.ReadOnly
		if cl != nil && cl.getUser() == "" {
			cl.setUser(user)
		}
	}
	rq := &httpRequest{w: w, r: r, user: user, readOnly: readOnly}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.serveGet(rq)
	case http.MethodPost:
		s.serveUpload(rq)
	case http.MethodPut:
		s.servePut(rq)
	case http.MethodOptions:
		h.Set("Allow", allowedMethods(readOnly))
		w.WriteHeader(http.StatusNoContent)
	default:
		h.Set("Allow", allowedMethods(readOnly))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// stallReader extends the connection's read deadline before every read of a request body.
type stallReader struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (s *stallReader) Read(p []byte) (int, error) {
	_ = s.rc.SetReadDeadline(time.Now().Add(httpStallTimeout))
	return s.ReadCloser.Read(p)
}

// httpRequest is one request with the caller's identity and write permission (server-wide read-only, or a
// read-only user).
type httpRequest struct {
	w        *statusWriter
	r        *http.Request
	user     string
	readOnly bool
}

// canonicalPath cleans a request path, keeping a trailing slash.
func canonicalPath(p string) string {
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean
}

func allowedMethods(readOnly bool) string {
	if readOnly {
		return "GET, HEAD, OPTIONS"
	}
	return "GET, HEAD, POST, PUT, OPTIONS"
}

func (s *httpService) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="NexTerm file server", charset="UTF-8"`)
	http.Error(w, "Authentication required", http.StatusUnauthorized)
}

// fsError maps a file system error to an HTTP status.
func fsError(w http.ResponseWriter, err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "Not found", http.StatusNotFound)
		return http.StatusNotFound
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "Forbidden", http.StatusForbidden)
		return http.StatusForbidden
	default:
		// Paths escaping the root (symlinks leading outside) and other failures look like "not found".
		http.Error(w, "Not found", http.StatusNotFound)
		return http.StatusNotFound
	}
}

func (s *httpService) serveGet(rq *httpRequest) {
	w, r, user := rq.w, rq.r, rq.user
	p := r.URL.Path
	fi, err := s.fs.Stat(p)
	if err != nil {
		if p == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		code := fsError(w, err)
		s.in.logf(levelInfo, r.RemoteAddr, user, "%s %s → %d", r.Method, p, code)
		return
	}
	if fi.IsDir() {
		if !strings.HasSuffix(p, "/") {
			target := (&url.URL{Path: p + "/"}).EscapedPath()
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		if _, list := r.URL.Query()["list"]; !list || !s.cfg.Listing {
			if idx, err := s.fs.Stat(path.Join(p, "index.html")); err == nil && idx.Mode().IsRegular() {
				s.serveFile(w, r, path.Join(p, "index.html"), idx, user)
				return
			}
		}
		if !s.cfg.Listing {
			http.Error(w, "Directory listing is disabled", http.StatusForbidden)
			s.in.logf(levelInfo, r.RemoteAddr, user, "%s %s → 403 (listing disabled)", r.Method, p)
			return
		}
		s.serveListing(rq, p)
		return
	}
	if !fi.Mode().IsRegular() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	s.serveFile(w, r, p, fi, user)
}

func (s *httpService) serveFile(w *statusWriter, r *http.Request, p string, fi os.FileInfo, user string) {
	f, err := s.fs.OpenRegular(p)
	if err != nil {
		code := fsError(w, err)
		s.in.logf(levelInfo, r.RemoteAddr, user, "%s %s → %d", r.Method, p, code)
		return
	}
	tf := &transferFile{File: f, name: p}
	defer tf.Close()
	h := w.Header()
	// User content runs in an opaque origin: scripts in shared HTML files cannot reach NexTerm (same site, other port).
	h.Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads")
	if _, dl := r.URL.Query()["download"]; dl {
		h.Set("Content-Disposition", contentDisposition(fi.Name()))
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), tf)
	n := tf.read.Load()
	if r.Method == http.MethodGet && n > 0 {
		s.in.stats.transfers.Add(1)
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	s.in.logf(levelInfo, r.RemoteAddr, user, "%s %s → %d (%s)", r.Method, p, status, humanBytes(n))
}

// contentDisposition builds an attachment header with an RFC 5987 file name.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

// ---- listing ------------------------------------------------------------------------------------------------------

type listingEntry struct {
	Name     string
	Href     string
	Dir      bool
	Link     bool
	Size     string
	Modified string
}

type crumb struct {
	Name string
	Href string
}

type listingPage struct {
	Path     string
	Crumbs   []crumb
	Parent   string
	Entries  []listingEntry
	Upload   bool
	Message  string
	Error    string
	Sort     string
	Desc     bool
	Count    int
	MaxMB    int
	SortHref map[string]string
}

var listingTmpl = template.Must(template.New("listing").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Index of {{.Path}}</title>
<style>
:root{color-scheme:light dark;--fg:#1f2328;--muted:#656d76;--bg:#fff;--line:#d0d7de;--hover:#f6f8fa;--accent:#0969da;--ok:#1a7f37;--err:#cf222e}
@media (prefers-color-scheme:dark){:root{--fg:#e6edf3;--muted:#8d96a0;--bg:#0d1117;--line:#30363d;--hover:#161b22;--accent:#4493f8;--ok:#3fb950;--err:#f85149}}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif;color:var(--fg);background:var(--bg)}
main{max-width:1100px;margin:0 auto;padding:20px 16px 40px}h1{font-size:18px;font-weight:600;margin:0 0 14px;word-break:break-all}
h1 a{color:var(--accent);text-decoration:none}h1 a:hover{text-decoration:underline}.sep{color:var(--muted);margin:0 4px}
table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:6px 10px;border-bottom:1px solid var(--line);white-space:nowrap}
th{font-weight:600;font-size:12px;color:var(--muted);text-transform:uppercase;letter-spacing:.03em}th a{color:inherit;text-decoration:none}
td.name{white-space:normal;word-break:break-all;width:100%}td.num{text-align:right;font-variant-numeric:tabular-nums;color:var(--muted)}
tr:hover td{background:var(--hover)}a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
.icon{display:inline-block;width:1.4em;color:var(--muted)}.dl{margin-left:8px;font-size:12px;color:var(--muted)}
.panel{display:flex;flex-wrap:wrap;gap:12px;align-items:center;margin:0 0 16px;padding:10px 12px;border:1px solid var(--line);border-radius:8px}
.panel form{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:0}
input[type=text]{padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg)}
button{padding:4px 12px;border:1px solid var(--line);border-radius:6px;background:var(--hover);color:var(--fg);cursor:pointer}
.msg{margin:0 0 12px;color:var(--ok)}.err{margin:0 0 12px;color:var(--err)}footer{margin-top:18px;color:var(--muted);font-size:12px}
</style></head><body><main>
<h1>{{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="{{$c.Href}}">{{$c.Name}}</a>{{end}}</h1>
{{if .Message}}<p class="msg">{{.Message}}</p>{{end}}{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Upload}}<div class="panel">
<form method="post" enctype="multipart/form-data"><input type="hidden" name="action" value="upload"><input type="file" name="file" multiple required><button type="submit">Upload</button>{{if .MaxMB}}<span class="dl">max {{.MaxMB}} MB</span>{{end}}</form>
<form method="post" enctype="multipart/form-data"><input type="hidden" name="action" value="mkdir"><input type="text" name="mkdir" placeholder="New folder" maxlength="255" required><button type="submit">Create</button></form>
</div>{{end}}
<table><thead><tr><th><a href="{{index .SortHref "name"}}">Name</a></th><th class="num"><a href="{{index .SortHref "size"}}">Size</a></th><th><a href="{{index .SortHref "time"}}">Modified</a></th></tr></thead><tbody>
{{if .Parent}}<tr><td class="name"><span class="icon">↩</span><a href="{{.Parent}}">..</a></td><td class="num"></td><td></td></tr>{{end}}
{{range .Entries}}<tr><td class="name"><span class="icon">{{if .Dir}}📁{{else if .Link}}🔗{{else}}📄{{end}}</span><a href="{{.Href}}">{{.Name}}{{if .Dir}}/{{end}}</a>{{if not .Dir}}<a class="dl" href="{{.Href}}?download" title="Download">⤓</a>{{end}}</td><td class="num">{{.Size}}</td><td>{{.Modified}}</td></tr>
{{else}}<tr><td class="name" colspan="3" style="color:var(--muted)">This folder is empty.</td></tr>{{end}}
</tbody></table>
<footer>{{.Count}} item(s) · NexTerm HTTP file server</footer>
</main></body></html>`))

func (s *httpService) serveListing(rq *httpRequest, p string) {
	w, r, user := rq.w, rq.r, rq.user
	list, err := s.fs.ReadDir(p)
	if err != nil {
		code := fsError(w, err)
		s.in.logf(levelInfo, r.RemoteAddr, user, "%s %s → %d", r.Method, p, code)
		return
	}
	q := r.URL.Query()
	sortBy := q.Get("sort")
	if sortBy != "size" && sortBy != "time" {
		sortBy = "name"
	}
	desc := q.Get("order") == "desc"
	page := listingPage{Path: p, Upload: !rq.readOnly && s.cfg.Upload, Sort: sortBy, Desc: desc, Count: len(list),
		MaxMB: s.cfg.MaxUploadMB, Message: truncate(q.Get("msg"), 200), Error: truncate(q.Get("err"), 300),
		SortHref: map[string]string{}}
	for _, k := range []string{"name", "size", "time"} {
		order := "asc"
		if k == sortBy && !desc {
			order = "desc"
		}
		page.SortHref[k] = "?sort=" + k + "&order=" + order
	}
	// Breadcrumbs.
	page.Crumbs = []crumb{{Name: "/", Href: "/"}}
	acc := "/"
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg == "" {
			continue
		}
		acc += url.PathEscape(seg) + "/"
		page.Crumbs = append(page.Crumbs, crumb{Name: seg, Href: acc})
	}
	if p != "/" {
		page.Parent = "../"
	}
	entries := make([]listingEntry, 0, len(list))
	infos := make(map[string]os.FileInfo, len(list))
	for _, fi := range list {
		name := fi.Name()
		isLink := fi.Mode()&os.ModeSymlink != 0
		isDir := fi.IsDir()
		if isLink {
			if st, err := s.fs.Stat(path.Join(p, name)); err == nil {
				isDir = st.IsDir()
				fi = st
			}
		}
		infos[name] = fi
		e := listingEntry{Name: name, Href: "./" + url.PathEscape(name), Dir: isDir, Link: isLink,
			Modified: fi.ModTime().Local().Format("2006-01-02 15:04")}
		if isDir {
			e.Href += "/"
			e.Size = "—"
		} else {
			e.Size = humanBytes(fi.Size())
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		var less bool
		switch sortBy {
		case "size":
			less = infos[a.Name].Size() < infos[b.Name].Size()
		case "time":
			less = infos[a.Name].ModTime().Before(infos[b.Name].ModTime())
		default:
			less = strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		if desc {
			return !less
		}
		return less
	})
	page.Entries = entries
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'self'")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := listingTmpl.Execute(w, page); err != nil {
		s.m.log.Debug("servers: listing render failed", "err", err)
	}
	s.in.logf(levelInfo, r.RemoteAddr, user, "%s %s → 200 (listing, %d items)", r.Method, p, len(list))
}

// ---- uploads ------------------------------------------------------------------------------------------------------

// sameOrigin rejects cross-site browser form posts (CSRF against a logged-in user's file server).
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return origin == ""
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *httpService) uploadLimit() int64 {
	if s.cfg.MaxUploadMB <= 0 {
		return -1
	}
	return int64(s.cfg.MaxUploadMB) << 20
}

// validName checks a single path component supplied by a client.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeAtomic stores src as dir/name through a temporary file renamed into place. limit < 0 = unlimited.
func (s *httpService) writeAtomic(dir, name string, src io.Reader, limit int64) (int64, bool, error) {
	final := path.Join(dir, name)
	existed := false
	if st, err := s.fs.Stat(final); err == nil {
		if st.IsDir() {
			return 0, false, &os.PathError{Op: "write", Path: final, Err: errIsDir}
		}
		existed = true
	}
	tmp := path.Join(dir, "."+name+".nexterm-part-"+randomSuffix())
	if len(path.Base(tmp)) > 255 {
		tmp = path.Join(dir, ".nexterm-part-"+randomSuffix())
	}
	f, err := s.fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, existed, err
	}
	var r io.Reader = src
	if limit >= 0 {
		r = io.LimitReader(src, limit+1)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && limit >= 0 && n > limit {
		err = errTooLarge
	}
	if err == nil {
		err = s.fs.Rename(tmp, final)
	}
	if err != nil {
		_ = s.fs.Remove(tmp)
		return n, existed, err
	}
	return n, existed, nil
}

var (
	errIsDir    = errors.New("is a directory")
	errTooLarge = errors.New("the upload exceeds the size limit")
)

func (s *httpService) serveUpload(rq *httpRequest) {
	w, r, user := rq.w, rq.r, rq.user
	dir := r.URL.Path
	if rq.readOnly || !s.cfg.Upload {
		s.in.logf(levelWarn, r.RemoteAddr, user, "Upload to %s refused (uploads are disabled)", dir)
		http.Error(w, "Uploads are disabled", http.StatusForbidden)
		return
	}
	if !sameOrigin(r) {
		s.in.logf(levelWarn, r.RemoteAddr, user, "Cross-site upload to %s rejected", dir)
		http.Error(w, "Cross-site upload rejected", http.StatusForbidden)
		return
	}
	st, err := s.fs.Stat(dir)
	if err != nil || !st.IsDir() {
		http.Error(w, "Not a folder", http.StatusNotFound)
		return
	}
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	limit := s.uploadLimit()
	if limit >= 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected a multipart/form-data upload", http.StatusBadRequest)
		return
	}
	var saved, created []string
	var failure string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			failure = "upload interrupted"
			break
		}
		if msg := s.handlePart(part, dir, limit, r.RemoteAddr, user, &saved, &created); msg != "" {
			failure = msg
			_ = part.Close()
			break
		}
		_ = part.Close()
	}
	q := url.Values{}
	switch {
	case failure != "":
		q.Set("err", failure)
	case len(saved) > 0 || len(created) > 0:
		var parts []string
		if len(saved) > 0 {
			parts = append(parts, fmt.Sprintf("Uploaded %d file(s)", len(saved)))
		}
		if len(created) > 0 {
			parts = append(parts, "created "+strings.Join(created, ", "))
		}
		q.Set("msg", strings.Join(parts, "; "))
	}
	target := (&url.URL{Path: dir}).EscapedPath()
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handlePart stores one multipart part (a file or the "mkdir" field); it returns a user-facing error message.
func (s *httpService) handlePart(part *multipart.Part, dir string, limit int64, remote, user string, saved, created *[]string) string {
	switch {
	case part.FormName() == "mkdir":
		b, _ := io.ReadAll(io.LimitReader(part, 512))
		name := strings.TrimSpace(string(b))
		if !validName(name) {
			return "invalid folder name"
		}
		if err := s.fs.Mkdir(path.Join(dir, name), 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return "a file or folder with that name already exists"
			}
			return "cannot create the folder"
		}
		*created = append(*created, name)
		s.in.logf(levelInfo, remote, user, "Created folder %s", path.Join(dir, name))
	case part.FileName() != "":
		name := path.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
		if !validName(name) {
			return "invalid file name"
		}
		n, _, err := s.writeAtomic(dir, name, part, limit)
		if err != nil {
			s.in.logf(levelWarn, remote, user, "Upload of %s failed: %v", path.Join(dir, name), rootCause(err))
			if errors.Is(err, errTooLarge) {
				return fmt.Sprintf("%s exceeds the %d MB limit", name, s.cfg.MaxUploadMB)
			}
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return fmt.Sprintf("the upload exceeds the %d MB limit", s.cfg.MaxUploadMB)
			}
			return "cannot store " + name
		}
		*saved = append(*saved, name)
		s.in.stats.transfers.Add(1)
		s.in.logf(levelInfo, remote, user, "Uploaded %s (%s)", path.Join(dir, name), humanBytes(n))
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(part, 4096))
	}
	return ""
}

func (s *httpService) servePut(rq *httpRequest) {
	w, r, user := rq.w, rq.r, rq.user
	p := r.URL.Path
	if rq.readOnly {
		s.in.logf(levelWarn, r.RemoteAddr, user, "PUT %s refused (read-only)", p)
		http.Error(w, "Read-only access", http.StatusForbidden)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "Cross-site upload rejected", http.StatusForbidden)
		return
	}
	dir, name := path.Split(path.Clean("/" + p))
	if strings.HasSuffix(p, "/") || !validName(name) {
		http.Error(w, "PUT needs a file path", http.StatusBadRequest)
		return
	}
	if st, err := s.fs.Stat(dir); err != nil || !st.IsDir() {
		http.Error(w, "The parent folder does not exist", http.StatusConflict)
		return
	}
	limit := s.uploadLimit()
	if limit >= 0 && r.ContentLength > limit {
		http.Error(w, "Upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	n, existed, err := s.writeAtomic(dir, name, r.Body, limit)
	if err != nil {
		s.in.logf(levelWarn, r.RemoteAddr, user, "PUT %s failed: %v", p, rootCause(err))
		switch {
		case errors.Is(err, errTooLarge):
			http.Error(w, "Upload too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, errIsDir):
			http.Error(w, "A folder with that name exists", http.StatusConflict)
		case errors.Is(err, fs.ErrPermission):
			http.Error(w, "Forbidden", http.StatusForbidden)
		default:
			http.Error(w, "Cannot store the file", http.StatusInternalServerError)
		}
		return
	}
	s.in.stats.transfers.Add(1)
	s.in.logf(levelInfo, r.RemoteAddr, user, "PUT %s (%s)", p, humanBytes(n))
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Location", (&url.URL{Path: path.Join(dir, name)}).EscapedPath())
	w.WriteHeader(http.StatusCreated)
}

// humanBytes formats a byte count (1.5 MB).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
