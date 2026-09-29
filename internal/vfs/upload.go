package vfs

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Resumable uploads (FILE-6): PUT /api/fs/{id}/upload?path=<target>&offset=<n>[&final=1][&mtime=][&total=] streams
// the raw body into "<target>.astraterm-part" at offset; final=1 atomically renames the part onto the target. The part
// size is the resume point (GET/HEAD upload?path=). S3 uses native multipart uploads instead of part files, WebDAV
// (no partial writes) stages chunks locally until the final one.

// uploadState remembers the running SHA-256 of an upload so the audit entry can carry the file hash (REC-6).
type uploadState struct {
	offset int64
	hash   []byte // marshaled sha256 state
	used   time.Time
}

const maxTrackedUploads = 256

func (hd *Handle) hashFor(target string, offset int64) hash.Hash {
	h := sha256.New()
	if offset == 0 {
		return h
	}
	hd.mu.Lock()
	st := hd.uploads[target]
	hd.mu.Unlock()
	if st == nil || st.offset != offset || st.hash == nil {
		return nil
	}
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(st.hash); err != nil {
		return nil
	}
	return h
}

func (hd *Handle) saveHash(target string, h hash.Hash, offset int64) {
	hd.mu.Lock()
	defer hd.mu.Unlock()
	if hd.uploads == nil {
		hd.uploads = map[string]*uploadState{}
	}
	if h == nil {
		delete(hd.uploads, target)
		return
	}
	b, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		delete(hd.uploads, target)
		return
	}
	if len(hd.uploads) >= maxTrackedUploads {
		var oldK string
		var oldT time.Time
		for k, v := range hd.uploads {
			if oldK == "" || v.used.Before(oldT) {
				oldK, oldT = k, v.used
			}
		}
		delete(hd.uploads, oldK)
	}
	hd.uploads[target] = &uploadState{offset: offset, hash: b, used: time.Now()}
}

func (hd *Handle) dropHash(target string) {
	hd.mu.Lock()
	delete(hd.uploads, target)
	hd.mu.Unlock()
}

// cleanupUploads removes locally staged upload chunks of the handle.
func (hd *Handle) cleanupUploads() {
	if hd.stagingDir != "" {
		_ = os.RemoveAll(hd.stagingDir)
	}
}

// parseMtime accepts unix seconds or milliseconds (JavaScript File.lastModified).
func parseMtime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
		return time.Time{}, false
	}
	if f > 1e11 { // milliseconds
		return time.UnixMilli(int64(f)).UTC(), true
	}
	return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC(), true
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// offsetMismatch answers 409 with the stored size (the client resumes from there).
func offsetMismatch(c *echo.Context, size int64) error {
	c.Response().Header().Set("Upload-Offset", strconv.FormatInt(size, 10))
	return c.JSON(http.StatusConflict, map[string]any{"error": fmt.Sprintf("upload offset mismatch: %d bytes are stored", size),
		"code": codeOffsetMismatch, "size": size, "offset": size})
}

type uploadResult struct {
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Final  bool   `json:"final"`
	Entry  *Entry `json:"entry,omitempty"`
}

func (h *handler) uploadStatus(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	target, err := requirePath(hd, c.QueryParam("path"), "path")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var size int64
	exists := false
	switch {
	case isNative(hd.FS):
		size, exists = hd.FS.(NativeUploader).UploadOffset(ctx, target)
	case !hd.Capabilities.Resume || isStaged(hd.FS):
		if fi, err := os.Stat(hd.stagePath(target)); err == nil {
			size, exists = fi.Size(), true
		}
	default:
		if e, err := hd.FS.Stat(ctx, target+partSuffix); err == nil {
			size, exists = e.Size, true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fsError(err, target)
		}
	}
	c.Response().Header().Set("Upload-Offset", strconv.FormatInt(size, 10))
	return c.JSON(http.StatusOK, map[string]any{"size": size, "offset": size, "exists": exists})
}

func isNative(f FS) bool {
	_, ok := f.(NativeUploader)
	return ok
}

// isStaged reports drivers without partial writes (their chunks are staged locally).
func isStaged(f FS) bool {
	_, ok := f.(*webdavFS)
	return ok
}

