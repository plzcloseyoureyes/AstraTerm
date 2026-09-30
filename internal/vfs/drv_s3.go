package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// s3FS is the S3 / S3-compatible driver (PROTO-26, RESEARCH §3.15): buckets and "/"-delimited prefixes appear as
// folders, uploads are multipart for big files (and natively resumable across HTTP chunks), downloads stream with
// Range, copies are server side, presigned URLs are available. Checksums are only computed when an operation
// requires them (compatibility with MinIO, SeaweedFS, R2, Wasabi...).
type s3FS struct {
	cl      *s3.Client
	presign *s3.PresignClient
	bucket  string // fixed bucket; "" = "/" lists buckets and paths are /bucket/key
	home    string
	closeFn func()

	mu      sync.Mutex
	uploads map[string]*s3Upload // pending resumable uploads by target path
	stop    chan struct{}
}

// Part sizes: S3 requires ≥ 5 MiB for every part but the last and allows 10 000 parts.
const (
	s3MinPart  = 5 << 20
	s3PartSize = 16 << 20
)

func newS3FS(cl *s3.Client, bucket, home string, closeFn func()) *s3FS {
	f := &s3FS{cl: cl, presign: s3.NewPresignClient(cl), bucket: bucket, home: home, closeFn: closeFn,
		uploads: map[string]*s3Upload{}, stop: make(chan struct{})}
	if f.home == "" {
		f.home = "/"
	}
	go f.reapUploads()
	return f
}

func (f *s3FS) Close() error {
	close(f.stop)
	f.mu.Lock()
	ups := f.uploads
	f.uploads = map[string]*s3Upload{}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, u := range ups {
		u.abort(ctx, f)
	}
	if f.closeFn != nil {
		f.closeFn()
	}
	return nil
}

// split maps an API path to bucket and key ("" key = bucket root).
func (f *s3FS) split(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if f.bucket != "" {
		return f.bucket, p
	}
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

// s3Err maps SDK errors to fs errors.
func s3Err(err error, p string) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	var nsb *types.NoSuchBucket
	if errors.As(err, &nsk) || errors.As(err, &nf) || errors.As(err, &nsb) {
		return &os.PathError{Op: "s3", Path: p, Err: fs.ErrNotExist}
	}
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket", "NoSuchUpload":
			return &os.PathError{Op: "s3", Path: p, Err: fs.ErrNotExist}
		case "AccessDenied", "Forbidden", "AllAccessDisabled", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return &os.PathError{Op: "s3", Path: p, Err: fs.ErrPermission}
		case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
			return &os.PathError{Op: "s3", Path: p, Err: fs.ErrExist}
		}
		return fmt.Errorf("s3: %s: %s", ae.ErrorCode(), cleanMsg(ae.ErrorMessage()))
	}
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		switch hs.HTTPStatusCode() {
		case http.StatusNotFound:
			return &os.PathError{Op: "s3", Path: p, Err: fs.ErrNotExist}
		case http.StatusForbidden, http.StatusUnauthorized:
			return &os.PathError{Op: "s3", Path: p, Err: fs.ErrPermission}
		}
	}
	return err
}

