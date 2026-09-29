package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Downloads (FILE-7, FILE-9, FILE-12): single files with Range / If-Range / ETag and RFC 6266 file names, inline
// previews with a safe Content-Type, folders and multi-selections as streamed zip (or tar.gz) archives.

func (h *handler) download(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	q := c.QueryParams()
	raw := append([]string(nil), q["paths"]...)
	if p := q.Get("path"); p != "" {
		raw = append([]string{p}, raw...)
	}
	if len(raw) == 0 {
		return httpx.BadRequest("path is required")
	}
	seen := map[string]bool{}
	var paths []string
	for _, r := range raw {
		p, err := requirePath(hd, r, "path")
		if err != nil {
			return err
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	ctx := c.Request().Context()
	asArchive := parseBool(q.Get("zip")) || len(paths) > 1 || q.Get("format") != ""
	if !asArchive {
		e, err := hd.FS.Stat(ctx, paths[0])
		if err != nil {
			return fsError(err, paths[0])
		}
		switch e.Type {
		case model.FileTypeDir:
			asArchive = true
		case model.FileTypeOther:
			return readable(e, paths[0])
		default:
			return h.serveFile(c, hd, paths[0], e, parseBool(q.Get("inline")))
		}
	}
	return h.serveArchive(c, hd, paths, q.Get("format"), q.Get("name"))
}

// contentDisposition renders an RFC 6266 header with an ASCII fallback and a UTF-8 filename*.
func contentDisposition(kind, name string) string {
	var ascii strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			ascii.WriteByte('_')
		case r >= 0x20 && r < 0x7f:
			ascii.WriteRune(r)
		default:
			ascii.WriteByte('_')
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii.String(), rfc5987(name))
}

// rfc5987 percent-encodes a value for ext-value (RFC 5987 / 8187): everything but attr-char is %XX-escaped, so no
// quote, separator or control character of a remote file name can end or extend the header.
func rfc5987(s string) string {
	const attrChar = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(attrChar, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// previewType decides the Content-Type of an inline preview and whether the response must be sandboxed. Active
// content (HTML, XML, SVG, scripts) is never served in a form a browser would execute.
func previewType(name string, head []byte) (ctype string, sandbox, inline bool) {
	ext := strings.ToLower(path.Ext(name))
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = http.DetectContentType(head)
	}
	base, _, _ := mime.ParseMediaType(ct)
	switch {
	case base == "image/svg+xml":
		return "image/svg+xml", true, true
	case strings.HasPrefix(base, "image/"), strings.HasPrefix(base, "audio/"), strings.HasPrefix(base, "video/"):
		return base, true, true
	case base == "application/pdf":
		return base, false, true
	case strings.HasPrefix(base, "text/"), base == "application/json", base == "application/xml",
		base == "application/javascript", base == "application/x-sh", base == "application/xhtml+xml",
		base == "application/x-yaml", base == "application/toml":
		return "text/plain; charset=utf-8", true, true
	}
	if utf8.Valid(head) && !containsNUL(head) && len(head) > 0 {
		return "text/plain; charset=utf-8", true, true
	}
	return "application/octet-stream", true, false
}

// parseRange parses a single "bytes=" range against size. ok=false means "serve everything"; unsatisfiable reports a
// range that starts beyond the end.
func parseRange(h string, size int64) (start, end int64, ok, unsatisfiable bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	a, b, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false, false
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "" && b == "":
		return 0, 0, false, false
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, n == 0 && err == nil
		}
		if n > size {
			n = size
		}
		if size == 0 {
			return 0, 0, false, true
		}
		return size - n, size - 1, true, false
	default:
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 {
			return 0, 0, false, false
		}
		if s >= size {
			return 0, 0, false, true
		}
		e := size - 1
		if b != "" {
			v, err := strconv.ParseInt(b, 10, 64)
			if err != nil || v < s {
				return 0, 0, false, false
			}
			e = min(v, size-1)
		}
		return s, e, true, false
	}
}

