package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

// notBuiltPage is served when the embedded frontend has no index.html (backend built without `make web`).
const notBuiltPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>AstraTerm — frontend not built</title>
<style>body{font:15px/1.5 system-ui,sans-serif;background:#0f1115;color:#e6e6e6;display:grid;place-items:center;height:100vh;margin:0}
main{max-width:34rem;padding:2rem}code{background:#1d2129;padding:.1rem .35rem;border-radius:4px}</style></head>
<body><main><h1>AstraTerm backend is running</h1>
<p>The web UI has not been built into this binary. Run <code>make web</code> (or <code>make build</code>) and restart,
or start the Vite dev server with <code>make dev-web</code> and open <code>http://localhost:5173</code>.</p>
<p>The REST API is available under <code>/api</code>.</p></main></body></html>`

// spaHandler serves the embedded single-page app (the router's fallback for GET/HEAD requests outside /api and /ws
// that match no route): real files when they exist (immutable caching for /assets/*), a plain 404 for missing
// assets, and index.html for every other path (client-side routing, never cached).
//
// `make web` precompresses the large assets (internal/webui/precompress): for those only name.gz is embedded. They
// are sent as-is with Content-Encoding: gzip, or decompressed on the fly for the rare client without gzip support.
func spaHandler(fsys fs.FS) echo.HandlerFunc {
	etags := &etagCache{fsys: fsys}
	return func(c *echo.Context) error {
		name := strings.TrimPrefix(path.Clean("/"+c.Request().URL.Path), "/")
		if name != "" && name != "index.html" && fs.ValidPath(name) {
			if exists(fsys, name) || exists(fsys, name+".gz") {
				if strings.HasPrefix(name, "assets/") {
					c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=31536000, immutable")
				} else {
					c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
				}
				serveFile(c, fsys, etags, name)
				return nil
			}
			if strings.HasPrefix(name, "assets/") {
				http.Error(c.Response(), "404 page not found", http.StatusNotFound)
				return nil
			}
		}
		serveIndex(c, fsys)
		return nil
	}
}

func exists(fsys fs.FS, name string) bool {
	fi, err := fs.Stat(fsys, name)
	return err == nil && !fi.IsDir()
}

func serveIndex(c *echo.Context, fsys fs.FS) {
	h := c.Response().Header()
	h.Set(echo.HeaderCacheControl, "no-store")
	b, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		b = []byte(notBuiltPage)
	}
	h.Set(echo.HeaderContentType, "text/html; charset=utf-8")
	http.ServeContent(c.Response(), c.Request(), "index.html", time.Time{}, strings.NewReader(string(b)))
}

// uiTypes are the content types of the UI's own assets. They are fixed rather than looked up in the operating system:
// on Windows mime.TypeByExtension reads the registry, which may map .js to text/plain — and browsers refuse module
// scripts of that type, so the UI would not load.
var uiTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".wasm":  "application/wasm",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ttf":   "font/ttf",
	".txt":   "text/plain; charset=utf-8",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := uiTypes[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext)
}

// serveFile serves name: a precompressed .br / .gz variant when the client accepts it, else the plain file, else
// the .gz variant decompressed (precompressed-only assets). Every response carries a strong ETag of what it sends.
func serveFile(c *echo.Context, fsys fs.FS, etags *etagCache, name string) {
	w, r := c.Response(), c.Request()
	w.Header().Add("Vary", "Accept-Encoding")
	ct := contentType(name)
	ae := r.Header.Get("Accept-Encoding")
	for _, enc := range []struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if !acceptsEncoding(ae, enc.token) {
			continue
		}
		if serveStored(w, r, fsys, etags, name+enc.ext, ct, enc.token) {
			return
		}
	}
	if serveStored(w, r, fsys, etags, name, ct, "") {
		return
	}
	serveGunzipped(w, r, fsys, etags, name, ct)
}

// serveStored sends the embedded file stored as-is (with Content-Encoding when encoding is set); false when missing.
func serveStored(w http.ResponseWriter, r *http.Request, fsys fs.FS, etags *etagCache, stored, ct, encoding string) bool {
	f, err := fsys.Open(stored)
	if err != nil {
		return false
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	h := w.Header()
	if ct == "" && encoding != "" {
		ct = "application/octet-stream" // never let ServeContent sniff the compressed bytes
	}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
		r.Header.Del("Range") // ranges over compressed bytes are meaningless for clients
	}
	if tag := etags.get(stored); tag != "" {
		h.Set("ETag", `"`+tag+`"`)
	}
	http.ServeContent(w, r, path.Base(stored), time.Time{}, rs)
	return true
}

// serveGunzipped streams name.gz decompressed, for clients that do not accept gzip (no Range support).
func serveGunzipped(w http.ResponseWriter, r *http.Request, fsys fs.FS, etags *etagCache, name, ct string) {
	stored := name + ".gz"
	b, err := fs.ReadFile(fsys, stored)
	if err != nil {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		http.Error(w, "500 internal server error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	etag := `"` + etags.get(stored) + `-identity"`
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagListMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if len(b) >= 4 { // ISIZE trailer: uncompressed length mod 2^32 (assets are far smaller)
		h.Set("Content-Length", strconv.FormatUint(uint64(binary.LittleEndian.Uint32(b[len(b)-4:])), 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, zr)
	}
}

func etagListMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimPrefix(strings.TrimSpace(part), "W/")
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

// etagCache hashes embedded files on first use (the embedded FS never changes while the process runs).
type etagCache struct {
	fsys fs.FS
	m    sync.Map // stored name → tag
}

func (e *etagCache) get(stored string) string {
	if v, ok := e.m.Load(stored); ok {
		return v.(string)
	}
	f, err := e.fsys.Open(stored)
	if err != nil {
		return ""
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return ""
	}
	tag := hex.EncodeToString(hash.Sum(nil)[:16])
	e.m.Store(stored, tag)
	return tag
}

func acceptsEncoding(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), token) {
			continue
		}
		for _, f := range fields[1:] {
			if q := strings.TrimSpace(f); q == "q=0" || q == "q=0.0" || q == "q=0.00" || q == "q=0.000" {
				return false
			}
		}
		return true
	}
	return false
}
