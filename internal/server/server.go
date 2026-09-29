// Package server assembles AstraTerm: it builds the core services (store, vault, events, audit), installs the HTTP
// middleware and authentication, mounts every module, serves the embedded SPA and runs the HTTP(S) server with
// graceful shutdown.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/pkg/browser"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/audit"
	"github.com/plzcloseyoureyes/astraterm/internal/auth"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/importer"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/recording"
	"github.com/plzcloseyoureyes/astraterm/internal/servers"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
	"github.com/plzcloseyoureyes/astraterm/internal/vault"
	"github.com/plzcloseyoureyes/astraterm/internal/webui"
)

// Server is a fully wired AstraTerm instance.
type Server struct {
	Cfg  *config.Config
	Deps *app.Deps
	Core *core.Core
	Auth *auth.Service

	log    *slog.Logger
	cancel context.CancelFunc
	vaultL *failLimiter
}

// NewLogger builds the process logger for cfg.LogLevel.
func NewLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var lvl slog.Level
	switch cfg.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	// The recording module's capture handler feeds the admin debug-log viewer (REC-9) with every record.
	return slog.New(recording.CaptureLogs(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})))
}

// New builds every core service and mounts all modules. The returned server must be closed with Close.
func New(parent context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Server{Cfg: cfg, log: log, cancel: cancel, vaultL: &failLimiter{}}
	ok := false
	defer func() {
		if !ok {
			cancel()
		}
	}()
	// A restore staged by the admin UI (importer, IMP-4) replaces the database before it is opened.
	if _, err := importer.ApplyStagedRestore(cfg, log); err != nil {
		return nil, err
	}
	st, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			st.Close()
		}
	}()
	v, err := vault.Open(ctx, st, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	router := httpx.NewRouter(httpx.Options{
		Log:            log.With("module", "http"),
		TrustedProxies: cfg.TrustedProxies,
		Dev:            cfg.Dev,
		LoopbackOnly:   cfg.ListenIsLoopback(),
		AllowedHosts:   cfg.AllowedHosts,
		TLS:            cfg.TLSEnabled(),
	})
	hub := events.NewHub(ctx, log.With("module", "events"))
	d := &app.Deps{
		Ctx:    ctx,
		Cfg:    cfg,
		Log:    log,
		Router: router,
		Store:  st,
		Vault:  v,
		Events: hub,
		Jobs:   events.NewJobs(ctx, hub, log.With("module", "jobs")),
		Audit:  audit.New(st, log.With("module", "audit")),
	}
	s.Deps = d
	v.OnChange(func(locked bool) { hub.Broadcast(vaultEvent{Type: model.EvVault, Locked: locked}) })

	if s.Auth, err = auth.Mount(d); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	s.mountCore()

	sessions := term.New(d)
	s.Core = &core.Core{Sessions: sessions, SSH: sshx.New(d, sessions)}
	mountModules(d, s.Core)

	// Modules may register migrations while mounting.
	if err := st.Migrate(ctx); err != nil {
		return nil, err
	}
	router.SetFallback(spaHandler(webui.FS()))
	ok = true
	return s, nil
}

// mountCore registers the core REST endpoints owned by the server (CRUD, settings, vault, jobs, audit, events).
func (s *Server) mountCore() {
	r := s.Deps.Router
	r.WS("/ws/events", s.Deps.Events.ServeWS)
	audit.Mount(r, s.Deps.Store)
	s.mountFolders()
	s.mountConnections()
	s.mountIdentities()
	s.mountSettings()
	s.mountVault()
	api := r.API()
	api.POST("/jobs/:id/cancel", func(c *echo.Context) error {
		if err := s.Deps.Jobs.Cancel(httpx.UserFrom(c), c.Param("id")); err != nil {
			return err
		}
		return httpx.OK(c)
	})
	api.GET("/jobs", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, s.Deps.Jobs.List(httpx.UserFrom(c)))
	})
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.Deps.Router }

// Close cancels the server context (stopping sessions, sockets and jobs) and closes the database. Closing sessions
// writes audit entries and finalizes recording rows, so the database is closed only after the session manager's
// shutdown sweep has finished (bounded, so a wedged backend cannot block the exit).
func (s *Server) Close() error {
	s.cancel()
	if s.Core != nil && s.Core.Sessions != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownSweepTimeout)
		if err := s.Core.Sessions.Wait(ctx); err != nil {
			s.log.Warn("sessions did not finish closing before the database was closed", "err", err)
		}
		cancel()
	}
	// Embedded servers stop with the context; wait so syslog files are flushed before the process exits.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownSweepTimeout)
	if err := servers.WaitShutdown(ctx, s.Deps); err != nil {
		s.log.Warn("embedded servers did not finish stopping", "err", err)
	}
	cancel()
	return s.Deps.Store.Close() // waits for in-flight queries
}