// stagePath is the local staging file of an upload target.
func (hd *Handle) stagePath(target string) string {
	sum := sha1.Sum([]byte(target))
	return filepath.Join(hd.stagingDir, hex.EncodeToString(sum[:])+".part")
}

func (h *handler) upload(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	q := c.QueryParams()
	target, err := requirePath(hd, q.Get("path"), "path")
	if err != nil {
		return err
	}
	if target == "/" || strings.HasSuffix(target, partSuffix) {
		return httpx.BadRequest("invalid upload target")
	}
	var offset int64
	if v := q.Get("offset"); v != "" {
		if offset, err = strconv.ParseInt(v, 10, 64); err != nil || offset < 0 {
			return httpx.BadRequest("invalid offset")
		}
	}
	final := parseBool(q.Get("final"))
	total := int64(-1)
	if v := q.Get("total"); v != "" {
		if total, err = strconv.ParseInt(v, 10, 64); err != nil || total < 0 {
			return httpx.BadRequest("invalid total")
		}
	}
	mtime, hasMtime := parseMtime(q.Get("mtime"))
	req := c.Request()
	n := req.ContentLength
	if total >= 0 && n >= 0 && offset+n == total {
		final = true
	}
	ctx := req.Context()
	if final {
		if te, err := hd.FS.Lstat(ctx, target); err == nil && te.Type == model.FileTypeDir {
			return httpx.NewError(http.StatusConflict, codeExists, "a folder with this name exists: "+target)
		}
	}
	hasher := hd.hashFor(target, offset)
	var body io.Reader = req.Body
	if hasher != nil {
		body = io.TeeReader(body, hasher)
	}

	var size int64
	switch {
	case isNative(hd.FS):
		size, err = hd.FS.(NativeUploader).UploadChunk(ctx, target, offset, body, n, final)
		var om errOffsetMismatch
		if errors.As(err, &om) {
			return offsetMismatch(c, om.size)
		}
		if err != nil {
			return fsError(err, target)
		}
	case isStaged(hd.FS):
		if size, err = h.stageChunk(hd, target, offset, body, n); err != nil {
			var om errOffsetMismatch
			if errors.As(err, &om) {
				return offsetMismatch(c, om.size)
			}
			return fsError(err, target)
		}
		if final {
			if err := h.commitStaged(ctx, hd, target, size); err != nil {
				return fsError(err, target)
			}
		}
	default:
		size, err = writePart(ctx, hd.FS, target, offset, body, n, final)
		var om errOffsetMismatch
		if errors.As(err, &om) {
			return offsetMismatch(c, om.size)
		}
		if err != nil {
			hd.dropHash(target)
			return fsError(err, target)
		}
		if final {
			if err := commitPart(ctx, hd.FS, target); err != nil {
				return fsError(err, target)
			}
		}
	}

	res := uploadResult{Size: size, Offset: size, Final: final}
	if !final {
		hd.saveHash(target, hasher, size)
		return c.JSON(http.StatusOK, res)
	}
	hd.dropHash(target)
	if hasMtime {
		if err := hd.FS.Chtimes(ctx, target, mtime, mtime); err != nil && !errors.Is(err, ErrNotSupported) {
			h.reg.log.Debug("upload: cannot set mtime", "path", target, "err", err)
		}
	}
	details := map[string]any{"fs": hd.ID, "host": hd.Host, "size": size}
	if hasher != nil {
		details["sha256"] = hex.EncodeToString(hasher.Sum(nil))
	}
	h.audit(c, "fs.upload", target, details)
	if e, err := entryFor(ctx, hd, target); err == nil {
		res.Entry = e
	}
	return c.JSON(http.StatusOK, res)
}

