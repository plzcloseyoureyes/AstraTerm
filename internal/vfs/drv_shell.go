package vfs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// shellFS is the SCP / shell fallback of the SSH browser (PROTO-25, RESEARCH §3.3) for servers without an SFTP
// subsystem (Dropbear, restricted appliances) or connections with sshBrowser: 'scp'. Everything runs as shell
// commands over exec channels of the pooled transport: listings via `find -exec stat -c` (falling back to `ls -la`
// parsing), downloads via `scp -f` (or cat / tail -c for offsets), uploads via `scp -t` when the size is known (else
// cat), and coreutils for the rest. Every path is shell-quoted.
type shellFS struct {
	src    *sshSource
	owners *ownerCache
	sudo   *sudoHelper

	scpOnce sync.Once
	hasSCP  bool
	useLS   atomic.Bool // stat -c unavailable: parse ls -la
}

func newShellFS(src *sshSource, sudo *sudoHelper) *shellFS {
	return &shellFS{src: src, owners: newOwnerCache(src), sudo: sudo}
}

func (s *shellFS) Close() error { s.src.close(); return nil }

func (s *shellFS) run(ctx context.Context, cmd string) ([]byte, error) {
	return runShell(ctx, s.src, cmd, nil)
}

func (s *shellFS) scpAvailable(ctx context.Context) bool {
	s.scpOnce.Do(func() {
		_, err := s.run(ctx, "command -v scp >/dev/null 2>&1")
		s.hasSCP = err == nil
	})
	return s.hasSCP
}

// ---- listing ------------------------------------------------------------------------------------------------------

// linkScript prints, for each quoted name, "<hex mode of the target>\t<link target>\0".
func linkScript(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = shq(n)
	}
	return `for f in ` + strings.Join(q, " ") + `; do m=$(stat -L -c %f -- "$f" 2>/dev/null); t=$(readlink -- "$f" 2>/dev/null); printf '%s\t%s\000' "$m" "$t"; done`
}

// fillLinks resolves symlink targets and target types of entries in one command.
func (s *shellFS) fillLinks(ctx context.Context, entries []*Entry) {
	var links []*Entry
	for _, e := range entries {
		if e.Type == "symlink" {
			links = append(links, e)
		}
	}
	for len(links) > 0 {
		batch := links[:min(len(links), 200)]
		links = links[len(batch):]
		names := make([]string, len(batch))
		for i, e := range batch {
			names[i] = e.Path
		}
		out, err := s.run(ctx, linkScript(names))
		if err != nil {
			continue
		}
		recs := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		for i, rec := range recs {
			if i >= len(batch) {
				break
			}
			m, t, _ := strings.Cut(rec, "\t")
			batch[i].LinkTarget = t
			if m == "" {
				batch[i].LinkType = "broken"
			} else if v, err := strconv.ParseUint(m, 16, 32); err == nil && uint32(v)&sIFMT == sIFDIR {
				batch[i].LinkType = "dir"
			} else {
				batch[i].LinkType = "file"
			}
		}
	}
}

func (s *shellFS) List(ctx context.Context, dir string) ([]*Entry, error) {
	var entries []*Entry
	var err error
	if !s.useLS.Load() {
		entries, err = s.listStat(ctx, dir)
		if errors.Is(err, errNoStat) {
			s.useLS.Store(true)
		}
	}
	if s.useLS.Load() {
		entries, err = s.listLS(ctx, dir)
	}
	if err != nil {
		return nil, err
	}
	s.fillLinks(ctx, entries)
	return entries, nil
}

var errNoStat = errors.New("stat -c is not available")