func (h *handler) serveFile(c *echo.Context, hd *Handle, p string, e *Entry, inline bool) error {
	ctx := c.Request().Context()
	req, w := c.Request(), c.Response()
	size := e.Size
	etag := fmt.Sprintf(`"%x-%x"`, size, e.Mtime.Unix())
	name := baseName(p)

	start, end, ranged, unsat := parseRange(req.Header.Get("Range"), size)
	if ir := req.Header.Get("If-Range"); (ranged || unsat) && ir != "" && ir != etag {
		// A validator that does not match means "send the whole (changed) file" (RFC 9110 §13.1.5).
		if t, err := http.ParseTime(ir); err != nil || !e.Mtime.Truncate(time.Second).Equal(t) {
			ranged, unsat = false, false
		}
	}
	hdr := w.Header()
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("ETag", etag)
	if !e.Mtime.IsZero() {
		hdr.Set("Last-Modified", e.Mtime.UTC().Format(http.TimeFormat))
	}
	hdr.Set("Cache-Control", "private, no-store")
	if unsat {
		hdr.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return httpx.NewError(http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "requested range not satisfiable")
	}
	if !ranged {
		start, end = 0, size-1
	}
	r, err := hd.FS.Open(ctx, p, start)
	if err != nil && ranged && errors.Is(err, ErrNotSupported) {
		// The driver cannot read from an offset: answer 200 with the whole file (allowed for Range requests).
		ranged, start, end = false, 0, size-1
		r, err = hd.FS.Open(ctx, p, 0)
	}
	if err != nil {
		return fsError(err, p)
	}
	defer r.Close()

	// Sniff the first bytes for the preview type; keep them for the body.
	var head []byte
	br := io.Reader(r)
	if inline {
		buf := make([]byte, 512)
		k, _ := io.ReadFull(r, buf)
		head = buf[:k]
		br = io.MultiReader(strings.NewReader(string(head)), r)
	}
	ctype, sandbox := "application/octet-stream", true
	disp := "attachment"
	if inline {
		var ok bool
		ctype, sandbox, ok = previewType(name, head)
		if ok {
			disp = "inline"
		}
	}
	hdr.Set("Content-Type", ctype)
	hdr.Set("Content-Disposition", contentDisposition(disp, name))
	switch {
	case sandbox:
		hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; "+
			"style-src 'unsafe-inline'; frame-ancestors 'self'")
	case ctype == "application/pdf":
		// The browser's PDF viewer is a plugin that object-src 'none' / sandbox would block; its scripts run in the
		// viewer, not in this origin. Only framing stays restricted.
		hdr.Set("Content-Security-Policy", "frame-ancestors 'self'")
	}
	length := end - start + 1
	if size == 0 {
		length = 0
	}
	hdr.Set("Content-Length", strconv.FormatInt(length, 10))
	status := http.StatusOK
	if ranged {
		status = http.StatusPartialContent
		hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.WriteHeader(status)
	if req.Method == http.MethodHead || length == 0 {
		return nil
	}
	var sum io.Writer = io.Discard
	var hasher = sha256.New()
	if !ranged {
		sum = hasher
	}
	n, err := CopyStream(ctx, io.MultiWriter(writerOnly{w}, sum), br, length)
	if !ranged {
		details := map[string]any{"fs": hd.ID, "host": hd.Host, "size": n, "inline": disp == "inline", "complete": err == nil && n == length}
		if err == nil && n == length {
			details["sha256"] = hex.EncodeToString(hasher.Sum(nil))
		}
		h.audit(c, "fs.download", p, details)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		h.reg.log.Debug("download aborted", "path", p, "err", err)
		panic(http.ErrAbortHandler) // make the client see a failed transfer instead of a short file
	}
	return nil
}

// writerOnly hides io.ReaderFrom so CopyStream drives the copy with its large buffer.
type writerOnly struct{ io.Writer }

func (h *handler) serveArchive(c *echo.Context, hd *Handle, paths []string, format, name string) error {
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "", "zip":
		format = "zip"
	case "tgz", "tar.gz", "targz":
		format = "tar.gz"
	case "tar":
	default:
		return httpx.BadRequest("format must be zip, tar.gz or tar")
	}
	ctx := c.Request().Context()
	for _, p := range paths {
		if _, err := hd.FS.Lstat(ctx, p); err != nil {
			return fsError(err, p)
		}
	}
	ext := map[string]string{"zip": ".zip", "tar.gz": ".tar.gz", "tar": ".tar"}[format]
	name = strings.TrimSpace(name)
	if name == "" {
		name = baseName(paths[0])
		if len(paths) > 1 {
			name = baseName(parentDir(paths[0]))
		}
		if name == "/" || name == "" {
			name = "download"
		}
	}
	if !strings.HasSuffix(strings.ToLower(name), ext) {
		name += ext
	}
	w := c.Response()
	ctype := map[string]string{"zip": "application/zip", "tar.gz": "application/gzip", "tar": "application/x-tar"}[format]
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
	w.Header().Set("Cache-Control", "private, no-store")
	if c.Request().Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	cw := &countingWriter{w: w}
	// Fast path: tar streams straight from the server (one exec) when the driver can run commands.
	if format != "zip" {
		if x, ok := hd.FS.(Execer); ok {
			if err := execTarStream(ctx, x, paths, format, cw); err == nil {
				h.audit(c, "fs.download", paths[0], map[string]any{"fs": hd.ID, "host": hd.Host, "paths": paths, "archive": format,
					"bytes": cw.n})
				return nil
			} else if cw.n > 0 {
				panic(http.ErrAbortHandler)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	st, err := writeArchive(ctx, cw, hd.FS, paths, format, "")
	h.audit(c, "fs.download", paths[0], map[string]any{"fs": hd.ID, "host": hd.Host, "paths": paths, "archive": format,
		"files": st.Files, "bytes": cw.n, "complete": err == nil})
	if err != nil {
		h.reg.log.Debug("archive download aborted", "err", err)
		panic(http.ErrAbortHandler)
	}
	return nil
}

type countingWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// execTarStream streams `tar -cz` output of paths into w (headers are written on the first byte).
func execTarStream(ctx context.Context, x Execer, paths []string, format string, w *countingWriter) error {
	parent, rel := commonParent(paths)
	q := make([]string, len(rel))
	for i, r := range rel {
		q[i] = shq(r)
	}
	flags := "-cf"
	if format == "tar.gz" {
		flags = "-czf"
	}
	cmd := "cd -- " + shq(parent) + " && tar " + flags + " - " + strings.Join(q, " ")
	lw := &lazyHeaderWriter{w: w}
	stderr, code, err := x.Exec(ctx, cmd, nil, lw)
	if err != nil {
		return err
	}
	if code != 0 && code != 1 { // GNU tar exits 1 when files changed while reading
		return shellError(stderr, code, "tar")
	}
	if w.n == 0 {
		w.w.WriteHeader(http.StatusOK)
	}
	return nil
}

// lazyHeaderWriter writes the 200 status just before the first body byte, so a command that fails immediately can
// still fall back to another strategy.
type lazyHeaderWriter struct {
	w       *countingWriter
	started bool
}

func (l *lazyHeaderWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.w.WriteHeader(http.StatusOK)
	}
	return l.w.Write(p)
}
