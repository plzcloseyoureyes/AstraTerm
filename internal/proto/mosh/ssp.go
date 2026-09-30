package mosh

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"syscall"
	"time"

	mosh "github.com/unixshells/mosh-go"
)

// sspClient is the client side of mosh's State Synchronization Protocol, built on mosh-go's wire primitives (AES-OCB,
// fragments, protobuf messages). It replaces mosh-go's Client, which (v0.5.2) has two protocol defects against the
// reference mosh-server:
//
//   - it never sends throwaway_num, so the server keeps every client state; past 1024 states the server accepts one
//     new state per 15 seconds (verified against mosh-server 1.4: keystrokes then take 15 s each);
//   - it renders host diffs whatever state they are based on; a diff based on a state other than the one on screen
//     garbles the display.
//
// Client→server "states" are the cumulative user-input stream (keystrokes and resizes); each new state carries every
// action since the newest state the server acknowledged (the diff base, also sent as throwaway_num). Host states are
// screen diffs; only those based on the state currently displayed are applied, others are dropped (the server
// retransmits from the acknowledged state), exactly like packet loss.
type sspClient struct {
	conn net.Conn // connected UDP socket
	ocb  *mosh.OCB

	mu   sync.Mutex
	cond *sync.Cond

	// Sender (user stream).
	actions  []mosh.UserInstruction // actions after the base state, oldest first
	keyBytes int                    // bytes of Keys in actions (bounded)
	baseNum  uint64                 // newest of our states the server acknowledged
	sent     []sentState            // states sent after baseNum, oldest first
	nextNum  uint64
	dirty    bool // actions added since the newest sent state
	lastNew  time.Time

	// Receiver (host stream).
	recvNum    uint64 // newest host state applied
	ackedRecv  uint64 // recvNum last reported to the server
	forceAck   bool   // report recvNum again (a duplicate or unusable host state arrived)
	lastRecv   time.Time
	remoteTS   int // last timestamp seen from the server, -1 = none
	remoteTSAt time.Time
	shutdown   bool // the server entered its shutdown state (the remote shell exited)

	// Wire.
	seq       uint64
	fragID    uint64
	lastSend  time.Time
	srtt      float64 // ms
	rttvar    float64 // ms
	rttInit   bool
	assembler mosh.FragmentAssembler // receive goroutine only

	// Output.
	out    []byte
	closed bool

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

type sentState struct {
	num      uint64
	nActions int // actions[:nActions] make up this state
	sentAt   time.Time
}

const (
	protocolVersion = 2
	tickInterval    = 8 * time.Millisecond  // mosh SEND_MINDELAY
	heartbeat       = 3 * time.Second       // mosh ACK_INTERVAL
	minRTO          = 50 * time.Millisecond // mosh MIN_RTO
	maxRTO          = time.Second           // mosh MAX_RTO
	maxInFlight     = 32
	maxPendingKeys  = 4 << 20 // typed while the server is unreachable
	maxOutput       = 4 << 20 // undelivered screen output before states are refused (backpressure)
	maxInstruction  = 1 << 20
	noTimestamp     = 0xffff
)

var errBacklog = errors.New("mosh: input backlog full (no contact with the server)")

// newSSPClient starts the protocol on a connected UDP socket.
func newSSPClient(conn net.Conn, ocb *mosh.OCB, cols, rows int) *sspClient {
	c := &sspClient{conn: conn, ocb: ocb, nextNum: 1, remoteTS: -1, srtt: 1000, rttvar: 500, done: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	c.resizeLocked(cols, rows)
	c.wg.Add(2)
	go c.recvLoop()
	go c.tickLoop()
	return c
}

// ---- API ------------------------------------------------------------------------------------------------------------

// Read returns screen output; io.EOF after the server's shutdown (once the output is drained) or Close.
func (c *sspClient) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.out) == 0 && !c.shutdown && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return 0, io.EOF
	}
	if len(c.out) == 0 {
		return 0, io.EOF // shutdown and drained
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	if len(c.out) == 0 {
		c.out = nil
	}
	return n, nil
}

// Send queues keystrokes.
func (c *sspClient) Send(keys []byte) error {
	if len(keys) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	if c.keyBytes+len(keys) > maxPendingKeys {
		return errBacklog
	}
	c.actions = append(c.actions, mosh.UserInstruction{Keys: append([]byte(nil), keys...)})
	c.keyBytes += len(keys)
	c.dirty = true
	return nil
}

// Resize queues a window-size change.
func (c *sspClient) Resize(cols, rows int) {
	c.mu.Lock()
	c.resizeLocked(cols, rows)
	c.mu.Unlock()
}