func (s *shellFS) listStat(ctx context.Context, dir string) ([]*Entry, error) {
	cmd := fmt.Sprintf(`cd -- %s || exit 2; stat -c %s -- . >/dev/null 2>&1 || exit 99; LC_ALL=C find . -mindepth 1 -maxdepth 1 -exec stat -c %s -- {} + 2>/dev/null; exit 0`,
		shq(dir), shq(statFormat), shq(statFormat))
	out := &capWriter{max: maxShellOutput}
	stderr, code, err := s.src.Exec(ctx, cmd, nil, out)
	if err != nil {
		return nil, err
	}
	switch code {
	case 0:
	case 99:
		return nil, errNoStat
	default:
		return nil, shellError(stderr, code, "list")
	}
	var entries []*Entry
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		e, err := parseStatLine(line)
		if err != nil {
			continue
		}
		name := strings.TrimPrefix(e.Path, "./")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		e.Name, e.Path = name, joinPath(dir, name)
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *shellFS) listLS(ctx context.Context, dir string) ([]*Entry, error) {
	out, err := s.run(ctx, "cd -- "+shq(dir)+" && LC_ALL=C ls -lan")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var entries []*Entry
	for _, line := range strings.Split(string(out), "\n") {
		e, ok := parseLSLine(line, now)
		if !ok || e.Name == "." || e.Name == ".." {
			continue
		}
		e.Path = joinPath(dir, e.Name)
		entries = append(entries, e)
	}
	return entries, nil
}

// parseLSLine parses one `ls -ln` line (GNU, busybox, BSD): perms, links, uid, gid, size (or major, minor), date
// (3 fields), name [-> target].
func parseLSLine(line string, now time.Time) (*Entry, bool) {
	f := strings.Fields(line)
	if len(f) < 9 || len(f[0]) < 10 {
		return nil, false
	}
	mode, ok := parsePermString(f[0])
	if !ok {
		return nil, false
	}
	uid, err1 := strconv.Atoi(f[2])
	gid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return nil, false
	}
	i := 4
	var size int64
	if strings.HasSuffix(f[i], ",") { // device: "major, minor"
		i += 2
	} else {
		size, _ = strconv.ParseInt(f[i], 10, 64)
		i++
	}
	if len(f) < i+4 {
		return nil, false
	}
	mt := parseLSDate(f[i], f[i+1], f[i+2], now)
	// The name is everything after the date fields; recover it from the original line to keep inner spaces.
	idx := 0
	for k := 0; k < i+3; k++ {
		j := strings.Index(line[idx:], f[k])
		if j < 0 {
			return nil, false
		}
		idx += j + len(f[k])
	}
	name := strings.TrimPrefix(line[idx:], " ")
	e := &Entry{Mode: mode, Size: size, Mtime: mt, UID: intPtr(uid), GID: intPtr(gid)}
	e.Type = typeFromMode(mode)
	if e.Type == "symlink" {
		if n, t, ok := strings.Cut(name, " -> "); ok {
			name, e.LinkTarget = n, t
		}
	}
	e.Name = name
	return e, name != ""
}

// parsePermString converts "drwxr-sr-t" into st_mode.
func parsePermString(s string) (uint32, bool) {
	var mode uint32
	switch s[0] {
	case '-':
		mode = sIFREG
	case 'd':
		mode = sIFDIR
	case 'l':
		mode = sIFLNK
	case 'c':
		mode = sIFCHR
	case 'b':
		mode = sIFBLK
	case 'p':
		mode = sIFIFO
	case 's':
		mode = sIFSOCK
	default:
		return 0, false
	}
	bits := []uint32{0o400, 0o200, 0o100, 0o040, 0o020, 0o010, 0o004, 0o002, 0o001}
	for i := 0; i < 9; i++ {
		c := s[i+1]
		switch c {
		case '-':
		case 'r', 'w', 'x':
			mode |= bits[i]
		case 's':
			mode |= bits[i]
			if i == 2 {
				mode |= 0o4000
			} else if i == 5 {
				mode |= 0o2000
			}
		case 'S':
			if i == 2 {
				mode |= 0o4000
			} else if i == 5 {
				mode |= 0o2000
			}
		case 't':
			mode |= bits[i] | 0o1000
		case 'T':
			mode |= 0o1000
		default:
			return 0, false
		}
	}
	return mode, true
}

var months = map[string]time.Month{"Jan": 1, "Feb": 2, "Mar": 3, "Apr": 4, "May": 5, "Jun": 6, "Jul": 7, "Aug": 8,
	"Sep": 9, "Oct": 10, "Nov": 11, "Dec": 12}

