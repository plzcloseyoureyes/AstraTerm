package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/server"
)

// readyPrefix starts the one stdout line the desktop app waits for: "ASTRATERM_READY <url>".
const readyPrefix = "ASTRATERM_READY "

// sidecar runs AstraTerm inside the desktop app (desktop/, Tauri), which starts this binary and shows its UI in a
// native window. Same flags as serve. It prints readyPrefix + the UI URL (with the one-time launch token) on stdout
// once the UI can be loaded, logs to stderr and to astraterm-desktop.log in the data directory, and stops gracefully
// when stdin closes — the app quit or died. While it runs it is a normal server: browsers can connect too.
//
// If AstraTerm already answers on the loopback address (a running server or app on the same port), it reports that
// URL instead of starting a second instance. When the default port is taken by another program, it uses a free one.
func sidecar(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, apply := config.FlagSet("astraterm sidecar", stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := apply()
	if err != nil {
		fmt.Fprintf(stderr, "astraterm: %v\n", err)
		return 1
	}
	cfg.Version = version
	cfg.LocalAccount = true // a desktop program: no account setup, the app signs in by itself

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the app is gone our stdout/stderr pipes are broken: a write must fail, not kill the process (Go's default
	// for SIGPIPE on fd 1 and 2), so the shutdown below stays graceful.
	signal.Ignore(syscall.SIGPIPE)
	go func() {
		_, _ = io.Copy(io.Discard, stdin) // returns when the app closes our stdin
		stop()
	}()

	if url, ok := runningAt(ctx, cfg.Listen); ok {
		fmt.Fprintln(stdout, readyPrefix+url)
		<-ctx.Done()
		return 0
	}
	if cfg.Listen == config.DefaultListen && !portFree(cfg.Listen) {
		cfg.Listen = "127.0.0.1:0"
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err == nil {
		if f, err := os.Create(filepath.Join(cfg.DataDir, "astraterm-desktop.log")); err == nil {
			defer f.Close()
			stderr = io.MultiWriter(f, stderr) // the file first: MultiWriter stops at a failing writer
		}
	}
	log := server.NewLogger(cfg, stderr)
	cfg.Open = true
	ready := func(url string) error {
		_, err := fmt.Fprintln(stdout, readyPrefix+url)
		return err
	}
	// No startup banner: it repeats the one-time sign-in URL, which only the app needs (the ready line).
	if err := server.RunWith(ctx, cfg, log, io.Discard, ready); err != nil {
		log.Error("astraterm stopped", "err", err)
		return 1
	}
	return 0
}

// runningAt reports whether AstraTerm already answers on the loopback address listen, and its URL.
func runningAt(ctx context.Context, listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "0" {
		return "", false
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", false
	}
	url := "http://" + net.JoinHostPort(host, port)
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/auth/state", nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var state struct {
		Mode string `json:"mode"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&state) != nil || state.Mode == "" {
		return "", false
	}
	return url, true
}

// portFree reports whether addr can be listened on right now.
func portFree(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