func (f *s3FS) List(ctx context.Context, dir string) ([]*Entry, error) {
	bucket, key := f.split(dir)
	if bucket == "" {
		out, err := f.cl.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err != nil {
			return nil, s3Err(err, dir)
		}
		var res []*Entry
		for _, b := range out.Buckets {
			name := aws.ToString(b.Name)
			e := &Entry{Name: name, Path: "/" + name, Type: "dir", Mode: sIFDIR | 0o755}
			if b.CreationDate != nil {
				e.Mtime = b.CreationDate.UTC()
			}
			res = append(res, e)
		}
		return res, nil
	}
	prefix := ""
	if key != "" {
		prefix = strings.TrimSuffix(key, "/") + "/"
	}
	var res []*Entry
	seen := map[string]bool{}
	pg := s3.NewListObjectsV2Paginator(f.cl, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix),
		Delimiter: aws.String("/")})
	pages := 0
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, s3Err(err, dir)
		}
		pages++
		for _, cp := range out.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), prefix), "/")
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			res = append(res, &Entry{Name: name, Path: joinPath(dir, name), Type: "dir", Mode: sIFDIR | 0o755})
		}
		for _, o := range out.Contents {
			name := strings.TrimPrefix(aws.ToString(o.Key), prefix)
			if name == "" || strings.Contains(name, "/") || seen[name] {
				continue // the folder marker itself
			}
			seen[name] = true
			e := &Entry{Name: name, Path: joinPath(dir, name), Type: "file", Mode: sIFREG | 0o644, Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				e.Mtime = o.LastModified.UTC()
			}
			res = append(res, e)
		}
		if pages > 1000 { // 1M entries: stop
			break
		}
	}
	if len(res) == 0 && key != "" {
		// Distinguish an empty folder from a missing one.
		if _, err := f.Stat(ctx, dir); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (f *s3FS) Stat(ctx context.Context, p string) (*Entry, error) {
	bucket, key := f.split(p)
	if bucket == "" {
		return &Entry{Name: "/", Path: "/", Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	if key == "" {
		if _, err := f.cl.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
			return nil, s3Err(err, p)
		}
		return &Entry{Name: baseName(p), Path: p, Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	if !strings.HasSuffix(key, "/") {
		out, err := f.cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err == nil {
			e := &Entry{Name: baseName(p), Path: p, Type: "file", Mode: sIFREG | 0o644, Size: aws.ToInt64(out.ContentLength)}
			if out.LastModified != nil {
				e.Mtime = out.LastModified.UTC()
			}
			return e, nil
		}
		if err = s3Err(err, p); !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if n, ok := f.pendingSize(p); ok {
		return &Entry{Name: baseName(p), Path: p, Type: "file", Mode: sIFREG | 0o644, Size: n, Mtime: time.Now().UTC()}, nil
	}
	out, err := f.cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket),
		Prefix: aws.String(strings.TrimSuffix(key, "/") + "/"), MaxKeys: aws.Int32(1)})
	if err != nil {
		return nil, s3Err(err, p)
	}
	if aws.ToInt32(out.KeyCount) > 0 || len(out.Contents) > 0 || len(out.CommonPrefixes) > 0 {
		return &Entry{Name: baseName(p), Path: p, Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	return nil, &os.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}

func (f *s3FS) Lstat(ctx context.Context, p string) (*Entry, error) { return f.Stat(ctx, p) }

func (f *s3FS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	bucket, key := f.split(p)
	if bucket == "" || key == "" {
		return nil, &os.PathError{Op: "open", Path: p, Err: errIsDir}
	}
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if offset > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	out, err := f.cl.GetObject(ctx, in)
	if err != nil {
		var ae smithy.APIError
		if offset > 0 && errors.As(err, &ae) && ae.ErrorCode() == "InvalidRange" {
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		return nil, s3Err(err, p)
	}
	return out.Body, nil
}

// Create streams into a (multipart when large) upload committed on Close.
func (f *s3FS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	if offset > 0 {
		return nil, fmt.Errorf("s3 objects cannot be written at an offset: %w", ErrNotSupported)
	}
	bucket, key := f.split(p)
	if bucket == "" || key == "" {
		return nil, &os.PathError{Op: "create", Path: p, Err: fs.ErrInvalid}
	}
	u := &s3Upload{bucket: bucket, key: key, path: p}
	return &s3Writer{ctx: ctx, f: f, u: u}, nil
}

type s3Writer struct {
	ctx    context.Context
	f      *s3FS
	u      *s3Upload
	closed bool
	err    error
}

func (w *s3Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.u.write(w.ctx, w.f, p)
	if err != nil {
		w.err = err
		w.u.abort(context.WithoutCancel(w.ctx), w.f)
	}
	return n, err
}

func (w *s3Writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	if err := w.u.complete(w.ctx, w.f); err != nil {
		w.u.abort(context.WithoutCancel(w.ctx), w.f)
		w.err = err
	}
	return w.err
}

// s3Upload is one object upload in progress: data below s3MinPart is buffered; full parts are sent as multipart
// parts. It serves both Create (single stream) and resumable HTTP uploads (NativeUploader, state kept between
// requests).
type s3Upload struct {
	mu       sync.Mutex
	bucket   string
	key      string
	path     string
	uploadID string
	parts    []types.CompletedPart
	buf      []byte
	size     int64 // bytes received (sent parts + buffer)
	lastUsed time.Time
	gone     bool // removed from the pending uploads and aborted
}

func (u *s3Upload) write(ctx context.Context, f *s3FS, p []byte) (int, error) {
	u.buf = append(u.buf, p...)
	u.size += int64(len(p))
	u.lastUsed = time.Now()
	for len(u.buf) >= s3PartSize {
		if err := u.flushPart(ctx, f, u.buf[:s3PartSize]); err != nil {
			u.buf = u.buf[:len(u.buf)-len(p)]
			u.size -= int64(len(p))
			return 0, err
		}
		u.buf = append(u.buf[:0], u.buf[s3PartSize:]...)
	}
	return len(p), nil
}

func (u *s3Upload) flushPart(ctx context.Context, f *s3FS, data []byte) error {
	if u.uploadID == "" {
		out, err := f.cl.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(u.bucket),
			Key: aws.String(u.key)})
		if err != nil {
			return s3Err(err, u.path)
		}
		u.uploadID = aws.ToString(out.UploadId)
	}
	n := int32(len(u.parts) + 1)
	if n > 10000 {
		return fmt.Errorf("s3: object too large (10000 parts)")
	}
	out, err := f.cl.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(u.bucket), Key: aws.String(u.key),
		UploadId: aws.String(u.uploadID), PartNumber: aws.Int32(n), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data)))})
	if err != nil {
		return s3Err(err, u.path)
	}
	u.parts = append(u.parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)})
	return nil
}