// parseLSDate parses "Sep 27 10:00" (current year) or "Sep 27 2025", and ISO "2026-09-27 10:00" variants.
func parseLSDate(a, b, c string, now time.Time) time.Time {
	if m, ok := months[a]; ok {
		day, _ := strconv.Atoi(b)
		if strings.Contains(c, ":") {
			hh, mm, _ := strings.Cut(c, ":")
			h, _ := strconv.Atoi(hh)
			mi, _ := strconv.Atoi(mm)
			t := time.Date(now.Year(), m, day, h, mi, 0, 0, time.UTC)
			if t.After(now.Add(24 * time.Hour)) {
				t = t.AddDate(-1, 0, 0)
			}
			return t
		}
		y, _ := strconv.Atoi(c)
		return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	}
	if t, err := time.Parse("2006-01-02 15:04", a+" "+b); err == nil {
		return t
	}
	return time.Time{}
}

func (s *shellFS) statOne(ctx context.Context, p string, follow bool) (*Entry, error) {
	flag := ""
	if follow {
		flag = "-L "
	}
	if s.useLS.Load() {
		out, err := s.run(ctx, "LC_ALL=C ls -land "+flag+"-- "+shq(p))
		if err != nil {
			return nil, err
		}
		e, ok := parseLSLine(strings.TrimSpace(string(out)), time.Now())
		if !ok {
			return nil, fmt.Errorf("unexpected ls output")
		}
		e.Name, e.Path = baseName(p), p
		return e, nil
	}
	out, err := s.run(ctx, "stat "+flag+"-c "+shq(statFormat)+" -- "+shq(p))
	if err != nil {
		if errors.Is(err, ErrNotSupported) {
			s.useLS.Store(true)
			return s.statOne(ctx, p, follow)
		}
		return nil, &os.PathError{Op: "stat", Path: p, Err: unwrapPathErr(err)}
	}
	e, err := parseStatLine(strings.TrimRight(string(out), "\n"))
	if err != nil {
		return nil, err
	}
	e.Name, e.Path = baseName(p), p
	return e, nil
}

func unwrapPathErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (s *shellFS) Stat(ctx context.Context, p string) (*Entry, error) { return s.statOne(ctx, p, true) }

func (s *shellFS) Lstat(ctx context.Context, p string) (*Entry, error) {
	e, err := s.statOne(ctx, p, false)
	if err != nil {
		return nil, err
	}
	s.fillLinks(ctx, []*Entry{e})
	return e, nil
}

// ---- streaming ----------------------------------------------------------------------------------------------------

// execStream is a running command whose stdout is read (download) or stdin written (upload).
type execStream struct {
	sess    *ssh.Session
	release func()
	stdout  io.Reader
	stdin   io.WriteCloser
	stderr  *capWriter
	waitErr chan error
	once    sync.Once
}

func (s *shellFS) start(ctx context.Context, cmd string, wantStdin bool) (*execStream, error) {
	cl, err := s.src.client(ctx)
	if err != nil {
		return nil, err
	}
	sess, _, release, err := cl.NewSessionContext(ctx)
	if err != nil {
		return nil, err
	}
	st := &execStream{sess: sess, release: release, stderr: &capWriter{max: 16 << 10}, waitErr: make(chan error, 1)}
	sess.Stderr = st.stderr
	if st.stdout, err = sess.StdoutPipe(); err != nil {
		sess.Close()
		release()
		return nil, err
	}
	if wantStdin {
		if st.stdin, err = sess.StdinPipe(); err != nil {
			sess.Close()
			release()
			return nil, err
		}
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		release()
		return nil, err
	}
	go func() { st.waitErr <- sess.Wait() }()
	return st, nil
}

// wait returns the command's exit error (as an fs error).
func (st *execStream) wait(timeout time.Duration) error {
	select {
	case err := <-st.waitErr:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return shellError(st.stderr.Bytes(), ee.ExitStatus(), "command")
		}
		return err
	case <-time.After(timeout):
		return errors.New("remote command did not finish")
	}
}

func (st *execStream) kill() {
	st.once.Do(func() {
		_ = st.sess.Signal(ssh.SIGKILL)
		_ = st.sess.Close()
		st.release()
	})
}

// streamReader reads a command's stdout; at EOF it checks the exit status.
type streamReader struct {
	st   *execStream
	r    io.Reader
	done bool
	err  error
}

