package servers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
)

// ftpLoginTimeout disconnects clients that do not log in (slow / idle connections holding a client slot).
var ftpLoginTimeout = 60 * time.Second

// ftpService is the FTP / FTPS server (SRV-3, fclairamb/ftpserverlib) serving a jailed folder.
type ftpService struct {
	m     *Manager
	in    *instance
	cfg   *FTPConfig
	db    *userDB
	tls   *tls.Config
	srv   *ftpserver.FtpServer
	ln    net.Listener
	raw   net.Listener
	bind  netip.Addr
	files openFiles
	wg    sync.WaitGroup
}

func newFTPService(ctx context.Context, m *Manager, in *instance, cfg *FTPConfig) (service, error) {
	if len(cfg.Users) == 0 && !cfg.Anonymous {
		return nil, invalidf("add at least one user or allow anonymous access")
	}
	s := &ftpService{m: m, in: in, cfg: cfg, db: newUserDB(cfg.Users, &in.stats)}
	if a, err := netip.ParseAddr(cfg.BindAddress); err == nil {
		s.bind = a.Unmap()
	}
	if cfg.TLS != "off" {
		cert, fp, err := m.secrets.certificate(ctx)
		if err != nil {
			return nil, fmt.Errorf("TLS certificate: %w", err)
		}
		s.tls = tlsConfig(cert)
		in.fp = fp
	}
	return s, nil
}

func (s *ftpService) start() error {
	raw, err := net.Listen("tcp", hostPort(s.cfg.BindAddress, s.cfg.Port))
	if err != nil {
		return err
	}
	s.raw = raw
	s.in.addrs = []string{raw.Addr().String()}
	var ln net.Listener = &trackingListener{Listener: raw, set: s.in.clients}
	scheme := "ftp"
	switch s.cfg.TLS {
	case "implicit":
		ln = tls.NewListener(ln, s.tls)
		scheme = "ftps"
	case "required":
		scheme = "ftpes"
	}
	s.ln = ln
	s.in.url = serverURL(scheme, s.cfg.BindAddress, raw.Addr().(*net.TCPAddr).Port, "/")
	s.srv = ftpserver.NewFtpServer(s)
	s.srv.Logger = slog.New(&ftpLogHandler{in: s.in})
	if err := s.srv.Listen(); err != nil {
		_ = raw.Close()
		return err
	}
	s.wg.Go(func() {
		if err := s.srv.Serve(); err != nil && !errors.Is(err, net.ErrClosed) && s.in.ctx.Err() == nil {
			s.in.failed(err)
		}
	})
	return nil
}

func (s *ftpService) stop() {
	if s.srv != nil {
		_ = s.srv.Stop()
	} else if s.raw != nil {
		_ = s.raw.Close()
	}
	s.files.closeAll()
	s.in.clients.closeAll()
	s.wg.Wait()
}

// ---- ftpserver.MainDriver -----------------------------------------------------------------------------------------

func (s *ftpService) GetSettings() (*ftpserver.Settings, error) {
	st := &ftpserver.Settings{
		Listener:                s.ln,
		Banner:                  "AstraTerm FTP server",
		IdleTimeout:             s.cfg.IdleTimeoutSec,
		ConnectionTimeout:       30,
		ActiveTransferPortNon20: true,
		DefaultTransferType:     ftpserver.TransferTypeBinary,
		EnableHASH:              true,
		PublicHost:              s.cfg.PublicHost,
	}
	if s.cfg.PublicHost == "" {
		st.PublicIPResolver = s.publicIP
	}
	if s.cfg.PassivePortMin > 0 {
		st.PassiveTransferPortRange = ftpserver.PortRange{Start: s.cfg.PassivePortMin, End: s.cfg.PassivePortMax}
	}
	switch s.cfg.TLS {
	case "required":
		st.TLSRequired = ftpserver.MandatoryEncryption
	case "implicit":
		st.TLSRequired = ftpserver.ImplicitEncryption
	}
	return st, nil
}

// publicIP announces the local address of the control connection in PASV replies (IPv4 only; IPv6 clients use EPSV).
func (s *ftpService) publicIP(cc ftpserver.ClientContext) (string, error) {
	ap, err := netip.ParseAddrPort(cc.LocalAddr().String())
	if err != nil {
		return "", err
	}
	a := ap.Addr().Unmap()
	if !a.Is4() {
		return "", errors.New("PASV needs IPv4: use EPSV")
	}
	return a.String(), nil
}

// WrapPassiveListener refuses data connections arriving on another local address than the server binds (passive
// listeners are opened on every interface by the library).
func (s *ftpService) WrapPassiveListener(l net.Listener) (net.Listener, error) {
	return &passiveGuard{Listener: l, bind: s.bind}, nil
}