func (u *s3Upload) complete(ctx context.Context, f *s3FS) error {
	if u.uploadID == "" {
		_, err := f.cl.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(u.bucket), Key: aws.String(u.key),
			Body: bytes.NewReader(u.buf), ContentLength: aws.Int64(int64(len(u.buf)))})
		if err != nil {
			return s3Err(err, u.path)
		}
		u.buf = nil
		return nil
	}
	if len(u.buf) > 0 {
		if err := u.flushPart(ctx, f, u.buf); err != nil {
			return err
		}
		u.buf = nil
	}
	_, err := f.cl.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(u.bucket),
		Key: aws.String(u.key), UploadId: aws.String(u.uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: u.parts}})
	if err != nil {
		return s3Err(err, u.path)
	}
	u.uploadID = ""
	return nil
}

func (u *s3Upload) abort(ctx context.Context, f *s3FS) {
	if u.uploadID == "" {
		return
	}
	_, _ = f.cl.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(u.bucket),
		Key: aws.String(u.key), UploadId: aws.String(u.uploadID)})
	u.uploadID = ""
}

// ---- NativeUploader (resumable browser uploads) -------------------------------------------------------------------

func (f *s3FS) pendingSize(p string) (int64, bool) {
	f.mu.Lock()
	u := f.uploads[p]
	f.mu.Unlock()
	if u == nil {
		return 0, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.size, true
}

func (f *s3FS) UploadOffset(ctx context.Context, target string) (int64, bool) {
	return f.pendingSize(target)
}

func (f *s3FS) UploadChunk(ctx context.Context, target string, offset int64, body io.Reader, n int64, final bool) (int64, error) {
	bucket, key := f.split(target)
	if bucket == "" || key == "" {
		return 0, &os.PathError{Op: "upload", Path: target, Err: fs.ErrInvalid}
	}
	f.mu.Lock()
	u := f.uploads[target]
	if offset == 0 {
		if u != nil {
			old := u
			go old.abortLocked(f)
		}
		f.evictUploadsLocked()
		u = &s3Upload{bucket: bucket, key: key, path: target, lastUsed: time.Now()}
		f.uploads[target] = u
	}
	f.mu.Unlock()
	if u == nil {
		return 0, fmt.Errorf("%w (no pending upload)", errOffsetMismatch{size: 0})
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.gone {
		return 0, errOffsetMismatch{size: 0} // evicted or reaped meanwhile: the client starts over
	}
	if offset != u.size {
		return u.size, errOffsetMismatch{size: u.size}
	}
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return u.size, err
		}
		m, rerr := body.Read(buf)
		if m > 0 {
			if _, err := u.write(ctx, f, buf[:m]); err != nil {
				return u.size, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return u.size, rerr
		}
	}
	if final {
		err := u.complete(ctx, f)
		f.mu.Lock()
		if f.uploads[target] == u {
			delete(f.uploads, target)
		}
		f.mu.Unlock()
		if err != nil {
			u.abort(context.WithoutCancel(ctx), f)
			return 0, err
		}
	}
	return u.size, nil
}

// maxS3PendingUploads bounds the resumable uploads a handle keeps (each buffers up to one part in memory).
const maxS3PendingUploads = 16

// evictUploadsLocked makes room for a new pending upload by aborting the least recently used idle ones. f.mu held.
func (f *s3FS) evictUploadsLocked() {
	for len(f.uploads) >= maxS3PendingUploads {
		var oldK string
		var oldU *s3Upload
		for k, u := range f.uploads {
			if !u.mu.TryLock() {
				continue // receiving a chunk right now
			}
			if oldU == nil || u.lastUsed.Before(oldU.lastUsed) {
				oldK, oldU = k, u
			}
			u.mu.Unlock()
		}
		if oldU == nil {
			return // all busy: allow the extra one
		}
		delete(f.uploads, oldK)
		go oldU.abortLocked(f)
	}
}

// abortLocked aborts u under its lock (u was removed from the pending map).
func (u *s3Upload) abortLocked(f *s3FS) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gone = true
	u.abort(context.Background(), f)
}

