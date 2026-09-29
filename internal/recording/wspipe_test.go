package recording

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWSPair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv, cli, err := wsPair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.CloseNow()
	defer cli.CloseNow()
	big := bytes.Repeat([]byte("x"), 200<<10) // above the default 32 KiB read limit
	go func() { _ = srv.Write(ctx, websocket.MessageBinary, big) }()
	typ, data, err := cli.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || !bytes.Equal(data, big) {
		t.Fatalf("srv→cli: %v %v %d", err, typ, len(data))
	}
	go func() { _ = cli.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)) }()
	typ, data, err = srv.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != `{"type":"ping"}` {
		t.Fatalf("cli→srv: %v %v %q", err, typ, data)
	}
	go func() { _ = srv.Close(4410, "bye") }() // net.Pipe is unbuffered: the peer must be reading
	if _, _, err := cli.Read(ctx); websocket.CloseStatus(err) != 4410 {
		t.Fatalf("close status: %v", err)
	}
}
