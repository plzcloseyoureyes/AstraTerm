package vfs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// ftpFS is the FTP / FTPS driver (PROTO-23, RESEARCH §3.14): jlaffaye/ftp over Termstead's generic Dialer (proxies,
// jump hosts, sshTunnelVia), explicit (AUTH TLS) or implicit TLS with one shared TLS session cache so servers that
// require data-channel session reuse (vsftpd require_ssl_reuse) work, EPSV with PASV fallback (data connections go to
// the control host unless ftpTrustPasvIP), MLSD/MLST unless ftpMlsd:false, NOOP keepalive and REST-based resume.
// A small pool of logged-in control connections lets listings run while a transfer occupies another connection.
type ftpFS struct {
	cfg  ftpConfig
	dial func(ctx context.Context, addr string) (net.Conn, error)
	tls  *tls.Config

	mu      sync.Mutex
	idle    []*ftpConn
	sem     chan struct{}
	closed  bool
	stop    chan struct{}
	home    string
	canSet  bool
	closeFn func()
	life    context.Context // canceled on Close (data connection dials)
	endLife context.CancelFunc
}

type ftpConfig struct {
	Host, User, Pass string
	Port             int
	TLS              string // none | explicit | implicit
	Insecure         bool
	EPSV, MLSD       bool
	TrustPasvIP      bool
	Timeout          time.Duration
	MaxConns         int
}

type ftpConn struct {
	c        *ftp.ServerConn
	lastUsed time.Time
}

// errCannotTruncate means an offset write would leave stale bytes after the offset (FTP cannot truncate).
var errCannotTruncate = errors.New("the server cannot truncate files")

// ftpPathOK refuses paths that cannot be sent on the FTP control connection: FTP commands are CRLF-terminated text
// lines, so a CR or LF inside a file name (legal on POSIX, e.g. in a malicious source listing of a transfer) would
// smuggle a second command ("x\r\nDELE /important") to the server.
func ftpPathOK(paths ...string) error {
	for _, p := range paths {
		if strings.ContainsAny(p, "\r\n\x00") {
			return &os.PathError{Op: "ftp", Path: strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(p),
				Err: fmt.Errorf("file names with line breaks cannot be used over FTP: %w", fs.ErrInvalid)}
		}
	}
	return nil
}

func newFTPFS(ctx context.Context, cfg ftpConfig, dial func(ctx context.Context, addr string) (net.Conn, error)) (*ftpFS, error) {
	if cfg.Port == 0 {
		cfg.Port = 21
		if cfg.TLS == "implicit" {
			cfg.Port = 990
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 4
	}
	if cfg.User == "" {
		cfg.User, cfg.Pass = "anonymous", "anonymous@"
	}
	f := &ftpFS{cfg: cfg, dial: dial, sem: make(chan struct{}, cfg.MaxConns), stop: make(chan struct{})}
	f.life, f.endLife = context.WithCancel(context.Background())
	if cfg.TLS == "explicit" || cfg.TLS == "implicit" {
		f.tls = &tls.Config{
			ServerName:         cfg.Host,
			InsecureSkipVerify: cfg.Insecure, //nolint:gosec // explicit per-connection option (self-signed FTPS)
			ClientSessionCache: tls.NewLRUClientSessionCache(64),
			MinVersion:         tls.VersionTLS12,
		}
	}
	c, err := f.connect(ctx)
	if err != nil {
		f.endLife()
		return nil, err
	}
	if wd, err := c.c.CurrentDir(); err == nil && strings.HasPrefix(wd, "/") {
		f.home = path.Clean(wd)
	} else {
		f.home = "/"
	}
	f.canSet = c.c.IsSetTimeSupported()
	f.sem <- struct{}{} // put releases a pool slot
	f.put(c, nil)
	go f.keepalive()
	return f, nil
}

// connect dials and logs in a new control connection.
func (f *ftpFS) connect(ctx context.Context) (*ftpConn, error) {
	addr := net.JoinHostPort(f.cfg.Host, strconv.Itoa(f.cfg.Port))
	control := true
	dialFunc := func(network, address string) (net.Conn, error) {
		isControl := control
		control = false
		// The control connection is dialed within the caller's request; data connections are dialed later, for
		// the connection's whole life, so they must not inherit that (by then canceled) request context.
		base := ctx
		if !isControl {
			base = f.life
		}
		dctx, cancel := context.WithTimeout(base, f.cfg.Timeout)
		defer cancel()
		target := address
		if !isControl && !f.cfg.TrustPasvIP {
			// Data connections go to the control host: the PASV address is often private (NAT) and meaningless
			// behind proxies and SSH tunnels.
			if _, port, err := net.SplitHostPort(address); err == nil {
				target = net.JoinHostPort(f.cfg.Host, port)
			}
		}
		raw, err := f.dial(dctx, target)
		if err != nil {
			return nil, err
		}
		conn := net.Conn(&ftpNetConn{Conn: raw, idle: 2 * time.Minute})
		if f.tls != nil && (f.cfg.TLS == "implicit" || !isControl) {
			tc := tls.Client(conn, f.tls)
			if err := tc.HandshakeContext(dctx); err != nil {
				conn.Close()
				return nil, fmt.Errorf("TLS handshake: %w", err)
			}
			return tc, nil
		}
		return conn, nil
	}
	opts := []ftp.DialOption{
		ftp.DialWithDialFunc(dialFunc),
		ftp.DialWithDisabledEPSV(!f.cfg.EPSV),
		ftp.DialWithDisabledMLSD(!f.cfg.MLSD),
		ftp.DialWithShutTimeout(f.cfg.Timeout),
		ftp.DialWithWritingMDTM(true),
		ftp.DialWithForceListHidden(!f.cfg.MLSD),
	}
	switch f.cfg.TLS {
	case "explicit":
		opts = append(opts, ftp.DialWithExplicitTLS(f.tls))
	case "implicit":
		opts = append(opts, ftp.DialWithTLS(f.tls))
	}
	type res struct {
		c   *ftp.ServerConn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ftp.Dial(addr, opts...)
		if err == nil {
			if err = c.Login(f.cfg.User, f.cfg.Pass); err != nil {
				_ = c.Quit()
				c = nil
			}
		}
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, ftpLoginError(r.err)
		}
		return &ftpConn{c: r.c, lastUsed: time.Now()}, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Quit()
			}
		}()
		return nil, ctx.Err()
	}
}