type passiveGuard struct {
	net.Listener
	bind netip.Addr
}

func (g *passiveGuard) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !g.bind.IsValid() || g.bind.IsUnspecified() {
			return c, nil
		}
		if ap, err := netip.ParseAddrPort(c.LocalAddr().String()); err == nil && ap.Addr().Unmap() == g.bind {
			return c, nil
		}
		_ = c.Close()
	}
}

func (s *ftpService) clientFor(cc ftpserver.ClientContext) *client {
	if cl, ok := cc.Extra().(*client); ok {
		return cl
	}
	return nil
}

func (s *ftpService) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	addr := cc.RemoteAddr().String()
	cl := s.in.clients.byAddr(addr)
	cc.SetExtra(cl)
	if s.db.lim.blocked(remoteIP(addr)) {
		s.in.logf(levelWarn, addr, "", "Connection refused: too many failed logins")
		return "Too many failed logins, try again later", errTooManyFails
	}
	s.in.logf(levelInfo, addr, "", "Connected")
	if cl != nil {
		// ftpserverlib only knows an idle timeout: bound the time a connection may stay unauthenticated.
		time.AfterFunc(ftpLoginTimeout, func() {
			if cl.getUser() == "" {
				s.in.logf(levelInfo, addr, "", "Disconnected: no login within %s", ftpLoginTimeout)
				cl.close()
			}
		})
	}
	return "AstraTerm FTP server ready", nil
}

func (s *ftpService) ClientDisconnected(cc ftpserver.ClientContext) {
	user := ""
	if cl := s.clientFor(cc); cl != nil {
		user = cl.getUser()
	}
	s.in.logf(levelInfo, cc.RemoteAddr().String(), user, "Disconnected")
}

func isAnonymous(user string) bool {
	return strings.EqualFold(user, "anonymous") || strings.EqualFold(user, "ftp")
}

func (s *ftpService) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	addr := cc.RemoteAddr().String()
	ip := remoteIP(addr)
	cl := s.clientFor(cc)
	if isAnonymous(user) {
		if !s.cfg.Anonymous {
			s.in.logf(levelWarn, addr, user, "Login failed: anonymous access is disabled")
			return nil, s.db.fail(ip)
		}
		ro := s.cfg.ReadOnly || !s.cfg.AnonymousWrite
		if cl != nil {
			cl.setUser("anonymous")
		}
		s.in.logf(levelInfo, addr, "anonymous", "Logged in anonymously%s", roSuffix(ro))
		return s.driver(cl, addr, "anonymous", ro), nil
	}
	u, err := s.db.checkPassword(ip, user, pass)
	if err != nil {
		s.in.logf(levelWarn, addr, user, "Login failed: %v", err)
		return nil, err
	}
	ro := s.cfg.ReadOnly || u.ReadOnly
	if cl != nil {
		cl.setUser(u.Username)
	}
	s.in.logf(levelInfo, addr, u.Username, "Logged in%s", roSuffix(ro))
	return s.driver(cl, addr, u.Username, ro), nil
}

func roSuffix(ro bool) string {
	if ro {
		return " (read-only)"
	}
	return ""
}

func (s *ftpService) GetTLSConfig() (*tls.Config, error) {
	if s.tls == nil {
		return nil, errors.New("TLS is disabled on this server")
	}
	return s.tls, nil
}

// PostAuthMessage keeps authentication failures generic (no details for the client).
func (s *ftpService) PostAuthMessage(_ ftpserver.ClientContext, _ string, authErr error) string {
	switch {
	case authErr == nil:
		return "Login successful"
	case errors.Is(authErr, errTooManyFails):
		return "Too many failed logins, try again later"
	}
	return "Login incorrect"
}

func (s *ftpService) driver(cl *client, addr, user string, ro bool) *ftpDriver {
	return &ftpDriver{aferoFS: aferoFS{r: s.in.root.withReadOnly(ro)}, s: s, cl: cl, addr: addr, user: user}
}

// ---- client driver ------------------------------------------------------------------------------------------------

// ftpDriver is the per-login file system (jailed, read-only views for read-only users) with transfer accounting.
type ftpDriver struct {
	aferoFS
	s    *ftpService
	cl   *client
	addr string
	user string
}

