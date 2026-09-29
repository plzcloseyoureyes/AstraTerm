package servers

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	syslogFlushEvery  = 250 * time.Millisecond
	syslogMaxBatch    = 500
	syslogSourceTTL   = 5 * time.Minute
	syslogMaxTCPConns = 256
	syslogTCPIdle     = 10 * time.Minute
	defaultMsgLimit   = 500
	maxMsgLimit       = 5000
	// syslogMaxBytes bounds the memory of the message buffer (a flood of 64 KiB datagrams would otherwise hold
	// bufferSize × 64 KiB); the oldest messages are evicted first.
	syslogMaxBytes = 96 << 20
	// syslogMaxBatchBytes bounds one live event (older pending messages are dropped and counted).
	syslogMaxBatchBytes = 1 << 20
	// syslogMaxSources bounds the sender statistics (UDP source addresses are trivially spoofed).
	syslogMaxSources = 10_000
)

// syslogMsgSize estimates the memory held by a stored message.
func syslogMsgSize(m *SyslogMessage) int64 {
	return int64(len(m.Message)+len(m.Hostname)+len(m.AppName)+len(m.ProcID)+len(m.MsgID)+len(m.StructuredData)+
		len(m.Source)) + 160
}

// ---- store (survives restarts of the syslog server) ---------------------------------------------------------------

type sourceStat struct {
	addr     string
	lastSeen time.Time
	count    int64
	bytes    int64
}

// syslogStore is the message ring buffer, the live-event batcher, the sender statistics and the file logger.
type syslogStore struct {
	m *Manager

	mu       sync.Mutex
	ring     []SyslogMessage
	start    int
	n        int
	bytes    int64 // syslogMsgSize of the buffered messages
	maxBytes int64
	nextID   int64
	pending  []SyslogMessage
	pendingB int64
	dropped  int
	timer    *time.Timer
	sources  map[string]*sourceStat
	file     *syslogFile
}

func newSyslogStore(m *Manager) *syslogStore {
	return &syslogStore{m: m, ring: make([]SyslogMessage, 10_000), maxBytes: syslogMaxBytes, nextID: 1,
		sources: map[string]*sourceStat{}}
}

// evictOldestLocked drops the oldest buffered message (its slot is zeroed so the strings can be collected).
func (s *syslogStore) evictOldestLocked() {
	if s.n == 0 {
		return
	}
	old := &s.ring[s.start]
	s.bytes -= syslogMsgSize(old)
	*old = SyslogMessage{}
	s.start = (s.start + 1) % len(s.ring)
	s.n--
}

// resize changes the ring capacity keeping the newest messages.
func (s *syslogStore) resize(size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if size == len(s.ring) || size <= 0 {
		return
	}
	keep := s.n
	if keep > size {
		keep = size
	}
	ring := make([]SyslogMessage, size)
	var bytes int64
	for i := 0; i < keep; i++ {
		ring[i] = s.ring[(s.start+s.n-keep+i)%len(s.ring)]
		bytes += syslogMsgSize(&ring[i])
	}
	s.ring, s.start, s.n, s.bytes = ring, 0, keep, bytes
}