func ftpLoginError(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "530") || strings.Contains(msg, "login") || strings.Contains(msg, "password") {
		return fmt.Errorf("FTP login failed: %s: %w", cleanMsg(err.Error()), fs.ErrPermission)
	}
	return fmt.Errorf("FTP connection failed: %w", err)
}

// ftpNetConn guarantees a *net.TCPAddr RemoteAddr (jlaffaye asserts it; tunnelled conns may not have one) and
// applies a rolling I/O deadline so a dead server cannot block forever.
type ftpNetConn struct {
	net.Conn
	idle time.Duration
}

func (c *ftpNetConn) RemoteAddr() net.Addr {
	if a, ok := c.Conn.RemoteAddr().(*net.TCPAddr); ok {
		return a
	}
	return &net.TCPAddr{IP: net.IPv4zero}
}

func (c *ftpNetConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *ftpNetConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

// get borrows a control connection (idle one or a new login), waiting while the pool is exhausted.
func (f *ftpFS) get(ctx context.Context) (*ftpConn, error) {
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		<-f.sem
		return nil, errHandleClosed
	}
	var c *ftpConn
	if n := len(f.idle); n > 0 {
		c = f.idle[n-1]
		f.idle = f.idle[:n-1]
	}
	f.mu.Unlock()
	if c != nil && time.Since(c.lastUsed) > 30*time.Second {
		if err := c.c.NoOp(); err != nil {
			_ = c.c.Quit()
			c = nil
		}
	}
	if c == nil {
		var err error
		if c, err = f.connect(ctx); err != nil {
			<-f.sem
			return nil, err
		}
	}
	return c, nil
}

// put returns a connection to the pool; a connection that failed at the transport level is discarded.
func (f *ftpFS) put(c *ftpConn, err error) {
	defer func() { <-f.sem }()
	if c == nil {
		return
	}
	if err != nil && isDisconnect(err) {
		_ = c.c.Quit()
		return
	}
	c.lastUsed = time.Now()
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = c.c.Quit()
		return
	}
	f.idle = append(f.idle, c)
	f.mu.Unlock()
}

// with runs fn on a pooled connection.
func (f *ftpFS) with(ctx context.Context, fn func(c *ftp.ServerConn) error) error {
	c, err := f.get(ctx)
	if err != nil {
		return err
	}
	err = fn(c.c)
	f.put(c, err)
	return err
}