// reapUploads aborts resumable uploads abandoned for an hour.
func (f *s3FS) reapUploads() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
		}
		f.mu.Lock()
		var stale []*s3Upload
		for k, u := range f.uploads {
			if u.mu.TryLock() {
				if time.Since(u.lastUsed) > time.Hour {
					stale = append(stale, u)
					delete(f.uploads, k)
				}
				u.mu.Unlock()
			}
		}
		f.mu.Unlock()
		for _, u := range stale {
			u.abortLocked(f)
		}
	}
}

// ---- other operations ---------------------------------------------------------------------------------------------

func (f *s3FS) Mkdir(ctx context.Context, p string) error {
	bucket, key := f.split(p)
	if bucket == "" {
		return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	if key == "" {
		if f.bucket != "" {
			return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
		}
		_, err := f.cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		return s3Err(err, p)
	}
	if _, err := f.Stat(ctx, p); err == nil {
		return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	_, err := f.cl.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(strings.TrimSuffix(key, "/") + "/"),
		Body: bytes.NewReader(nil), ContentLength: aws.Int64(0)})
	return s3Err(err, p)
}

func (f *s3FS) MkdirAll(ctx context.Context, p string) error {
	if e, err := f.Stat(ctx, p); err == nil {
		if e.Type == "dir" {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	return f.Mkdir(ctx, p)
}

func (f *s3FS) Remove(ctx context.Context, p string) error {
	e, err := f.Stat(ctx, p)
	if err != nil {
		return err
	}
	bucket, key := f.split(p)
	if e.Type == "dir" {
		if key == "" {
			if f.bucket != "" {
				return fmt.Errorf("refusing to delete the bucket root")
			}
			_, err := f.cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
			return s3Err(err, p)
		}
		prefix := strings.TrimSuffix(key, "/") + "/"
		out, err := f.cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(2)})
		if err != nil {
			return s3Err(err, p)
		}
		for _, o := range out.Contents {
			if aws.ToString(o.Key) != prefix {
				return errNotEmpty
			}
		}
		_, err = f.cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix)})
		return s3Err(err, p)
	}
	_, err = f.cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	return s3Err(err, p)
}