// shutdownSweepTimeout bounds how long Close waits for sessions to finish closing.
const shutdownSweepTimeout = 10 * time.Second

// Opener shows the UI at url (with its one-time setup / launch token) once AstraTerm listens and cfg.Open is set.
type Opener func(url string) error

// OpenBrowser is the default Opener: the system's default browser.
func OpenBrowser(url string) error {
	browser.Stdout, browser.Stderr = io.Discard, io.Discard
	return browser.OpenURL(url)
}

// Run starts AstraTerm and blocks until ctx is cancelled (graceful shutdown) or the listener fails. The startup banner
// goes to out; with cfg.Open the UI opens in the default browser.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger, out io.Writer) error {
	return RunWith(ctx, cfg, log, out, OpenBrowser)
}

// RunWith is Run with a custom Opener (the desktop edition shows the UI in its own window).
func RunWith(ctx context.Context, cfg *config.Config, log *slog.Logger, out io.Writer, open Opener) error {
	slog.SetDefault(log)
	s, err := New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer s.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("cannot listen on %s: address already in use (is AstraTerm already running? use --listen to pick another port)", cfg.Listen)
		}
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	scheme := "http"
	if cfg.TLSEnabled() {
		tc, err := tlsConfig(cfg)
		if err != nil {
			ln.Close()
			return err
		}
		ln = tls.NewListener(ln, tc)
		scheme = "https"
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return s.Deps.Ctx },
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	u, alsoAt := listenURLs(scheme, ln.Addr().String(), cfg.Listen, interfaceAddrs(), hostname())
	openURL := u
	if tok := s.Auth.SetupToken(); tok != "" {
		// Only the operator sees this link: first-run setup (creating the administrator) requires its token.
		openURL = u + "?setup=" + url.QueryEscape(tok)
	} else if tok := s.Auth.LaunchToken(); tok != "" {
		openURL = u + "?launch=" + url.QueryEscape(tok)
	}
	fmt.Fprintf(out, "AstraTerm %s (%s mode)\n  URL:      %s\n", cfg.Version, cfg.Mode, openURL)
	for i, a := range alsoAt {
		label := "          "
		if i == 0 {
			label = "Also at:  "
		}
		fmt.Fprintf(out, "  %s%s\n", label, a)
	}
	fmt.Fprintf(out, "  Data dir: %s\n", cfg.DataDir)
	if s.Auth.SetupToken() != "" {
		fmt.Fprintf(out, "  Setup:    open the URL above to create the administrator account (one-time setup link)\n")
	}
	if cfg.Dev {
		fmt.Fprintf(out, "  Dev mode: Vite origins %s allowed\n", strings.Join(httpx.DevOrigins, ", "))
	}
	log.Info("listening", "addr", ln.Addr().String(), "tls", cfg.TLSEnabled(), "mode", cfg.Mode)
	if cfg.Open && open != nil {
		if err := open(openURL); err != nil {
			log.Warn("could not open the UI", "err", err)
		}
	}

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.cancel() // stop sessions, sockets and jobs first so hijacked connections close
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("graceful shutdown incomplete", "err", err)
		_ = srv.Close()
	}
	return nil
}

// ResetPassword sets a user's password from the CLI (offline) and revokes their login sessions.
func ResetPassword(ctx context.Context, cfg *config.Config, username, password string) error {
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}
	st, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.Users.GetByUsername(ctx, username)
	if errors.Is(err, model.ErrNotFound) {
		return fmt.Errorf("user %q does not exist", username)
	}
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := st.Users.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	if err := st.AuthSessions.DeleteAllForUser(ctx, u.ID, ""); err != nil {
		return err
	}
	host, _ := os.Hostname()
	return st.Audit.Insert(ctx, &model.AuditEntry{UserID: u.ID, Username: u.Username, Action: "cli.reset_password",
		Target: u.ID, IP: "cli@" + host})
}

type vaultEvent struct {
	Type   string `json:"type"`
	Locked bool   `json:"locked"`
}