// keepalive NOOPs idle connections every minute and drops surplus ones idle for more than 5 minutes.
func (f *ftpFS) keepalive() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
		}
		f.mu.Lock()
		conns := f.idle
		f.idle = nil
		f.mu.Unlock()
		var keep []*ftpConn
		for i, c := range conns {
			if i > 0 && time.Since(c.lastUsed) > 5*time.Minute {
				_ = c.c.Quit()
				continue
			}
			if err := c.c.NoOp(); err != nil {
				_ = c.c.Quit()
				continue
			}
			keep = append(keep, c)
		}
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			for _, c := range keep {
				_ = c.c.Quit()
			}
			return
		}
		f.idle = append(f.idle, keep...)
		f.mu.Unlock()
	}
}

func (f *ftpFS) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	conns := f.idle
	f.idle = nil
	f.mu.Unlock()
	close(f.stop)
	f.endLife()
	for _, c := range conns {
		_ = c.c.Quit()
	}
	if f.closeFn != nil {
		f.closeFn()
	}
	return nil
}

// ---- entries ------------------------------------------------------------------------------------------------------

func ftpEntry(dir string, e *ftp.Entry) *Entry {
	out := &Entry{Name: e.Name, Path: joinPath(dir, e.Name), Mtime: e.Time.UTC()}
	switch e.Type {
	case ftp.EntryTypeFolder:
		out.Type, out.Mode = "dir", sIFDIR|0o755
	case ftp.EntryTypeLink:
		out.Type, out.Mode, out.LinkTarget = "symlink", sIFLNK|0o777, e.Target
	default:
		out.Type, out.Mode, out.Size = "file", sIFREG|0o644, int64(e.Size)
	}
	return out
}

func (f *ftpFS) List(ctx context.Context, dir string) ([]*Entry, error) {
	if err := ftpPathOK(dir); err != nil {
		return nil, err
	}
	var entries []*ftp.Entry
	err := f.with(ctx, func(c *ftp.ServerConn) (err error) {
		entries, err = c.List(dir)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Entry, 0, len(entries))
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." || e.Name == "" || strings.Contains(e.Name, "/") {
			continue
		}
		out = append(out, ftpEntry(dir, e))
	}
	return out, nil
}

func (f *ftpFS) Stat(ctx context.Context, p string) (*Entry, error) {
	e, err := f.Lstat(ctx, p)
	if err != nil || e.Type != "symlink" {
		return e, err
	}
	target := e.LinkTarget
	if !strings.HasPrefix(target, "/") {
		target = path.Join(parentDir(p), target)
	}
	t, err := f.Lstat(ctx, path.Clean(target))
	if err != nil {
		return nil, err
	}
	t.Name, t.Path = baseName(p), p
	return t, nil
}