// GetHandle implements ftpserver.ClientDriverExtentionFileTransfer (RETR / STOR / APPE).
func (d *ftpDriver) GetHandle(name string, flags int, offset int64) (ftpserver.FileTransfer, error) {
	upload := flags&(os.O_WRONLY|os.O_RDWR|os.O_APPEND) != 0
	var (
		f   *os.File
		err error
	)
	if upload {
		if err = d.r.checkUploadTarget(name); err == nil {
			f, err = d.r.OpenFile(name, flags, 0o644)
		}
	} else {
		f, err = d.r.OpenRegular(name)
	}
	if err != nil {
		verb := "Download"
		if upload {
			verb = "Upload"
		}
		d.s.in.logf(levelWarn, d.addr, d.user, "%s of %s refused: %v", verb, name, rootCause(err))
		return nil, err
	}
	tf := &transferFile{File: f, name: name, upload: upload}
	tf.done = func(t *transferFile) { d.transferDone(t, offset) }
	if !d.s.files.add(tf) {
		_ = f.Close()
		return nil, errors.New("the server is stopping")
	}
	if d.cl != nil {
		if upload {
			d.cl.setActivity("Uploading " + name)
		} else {
			d.cl.setActivity("Downloading " + name)
		}
	}
	return tf, nil
}

func (d *ftpDriver) transferDone(t *transferFile, offset int64) {
	d.s.files.remove(t)
	if d.cl != nil {
		d.cl.setActivity("")
	}
	n := t.bytes()
	what := "Downloaded"
	if t.upload {
		what = "Uploaded"
	}
	resumed := ""
	if offset > 0 {
		resumed = fmt.Sprintf(", resumed at %s", humanBytes(offset))
	}
	if t.failed.Load() {
		d.s.in.logf(levelWarn, d.addr, d.user, "%s %s incompletely (%s%s, transfer aborted)", what, t.name, humanBytes(n), resumed)
		return
	}
	d.s.in.stats.transfers.Add(1)
	d.s.in.logf(levelInfo, d.addr, d.user, "%s %s (%s%s)", what, t.name, humanBytes(n), resumed)
}

// ReadDir implements ftpserver.ClientDriverExtensionFileList.
func (d *ftpDriver) ReadDir(name string) ([]os.FileInfo, error) { return d.r.ReadDir(name) }

// RemoveDir implements ftpserver.ClientDriverExtensionRemoveDir (RMD removes empty folders only).
func (d *ftpDriver) RemoveDir(name string) error {
	st, err := d.r.Lstat(name)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return &os.PathError{Op: "rmdir", Path: name, Err: errors.New("not a directory")}
	}
	if err := d.r.Remove(name); err != nil {
		return err
	}
	d.s.in.logf(levelInfo, d.addr, d.user, "Removed folder %s", name)
	return nil
}

// Remove (DELE) deletes files only.
func (d *ftpDriver) Remove(name string) error {
	st, err := d.r.Lstat(name)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return &os.PathError{Op: "remove", Path: name, Err: errors.New("is a directory")}
	}
	if err := d.r.Remove(name); err != nil {
		return err
	}
	d.s.in.logf(levelInfo, d.addr, d.user, "Deleted %s", name)
	return nil
}

func (d *ftpDriver) Mkdir(name string, perm os.FileMode) error {
	if err := d.r.Mkdir(name, perm); err != nil {
		return err
	}
	d.s.in.logf(levelInfo, d.addr, d.user, "Created folder %s", name)
	return nil
}

func (d *ftpDriver) Rename(from, to string) error {
	if err := d.r.Rename(from, to); err != nil {
		return err
	}
	d.s.in.logf(levelInfo, d.addr, d.user, "Renamed %s → %s", from, to)
	return nil
}

// Symlink implements ftpserver.ClientDriverExtensionSymlink (SITE SYMLINK).
func (d *ftpDriver) Symlink(oldname, newname string) error { return d.r.Symlink(oldname, newname) }

// ---- logging ------------------------------------------------------------------------------------------------------

// ftpLogHandler forwards the library's warnings and errors to the server log. Debug output (which contains raw
// command lines, including PASS) is never forwarded.
type ftpLogHandler struct {
	in    *instance
	attrs []slog.Attr
}

func (h *ftpLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *ftpLogHandler) Handle(_ context.Context, r slog.Record) error {
	var parts []string
	add := func(a slog.Attr) bool {
		switch a.Key {
		case "line", "param", "clientId":
			return true
		}
		parts = append(parts, a.Key+"="+a.Value.String())
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	msg := r.Message
	if len(parts) > 0 {
		msg += " (" + strings.Join(parts, ", ") + ")"
	}
	level := levelWarn
	if r.Level >= slog.LevelError {
		level = levelError
	}
	// Disconnects and closed sockets are routine.
	if strings.Contains(msg, "use of closed network connection") || strings.Contains(msg, "connection reset") {
		level = levelDebug
	}
	h.in.logf(level, "", "", "%s", msg)
	return nil
}

func (h *ftpLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ftpLogHandler{in: h.in, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *ftpLogHandler) WithGroup(string) slog.Handler { return h }