// RemoveAll deletes the object or every object below the prefix (DeleteObjects batches, per-object fallback).
func (f *s3FS) RemoveAll(ctx context.Context, p string) error {
	bucket, key := f.split(p)
	if bucket == "" || (key == "" && f.bucket != "") {
		return fmt.Errorf("refusing to delete the root")
	}
	if key != "" {
		_, _ = f.cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	}
	prefix := ""
	if key != "" {
		prefix = strings.TrimSuffix(key, "/") + "/"
	}
	pg := s3.NewListObjectsV2Paginator(f.cl, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			if errors.Is(s3Err(err, p), fs.ErrNotExist) {
				return nil
			}
			return s3Err(err, p)
		}
		if len(out.Contents) == 0 {
			continue
		}
		ids := make([]types.ObjectIdentifier, 0, len(out.Contents))
		for _, o := range out.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: o.Key})
		}
		if _, err := f.cl.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}}); err != nil {
			for _, id := range ids {
				if _, err := f.cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: id.Key}); err != nil {
					return s3Err(err, p)
				}
			}
		}
	}
	if key == "" && f.bucket == "" {
		_, err := f.cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		return s3Err(err, p)
	}
	return nil
}

// copyObject copies one object server side (streaming fallback above the 5 GiB CopyObject limit).
func (f *s3FS) copyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string, size int64) error {
	if size <= 5<<30 {
		_, err := f.cl.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(dstBucket), Key: aws.String(dstKey),
			CopySource: aws.String(srcBucket + "/" + urlPathEscape(srcKey))})
		return s3Err(err, "/"+dstKey)
	}
	r, err := f.Open(ctx, f.pathOf(srcBucket, srcKey), 0)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := f.Create(ctx, f.pathOf(dstBucket, dstKey), 0)
	if err != nil {
		return err
	}
	if _, err := copyBuffer(ctx, w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func (f *s3FS) pathOf(bucket, key string) string {
	if f.bucket != "" {
		return "/" + key
	}
	return "/" + bucket + "/" + key
}

// urlPathEscape escapes an object key for the CopySource header (slashes kept).
func urlPathEscape(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// CopyServerSide copies a file or a prefix tree inside the store.
func (f *s3FS) CopyServerSide(ctx context.Context, src, dst string) error {
	e, err := f.Stat(ctx, src)
	if err != nil {
		return err
	}
	sb, sk := f.split(src)
	db, dk := f.split(dst)
	if e.Type == "file" {
		return f.copyObject(ctx, sb, sk, db, dk, e.Size)
	}
	if sk == "" || dk == "" {
		return ErrNotSupported
	}
	sp, dp := strings.TrimSuffix(sk, "/")+"/", strings.TrimSuffix(dk, "/")+"/"
	pg := s3.NewListObjectsV2Paginator(f.cl, &s3.ListObjectsV2Input{Bucket: aws.String(sb), Prefix: aws.String(sp)})
	copied := false
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return s3Err(err, src)
		}
		for _, o := range out.Contents {
			rest := strings.TrimPrefix(aws.ToString(o.Key), sp)
			if err := f.copyObject(ctx, sb, aws.ToString(o.Key), db, dp+rest, aws.ToInt64(o.Size)); err != nil {
				return err
			}
			copied = true
		}
	}
	if !copied {
		return f.Mkdir(ctx, dst)
	}
	return nil
}