func (f *ftpFS) Lstat(ctx context.Context, p string) (*Entry, error) {
	if p == "/" {
		return &Entry{Name: "/", Path: "/", Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	if err := ftpPathOK(p); err != nil {
		return nil, err
	}
	var found *Entry
	err := f.with(ctx, func(c *ftp.ServerConn) error {
		if c.IsTimePreciseInList() {
			if e, err := c.GetEntry(p); err == nil {
				if e.Name == "" || strings.Contains(e.Name, "/") {
					e.Name = baseName(p)
				}
				found = ftpEntry(parentDir(p), e)
				found.Name, found.Path = baseName(p), p
				return nil
			} else if isDisconnect(err) {
				return err
			}
		}
		entries, err := c.List(parentDir(p))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name == baseName(p) {
				found = ftpEntry(parentDir(p), e)
				// LIST dates have minute (or day) precision: MDTM gives the exact mtime of a file.
				if found.Type == "file" && c.IsGetTimeSupported() {
					if t, err := c.GetTime(p); err == nil {
						found.Mtime = t.UTC()
					}
				}
				return nil
			}
		}
		return &os.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (f *ftpFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	if err := ftpPathOK(p); err != nil {
		return nil, err
	}
	c, err := f.get(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.c.RetrFrom(p, uint64(max(offset, 0)))
	var tpe *textproto.Error
	if err != nil && offset > 0 && errors.As(err, &tpe) && tpe.Code >= 500 && tpe.Code <= 504 {
		// The server does not support REST: read from the start and skip offset bytes.
		if resp, err = c.c.Retr(p); err == nil {
			if _, err = io.CopyN(io.Discard, resp, offset); err != nil {
				resp.Close()
			}
		}
	}
	if err != nil {
		f.put(c, err)
		return nil, err
	}
	return &ftpReader{f: f, c: c, resp: resp}, nil
}

type ftpReader struct {
	f    *ftpFS
	c    *ftpConn
	resp *ftp.Response
	once sync.Once
	err  error
}

func (r *ftpReader) Read(p []byte) (int, error) {
	n, err := r.resp.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func (r *ftpReader) Close() error {
	var err error
	r.once.Do(func() {
		err = r.resp.Close()
		if r.err != nil {
			err = r.err
		}
		r.f.put(r.c, err)
	})
	return err
}

func (f *ftpFS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	if err := ftpPathOK(p); err != nil {
		return nil, err
	}
	if offset > 0 {
		e, err := f.Lstat(ctx, p)
		if err != nil {
			return nil, err
		}
		if e.Size != offset {
			return nil, fmt.Errorf("%w (current size %d)", errCannotTruncate, e.Size)
		}
	}
	c, err := f.get(ctx)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := c.c.StorFrom(p, pr, uint64(max(offset, 0)))
		pr.CloseWithError(err)
		f.put(c, err)
		done <- err
	}()
	return &ftpWriter{pw: pw, done: done}, nil
}

type ftpWriter struct {
	pw     *io.PipeWriter
	done   chan error
	closed bool
	err    error
}

func (w *ftpWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *ftpWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	_ = w.pw.Close()
	w.err = <-w.done
	return w.err
}

func (f *ftpFS) Mkdir(ctx context.Context, p string) error {
	if err := ftpPathOK(p); err != nil {
		return err
	}
	return f.with(ctx, func(c *ftp.ServerConn) error { return c.MakeDir(p) })
}

func (f *ftpFS) MkdirAll(ctx context.Context, p string) error {
	if p == "/" {
		return nil
	}
	if e, err := f.Lstat(ctx, p); err == nil {
		if e.Type == "dir" || e.LinkType == "dir" {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.MkdirAll(ctx, parentDir(p)); err != nil {
		return err
	}
	return f.Mkdir(ctx, p)
}

func (f *ftpFS) Remove(ctx context.Context, p string) error {
	e, err := f.Lstat(ctx, p)
	if err != nil {
		return err
	}
	return f.with(ctx, func(c *ftp.ServerConn) error {
		if e.Type == "dir" {
			return c.RemoveDir(p)
		}
		return c.Delete(p)
	})
}

func (f *ftpFS) RemoveAll(ctx context.Context, p string) error { return removeAllGeneric(ctx, f, p) }

func (f *ftpFS) Rename(ctx context.Context, from, to string) error {
	if err := ftpPathOK(from, to); err != nil {
		return err
	}
	return f.with(ctx, func(c *ftp.ServerConn) error { return c.Rename(from, to) })
}

func (f *ftpFS) Chmod(ctx context.Context, p string, perm uint32) error  { return ErrNotSupported }
func (f *ftpFS) Chown(ctx context.Context, p string, uid, gid int) error { return ErrNotSupported }
func (f *ftpFS) Symlink(ctx context.Context, target, link string) error  { return ErrNotSupported }
func (f *ftpFS) Home(ctx context.Context) (string, error)                { return f.home, nil }
func (f *ftpFS) Readlink(ctx context.Context, p string) (string, error)  { return f.readlink(ctx, p) }
func (f *ftpFS) Realpath(ctx context.Context, p string) (string, error)  { return f.realpath(ctx, p) }
func (f *ftpFS) Chtimes(ctx context.Context, p string, a, m time.Time) error {
	return f.chtimes(ctx, p, m)
}

func (f *ftpFS) chtimes(ctx context.Context, p string, mtime time.Time) error {
	if !f.canSet {
		return ErrNotSupported
	}
	if err := ftpPathOK(p); err != nil {
		return err
	}
	return f.with(ctx, func(c *ftp.ServerConn) error { return c.SetTime(p, mtime) })
}

func (f *ftpFS) readlink(ctx context.Context, p string) (string, error) {
	e, err := f.Lstat(ctx, p)
	if err != nil {
		return "", err
	}
	if e.Type != "symlink" || e.LinkTarget == "" {
		return "", &os.PathError{Op: "readlink", Path: p, Err: fs.ErrInvalid}
	}
	return e.LinkTarget, nil
}

func (f *ftpFS) realpath(ctx context.Context, p string) (string, error) {
	if _, err := f.Stat(ctx, p); err != nil {
		return "", err
	}
	return p, nil
}
