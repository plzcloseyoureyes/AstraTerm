// Command nexterm runs the NexTerm remote-access workstation: a single binary serving the web UI, REST API and
// WebSockets, with every protocol handled in the Go backend.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/server"
)

// Build information, set at build time (Makefile, .goreleaser.yaml):
//
//	-ldflags "-X main.version=v1.2.3 -X main.commit=<git sha> -X main.date=<RFC 3339, UTC>"
//
// Without them (plain `go build` / `go run`) commit and date come from the Go VCS stamp when available.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-version" || args[0] == "-v") {
		cmd = "version"
	}
	switch cmd {
	case "serve":
		return serve(args, stdout, stderr)
	case "version":
		printVersion(stdout)
		return 0
	case "reset-password":
		return resetPassword(args, stdin, stdout, stderr)
	case "help":
		usage(stdout, nil)
		return 0
	default:
		fmt.Fprintf(stderr, "nexterm: unknown command %q\n\n", cmd)
		usage(stderr, nil)
		return 2
	}
}

// printVersion prints the version line (kept as the first line for scripts) followed by the build details.
func printVersion(w io.Writer) {
	c, d, dirty := commit, date, false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			case s.Key == "vcs.modified" && s.Value == "true" && commit == "":
				dirty = true
			}
		}
	}
	if c == "" {
		c = "unknown"
	} else if dirty {
		c += " (modified)"
	}
	if d == "" {
		d = "unknown"
	}
	fmt.Fprintf(w, "nexterm %s\n  commit:   %s\n  built:    %s\n  go:       %s %s/%s\n",
		version, c, d, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `NexTerm — remote access workstation (SSH, SFTP, RDP, VNC, telnet, serial, …) in your browser.

Usage:
  nexterm [serve] [flags]                       start the server (default command)
  nexterm version                               print the version
  nexterm reset-password <username> [flags]     set a user's password (prompts when --password is omitted)
  nexterm help                                  show this help

Serve flags (each can also be set with the NEXTERM_<NAME> environment variable):
`)
	if fs == nil {
		fs, _ = config.FlagSet("nexterm", w)
	}
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func serve(args []string, stdout, stderr io.Writer) int {
	fs, apply := config.FlagSet("nexterm", stderr)
	fs.Usage = func() { usage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "nexterm: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	cfg, err := apply()
	if err != nil {
		fmt.Fprintf(stderr, "nexterm: %v\n", err)
		return 1
	}
	cfg.Version = version
	log := server.NewLogger(cfg, stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, cfg, log, stdout); err != nil {
		log.Error("nexterm stopped", "err", err)
		return 1
	}
	return 0
}

func resetPassword(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexterm reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	password := fs.String("password", "", "new password (prompted when omitted; avoid: visible in the process list)")
	dataDir := fs.String("data-dir", os.Getenv("NEXTERM_DATA_DIR"), "data directory")
	portable := fs.Bool("portable", false, "portable mode (data beside the executable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: nexterm reset-password <username> [--password P] [--data-dir DIR] [--portable]")
		fs.PrintDefaults()
	}
	// Accept flags before and after the username.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	username := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "nexterm: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	dir, isPortable, err := config.ResolveDataDir(*dataDir, *portable)
	if err != nil {
		fmt.Fprintf(stderr, "nexterm: %v\n", err)
		return 1
	}
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintf(stderr, "nexterm: data directory %s not found: %v\n", dir, err)
		return 1
	}
	cfg := &config.Config{DataDir: dir, Portable: isPortable}

	pw := *password
	if pw == "" {
		if pw, err = readNewPassword(stdin, stderr); err != nil {
			fmt.Fprintf(stderr, "nexterm: %v\n", err)
			return 1
		}
	}
	if err := server.ResetPassword(context.Background(), cfg, username, pw); err != nil {
		fmt.Fprintf(stderr, "nexterm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Password for %q updated; existing login sessions were revoked.\n", username)
	return 0
}

// readNewPassword prompts twice without echo on a terminal, or reads one line from a pipe.
func readNewPassword(stdin *os.File, prompt io.Writer) (string, error) {
	fd := int(stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return "", errors.New("no password given on stdin")
		}
		return line, nil
	}
	fmt.Fprint(prompt, "New password: ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	fmt.Fprint(prompt, "Repeat password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords do not match")
	}
	return string(a), nil
}