func (s *syslogStore) add(msg SyslogMessage, size int) {
	s.mu.Lock()
	msg.ID = s.nextID
	s.nextID++
	if s.n == len(s.ring) {
		s.evictOldestLocked()
	}
	sz := syslogMsgSize(&msg)
	for s.n > 0 && s.bytes+sz > s.maxBytes {
		s.evictOldestLocked()
	}
	s.ring[(s.start+s.n)%len(s.ring)] = msg
	s.n++
	s.bytes += sz
	st := s.sources[msg.Source]
	if st == nil {
		if len(s.sources) >= syslogMaxSources {
			s.pruneSourcesLocked(time.Now())
		}
		if len(s.sources) < syslogMaxSources {
			st = &sourceStat{addr: msg.Source}
			s.sources[msg.Source] = st
		}
	}
	if st != nil {
		st.lastSeen = msg.Received
		st.count++
		st.bytes += int64(size)
	}
	file := s.file
	if s.m.subs.hasSyslog() {
		for len(s.pending) > 0 && (len(s.pending) >= syslogMaxBatch || s.pendingB+sz > syslogMaxBatchBytes) {
			s.pendingB -= syslogMsgSize(&s.pending[0])
			s.pending[0] = SyslogMessage{}
			s.pending = s.pending[1:]
			s.dropped++
		}
		s.pending = append(s.pending, msg)
		s.pendingB += sz
		if s.timer == nil {
			s.timer = time.AfterFunc(syslogFlushEvery, s.flush)
		}
	}
	s.mu.Unlock()
	if file != nil {
		file.write(&msg)
	}
}

func (s *syslogStore) flush() {
	s.mu.Lock()
	batch, dropped := s.pending, s.dropped
	s.pending, s.pendingB, s.dropped, s.timer = nil, 0, 0, nil
	s.mu.Unlock()
	if len(batch) > 0 {
		s.m.subs.publishSyslog(batch, dropped)
	}
}

func (s *syslogStore) pruneSourcesLocked(now time.Time) {
	for k, st := range s.sources {
		if now.Sub(st.lastSeen) > syslogSourceTTL {
			delete(s.sources, k)
		}
	}
}

// recentSources lists the senders seen within syslogSourceTTL (newest first).
func (s *syslogStore) recentSources() []ClientInfo {
	now := time.Now()
	s.mu.Lock()
	var out []ClientInfo
	for _, st := range s.sources {
		if now.Sub(st.lastSeen) <= syslogSourceTTL {
			out = append(out, ClientInfo{ID: st.addr, Addr: st.addr, Since: st.lastSeen,
				Activity: fmt.Sprintf("%d messages", st.count), BytesIn: st.bytes})
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	if out == nil {
		out = []ClientInfo{}
	}
	return out
}

func (s *syslogStore) clear() {
	s.mu.Lock()
	s.ring = make([]SyslogMessage, len(s.ring)) // release the messages' memory
	s.start, s.n, s.bytes = 0, 0, 0
	s.pending, s.pendingB, s.dropped = nil, 0, 0
	s.sources = map[string]*sourceStat{}
	s.mu.Unlock()
}

func (s *syslogStore) setFile(f *syslogFile) {
	s.mu.Lock()
	old := s.file
	s.file = f
	s.mu.Unlock()
	if old != nil && old != f {
		old.close()
	}
}

func (s *syslogStore) closeFile() { s.setFile(nil) }

// syslogQuery selects messages (GET /api/servers/syslog/messages).
type syslogQuery struct {
	text     string
	re       *regexp.Regexp
	maxSev   int // show severities 0…maxSev (-1 = all)
	facility int // -1 = any
	host     string
	app      string
	after    int64
	before   int64
	limit    int
}

// SyslogPage is a query result: matching messages oldest first.
type SyslogPage struct {
	Messages []SyslogMessage `json:"messages"`
	// HasMore reports older matching messages before the first one returned.
	HasMore bool `json:"hasMore"`
	// Total is the number of messages in the buffer; LastID the newest message ID.
	Total    int   `json:"total"`
	LastID   int64 `json:"lastId"`
	Capacity int   `json:"capacity"`
}

func (q *syslogQuery) match(m *SyslogMessage) bool {
	if q.after > 0 && m.ID <= q.after || q.before > 0 && m.ID >= q.before {
		return false
	}
	if q.maxSev >= 0 && m.Severity > q.maxSev {
		return false
	}
	if q.facility >= 0 && m.Facility != q.facility {
		return false
	}
	if q.host != "" && !strings.EqualFold(m.Source, q.host) && !strings.EqualFold(m.Hostname, q.host) {
		return false
	}
	if q.app != "" && !strings.EqualFold(m.AppName, q.app) {
		return false
	}
	if q.re != nil {
		return q.re.MatchString(m.Message) || q.re.MatchString(m.Hostname) || q.re.MatchString(m.AppName)
	}
	if q.text != "" {
		t := q.text
		return containsFold(m.Message, t) || containsFold(m.Hostname, t) || containsFold(m.AppName, t) ||
			containsFold(m.Source, t) || containsFold(m.MsgID, t)
	}
	return true
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), sub)
}