// writePart streams body into target's part file at offset and returns the part size afterwards.
func writePart(ctx context.Context, fsys FS, target string, offset int64, body io.Reader, n int64, final bool) (int64, error) {
	part := target + partSuffix
	if offset > 0 {
		st, err := fsys.Stat(ctx, part)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, errOffsetMismatch{size: 0}
		}
		if err != nil {
			return 0, err
		}
		if st.Size < offset {
			return 0, errOffsetMismatch{size: st.Size}
		}
	}
	if offset == 0 {
		// Folder uploads (drag & drop of a tree): create missing parent folders on the first chunk.
		if _, err := fsys.Stat(ctx, parentDir(target)); errors.Is(err, fs.ErrNotExist) {
			if err := fsys.MkdirAll(ctx, parentDir(target)); err != nil {
				return 0, err
			}
		}
	}
	var w io.WriteCloser
	var err error
	if sc, ok := fsys.(sizedCreator); ok && offset == 0 && final && n >= 0 {
		w, err = sc.CreateSized(ctx, part, n, 0o644)
	} else {
		w, err = fsys.Create(ctx, part, offset)
	}
	if err != nil {
		if errors.Is(err, errCannotTruncate) {
			if st, serr := fsys.Stat(ctx, part); serr == nil {
				return 0, errOffsetMismatch{size: st.Size}
			}
		}
		return 0, err
	}
	written, err := CopyStream(ctx, w, body, n)
	cerr := w.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n >= 0 && written != n {
		err = fmt.Errorf("upload body incomplete (%d of %d bytes)", written, n)
	}
	if err != nil {
		return 0, err
	}
	return offset + written, nil
}

// commitPart atomically replaces target with its part file, keeping the permissions of a replaced file.
func commitPart(ctx context.Context, fsys FS, target string) error {
	part := target + partSuffix
	if te, err := fsys.Stat(ctx, target); err == nil && te.Type == model.FileTypeFile {
		_ = fsys.Chmod(ctx, part, te.Mode&0o7777)
	}
	return fsys.Rename(ctx, part, target)
}

// stagingReserve is the free space always left on the AstraTerm host's disk when data is staged locally (WebDAV
// uploads, zip extraction): a user's upload must never fill the disk that holds the database.
func stagingReserve(total int64) int64 { return max(1<<30, total/20) }

// errNoStagingRoom is answered as 507 Insufficient Storage.
var errNoStagingRoom = httpx.NewError(http.StatusInsufficientStorage, "insufficient_storage",
	"not enough free disk space on the AstraTerm server to stage this data")

// stagingRoom reports whether need more bytes can be staged in dir (unknown sizes / platforms pass).
func stagingRoom(dir string, need int64) error {
	if need <= 0 {
		return nil
	}
	sp, err := diskSpace(dir)
	if err != nil || sp.Total <= 0 {
		return nil
	}
	if sp.Avail-need < stagingReserve(sp.Total) {
		return errNoStagingRoom
	}
	return nil
}

// stageChunk appends a chunk to the local staging file of target (drivers without partial writes).
func (h *handler) stageChunk(hd *Handle, target string, offset int64, body io.Reader, n int64) (int64, error) {
	if err := os.MkdirAll(hd.stagingDir, 0o700); err != nil {
		return 0, err
	}
	if err := stagingRoom(hd.stagingDir, n); err != nil {
		return 0, err
	}
	sp := hd.stagePath(target)
	var size int64
	if fi, err := os.Stat(sp); err == nil {
		size = fi.Size()
	}
	if offset > size {
		return 0, errOffsetMismatch{size: size}
	}
	f, err := os.OpenFile(sp, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := f.Truncate(offset); err != nil {
		return 0, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	written, err := io.Copy(f, body)
	if err != nil {
		return 0, err
	}
	if n >= 0 && written != n {
		return 0, fmt.Errorf("upload body incomplete (%d of %d bytes)", written, n)
	}
	return offset + written, nil
}

// commitStaged uploads a completed staging file to the target.
func (h *handler) commitStaged(ctx context.Context, hd *Handle, target string, size int64) error {
	sp := hd.stagePath(target)
	f, err := os.Open(sp)
	if err != nil {
		return err
	}
	defer os.Remove(sp)
	defer f.Close()
	if wd, ok := hd.FS.(*webdavFS); ok {
		return davErr(wd.c.WriteStreamWithLength(target, f, size, 0o644), target)
	}
	w, err := hd.FS.Create(ctx, target, 0)
	if err != nil {
		return err
	}
	if _, err := CopyStream(ctx, w, f, size); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
