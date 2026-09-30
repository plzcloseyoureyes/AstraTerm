package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pin/tftp/v3"
)

// tftpService is the TFTP server (SRV-2, pin/tftp): read requests from the root folder, write requests when not
// read-only (stored through a temporary file, so a failed upload never leaves a partial file behind).
type tftpService struct {
	m    *Manager
	in   *instance
	cfg  *TFTPConfig
	fs   *rootFS
	srv  *tftp.Server
	conn net.PacketConn
	spc  *singlePortConn
	wg   sync.WaitGroup
}

// singlePortConn lets stop() release the UDP port at once in single-port mode: after forceClose, reads report
// timeouts (the library keeps waiting for its running transfers, which abort) instead of errors it would spin on.
type singlePortConn struct {
	net.PacketConn
	closed atomic.Bool
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func (c *singlePortConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if !c.closed.Load() {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err == nil || !c.closed.Load() {
			return n, addr, err
		}
	}
	time.Sleep(100 * time.Millisecond)
	return 0, nil, timeoutError{}
}

func (c *singlePortConn) forceClose() {
	c.closed.Store(true)
	_ = c.PacketConn.Close()
}

func newTFTPService(m *Manager, in *instance, cfg *TFTPConfig) (service, error) {
	return &tftpService{m: m, in: in, cfg: cfg, fs: in.root.withReadOnly(cfg.ReadOnly)}, nil
}

func (s *tftpService) start() error {
	conn, err := net.ListenPacket("udp", hostPort(s.cfg.BindAddress, s.cfg.Port))
	if err != nil {
		return err
	}
	s.conn = conn
	s.in.addrs = []string{conn.LocalAddr().String()}
	s.in.url = serverURL("tftp", s.cfg.BindAddress, conn.LocalAddr().(*net.UDPAddr).Port, "/")
	s.srv = tftp.NewServer(s.handleRead, s.handleWrite)
	s.srv.SetTimeout(time.Duration(s.cfg.TimeoutSec) * time.Second)
	s.srv.SetRetries(s.cfg.Retries)
	if s.cfg.BlockSize > 0 {
		s.srv.SetBlockSize(s.cfg.BlockSize)
	}
	if s.cfg.SinglePort {
		s.srv.EnableSinglePort()
	}
	s.srv.SetHook(tftpHook{s})
	var serveConn net.PacketConn = conn
	if s.cfg.SinglePort {
		s.spc = &singlePortConn{PacketConn: conn}
		serveConn = s.spc
		// The wrapper hides the interface MTU from the library: honor the block size the client asks for instead
		// (still capped by BlockSize when set).
		s.srv.SetBlockSizeNegotiation(false)
	}
	s.wg.Go(func() {
		err := s.srv.Serve(serveConn)
		if s.in.ctx.Err() == nil {
			if err == nil {
				err = errors.New("the TFTP listener stopped")
			}
			s.in.failed(err)
		}
	})
	return nil
}

func (s *tftpService) stop() {
	if s.srv != nil {
		done := make(chan struct{})
		go func() {
			s.srv.Shutdown() // closes the socket, then waits for the running transfers (they abort: the context is done)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		if s.spc != nil {
			s.spc.forceClose() // single-port Shutdown does not close the socket
		}
	} else if s.conn != nil {
		_ = s.conn.Close()
	}
	s.in.clients.closeAll()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// tftpName maps a requested file name to a root-relative path (DOS separators and drive letters are accepted).
func tftpName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || len(name) > maxPathLen {
		return "", errors.New("invalid file name")
	}
	n := strings.ReplaceAll(name, `\`, "/")
	if len(n) >= 2 && n[1] == ':' && (n[0]|0x20 >= 'a' && n[0]|0x20 <= 'z') {
		n = n[2:]
	}
	rel := strings.TrimPrefix(path.Clean("/"+n), "/")
	if rel == "" {
		return "", errors.New("missing file name")
	}
	return rel, nil
}

// transferCtx gives every transfer a context cancelled by a server stop or by disconnecting the client. It fails when
// the server is stopping or too many transfers run (the connection limits of clientSet).
func (s *tftpService) transferCtx(remote string, activity string) (context.Context, *client, func(), error) {
	ctx, cancel := context.WithCancel(s.in.ctx)
	cl, err := s.in.clients.add(remote, cancel)
	if err != nil {
		cancel()
		return nil, nil, func() {}, err
	}
	cl.setActivity(activity)
	return ctx, cl, func() {
		cancel()
		s.in.clients.remove(cl)
	}, nil
}

func (s *tftpService) handleRead(filename string, rf io.ReaderFrom) error {
	remote := ""
	ot, _ := rf.(tftp.OutgoingTransfer)
	if ot != nil {
		a := ot.RemoteAddr()
		remote = a.String()
	}
	name, err := tftpName(filename)
	if err != nil {
		s.in.logf(levelWarn, remote, "", "Read %q refused: %v", filename, err)
		return err
	}
	ctx, cl, done, err := s.transferCtx(remote, "Sending "+name)
	defer done()
	if err != nil {
		return err
	}
	f, err := s.fs.OpenRegular(name)
	if err != nil {
		s.in.logf(levelWarn, remote, "", "Read %s refused: %v", name, tftpError(err))
		return tftpError(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return tftpError(err)
	}
	if ot != nil {
		ot.SetSize(st.Size())
	}
	start := time.Now()
	n, err := rf.ReadFrom(&ctxReader{ctx: ctx, r: f, count: &s.in.stats.bytesOut, cl: cl})
	if err != nil {
		s.in.logf(levelWarn, remote, "", "Sending %s failed after %s: %v", name, humanBytes(n), err)
		return err
	}
	s.in.stats.transfers.Add(1)
	s.in.logf(levelInfo, remote, "", "Sent %s (%s in %s)", name, humanBytes(n), time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *tftpService) handleWrite(filename string, wt io.WriterTo) error {
	remote := ""
	it, _ := wt.(tftp.IncomingTransfer)
	if it != nil {
		a := it.RemoteAddr()
		remote = a.String()
	}
	if s.cfg.ReadOnly {
		s.in.logf(levelWarn, remote, "", "Write %q refused: the server is read-only", filename)
		return errors.New("access violation: the server is read-only")
	}
	name, err := tftpName(filename)
	if err != nil {
		s.in.logf(levelWarn, remote, "", "Write %q refused: %v", filename, err)
		return err
	}
	ctx, cl, done, err := s.transferCtx(remote, "Receiving "+name)
	defer done()
	if err != nil {
		return err
	}
	if st, err := s.fs.Stat(name); err == nil && st.IsDir() {
		return errors.New("a folder with that name exists")
	}
	dir, base := path.Split(filepath.ToSlash(name))
	tmp := path.Join(dir, "."+base+".astraterm-part-"+randomSuffix())
	f, err := s.fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		s.in.logf(levelWarn, remote, "", "Write %s refused: %v", name, tftpError(err))
		return tftpError(err)
	}
	start := time.Now()
	n, err := wt.WriteTo(&ctxWriter{ctx: ctx, w: f, count: &s.in.stats.bytesIn, cl: cl})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.fs.Rename(tmp, name)
	}
	if err != nil {
		_ = s.fs.Remove(tmp)
		s.in.logf(levelWarn, remote, "", "Receiving %s failed after %s: %v", name, humanBytes(n), rootCause(err))
		return err
	}
	s.in.stats.transfers.Add(1)
	s.in.logf(levelInfo, remote, "", "Received %s (%s in %s)", name, humanBytes(n), time.Since(start).Round(time.Millisecond))
	return nil
}

// tftpError turns a file system error into the message sent to the client (no host paths).
func tftpError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("file not found")
	case errors.Is(err, fs.ErrPermission), strings.Contains(err.Error(), "escapes"):
		return errors.New("access violation")
	case errors.Is(err, errNotRegular):
		return errors.New("not a file")
	}
	return fmt.Errorf("%v", rootCause(err))
}

// tftpHook logs library-level transfer failures (e.g. timeouts) at debug level.
type tftpHook struct{ s *tftpService }

func (h tftpHook) OnSuccess(tftp.TransferStats) {}

func (h tftpHook) OnFailure(st tftp.TransferStats, err error) {
	if st.Filename == "" {
		return // malformed packets
	}
	h.s.in.logf(levelDebug, st.RemoteAddr.String(), "", "Transfer of %s failed: %v", st.Filename, err)
}

// ctxReader / ctxWriter abort a transfer when ctx ends and count payload bytes.
type ctxReader struct {
	ctx   context.Context
	r     io.Reader
	count interface{ Add(int64) int64 }
	cl    *client
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, errors.New("transfer cancelled")
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.count.Add(int64(n))
		if c.cl != nil {
			c.cl.bytesOut.Add(int64(n))
		}
	}
	return n, err
}

type ctxWriter struct {
	ctx   context.Context
	w     io.Writer
	count interface{ Add(int64) int64 }
	cl    *client
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, errors.New("transfer cancelled")
	}
	n, err := c.w.Write(p)
	if n > 0 {
		c.count.Add(int64(n))
		if c.cl != nil {
			c.cl.bytesIn.Add(int64(n))
		}
	}
	return n, err
}