func (s *syslogStore) query(q syslogQuery) SyslogPage {
	if q.limit <= 0 {
		q.limit = defaultMsgLimit
	}
	if q.limit > maxMsgLimit {
		q.limit = maxMsgLimit
	}
	q.text = strings.ToLower(q.text)
	s.mu.Lock()
	defer s.mu.Unlock()
	page := SyslogPage{Total: s.n, LastID: s.nextID - 1, Capacity: len(s.ring)}
	var out []SyslogMessage
	// Walk newest → oldest so the newest matches win the limit.
	for i := s.n - 1; i >= 0; i-- {
		m := &s.ring[(s.start+i)%len(s.ring)]
		if q.after > 0 && m.ID <= q.after {
			break
		}
		if !q.match(m) {
			continue
		}
		if len(out) == q.limit {
			page.HasMore = true
			break
		}
		out = append(out, *m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []SyslogMessage{}
	}
	page.Messages = out
	return page
}

// formatSyslogLine renders a message as one text line (file logging and exports).
func formatSyslogLine(m *SyslogMessage) string {
	var b strings.Builder
	b.WriteString(m.Received.Local().Format("2006-01-02T15:04:05.000Z07:00"))
	b.WriteByte(' ')
	b.WriteString(m.Source)
	b.WriteByte(' ')
	b.WriteString(facilityName(m.Facility))
	b.WriteByte('.')
	b.WriteString(severityName(m.Severity))
	if m.Hostname != "" {
		b.WriteByte(' ')
		b.WriteString(m.Hostname)
	}
	if m.AppName != "" {
		b.WriteByte(' ')
		b.WriteString(m.AppName)
		if m.ProcID != "" {
			b.WriteString("[" + m.ProcID + "]")
		}
		b.WriteByte(':')
	}
	if m.StructuredData != "" {
		b.WriteByte(' ')
		b.WriteString(m.StructuredData)
	}
	b.WriteByte(' ')
	b.WriteString(strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(m.Message))
	return b.String()
}

// ---- file logging -------------------------------------------------------------------------------------------------

// syslogFile appends messages to daily files syslog-YYYY-MM-DD.log (0600), removes files older than the retention
// and stops writing for the day once the size cap is reached.
type syslogFile struct {
	dir       string
	retention int
	maxBytes  int64
	log       func(level, msg string)

	mu     sync.Mutex
	day    string
	f      *os.File
	w      *bufio.Writer
	size   int64
	capped bool
	closed bool
	stop   chan struct{}
}

func newSyslogFile(dir string, retentionDays, maxMB int, log func(level, msg string)) (*syslogFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the log folder %s: %w", dir, rootCause(err))
	}
	f := &syslogFile{dir: dir, retention: retentionDays, maxBytes: int64(maxMB) << 20, log: log, stop: make(chan struct{})}
	go f.flushLoop()
	return f, nil
}

func (f *syslogFile) flushLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			f.mu.Lock()
			if f.w != nil {
				_ = f.w.Flush()
			}
			f.mu.Unlock()
		}
	}
}

