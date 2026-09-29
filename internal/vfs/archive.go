package vfs

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/model"
)

// storedExts are already-compressed formats stored without deflate in zips (FILE-9).
var storedExts = map[string]bool{
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true, ".7z": true, ".rar": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true, ".heic": true,
	".mp3": true, ".mp4": true, ".m4a": true, ".m4v": true, ".mkv": true, ".mov": true, ".webm": true, ".ogg": true,
	".flac": true, ".jar": true, ".apk": true, ".docx": true, ".xlsx": true, ".pptx": true, ".odt": true, ".pdf": true,
}

// archiveItem is one path to put into an archive with its name inside the archive.
type archiveItem struct {
	src  string
	name string
}

// archiveItems maps selected paths to archive names relative to their common parent.
func archiveItems(paths []string) []archiveItem {
	_, rel := commonParent(paths)
	items := make([]archiveItem, len(paths))
	for i, p := range paths {
		items[i] = archiveItem{src: p, name: strings.TrimPrefix(rel[i], "./")}
	}
	return items
}

// archiveStats reports what writeArchive stored.
type archiveStats struct {
	Files int
	Bytes int64
}

// writeArchive streams paths (files and trees, Lstat based) as a zip or tar.gz into w without temp files. File
// symlinks are followed, directory symlinks are stored as links (tar) or skipped (zip). skip is a path never included
// (the archive being written).
func writeArchive(ctx context.Context, w io.Writer, fsys FS, paths []string, format, skip string) (archiveStats, error) {
	var st archiveStats
	items := archiveItems(paths)
	switch format {
	case "zip", "":
		zw := zip.NewWriter(w)
		for _, it := range items {
			if err := zipTree(ctx, zw, fsys, it.src, it.name, skip, &st); err != nil {
				return st, err
			}
		}
		return st, zw.Close()
	case "tar.gz", "tgz", "tar":
		var gz *gzip.Writer
		tw := tar.NewWriter(w)
		if format != "tar" {
			gz, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
			tw = tar.NewWriter(gz)
		}
		for _, it := range items {
			if err := tarTree(ctx, tw, fsys, it.src, it.name, skip, &st); err != nil {
				return st, err
			}
		}
		if err := tw.Close(); err != nil {
			return st, err
		}
		if gz != nil {
			return st, gz.Close()
		}
		return st, nil
	}
	return st, fmt.Errorf("unsupported archive format %q", format)
}

func zipTree(ctx context.Context, zw *zip.Writer, fsys FS, src, name, skip string, st *archiveStats) error {
	return walk(ctx, fsys, src, func(e *Entry) error {
		if e.Path == skip {
			return nil
		}
		zname := name
		if r := relTo(e.Path, src); r != "" {
			zname = name + "/" + r
		}
		switch e.Type {
		case model.FileTypeDir:
			h := &zip.FileHeader{Name: zname + "/", Method: zip.Store, Modified: e.Mtime}
			h.SetMode(fs.ModeDir | posixPermToGo(e.Mode&0o7777))
			_, err := zw.CreateHeader(h)
			return err
		case model.FileTypeSymlink:
			t, err := fsys.Stat(ctx, e.Path)
			if err != nil || t.Type != model.FileTypeFile {
				return nil // broken or directory symlink: skipped
			}
			return zipFile(ctx, zw, fsys, e.Path, zname, t, st)
		case model.FileTypeFile:
			return zipFile(ctx, zw, fsys, e.Path, zname, e, st)
		}
		return nil
	})
}

func zipFile(ctx context.Context, zw *zip.Writer, fsys FS, p, zname string, e *Entry, st *archiveStats) error {
	method := zip.Deflate
	if storedExts[strings.ToLower(path.Ext(zname))] {
		method = zip.Store
	}
	h := &zip.FileHeader{Name: zname, Method: method, Modified: e.Mtime}
	h.SetMode(posixPermToGo(e.Mode & 0o7777))
	if e.Size >= 0 {
		h.UncompressedSize64 = uint64(e.Size)
	}
	fw, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	r, err := fsys.Open(ctx, p, 0)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
			return nil // unreadable: leave an empty entry rather than aborting the whole download
		}
		return err
	}
	defer r.Close()
	n, err := CopyStream(ctx, fw, r, -1)
	st.Files++
	st.Bytes += n
	return err
}