func (r *streamReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	n, err := r.r.Read(p)
	if err == io.EOF {
		r.done = true
		if werr := r.st.wait(30 * time.Second); werr != nil {
			r.err = werr
			return n, werr
		}
		r.err = io.EOF
	}
	return n, err
}

func (r *streamReader) Close() error {
	r.st.kill()
	return nil
}

func (s *shellFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	e, err := s.Stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if e.Type == "dir" {
		return nil, &os.PathError{Op: "open", Path: p, Err: errIsDir}
	}
	if offset == 0 && s.scpAvailable(ctx) {
		return s.scpDownload(ctx, p)
	}
	cmd := "cat -- " + shq(p)
	if offset > 0 {
		cmd = "tail -c +" + strconv.FormatInt(offset+1, 10) + " -- " + shq(p)
	}
	st, err := s.start(ctx, cmd, false)
	if err != nil {
		return nil, err
	}
	return &streamReader{st: st, r: st.stdout}, nil
}

// scpDownload speaks the SCP source protocol: `scp -f` sends "C<mode> <size> <name>\n", the data and a status byte.
func (s *shellFS) scpDownload(ctx context.Context, p string) (io.ReadCloser, error) {
	st, err := s.start(ctx, "scp -f -- "+shq(p), true)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) {
		st.kill()
		return nil, err
	}
	br := bufio.NewReaderSize(st.stdout, 64<<10)
	if _, err := st.stdin.Write([]byte{0}); err != nil {
		return fail(err)
	}
	var size int64 = -1
	for size < 0 {
		line, err := br.ReadString('\n')
		if err != nil {
			if werr := st.wait(5 * time.Second); werr != nil {
				return fail(werr)
			}
			return fail(fmt.Errorf("scp: %w", err))
		}
		switch line[0] {
		case 'T': // times (with -p): acknowledge
			if _, err := st.stdin.Write([]byte{0}); err != nil {
				return fail(err)
			}
		case 'C':
			f := strings.Fields(line[1:])
			if len(f) < 3 {
				return fail(fmt.Errorf("scp: bad header %q", cleanMsg(line)))
			}
			if size, err = strconv.ParseInt(f[1], 10, 64); err != nil || size < 0 {
				return fail(fmt.Errorf("scp: bad size in %q", cleanMsg(line)))
			}
		case 1, 2:
			return fail(shellError([]byte(line[1:]), 1, "scp"))
		default:
			return fail(fmt.Errorf("scp: unexpected response %q", cleanMsg(line)))
		}
	}
	if _, err := st.stdin.Write([]byte{0}); err != nil {
		return fail(err)
	}
	return &scpReader{st: st, br: br, remain: size}, nil
}

type scpReader struct {
	st     *execStream
	br     *bufio.Reader
	remain int64
	done   bool
}

func (r *scpReader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		if !r.done {
			r.done = true
			status, err := r.br.ReadByte()
			if err != nil {
				return 0, err
			}
			if status != 0 {
				msg, _ := r.br.ReadString('\n')
				return 0, shellError([]byte(msg), 1, "scp")
			}
			_, _ = r.st.stdin.Write([]byte{0})
		}
		return 0, io.EOF
	}
	if int64(len(p)) > r.remain {
		p = p[:r.remain]
	}
	n, err := r.br.Read(p)
	r.remain -= int64(n)
	if err == io.EOF && r.remain > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *scpReader) Close() error {
	if r.st.stdin != nil {
		_ = r.st.stdin.Close()
	}
	r.st.kill()
	return nil
}

// streamWriter feeds a command's stdin; Close waits for its exit status.
type streamWriter struct {
	st     *execStream
	closed bool
}

func (w *streamWriter) Write(p []byte) (int, error) { return w.st.stdin.Write(p) }

func (w *streamWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.st.stdin.Close()
	err := w.st.wait(5 * time.Minute)
	w.st.kill()
	return err
}

func (s *shellFS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	cmd := "cat > " + shq(p)
	if offset > 0 {
		cmd = "truncate -s " + strconv.FormatInt(offset, 10) + " -- " + shq(p) + " && cat >> " + shq(p)
	}
	st, err := s.start(ctx, cmd, true)
	if err != nil {
		return nil, err
	}
	return &streamWriter{st: st}, nil
}