func (f *syslogFile) write(m *SyslogMessage) {
	line := formatSyslogLine(m) + "\n"
	day := m.Received.Local().Format("2006-01-02")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	if day != f.day {
		f.rotateLocked(day)
	}
	if f.w == nil || f.capped {
		return
	}
	if f.maxBytes > 0 && f.size+int64(len(line)) > f.maxBytes {
		f.capped = true
		note := fmt.Sprintf("%s AstraTerm: daily size limit (%d MB) reached; messages are no longer written today\n",
			time.Now().Format("2006-01-02T15:04:05.000Z07:00"), f.maxBytes>>20)
		_, _ = f.w.WriteString(note)
		_ = f.w.Flush()
		if f.log != nil {
			go f.log(levelWarn, "Syslog file size limit reached for "+day)
		}
		return
	}
	n, err := f.w.WriteString(line)
	f.size += int64(n)
	if err != nil && f.log != nil {
		go f.log(levelError, "Writing the syslog file failed: "+err.Error())
	}
}

func (f *syslogFile) rotateLocked(day string) {
	if f.w != nil {
		_ = f.w.Flush()
		_ = f.f.Close()
		f.w, f.f = nil, nil
	}
	f.day, f.capped, f.size = day, false, 0
	name := filepath.Join(f.dir, "syslog-"+day+".log")
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		if f.log != nil {
			go f.log(levelError, "Cannot open the syslog file: "+rootCause(err).Error())
		}
		return
	}
	if st, err := file.Stat(); err == nil {
		f.size = st.Size()
		f.capped = f.maxBytes > 0 && f.size >= f.maxBytes
	}
	f.f, f.w = file, bufio.NewWriterSize(file, 64<<10)
	go f.prune(day)
}

// prune deletes daily files older than the retention (0 = keep).
func (f *syslogFile) prune(today string) {
	if f.retention <= 0 {
		return
	}
	t, err := time.ParseInLocation("2006-01-02", today, time.Local)
	if err != nil {
		return
	}
	cutoff := t.AddDate(0, 0, -f.retention)
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "syslog-") || !strings.HasSuffix(name, ".log") || e.IsDir() {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(name, "syslog-"), ".log"), time.Local)
		if err == nil && d.Before(cutoff) {
			_ = os.Remove(filepath.Join(f.dir, name))
		}
	}
}

func (f *syslogFile) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.stop)
	if f.w != nil {
		_ = f.w.Flush()
		_ = f.f.Close()
	}
}

// ---- service ------------------------------------------------------------------------------------------------------

// syslogService receives syslog over UDP (one message per datagram, several lines allowed) and TCP (RFC 6587 octet
// counting or newline framing).
type syslogService struct {
	m     *Manager
	in    *instance
	cfg   *SyslogConfig
	store *syslogStore
	udp   net.PacketConn
	tcp   net.Listener
	wg    sync.WaitGroup
	sem   chan struct{}
}

func newSyslogService(m *Manager, in *instance, cfg *SyslogConfig) (service, error) {
	return &syslogService{m: m, in: in, cfg: cfg, store: m.syslog, sem: make(chan struct{}, syslogMaxTCPConns)}, nil
}

func (s *syslogService) start() error {
	addr := hostPort(s.cfg.BindAddress, s.cfg.Port)
	port := s.cfg.Port
	if s.cfg.UDP {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return err
		}
		s.udp = pc
		port = pc.LocalAddr().(*net.UDPAddr).Port
		s.in.addrs = append(s.in.addrs, pc.LocalAddr().String()+"/udp")
	}
	if s.cfg.TCP {
		tcpAddr := addr
		if s.cfg.Port == 0 && s.udp != nil {
			tcpAddr = hostPort(s.cfg.BindAddress, port)
		}
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			if s.udp != nil {
				_ = s.udp.Close()
				s.udp = nil
			}
			return err
		}
		s.tcp = ln
		port = ln.Addr().(*net.TCPAddr).Port
		s.in.addrs = append(s.in.addrs, ln.Addr().String()+"/tcp")
	}
	s.in.url = serverURL("syslog", s.cfg.BindAddress, port, "")
	s.store.resize(s.cfg.BufferSize)
	if s.cfg.LogToFile {
		dir := s.cfg.LogDir
		if dir == "" {
			dir = filepath.Join(s.m.env.dataDir, "logs", "syslog")
		}
		f, err := newSyslogFile(dir, s.cfg.RetentionDays, s.cfg.MaxFileMB, func(level, msg string) {
			s.in.logf(level, "", "", "%s", msg)
		})
		if err != nil {
			if s.udp != nil {
				_ = s.udp.Close()
			}
			if s.tcp != nil {
				_ = s.tcp.Close()
			}
			return invalidf("%v", err)
		}
		s.store.setFile(f)
		s.in.logf(levelInfo, "", "", "Writing messages to %s", dir)
	}
	if s.udp != nil {
		s.wg.Add(1)
		go s.serveUDP()
	}
	if s.tcp != nil {
		s.wg.Add(1)
		go s.serveTCP()
	}
	return nil
}