func (c *sspClient) resizeLocked(cols, rows int) {
	cols = min(max(cols, 1), math.MaxInt16)
	rows = min(max(rows, 1), math.MaxInt16)
	c.actions = append(c.actions, mosh.UserInstruction{Width: int32(cols), Height: int32(rows)})
	c.dirty = true
}

// LastContact returns when the last authentic datagram arrived (zero before the first).
func (c *sspClient) LastContact() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRecv
}

// Ended reports whether the server announced its shutdown.
func (c *sspClient) Ended() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shutdown
}

// Close sends the shutdown state (so the server stops and hangs up its shell; it also acknowledges a server-side
// shutdown) a few times, then stops the client.
func (c *sspClient) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		ti := mosh.TransportInstruction{
			ProtocolVersion: protocolVersion,
			OldNum:          c.baseNum,
			NewNum:          math.MaxUint64,
			AckNum:          c.recvNum,
			ThrowawayNum:    c.baseNum,
		}
		dgs := c.encodeLocked(&ti, time.Now())
		c.cond.Broadcast()
		c.mu.Unlock()
		for range 3 {
			for _, dg := range dgs {
				_, _ = c.conn.Write(dg)
			}
			time.Sleep(15 * time.Millisecond)
		}
		close(c.done)
		c.conn.Close()
		c.wg.Wait()
	})
}

// ---- sending --------------------------------------------------------------------------------------------------------

func (c *sspClient) tickLoop() {
	defer c.wg.Done()
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-t.C:
			for _, dg := range c.tick(now) {
				_, _ = c.conn.Write(dg)
			}
		}
	}
}

// tick decides what to send now: a new state (new input, or an acknowledgement with nothing in flight), a
// retransmission of the newest state in flight (timeout, or to carry an acknowledgement), or nothing.
func (c *sspClient) tick(now time.Time) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	needAck := c.recvNum != c.ackedRecv || c.forceAck
	var st *sentState
	switch {
	case c.dirty && len(c.sent) < maxInFlight:
		c.sent = append(c.sent, sentState{num: c.nextNum, nActions: len(c.actions), sentAt: now})
		c.nextNum++
		c.dirty = false
		c.lastNew = now
		st = &c.sent[len(c.sent)-1]
	case len(c.sent) > 0 && (needAck || now.Sub(c.lastSend) >= heartbeat ||
		// Only states carrying unacknowledged input are retransmitted on timeout (empty ones wait for the
		// heartbeat, like mosh's send_empty_ack).
		(c.sent[len(c.sent)-1].nActions > 0 && now.Sub(c.sent[len(c.sent)-1].sentAt) >= c.rtoLocked())):
		st = &c.sent[len(c.sent)-1]
		st.sentAt = now
	case len(c.sent) == 0 && (needAck || now.Sub(c.lastSend) >= heartbeat):
		// Nothing in flight: an empty new state carries the acknowledgement (like mosh's send_empty_ack; an empty
		// diff is only correct when every action is already acknowledged).
		c.sent = append(c.sent, sentState{num: c.nextNum, nActions: len(c.actions), sentAt: now})
		c.nextNum++
		c.dirty = false
		st = &c.sent[len(c.sent)-1]
	default:
		return nil
	}
	ti := mosh.TransportInstruction{
		ProtocolVersion: protocolVersion,
		OldNum:          c.baseNum,
		NewNum:          st.num,
		AckNum:          c.recvNum,
		ThrowawayNum:    c.baseNum,
	}
	if st.nActions > 0 {
		ti.Diff = mosh.MarshalUserMessage(c.actions[:st.nActions])
	}
	c.ackedRecv, c.forceAck = c.recvNum, false
	return c.encodeLocked(&ti, now)
}

func (c *sspClient) rtoLocked() time.Duration {
	rto := time.Duration((c.srtt + 4*c.rttvar) * float64(time.Millisecond))
	return min(max(rto, minRTO), maxRTO)
}