func tarTree(ctx context.Context, tw *tar.Writer, fsys FS, src, name, skip string, st *archiveStats) error {
	return walk(ctx, fsys, src, func(e *Entry) error {
		if e.Path == skip {
			return nil
		}
		tname := name
		if r := relTo(e.Path, src); r != "" {
			tname = name + "/" + r
		}
		h := &tar.Header{Name: tname, Mode: int64(e.Mode & 0o7777), ModTime: e.Mtime, Format: tar.FormatPAX}
		if e.UID != nil {
			h.Uid = *e.UID
		}
		if e.GID != nil {
			h.Gid = *e.GID
		}
		if e.Owner != "" && (e.UID == nil || e.Owner != fmt.Sprint(*e.UID)) {
			h.Uname = e.Owner
		}
		if e.Group != "" && (e.GID == nil || e.Group != fmt.Sprint(*e.GID)) {
			h.Gname = e.Group
		}
		switch e.Type {
		case model.FileTypeDir:
			h.Typeflag, h.Name = tar.TypeDir, tname+"/"
			return tw.WriteHeader(h)
		case model.FileTypeSymlink:
			t, err := fsys.Readlink(ctx, e.Path)
			if err != nil {
				return nil
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, t
			return tw.WriteHeader(h)
		case model.FileTypeFile:
			r, err := fsys.Open(ctx, e.Path, 0)
			if err != nil {
				if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			defer r.Close()
			h.Typeflag, h.Size = tar.TypeReg, e.Size
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			// The header announced e.Size bytes: copy exactly that (pad if the file shrank meanwhile).
			n, err := CopyStream(ctx, tw, io.LimitReader(r, e.Size), e.Size)
			if err != nil {
				return err
			}
			if n < e.Size {
				if _, err := io.CopyN(tw, zeroReader{}, e.Size-n); err != nil {
					return err
				}
			}
			st.Files++
			st.Bytes += n
		}
		return nil
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// createArchive writes an archive file at dest on fsys (native tools first, streaming fallback).
func createArchive(ctx context.Context, fsys FS, paths []string, dest, format string) error {
	if format == "" {
		format = "zip"
	}
	if format == "tgz" {
		format = "tar.gz"
	}
	if format != "zip" && format != "tar.gz" && format != "tar" {
		return fmt.Errorf("unsupported archive format %q (zip, tar.gz)", format)
	}
	if a, ok := fsys.(Archiver); ok {
		err := a.Archive(ctx, paths, dest, format)
		if err == nil || !errors.Is(err, ErrNotSupported) {
			return err
		}
	}
	w, err := fsys.Create(ctx, dest, 0)
	if err != nil {
		return err
	}
	if _, err := writeArchive(ctx, w, fsys, paths, format, dest); err != nil {
		w.Close()
		_ = fsys.Remove(context.WithoutCancel(ctx), dest)
		return err
	}
	return w.Close()
}

// ---- extraction ---------------------------------------------------------------------------------------------------

// Extraction limits of the Go implementation.
const (
	// maxExtractEntries bounds generic extraction (zip bombs with millions of entries).
	maxExtractEntries = 200000
	// Zip bombs: the extracted total may not exceed extractRatio × the archive size (at least minExtractLimit).
	extractRatio    = 200
	minExtractLimit = 1 << 30
)

var (
	errUnsafeMember = errors.New("the archive would write through a symbolic link or over a folder")
	errExtractBomb  = errors.New("the archive expands too much (possible zip bomb)")
)

// extractArchive unpacks archive into destDir on fsys: native tools first, then a Go implementation for zip and
// tar(.gz) that is zip-slip safe: entries escaping destDir are skipped, links are never created, nothing is written
// through a symbolic link that already exists below destDir (or replaced one), and the extracted size is bounded.
func extractArchive(ctx context.Context, fsys FS, archive, destDir, tmpDir string) error {
	if x, ok := fsys.(Extractor); ok {
		err := x.Extract(ctx, archive, destDir)
		if err == nil || !errors.Is(err, ErrNotSupported) {
			return err
		}
	}
	st, err := fsys.Stat(ctx, archive)
	if err != nil {
		return err
	}
	if err := fsys.MkdirAll(ctx, destDir); err != nil {
		return err
	}
	x := &extractor{ctx: ctx, fsys: fsys, destDir: destDir, verified: map[string]bool{},
		limit: max(minExtractLimit, extractRatio*st.Size)}
	switch archiveKind(archive) {
	case "zip":
		return x.zip(archive, st.Size, tmpDir)
	case "tar.gz", "tar":
		r, err := fsys.Open(ctx, archive, 0)
		if err != nil {
			return err
		}
		defer r.Close()
		var src io.Reader = r
		if archiveKind(archive) == "tar.gz" {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return fmt.Errorf("invalid gzip data: %w", err)
			}
			defer gz.Close()
			src = gz
		}
		return x.tar(tar.NewReader(src))
	}
	return fmt.Errorf("cannot extract %s: %w", baseName(archive), ErrNotSupported)
}

// safeJoin maps an archive member name into destDir, rejecting absolute paths and ".." escapes.
func safeJoin(destDir, name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.IndexByte(name, 0) >= 0 {
		return "", false
	}
	clean := path.Clean("/" + name)
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", false
		}
	}
	p := path.Clean(destDir + clean)
	if !isWithin(p, destDir) || p == destDir {
		return "", false
	}
	return p, true
}

// extractor writes archive members below destDir.
type extractor struct {
	ctx      context.Context
	fsys     FS
	destDir  string
	verified map[string]bool // folders below destDir known to be real folders (not links) — created or checked
	written  int64
	limit    int64
}

// dir makes d (destDir or below it) exist as a chain of real folders: an existing symlink or file anywhere in the
// chain aborts the extraction rather than letting a member land outside destDir.
func (x *extractor) dir(d string) error {
	if d == x.destDir || x.verified[d] {
		return nil
	}
	if !isWithin(d, x.destDir) {
		return errUnsafeMember
	}
	if err := x.dir(parentDir(d)); err != nil {
		return err
	}
	e, err := x.fsys.Lstat(x.ctx, d)
	switch {
	case err == nil && e.Type == model.FileTypeDir:
	case err == nil:
		return fmt.Errorf("%s: %w", d, errUnsafeMember)
	case errors.Is(err, fs.ErrNotExist):
		if err := x.fsys.Mkdir(x.ctx, d); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if e, err := x.fsys.Lstat(x.ctx, d); err != nil || e.Type != model.FileTypeDir {
			return fmt.Errorf("%s: %w", d, errUnsafeMember)
		}
	default:
		return err
	}
	x.verified[d] = true
	return nil
}

// file writes one regular member. An existing symlink at p is replaced (never followed); a folder is refused.
func (x *extractor) file(p string, r io.Reader, perm uint32, mtime time.Time) error {
	if err := x.dir(parentDir(p)); err != nil {
		return err
	}
	if e, err := x.fsys.Lstat(x.ctx, p); err == nil {
		switch e.Type {
		case model.FileTypeDir:
			return fmt.Errorf("%s: %w", p, errUnsafeMember)
		case model.FileTypeSymlink, model.FileTypeOther:
			if err := x.fsys.Remove(x.ctx, p); err != nil {
				return err
			}
		}
	}
	w, err := x.fsys.Create(x.ctx, p, 0)
	if err != nil {
		return err
	}
	lr := &io.LimitedReader{R: r, N: x.limit - x.written + 1}
	n, err := CopyStream(x.ctx, w, lr, -1)
	x.written += n
	if err == nil && x.written > x.limit {
		err = errExtractBomb
	}
	if err != nil {
		w.Close()
		_ = x.fsys.Remove(context.WithoutCancel(x.ctx), p)
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if perm != 0 {
		_ = x.fsys.Chmod(x.ctx, p, perm)
	}
	if !mtime.IsZero() {
		_ = x.fsys.Chtimes(x.ctx, p, mtime, mtime)
	}
	return nil
}

func (x *extractor) tar(tr *tar.Reader) error {
	for n := 0; ; n++ {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		if n > maxExtractEntries {
			return fmt.Errorf("archive has too many entries")
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid tar data: %w", err)
		}
		p, ok := safeJoin(x.destDir, h.Name)
		if !ok {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := x.dir(p); err != nil {
				return err
			}
			_ = x.fsys.Chmod(x.ctx, p, uint32(h.Mode)&0o7777|0o700)
		case tar.TypeReg, tar.TypeRegA:
			if h.Size > x.limit-x.written {
				return errExtractBomb
			}
			if err := x.file(p, tr, uint32(h.Mode)&0o777, h.ModTime); err != nil {
				return err
			}
		}
	}
}

func (x *extractor) zip(archive string, size int64, tmpDir string) error {
	r, err := x.fsys.Open(x.ctx, archive, 0)
	if err != nil {
		return err
	}
	defer r.Close()
	ra, ok := r.(io.ReaderAt)
	if !ok {
		// Zip needs random access: stage the archive in a local temporary file.
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return err
		}
		if err := stagingRoom(tmpDir, size); err != nil {
			return err
		}
		f, err := os.CreateTemp(tmpDir, "extract-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if _, err := CopyStream(x.ctx, f, r, size); err != nil {
			return err
		}
		ra = f
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return fmt.Errorf("invalid zip data: %w", err)
	}
	if len(zr.File) > maxExtractEntries {
		return fmt.Errorf("archive has too many entries")
	}
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64 // archive/zip never yields more than the declared size
		if total > uint64(x.limit) {
			return errExtractBomb
		}
	}
	for _, f := range zr.File {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		p, ok := safeJoin(x.destDir, f.Name)
		if !ok {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := x.dir(p); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("%s: %w", f.Name, err)
			}
			perm := uint32(mode.Perm())
			if perm == 0 {
				perm = 0o644
			}
			err = x.file(p, rc, perm, f.Modified)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