// CreateSized uploads exactly size bytes with the SCP sink protocol (`scp -t`) when scp exists (sizedCreator).
func (s *shellFS) CreateSized(ctx context.Context, p string, size int64, perm uint32) (io.WriteCloser, error) {
	if !s.scpAvailable(ctx) {
		return s.Create(ctx, p, 0)
	}
	st, err := s.start(ctx, "scp -t -- "+shq(p), true)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(st.stdout)
	if err := scpAck(br); err != nil {
		st.kill()
		return nil, err
	}
	if perm == 0 {
		perm = 0o644
	}
	if _, err := fmt.Fprintf(st.stdin, "C%04o %d %s\n", perm&0o7777, size, scpName(baseName(p))); err != nil {
		st.kill()
		return nil, err
	}
	if err := scpAck(br); err != nil {
		st.kill()
		return nil, err
	}
	return &scpWriter{st: st, br: br, size: size}, nil
}

// scpName makes a file name safe for the single-line SCP header.
func scpName(n string) string { return strings.NewReplacer("\n", "_", "\r", "_").Replace(n) }

func scpAck(br *bufio.Reader) error {
	b, err := br.ReadByte()
	if err != nil {
		return fmt.Errorf("scp: %w", err)
	}
	if b == 0 {
		return nil
	}
	msg, _ := br.ReadString('\n')
	return shellError([]byte(msg), 1, "scp")
}

type scpWriter struct {
	st      *execStream
	br      *bufio.Reader
	size    int64
	written int64
	closed  bool
}

func (w *scpWriter) Write(p []byte) (int, error) {
	if w.written+int64(len(p)) > w.size {
		return 0, fmt.Errorf("scp: more data than announced")
	}
	n, err := w.st.stdin.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *scpWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.st.kill()
	if w.written != w.size {
		return fmt.Errorf("scp: upload incomplete (%d of %d bytes)", w.written, w.size)
	}
	if _, err := w.st.stdin.Write([]byte{0}); err != nil {
		return err
	}
	if err := scpAck(w.br); err != nil {
		return err
	}
	_ = w.st.stdin.Close()
	return w.st.wait(time.Minute)
}

// ---- simple operations --------------------------------------------------------------------------------------------

func (s *shellFS) Mkdir(ctx context.Context, p string) error {
	_, err := s.run(ctx, "mkdir -- "+shq(p))
	return err
}

func (s *shellFS) MkdirAll(ctx context.Context, p string) error {
	_, err := s.run(ctx, "mkdir -p -- "+shq(p))
	return err
}

func (s *shellFS) Remove(ctx context.Context, p string) error {
	q := shq(p)
	_, err := s.run(ctx, "if [ -d "+q+" ] && [ ! -L "+q+" ]; then rmdir -- "+q+"; else rm -f -- "+q+"; fi")
	return err
}

func (s *shellFS) RemoveAll(ctx context.Context, p string) error {
	if p == "/" {
		return fmt.Errorf("refusing to delete the root directory")
	}
	_, err := s.run(ctx, "rm -rf -- "+shq(p))
	return err
}

func (s *shellFS) Rename(ctx context.Context, from, to string) error {
	t := shq(to)
	_, err := s.run(ctx, "if [ -d "+t+" ] && [ ! -L "+t+" ]; then echo 'File exists' >&2; exit 1; fi; mv -f -- "+shq(from)+" "+t)
	return err
}

func (s *shellFS) Chmod(ctx context.Context, p string, perm uint32) error {
	_, err := s.run(ctx, fmt.Sprintf("chmod %04o -- %s", perm&0o7777, shq(p)))
	return err
}

func (s *shellFS) Chown(ctx context.Context, p string, uid, gid int) error {
	var cmd string
	switch {
	case uid >= 0 && gid >= 0:
		cmd = fmt.Sprintf("chown -h %d:%d -- %s", uid, gid, shq(p))
	case uid >= 0:
		cmd = fmt.Sprintf("chown -h %d -- %s", uid, shq(p))
	case gid >= 0:
		cmd = fmt.Sprintf("chgrp -h %d -- %s", gid, shq(p))
	default:
		return nil
	}
	_, err := s.run(ctx, cmd)
	return err
}

func (s *shellFS) Chtimes(ctx context.Context, p string, atime, mtime time.Time) error {
	_, err := s.run(ctx, "TZ=UTC0 touch -m -t "+mtime.UTC().Format("200601021504.05")+" -- "+shq(p))
	return err
}