// encodeLocked compresses, fragments and encrypts an instruction into client→server datagrams.
func (c *sspClient) encodeLocked(ti *mosh.TransportInstruction, now time.Time) [][]byte {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(ti.Marshal())
	_ = zw.Close()
	c.fragID++
	frags := mosh.Fragmentize(c.fragID, z.Bytes())

	var tsReply uint16 = noTimestamp
	if c.remoteTS >= 0 {
		if held := now.Sub(c.remoteTSAt); held < time.Second {
			tsReply = uint16(c.remoteTS + int(held/time.Millisecond))
		}
		c.remoteTS = -1
	}
	ts := uint16(now.UnixMilli())
	out := make([][]byte, 0, len(frags))
	for i := range frags {
		c.seq++
		var nonce [12]byte
		binary.BigEndian.PutUint64(nonce[4:], c.seq&^(1<<63)) // direction bit 0: to server
		fw := frags[i].Marshal()
		plain := make([]byte, 4+len(fw))
		binary.BigEndian.PutUint16(plain[0:], ts)
		binary.BigEndian.PutUint16(plain[2:], tsReply)
		copy(plain[4:], fw)
		ct := c.ocb.Encrypt(nonce[:], plain)
		wire := make([]byte, 8+len(ct))
		copy(wire, nonce[4:])
		copy(wire[8:], ct)
		out = append(out, wire)
	}
	c.lastSend = now
	return out
}

// ---- receiving ------------------------------------------------------------------------------------------------------

func (c *sspClient) recvLoop() {
	defer c.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
			}
			if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
				continue // ICMP unreachable: the server may be restarting or roaming; mosh keeps trying
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		c.handleDatagram(buf[:n], time.Now())
	}
}

func (c *sspClient) handleDatagram(d []byte, now time.Time) {
	if len(d) < 8+16+4 {
		return
	}
	if binary.BigEndian.Uint64(d[:8])&(1<<63) == 0 {
		return // not server→client
	}
	var nonce [12]byte
	copy(nonce[4:], d[:8])
	plain := c.ocb.Decrypt(nonce[:], d[8:])
	if len(plain) < 4 {
		return // forged or corrupt
	}
	ts := binary.BigEndian.Uint16(plain[0:])
	tsReply := binary.BigEndian.Uint16(plain[2:])

	c.mu.Lock()
	c.lastRecv = now
	c.remoteTS, c.remoteTSAt = int(ts), now
	if tsReply != noTimestamp {
		if r := float64(uint16(uint16(now.UnixMilli()) - tsReply)); r < 5000 {
			c.sampleRTTLocked(r)
		}
	}
	c.mu.Unlock()

	if len(plain) < 4+10 {
		return
	}
	frag, err := mosh.UnmarshalFragment(plain[4:])
	if err != nil {
		return
	}
	msg := c.assembler.Add(frag)
	if msg == nil {
		return
	}
	data, err := inflate(msg)
	if err != nil {
		return
	}
	var ti mosh.TransportInstruction
	if ti.Unmarshal(data) != nil || ti.ProtocolVersion != protocolVersion {
		return
	}
	c.apply(&ti)
}

func (c *sspClient) sampleRTTLocked(r float64) {
	if !c.rttInit {
		c.srtt, c.rttvar, c.rttInit = r, r/2, true
		return
	}
	c.rttvar = 0.75*c.rttvar + 0.25*math.Abs(c.srtt-r)
	c.srtt = 0.875*c.srtt + 0.125*r
}

// apply processes the acknowledgement and the host state carried by an instruction.
func (c *sspClient) apply(ti *mosh.TransportInstruction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	// The server has our state AckNum (its newest): it becomes the diff base; its actions are done.
	if ti.AckNum > c.baseNum && ti.AckNum != math.MaxUint64 {
		for i, st := range c.sent {
			if st.num != ti.AckNum {
				continue
			}
			drop := st.nActions
			for _, a := range c.actions[:drop] {
				c.keyBytes -= len(a.Keys)
			}
			c.actions = append([]mosh.UserInstruction(nil), c.actions[drop:]...)
			rest := append([]sentState(nil), c.sent[i+1:]...)
			for j := range rest {
				rest[j].nActions -= drop
			}
			c.sent = rest
			c.baseNum = ti.AckNum
			break
		}
	}
	if ti.NewNum <= c.recvNum {
		c.forceAck = true // duplicate: the server did not see our acknowledgement yet
		return
	}
	if ti.OldNum != c.recvNum || len(c.out) > maxOutput {
		// Based on a state that is not on screen (or no room): treat like loss; the server resends from the
		// acknowledged state.
		c.forceAck = true
		return
	}
	if len(ti.Diff) > 0 {
		instrs, err := mosh.UnmarshalHostMessage(ti.Diff)
		if err != nil {
			return
		}
		for _, hi := range instrs {
			c.out = append(c.out, hi.Hoststring...)
		}
	}
	c.recvNum = ti.NewNum
	if ti.NewNum == math.MaxUint64 {
		c.shutdown = true
	}
	c.cond.Broadcast()
}

func inflate(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxInstruction+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxInstruction {
		return nil, errors.New("mosh: instruction too large")
	}
	return out, nil
}
