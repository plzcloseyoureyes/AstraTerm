package rawtcp

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/proto/rawtcp/linedisc"
)

func TestRoutedDetection(t *testing.T) {
	if routed(&model.Connection{}) {
		t.Error("plain should be direct")
	}
	if !routed(&model.Connection{Options: model.Options{"proxy": map[string]any{"type": "http", "host": "h", "port": 8080}}}) {
		t.Error("http proxy should be routed")
	}
	if routed(&model.Connection{Options: model.Options{"proxy": map[string]any{"type": "none"}}}) {
		t.Error("proxy type none is direct")
	}
	if !routed(&model.Connection{Options: model.Options{"sshTunnelVia": "abc"}}) {
		t.Error("SSH gateway should be routed")
	}
}

// collector reads a backend with one goroutine and lets tests wait for data.
type collector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func collect(r io.Reader) *collector {
	c := &collector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			c.mu.Lock()
			c.buf = append(c.buf, buf[:n]...)
			if err != nil {
				c.err = err
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *collector) until(timeout time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-acc
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// TestLocalEchoWithoutRemoteOutput: the echo must appear although the peer never sends anything (it used to wait for
// the next remote chunk), and the peer receives CR LF.
func TestLocalEchoWithoutRemoteOutput(t *testing.T) {
	cl, srv := tcpPair(t)
	b := newBackend(cl, linedisc.CRLF, true)
	defer b.Close()
	col := collect(b)
	if _, err := b.Write([]byte("hi\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := col.until(2*time.Second, func(acc []byte) bool { return bytes.HasSuffix(acc, []byte("\r\n")) }); string(got) != "hi\r\n" {
		t.Fatalf("echo = %q, want %q", got, "hi\r\n")
	}
	buf := make([]byte, 16)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := io.ReadAtLeast(srv, buf, 4)
	if string(buf[:n]) != "hi\r\n" {
		t.Fatalf("server received %q", buf[:n])
	}
	// Remote output and echo interleave in arrival order.
	_, _ = srv.Write([]byte("pong\r\n"))
	if got := col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("pong")) }); string(got) != "pong\r\n" {
		t.Fatalf("remote data = %q", got)
	}
}

func TestLineEndingsOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		ending linedisc.Ending
		want   string
	}{{linedisc.CRLF, "a\r\n"}, {linedisc.LF, "a\n"}, {linedisc.CR, "a\r"}} {
		cl, srv := tcpPair(t)
		b := newBackend(cl, tc.ending, false)
		if _, err := b.Write([]byte("a\r")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8)
		_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := io.ReadAtLeast(srv, buf, len(tc.want))
		if string(buf[:n]) != tc.want {
			t.Errorf("%v: server received %q, want %q", tc.ending, buf[:n], tc.want)
		}
		b.Close()
	}
}

func TestPeerCloseIsEOFAndCloseUnblocks(t *testing.T) {
	cl, srv := tcpPair(t)
	b := newBackend(cl, linedisc.CRLF, false)
	col := collect(b)
	_, _ = srv.Write([]byte("bye"))
	srv.Close()
	if got := col.until(2*time.Second, func([]byte) bool { return false }); string(got) != "bye" {
		t.Fatalf("got %q", got)
	}
	col.mu.Lock()
	err := col.err
	col.mu.Unlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("after peer close: %v, want EOF", err)
	}
	b.Close()

	cl2, _ := tcpPair(t)
	b2 := newBackend(cl2, linedisc.CRLF, false)
	errc := make(chan error, 1)
	go func() {
		_, err := b2.Read(make([]byte, 8))
		errc <- err
	}()
	time.Sleep(30 * time.Millisecond)
	b2.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read after Close = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

// TestUDPSurvivesPortUnreachable: a datagram to a closed port raises ICMP port unreachable; the session must keep
// reading and deliver later datagrams.
func TestUDPSurvivesPortUnreachable(t *testing.T) {
	// Reserve a port, then close it so the first datagram is refused.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	probe.Close()

	raw, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	b := newBackend(&udpConn{Conn: raw}, linedisc.CRLF, false)
	defer b.Close()
	col := collect(b)
	if _, err := b.Write([]byte("ping\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the ICMP error arrive

	srv, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Skipf("cannot rebind %s: %v", addr, err)
	}
	defer srv.Close()
	if _, err := srv.WriteTo([]byte("hello"), raw.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	got := col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("hello")) })
	if string(got) != "hello" {
		col.mu.Lock()
		defer col.mu.Unlock()
		t.Fatalf("got %q (err %v)", got, col.err)
	}
}
