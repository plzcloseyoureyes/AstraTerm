package guac

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeGuacd answers one handshake on c: it announces args and records what the client sent.
func fakeGuacd(t *testing.T, c net.Conn, version string, args []string, final Instruction) chan []Instruction {
	t.Helper()
	got := make(chan []Instruction, 1)
	go func() {
		defer c.Close()
		r := NewReader(c, CodePoints)
		var seen []Instruction
		sel, err := r.Read()
		if err != nil {
			got <- nil
			return
		}
		seen = append(seen, sel)
		all := args
		if version != "" {
			all = append([]string{version}, args...)
		}
		_, _ = c.Write(New("args", all...).Encode(CodePoints))
		for {
			in, err := r.Read()
			if err != nil {
				got <- seen
				return
			}
			seen = append(seen, in)
			if in.Opcode == "connect" {
				_, _ = c.Write(final.Encode(CodePoints))
				got <- seen
				return
			}
		}
	}()
	return got
}

func TestConnectHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	got := fakeGuacd(t, server, Version150, []string{"hostname", "port", "password", "unused"}, New("ready", "$abc"))
	gc, err := Connect(context.Background(), client, Handshake{
		Protocol: "rdp",
		Params:   map[string]string{"hostname": "rdp", "port": "3389", "password": "sé😀cret", "ignored": "x"},
		Width:    1280, Height: 720, DPI: 120,
		Audio:    []string{"audio/L16"},
		Image:    []string{"image/png", "image/jpeg"},
		Timezone: "Europe/Paris",
		Name:     "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gc.ID != "$abc" || gc.Version != Version150 || strings.Join(gc.Args, ",") != "hostname,port,password,unused" {
		t.Fatalf("conn %+v", gc)
	}
	seen := <-got
	var ops []string
	for _, in := range seen {
		ops = append(ops, in.String())
	}
	want := []string{
		"6.select,3.rdp;",
		"4.size,4.1280,3.720,3.120;",
		"5.audio,9.audio/L16;",
		"5.video;",
		"5.image,9.image/png,10.image/jpeg;",
		"8.timezone,12.Europe/Paris;",
		"4.name,5.alice;",
		"7.connect,13.VERSION_1_5_0,3.rdp,4.3389,7.sé😀cret,0.;",
	}
	if strings.Join(ops, "") != strings.Join(want, "") {
		t.Fatalf("handshake:\n got %q\nwant %q", ops, want)
	}
}

func TestConnectOldServerAndErrors(t *testing.T) {
	// A 1.0.0 server announces no version: no timezone / name, connect without a version element.
	client, server := net.Pipe()
	got := fakeGuacd(t, server, "", []string{"hostname"}, New("ready", "$x"))
	gc, err := Connect(context.Background(), client, Handshake{Protocol: "rdp", Params: map[string]string{"hostname": "h"}, Timezone: "UTC", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	seen := <-got
	last := seen[len(seen)-1].String()
	if gc.Version != Version100 || last != "7.connect,1.h;" {
		t.Fatalf("version %s, connect %q", gc.Version, last)
	}
	for _, in := range seen {
		if in.Opcode == "timezone" || in.Opcode == "name" {
			t.Fatalf("sent %s to a 1.0.0 server", in.Opcode)
		}
	}
	client.Close()

	// "error" during the handshake.
	client, server = net.Pipe()
	fakeGuacd(t, server, Version150, []string{"hostname"}, New("error", "Connection failed", "519"))
	_, err = Connect(context.Background(), client, Handshake{Protocol: "rdp"})
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != StatusUpstreamNotFound || ge.Message != "Connection failed" {
		t.Fatalf("error: %v", err)
	}
	client.Close()

	// Server closes the connection.
	client, server = net.Pipe()
	go func() { _, _ = NewReader(server, CodePoints).Read(); server.Close() }()
	if _, err := Connect(context.Background(), client, Handshake{Protocol: "rdp"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("closed: %v", err)
	}
	client.Close()

	// Cancelled context.
	client, server = net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { _, _ = NewReader(server, CodePoints).Read() }()
	if _, err := Connect(ctx, client, Handshake{Protocol: "rdp"}); err == nil {
		t.Fatal("no error after cancellation")
	}
	client.Close()
}

// TestGuacdTestEnv runs the handshake against the shared test environment's guacd (NEXTERM_TESTENV=1).
func TestGuacdTestEnv(t *testing.T) {
	if os.Getenv("NEXTERM_TESTENV") != "1" {
		t.Skip("NEXTERM_TESTENV=1 not set")
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", "127.0.0.1:22822")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, args, err := ProbeVersion(ctx, dial, "rdp")
	if err != nil {
		t.Fatal(err)
	}
	if !AtLeast(version, Version150) || !strings.Contains(strings.Join(args, ","), "hostname") {
		t.Fatalf("version %s args %v", version, args)
	}
}