func (s *shellFS) Symlink(ctx context.Context, target, link string) error {
	_, err := s.run(ctx, "ln -s -- "+shq(target)+" "+shq(link))
	return err
}

func (s *shellFS) Link(ctx context.Context, oldname, newname string) error {
	_, err := s.run(ctx, "ln -- "+shq(oldname)+" "+shq(newname))
	return err
}

func (s *shellFS) Readlink(ctx context.Context, p string) (string, error) {
	out, err := s.run(ctx, "readlink -- "+shq(p))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func (s *shellFS) Realpath(ctx context.Context, p string) (string, error) {
	q := shq(p)
	out, err := s.run(ctx, "realpath -- "+q+" 2>/dev/null || readlink -f -- "+q+" 2>/dev/null || (cd -- "+q+" && pwd -P)")
	if err != nil {
		return "", err
	}
	r := strings.TrimSpace(string(out))
	if !strings.HasPrefix(r, "/") {
		return "", &os.PathError{Op: "realpath", Path: p, Err: fs.ErrNotExist}
	}
	return r, nil
}

func (s *shellFS) Home(ctx context.Context) (string, error) {
	out, err := s.run(ctx, `cd && pwd -P`)
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(string(out))
	if !strings.HasPrefix(h, "/") {
		return "/", nil
	}
	return h, nil
}

func (s *shellFS) Space(ctx context.Context, p string) (Space, error) {
	return execSpace(ctx, s.src, p)
}

func (s *shellFS) nameOwners(ctx context.Context, entries []*Entry) {
	s.owners.nameOwners(ctx, entries)
}

func (s *shellFS) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) ([]byte, int, error) {
	return s.src.Exec(ctx, cmd, stdin, stdout)
}

func (s *shellFS) Checksum(ctx context.Context, p, algo string) (string, error) {
	return execChecksum(ctx, s.src, p, algo)
}

func (s *shellFS) Search(ctx context.Context, dir, pattern, content string, max int) ([]*Entry, bool, error) {
	paths, more, err := execSearch(ctx, s.src, dir, pattern, content, max)
	if err != nil {
		return nil, false, err
	}
	return s.statMany(ctx, paths), more, nil
}

// statMany stats many paths with one command per batch.
func (s *shellFS) statMany(ctx context.Context, paths []string) []*Entry {
	var out []*Entry
	for len(paths) > 0 {
		batch := paths[:min(len(paths), 500)]
		paths = paths[len(batch):]
		q := make([]string, len(batch))
		for i, p := range batch {
			q[i] = shq(p)
		}
		res, _ := s.run(ctx, "stat -c "+shq(statFormat)+" -- "+strings.Join(q, " ")+" 2>/dev/null; true")
		for _, line := range strings.Split(string(res), "\n") {
			if e, err := parseStatLine(line); err == nil {
				out = append(out, e)
			}
		}
	}
	s.fillLinks(ctx, out)
	return out
}

func (s *shellFS) Archive(ctx context.Context, paths []string, dest, format string) error {
	return execArchive(ctx, s.src, paths, dest, format)
}

func (s *shellFS) Extract(ctx context.Context, archive, destDir string) error {
	return execExtract(ctx, s.src, archive, destDir)
}

func (s *shellFS) CopyServerSide(ctx context.Context, src, dst string) error {
	_, err := s.run(ctx, "cp -a -- "+shq(src)+" "+shq(dst))
	return err
}

func (s *shellFS) SudoWrite(ctx context.Context, p string, data []byte) error {
	if s.sudo == nil {
		return ErrNotSupported
	}
	return s.sudo.tee(ctx, p, data)
}

// atomicReplace: mv within one folder is rename(2).
func (s *shellFS) atomicReplace(ctx context.Context) bool { return true }

func (s *shellFS) keepInPlace(ctx context.Context, p string) string {
	return execKeepInPlace(ctx, s.src, p)
}

// sizedCreator is implemented by drivers that transfer faster (or only) when the size is known up front (SCP).
type sizedCreator interface {
	CreateSized(ctx context.Context, p string, size int64, perm uint32) (io.WriteCloser, error)
}