func (f *s3FS) Rename(ctx context.Context, from, to string) error {
	if err := f.CopyServerSide(ctx, from, to); err != nil {
		return err
	}
	return f.RemoveAll(ctx, from)
}

func (f *s3FS) Chmod(ctx context.Context, p string, perm uint32) error      { return ErrNotSupported }
func (f *s3FS) Chown(ctx context.Context, p string, uid, gid int) error     { return ErrNotSupported }
func (f *s3FS) Chtimes(ctx context.Context, p string, a, m time.Time) error { return ErrNotSupported }
func (f *s3FS) Symlink(ctx context.Context, target, link string) error      { return ErrNotSupported }
func (f *s3FS) Readlink(ctx context.Context, p string) (string, error)      { return "", ErrNotSupported }
func (f *s3FS) Home(ctx context.Context) (string, error)                    { return f.home, nil }

func (f *s3FS) Realpath(ctx context.Context, p string) (string, error) {
	if _, err := f.Stat(ctx, p); err != nil {
		return "", err
	}
	return p, nil
}

func (f *s3FS) Presign(ctx context.Context, p string, expires time.Duration) (string, error) {
	bucket, key := f.split(p)
	if bucket == "" || key == "" {
		return "", &os.PathError{Op: "presign", Path: p, Err: fs.ErrInvalid}
	}
	req, err := f.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)},
		s3.WithPresignExpires(expires))
	if err != nil {
		return "", s3Err(err, p)
	}
	return req.URL, nil
}

// Search lists every object below dir (no delimiter) and matches names, far cheaper than walking prefixes.
func (f *s3FS) Search(ctx context.Context, dir, pattern, content string, max int) ([]*Entry, bool, error) {
	if content != "" {
		return nil, false, ErrNotSupported
	}
	bucket, key := f.split(dir)
	if bucket == "" {
		return nil, false, ErrNotSupported
	}
	prefix := ""
	if key != "" {
		prefix = strings.TrimSuffix(key, "/") + "/"
	}
	glob := globPattern(pattern)
	var res []*Entry
	dirs := map[string]bool{}
	pg := s3.NewListObjectsV2Paginator(f.cl, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, false, s3Err(err, dir)
		}
		for _, o := range out.Contents {
			rest := strings.TrimPrefix(aws.ToString(o.Key), prefix)
			// Intermediate "folders" of the key.
			parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
			if slices.ContainsFunc(parts, func(s string) bool { return !entryNameOK(s) }) {
				continue // "a//b", "a/../b": not addressable as a path below dir
			}
			for i := 0; i < len(parts)-1; i++ {
				d := strings.Join(parts[:i+1], "/")
				if !dirs[d] && matchName(glob, parts[i]) {
					dirs[d] = true
					if len(res) >= max {
						return res, true, nil
					}
					res = append(res, &Entry{Name: parts[i], Path: path.Join(dir, d), Type: "dir", Mode: sIFDIR | 0o755})
				}
			}
			if strings.HasSuffix(rest, "/") || rest == "" {
				continue
			}
			name := parts[len(parts)-1]
			if !matchName(glob, name) {
				continue
			}
			if len(res) >= max {
				return res, true, nil
			}
			e := &Entry{Name: name, Path: path.Join(dir, rest), Type: "file", Mode: sIFREG | 0o644, Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				e.Mtime = o.LastModified.UTC()
			}
			res = append(res, e)
		}
	}
	return res, false, nil
}

// errOffsetMismatch reports the size actually stored for a resumable upload.
type errOffsetMismatch struct{ size int64 }

func (e errOffsetMismatch) Error() string {
	return fmt.Sprintf("upload offset mismatch (stored %d bytes)", e.size)
}