func (s *syslogService) stop() {
	if s.udp != nil {
		_ = s.udp.Close()
	}
	if s.tcp != nil {
		_ = s.tcp.Close()
	}
	s.in.clients.closeAll()
	s.wg.Wait()
	s.store.closeFile()
}

func (s *syslogService) clientCount() int { return len(s.store.recentSources()) }

func (s *syslogService) clientList() []ClientInfo { return s.store.recentSources() }

func (s *syslogService) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, addr, err := s.udp.ReadFrom(buf)
		if err != nil {
			if s.in.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				s.in.failed(err)
			}
			return
		}
		s.in.stats.bytesIn.Add(int64(n))
		src := remoteIP(addr.String())
		for _, line := range bytes.Split(buf[:n], []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) > 0 {
				s.accept(line, src, "udp")
			}
		}
	}
}

func (s *syslogService) accept(raw []byte, src, transport string) {
	msg := parseSyslog(raw, time.Now())
	msg.Source, msg.Transport = src, transport
	s.in.stats.messages.Add(1)
	s.store.add(msg, len(raw))
}

func (s *syslogService) serveTCP() {
	defer s.wg.Done()
	ln := &trackingListener{Listener: s.tcp, set: s.in.clients}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.in.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				s.in.failed(err)
			}
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			s.in.logf(levelWarn, conn.RemoteAddr().String(), "", "Connection refused: too many TCP senders")
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.serveConn(conn)
		}()
	}
}

// serveConn reads RFC 6587 frames: "LEN SP MSG" (octet counting) or LF-terminated lines.
func (s *syslogService) serveConn(conn net.Conn) {
	defer conn.Close()
	addr := conn.RemoteAddr().String()
	src := remoteIP(addr)
	s.in.logf(levelDebug, addr, "", "TCP sender connected")
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(syslogTCPIdle))
		c, err := br.Peek(1)
		if err != nil {
			break
		}
		var frame []byte
		if c[0] >= '1' && c[0] <= '9' {
			frame, err = readOctetFrame(br)
		} else {
			frame, err = readLineFrame(br)
		}
		if len(frame) > 0 {
			s.in.stats.bytesIn.Add(int64(len(frame)))
			s.accept(frame, src, "tcp")
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && s.in.ctx.Err() == nil {
				s.in.logf(levelDebug, addr, "", "TCP sender: %v", err)
			}
			break
		}
	}
	s.in.logf(levelDebug, addr, "", "TCP sender disconnected")
}

func readOctetFrame(br *bufio.Reader) ([]byte, error) {
	head, err := br.ReadSlice(' ')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(string(head[:len(head)-1]))
	if err != nil || n <= 0 || n > maxSyslogMessage {
		return nil, fmt.Errorf("invalid frame length %q", truncate(string(head), 16))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readLineFrame(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(out)+len(chunk) <= maxSyslogMessage {
			out = append(out, chunk...)
		}
		if err == nil {
			return bytes.TrimRight(out, "\r\n\x00"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(out, "\r\n\x00"), err
	}
}
