# AstraTerm — Architecture & Contract Specification

AstraTerm is an organized remote-management workspace. The **UI runs in the browser** (React 19 + Vite 8 +
TypeScript 7 + Tailwind 4) and **all protocol logic runs in a Go backend** (Go 1.26). `make build` compiles the
frontend into `internal/webui/dist`, which is embedded with `go:embed`, producing **one self-contained executable**
(`bin/astraterm`) for macOS / Linux / Windows. CGO is **disabled** (`CGO_ENABLED=0`) — every dependency must be pure Go.

This document is the binding contract between all contributors (human or agent). If you need to deviate, keep the
deviation local to your module and document it in your module's `README.md` section at the bottom of this file.

---------------------------------------------------------------------------------------------------------------------

## 1. Principles

1. **Backend owns connections.** The browser never talks to remote hosts directly. Remote sessions live in the Go
   process and survive browser reloads, tab closes and network blips (clients re-attach and get scrollback replayed).
2. **Everything is a tab.** Terminals, file browsers, editors, VNC/RDP viewers, tools, settings, admin pages — all are
   dock-able tabs in a split/float/pop-out capable workspace (dockview).
3. **Secrets never leave the backend in plaintext unless strictly required** (e.g. VNC password for noVNC). Secrets are
   write-only in the API; reads return `secretKeys: string[]` listing which secrets are set.
4. **Interactive auth is first-class.** Host-key verification, keyboard-interactive (2FA/OTP), missing passwords and key
   passphrases are relayed to the user through the *prompt broker* (events WebSocket) — never silently failing.
5. **Pure-Go, single binary, zero required external services.** Optional integrations (guacd for RDP, Docker socket,
   kubectl, mosh-client) are detected at runtime and degrade gracefully with actionable UI messages.
6. **Security by default.** Loopback bind, mandatory login, argon2id password hashing, encrypted secret storage,
   SameSite=Strict cookies + CSRF header, WebSocket origin checks, audit log.
7. **Two run modes** (`--mode desktop|server`, default `desktop`). *Desktop*: binds 127.0.0.1, on start prints and opens
   `http://127.0.0.1:7822/?launch=<one-time token>`; the token is exchanged (`POST /api/auth/launch`) for a session cookie of
   the first admin user (if setup is not done yet the UI shows the setup wizard first, then auto-logs in). Local shell,
   serial, embedded servers, X11-to-local-display are enabled. *Server*: multi-user; launch tokens disabled; binding a
   non-loopback address requires TLS (`--tls-*`) unless `--insecure-http`; local shell / serial / servers are admin-only.

The exhaustive feature inventory (≈300 features with IDs such as `SSH-17`, `FILE-2`, `TERM-15`) lives in
**`docs/RESEARCH.md`** together with implementation notes (§3 Protocol Strategies) and the verified library stack (§2).
Module scopes in §10 reference those IDs — read the rows for your IDs before implementing.

---------------------------------------------------------------------------------------------------------------------

## 2. Repository layout

```
cmd/astraterm/main.go            CLI entry (flags, subcommands: serve [default], version, reset-password, export)
internal/app/                  Core contracts: Deps, Router helpers, errors, context helpers, interfaces (SEE §4)
internal/model/                Shared domain structs (JSON contract types) — mirrors web/src/api/types.ts
internal/config/               Config (flags + env + data dir resolution, portable mode)
internal/store/                SQLite (modernc.org/sqlite) + migrations + repositories for all core entities
internal/vault/                Secret encryption (DEK/KEK, argon2id, XChaCha20-Poly1305), master password, lock state
internal/auth/                 Users, login, sessions (cookie), TOTP, API tokens, middleware, admin user mgmt handlers
internal/events/               Per-user event hub (/ws/events), topic subscriptions, prompt broker, job registry
internal/audit/                Audit logger + handlers
internal/server/               http.Server assembly, middleware chain, mounting of every module, SPA/static serving
internal/term/                 Runtime session manager: Session, ring buffer, attach/detach, WS protocol, OSC scanner,
                               protocol registry, recording/logging hooks
internal/sshx/                 SSH dialer & pool: auth methods, jump chains, proxies, host-key verification, keepalive,
                               algorithms, agent + agent-forwarding, exec helper, SSH terminal backend
internal/proto/telnet/         Telnet backend (RFC 854 + NAWS/TTYPE/ECHO/SGA negotiation)
internal/proto/rlogin/         Rlogin backend
internal/proto/rawtcp/         Raw TCP socket backend
internal/proto/serial/         Serial backend (go.bug.st/serial) + port enumeration
internal/proto/local/          Local shell PTY backend (creack/pty on unix, ConPTY on windows) + shell detection + WSL
internal/proto/docker/         Docker Engine API (unix socket / npipe / tcp) container list + exec backend
internal/proto/kube/           Kubernetes exec via kubectl wrapper (local PTY)
internal/proto/mosh/           Mosh via local mosh-client wrapper (detect availability)
internal/vfs/                  Virtual filesystem interface + sftp / ftp / local / s3 implementations, handle registry
internal/transfer/             Transfer manager (queue, progress events, cancel, resume, server-side copies, zip)
internal/tunnel/               Port-forward manager (local, remote, dynamic SOCKS5), persistence, status events
internal/vnc/                  VNC WebSocket<->TCP proxy (optional via SSH), for noVNC
internal/guac/                 Guacamole protocol client to guacd, WebSocket tunnel for guacamole-common-js (RDP)
internal/monitor/              Remote host monitoring via SSH exec (/proc parsing), process list/kill
internal/tools/                Network tools (ping, traceroute, port scan, DNS, whois, WoL, interfaces, listening
                               ports, HTTP check, TLS cert inspect), key generation helpers
internal/servers/              Embedded servers: HTTP file server, TFTP, SFTP/SSH, FTP — start/stop/status
internal/snippets/             Snippets + macros REST
internal/recording/            asciicast v2 recorder, text logs, recordings REST, session sharing links
internal/importer/             Import/export: ~/.ssh/config, PuTTY (.reg), MobaXterm (.mxtsessions/.ini), JSON, CSV
internal/keys/                 SSH key mgmt REST (generate/import/export incl. PuTTY PPK), identities, known hosts,
                               built-in SSH agent socket
internal/webui/                go:embed of the built SPA (dist/)
web/                           React app (see §7)
docs/SPEC.md                   This file
Makefile                       web, build, dev, test, cross-compile targets
```

---------------------------------------------------------------------------------------------------------------------

## 3. Conventions (Go)

* Module path: `github.com/plzcloseyoureyes/astraterm`. Go 1.26, `CGO_ENABLED=0`.
* Logging: `log/slog` (`app.Deps.Log`). No `fmt.Println` in library code.
* HTTP: [Echo v5](https://github.com/labstack/echo) (`github.com/labstack/echo/v5`) behind `internal/httpx` (§4 "Router
  helpers"). Routes are registered on the router's groups — `d.Router.API().GET("/connections/:id", h.get)` — with Echo
  path syntax (`:id`, `*`). Handlers are `func(c *echo.Context) error`: `c.Param("id")`, `c.QueryParam("q")`,
  `httpx.Bind(c, &v)` for JSON bodies, `httpx.UserFrom(c)` for the caller, `c.JSON(status, v)` / `httpx.OK(c)` to reply.
  Code below the HTTP layer takes a `context.Context` (`c.Request().Context()`), which carries the user and client IP.
* JSON: camelCase field names, `omitempty` for optional fields, times as RFC3339 (`time.Time`).
* IDs: `model.NewID()` → 20-char lowercase base32 random string. All primary keys are TEXT.
* Errors: handlers simply `return err` — typed errors `httpx.ErrNotFound`, `httpx.ErrForbidden`, `httpx.BadRequest(msg)`,
  `httpx.Conflict(msg)`, `httpx.ErrLocked` (vault locked → HTTP 423), `model.Err*`; the router's error handler renders
  `{"error": "message", "code": "not_found"}` (unknown errors → 500 `internal`, details only in the log).
* Context: every blocking call takes `context.Context`. Long-lived goroutines are tied to a manager context cancelled on
  shutdown (`app.Deps.Ctx`).
* Concurrency: guard shared maps with mutexes; never block the events hub on a slow client (drop/close slow clients).
* Auth: all `/api/**` except `/api/auth/state`, `/api/auth/login`, `/api/auth/setup`, `/api/share/**` require an
  authenticated user (`httpx.UserFrom(c)`; routes on `d.Router.API()`). Mutating requests (POST/PUT/PATCH/DELETE) must
  carry header `X-AstraTerm: 1` (CSRF guard; checked by middleware). Admin-only routes are registered on `d.Router.Admin()`.
* Ownership/visibility: user-owned rows have `ownerId`. A user sees their own rows + rows with `shared=true`
  (shared connections are read-only for non-owners unless admin; their secrets are *used* server-side but never returned).
* Module wiring: every module exposes `func Mount(d *app.Deps, ...managers it needs)` which registers routes with `d.Router` and may register
  term protocols / vfs drivers / migrations. `internal/server` calls every `Mount` in a fixed order.
* Migrations: `store.RegisterMigration(module string, version int, sql string)` — applied in (module, version) order at
  startup, tracked in `schema_migrations(module, version)`. Core tables are created by module `core` (store package).
* Tests: `go test ./...` must pass. Put unit tests next to code. Integration tests needing network targets are guarded by
  env vars (e.g. `ASTRATERM_TEST_SSH=user:pass@127.0.0.1:2222`).

---------------------------------------------------------------------------------------------------------------------

## 4. Core backend contracts

### 4.0 Package dependency DAG (imports may only point "down" this list — no cycles)
```
model      (leaf)   domain/JSON structs, NewID()
config     (leaf)   Config struct, Load(flags/env), DataDir helpers
httpx      (model)  Router (Echo v5: middleware, route groups, error handler), Bind/OK helpers, typed errors,
                    UserFrom/WithUser ctx helpers, WS accept helper
store      (model, config)            SQLite open + migrations + repositories (Users, AuthSessions, Folders, Connections,
                                      Identities, Keys, KnownHosts, Snippets, Macros, Tunnels, Settings, Audit,
                                      Recordings, ShareLinks, APITokens, VaultMeta). Stores ciphertext as []byte.
vault      (store)                    Seal/Open bytes, SealJSON/OpenJSON(map[string]string), master password, lock/unlock
events     (model, httpx)             Hub (per-user fan-out, /ws/events handler), Prompt broker, Jobs, topic subscriptions
audit      (model, store, httpx)      Logger + handlers
app        (all of the above)         Deps struct + Resolve helpers (connection+identity+decrypted secrets, key material)
auth       (app)                      login/setup/totp/tokens/users + middleware installed into httpx.Router
term       (app)                      runtime session manager + /ws/terminal + /api/sessions + protocol registry
sshx       (app, term)                SSH dial/pool/hostkeys/agent + registers protocol "ssh"
proto/*    (app, term[, sshx])        telnet, rlogin, raw, serial, local, docker, kube, mosh
vfs        (app[, sshx])              VFS registry + drivers (local, sftp, ftp, s3) + /api/fs handlers
transfer   (app, vfs)                 transfer queue + /api/transfers
tunnel, monitor, vnc, guac, keys, snippets, recording, importer, tools, servers   (app, term, sshx, vfs as needed)
server     (everything)               builds Deps, installs middleware, calls every module's Mount, serves SPA
cmd/astraterm (server, config)
```

### 4.1 `internal/app.Deps`
```go
type Deps struct {
    Ctx    context.Context   // cancelled on shutdown
    Cfg    *config.Config
    Log    *slog.Logger
    Router *httpx.Router
    Store  *store.Store
    Vault  *vault.Vault
    Events *events.Hub       // Publish(userID, ev), Broadcast(ev), Prompt(ctx, userID, Prompt) (PromptResponse, error), Subscribe hooks
    Jobs   *events.Jobs      // Start(userID, func(ctx, emit func(any)) error) jobID; Cancel(jobID)
    Audit  *audit.Logger     // Log(c *echo.Context | *http.Request | ctx, action, target string, details any)
}
// Resolve loads a connection visible to user, merges its identity (username/key/secrets), decrypts secrets.
func (d *Deps) ResolveConnection(ctx context.Context, user *model.User, connID string) (*model.Connection, map[string]string, error)
// KeyMaterial returns the decrypted private key PEM + passphrase for a stored key visible to user.
func (d *Deps) KeyMaterial(ctx context.Context, user *model.User, keyID string) (pem []byte, passphrase string, err error)
```
Higher-level managers are constructed in `internal/server` and passed explicitly to the modules that need them, e.g.
`term.New(d) *term.Manager`, `sshx.New(d, sessions) *sshx.Pool`, `vfs.New(d, pool) *vfs.Registry`,
`transfer.New(d, fsreg)`, `tunnel.New(d, pool)`, and each module exposes `Mount(...)` taking what it needs.

### Router helpers (`internal/httpx`, Echo v5)
`d.Router` wraps an `*echo.Echo` (`github.com/labstack/echo/v5`). Route paths use Echo syntax (`:id` parameters, `*`
wildcards); group paths are relative to `/api`. `Public()` / `API()` / `Admin()` return a new group on every call, so a
module's `Use` or sub-groups only affect the routes it registers through that group.
```go
type Router struct { /* wraps *echo.Echo */ }
func NewRouter(opts Options) *Router
func (r *Router) Echo() *echo.Echo                         // escape hatch: routes outside /api and /ws (global middleware, no auth requirement)
func (r *Router) Public() *echo.Group                      // "/api", no auth required (the user is still resolved when present)
func (r *Router) API() *echo.Group                         // "/api", authenticated user required (401)
func (r *Router) Admin() *echo.Group                       // "/api", admin required (401 anonymous / 403 non-admin)
func (r *Router) WS(path string, h echo.HandlerFunc)       // GET, authenticated, Origin-checked WebSocket route, e.g. "/ws/terminal/:id"
func (r *Router) PublicWS(path string, h echo.HandlerFunc) // GET, unauthenticated (share links), Origin-checked
func (r *Router) SetAuthenticator(a Authenticator)         // auth module: Authenticate(c *echo.Context) (*model.User, AuthInfo, error)
func (r *Router) SetFallback(h echo.HandlerFunc)           // server: SPA/static handler for non-/api, non-/ws GET/HEAD
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request)

func UserFrom(c *echo.Context) *model.User                 // nil when anonymous
func UserFromContext(ctx context.Context) *model.User      // below the HTTP layer (the request ctx carries the user)
func AuthInfoFrom(c *echo.Context) AuthInfo                // AuthInfoFromContext(ctx) below the HTTP layer
func ClientIP(c *echo.Context) string                      // trusted-proxy aware; ClientIPFrom(ctx) below the HTTP layer
func Bind(c *echo.Context, v any) error                    // JSON body ≤ 2 MiB → typed 400 / 413; c.Bind(&v) behaves the same
func BindOptional(c *echo.Context, v any) error            // empty body allowed (v untouched)
func BindLimit(c *echo.Context, v any, limit int64) error  // Bind with another size limit
func OK(c *echo.Context) error                             // 200 {"ok":true} (mutations without a resource body)
func AcceptWS(c *echo.Context, opts *websocket.AcceptOptions) (*websocket.Conn, error) // coder/websocket, 16 MiB read limit
func BodyLimit(n int64) echo.MiddlewareFunc                // route: raw request body cap instead of 2 MiB (n ≤ 0: none)
var RequireUser, RequireAdmin echo.MiddlewareFunc          // for groups/routes built on Echo()
```
* **Handlers** `return err`: `*httpx.HTTPError` (`ErrNotFound`, `BadRequest(msg)`, `Conflict(msg)`, `Forbidden(msg)`,
  `Unauthorized(code, msg)`, `TooManyRequests(msg, sec)` + Retry-After, `ErrLocked` 423, `Internal(err)`), errors with
  `ErrorCode()` (`model.Err*`) and Echo's own errors (no route, 405, 413) become `{error, code}`; anything else is a
  logged 500 `{"error":"internal server error","code":"internal"}`. Nothing is written once the response is committed
  (streams, WebSockets). `c.JSON` sends `application/json; charset=utf-8` and `Cache-Control: no-store`.
* **Middleware** (outermost first): Pre — request log (slog, share tokens redacted), recover, security headers
  (CSP/HSTS/nosniff/frame), Host guard (loopback binds); routing; Use — authentication (cookie / Bearer, stored in the
  request context), CSRF (`X-AstraTerm: 1` on mutating requests unless Bearer), canonical-path redirect, body limit; then
  the route's Origin check (WS) and `RequireUser` / `RequireAdmin`. Unmatched `/api` and `/ws` paths → JSON 404
  `no such endpoint`; other methods than GET/HEAD elsewhere → JSON 405; everything else → the SPA fallback.

```go
type handler struct{ d *app.Deps; c *core.Core }

func Mount(d *app.Deps, c *core.Core) error {
	h := &handler{d: d, c: c}
	api := d.Router.API()
	api.GET("/tunnels", h.list)
	api.POST("/tunnels/:id/start", h.start)
	d.Router.WS("/ws/tunnels/:id", h.watch)
	return nil
}

func (h *handler) list(c *echo.Context) error {
	list, err := h.d.Store.Tunnels.ListByOwner(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err // rendered by the router's error handler
	}
	return c.JSON(http.StatusOK, list)
}

func (h *handler) start(c *echo.Context) error {
	var req struct {
		Force bool `json:"force"`
	}
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	t, err := h.d.Store.Tunnels.Get(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err // model.ErrNotFound → 404 {code:"not_found"}
	}
	// … start it …
	h.d.Audit.Log(c, "tunnel.start", t.ID, nil)
	return httpx.OK(c)
}

func (h *handler) watch(c *echo.Context) error {
	ws, err := httpx.AcceptWS(c, nil)
	if err != nil {
		return nil // the handshake error response has been written
	}
	defer ws.CloseNow()
	// … c.Request().Context() lives until the handler returns …
	return nil
}
```

### Prompt broker
```go
type Prompt struct {           // JSON contract, see §6.1
    ID, Kind, Title, Message, SessionID, ConnectionID string
    Fields   []PromptField     // {label, echo bool, value?}
    HostKey  *HostKeyInfo      // for kind=hostkey
    AllowSave bool             // offer "remember" checkbox (save password / trust host)
}
type PromptResponse struct { Accept bool; Values []string; Save bool }
// Blocks until a response, ctx done, or timeout (default 3 minutes). Returns events.ErrNoInteractiveClient if the user
// has no connected events socket.
func (h *Hub) Prompt(ctx context.Context, userID string, p Prompt) (PromptResponse, error)
```

### Terminal protocol registry (`internal/term`)
```go
type Backend interface {
    io.Reader                         // output from remote (blocking)
    io.Writer                         // input to remote
    Resize(cols, rows int) error
    Close() error
}
// Optional capabilities discovered by type assertion:
type Breaker interface { SendBreak() error }              // serial/telnet break
type Signaler interface { Signal(name string) error }     // local/ssh signals (INT, TERM, KILL)
type ExitCoder interface { ExitCode() int }

type OpenRequest struct {
    Session    *Session          // runtime session being opened (ID, Owner, Cols, Rows, Protocol, ...)
    Connection *model.Connection // resolved saved connection (or synthesized for quick-connect)
    Secrets    map[string]string // decrypted secrets of the connection (+ identity)
    User       *model.User
}
type Opener func(ctx context.Context, req OpenRequest) (Backend, error)
func RegisterProtocol(protocol string, open Opener)
```
The manager: creates Session → runs opener in a goroutine (state `connecting`) → pumps backend output into the offset
ring buffer (§6.2), attached WebSocket clients, recorder (asciicast v3) and text logger, and an OSC scanner (OSC 0/2
title, OSC 7 cwd, OSC 133 prompt marks, BEL). Sessions persist when no client is attached; detached sessions are reaped
after `Cfg.DetachedSessionTTL` (default 24h; 0 = never). `reconnect` re-runs the opener on the same session id (and
`autoReconnect` does so automatically with backoff). Graphical sessions (`vnc`, `rdp`) are also `Session`s
(kind != terminal) created through the same `POST /api/sessions`; they have no Backend — their module owns the WS and
reports state through `Manager.SetState`.

```go
func New(d *app.Deps) *Manager
func (m *Manager) Create(ctx context.Context, user *model.User, req CreateRequest) (*Session, error)
func (m *Manager) Get(id string) *Session                 // nil if unknown
func (m *Manager) List(user *model.User, all bool) []*Session
func (m *Manager) Write(id string, data []byte) error      // inject input (snippets, multi-exec, logon actions, macros)
func (m *Manager) Close(id string) error
func (m *Manager) SetState(id string, st model.SessionState, msg string)
func (m *Manager) Attach(ctx context.Context, id string, c *websocket.Conn, opts AttachOptions) error // used by /ws/terminal & /ws/share
// Hooks let other modules observe sessions without editing term (logon actions, triggers, recording, audit, monitor):
type Hooks struct {
    OnState  func(s *Session, st model.SessionState)
    OnOutput func(s *Session, data []byte) // called synchronously from the pump: MUST be fast & non-blocking
    OnInput  func(s *Session, data []byte)
    OnClose  func(s *Session)
}
func (m *Manager) AddHooks(h Hooks) (remove func())
func (s *Session) Info() model.RuntimeSession
```

**Vault keys.** The vault holds two keys: the *system key* (random, stored in `<data>/system.key`, 0600) used for
server-internal secrets that must be usable before anyone unlocks anything (TOTP secrets, share tokens, launch tokens), and
the *data key* (DEK) that encrypts user secrets (connection/identity secrets, private keys, passphrases). The DEK is
wrapped either by the system key (no master password) or by an argon2id-derived KEK from the master password (then the
vault starts **locked** after each restart until an admin unlocks it; `ResolveConnection` returns `httpx.ErrLocked` → UI
shows the unlock dialog). Connections without secrets keep working while locked.

### SSH pool (`internal/sshx`)
```go
func New(d *app.Deps, sessions *term.Manager) *Pool      // registers protocol "ssh" (and "sftp" file-only kind)
// Get returns a shared, ref-counted *Client for a saved connection (dialing through jump hosts / proxies, verifying host
// keys and prompting as needed). Always call release() when done. Clients close after the last release + idle TTL (60s).
func (p *Pool) Get(ctx context.Context, user *model.User, connID string) (c *Client, release func(), err error)
// ForSession returns the SSH client used by a live runtime session (works for quick-connect sessions too), so the SFTP
// side panel, monitoring and "open tunnel from session" reuse the terminal's connection (no second 2FA prompt).
func (p *Pool) ForSession(ctx context.Context, user *model.User, sessionID string) (c *Client, release func(), err error)
// Acquire dials (or reuses) an ad-hoc connection spec (quick connect, jump hops); keyed by user+host+port+username.
func (p *Pool) Acquire(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (c *Client, release func(), err error)
type Client struct { *ssh.Client; Conn *model.Connection; /* ... */ }
func (c *Client) Exec(ctx context.Context, cmd string) (stdout, stderr []byte, exitCode int, err error)
func (c *Client) SFTP() (*sftp.Client, error)              // lazily created, shared, reconnect-aware
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) // direct-tcpip
func (c *Client) Info() model.SSHConnInfo                  // server version, negotiated algorithms, host key fp, latency
```
Generic network dialing for every protocol (telnet, raw, vnc, rdp, ftp...) goes through
`sshx.Dialer(ctx, user, conn)` which applies `options.proxy` and `options.jumpHosts` / `sshTunnelVia` so every
protocol can be reached through proxies and SSH gateways (RESEARCH "one Dialer abstraction").

---------------------------------------------------------------------------------------------------------------------

## 5. Data model

JSON contract types live in `internal/model` (Go) and `web/src/api/types.ts` (TS). They must match field-for-field.

### 5.1 Tables (module `core`, created by `internal/store`)

| table | columns |
|---|---|
| users | id, username UNIQUE, display_name, password_hash, role ('admin'\|'user'), totp_secret_enc, totp_enabled, disabled, created_at, updated_at, last_login_at |
| auth_sessions | id (sha256 of token), user_id, created_at, expires_at, last_seen_at, ip, user_agent |
| api_tokens | id, user_id, name, token_hash, created_at, last_used_at, expires_at |
| folders | id, owner_id, parent_id NULL, name, color, icon, sort_order, shared, created_at, updated_at |
| connections | id, owner_id, folder_id NULL, name, protocol, host, port, username, identity_id NULL, key_id NULL, auth_method, color, icon, tags (JSON), notes, favorite, sort_order, options (JSON), secrets_enc (encrypted JSON map), shared, last_used_at, created_at, updated_at |
| identities | id, owner_id, name, username, key_id NULL, secrets_enc, created_at, updated_at |
| ssh_keys | id, owner_id, name, type, bits, public_key, private_key_enc, passphrase_enc, fingerprint, comment, certificate, created_at |
| known_hosts | id, host, port, key_type, public_key, fingerprint, comment, created_at (global, shared by all users) |
| snippets | id, owner_id, name, folder, description, content, tags (JSON), send_mode ('paste'\|'execute'), shortcut, created_at, updated_at |
| macros | id, owner_id, name, steps (JSON [{data, delayMs}]), created_at, updated_at |
| tunnels | id, owner_id, name, type ('local'\|'remote'\|'dynamic'), connection_id, bind_host, bind_port, dest_host, dest_port, auto_start, created_at, updated_at |
| settings | scope ('global' or user id), key, value (JSON), updated_at — PRIMARY KEY(scope, key) |
| audit_log | id INTEGER PK AUTOINCREMENT, ts, user_id, username, action, target, details (JSON), ip |
| recordings | id, owner_id, session_id, connection_id, title, kind ('asciicast'\|'log'), path, size, cols, rows, started_at, ended_at |
| share_links | id, token_hash, session_id, owner_id, mode ('read'\|'write'), created_at, expires_at |
| vault_meta | key, value (DEK wrapped with KEK; KDF params; verifier) |

### 5.2 Core JSON types (authoritative field names)

```ts
type Role = 'admin' | 'user'
interface User { id: string; username: string; displayName: string; role: Role; totpEnabled: boolean; disabled: boolean; createdAt: string; lastLoginAt?: string }

type Protocol = 'ssh' | 'telnet' | 'rlogin' | 'raw' | 'serial' | 'local' | 'mosh' | 'docker' | 'kube'
              | 'sftp' | 'ftp' | 's3' | 'vnc' | 'rdp' | 'winrm' | 'ipmi' | 'web'   // 'web' = saved browser session (webproxy)
interface Folder { id: string; parentId?: string | null; name: string; color?: string; icon?: string; sortOrder: number; shared: boolean; ownerId: string }
interface Connection {
  id: string; folderId?: string | null; name: string; protocol: Protocol
  host: string; port: number; username: string
  identityId?: string | null; keyId?: string | null
  authMethod: 'auto' | 'password' | 'key' | 'agent' | 'keyboard-interactive' | 'none'
  color?: string; icon?: string; tags: string[]; notes: string; favorite: boolean; sortOrder: number
  options: ConnectionOptions          // protocol specific, see §5.3
  secrets?: Record<string, string>    // WRITE-ONLY. Omitted key = unchanged, "" = delete
  secretKeys: string[]                // READ-ONLY. e.g. ["password","passphrase"]
  shared: boolean; ownerId: string
  lastUsedAt?: string; createdAt: string; updatedAt: string
}
interface Identity { id: string; name: string; username: string; keyId?: string | null; secrets?: Record<string,string>; secretKeys: string[]; createdAt: string; updatedAt: string }
interface SSHKey { id: string; name: string; type: string; bits: number; publicKey: string; fingerprint: string; comment: string; hasPassphrase: boolean; certificate?: string; createdAt: string }
interface KnownHost { id: string; host: string; port: number; keyType: string; publicKey: string; fingerprint: string; comment: string; createdAt: string }
interface Snippet { id: string; name: string; folder: string; description: string; content: string; tags: string[]; sendMode: 'paste' | 'execute'; shortcut?: string; createdAt: string; updatedAt: string }
interface Macro { id: string; name: string; steps: { data: string; delayMs: number }[]; createdAt: string; updatedAt: string }
interface Tunnel { id: string; name: string; type: 'local' | 'remote' | 'dynamic'; connectionId: string; bindHost: string; bindPort: number; destHost: string; destPort: number; autoStart: boolean; status: TunnelStatus; createdAt: string; updatedAt: string }
interface TunnelStatus { state: 'stopped' | 'starting' | 'running' | 'error'; error?: string; activeConns: number; totalConns: number; bytesIn: number; bytesOut: number; startedAt?: string }

type SessionKind = 'terminal' | 'vnc' | 'rdp'
type SessionState = 'connecting' | 'authenticating' | 'connected' | 'disconnected' | 'closed' | 'error'
interface RuntimeSession {
  id: string; kind: SessionKind; protocol: Protocol; connectionId?: string; title: string
  host?: string; username?: string; state: SessionState; stateMessage?: string; exitCode?: number
  cols: number; rows: number; clients: number; cwd?: string
  recording: boolean; recordingId?: string; logging: boolean
  ownerId: string; createdAt: string; connectedAt?: string
}
```

### 5.3 `ConnectionOptions` (single flat object; each protocol reads its keys, unknown keys are preserved)

Common: `term` (default `xterm-256color`), `encoding` (default `utf-8`), `backspace` (`del`|`ctrl-h`),
`startupCommand` (sent after connect), `autoReconnect` (bool), `record` (bool: asciicast), `log` (bool: text log),
`terminal` ({fontFamily, fontSize, theme, cursorStyle, scrollback, ...} overrides), `keepAliveSec`.

| protocol | keys |
|---|---|
| ssh | `jumpHosts: string[]` (connection IDs, first hop first) · `proxy: {type:'none'\|'socks5'\|'socks4'\|'http', host, port, username}` (password in secrets.proxyPassword) · `agentForwarding` · `compression` · `x11Forwarding` · `env: Record<string,string>` · `remoteCommand` · `followCwd` (SFTP panel follows terminal cwd) · `monitoring` (status bar stats, default true) · `sftpRoot` · `ciphers`, `kex`, `macs`, `hostKeyAlgorithms` (string[]) · `legacyAlgorithms` · `connectTimeoutSec` · `portKnock: {port:number, proto:'tcp'\|'udp'}[]` · `useAgent` (use local ssh-agent / pageant) |
| telnet | `term`, `encoding`, `backspace`, `negotiate` (bool, default true) |
| rlogin / raw | `encoding`, `localEcho`, `lineEnding` (`crlf`\|`lf`\|`cr`) |
| serial | `device`, `baud` (9600), `dataBits` (8), `parity` (`none`\|`odd`\|`even`\|`mark`\|`space`), `stopBits` (`1`\|`1.5`\|`2`), `flowControl` (`none`\|`rtscts`\|`xonxoff`), `localEcho`, `lineEnding`, `hexView` |
| local | `shell` (path or id from /api/local/shells), `args: string[]`, `cwd`, `env` |
| mosh | ssh keys + `moshPorts` (e.g. `60000:61000`), `predict` (`adaptive`\|`always`\|`never`) |
| docker | `dockerHost` (default local socket), `container`, `shell` (default `/bin/sh`), `user`, `viaConnectionId` (docker over SSH) |
| kube | `context`, `namespace`, `pod`, `container`, `shell` |
| sftp / ftp / s3 | ftp: `ftpTls` (`none`\|`explicit`\|`implicit`), `passive` (true), `insecureTls`; s3: `endpoint`, `region`, `bucket`, `pathStyle`, `accessKeyId` (secret: `secretAccessKey`) ; all: `initialPath` |
| vnc | `viewOnly`, `quality` (0-9), `compression` (0-9), `shared` (true), `sshTunnelVia` (connection id), `scaling` (`fit`\|`remote-resize`\|`none`) (secret: `vncPassword`) |
| rdp | `rdpEngine` (`ironrdp`\|`guacd`, default `ironrdp`), `domain`, `security` (`any`\|`nla`\|`tls`\|`rdp`\|`vmconnect`), `ignoreCert`, `width`, `height`, `dpi`, `colorDepth` (8/16/24/32), `resizeMethod` (`display-update`\|`reconnect`), `enableAudio`, `enableMic`, `enableDrive`, `driveName`, `enablePrinting`, `disableClipboard`, `console`, `initialProgram`, `serverLayout`, `timezone`, `gatewayHost/Port/Username/Domain` (secret `gatewayPassword`), `sshTunnelVia`, `enableWallpaper`, `enableTheming`, `enableFontSmoothing`, `recording` |

Known secret keys: `password`, `passphrase`, `proxyPassword`, `vncPassword`, `secretAccessKey`, `gatewayPassword`,
`sudoPassword`.

---------------------------------------------------------------------------------------------------------------------

## 6. API

All JSON. Base path `/api`. Mutating calls require header `X-AstraTerm: 1`. Errors: `{error, code}`.

### 6.0 REST endpoint catalogue

**Auth & account** (`internal/auth`)
- `GET /api/auth/state` → `{setupRequired, authenticated, user?, mode:'desktop'|'server', vaultLocked, vaultHasMasterPassword, version, features: {guacd:boolean, docker:boolean, kubectl:boolean, mosh:boolean, wsl:boolean}}`
- `POST /api/auth/setup` `{username, password, displayName?}` → creates first admin, logs in
- `POST /api/auth/login` `{username, password, totp?, remember?}` → `{user}` | 401 `{code:'totp_required'}`
- `POST /api/auth/logout`
- `POST /api/auth/launch {token}` → desktop-mode one-time launch token exchange (logs in as first admin)
- `POST /api/auth/password` `{currentPassword, newPassword}`
- `POST /api/auth/totp/setup` → `{secret, otpauthUrl}` ; `POST /api/auth/totp/enable` `{code}` ; `POST /api/auth/totp/disable` `{password}`
- `GET/POST/DELETE /api/auth/tokens[/{id}]` API tokens (Bearer auth)
- `GET /api/auth/sessions`, `DELETE /api/auth/sessions/{id}` (browser login sessions)
- Admin: `GET/POST /api/admin/users`, `PATCH/DELETE /api/admin/users/{id}`, `POST /api/admin/users/{id}/reset-password`

**Vault** (`internal/vault` handlers) — `GET /api/vault/status` → `{locked, hasMasterPassword}` · `POST /api/vault/unlock {password}` ·
`POST /api/vault/lock` · `POST /api/vault/master-password {currentPassword?, newPassword}` (empty newPassword removes) — admin only.

**Settings** — `GET /api/settings` → user settings object merged over global · `PUT /api/settings` (partial merge) ·
`GET/PUT /api/admin/settings` (global).

**Folders / Connections / Identities** (`internal/store` + handlers in `internal/server/crud*.go`)
- `GET /api/folders`, `POST /api/folders`, `PATCH /api/folders/{id}`, `DELETE /api/folders/{id}?recursive=1`
- `GET /api/connections` (list, visible to user), `POST`, `GET/PATCH/DELETE /api/connections/{id}`,
  `POST /api/connections/{id}/duplicate`, `POST /api/connections/reorder` `{items:[{id, folderId, sortOrder}]}`,
  `POST /api/connections/bulk-delete` `{ids}`
- `GET/POST /api/identities`, `PATCH/DELETE /api/identities/{id}`

**Keys / known hosts / agent** (`internal/keys`)
- `GET /api/keys`, `POST /api/keys/generate {name, type:'ed25519'|'rsa'|'ecdsa', bits?, comment?, passphrase?}`,
  `POST /api/keys/import {name, privateKey, passphrase?}` (OpenSSH, PEM, PuTTY PPK v2/v3), `PATCH/DELETE /api/keys/{id}`,
  `GET /api/keys/{id}/export?format=openssh|ppk|public` (returns text)
  `POST /api/keys/{id}/install {connectionId}` (append public key to remote ~/.ssh/authorized_keys = ssh-copy-id)
- `GET /api/known-hosts`, `POST /api/known-hosts`, `DELETE /api/known-hosts/{id}`, `POST /api/known-hosts/import {text}` (OpenSSH known_hosts format)
- `GET /api/agent/status`, `POST /api/agent/start`, `POST /api/agent/stop` (built-in agent socket exposing stored keys)

**Runtime sessions** (`internal/term`)
- `GET /api/sessions` → `RuntimeSession[]` (own; admin: `?all=1`)
- `POST /api/sessions` `{connectionId? , quick?: Partial<Connection> & {password?: string}, cols, rows, title?}` → `RuntimeSession`
  (kind derived from protocol; opener runs async; prompts via events)
- `GET /api/sessions/{id}`, `DELETE /api/sessions/{id}` (close), `POST /api/sessions/{id}/reconnect`,
  `PATCH /api/sessions/{id}` `{title?}`, `POST /api/sessions/{id}/input` `{data}` (used by snippets/multi-exec APIs),
  `POST /api/sessions/{id}/signal {name}`, `POST /api/sessions/{id}/break`,
  `POST /api/sessions/{id}/record {enabled}`, `POST /api/sessions/{id}/log {enabled}`,
  `GET /api/sessions/{id}/scrollback` → text/plain (ring buffer as plain text, ANSI stripped with `?raw=0`)
- `POST /api/sessions/{id}/share {mode, expiresInSec}` → `{token, url}`; `GET /api/share/{token}` (public: session meta);
  WS `/ws/share/{token}` (read-only or read-write attach)
- `GET /api/local/shells` → `[{id, name, path, args}]` ; `GET /api/serial/ports` → `[{name, description, vid, pid, serial}]`
- `GET /api/docker/containers?host=` → list ; `GET /api/kube/contexts`, `/api/kube/pods?context=&namespace=`

**Files** (`internal/vfs`, `internal/transfer`)
- `POST /api/fs` `{sessionId?|connectionId?|local?:true}` → `{id, kind:'sftp'|'ftp'|'local'|'s3', home, root, label, capabilities: {chmod, chown, symlink, exec, checksum}}`
- `DELETE /api/fs/{id}`
- `GET /api/fs/{id}/list?path=` → `{path, parent, entries: FileEntry[]}`
- `GET /api/fs/{id}/stat?path=` → `FileEntry` ; `GET /api/fs/{id}/realpath?path=`
- `GET /api/fs/{id}/download?path=[&paths=..&zip=1]` → stream (Range supported for files; dirs/multiple → zip)
- `PUT /api/fs/{id}/upload?path=<full target path>&offset=<n>&mtime=<unix>` raw body → `{size}` (chunked/resumable)
- `GET /api/fs/{id}/read?path=&maxBytes=` → `{content (utf-8 or base64), encoding:'utf-8'|'base64', size, mtime, mode}`
- `PUT /api/fs/{id}/write` `{path, content, encoding, expectMtime?}` → `FileEntry` (409 if changed remotely)
- `POST /api/fs/{id}/mkdir {path, parents?}` · `/rename {from, to}` · `/delete {paths, recursive}` · `/chmod {paths, mode, recursive}`
  · `/chown {paths, uid?, gid?, recursive}` · `/symlink {target, link}` · `/touch {path}` · `/copy {from:[..], toDir}`
  (same-fs copy; exec `cp -a` when available else stream) · `/checksum {path, algo:'md5'|'sha1'|'sha256'}` · `/search {path, pattern, maxResults}`
  · `/archive {paths, dest, format:'zip'|'tar.gz'}` · `/extract {path, destDir}` (exec-capable fs only)
- `GET /api/transfers` → `Transfer[]` ; `POST /api/transfers` `{srcFs, srcPaths[], dstFs, dstDir, overwrite:'ask'|'overwrite'|'skip'|'resume'|'rename'}` → `Transfer`
  ; `POST /api/transfers/{id}/cancel` ; `DELETE /api/transfers/{id}` (clear finished)

```ts
interface FileEntry { name: string; path: string; type: 'file' | 'dir' | 'symlink' | 'other'; size: number; mode: number; perm: string /* "drwxr-xr-x" */; mtime: string; owner?: string; group?: string; uid?: number; gid?: number; linkTarget?: string; linkType?: 'file' | 'dir' | 'broken'; hidden: boolean }
interface Transfer { id: string; srcFs: string; dstFs: string; srcPaths: string[]; dstDir: string; label: string; state: 'queued' | 'running' | 'done' | 'error' | 'canceled'; error?: string; totalBytes: number; doneBytes: number; totalFiles: number; doneFiles: number; currentFile?: string; bytesPerSec: number; createdAt: string; finishedAt?: string }
```

**Tunnels** (`internal/tunnel`) — `GET/POST /api/tunnels`, `PATCH/DELETE /api/tunnels/{id}`, `POST /api/tunnels/{id}/start`, `POST /api/tunnels/{id}/stop`.

**Snippets & macros** (`internal/snippets`) — `GET/POST /api/snippets`, `PATCH/DELETE /api/snippets/{id}`; `GET/POST /api/macros`, `PATCH/DELETE /api/macros/{id}`;
`POST /api/snippets/{id}/run {sessionIds[], variables}` (server-side send to multiple sessions).

**Monitoring** (`internal/monitor`) — subscribe over events (`{type:'subscribe', topic:'monitor', sessionId}`);
`GET /api/monitor/{sessionId}/processes` → `Process[]`; `POST /api/monitor/{sessionId}/kill {pid, signal}`;
`GET /api/monitor/{sessionId}/snapshot` → `MonitorStats`.

```ts
interface MonitorStats { ts: string; os: string; hostname: string; kernel: string; uptimeSec: number; load: [number, number, number]
  cpu: { usage: number; cores: number; perCore: number[] }; mem: { total: number; used: number; available: number; swapTotal: number; swapUsed: number }
  disks: { mount: string; fs: string; total: number; used: number }[]; net: { iface: string; rxBps: number; txBps: number; rxTotal: number; txTotal: number }[]
  users: number; processes: number }
interface Process { pid: number; ppid: number; user: string; cpu: number; mem: number; rss: number; state: string; started: string; command: string }
```

**Graphical** — see §6.3. `POST /api/sessions/{id}/rdp-ticket`, `GET /api/guacd/status` → `{configured, reachable, version?}`.

**Recording** (`internal/recording`) — `GET /api/recordings`, `GET /api/recordings/{id}` meta, `GET /api/recordings/{id}/file` (asciicast / log),
`DELETE /api/recordings/{id}`.

**Tools** (`internal/tools`) — jobs: `POST /api/tools/{tool}` → `{jobId}` with results streamed as `job` events; `POST /api/jobs/{id}/cancel`.
Tools: `ping {host, count, intervalMs}`, `traceroute {host, maxHops}`, `portscan {host(s)/cidr, ports:"22,80,1000-2000", timeoutMs, concurrency}`,
`dns {name, type}`, `whois {query}`, `wol {mac, broadcast, port}`, `httpcheck {url, method}`, `tlscert {host, port}`, `netscan {cidr}` (ping sweep + common ports),
sync: `GET /api/tools/interfaces`, `GET /api/tools/listening` (local listening ports).

**Embedded servers** (`internal/servers`) — `GET /api/servers` → `ServerStatus[]`; `PUT /api/servers/{kind}` config; `POST /api/servers/{kind}/start|stop`.
Kinds: `http`, `tftp`, `sftp`, `ftp`. `ServerStatus {kind, running, config, error?, addr?, clients, startedAt?}`.

**Import / export** (`internal/importer`) — `POST /api/import/preview {format, content (text or base64)}` → `{connections, folders, warnings}`;
`POST /api/import/commit {format, content, targetFolderId?}` → `{created}`; formats: `ssh_config`, `putty_reg`, `mobaxterm`, `json`, `csv`, `known_hosts`, `termius_csv`.
`GET /api/export?format=json&includeSecrets=0` → file.

**Audit** — `GET /api/admin/audit?limit=&before=&userId=&action=` (admin), `GET /api/audit/me`.

### 6.1 Events WebSocket `/ws/events`

Text JSON frames. Server → client:
```ts
type ServerEvent =
  | { type: 'hello'; clientId: string; user: User }
  | { type: 'session.updated'; session: RuntimeSession }
  | { type: 'session.closed'; id: string }
  | { type: 'prompt'; prompt: Prompt }
  | { type: 'prompt.cancel'; id: string }
  | { type: 'transfer'; transfer: Transfer }
  | { type: 'tunnel'; id: string; status: TunnelStatus }
  | { type: 'monitor'; sessionId: string; stats?: MonitorStats; error?: string }
  | { type: 'job'; jobId: string; event: 'data' | 'done' | 'error'; data?: unknown; error?: string }
  | { type: 'notify'; level: 'info' | 'success' | 'warning' | 'error'; title: string; message?: string }
  | { type: 'server'; status: ServerStatus }
  | { type: 'vault'; locked: boolean }
  | { type: 'pong' }
interface Prompt { id: string; kind: 'hostkey' | 'password' | 'passphrase' | 'keyboard-interactive' | 'confirm'; title: string; message?: string
  sessionId?: string; connectionId?: string; fields: { label: string; echo: boolean; value?: string }[]
  hostKey?: { host: string; port: number; keyType: string; fingerprint: string; fingerprintMd5: string; status: 'unknown' | 'mismatch'; knownFingerprint?: string }
  allowSave: boolean }
```
Client → server: `{type:'prompt.response', id, accept, values?, save?}`, `{type:'subscribe', topic:'monitor', sessionId}`,
`{type:'unsubscribe', topic:'monitor', sessionId}`, `{type:'ping'}`.

### 6.2 Terminal WebSocket `/ws/terminal/{sessionId}?offset=<n>` (also `/ws/share/{token}`)

Output is stored in a per-session ring buffer (default 4 MiB, `Cfg.ScrollbackBytes`) addressed by a monotonically
increasing **byte offset** (total bytes ever produced). Each attached client has its own cursor.

* Attach: client passes the last offset it fully rendered (`offset=0` or omitted for a fresh terminal).
  - If `offset` is still inside the ring: server sends `{type:'attach', mode:'delta', from:offset, head}` then binary chunks
    from `offset` to head.
  - Otherwise: `{type:'attach', mode:'reset', from:<ring tail>, head}`; the client must `term.reset()` and then receives
    the whole ring from its tail (the server prefixes nothing; the client resets). Then `{type:'attach-end', head}`.
  - After that, live output streams as binary frames. Every binary frame is exactly the bytes following the previous one.
* Flow control (TERM-4/CORE-3): the client sends `{type:'ack', offset}` after xterm has *parsed* data
  (`term.write(chunk, cb)`), at least every 64 KiB. The server stops sending to a client when `sent - acked > 1 MiB`
  and resumes below 256 KiB. The backend reader pauses (thus throttling the remote via SSH windowing) only while **every**
  attached client is over the high-water mark; with no clients attached the reader never pauses (ring overwrites).
  Output is coalesced into frames of up to 32 KiB / 8 ms.
* Client → server **binary**: raw input bytes. **text** JSON: `{type:'resize', cols, rows}`, `{type:'ack', offset}`,
  `{type:'ping'}`, `{type:'reconnect'}`, `{type:'break'}`, `{type:'signal', name}`.
* Server → client **binary**: output bytes. **text** JSON: `attach` / `attach-end` (above),
  `{type:'state', state, message?, exitCode?}`, `{type:'title', title}`, `{type:'cwd', path}`, `{type:'bell'}`,
  `{type:'readonly', value}`, `{type:'resize', cols, rows}` (authoritative size for viewers), `{type:'pong'}`,
  `{type:'error', message}`, `{type:'prompt-mark', kind:'A'|'B'|'C'|'D', offset, exitCode?}` (OSC 133 shell integration).
* Size policy: the most recently active writer's size wins; the server clamps cols/rows to [2..1000].
* Read-only viewers (shares, admin shadow) never send input/resize (server enforces).

### 6.3 Graphical WebSockets
* **VNC** `/ws/vnc/{sessionId}` — binary, *no subprotocol required*. The Go side performs **RFB security termination**
  (RESEARCH §3.12): it connects to the VNC server (optionally via SSH `sshTunnelVia`), completes VNCAuth / VeNCrypt /
  None with vault credentials, then presents `RFB 003.008` + security type None to noVNC and relays the rest. If the
  server requires credentials that are not stored, the backend asks through the prompt broker. The password never reaches
  the browser.
* **RDP (default)** `/ws/rdp/{sessionId}` — RDCleanPath relay for the IronRDP WASM client (RESEARCH §3.10 Path A). The
  frontend obtains a one-time `authToken` + connection parameters from `POST /api/sessions/{id}/rdp-ticket` →
  `{token, destination, username, domain, password, width, height, ...}` (credentials go to the browser only here, over
  the authenticated channel, for CredSSP). The token is bound server-side to that session's destination.
* **RDP (guacd, optional)** `/ws/guac/{sessionId}?<params>` — subprotocol `guacamole`, Guacamole WebSocketTunnel semantics
  (RESEARCH §3.11). Used when `Cfg.Guacd` is set and the connection option `rdpEngine` = `guacd` (or globally preferred).
  Credentials stay server-side.

---------------------------------------------------------------------------------------------------------------------

## 7. Frontend architecture (`web/`)

* Stack: React 19, Vite 8, TS 7 (`npm run typecheck`), Tailwind 4 (CSS-first `@theme` tokens in `src/index.css`), Radix UI
  primitives via `radix-ui`, `lucide-react` icons, `sonner` toasts, `cmdk` command palette, `zustand` stores,
  `@tanstack/react-query` for REST resources, **`dockview-react`** for the tab/split/float/popout workspace (CSS
  `dockview-react/dist/styles/dockview.css`), `@xterm/xterm` 6 + addons (never `addon-canvas`), `@headless-tree/react` for
  trees, **Monaco** (`monaco-editor` bundled locally + `@monaco-editor/react`, workers via Vite `?worker`, no CDN; owner decision 2026-09-28 — supersedes the earlier CodeMirror choice) for file editing/diff in the editor feature (CodeMirror remains available for small embedded code fields, e.g. automation scripts),
  `tinykeys` for shortcuts, `react-hook-form` + `zod` for forms, `fuse.js` for fuzzy search, `@dnd-kit/*` for drag & drop,
  `@tanstack/react-table` + `@tanstack/react-virtual` for big lists, `uplot` for charts, `asciinema-player` for playback,
  `@novnc/novnc` (lazy `import()`), `@devolutions/iron-remote-desktop(-rdp)` (lazy), `guacamole-common-js` (lazy),
  `trzsz` / `zmodem.js`, `@fontsource-variable/jetbrains-mono` + `@fontsource-variable/inter` (offline fonts), `qrcode.react`,
  `@simplewebauthn/browser`, `react-markdown`, `@xyflow/react`, `date-fns`. All are already installed — do not add more.
  Heavy features (editor, vnc, rdp, player, charts, xyflow) must be lazy-loaded with `React.lazy` / dynamic `import()`.
* Path alias `@/` → `web/src/`.
* Directory layout:
```
src/main.tsx, src/App.tsx               bootstrap: QueryClient, theme, auth gate, events socket
src/api/client.ts                       fetch wrapper (adds X-AstraTerm header, JSON, ApiError), ws URL helper
src/api/types.ts                        ALL shared JSON types (§5.2, §6) — single source of truth on the frontend
src/api/<resource>.ts                   typed endpoint functions + react-query hooks per resource
src/lib/events.ts                       events WebSocket singleton (auto-reconnect, typed subscribe(type, cb))
src/lib/utils.ts                        cn(), formatBytes, formatDuration, etc.
src/stores/                             zustand: workspace (tabs/panels), settings, ui (dialogs), multiexec, prompts
src/components/ui/                      design-system primitives (button, input, select, dialog, dropdown, context-menu,
                                        tabs, tooltip, switch, checkbox, badge, scroll-area, separator, popover, table,
                                        empty-state, spinner, kbd, form field helpers)
src/layout/                             AppShell: MenuBar + Ribbon toolbar + QuickConnect, Sidebar (vertical icon tabs),
                                        Workspace (dockview), StatusBar, CommandPalette, PromptHost, LockScreen
src/app/registry.ts                     extension points (tab kinds, sidebar panels, commands, settings sections,
                                        protocol editors, status bar items) — features self-register via
                                        src/features/<name>/index.ts imported by src/features/index.ts
src/features/<feature>/                 one folder per feature module (terminal, sessions, sftp, editor, tunnels, keys,
                                        vnc, rdp, monitor, tools, servers, snippets, macros, recordings, admin, settings,
                                        home, importer, player, share)
```
* **Registries** (`src/app/registry.ts`):
```ts
registerTabKind({ kind: 'terminal', title: (p) => string, icon, component: React.FC<{ tabId: string; params: any }> })
registerSidebarPanel({ id: 'sessions', title, icon, order, component })
registerCommand({ id, title, category, shortcut?, run: (ctx) => void, when?: () => boolean })
registerSettingsSection({ id, title, order, component })
registerProtocolEditor({ protocol, label, icon, defaultPort, component: React.FC<{ value: Connection; onChange }> })
registerStatusItem({ id, align: 'left'|'right', order, component })
```
* Workspace API (`src/stores/workspace.ts`): `openTab({kind, params, title?, split?: 'right'|'below'|'none', float?})`,
  `closeTab`, `focusTab`, `activeTab`, `splitActive(direction)`, `popout(tabId)`, layout persistence to settings.
* Opening a connection: `openConnection(conn | connectionId, opts)` in `src/features/terminal/open.ts` → POST /api/sessions → opens
  tab of kind `terminal` / `vnc` / `rdp`, or for `sftp`/`ftp`/`s3` protocols a `files` tab.
* Visual language: dark-first professional UI (light theme supported), dense like an IDE, 13px base, JetBrains Mono / system
  mono for terminals. MobaXterm familiarity: ribbon of big icon buttons (Session, Servers, Tools, Sessions, View, Split,
  MultiExec, Tunneling, Keys, Settings, Help), left vertical sidebar tabs (Sessions, SFTP, Snippets, Macros, Tools),
  bottom status bar with remote monitoring.

---------------------------------------------------------------------------------------------------------------------

## 8. Build

```
make web      # cd web && npm ci && npm run build   (outputs to internal/webui/dist)
make build    # CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=…" -o bin/astraterm ./cmd/astraterm
make dev      # go run ./cmd/astraterm --dev & (cd web && npm run dev)   — vite proxies /api and /ws to :7822
make release  # cross-compile darwin/linux/windows × amd64/arm64 into dist/
```
Defaults: listen `127.0.0.1:7822`, data dir `os.UserConfigDir()/astraterm` (or `./astraterm-data` with `--portable`),
`--open` opens the browser, `--tls-cert/--tls-key` or `--tls-self-signed`, `--guacd 127.0.0.1:4822`.

---------------------------------------------------------------------------------------------------------------------

## 9. Module notes (append-only; each module owner documents deviations / extra endpoints here)

### B0 — backend core (`model`, `config`, `httpx`, `store`, `vault`, `events`, `audit`, `app`, `auth`, `server`, `cmd`)
* **Conventions.** Mutations without a resource body return `200 {"ok":true}`; creates return `201` + resource. `httpx.Error`
  maps any error exposing `ErrorCode()` (`model.ErrNotFound/ErrConflict/ErrLocked/ErrForbidden`, store errors, `model.Error`)
  to its status; unknown errors → 500 without details. `model.ErrLocked` ≡ `httpx.ErrLocked` under `errors.Is`. Extra
  codes: `csrf` 403, `invalid_host` 421, `too_large` 413.
* **Router.** Extra `r.PublicWS(pattern, h)` (no auth, Origin check) for `/ws/share/{token}`. Pattern conflicts are logged,
  not fatal. Bearer-token requests skip the CSRF header. On a loopback bind, a non-loopback `Host` → 421 (DNS rebinding).
  Every WebSocket must upgrade with `httpx.AcceptWS` (same-origin + dev origins, 16 MiB read limit).
* **Auth.** 401 codes `invalid_credentials` / `totp_required` / `totp_invalid`; 403 `account_disabled`; 429 + Retry-After.
  Re-auth failures (password change, verify-password, TOTP disable/regenerate) → 403 `invalid_password` (never 401).
  `POST /api/auth/launch` before setup → 409 `setup_required` (token not consumed). Extra endpoints:
  `POST /api/auth/verify-password {password}`, `POST /api/auth/totp/recovery-codes {password}` → `{recoveryCodes}`;
  `totp/enable` → `{recoveryCodes}` (10, shown once). `POST /api/auth/tokens {name, expiresInDays?}` → 201
  `{...APIToken, token}` (token shown once). `GET /api/auth/sessions` items carry `current`. Admin
  `PATCH /api/admin/users/{id}` accepts `{displayName?, role?, disabled?, totpEnabled:false}` (false = reset 2FA).
  Non-"remember" logins get a browser-session cookie (server-side 7 d sliding); remember / desktop setup / launch → 30 d.
* **Connections/folders.** Only admins may set `shared=true` (a shared row is visible to every user). Duplicating another
  user's shared connection drops secrets, `identityId`, `keyId`. PATCH: `options` replaces the whole object; `secrets`
  merges (omitted = unchanged, `""` = delete); `null` clears nullable IDs. Recursive folder delete requires owning every
  item in the subtree (admins exempt). Identities are private; a connection's identity must belong to its owner.
* **Settings.** `PUT /api/settings` and `PUT /api/admin/settings` are RFC 7396 merge patches (null deletes, objects merge).
* **Vault.** Wrong master password → 403 `wrong_password`; lock without master password → 409; `{type:'vault', locked}`
  is broadcast on lock/unlock and after master-password changes.
* **Events.** Failed/unknown subscriptions → `{type:'subscribe.error', topic, params, error}`. Pending prompts are re-sent
  to sockets that connect later (page reload). Job events can arrive before the POST response carrying the `jobId` —
  clients should buffer unknown job IDs briefly. Extra `GET /api/jobs`. Hub: `PublishClient(clientID, ev)`, `HasClient`,
  `CloseUser`, `CloseAuthSession`. `Jobs.Start(user, name, fn)` / `Jobs.Cancel(user, id)`.
* **Audit.** List endpoints return `AuditEntry[]` newest first; page with `before=<last id>`; filters `action` (exact, or
  prefix when ending in `.`/`*`), `target`, `since`, `until` (RFC 3339); admin list also `userId`.
* **For modules.** `app.Visible/CanModify`, `app.RegisterFeature(name, probe)` (adds/overrides `features.<name>` in
  `/api/auth/state`), `d.Store.Tx`, `store.Now()`, `Connections.TouchUsed` (term: call on open),
  `Settings.GetJSON/SetJSON(scope, key)` (`store.ScopeGlobal` or user ID) for module config, `d.Vault.SystemSeal/Open` for
  server-side tokens. Migrations registered in `Mount` need `d.Store.Migrate(ctx)` before use during `Mount`.
  `Cfg.Guacd` defaults to `127.0.0.1:4822` (`--guacd off` disables). Integration tests: `internal/server/servertest`
  (`servertest.New(t)`, `env.Setup`, `env.CreateUser`, JSON client helpers).

### F0 — frontend shell (`web/**` except feature folders owned by others)
* **Registry extras** (`src/app/registry.ts`, all additive to §10.2): `registerCommand` also takes `keybinding: string|string[]`,
  `global` (fires inside text inputs), `hidden` (not in palette), `keywords`, `description`; `run(ctx)` gets
  `{args, source, activeTab, event}`. `registerTabKind` also takes `iconFor(params)`, `canClose(tab)→bool|Promise`,
  `onClose(tab)`, `duplicate(tab, position)`, `reopen(closedTab)`, `noReopen`. `registerRibbonButton` takes `args`, `tooltip`
  (buttons whose command is unregistered render disabled). `registerSettingsSection` takes `keywords`, `adminOnly`.
  Protocol editor props: `{value: Connection, onChange(next: Connection), mode: 'create'|'edit'}`. Context-menu `items(ctx)`
  contexts are typed in `ContextMenuContextMap` (`tab` → `{tabId, kind, params, title}`). Registries expose `useList()` /
  `useItem(id)` hooks. Commands run via `runCommand(id, args)` / `useCommand(id)` (`src/app/commands.ts`).
* **Workspace** (`src/stores/workspace.ts`): `openTab({kind, params?, title?, position?: 'tab'|'right'|'below'|'left'|'above'|
  'float'|'window', id?, activate?, reference?})` → tabId (queued until the dock is ready; singleton kinds use the kind as id
  and get their params updated). Also `closeTab`, `focusTab`, `renameTab` (user title sticks), `setTabTitle(id, t, {force})`
  (automatic titles, ignored after a user rename), `updateTabParams(id, patch)` (persisted with the layout),
  `setTabState(id, {status, progress: 0..1|'indeterminate'|null, activity})`, `useTabState`, `useIsTabVisible`,
  `splitActive`, `arrangeLayout('single'|'columns'|'rows'|'grid')`, `popout`, `float`, `dockTab`, `reopenClosed`.
  Every dock panel is one generic host; tab params must be JSON-serialisable. Conventions read by the shell:
  `params.sessionId` (Home / menus find the tab showing a runtime session) and `params.protocol` (tab icon).
  Panels use dockview renderer `always` (hidden tabs stay mounted — keep xterm alive). Layout: localStorage per user;
  pop-outs are restored as floating groups. Pop-out windows load `/popout.html` (in `web/public`).
* **Commands the shell/home/ribbon call when registered** (owners register them): `sessions.new`,
  `sessions.connect {id}`, `sessions.quickConnect` (arg = raw quick-connect string), `terminal.newLocal`,
  `terminal.attach {sessionId}` (and `<kind>.attach` for vnc/rdp sessions), `terminal.multiexec.toggle`, `files.openLocal`,
  `servers.open`, `tools.open`, `tunnels.open`, `keys.open`, `recordings.open`. Commands with `category: 'Security'`
  are linked from Settings → Security. Built-ins: `app.home|lock|logout|about`, `palette.open`, `workspace.*`, `view.*`,
  `settings.open {section?}`, `vault.lock|unlock`, `quickConnect.focus`, `help.shortcuts`.
* **Keyboard.** One capture-phase dispatcher (`src/app/keybindings.ts`). Focus inside `.xterm` / `[data-terminal]` is the
  terminal scope: plain Ctrl+<printable> always goes to the shell; features add `claimKeys(pred)`. `suspendKeybindings()`
  pauses app shortcuts. User overrides live in settings key `keybindings` (`{commandId: string[]}`, `[]` = unbound).
* **Settings** (`src/stores/settings.ts`): features call `defineSettings('<section>', defaults)` → `{use, useValue, get, set,
  replace, reset, subscribe}`; top-level keys are sections (`appearance`, `general`, `security`, `keybindings`, …).
  Writes are debounced RFC 7396 diffs against the last server value. Theme helpers: `isDarkTheme()`, `onThemeChange()`.
* **Auth UI.** Recovery codes are sent in `totp` (server tells them apart by length). The lock screen uses
  `POST /api/auth/verify-password`. `?launch=` is stripped from the URL before the exchange request.
* **Deviation.** `react-resizable-panels` is not installed; the shell uses its own accessible `ResizeHandle`
  (`src/components/ui/resizable.tsx`). Tailwind type scale is IDE-dense (`text-base` = 13px, `text-sm` = 12px); tab
  content should use container queries (`@container` is set on every dock panel).

### B1 — terminal runtime, SSH core, local shell (`term`, `sshx`, `proto/local`)
* **Sessions REST.** `POST /api/sessions` → 201 `RuntimeSession` (`connecting`; vnc/rdp get no backend). `quick` =
  `Partial<Connection>` + `password` + `secrets` (no `connectionId`; `quick.identityId` must be the caller's). Saved connections
  are re-resolved on every (re)connect; prompt answers live only in session memory. Extra `POST /api/sessions/{id}/resize
  {cols, rows}`. Access: owner = all; other admins = GET, `?all=1`, scrollback, DELETE (force close), read-only attach (last
  three audited: `session.scrollback`, `session.close`, `session.shadow`); everyone else → 404. `PATCH {title:""}` restores the
  default title. `RuntimeSession.title` is the connection/request/renamed title — OSC titles only reach sockets as `title`.
  `scrollback` returns raw ring bytes by default (and `?raw=1`); `?raw=0` → ANSI-stripped text (as §6.0 and `web/src/api`).
* **Terminal WS.** Attach sends `attach`, `readonly{value}` (always), `state`, `resize`, `title`/`cwd` (if known), replays
  buffered `prompt-mark`s, then `attach-end` in both modes. A client that falls out of the ring (possible only with several
  clients) gets a new `attach(reset)` … `attach-end` mid-stream — handle `attach` at any time. Sending stops at sent−acked >
  1 MiB (resumes < 256 KiB); the backend reader pauses while *every* client has > 1 MiB produced-but-unacked (clients must
  ack); ring ≥ 2 MiB. Frames ≤ 32 KiB, partial frames ≤ one per 8 ms. `{type:'error'}` answers read-only input and failed
  control messages. Dim local notices are written into the stream (SSH banner, connect failure, `Session ended (exit code N)`,
  reconnect separator). Close code 4404 = unknown session.
* **Opener contract.** `Backend.Read` returns `io.EOF` on a normal end and another error on connection loss; only the latter
  triggers `autoReconnect` (1 s → 60 s ±20 %, only after a first successful connect, never for `term.Permanent(err)` errors —
  auth failures, rejected host keys, cancelled prompts). `ExitCoder` −1 = unknown. Signals: INT TERM KILL HUP QUIT USR1 USR2
  STOP CONT WINCH. `startupCommand` is sent after every successful (re)connect, one CR-terminated line per line.
* **For modules (additions).** `term.RegisterPolicy(protocol, fn)` (create-time check), `term.Permanent/IsPermanent`,
  `term.SessionFromContext/WithSession`, `term.LookupEncoding`, `term.StripText`; `Session.SetStatus/Notice/Banner/
  RememberSecret/Resolve(ctx)/Connection/Size/Cwd/Scrollback/Offsets/Signal/Break`; `Manager.Resize/Reconnect/Rename/
  CloseContext/SetRecording/SetLogging/TrackClient(id) → release`. Graphical modules must `TrackClient` each viewer (else the
  detached-session reaper closes the session after `DetachedSessionTTL`) and get credentials with `Session.Resolve(ctx)`.
  Hooks run without session locks; copy `OnOutput`/`OnInput` data before retaining it.
* **Recording & logs.** Options `record`, `recordInput` (default false: input can contain passwords), `log`, `logTimestamps`
  fall back to the owner's, then the global settings section `terminal` (same keys). Asciicast v3 → `<data>/recordings/
  <recordingId>.cast` (`o`, `i`, `r`, `m` on OSC 133 A, `x`); text logs → `<data>/logs/<YYYY-MM-DD>_<title>_<recordingId>.log`
  (ANSI-stripped, CR/BS applied, optional `[YYYY-MM-DD HH:MM:SS.mmm] ` prefix). Both files 0600 with a `recordings` row
  (kind `asciicast`/`log`); size and `endedAt` are set when recording stops or the session closes.
* **SSH pool.** Key = user + host (lower-case) + port + username; idle close 60 s after the last release. `Get` accepts
  ssh/sftp/mosh connections. `Client.NewSessionContext(ctx) (sess, owner, release, err)` falls back to another connection on
  MaxSessions. Generic dialing is a method: `Pool.Dialer(ctx, user, conn, secrets) (*Dialer, error)` (`DialContext`, `Dial`,
  `Via`, `Close`) and `Pool.DialConnection(ctx, user, conn, secrets) (net.Conn, error)` (closing the conn releases gateways);
  order: `sshTunnelVia`, else `jumpHosts`, else `proxy`/`proxyCommand`. `jumpHosts` entries are connection IDs or ad-hoc
  `[user@]host[:port]`; a hop's own `jumpHosts`/`sshTunnelVia` are ignored; the first hop uses its own proxy, else the target's.
* **SSH options (extra).** `proxyCommand` (`%h %p %r %n %%`; desktop mode or admins), proxy types also `socks4a`/`socks5h`,
  `noPty` (with `remoteCommand`: split stdout/stderr, stderr red), `clientVersion`, `portKnock[].delayMs`, `keepAliveSec`
  (default 30, 0 = off; 3 misses close the connection), `connectTimeoutSec` (default 20, paused during prompts), algorithm
  lists accept ssh_config `+`/`-`/`^` modifiers, `useAgent` defaults to true in desktop mode. Empty username → "Login as"
  prompt (kind `keyboard-interactive`, one echo field).
* **SSH auth.** `auto`: publickey (stored key + certificate, then ≤ 6 host-agent keys), then password or KI (password first when
  one is stored), ≤ 3 tries each. A stored password answers one lone "Password:" KI question. `save`d answers go to the owner's
  connection secrets (`password`, `passphrase`) only once proven correct; a jump hop's answers are remembered as
  `hop:<id>:<key>`. Host agent (auth, forwarding) only in desktop mode or for admins — otherwise forwarding serves a read-only
  keyring of the user's decryptable stored keys. X11 forwarding: desktop mode only.
* **Known hosts.** `host` lower-case, `port` separate, `publicKey` in authorized_keys format (bare base64 accepted),
  `fingerprint` `SHA256:…`; prompts carry `fingerprintMd5` as `MD5:aa:…`. Replacing a changed key ("save" on a mismatch) is
  allowed in desktop mode or for admins only; audited `known_host.add|replace`, `ssh.hostkey.mismatch_accepted`. Helpers:
  `sshx.ParseKnownHostKey`, `FormatKnownHostKey`, `FingerprintMD5`, `ParsePrivateKey` (OpenSSH/PEM/PPK),
  `NeedsPassphraseError`.
* **Extra endpoints.** `GET /api/sessions/{id}/ssh-info` → `SSHConnInfo`; `GET /api/ssh/algorithms` → `{supported, insecure}`
  of `{kex, ciphers, macs, hostKeys, publicKeyAuths}`; `GET /api/ssh/agent` → `{available}`.
* **Local shells.** `GET /api/local/shells` is admin-only in server mode (like the protocol). Unix IDs are basenames ($SHELL
  first, /etc/shells, PATH); Windows: `pwsh`, `powershell`, `cmd`, `git-bash`, `wsl:<distro>`. Options: `shell` (ID, path or
  command), `args`, `cwd` (`~` expanded, default home), `env`, `loginShell` (Unix, default true: argv0 `-name` when no args).
  The environment gets TERM, COLORTERM=truecolor, TERM_PROGRAM=AstraTerm, LANG=en_US.UTF-8 if unset; ASTRATERM_* is dropped.


### F1a — terminal UI (`web/src/features/terminal/**`)
* **Opening sessions** lives in `src/features/terminal/open.ts`: `openConnection(connOrId, {position?,
  reference?, activate?, title?})`, `openQuick(quick & {password?})`, `openLocalShell(shellId?)`, `attachSession(sessionId)`,
  `duplicateSession(tabId, {position})`, `restartSessionInTab(tabId)`, `estimateTerminalSize()`; all resolve the tab id or null
  (errors are toasted). Tab kinds by session kind: `terminal` / `vnc` / `rdp` with params `{sessionId, protocol, connectionId?,
  quick?, shell?, color?, title}`; `sftp`/`ftp`/`s3` open a `files` tab `{connectionId, protocol}` — or `{quick, protocol}` for
  quick connect (the quick spec never contains secrets; the files tab must let the backend prompt).
* **Terminal tab params** add `zoom?` (font px delta), `monitorActivity?`, `monitorSilence?`, `fixedSize?: {cols, rows}` (CC-19).
  Closing a tab DELETEs its session (confirmation when running, `general.confirmCloseRunning`); `terminal.detach` / tab menu
  "Detach" closes the tab only. Reopen (CC-7) re-attaches a still-running session, else creates one with the same parameters.
* **Bus** `src/features/terminal/bus.ts`: §10 API plus `useTerminals()`, `useTerminalInfo(tabId)`, `useActiveTerminalInfo()`,
  `getTerminalByTab`, `getTerminalsBySession`. `TerminalHandle` (types.ts) also has `paste(text, {mode:'paced'})` (safety
  pipeline), `input` (as typed: MultiExec + observers) vs `send` (raw, this session only), `writeLocal`, `pasteFromClipboard`,
  `zoomBy`, `togglePause`, `saveOutput`, `copyAll`, `jumpToPrompt`, `sendSignal`, `sendBreak`, `reconnect`, `info()`.
  `broadcast(data)` defaults to every open terminal tab's session; without an open tab `sendToSession` uses REST `/input`.
* **Plugins**: the setup ctx is a superset — cast to `TerminalPluginContextEx` for `handle`, `writeLocal`,
  `addOutputFilter(fn → bytes|null)` (filters what xterm renders; offsets/acks still count raw bytes) and `isReplaying()`.
  `ctx.send` is raw input (no MultiExec, no input observers); `ctx.settings` is a live getter.
* **Replay semantics.** A view attaching from offset 0 to an existing session (or getting `attach(reset)`) treats the replayed
  bytes as history: xterm's automatic replies (DA, CPR, OSC 52…), bells and notifications generated while parsing them are
  dropped. Deltas after the view's own render point and the first output of a session this page created are live. Reload:
  a serialize snapshot + offset per session is kept in sessionStorage (`astraterm:term:v1:<sessionId>`, only at a clean VT/UTF-8
  boundary) → delta attach. The UI prints no reconnect separator (the backend's notices do).
* **Commands** (category Terminal, all rebindable): `terminal.newLocal` ($mod+Shift+L), `terminal.attach {sessionId}`,
  `terminal.duplicate`, `terminal.splitRight|splitDown`, `terminal.find` ($mod+Shift+F), `terminal.clear`, `terminal.reset`,
  `terminal.zoomIn|zoomOut|zoomReset` ($mod +/−/0), `terminal.copy` (Ctrl+Shift+C), `terminal.paste` (Ctrl+Shift+V /
  Shift+Insert — claimed so the browser raises a native paste event), `terminal.pasteSlow`, `terminal.clipboardHistory`,
  `terminal.selectAll`, `terminal.multiexec.toggle`, `terminal.reconnect`, `terminal.pauseOutput` (ScrollLock),
  `terminal.saveOutput` ($mod+Shift+S), `terminal.saveOutputHtml`, `terminal.copyAll`, `terminal.prevPrompt|nextPrompt`,
  `terminal.interrupt`, `terminal.detach`, `terminal.settings`. Used when registered by others: `sessions.quickConnect` (ssh://…
  links), `files.openForSession {sessionId, path?}` (context menu "Open files here", file:// links).
* **Settings** section `terminal` (see `settings.ts` for keys/limits); `connection.options.terminal` may override any key
  except `customSchemes`/`passthroughKeys` (a `theme` override applies to both UI themes). Scheme ids: built-ins
  (`BUILTIN_SCHEMES`) or `custom:<id>`. For connection editors: `TerminalOverridesEditor` in `terminal/overrides.tsx`.
* **MultiExec** fans input out client-side over each target's own terminal socket (there is no multi-target WS message).

### F1b — sessions UI (`web/src/features/sessions/**`)
* **Commands.** `sessions.new {folderId?, protocol?, initial?}` ($mod+Shift+N, Alt+Shift+N), `sessions.edit {id, tab?}`,
  `sessions.connect {id, position?: 'tab'|'right'|'below'|'window'}`, `sessions.quickConnect` (raw text or `{text}`),
  `sessions.focusSearch {text?}`, `sessions.newFolder {parentId?}`, `sessions.duplicate {id}`, `sessions.delete {id|ids}`,
  `sessions.reveal {id}`, `sessions.connectFolder {folderId}`, `sessions.expandAll|collapseAll`. Used when registered:
  `files.openForConnection {connectionId}`, `importer.open`, `keys.identities`, `keys.open`.
* **Protocol editors.** One `editors/<protocol>.tsx` per protocol calls `defineProtocol()` (editors/define.ts):
  `registerProtocolEditor` + a *profile* (basic fields shown, credential mode, tabs, `jump: 'chain'|'gateway'`) + `validate`.
  Editors get errors / read-only / saved connections from `EditorEnvContext` (editors/context.ts); shared controls
  (incl. `SecretInput`, searchable `ComboInput`, `ConnectionSelectOption`) are in editors/fields.tsx. Pickers for
  serial ports, local shells, containers, kube contexts/pods already use the §6.0 list endpoints (404-tolerant).
* **Option keys beyond §5.3** (backend owners please honour): `logTimestamps`, `recordInput`, `loginShell`, `noPty`,
  `clientVersion`, `proxyCommand`, proxy types `socks5h`/`socks4a`, `portKnock[].delayMs` (all per B1); WinRM `https`,
  `insecureTls`, `winrmAuth` ('ntlm'|'basic'), `shell` ('powershell'|'cmd'); IPMI `ipmiInterface` ('lanplus'|'lan'),
  `cipherSuite` (0–17), `privilegeLevel` ('administrator'|'operator'|'user'). The editor offers `jumpHosts` (saved ids or
  ad-hoc `[user@]host[:port]`) only for ssh/sftp/mosh and `sshTunnelVia` for other network protocols, and drops the other
  key on a protocol switch; proxy / knocking / timeouts are offered for every network protocol (generic Dialer).
* **Icons** (`connection.icon`, `folder.icon`): `lucide:<Name>` from a built-in set, or a `data:image/…;base64` PNG
  (uploads are rasterised to 64×64). Other values are ignored when rendering; custom images only render via `<img>`.
* **Settings** section `sessions`: `expanded` (folder ids), `sortMode`, `showFilters`, `showFavorites`, `showRecent`,
  `favoritesOpen`, `recentOpen`, `quickConnectHistory` (inline passwords stripped), `lastProtocol`, `connectAllConfirm`.
* **Menus.** `session-node` context: `selection` = selected connection ids, plus `folderIds: string[]` and
  `area: 'root'` (empty space). Tabs whose `params.connectionId` is a saved connection get "Edit session…" / "Show in
  session tree". The `sessions` ribbon button is re-registered (replaces the shell placeholder); menubar additions at order 150.
* **Dialogs** (editor, folder) render in their own small React root (dialogs/host.tsx) sharing the query client and
  stores: hidden but kept while the screen is locked, dropped on sign-out, app shortcuts suspended while open.
* **Quick connect** scrubs inline passwords from the ribbon field's local history (`astraterm:quickconnect-history`); the
  shell could store the sanitised text itself.

### Integration — foundation stage
* **Setup token.** In server mode (or on any non-loopback bind) a fresh instance generates a one-time setup token and
  prints its URL as `…/?setup=<token>` in the startup banner. `POST /api/auth/setup` must then carry `setupToken` (403
  `setup_token_required` when missing or wrong, rate-limited per IP), and `GET /api/auth/state` reports
  `setupTokenRequired: true`. The UI strips `?setup=` from the address bar and sends it, or asks for it. Desktop mode on
  loopback is unchanged. `servertest.Env.Setup` passes the token automatically; the CLI's `reset-password` needs none.
* **Quick-connect history.** The ribbon field stores only `sanitizeQuickConnect(text)` (`src/app/quickconnect.ts`).
  The sessions feature registers its parser with `setQuickConnectSanitizer`, so inline passwords (`user:pw@`,
  `/p:`, `-P`, …) never reach localStorage; until it registers, a fallback strips `user:password@`.
* **Settings concurrency.** `PUT /api/settings` and `PUT /api/admin/settings` merge inside one write transaction
  (`store.Settings.Update`), so concurrent patches from several tabs no longer lose keys.
* **Settings layering (frontend).** `defineSettings(...).set(patch)` persists only the keys it is given, not the
  section's defaults, so admin global values keep applying to keys the user never changed. `reset([keys])` removes the
  user's values so they inherit again (global, else default). After each write the store adopts the effective values
  that the `PUT /api/settings` response returns. Before this change, the first `set()` in a section froze every
  default into the user scope and shadowed the admin's global settings.
* **Shutdown order.** `term.Manager.Wait(ctx)` returns once the manager's shutdown sweep (every session closed, audit and
  recording rows written) is done; `server.Server.Close` waits for it (≤ 10 s) before closing the database. A module
  whose shutdown writes to the store must do the same (expose a wait and have `Close` call it before `Store.Close`).
* **Shell layout.** The document never scrolls: `#root` and the workspace `<main>` use `overflow: clip`, hidden dockview
  render overlays are anchored at the origin (index.css), and `AppShell` snaps any programmatic document scroll back to
  0 (main window and pop-outs). Feature UIs scroll inside their own containers; auth screens use `justify-center-safe`.
* **Vault unlock.** A `{type:'vault', locked:false}` event (unlocked in another tab/device or by another admin) resolves a
  pending unlock dialog, so requests blocked by 423 retry without asking for the password again.
* **SSH pooling.** Transports are shared per user + host + port + username (60 s idle TTL), so a quick connect to an
  address the user already has a live or recently idle transport to reuses it without prompting. A different host
  spelling (`localhost` vs `127.0.0.1`) is a different transport and known-hosts entry.
* **Build/test.** `make web` runs `npm ci` only when `web/package.json` / `package-lock.json` change (the lockfile is
  never rewritten); `make fmt-check` covers `internal/deps` too. End-to-end smoke test of the built binary (desktop +
  server mode, Docker SSH target): `make smoke` → `scripts/smoke/smoke.mjs` (see `scripts/smoke/README.md`).

### HTTP layer migrated to Echo v5 (2026-09-27)
* **What changed.** `internal/httpx` wraps Echo v5 (`github.com/labstack/echo/v5` v5.3.1, now a direct dependency) instead
  of `net/http.ServeMux`; every route, middleware and handler was converted (§3, §4 "Router helpers"). External behavior
  is unchanged: paths, methods, status codes, JSON bodies, headers (CSP, HSTS, cache-control, nosniff), cookies, CSRF,
  Host guard, WS Origin check, rate limits, error bodies, SPA fallback and request logs.
* **Go API renames** (old → new):
  - `r.Public("GET /api/x", h)`, `r.Handle(…)`, `r.Admin(…)` → `d.Router.Public()` / `.API()` / `.Admin()` `.GET("/x", h)`
    (groups are rooted at `/api`); `{id}` → `:id`; `r.PathValue("id")` → `c.Param("id")`; `r.URL.Query().Get(k)` →
    `c.QueryParam(k)`. `r.WS` / `r.PublicWS` keep their names and take `echo.HandlerFunc` and full paths (`/ws/x/:id`).
  - Handlers `func(w http.ResponseWriter, r *http.Request)` → `func(c *echo.Context) error`; `httpx.Error(w, err)` →
    `return err`; `httpx.JSON(w, st, v)` → `c.JSON(st, v)`; `httpx.OK(w)` → `httpx.OK(c)`.
  - `httpx.Decode(r, &v)` / `DecodeOptional` / `DecodeLimit(r, &v, n)` → `httpx.Bind(c, &v)` / `BindOptional` /
    `BindLimit(c, &v, n)` (same limits and error codes; `BindOptional` no longer swallows malformed JSON).
  - `httpx.UserFrom(ctx)` → `httpx.UserFrom(c)` in handlers and `httpx.UserFromContext(ctx)` elsewhere;
    `httpx.AuthInfoFrom(ctx)` → `AuthInfoFrom(c)` / `AuthInfoFromContext(ctx)`; `httpx.ClientIP(r)` → `ClientIP(c)`
    (`ClientIPFrom(ctx)` is unchanged).
  - `httpx.AcceptWS(w, r, opts)` → `httpx.AcceptWS(c, opts)`; `events.Hub.ServeWS` is an `echo.HandlerFunc`.
  - `httpx.Authenticator.Authenticate(w, r)` → `Authenticate(c *echo.Context)` (`AuthenticatorFunc` likewise).
  - `audit.Logger.Log` / `LogUser` also accept the `*echo.Context`; `router.Public("/", spa)` → `router.SetFallback(spa)`.
* **New.** Request bodies are capped at 2 MiB (`httpx.DefaultBodyLimit`) for handlers that read `c.Request().Body`
  directly; a route that needs more (uploads) adds `httpx.BodyLimit(n)` (n ≤ 0: no cap). `Bind` / `BindLimit` apply their
  own limit (but honor a smaller `BodyLimit`). `httpx.RequireUser` / `RequireAdmin` and `Router.Echo()` for routes outside
  the `/api` groups. `c.Bind(&v)` is `httpx.Bind`.
* **Routing.** GET routes also answer HEAD. Registering a method + path twice (parameter names ignored) logs the conflict
  and keeps the first route. Paths match on canonically escaped segments and parameters are decoded exactly once, as
  with ServeMux (`/api/connections/a%2Fb` → id `a/b`); non-canonical paths (`//`, `/./`, `/../`) get a 307 to the clean
  path (after the CSRF check). Routes may be registered at any time (concurrency-safe router).
* **Rule.** Echo pools `*echo.Context`: never use it after the handler returns (goroutines get `c.Request().Context()`
  and plain values). A WebSocket handler blocks until the socket is done.

### files-backend — `internal/vfs`, `internal/transfer` (SSH-browser backend, FILE-*, PROTO-23..27)
* **Handles.** `POST /api/fs` → **201**. Body: `sessionId` | `connectionId` | `local:true` | `quick` (a
  `Partial<Connection>` + optional `password`/`secrets` for quick-connect file tabs of protocol sftp/ssh/ftp/s3/webdav/
  smb; missing credentials are prompted for over the events socket) and optional `sudo:true` (SSH only: browse as root
  through `sudo <sftp-server>`; password = secret `sudoPassword`, else the login `password`, else a `password` prompt
  offering "remember" → saved as `sudoPassword`). A `sessionId` handle uses that terminal's own SSH transport
  (`Pool.ForSession`; after a reconnect it re-acquires the new one) — no second login / 2FA. Reply `{id, kind, driver,
  home, userHome, root, label, capabilities, sessionId?, connectionId?, protocol?, host?, username?, port?, createdAt}`:
  `kind` ∈ `sftp|ftp|local|s3|webdav|smb` (the SCP/shell fallback and sudo mode stay `kind:'sftp'`; `driver` ∈
  `sftp|sudo-sftp|scp|ftp|local|s3|webdav|smb`); `home` = start folder (`sftpRoot` of ssh / `initialPath` of file
  connections when it exists, else the login folder), `userHome` = login folder, `root` = `/`. `capabilities` =
  `{chmod, chown, symlink, exec, checksum, search, archive, extract, copy, presign, sudo, resume, mtime, hardlink,
  space}`. Handles belong to their creator (checked on every call, admins included), close after 10 min unused (a
  running transfer pins them), and close with their runtime session. Extra: `GET /api/fs` (own handles), `GET
  /api/fs/{id}`. Unknown / closed / foreign handle → 404 `{code:'fs_not_found'}` (reopen). The session's transport is
  gone → 409 `session_not_connected`. Local: desktop mode = the whole host (Windows: `/C:/…`, `/` lists drives);
  server mode = admins only, jailed with `os.Root` in `$ASTRATERM_LOCAL_FS_ROOT`, else global setting `files.localRoot`,
  else `<data dir>/files`.
* **Connection options.** ssh: `sshBrowser: 'sftp'|'scp'|'none'` (default sftp, automatic SCP/shell fallback when the
  SFTP subsystem is missing; `none` → 409 `ssh_browser_disabled`), `sftpRoot`, `sftpServerCommand` (sudo mode),
  `followCwd`. ftp: `ftpTls none|explicit|implicit`, `insecureTls`, `ftpEpsv` (true), `ftpMlsd` (true),
  `ftpTrustPasvIP` (false: data connections go to the control host), `ftpMaxConnections` (4), `initialPath`; passive
  mode only. s3: `endpoint` (URL; else `https://host:port`, `https:false` for http), `region`, `bucket` (empty = `/`
  lists buckets, paths `/bucket/key`), `pathStyle`, `accessKeyId` + secret `secretAccessKey` (+ `sessionToken`);
  without a key the host AWS config / `profile` is used (desktop mode or admins only). webdav (protocol `webdav`): `url`
  or host/port + `https` (default true unless port 80) + `basePath`, `insecureTls`. smb (protocol `smb`): `share`
  (empty = `/` lists shares), `domain`. All: proxy / `jumpHosts` / `sshTunnelVia` via the generic Dialer.
* **Errors.** 403 `permission_denied`, 404 `not_found`, 409 `exists`, 409 `not_empty`, 400 `not_supported` (the driver
  cannot do it), 409 `disconnected` (transport dropped mid-operation), 422 `fs_error` (other remote failure; message =
  the server's), 413 `too_large` (+ `size`), 409 `offset_mismatch` (+ `size`).
* **FileEntry.** `mode` = POSIX `st_mode` (type bits | permission bits → use `mode & 0o7777`); `perm` like `ls -l`;
  `owner`/`group` = names (remote `getent`, batched + cached per handle) or the numeric id; symlinks carry `linkTarget`
  and `linkType` (`file|dir|broken`). `list` → `{path, parent, entries}`, dirs (and links to dirs) first, then name
  (case-insensitive); Lstat semantics. `stat` = Lstat.
* **Upload.** `PUT /api/fs/{id}/upload?path=<target>&offset=<n>[&final=1][&total=<bytes>][&mtime=<unix s|ms>]`, raw
  body, no size cap. Data goes to `<target>.astraterm-part` from `offset` (0 truncates and creates missing parent folders;
  an offset below the part size truncates = safe chunk retry; above it → 409 `offset_mismatch` with the stored `size`,
  header `Upload-Offset`). `final=1` — or `offset + body == total` — atomically renames the part onto the target
  (replacing a file, keeping its permissions; a folder of that name → 409) and applies `mtime`. **Without `final`/`total`
  the part is kept.** Reply `{size, offset, final, entry?}`. Resume point: `GET` (or `HEAD`) `…/upload?path=<target>` →
  `{size, offset, exists}` + `Upload-Offset`. S3 keeps native multipart state instead of part files, WebDAV stages
  chunks in `<data dir>/tmp` until the final one (both are lost on a server restart → the client restarts at 0).
* **Download.** One file: `Range` (one range: `a-b`, `a-`, `-n`; ends beyond the size are clamped) → 206 with exact
  `Content-Range` / `Content-Length`; an invalid or multi-range header → 200 full; unsatisfiable → 416 `Content-Range:
  bytes */<size>`; `If-Range` (ETag or date) that does not match → 200 full; `HEAD` answers the same headers; a driver
  that cannot read from an offset answers 200 full (FTP without REST and WebDAV servers ignoring Range are emulated by
  skipping). `ETag` / `Last-Modified`, `Content-Disposition: attachment` (RFC 6266 `filename*`). `inline=1` → real `Content-Type` for images / audio / video / pdf / text; HTML,
  XML, SVG and scripts are sent as `text/plain` or with a `sandbox` CSP, never as active content. A folder, several
  `paths=` (repeated), `zip=1` or `format=tar.gz|tar` → streamed archive (`name=` file name; tar is produced by the
  server's `tar` when it can run commands); file symlinks are followed, folder symlinks skipped. A transfer that
  fails mid-way aborts the connection (no silently short files).
* **Read / write (editor).** `GET read?path=&maxBytes=` (default 5 MiB, max 64 MiB; symlinks followed) → `{path,
  content, encoding:'utf-8'|'base64', size, mtime, mode}`; larger → 413 `too_large` + `size`. `PUT write {path,
  content, encoding, expectMtime?, expectSize?, sudo?}` (body ≤ 96 MiB) rewrites the file in place (inode, owner,
  mode and symlinks kept); the reply is the **resolved target's** `FileEntry` (symlink followed, `path` as requested) —
  the same view `read` and the `expectMtime` check use. Changed (second precision) or deleted since `expectMtime` →
  409 `{code:'conflict', mtime?, size?}`. Permission denied → 403 `permission_denied`; retry with `sudo:true` (SSH
  with shell access: `sudo tee`, password validated with `sudo -S -v` first).
* **Operations.** `mkdir {path, parents}` / `rename {from, to (path or bare name), overwrite?}` (existing target →
  409 `exists` unless `overwrite` for files) / `symlink {target, link}` / `link {target, link}` (hard link) /
  `touch {path, mtime?}` / `archive {paths, dest?, format:'zip'|'tar.gz', overwrite?}` → `FileEntry`;
  `delete {paths, recursive}` → `{ok, deleted}` (Lstat-based post-order or `rm -rf`, never follows symlinks, refuses
  `/`); `chmod {paths, mode, recursive}` (`mode`: number = permission bits, or string: octal `"755"` / symbolic
  `"u+x,go-w"`, `X` supported; symlinks skipped); `chown {paths, uid?, gid?, owner?, group?, recursive}`;
  `extract {path, destDir?}` (native tar/unzip/7z/unrar when available, else zip / tar(.gz) in Go, zip-slip safe) →
  `{ok:true}`; `copy {from, toDir, overwrite?}` → `{entries}` (same folder → "name (copy).ext"; `cp -a` or server-side
  COPY/CopyObject when available); `checksum {path, algo: md5|sha1|sha256|sha512}` → `{path, algo, hash}`;
  `search {path, pattern, content?, maxResults}` → `{entries, truncated}` (case-insensitive glob; text without
  wildcards = substring; `content` = grep; `find -xdev` over exec, walk otherwise); `realpath` → `{path}`.
* **Extra endpoints.** `GET /api/fs/{id}/cwd[?sessionId=]` → `{path, sessionId?}` (terminal cwd, `""` if unknown);
  `GET /api/fs/{id}/space?path=` → `{total, free, avail}`; `POST /api/fs/{id}/presign {path, expiresSec}` → `{url,
  expiresAt}` (S3, ≤ 7 days); `POST /api/fs/compare {left:{fsId,path}, right:{fsId,path}, recursive, mode:
  'size-mtime'|'checksum', excludes?, maxItems?}` → `{items:[{path, type, status:'same'|'different'|'left-only'|
  'right-only'|'type-mismatch', reason?:'size'|'mtime'|'checksum'|'target', left?, right?}], truncated, summary:{same,
  different, leftOnly, rightOnly, typeMismatch}}` (mtime tolerance 2 s; one-sided folders are not expanded).
* **Transfers.** `POST /api/transfers` → 201; extra keys `move` (sources deleted after each copied file; same handle →
  rename), `preserve` (mtime + permissions, default true), `verify` (SHA-256 of source vs destination), `label`.
  Copies go through `<name>.astraterm-part` + rename, recurse (folders merge), recreate symlinks where the destination
  supports them (else file links are followed, folder links skipped), retry transient network errors 3× resuming from
  the part size, run ≤ 3 at a time (others `queued`). `overwrite:'resume'` continues a smaller destination / part file.
  `overwrite:'ask'` raises a `confirm` prompt per conflict (title "File already exists", one field `action` = default):
  Yes = overwrite, No = skip, "Remember my choice" (`save`) = same answer for the rest of the transfer; custom UIs may
  answer `values:['overwrite'|'skip'|'resume'|'rename']`; no connected socket / timeout = skip. Transfer JSON adds
  `move`, `overwrite`, `skippedFiles`, `failedFiles`, `errors` (≤ 20 `"<path>: <message>"`); a transfer with failed
  files ends `error` with "N of M files failed". Events `{type:'transfer'}` ≤ 4/s per transfer (state changes
  immediately). In memory: survive browser reloads, not server restarts (finished ones kept 24 h / 200 per user).
  Extra: `GET /api/transfers/{id}`, `DELETE /api/transfers` (forget finished) → `{removed}`; `DELETE
  /api/transfers/{id}` cancels a running transfer first.
* **Audit.** `fs.upload` (size, sha256), `fs.download` (size, sha256 of complete single-file downloads; archives:
  paths/files/bytes; Range requests are not logged), `fs.write`, `fs.delete`, `fs.rename`, `fs.mkdir`, `fs.chmod`,
  `fs.chown`, `fs.symlink`, `fs.copy`, `fs.archive`, `fs.extract`, `fs.presign`, `fs.create`,
  `transfer.start|done|error|canceled`, `connection.secret.save` (sudo password remembered).
* **Follow terminal folder (FILE-2).** For SSH terminal sessions (connection `followCwd` not false; without the option
  the owner's / global `files.followTerminal: false` disables it; skipped with `remoteCommand` or `sshBrowser:'none'`),
  after (re)connect the backend runs `echo "$SHELL"` on the session's own transport and waits for a **quiet prompt**:
  output idle ≥ 0.5 s, no keystroke for 0.5 s, no half-typed line (the last input ended with Enter / Ctrl-C / Ctrl-D),
  no full-screen program (alternate screen), and a cursor line that looks like a shell prompt (ends in `$ # % ❯ ➜ → » λ
  ▶`, or `>` in a prompt showing user@host or a path; REPLs such as `>>>`, `mysql>`, psql `db=#`, continuation
  prompts and password / pager prompts are refused). Typing early does not abandon it: it waits for the next quiet
  prompt (up to 30 min). It then types ONE hidden line (leading space; bash also deletes it from its history) that
  installs an OSC 7 reporter: bash `PROMPT_COMMAND` (array append on ≥ 5.1, else prepended), zsh `precmd_functions`,
  fish `--on-variable PWD`, ksh93/mksh/busybox ash `PS1=$(…)` (dash/sh: initial folder only; csh/tcsh/others:
  skipped). Its echo and the prompt redraw it causes are filtered out of the terminal stream (ring, clients,
  recordings and logs never see them; state-changing sequences such as SGR / modes / OSC pass); keystrokes typed while
  it runs are held back and delivered right after it (never filtered, never lost); if the shell does not answer as
  expected within 4 s, everything held is released verbatim. The folder then arrives as `RuntimeSession.cwd`
  (session.updated) and terminal `{type:'cwd'}` as before. Skipped when the shell already reports its folder.
* **term additions (FILE-2 wiring, `internal/term/shellint.go`).** `term.ShellFamily(shell)`,
  `term.ShellIntegrationLine(family)`, `Manager.InjectHidden(sessionID, line, InjectOptions{MarkerOSC, Timeout,
  Precheck})` (errors `ErrNotAtPrompt`, `ErrInjectPending`, `ErrUserActive`), `term.IsTerminalReply(input)` (automatic
  terminal answers — cursor-position / device-attribute / mode / focus reports, OSC / DCS replies — are not typing).
  Wiring in `session.go` (three lines): a `Session.echo` field, the pump emitting through `emitBackend` (a pass-through
  unless a hidden command is in flight) and `writeInput` deferring input while one is (`deferInput`).

### files-ui — `web/src/features/files/**` (SSH-browser UI, FILE-1..5, 8, 9, 12, 13, 15..17, GFX-16)
* **SFTP side panel** (`registerSidebarPanel` id `sftp`, order 2, rail badge = active transfers). It follows the active
  *terminal-like* tab (terminal / vnc / rdp) and keeps showing the last one while other tabs (an editor opened from it,
  Home, Settings…) are active. SSH sessions browse through `POST /api/fs {sessionId}` (the terminal's transport); the
  handle is cached per session, reopened when `RuntimeSession.connectedAt` changes (reconnect) and closed with the
  session. Per-session view state (folder, history, selection) survives tab switches and sidebar panel switches.
  vnc/rdp tabs (GFX-16) offer saved ssh/sftp connections to the same host as an "SFTP companion".
* **Auto-show** (MobaXterm): on the first `connected` of an ssh session this page opened (`isFreshSession`), when it is
  the active tab and `files.autoShowPanel` (default true) and `sshBrowser !== 'none'`, the panel is shown with
  `showSidebarPanel('sftp')` (`src/layout/Sidebar.tsx`). Skipped on phones and while the sidebar is hidden or
  collapsed (collapse state read — read-only — from the shell's `astraterm:sidebar` localStorage prefs).
* **Follow terminal folder** (FILE-2): per session; default = `files.followTerminal` ∧ connection `followCwd !== false`.
  cwd = terminal info `cwd` (terminal WS) / `RuntimeSession.cwd`, polling `GET /api/fs/{id}/cwd` while unknown. While
  it stays unknown the footer shows a focusable hint explaining why (option / setting off, remote command, or typing
  before the first prompt — the backend then skips its OSC 7 setup).
* **Session options** (sessions feature, SSH editor): `sshBrowser` ('sftp' default | 'scp' | 'none', "SSH-browser
  type") and `followCwd` ("Follow SSH path", now default **true** in the editor, like the backend).
* **Tab kind `files`** params `{fsId?|connectionId?|sessionId?|local?|quick?, protocol?, path?, dual?, right?: {same
  source keys, path?}, activePane?}` — `{connectionId, protocol}` / `{quick, protocol}` as opened by the terminal
  feature. `dual` = commander (F3 view, F4 edit, F5 copy, F6 move, F7 mkdir, F8 delete, Tab switches panes); each pane
  can show the local host, any open ssh session or any saved sftp/ftp/s3/ssh connection.
* **Commands**: `files.openForSession {sessionId, path?}` (files tab of a session — the terminal's "Open files here"),
  `files.openForConnection {connectionId, path?}`, `files.openLocal {path?}`, `files.commander`, `files.transfers`
  (toggle the queue), `files.toggleFollow`, `files.search`, hidden `files.revealInPanel {sessionId, path?}` and hidden
  `files.upload {sessionId|connectionId|fsId|local, path, files?: File[]|FileList, dataTransfer?}` (upload through this
  engine + queue, e.g. for FILE-22 drop-on-terminal; read a DataTransfer synchronously inside the drop handler).
* **Context menus.** `file` context = `{fsId, path: <folder shown>, entries: <selection; [] = the folder itself>}` plus
  `viewId` and `ctx` (FsContext) extras; built-in items at order 0 (id `files.builtin`). Contributed: `terminal`
  "Show in SFTP browser" (ssh), `session-node` "Open SFTP browser" (ssh connection with a running session in a tab),
  `tab` "Dual-pane commander" (files tabs). Menus: Sessions (order 160) and Tools (order 150).
* **Other features' commands used when registered**: `editor.open {fsId, path, label, source: FsOpenRequest}`
  (double-click / Enter on text files; without it: preview or download + toast), `monitor.toggleBar {sessionId,
  enabled}` (the panel's "Remote monitoring" checkbox; `enabled` is the desired state), `sessions.new {protocol:'ssh'}`,
  `sessions.edit {id}`.
* **Uploads** (browser → fs): chunked `PUT …/upload` (8 MiB, `total` on every chunk, `final=1` on the last), XHR
  progress, 3 files in parallel (`files.uploadParallelism`), `offset_mismatch` → continue from the reply's `size`,
  other failures → resume from `GET …/upload?path=` (≤ 6 attempts, 423 → vault unlock dialog). OS drops walk folders
  (`webkitGetAsEntry`) and create empty folders too. Conflicts with top-level names are resolved in the UI first
  (`files.conflictPolicy`: ask | overwrite | skip | rename). Removing a canceled / failed upload from the queue deletes
  the `<target>.astraterm-part` it left (best effort; not while another upload writes that target). Drops into the list
  go to the folder the list shows (a navigation still loading does not count). **Server transfers** send `overwrite`
  (resolved in the UI), `move` (moves) and `preserve: true`; same-fs moves use `rename`, same-fs copies `copy` with a
  boolean `overwrite` (into the same folder = duplicate; the "rename" policy goes through a server transfer).
  **Downloads** go to the browser's download manager (`zip=1` + `name=` for folders / several items); a single file row
  can be dragged to the desktop in Chromium (`DownloadURL`).
* **Queue UI** (FILE-8): overlay drawer bottom-right above the status bar, status bar item, rail badge, per-panel mini
  progress. Toasts announce finished / failed transfers only while the drawer is closed.
* **Listings** refresh in the background every 8 s while shown (not for folders above 5 000 entries or after errors)
  and after every change made through AstraTerm.
* **Preview** (FILE-12): images (zoom/pan, previous/next), PDF (iframe), audio/video via `download?inline=1`; text and
  Markdown read the first 256 KiB with a `Range` request on `download` (`read` answers 413 for larger files); when
  the ranged response fails the client streams the plain download and stops after 256 KiB.
* **Settings** section `files` ("Files & SFTP"): `autoShowPanel`, `followTerminal`, `showHidden`, `confirmDelete`,
  `doubleClickAction` ('edit'|'preview'|'download'), `uploadParallelism`, `foldersFirst`, `conflictPolicy`,
  `openQueueOnTransfer`, `columns`, `columnSizes`, `sortBy`, `sortDesc`, `bookmarks` (per place: `conn:<id>`,
  `host:<user@host:port>`, `local`). Recent folders are browser-local (`astraterm:files:recent:v1`).

### protocols — `internal/proto/{telnet,rlogin,rawtcp,serial,docker,kube,mosh,winrm,ipmi}` (PROTO-6..13,15,30,31, CC-1,2,18)
* **Backends.** All dial network protocols through `sshx.Pool.DialConnection` / `Dialer`, so `proxy` / `jumpHosts` /
  `sshTunnelVia` apply. Charset conversion is the term layer's job (`options.encoding`); backends deal in native bytes.
* **Extra `options` keys** (beyond §5.3, all additive): telnet `tls` (bool, telnets 992), `insecureTls`, `lineEnding`
  (`crlf|cr|lf`; default CR-NUL); rlogin `variant` (`rlogin`|`rsh`), `command` (rsh; required), `localUser`,
  `localEcho`, `lineEnding`; raw `transport` (`tcp|tls|udp`), `insecureTls`, `localEcho`, `lineEnding`; docker
  `dockerMode` (`logs` = follow-logs session instead of exec) — `dockerHost`, `container`, `shell`, `user`,
  `viaConnectionId` per §5.3; kube `kubeMode` (`logs`); mosh `moshPorts`. `rsh` is a *variant* of protocol `rlogin`, not
  a distinct protocol (the model `Protocol` union is unchanged; `winrm`/`ipmi` were already present).
* **New endpoints.**
  - `GET  /api/serial/ports` → `[{name, description, vid, pid, serial}]` (USB details on linux/windows; names only on
    darwin under CGO-off). `POST /api/serial/autobaud {device, rates?}` → `{baud, score, sample, tried[]}` (CC-18;
    the port must be free). `POST /api/sessions/:id/serial {dtr?, rts?}` and `GET /api/sessions/:id/serial/status` →
    `{device, dtr, rts, cts, dsr, dcd, ri}` (owner only; live serial session).
  - `GET  /api/docker/containers?host=&connectionId=&all=` (extends §6.0 with `connectionId` = reach the engine over
    that saved SSH connection, and `all`). `POST /api/docker/containers/:id/{start|stop|restart}?host=&connectionId=`.
  - `GET  /api/kube/contexts`, `GET /api/kube/pods?context=&namespace=` (as §6.0).
  - `POST /api/ipmi/:connectionId/power {action}` (`status|on|off|cycle|reset|soft`) → `{action, powerOn}`.
* **Gating (SPEC principle 7).** Serial and Kubernetes (host hardware / host kubectl) and local/TCP Docker engines are
  admin-only in server mode; Docker over a caller's own SSH connection (`viaConnectionId`) is allowed for everyone.
  Registered via `term.RegisterPolicy` and re-checked in each opener/endpoint.
* **Frontend.** `web/src/features/protocols/` registers a terminal plugin (tees serial/raw bytes to a hex monitor),
  an overlay (serial line-status + DTR/RTS + BREAK toolbar, hex monitor drawer with hex-send, Docker "Containers"
  dialog with Exec/Logs/Start/Stop/Restart), commands `protocols.hexMonitor.toggle` / `protocols.docker.containers`,
  a Tools-menu entry and a terminal context-menu entry. Editors for these protocols extend
  `sessions/editors/{telnet,rlogin,raw,serial,docker,kube,mosh,winrm,ipmi}.tsx`.
* **Not implemented here.** PROTO-11 (Web Serial on the viewer's machine) is a frontend-only feature needing a
  client-side terminal backend inside the terminal feature; PROTO-15 (WSL) is served by the foundation's `proto/local`
  (`wsl:<distro>` shells). Serial hardware/software flow control is accepted as an option but not applied
  (`go.bug.st/serial` v1.8.0 exposes no flow-control config). IPMI uses go-ipmi's SOL (verified); the `ipmitool` PTY
  fallback was deemed unnecessary and is not implemented.

### protocols — review addendum (supersedes the matching points above)
* **Line discipline.** Local echo (raw, serial, rlogin/rsh, telnet while the server does not echo) is delivered at once
  (it used to wait for the next remote bytes) through `internal/proto/rawtcp/linedisc` (shared by these backends).
  Echo swallows escape sequences and erases one character per Backspace. `lineEnding` defaults: telnet CR NUL, rlogin
  CR (the remote pty maps CR; CR LF gave double prompts), serial CR, rsh LF, raw CR LF.
* **Serial.** `flowControl` is applied: `rtscts`, `xonxoff` (Linux/macOS termios, Windows DCB; read back — a driver
  that ignores it is an error, not silence) and `dsrdtr` (Windows, macOS). Extra options `dtr`/`rts` (bool, default
  true) = line state at open. `stopBits: "1.5"` is Windows-only (refused elsewhere). Device paths must be character
  devices under `/dev` (`COMn`/`\\.\name` on Windows). A reconnect after an unplug waits ≤ 60 s for the device to come
  back (with `autoReconnect`). `GET …/serial/status` adds `flowControl`; `POST …/serial` answers 409 when the line is
  driven by flow control. `POST /api/serial/autobaud {device, rates?, probe?}` (`probe` sends CR at each rate); 409 =
  port busy, 400 = no data at any rate.
* **Telnet.** RFC 1143 option states (no redundant answers, no loops); NAWS is sent on resize in passive mode too;
  `localEcho` option; TLS defaults to port 992 when `tls` is set without a port.
* **Rsh.** Ctrl+D closes the command's stdin; `localEcho` defaults to true; the stderr back-channel listens on the
  connection's local address only and accepts the server's IP only; it needs a reserved port (root) — otherwise port 0
  is sent and stderr arrives merged.
* **Mosh.** The built-in client is a protocol client of our own (`internal/proto/mosh/ssp.go`) on mosh-go's wire
  primitives: mosh-go v0.5.2's Client never sends `throwaway_num`, so mosh-server ≥ 1.4 accepted only one new client
  state per 15 s after ~1024 keystrokes (reproduced), and it applied diffs regardless of their base state. The session
  ends when the remote shell exits (server shutdown state) and Close sends the shutdown so mosh-server and its shell do
  not linger. Options `moshServer` (remote path), `moshClient` (`builtin` default | `system` = installed mosh-client,
  which honours `predict`); the SSH editor's `remoteCommand` (run via `/bin/sh -c`) and `env` (passed through `env`,
  no AcceptEnv needed) are honoured, shell-quoted. Agent forwarding cannot apply (SSH ends after the bootstrap). The UDP target is the IP the SSH connection reached (resolved host name when SSH went
  through jump hosts/proxies); no reply within 15 s fails with a clear error. Feature flag `mosh` is always true.
* **Docker.** `dockerMode: "logs"` + `logTail` (default 200); exec sets `TERM` from `options.term`; Windows default
  engine `npipe:////./pipe/docker_engine`. Through SSH, `dockerHost` may name the remote `unix:///…` socket.
* **Kubernetes.** New `GET /api/kube/namespaces?context=` → `[{name, status?}]`; `kubeMode: "logs"` + `logTail`;
  kubectl is also looked up in the usual install locations (feature flag `kubectl` uses the same lookup).
* **WinRM.** NTLM over plain HTTP uses message encryption when the host is reached directly (Windows default policy
  rejects unencrypted WinRM); `noLocalEcho` (bool) turns the local line editor's echo off.
* **IPMI.** Connections configured with a proxy / SSH gateway are refused (UDP cannot follow). Power control UI:
  command `protocols.ipmi.power {connectionId?}`, session-tree and terminal context menus, and the IPMI editor.
* **Frontend.** Hex capture runs on the terminal bus (only while a monitor is open; replayed history excluded; TX
  includes hex sends); `options.hexView` (serial, raw) opens the monitor with the session. Commands
  `protocols.hexMonitor.toggle {sessionId?}`, `protocols.docker.containers {connectionId?, host?}` (also on SSH
  connections in the session tree), `protocols.ipmi.power {connectionId?}`.

### tools — `internal/tools`, `web/src/features/tools` (TOOL-4..9, CC-4, CC-17)
* **Job tools.** `POST /api/tools/{tool}` → `200 {jobId}` and streams results as `{type:'job'}` events; cancel with
  `POST /api/jobs/{id}/cancel`. Tools: `ping`, `traceroute`, `portscan`, `netscan`, `dns`, `whois`, `wol`,
  `httpcheck`, `tlscert`, `snmp` (CC-4), `sshaudit` (CC-17). Every job-data event is `{rows: Row[]}` where each row has
  a `kind` (`reply`/`hop`/`port`/`host`/`mac`/`record`/`text`/`var`/`category`/`finding`/`terrapin`/`result`/`cert`/
  `connection`/`info`/`progress`/`summary`); rows are coalesced (≤60 rows or 100 ms) so a wide scan never overflows the
  events hub queue. `ping`/`traceroute`/`portscan` accept `viaConnectionId` to run from a saved SSH host (`portscan`
  dials through the gateway's direct-tcpip; ping/traceroute exec the remote `ping`/`traceroute`, target arg validated).
* **Sync endpoints.** `GET /api/tools/interfaces`, `GET /api/tools/listening[?all=1]` (TOOL-6, gopsutil; each socket
  carries the owning pid/process/user + service name), `POST /api/tools/listening/kill {pid, signal?:'TERM'|'KILL'}`.
* **Gating.** In **server mode** `portscan`, `netscan`, `interfaces`, `listening` and `listening/kill` require an admin
  (SPEC principle 7 / RESEARCH TOOL-5); ping/traceroute/dns/whois/wol/httpcheck/tlscert/snmp/sshaudit are
  available to any authenticated user. Desktop mode allows all. No module tables (run history is client-side only).
* **Frontend.** Singleton `tools` tab `{tool?}` with a searchable tool list; command `tools.open {tool?}` plus one
  palette command per tool (`tools.<id>`); the ribbon "Tools" dropdown lists every tool.

### vnc — `internal/vnc`, `web/src/features/vnc` (PROTO-17, GFX-1..5, GFX-17, GFX-18)
* **WS `/ws/vnc/{sessionId}`** (binary; no subprotocol required, `binary` accepted). The owner attaches read-write;
  other admins attach **read-only** (audited `session.shadow`): Go drops their KeyEvent / PointerEvent / ClientCutText /
  SetDesktopSize / XVP / QEMU key messages, forces `shared`, and refuses pass-through servers. Each viewer is counted
  with `TrackClient` and gets its own upstream TCP connection (the server keeps the desktop; reconnect = new socket).
  The socket is upgraded first and errors are **close codes** (reason ≤ 120 bytes): 1000 server ended · 1001 shutdown ·
  4400 bad request · 4401 auth failed · 4403 forbidden · 4404 unknown session · 4409 reverse connection already used ·
  4410 session closed · 4423 vault locked · 4499 prompt canceled / certificate rejected · 4500 internal · 4502 connect
  failed / connection lost · 4504 timeout · 4505 no supported security type. Session state (`SetState`) follows the
  viewers: connecting / authenticating (live messages: "Negotiating TLS", "Waiting for the VNC password"…) /
  connected / disconnected / error. Audit: `vnc.connect`, `vnc.disconnect` (duration, bytes, reason).
* **Security termination** (Go ⇄ server; noVNC always gets RFB 3.8 + None + the server's ServerInit). RFB 3.3 / 3.7 /
  3.8 (and 3.889 / 4.x / 5.x as 3.8), UltraVNC repeater greeting. Handled in Go: None, VNC Authentication, **Apple
  Remote Desktop (30)** (deviation from the brief's pass-through list: macOS Screen Sharing works with vault
  credentials, the password stays server-side), VeNCrypt 0.2 Plain / TLSNone / TLSVnc / TLSPlain / X509None / X509Vnc /
  X509Plain. Preference: VeNCrypt (X509* > anonymous TLS* > unencrypted; Plain variants first when the connection has
  a user name, None variants first when no password is stored), then ARD when a user name is known, VNC Auth, ARD,
  None. **Anonymous TLS**: Go's crypto/tls has no anon (EC)DH suites, so `internal/vnc/anontls` is a minimal TLS 1.2
  client (DH/ECDH-anon with AES-GCM/CBC, X25519/P-256/P-384/FFDHE, extended master secret; verified against OpenSSL
  and GnuTLS/TigerVNC). It is unauthenticated (UI shows "Encrypted (server not authenticated)"); when the server
  cannot negotiate it, AstraTerm reconnects without it. X509 certificates: system roots for the host, else TOFU through a
  `hostkey` prompt (fingerprint `SHA256:AA:…`, `fingerprintMd5` = MD5 of the DER); X509 failures never downgrade.
  Other types noVNC supports (RA2ne, Tight, XVP, MS-Logon II) are **passed through** (noVNC asks for credentials in
  the tab; not stored).
* **Credentials.** `secrets.vncPassword`, else `secrets.password` (quick connect `password`); user name from
  `connection.username`, else asked (ARD / Plain). Missing or rejected passwords are prompted (`password`, `allowSave`
  for the owner's saved connection with an unlocked vault; saving stores `vncPassword` and fills an empty username,
  audited `connection.secret.save`); answers are remembered in session memory (`vncPassword`, `username`). Up to 4
  connection attempts (rejected password, connection lost while prompting, security fallback).
* **Options.** `shared` (ClientInit flag), `repeaterId` (UltraVNC repeater mode II: "ID:<id>"), `scaling`, `quality`,
  `compression`, `viewOnly` and `autoReconnect` are **viewer defaults** (applied by the frontend; `viewOnly` is not
  enforced server-side for the owner). Listener sessions carry `reverse`, `listenerId`, `listenAddress`.
* **Endpoints.** `GET /api/sessions/{id}/vnc-info` → `{sessionId, connected, viewers, host, port, serverVersion, protocol,
  security, encrypted, tls?: {version, cipherSuite, anonymous, group?, subject?, issuer?, notAfter?, fingerprint?,
  trust}, passthrough, desktopName, width, height, route, reverse, connectedAt?, lastError?}` (owner or admin).
  `GET /api/vnc/certs` → trusted certificates; `DELETE /api/vnc/certs/{id}` (desktop mode or admins; audited
  `vnc.cert.delete`; also `vnc.cert.trust|replace|mismatch_accepted`). Module table `vnc_trusted_certs` (migration
  vnc/1), global like known_hosts, one entry per host:port.
* **Listening mode (GFX-17).** `GET /api/vnc/listen` (own; admins `?all=1`), `POST /api/vnc/listen {port (default
  5500), bindHost? ("" = all interfaces, IP or localhost), password? (memory only), viewOnly?}` → 201 listener
  `{id, ownerId, bindHost, port, address, hasPassword, viewOnly, accepted, pending, lastFrom?, lastAt?, createdAt}`,
  `DELETE /api/vnc/listen/{id}`. Server mode: admins only. Listeners are in memory (not persisted; closed on
  shutdown); ≤ 8 per user, 30 accepts/min and ≤ 8 unclaimed connections per listener. Each accepted connection
  becomes a runtime session of the owner whose TCP stream is parked for one viewer (unclaimed after 2 min → closed)
  and is announced with `{type:'vnc.incoming', sessionId, listenerId, from, title}`; listener changes push
  `{type:'vnc.listeners'}`. Audit `vnc.listen.start|stop`, `vnc.reverse.accept`.
* **Frontend.** Tab kind `vnc` params `{sessionId, protocol, connectionId?, quick?, color?, title, reverse?}`; closing
  the tab closes the owner's session (confirm when connected), "Detach" keeps it; a tab whose session vanished (AstraTerm
  restart) starts an equivalent session once. Commands (category VNC): `vnc.attach {sessionId}`, `vnc.reconnect`,
  `vnc.disconnect`, `vnc.detach`, `vnc.ctrlAltDel` (Ctrl+Alt+End), `vnc.sendKeys {combo}`, `vnc.clipboard`,
  `vnc.pasteClipboard`, `vnc.typeClipboard`, `vnc.screenshot`, `vnc.copyScreenshot`, `vnc.fullscreen`
  (Ctrl+Alt+Enter; Keyboard Lock where supported), `vnc.viewOnly`, `vnc.scaleFit|scaleRemote|scaleNone`,
  `vnc.zoomIn|zoomOut|zoomReset`, `vnc.listen`, `vnc.connectRepeater`, `vnc.settings`. Settings section `vnc`:
  `scaling`, `quality`, `compression`, `clipboard` (auto|manual), `autoReconnect`, `dotCursor`, `keyboardCapture`
  (standard|all), `bell`, `openIncoming`, `typingDelay`, `showToolbar`. The desktop viewport is a `[data-terminal]`
  keybinding scope (plain Ctrl+key goes to the remote; `keyboardCapture = all` / fullscreen Keyboard Lock claims
  every key).

### rdp — `internal/rdp` (+ `internal/rdp/guac`), `web/src/features/rdp` (PROTO-16, GFX-1..9/11/18, CORE-16, CC-15)
* **Ticket.** `POST /api/sessions/{id}/rdp-ticket {width?, height?, dpi?, engine?, preconnectionBlob?}` (owner only) →
  `{engine:'ironrdp'|'guacd', token, expiresIn, destination, host, port, username, domain, password, width, height,
  dpi, fixedSize, resizeMethod, security, enableCredssp, preConnectionBlob?, clipboard, audio, microphone, drive,
  driveName?, printing, colorDepth, serverLayout?, via?, autoReconnect}` (superset of `RdpTicket`). The token (32 random
  bytes) is single-use, 60 s, bound to session + owner + engine + the destination resolved **now** (never the
  client's). `password` is only filled for IronRDP (its CredSSP runs in the browser). IronRDP: a missing password
  (security nla/vmconnect, or any + username) is asked through the prompt broker (kind `password`, `allowSave` for the
  owner's saved connection; saved only after the viewer reports `connected`); cancelling with security `any` connects
  without CredSSP (the server's logon screen); an empty username also disables CredSSP. Errors: 409 `guacd_unavailable`,
  400 `vm_id_required` (vmconnect without a VM id — the UI asks and resends), 400 `unsupported_security` (security
  `rdp` with IronRDP), 409 `credentials_required`, 423 locked, 410/404 gone. Engine = request `engine` > option
  `rdpEngine` > global default > `ironrdp`.
* **Relay `/ws/rdp/{id}`** (binary, no subprotocol; owner only): RDCleanPath v3390 (DER, `[n] EXPLICIT`, UTF8String).
  Dials through the generic Dialer (`sshTunnelVia` / `jumpHosts` / proxy; gateway prompts belong to the session),
  sends the pre-connection PDU v2 (Hyper-V) and the client's X.224 request, reads the TPKT confirm, TLS with
  `InsecureSkipVerify` + `VerifyConnection` (TLS ≤ 1.2 when HYBRID/HYBRID_EX; `legacyTls` enables TLS 1.0 + RSA
  suites), answers `{x224 confirm, server cert chain, server_addr}`, then pumps. Failures go into the RDCleanPath error
  (general: HTTP 400/403/499/502, WSA 10051/10054/10060/10061/10065/11001, TLS alert; negotiation error code 2 with the
  server's confirm) **and** into the session state message (the UI shows that). Standard RDP security (no TLS) is
  refused. If the TLS handshake fails after a slow certificate prompt the relay redials once without re-asking.
* **Certificate trust** (both engines): system roots + host name, else the global known_hosts table with
  `keyType:'rdp-tls'`, `publicKey` = base64 DER certificate, `fingerprint` = `SHA256:<base64>` of the DER; unknown →
  `hostkey` prompt (SHA-1 thumbprint in the message), changed → mismatch prompt (replacing: desktop mode or admins).
  `ignoreCert` skips it. The guacd engine probes the certificate this way first (X.224 + TLS through the Dialer) and
  then passes `ignore-cert` to guacd. Audit `known_host.add|replace`, `rdp.cert.mismatch_accepted`.
* **Tunnel `/ws/guac/{id}?token&width&height&dpi&audio*&image*&video*&timezone`** (subprotocol `guacamole`, owner
  only): internal-opcode UUID first (then pings are echoed while guacd connects / prompts are open), guacd handshake
  with the vault credentials (never sent to the browser), whole instructions per text frame (coalesced ≤ 64 KiB).
  **Lengths**: guacd side counts code points, browser side counts UTF-16 units (what guacamole-common-js 1.5 does).
  guacd `required` is answered by Go (stored values, else a `password` prompt → argv streams on reserved stream
  indexes 56–63, whose acks are swallowed). `disableClipboard` drops clipboard streams in both directions (besides
  guacd's `disable-copy/paste`). Errors reach the browser as a Guacamole `error` instruction (+ close reason
  "<status> <message>") and the session state. First `sync` → `connected`. Audit `rdp.connect`, `rdp.drive.upload`,
  `rdp.drive.download`. Gateway routes: guacd connects to a loopback forwarder (bound to `rdp.guacdForwardHost` when
  that is a local IP, e.g. the Docker bridge; `host.docker.internal` on Docker Desktop).
* **State.** `POST /api/sessions/{id}/rdp-state {state, message?, authFailed?}` (owner; IronRDP reports
  connected/disconnected/error since the protocol runs in the browser; `authFailed` forgets a remembered password).
  Every viewer is `TrackClient`ed; the last viewer leaving sets `disconnected` unless already ended; closing the
  session cancels its viewers and tickets.
* **guacd.** Address = global `rdp.guacdAddress` ("off" disables) else `--guacd`; the `guacd` feature probe is
  overridden accordingly. `GET /api/guacd/status` → `{configured, reachable, version?, defaultEngine, address?, source?,
  error?, sidecar?}` (address/source/error/sidecar for admins; version = guacd's protocol version, probed ≤ every
  10 min — reachability is a TCP connect, so polling does not spawn guacd processes). **CORE-16**
  `POST /api/guacd/sidecar {action:'start'|'stop'}` (admin) → 202 `{jobId}` (job events `{stage, message}`): docker CLI,
  container `astraterm-guacd` (label `astraterm.managed=guacd`; containers without it are never touched),
  `guacamole/guacd:1.6.0` published on `127.0.0.1:4822`, restart unless-stopped; on start saves
  `rdp.{guacdAddress, guacdSidecar, guacdDataPath:'/tmp/astraterm', guacdForwardHost}`. Global section `rdp` (admin):
  `guacdAddress`, `defaultEngine`, `guacdSidecar`, `guacdDataPath` (drives `<path>/drives/<userId>`, recordings
  `<path>/recordings/<userId>` inside guacd's filesystem), `guacdForwardHost`. Only the global scope is read.
* **Options beyond §5.3** (the sessions editor may expose them): `preconnectionBlob` (Hyper-V VM id; else asked at
  connect time), `preconnectionId`, `legacyTls`, `loadBalanceInfo`, `remoteApp`, `remoteAppDir`, `remoteAppArgs`;
  common `autoReconnect` (viewer reconnects with 1–30 s backoff after an unexpected drop, never after auth failures).
  `resizeMethod: 'none'` disables resizing. guacd gets every §5.3 rdp key (`enable-drive`/`drive-path`/
  `create-drive-path`, `enable-printing`, `disable-audio`/`enable-audio-input`, `gateway-*` with `gatewayPassword`
  or the password, `recording-*`, `console`, `server-layout`, `timezone`, `color-depth`, …).
* **CC-15.** `GET /api/sessions/{id}/rdp-file` (owner/admin) and `GET /api/connections/{id}/rdp-file` (visible
  connection; works while the vault is locked) → `.rdp` attachment without secrets. `POST /api/connections/{id}/
  launch-native` and `POST /api/sessions/{id}/launch-native` (desktop mode only, 403 `desktop_only`; 409
  `no_native_client`) → `{client, forwarded?, passwordInjected}`: mstsc (+ `cmdkey` credential removed after 30 s),
  macOS "Windows App" / "Microsoft Remote Desktop" (`open -a` + temp .rdp, 0600, deleted after 2 min), FreeRDP
  (`/from-stdin:force`, password on stdin) or Remmina; gateway routes get a loopback forwarder (closed after 2 min
  idle). Audit `rdp.native.launch`.
* **Frontend.** Tab kind `rdp` `{sessionId, protocol, connectionId?, quick?, color?, title, scaling?, engine?, vmId?}`
  (close = end session with confirmation; reopen re-attaches or starts anew; "session gone" → start new session in
  place). IronRDP is lazy-loaded; its WASM ships as a `data:` URL that the CSP's `connect-src 'self'` would block, so
  the module answers that one fetch from memory while it initializes (no CSP change needed). The web component is
  created with an unknown `scale` and a shadow-root stylesheet so AstraTerm sizes it (its own sizing is window-based);
  hover focus is reduced to click-to-focus. Commands (category Remote desktop): `rdp.attach {sessionId}`,
  `rdp.ctrlAltDel`, `rdp.sendKeys {combo}`, `rdp.fullscreen`, `rdp.screenshot`, `rdp.copyScreenshot`,
  `rdp.scaling {mode}`, `rdp.reconnect`, `rdp.disconnect`, `rdp.downloadFile {connectionId|sessionId}`,
  `rdp.launchNative {connectionId|sessionId}`. Host keys in the viewer: Ctrl+Alt+End = Ctrl+Alt+Del, Ctrl+Alt+Enter /
  Ctrl+Alt+Break = fullscreen (Keyboard Lock). While a connected desktop has focus every key goes to it
  (`claimKeys`). User settings section `rdpViewer`: `scaling` (resize|fit|none), `hiDpi`, `autoClipboard`,
  `keyboardLock`, `localCursor`. Settings → Remote desktop (`settings.open {section:'rdp'}`) also edits the admin
  global section and runs the sidecar.

### tunnels — `internal/tunnel`, `web/src/features/tunnels` (TUN-1..9)
* **Model.** `Tunnel` adds `options` (`reverse` — dynamic tunnel served on the SSH server, TUN-5; `bindSocket` /
  `destSocket` — Unix sockets, TUN-6; `socksUsername` + write-only secret `socksPassword`; `httpProxy` (default true:
  HTTP CONNECT / absolute-URI proxying and `/proxy.pac` on a dynamic port); `autoReconnect` (default true); `onDemand` +
  `idleTimeoutSec` (local/dynamic: SSH connects on the first client, disconnects when idle); `maxConns` (default
  1024); `allowFrom` (IPs/CIDRs); `scheme`, `color`, `icon`, `notes`), `sortOrder`, `secretKeys`, `secrets`
  (write-only) and `connection` (`{id, name, protocol, host, port, username}`). `type` stays local|remote|dynamic.
  `status` adds `connected, localAddr, remoteAddr, rateIn, rateOut, failedConns, lastError, lastErrorAt, reconnects,
  retryAt, waiting ('vault'|'client'|'demand'), warning?`. Defaults: bindHost `127.0.0.1` (`*` = all interfaces; remote binds
  may be host names), bindPort 0 = automatic / server-allocated, destHost `localhost`. Module table `tunnel_meta`
  (options, sealed secrets, secret names, sort order; deleted with its tunnel).
* **REST** (own tunnels only, others' → 404). `GET/POST /api/tunnels` (201), `GET/PATCH/DELETE /api/tunnels/{id}`
  (PATCH: present keys; `options` replaces; `secrets` merges; a running tunnel whose forward changed is restarted),
  `POST /api/tunnels/{id}/start|stop|restart` → tunnel (start: 409 port in use, 403 policy, 423 vault locked, 400
  invalid; failures other than 423 also become the tunnel's error status), `POST /api/tunnels/{id}/duplicate` (201),
  `POST /api/tunnels/start-all|stop-all {ids?}` → `{started, failed:[{id, name, error}]}` / `{stopped}` (a locked
  vault answers 423 so the UI unlocks and repeats), `POST /api/tunnels/reorder {items:[{id, sortOrder}]}`,
  `POST /api/tunnels/check-bind {bindHost, bindPort, bindSocket?, id?}` → `{available, error?, suggestion?}` (editor
  pre-flight; names the caller's tunnel / session forward holding the port), `GET /api/tunnels/export` →
  `{format:'astraterm-tunnels', version:1, exportedAt, tunnels:[{…, connection:{id, name, host, port, username}}]}` (no
  secrets), `POST /api/tunnels/import {file, defaultConnectionId?, dryRun?}` → `{created, planned:[{name, connectionId,
  connectionName, matched: id|name+address|address|name|default}], skipped:[{name, reason}]}` (imported tunnels are
  stopped, without autostart or SOCKS credentials), `GET /api/tunnels/remote-ports?sessionId=|connectionId=` →
  `{method: ss|netstat|proc|lsof|bsd|windows, ports:[{address, port, scope: loopback|all|address, connectHost,
  process?, pid?}], note?}` (TUN-9), `GET /api/tunnels/session-forwards[?sessionId=]`, `POST
  /api/tunnels/session-forwards {sessionId, type, bindHost?, bindPort?, destHost?, destPort?, reverse?, bindSocket?,
  destSocket?}` (201, live SSH session of the caller), `DELETE /api/tunnels/session-forwards/{sessionId}:{forwardId}`.
* **Events.** `{type:'tunnel', id, status, change?:'created'|'updated'|'deleted'}` (counters at most once a second per
  tunnel); `{type:'tunnel.session', sessionId, forwards: SessionForward[]}` (`{id, sessionId, sessionTitle,
  connectionId?, source:'connection'|'adhoc', spec, status}`); topic `tunnel.ports` `{sessionId}` → `{type:
  'tunnel.ports', sessionId, initial?, ports, added?, removed?, error?}` polled every 5 s (changes only after the
  initial list).
* **Runtime.** One supervisor per started tunnel holding its own pooled client (`Pool.Get`), so tunnels outlive
  terminals; liveness is the connection's SSH keepalive. Link lost → reconnect with backoff 1 s…60 s ±20 % (never after
  auth / host-key failures, unanswered prompts, deleted connections or policy refusals; `autoReconnect:false` → error).
  Listeners on this machine stay bound across reconnects (clients wait ≤ 30 s for the link); remote listeners are
  re-created. A manual start binds synchronously and a failing first connect ends in `error`; autostart (server start)
  waits for the vault unlock / a connected window (login prompts) and retries. Stale Unix sockets: local ones nobody
  listens on are replaced; a remote one is removed (`rm`, only when a direct-streamlocal probe is refused) and the
  forward retried once. Every 30 s, tunnels whose row or owner is gone, whose owner is disabled, or whose owner's new
  role violates the policy are stopped. GatewayPorts (TUN-3): after a remote forward bound to a non-loopback address
  starts (once per start; session forwards too), the read-only TUN-9 port probe runs once; if sshd bound the port to
  loopback only, `status.warning` says so (and the terminal gets a notice for session forwards).
* **Dynamic proxies.** SOCKS5 (go-socks5, names resolved on the exit side, CONNECT only — BIND / UDP ASSOCIATE
  refused), SOCKS4/4a (refused when credentials are set), HTTP CONNECT and absolute-URI requests (Basic
  Proxy-Authorization with credentials), `/proxy.pac` and `/wpad.dat`. A proxy on a non-loopback address needs
  credentials or `allowFrom`. Reverse SOCKS never connects to AstraTerm's own port.
* **Policy.** AstraTerm's own listen port is never bindable. Server mode, non-admins: loopback TCP listeners on ports ≥
  1024 only; no remote or reverse-dynamic forwards, no Unix socket listeners (session forwards included).
* **TUN-7.** `connection.options.forwards: [{type, bindHost?, bindPort?, destHost?, destPort?, reverse?, bindSocket?,
  destSocket?, name?, disabled?}]` start over the terminal's client (`Pool.ForSession`) whenever an SSH terminal
  session reaches `connected`, and stop on connecting / disconnected / error / close. Results are written into the
  terminal as notices (`Port forward active: 127.0.0.1:53211 → web:80`, `Port forward … failed: …`).
* **Frontend.** Tab kind `tunnels` (singleton). Commands (category Tunnels): `tunnels.open`, `tunnels.new {connectionId?,
  type?: local|remote|dynamic|rdynamic, name?, bindHost?, bindPort?, destHost?, destPort?}` (defaults to the active SSH
  terminal's connection), `tunnels.edit|start|stop|toggle {id}`, `tunnels.startAll`, `tunnels.stopAll`,
  `tunnels.detectPorts {connectionId?|sessionId?}`, `tunnels.forwardPort {sessionId?|connectionId?, port, host?,
  open?}`, `tunnels.sessionForwards {connectionId}`, `tunnels.import`, `tunnels.export`. The "Tunneling" ribbon button
  is re-registered with a drop-down (kept over the shell's placeholder). "Port forwarding" submenus on the
  `session-node` (SSH connections) and `terminal` (SSH sessions) context menus; status item `tunnels`; settings section
  `tunnels` (`confirmDelete`, `openWith: auto|proxy|direct`, `showSessionForwards`, `watchPorts`, `notifyErrors`,
  `showStatusItem`). "Open in browser" calls `webproxy.open {tunnelId, connectionId, host, port, scheme}` when that
  command is registered and preferred (server mode by default), else opens the tunnel's local URL.
* **Not implemented.** TUN-10 (Kubernetes / SSM port-forwards): client-go and ssm-session-client are not in go.mod.

### monitor — `internal/monitor`, `web/src/features/monitor` (MON-1..6, SSH-38 display)
* **Feed.** Events topic `monitor` (`{type:'subscribe', topic:'monitor', sessionId}`; `sessionId:'local'` = the AstraTerm
  host, desktop mode or admins). One feed per session (ref-counted by sockets) attaches to one collector per SSH
  transport (shared by duplicate sessions, stopped 10 s after the last subscriber): a one-shot probe (`uname`,
  os-release…) picks the sampler, which runs as `/bin/sh -s` (script on stdin, login-shell independent) on ONE non-PTY
  exec channel — Linux /proc loop of RESEARCH §3.21, macOS (iostat/vm_stat/netstat -ibn), FreeBSD/OpenBSD/NetBSD
  (kern.cp_time…), Windows OpenSSH (PowerShell `-EncodedCommand`, raw perf counters). Local shell sessions are
  sampled in-process with gopsutil. Pushes `{type:'monitor', sessionId, stats}` every 2 s; problems as
  `{type:'monitor', sessionId, error, state}` with the extra `state`: `waiting` (not connected yet — the feed
  re-attaches after reconnects), `unavailable` (exec refused / unsupported OS, retried every minute), `error`
  (transient, backoff 2 s…60 s), `closed`. The last event is replayed to new subscribers. Only the session owner may
  subscribe (as `Pool.ForSession`). Only local, non-network filesystems are passed to df (a hung NFS mount cannot
  stall the loop); bind mounts of one filesystem are merged.
* **Stats** is a JSON superset of `MonitorStats`: `cpu.{user,system,iowait,steal}`, `mem.cached`, `disks[].{avail,
  device}` (`disks[0]` is the primary volume: `/`, the macOS Data volume, `C:`; df "Use%" = used/(used+avail)),
  `net[].virtual`, `netTotal {rxBps,txBps}` (physical interfaces; all non-loopback ones in containers), `diskIo
  {readBps,writeBps}`, `fds {used,max}`, `threads`, `platform`, `arch`, `cpuModel`, `virt`, `intervalSec`, `warmup`
  (first sample: rates not yet known). `load` on Windows = [processor queue length, 0, 0].
* **REST** (`:id` = a runtime session id the caller owns — SSH or local shell — or `local` = the AstraTerm host, which
  is desktop mode or admins only; non-owners get 404): `GET /api/monitor/local` → SystemInfo (host, cpu, mem, disks,
  net interfaces, users, top processes, AstraTerm server); `GET /api/monitor/{id}/host` → probe result (`platform, os,
  kernel, hostname, arch, cpuModel, cores, virt, model, tools{sudo,systemctl,journalctl,ss,lsof,…}`);
  `/snapshot` (the collector's sample when fresh, else two readings 1 s apart); `/processes` → `Process[]` superset
  (`name, threads, nice, vsz, cpuTime`; `rss`/`vsz` bytes; `cpu` = % of one core, recent delta between listings,
  lifetime average on the first; the listing script itself is hidden); `POST /kill {pid, signal, sudo?}` (TERM KILL
  INT HUP QUIT STOP CONT USR1 USR2 or numbers 1/2/3/9/15; PIDs ≤ 1 and AstraTerm itself refused); `POST /renice
  {pid, nice, sudo?}`; `GET /services` → `{manager:'systemd'|'windows'|'', services:[{name, description, load,
  active, sub, enabled?, pid?}], message?}`; `POST /services/{name}/{action} {sudo?}` (start stop restart reload
  enable disable); `GET /services/{name}/logs?lines=&sudo=` → `{lines}` (journalctl); `GET /ports?sudo=` →
  `[{proto, address, port, pid?, process?, user?}]`; `GET /du?path=&sudo=` → `{path, parent?, total, entries:[{name,
  path, size, dir}], partial?, warning?}` (du -xk -d 1; "(files)" entry for the files directly inside);
  `GET /ssh-info` → `SSHConnInfo` + `probeMs` (keepalive round trip now, −1 = stall), `remoteAddr, localAddr, user,
  host, port, pq, jumpHosts, proxy, authMethod, compression, agentForwarding, x11Forwarding, keepAliveSec,
  connectedAt, measuredAt`. Errors: 403 `permission_denied` (the UI offers "retry with sudo"), 403
  `sudo_cancelled`, 422 `monitor_unavailable` / `command_failed` / `sudo_unavailable`, 502 `monitor_failed`
  (transport). Audit: `monitor[.local].kill|renice|service.<action>|tail`.
* **Log follower (MON-5).** WS `/ws/monitor/{id}/tail?path=…[&path=…]` (≤ 8 files) or `?journal=1[&unit=]`,
  `&lines=` (≤ 5000), `&sudo=1`. Frames: `{type:'start', sources}`, `{type:'lines', lines:[[sourceIndex, text]…]}`,
  `{type:'error', message}`, `{type:'end', code, message?}`; client `{type:'stop'}` or close. The remote `tail -F`
  runs under a stdin watchdog, so it dies with the channel (non-PTY execs get no SIGHUP).
* **sudo.** `sudo -n` first; then the session's `sudoPassword` secret, then its login password, then a `password`
  prompt through the broker ("remember" saves it as the connection's `sudoPassword`, audited
  `connection.secret.save`). The password is only fed to `sudo -S` through a quoted here-document with a random
  delimiter (never on a command line); a working one is remembered in session memory.
* **Frontend.** Status item `monitor.bar` (left, order 30): the monitoring bar for the ACTIVE tab's session
  (`params.sessionId`; a monitor tab's target) when SSH (and local shells, setting) — host, CPU, RAM, swap, disk of
  the primary volume, net ↓↑, load, users, uptime, processes, with sparklines and warn/critical colours (80/90 %);
  hover = details, click = monitor tab on the matching panel; sampling only while shown and a AstraTerm window is
  visible. Tab kinds `monitor` `{target, panel?, path?, title?}` — the
  session is `params.target`, deliberately not `params.sessionId` (the shell / terminal would treat the monitor tab as
  a tab showing the session: closing the terminal would keep the session alive); `sessionId` is still accepted — and
  `sysinfo` (singleton). Commands: `monitor.open {sessionId?|target?, panel?}` (default: the active tab, else the
  focused terminal; `local` → System information), `monitor.toggleBar {enabled?, sessionId?}` (global setting, or
  per session like MobaXterm's SFTP-panel checkbox; the definition carries `checked()`; also
  `useMonitorBarShown(sessionId)` / `useMonitorBarEnabled()` in `features/monitor/settings`), `monitor.processes|
  services|ports|diskUsage|logs|connectionInfo`, `monitor.systemInfo`, `monitor.taskManager`.
  Menus: View "Remote monitoring bar" (checked), Terminal "Monitor host",
  Tools "Monitor host / System information / Task manager"; context menus `terminal` ("Monitor host ▸")
  and `tab` (terminal tabs). Settings section `monitor`. The Ports panel uses `tunnels.forwardPort` (else
  `tunnels.new`) and `webproxy.open` when registered.

### monitor — review addendum (additive)
* **`/ssh-info` adds** `hostCert` (the server used an OpenSSH host certificate), `reconnects`, `firstConnectedAt`,
  `lastDisconnectAt`, `lastDisconnect` (reason) and `termBytesIn` / `termBytesOut` — bytes of the terminal channel only
  (SFTP, port forwards and monitoring on the same transport are not counted); recorded from term hooks for every SSH
  session. The Connection panel derives terminal throughput from them.
* **Sampler.** A sampler producing no sample for max(8 × interval, 30 s) is ended, reported (`state:'error'`) and
  restarted with the usual backoff. The Linux sampler no longer passes container / system runtime mounts to df
  (`/var/lib/{docker,containers,kubelet,lxcfs}`, `/run` except `/run/media`, `/snap`, `/proc`, `/sys`, `/dev`).
* **Local host.** Disks come from all mounts minus pseudo / network / runtime ones (a Docker-deployed AstraTerm listed
  none); bind mounts merge; the volumes of an APFS container are reported as the Data volume. macOS memory follows
  Activity Monitor (vm_stat), as the remote macOS sampler does. Service names may contain systemd `\xNN` escapes.
* **Bar.** Degrades to fit the status bar (sparklines → users/uptime/processes/swap → host/load → CPU/RAM/disk) with
  a "⋯" item listing what is hidden; load is orange from 1 runnable task per core and red from 2 (Windows queue 2 / 4),
  not by the % thresholds. Per-session choices (`monitor.toggleBar {sessionId}`) persist in localStorage
  (`astraterm:monitor:bar-sessions:v1`; dropped when the session closes, after 30 days, beyond 200).

### keys — `internal/keys`, `web/src/features/keys`, `internal/sshx/{cert_hostca,agent_builtin}.go` (TOOL-1, SSH-5, SSH-11, SSH-12 built-in keyring, SSH-19, SSH-20, SM-7 UI)
* **Keys.** `SSHKey` responses add `fingerprintMd5, hasPrivateKey, passphraseSaved` (an encrypted key's passphrase is
  remembered in the vault), `certificateInfo?` (`CertInfo {type: user|host, keyId, serial (decimal string),
  principals, validAfter?, validBefore?, criticalOptions, extensions, keyType, keyFingerprint, caKeyType,
  caFingerprint, signatureType, status: valid|expired|not_yet_valid}`) and `usedBy {connections, identities}`. The
  imported text is stored as is when the SSH core reads it (OpenSSH, PEM, PKCS#8); PPK and PBES2 PKCS#8 are stored as
  OpenSSH with the same passphrase (DSA as PEM). One copy per public key and user (409 `key_exists`, the message names
  it). Key text ≤ 64 KiB; imported RSA keys 1024…16384 bits. Errors (400): `passphrase_required`,
  `wrong_passphrase`, `invalid_key`, `unsupported_key`, `public_key_only`, `certificate_mismatch`.
* **REST — keys** (own keys only, others → 404): `GET /api/keys`, `GET /api/keys/{id}`; `POST /api/keys/generate
  {name, type: ed25519|rsa|ecdsa, bits? (rsa 2048/3072 default/4096; ecdsa 256/384/521), comment? (default
  "<type>-key-YYYYMMDD"), passphrase?, rememberPassphrase? (default true), store? (default true)}` → 201 key, or with
  `store:false` a draft `{draftId, type, bits, publicKey, fingerprint, fingerprintMd5, comment, hasPassphrase,
  expiresAt}` held in server memory (30 min, ≤ 10 per user) for MobaKeyGen's "save before storing":
  `POST /api/keys/drafts/{id}/export {format, ppkVersion?}`, `POST /api/keys/drafts/{id}/store {name,
  rememberPassphrase?}` (201), `DELETE /api/keys/drafts/{id}`. `POST /api/keys/import {name?, privateKey, passphrase?,
  comment?, rememberPassphrase?, certificate?}` (OpenSSH; PEM PKCS#1 / SEC1 / DSA incl. legacy `Proc-Type`
  encryption; PKCS#8 incl. PBES2; PuTTY PPK v2/v3) → 201. `POST /api/keys/inspect {text, passphrase?}` → `{kind:
  private|public|certificate, format?, ppkVersion?, encrypted, needsPassphrase, wrongPassphrase, type?, bits?,
  fingerprint?, fingerprintMd5?, publicKey?, comment?, certificate?: CertInfo, existingKeyId?, existingKeyName?}` (live
  preview: passphrase problems are flags, not errors). `POST /api/keys/convert {privateKey, passphrase?, format,
  newPassphrase?, comment?, ppkVersion?, name?}` → export result, nothing stored. `PATCH /api/keys/{id} {name?,
  comment?, certificate? ("" detaches), passphrase? (remember it; "" forgets), newPassphrase? (re-encrypt; ""
  removes), rememberPassphrase?}`. `DELETE /api/keys/{id}`.
* **Export.** Formats `openssh | ppk | pem | pkcs8 | public` (authorized_keys line) `| rfc4716`. `GET
  /api/keys/{id}/export?format=&ppkVersion=2|3` returns text as documented (default `public`; a private format keeps
  the stored protection: unencrypted stays unencrypted, encrypted needs the remembered passphrase). `POST
  /api/keys/{id}/export {format, keepPassphrase?, passphrase? (new one; "" = unencrypted), currentPassphrase? (when not
  remembered), ppkVersion?}` → `{content, filename, mime, encrypted}`, so passphrases never travel in URLs. PPK is
  written by AstraTerm (v3 Argon2id 8 MiB / 16 passes, default; v2 SHA-1 KDF); OpenSSH via x/crypto (bcrypt KDF);
  encrypted PKCS#8 = PBES2 PBKDF2-SHA256 / AES-256-CBC.
* **Install (ssh-copy-id).** `POST /api/keys/{id}/install {connectionId}` → `{installed, alreadyPresent, method:
  sftp|shell, path, target}` (502 `connect_failed` / `install_failed`) over the pooled client: SFTP (mkdir `~/.ssh`
  0700, dedupe by key blob ignoring `cert-authority` lines, O_APPEND with a missing final newline fixed, chmod 0600,
  read back, `restorecon` under SELinux), else `sh -s` with the script on stdin. Appending (not RESEARCH's "atomic
  rename") keeps the file's inode, owner, ACLs and SELinux label, as ssh-copy-id does.
* **Certificates (SSH-5).** `PATCH {certificate}` / import `certificate` attach an OpenSSH user certificate (its key
  must match, the CA signature must verify); the SSH core already presents `SSHKey.certificate` with
  `ssh.NewCertSigner`. `POST /api/keys/{id}/sign` (key {id} = the CA) `{publicKey | subjectKeyId, certType:
  user|host, identity, principals (≥ 1), validAfter?, validBefore? (RFC 3339; none = forever), criticalOptions?
  (force-command, source-address, verify-required), extensions? (user certs; default ssh-keygen's permit-*), serial?,
  caPassphrase?, attach?}` → `{certificate, info, filename, attached}` (RSA CAs sign rsa-sha2-512; DSA CAs refused).
* **Known hosts (SSH-19).** `GET /api/known-hosts?q=`; `POST /api/known-hosts {host, port?, publicKey, comment?,
  replace?}` → 201 (409 `already_known`; 409 `host_key_conflict` when another key of that type is trusted — resend
  with `replace:true`); `DELETE /api/known-hosts/{id}`; `POST /api/known-hosts/bulk-delete {ids}`; `POST
  /api/known-hosts/import {text | source:'system' (~/.ssh/known_hosts of the AstraTerm host: desktop mode / admins),
  format: auto|openssh|putty (PuTTY `.reg` export, MobaXterm.ini `[SSH_Hostkeys]`), onConflict: skip|replace|add}` →
  `{format, added, replaced, skipped, conflicts, hashed, patterns, markers, invalid, errors:[{line, error}]}` (hashed
  host names and wildcard plain entries cannot be imported and are only counted); `GET
  /api/known-hosts/export[?hashed=1]` → known_hosts text including the markers. Server mode: only admins change known
  hosts and markers (403); everyone may list and export.
* **Markers (SSH-20).** `GET/POST /api/known-hosts/markers {marker: cert-authority|revoked, hosts (comma-separated
  OpenSSH patterns, `!` negation), publicKey | keyId, comment?}`, `DELETE /api/known-hosts/markers/{id}`; module table
  `keys_host_markers` (migration "keys" v1). Patterns are matched against both `host` and `[host]:port` (a negation of
  either wins).
* **sshx hooks** (new files `internal/sshx/cert_hostca.go`, `agent_builtin.go`; one-line calls added in existing
  files). `hostkey.go` `check()` first calls `checkMarkers`: a key — or a certificate's key or CA — marked @revoked is
  rejected (audit `ssh.hostkey.revoked`); a host certificate signed by a CA trusted for the host (principal = host
  name, validity window) is accepted without a prompt (session notice); any other certificate is checked as its plain
  key (OpenSSH semantics); re-keys accept the same or a renewed certificate of the same key. `dial.go` uses
  `hk.algorithms(known, opts)` (certificate host-key algorithms first when a CA covers the host). `auth.go`
  `signers()` calls `a.dialAgent()`, `agent.go` `serveAgentChannel()` calls `c.dialForwardAgent(ctx)` /
  `c.fallbackKeyring()` (below). Registration is per `*sshx.Pool` (`SetHostKeyMarkers`, `SetBuiltinAgent`) and is
  dropped when the pool's context ends.
* **Built-in agent (SSH-11; desktop mode, else 403).** `GET /api/agent/status` → `{supported, reason?, running, owner,
  socketPath?, platform, startedAt?, keyCount, locked, lockedByClient, vaultLocked, autostart, clients, signatures,
  lastUsedAt?}`; `POST /api/agent/start|stop` (409 `agent_unavailable` when the endpoint cannot be created, 409 when
  running for another user); `GET /api/agent/keys` → `[{id: "k:<keyId>"|"a:<hash>", source: stored|added, keyId?,
  name, type, bits, fingerprint, comment, loaded, excluded, unloaded, unlocked, needsPassphrase, confirm, certificate,
  expiresAt?}]`; `DELETE /api/agent/keys/{id}` (ssh-add -d); `POST /api/agent/reload` (offer removed keys again, forget
  decrypted keys and remembered confirmations); `POST /api/agent/lock|unlock` (unlock also clears an `ssh-add -x`
  lock). Endpoint: Unix socket `<data dir>/agent.sock` (0600; in a private 0700 `$TMPDIR/astraterm-agent-<uid>-<hash>/`
  when that path is too long; peer uid checked), Windows pipe `\\.\pipe\astraterm-ssh-agent-<hash>` with a
  current-user DACL. Offers the user's stored keys (certificates first) and keys added with `ssh-add` (memory only;
  `-t` / `-c` honoured, other constraints refused); missing passphrases are asked through the prompt broker (kind
  `passphrase`; "save" remembers); optional confirm-on-use (kind `confirm`, naming the key, the local program (pid /
  name) or forwarding server and — from `session-bind@openssh.com` — the destination host). Decrypted keys are purged
  when the vault locks. Audit `agent.start|stop|lock|unlock|sign.denied|forwarded.sign`; feature `features.sshAgent`.
* **Agent in AstraTerm's SSH connections (SSH-12, built-in keyring part).** While the built-in agent runs for the user,
  logins offer its keys after the connection's own key, merged with the host agent (its prompts pause the handshake
  deadline), and agent forwarding serves it read-only, merged with the host agent. Where the host agent is not used
  (server-mode non-admins, or no host agent), forwarding serves this module's keyring of the user's stored keys instead
  of the static keyring: excluded keys left out, optional confirmation naming the requesting server, passphrase
  prompts.
* **Settings** section `keys` (user over global). Backend-read: `agentAutostart` (the desktop user's value, at start),
  `agentConfirm`, `agentForwardConfirm`, `agentExclude: keyId[]`, `agentAutoLockMin`, `agentKeyLifetimeMin` (0 = off,
  ≤ 1 week). Frontend-only: `defaultType`, `defaultRsaBits`, `defaultEcdsaBits`, `ppkVersion`, `rememberPassphrase`.
  Audit: `key.create|update|delete|export|draft.export|install|sign|certificate.attach|certificate.detach|
  passphrase.change|passphrase.forget|passphrase.remember`, `known_host.add|replace|delete|bulk_delete|marker.add|
  marker.delete`, `known_hosts.import`.
* **Frontend.** Tab kind `keys` (singleton; `params.tab`: keys | identities | knownHosts | agent). Commands (category
  "SSH keys"): `keys.open {tab?}`, `keys.generate`, `keys.import {text?}`, `keys.convert`, `keys.identities`,
  `keys.identity.new {name?, username?, keyId?}`, `keys.knownHosts`, `keys.knownHosts.import`, `keys.agent`,
  `keys.agent.start|stop` (desktop), `keys.agent.toggle` (hidden), `keys.install {keyId?, connectionId?}`. The "Keys"
  ribbon button is re-registered with a drop-down (kept over the shell's placeholder); `session-node` context menu
  "Install SSH key…" (ssh / sftp / mosh); status item `keys.agent` (right, 70; desktop mode while the agent runs);
  overlay `keys`; settings section `keys` ("SSH keys & agent"). Identities (SM-7) use the core `/api/identities`.

### keys — review addendum (supersedes the matching points above)
* **OpenSSH key ciphers.** Import / inspect / convert read every cipher `ssh-keygen -Z` offers: aes128/192/256-ctr and
  -cbc, 3des-cbc, aes128-gcm@openssh.com, aes256-gcm@openssh.com and chacha20-poly1305@openssh.com (a failing AEAD tag
  is `wrong_passphrase`). An unknown cipher is `unsupported_key` before any passphrase is asked for; data after the key
  is `invalid_key`. Keys whose cipher x/crypto cannot read, and PuTTY keys (now as documented above), are stored as
  OpenSSH (aes256-ctr + bcrypt) with the same passphrase.
* **Expensive crypto.** Key derivations of imported files (bcrypt_pbkdf ≤ 2048 rounds, PPK Argon2 ≤ 128 MiB / 64 passes
  / 8 lanes, PBKDF2 ≤ 5 M iterations), encrypting exports and RSA generation share 2–4 process-wide slots; a request
  that waits more than 20 s gets 429 `too_many_requests` (+ Retry-After).
* **Install.** `connect_failed` / `install_failed` are **422** (not 502: the router replaces the message of every 5xx,
  and the dialog must show the reason). The SFTP path writes at the explicit end offset — servers that ignore
  `SSH_FXF_APPEND` (e.g. pkg/sftp's) otherwise overwrite the start of authorized_keys — refuses when the file changed
  between reading and writing, checks the old content survived, and is bounded (60 s) and cancellable.
* **Agent endpoint.** `<data dir>/agent.sock` only while the data directory is a real directory of this user without
  group/other access; otherwise (or when the path is too long) the private `$TMPDIR/astraterm-agent-<uid>-<hash>/`.
  Endpoint problems → 409 `agent_unavailable` with the reason.
* **Agent lock.** A locked built-in agent answers signature requests like "key not found", so the merged agents
  (logins, forwarding) still sign with the host agent's keys; it refuses extensions (`session-bind`) like ssh-agent.
  `ssh-add -X` attempts are serialized; each wrong passphrase adds 100 ms (≤ 10 s) to the answer, reset by a
  successful unlock, a AstraTerm unlock or a restart. Decrypted stored keys are dropped as soon as a locked vault is seen.
* **Markers.** Hashed names (`|1|salt|hmac`) are valid marker patterns (kept verbatim, may be negated with `!`).
* **sshx hook.** `checkMarkers` also accepts, at re-key, the host key accepted through its certificate when it is
  presented without the certificate (was a spurious "different host key during re-keying").

### tunnels — review addendum (supersedes the matching points above)
* **Traffic counters** (`bytesIn/bytesOut`, rates) count the payload relayed to and from destinations: SOCKS / HTTP
  CONNECT negotiation and locally answered requests (`/proxy.pac`, refused clients) are not traffic.
* **Open-proxy guard.** An `allowFrom` list containing `0.0.0.0/0` or `::/0` does not satisfy "credentials or allowed
  clients" for an unauthenticated proxy on a non-loopback address (400). Proxy clients must finish the negotiation
  (SOCKS greeting/auth/request, or the first HTTP request header) within 30 s — also reverse-proxy clients, which
  arrive as SSH channels without deadline support. The PAC file lists `PROXY host:port; SOCKS5 host:port` when the
  proxy has credentials (browsers cannot authenticate to SOCKS), else `SOCKS5; SOCKS; PROXY`; never `DIRECT`.
* **Stopping never hangs on a dead link.** Closing a remote (`-R`) listener waits at most 2 s for the server's
  `cancel-*-forward` reply (the rest finishes in the background), as does waiting for connection handlers; on a
  healthy link a restart still waits for the cancellation, so a fixed remote port is accepted again.
* **Reconcile** (every 30 s) also stops a tunnel whose SSH connection is no longer visible to its owner (e.g. no longer
  shared). **Delete** stops a runner registered by a concurrent start.
* **API additions.** `POST /api/tunnels {…, start: true}` starts the new tunnel (failures land in its status, 201
  either way). `POST /api/tunnels/import` → `planned[].warning` flags tunnels listening on non-loopback addresses.
* **TUN-9.** The port probe runs as `sh -c '<one-line script>'`, so fish / csh / tcsh login shells work. Topic
  `tunnel.ports`: one poller per SSH session shared by every subscribed window; a window joining a running poller
  receives the last list at once (`initial: true`).
* **Session forwards.** Terminal notices mark listeners reachable from other machines; an ad-hoc forward added while
  its session closes is refused (409) instead of being left unsupervised.
* **Frontend.** Saving a tunnel or session forwards that newly listen on a non-loopback address asks for explicit
  confirmation (editor, session-forwards dialog; the import preview shows warnings). Session-forward rows are validated
  like the server (local listeners: IP / localhost / `*`; no unauthenticated proxy off loopback); a reverse SOCKS proxy
  off loopback needs credentials. "Open in browser" / "Forward & open" use the tunnel address only when this browser
  can reach it (UI served from a loopback host, or a non-loopback bind), else the web proxy when registered, else they
  explain why.

### vnc — review addendum (supersedes the matching points above)
* **No silent downgrade — encryption policy** (`options.encryption`: `require` | `prefer` (default) | `allow-weak` |
  `allow-unencrypted`; replaces "when the server cannot negotiate it, AstraTerm reconnects without it"). Under `prefer`,
  a refused/failed anonymous-TLS handshake, an anonymous-TLS DH group of 1024–2047 bits, an unusable VeNCrypt offer, or
  VeNCrypt Plain without TLS (clear-text password) stops the viewer with close code **4426** instead of connecting;
  `vnc-info.confirm = {reason, weakTls, dhBits?, unencrypted, cleartext}` describes the choice. The viewer reconnects
  with `/ws/vnc/{id}?allow=weak|unencrypted` after the user's explicit decision; the owner's decision is remembered for
  the runtime session (not persisted unless the user ticks "Remember", which PATCHes `options.encryption`).
  `require` accepts VeNCrypt TLS only (4505 otherwise; `?allow` cannot override it); `allow-weak` accepts ≥ 1024-bit
  anonymous DH; `allow-unencrypted` is the old fallback. Servers that offer no encryption at all still connect under
  every policy but `require` (badge "Not encrypted"). VNC Authentication is preferred over VeNCrypt Plain without TLS.
  Reverse (listener) sessions fail instead of asking (their stream cannot be retried). X509 failures still never fall
  back.
* **vnc-info additions**: `encryptionPolicy`, `downgrade`, `passwordCleartext`, `confirm`, `clipboard`; `tls` adds
  `dhBits`, `weak`, `encryptThenMac`. Audit `vnc.connect` adds `encryptionPolicy` and, when relevant, `downgrade`,
  `weakTls`, `clipboard`.
* **Anonymous TLS** (`internal/vnc/anontls`): DH groups below 2048 bits are refused by default (standard RFC 7919 /
  RFC 3526 groups are recognized; other moduli must pass a primality test; 512-bit exponents only for the standard
  safe-prime groups); encrypt-then-MAC (RFC 7366) for CBC suites, MAC-then-encrypt hardened like crypto/tls (Lucky 13);
  at most 16 consecutive empty / warning / HelloRequest records; record version pinned after ServerHello; writes stop
  after a fatal alert or CloseWrite; AES-256 suites offered first (OpenSSL's automatic DH sizing uses 1024 bits for
  AES-128 anonymous suites); secrets wiped after use. Fuzz targets `anontls`: `FuzzServerStream` (deterministic replay
  corpus in `testdata/fuzz`, incl. OpenSSL and TigerVNC conversations; regenerate with `TestWriteFuzzCorpus`),
  `FuzzRecordOpen`, `FuzzPaddingLen`; `internal/vnc`: `FuzzRFBHandshake`, `FuzzForwardClientMessages`. Test helper
  `anontls/anontlstest` = an independent anonymous TLS server (used by `vnctest.Server.AnonTLS`).
* **ARD**: Diffie-Hellman keys below 1024 bits are refused; credential buffers and DH secrets are wiped after use.
* **Clipboard direction (GFX-2)**: `options.clipboardDirection` (`both` | `to-remote` | `from-remote` | `none`) ∩ the
  administrator's global settings section `vncPolicy.clipboardDirection` (read from the global scope only; Settings →
  VNC → "Policy for all users") ∩ the user's `vnc.clipboardDirection` setting. Local → remote is enforced server-side
  (ClientCutText dropped, the Extended Clipboard pseudo-encoding removed from SetEncodings; browser-side authentication
  is refused while local → remote is blocked), remote → local by the viewer (from `vnc-info.clipboard`); typing
  clipboard text is disabled along with local → remote.
* **Viewer**: settings `vnc.hiDpi` (remote resize in device pixels, displayed 1:1) and `vnc.clipboardDirection`; the
  security badge states the level in words ("Not encrypted" / "Weak encryption" / "Encrypted · unverified" /
  "Encrypted") and its popover edits the saved connection's `encryption` / `clipboardDirection` (owner or admin — the
  sessions editor does not show these keys yet; editor owners may add them).

### automation — `internal/automation`, `web/src/features/automation` (AUTO-1, AUTO-4..12, TERM-15, TERM-17 pacing, TERM-33, SEC-21)
* **Location.** The snippets & macros REST of §6.0 lives in `internal/automation` (no `internal/snippets` package);
  core tables/types unchanged. Extras: `GET /api/snippets/{id}`, `GET /api/macros/{id}`, `POST /api/macros/{id}/run
  {sessionIds, speed? (0 = no delays, default 1, max 100), confirmDangerous?}` → `{jobId}` (backend replay, keeps timing
  in background tabs). Macro limits: 10 000 steps, 64 KiB per step, 1 MiB total, delay ≤ 10 min. `POST
  /api/snippets/{id}/run {sessionIds (≤ 256, own sessions), variables?, sendMode?, confirmDangerous?}` → `{results:
  [{sessionId, ok, error?}]}`; unresolved placeholders → 400. Placeholder grammar (Go + TS): `{{name}}`,
  `{{name|default}}`, `{{name|a|b|c}}` (choices), `{{name:secret}}` (masked, never remembered), `\{{` literal; built-ins
  `host port user title protocol date time datetime timestamp clipboard` (clipboard browser-only).
* **Dangerous-command guard (SEC-21).** Server-side and authoritative for snippet runs, macro replays, paced sends,
  batch runs and schedules: `409 {error, code:'dangerous_command', matches:[{rule, message, severity, line}]}` unless
  the body has `confirmDangerous:true`. 20 built-in RE2 rules (danger; `warning` rules only with `guardStrict`) + custom
  rules. Settings section `automation` keys `guardEnabled` (default true), `guardStrict`, `guardCustom:[{pattern,
  message?}]` are read server-side (global scope, then the user's). `POST /api/automation/guard/check {text, typed?}` →
  `{enabled, matches}`. The browser additionally guards Enter in terminals (MultiExec broadcast always; typed input per
  `guardTyped: 'off'|'production'|'always'`, production = connection tag in `productionTags`).
* **Logon actions (AUTO-8).** `ConnectionOptions.logonActions: [{expect?, send?, secret?, enter? (default true),
  timeoutSec? (default 20, max 600), delayMs?, optional?}]` (≤ 32). Run server-side after every (re)connect via
  `term` hooks, capturing output from `connecting` so the first prompt is not missed; `expect` is an RE2 regex over
  ANSI-stripped output; `secret` names a stored secret (`password`, `sudoPassword`, …) typed server-side. A failed step
  stops the sequence with a terminal notice. `startupCommand` is still sent by `term` at connect (before the steps).
* **Secrets (AUTO-9 / TERM-33).** `POST /api/sessions/{id}/inject-secret {key, enter? (default true)}` (owner's
  connected session; 404 `secret_not_found`, 409 not connected, 423 vault locked, 403 when the connection is shared
  and the caller is not allowed to modify it) — audited as `session.inject_secret {key, connectionId}`, the value never
  leaves the server. `GET /api/sessions/{id}/secret-keys` → `{keys, injectable, locked?}`.
* **Triggers (AUTO-7).** `GET/POST /api/automation/triggers`, `PATCH/DELETE /api/automation/triggers/{id}`: `{name,
  enabled, pattern (RE2), caseSensitive, scope:{connectionIds?, protocols?, tags?}, actions:[{type: highlight|notify|
  sound|send|log|runScript|runSnippet, …}], cooldownMs (default 2000, min 250), once (per connection), sortOrder}`.
  Evaluated in the backend on ANSI-stripped complete lines of every session of the owner (per-session token bucket
  20 burst / 4 per s; alternate screen skipped), so they work with no browser attached; `highlight` is applied by the
  browser. Event `{type:'automation.trigger', triggerId, name, sessionId, sessionTitle, connectionId?, line, match,
  notify?:{title, message?, level, desktop?}, sound?, logged?, stats:{hits, lastHitAt}}` (stats are in memory).
  `GET/DELETE /api/automation/trigger-log` (last 1000 per user).
* **Paced send (TERM-17 reuse, AUTO-6).** `POST /api/automation/send {sessionIds, text, lineDelayMs?, charDelayMs?,
  waitPrompt?, promptPattern?, promptTimeoutMs?, enter?, confirmDangerous?}` → `{jobId}`; job data
  `{kind:'progress'|'done'|'warning', sessionId, done, total, ok?, error?, message?}`. Waiting for the prompt uses OSC
  133 marks when the shell emits them, else `promptPattern` (default `[$#%>❯»:\]]\s*$`) after output settles.
  Helpers: `POST /api/automation/regex/test {pattern, caseSensitive?, text}`, `POST /api/automation/template/vars
  {content}`, `GET /api/automation/capabilities` → `{scripts, mode, maxTargets, maxParallel, defaultTimeoutSec}`.
* **Scripts (AUTO-10).** `GET/POST /api/scripts`, `GET/PATCH/DELETE /api/scripts/{id}`, `POST /api/scripts/{id}/run
  {sessionId?|connectionId?, variables?, timeoutSec? (default 600, max 86400), content? (unsaved editor text)}` and
  `POST /api/scripts/run {content, name?, …}` → `{jobId, runId}`; job data `{kind:'log', level, text, ts}`; cancel with
  the Jobs API. goja sandbox (no require/fs/network; interrupt on timeout/cancel; ≤ 8 concurrent per user, 64 total).
  API: `session.send/sendLine/sendSecret/expect/waitFor/waitIdle/waitPrompt/run/screen/exec/close`, `sessions.open/get/
  list`, `connections.list`, `sleep`, `log/console.*`, `prompt/confirm` (prompt broker), `vars`, `exit`. **Server mode:**
  scripts (and script actions of triggers/batches/schedules) are admin-only unless the global admin setting
  `automation.userScripts` is true.
* **Batch & schedules (AUTO-11/12).** `POST /api/automation/batch {name?, connectionIds (≤ 256), kind: command|snippet|
  script, command?|snippetId?|scriptId?, variables?, mode: auto|exec|session, parallel (1..32, default 4), timeoutSec
  (default 60), stopOnError?, confirmDangerous?}` → `{jobId, runId}`; job data `{kind:'host'|'summary'|'log', result?,
  summary?, …}`. `auto` = SSH exec channel for SSH connections, a temporary terminal session otherwise (output cut at
  the returning prompt; no exit code without OSC 133). Schedules: `GET/POST /api/automation/schedules`, `PATCH/DELETE
  /api/automation/schedules/{id}`, `GET /api/automation/schedules/preview?spec=` → `{valid, error?, next[]}`, `POST
  /api/automation/schedules/{id}/run` → `{jobId, runId}`; `{name, enabled, spec (5-field cron, @hourly…, @every ≥ 1m,
  CRON_TZ=), action (JobAction as in batch), connectionIds, notify: never|failure|always}` (≤ 100 per user; they run
  while AstraTerm runs, as the owner; skipped for disabled users; an overlapping run is skipped). Runs: `GET
  /api/automation/runs?kind=&refId=&origin=&before=&limit=`, `GET/DELETE /api/automation/runs/{id}`, `DELETE
  /api/automation/runs` (last 500 per user; runs still running at shutdown become `error: interrupted`); event
  `{type:'automation.run', run}`. Module tables: `automation_scripts`, `automation_triggers`, `automation_schedules`,
  `automation_runs`, `automation_trigger_log` (migration `automation`/1).
* **Frontend.** Sidebar panels `snippets`, `macros`; singleton tab kind `automation` `{page?: scripts|batch|schedules|
  triggers|logon|history, scriptId?, runId?, connectionId?, triggerId?, connectionIds?}`; commands
  `automation.snippets|macros|runSnippet {id, sessionIds?}|compose|recordMacro|scripts {id?}|open {page?}|batch
  {connectionIds?}|schedules|triggers|history|logonActions {connectionId?}|sendPassword|injectSecret {key, sessionId?,
  enter?}|highlight.toggle|buttonBar.toggle|buttonBar.edit|saveSelection|newSnippet` plus dynamic
  `automation.snippet.<id>` / `automation.macro.<id>` (shortcuts); terminal plugins `automation.guard`,
  `automation.highlight` (xterm decorations over the viewport), `automation.passwordChip`; status items
  `automation-recording` and `automation-buttons` (the button bar, AUTO-4, lives in the status bar); settings section
  `automation` ("Highlighting & triggers": `highlight*`, `trigger*`, `passwordChip`, `guard*`, `productionTags`,
  `compose*`, `buttonBarVisible`, `buttonBars`, `macroShortcuts`, `macroSpeed`, `macroMergeMs`).

### servers — `internal/servers`, `web/src/features/servers` (SRV-1..6, CC-5)
* **Kinds** `http`, `ftp`, `sftp` (SSH/SFTP), `tftp`, `telnet`, `syslog`. `ServerStatus` superset: `{kind, name, running,
  state: stopped|starting|running|stopping|error, config, error?, errorCode?, addr?, addrs? (syslog "…/udp", "…/tcp"),
  url?, clients, startedAt?, stopAt?, stats: {connections, bytesIn, bytesOut, transfers, authFailures, messages?},
  warnings[], fingerprint? (SSH host key SHA256 / TLS certificate SHA-256)}`; `config.users[]` carry `hasPassword`, never
  hashes.
* **REST** (desktop mode: any signed-in user; server mode: administrators, else 403). `GET /api/servers` → `Status[]`;
  `GET /api/servers/host` → `{hostname, platform, osUser, home, defaultRoot, privilegedPorts, privilegedWildcardOk,
  astratermPort, interfaces[]}`; `GET|PUT /api/servers/{kind}` (PUT is a partial update: omitted keys keep their value,
  `users` replaces the list; per user write-only `password` — omitted = keep, `""` = remove; users are matched by `id`, so
  renames keep passwords; a running server restarts, a stale error of a stopped one is cleared) → `Status`;
  `POST /api/servers/{kind}/start|stop|restart` → `Status` (start errors: 400 `invalid_config`, 409 `port_in_use` |
  `port_privileged` | `address_unavailable` | `start_failed`, 503 `shutting_down`; the status keeps `error`/`errorCode`);
  `POST /api/servers/stop-all` → `Status[]`; `GET /api/servers/{kind}/logs?after=&limit=` → `{entries:[{id, ts, level:
  debug|info|warn|error, client?, user?, message}], lastId}` (2000-entry ring per server; survives server restarts, not
  AstraTerm restarts); `DELETE …/logs`; `GET /api/servers/{kind}/clients` → `[{id, addr, user?, since, activity?, bytesIn,
  bytesOut}]` (syslog: senders of the last 5 min); `DELETE /api/servers/{kind}/clients/{id}` disconnects one;
  `GET /api/servers/syslog/messages?q=(or filter=)&regex=1&severity=<max 0-7>&facility=&host=&app=&after=&before=&limit=` →
  `{messages (oldest first), hasMore, total, lastId, capacity}`; `DELETE /api/servers/syslog/messages`;
  `GET /api/servers/syslog/export?…` (text attachment, same filters, whole buffer).
* **Events** are topic based (same access rule, re-checked every 30 s in server mode): `servers` → `{type:'server',
  status}` (state changes at once, client counts ≤ 4/s, counters ≤ every 2 s); `servers.log {kind}` →
  `{type:'server.log', kind, entries, dropped?}`; `syslog` → `{type:'syslog', messages, dropped?}` (≤ 500 per 250 ms).
  The servers feature subscribes to `servers` whenever the user may use the servers.
* **Storage (deviation from the brief).** Configurations are stored in the settings table under `servers.<kind>` in the
  module-private scope `servers`, not `global`: `GET /api/settings` merges the global scope into every user's settings and
  would expose paths, user names and password hashes to non-admins. User passwords are argon2id hashes (verification only)
  rather than sealed values. SSH host keys (ed25519, ecdsa, rsa) and the self-signed TLS certificate (HTTPS / FTPS) are
  sealed with the vault system key (`servers.hostkeys`, `servers.tls`), so autostart works while the vault is locked.
* **Security.** Bind 127.0.0.1 by default (status warnings for wildcard / LAN binds, privileged ports, unencrypted logins,
  TFTP writes, shells); AstraTerm's own port is refused. Shared folders are jailed with `os.Root` (deviation: afero
  `BasePathFs` follows symlinks out of the root, so FTP gets an `os.Root`-backed afero.Fs); folders containing or inside
  AstraTerm's data directory are refused. Per-IP login throttling (10 failures / 10 min → 10 min block, 400 ms delay per
  failure). HTTP serves user files with `Content-Security-Policy: sandbox`, refuses cross-site form uploads and stores
  uploads through temporary files. SSH: port / agent / X11 forwarding refused; shell and exec only with `shell` on (run
  as the AstraTerm OS user, not jailed). ftpserverlib debug output (raw command lines incl. `PASS`) is never forwarded.
* **Defaults.** HTTP 8080 (read-only, listings), FTP 2121 (TLS optional), SFTP 2222, TFTP 69 (read-only), Telnet 2323,
  Syslog 514 (UDP + TCP, 10 000 messages); shared folder `~/AstraTermShare` (created on first start). Ports < 1024 need root
  on Linux and, on macOS, work unprivileged only on the wildcard address.
* **Audit** `server.config` (changed keys only), `server.start`, `server.stop`, `server.restart`, `server.stop_all`,
  `server.client.disconnect`, `server.syslog.clear`.
* **Frontend.** Tab kinds `servers` and `syslog` (singletons), overlay (configuration dialog, activity drawer), status item
  `servers`, settings section `servers` (`syslogViewerLimit`, `syslogAutoScroll`, `syslogShowDetails`, `syslogHighlights`,
  `confirmStopWithClients`, `showStatusItem`); the `servers` ribbon button is re-registered with a start / stop drop-down.
  Commands (category Servers): `servers.open`, `servers.syslog`, `servers.start|stop|toggle|restart {kind}`,
  `servers.configure {kind, tab?}`, `servers.activity {kind, tab?: log|clients|connect}`, `servers.stopAll`,
  `servers.toggle.<kind>`, `servers.configure.<kind>`. Uses `files.openLocal {path}` when registered.
* **Not implemented.** SRV-7 (NFS), SRV-8 (VNC server), SRV-9 (cron), SRV-10 (iperf); the legacy SCP protocol (OpenSSH ≥ 9
  `scp` uses SFTP). ftpserverlib opens passive data listeners on every interface; data connections arriving on another
  local address than the bind address, or from another peer than the control connection, are refused.

### tools — review update (reviewer-fixer, 2026-09-27; supersedes the conflicting details of the block above)
* **Requests are validated synchronously**: a bad body is `400 bad_request` from `POST /api/tools/{tool}` (not a failed
  job); unknown tool `404`; unknown / invisible `viaConnectionId` `404` (`423` while the vault is locked). At most 8
  running tool jobs per user (64 per server, 1 `throughput` per user) → `429 too_many_requests`. Scans are bounded:
  ≤ 8192 hosts and ≤ 262,144 host×port probes (`portscan`, `netscan`), SNMP walks ≤ 50,000 variables, whois replies
  ≤ 1 MiB per connection. Job rows are flushed every 100 ms (≤ 1000 rows per event).
* **Server-mode network guard**: for non-admin users (server mode) every connection a tool makes is vetted at dial
  time: loopback, unspecified (0.0.0.0/8, ::), link-local and cloud-metadata addresses (169.254.0.0/16, fe80::/10,
  fd00:ec2::254, 100.100.100.200) are refused (`httpcheck` incl. redirects, `tlscert`, `sshaudit`, `dns` resolver,
  `whois` server, `snmp`, TCP ping). Admins and desktop mode are unrestricted. `throughput` is admin-only in server
  mode (like `portscan` / `netscan` / host endpoints).
* **traceroute** `{host, mode:'trace'|'mtr', protocol:'icmp'|'udp', maxHops, probes, timeoutMs, resolveNames=true,
  ipv6, rounds (mtr, 0 = until stopped, ≤ 3600), intervalMs (mtr), viaConnectionId}`. Engines: Linux = ping socket or
  UDP with `IP_RECVERR` (unprivileged), macOS/BSD = unprivileged ICMP sockets, Windows/root = raw ICMP, else the
  system `traceroute`/`tracepath`/`tracert` (trace mode). Classic mode probes 8 TTLs at a time. Rows: `hop {ttl, from
  (string | string[]), host?, timesMs (null = lost), timeout, annotation? ('!H' '!N' '!P' '!X' …)}`; mtr: `mtr {ttl,
  sent, recv, loss, lastMs, avgMs, bestMs, worstMs, stdevMs, from?, hosts?, host?}` (latest per TTL wins) and
  `round {round, lastTtl, reached}` (show TTLs ≤ `lastTtl`); `summary {target, hops, reached, rounds?, engine}` is also
  sent after Stop. Via SSH: root uses `traceroute` (`-I` for ICMP), others `tracepath` first; mtr repeats one-probe
  traceroute rounds on the host.
* **throughput** (new) `{mode:'iperf3'|'ssh', host, port=5201, durationSec ≤ 60, parallel ≤ 8 (ssh ≤ 4), reverse,
  viaConnectionId (ssh)}` — `iperf3` = iperf3 protocol client (TCP) against any `iperf3 -s`; `ssh` = SSH channel
  throughput to a saved host. Rows `interval {startSec, endSec, bytes, bitsPerSec}`, `summary {mode, reverse, streams,
  durationSec, senderBytes, receiverBytes, senderBitsPerSec, receiverBitsPerSec, retransmits?, congestion?}`.
* **Other additions**: `ping` `{…, size, ipv6}`; `portscan` rows `hosterror {host, error}`, UDP closed detection,
  HTTP probe for silent ports; `netscan` `{…, viaConnectionId}` (names then resolved by the gateway), host rows add
  `netbios`, `workgroup`, `mdns`, `netbiosMac`; `dns` `{…, dnssec}` (TCP retry on truncation, summary `transport`,
  `authenticated`), types add `SSHFP`; `httpcheck` `{…, headers, body ≤ 1 MiB, count ≤ 100, intervalMs}` → `result`
  rows add `attempt, url, redirects, truncated, bodyPreview (2 KiB, text types), timing.redirectMs, tls.daysRemaining`,
  `redirect {attempt, to, hop, status}`, and a `summary {count, ok, failed, minMs, avgMs, maxMs}` when count > 1;
  `tlscert` `startTls` adds `postgres`, `ldap`, `probeVersions` (default true) → `version {version, supported,
  cipher?, error?, deprecated}` rows, cert rows add `keyAlg, keyBits, notYet, warnings`, connection `weakCipher, scts`,
  summary `hostnameMatch`; `snmp` accepts MIB names (`sysDescr.0`, `ifTable`, …) in `oid`/`oids`, `var` rows add
  `name`, summary `truncated`; `sshaudit` adds `hostkey {type, algorithm, sha256, md5, bits?, warning?}` rows;
  `wol` `{…, viaConnectionId}` (sent from the SSH host with wakeonlan or python3). `POST /api/tools/listening/kill`
  refuses pid ≤ 1 and AstraTerm itself, `signal` must be TERM or KILL (400), 403 when the OS denies it.
* **Frontend**: `tools.open {tool?, params?}` (`params` prefills the tool's form); "Network tools" submenus on the
  `session-node` and `terminal` context menus; tool state is kept per tool (switching tools keeps results / running
  jobs); closing the Tools tab cancels running tool jobs; panels are lazy chunks; run history is per user and never
  stores secrets, headers or bodies.

### files-ui — review addendum (supersedes the matching points above)
* **Partial uploads.** `<name>.astraterm-part` files (uploads / transfers in progress or interrupted) are hidden from
  listings and search results unless the new setting `files.showPartialUploads` (default false, Settings → Files &
  SFTP) is on; shown ones are dimmed. Empty-folder states say what the filters hide.
* **Remote monitoring checkbox** (panel footer) mirrors the monitor feature's own state — per-session override
  (`useMonitorBarOverride`) else global `monitor.showBar` ∧ the connection's `monitoring` option — and toggles it with
  `monitor.toggleBar {sessionId, enabled}`; the panel no longer keeps its own copy.
* **Offline panel.** While the terminal's transport is not usable (auto-reconnect `connecting`/`authenticating`,
  `disconnected`/`error`, ended) the browser is dimmed and `inert` under a card ("Reconnecting…", "Session
  disconnected" + Reconnect, "Session ended (exit code N)").
* **Uploads** wait (≤ 3 min, cancellable, "Waiting for the connection…" in the queue) when a chunk fails with
  `disconnected` / `session_not_connected`, then resume from the server's part size; a chunk whose bytes stop leaving
  the browser for 60 s is aborted and retried; reopened-handle retries are bounded. Downloads, previews, server
  transfers and folder compare resolve a live handle id first (`GET /api/fs/{id}`, reopen when gone).
* **Drops.** The whole file view is a drop zone (to the folder shown). The files feature installs a window-level
  fallback: an OS file drop nothing handled (e.g. between zones) is cancelled instead of letting the browser open the
  file. Dragging a folder / several rows to the desktop (Chromium) downloads a zip.
* **"Open terminal here"** (SSH) types the quoted `cd` only when the shell waits at a prompt (terminal plugin
  `files.promptProbe`: not in the alternate screen, no half-typed line, cursor line ends like a prompt or the output
  is quiet); otherwise a toast offers "Send anyway".
* **Misc.** Typing a file path in the location bar shows its folder with the file selected; sorting is also in the
  columns menu (keyboard); focus returns to the file list after dialogs; counts use thousands separators; the
  session editor's "Follow SSH path" shows the effective default (`files.followTerminal`). Node tests:
  `cd web && node --import ./src/features/files/__tests__/register.mjs --test 'src/features/files/__tests__/*.test.mjs'`.
* **Calm loading (docs/UX.md).** `features/files/delayed.tsx`: `useDelayedFlag(active, {delay: 300, minVisible: 400})`
  + `DelayedSpinner`. A folder the user opens keeps the old rows; only after 300 ms do they dim slightly and a thin bar
  runs (≥ 400 ms). Navigations from "Follow terminal folder" are *quiet* (`navigate(path, {quiet: true})`, view field
  `pendingQuiet`): no bar, no dimming; slow quiet navigations, refreshes and polls show only a small delayed spinner
  in a reserved slot of the status line. The first-load skeleton, connect / open spinners, preview / search / compare
  / properties spinners and the panel's offline card follow the same delay (auto-reconnect retries show one steady
  "Reconnecting…" card with "Reconnect now"). New rows fade in (150 ms) only when the folder changes. Transfer
  progress is monotonic per transfer (never recedes on chunk retries / resumes), bars ease over 500 ms, queued rows show
  an empty track (no animation), waiting uploads a warning-coloured bar. A pill under the list says what a drop does
  ("Upload to “logs”", "Move to …", "Copy to …").
* **Backend issue found (not fixed here).** `internal/vfs/followcwd.go` skips the OSC 7 setup when `Session.Cwd()` is
  non-empty, but `term` keeps the previous connection's cwd across reconnects, so after any reconnect "Follow terminal
  folder" silently stops (the stale folder stays). Fix belongs to term / files-backend: clear the session cwd when a
  (re)connect starts, or compare against the cwd captured at connect.

### term-transfer — `web/src/features/termtransfer/**` (FILE-19, FILE-20, FILE-22, CC-8; frontend only)
* **Shape.** One terminal plugin (`registerTerminalPlugin` id `termtransfer`, order 10) creates a controller per
  terminal view: a single output filter (replayed history passes untouched; then the trzsz stage, then the ZMODEM
  stage — a running transfer of one kind owns every byte), a capture-phase key guard (while a transfer or prompt is up
  no key reaches the remote — Ctrl+C cancels, Enter/Esc answer the prompt — and `disableStdin` blocks xterm's own
  input), the OS-drop target, and prompt/progress cards portalled into the pane by overlay `termtransfer` (so they
  follow pop-outs). No backend; no new endpoints.
* **One answering view per session.** Every view of a session sees a transfer start; an arbiter picks one (in the page:
  active tab > focused window > visible, ties by age; across pages/windows of the same browser: a BroadcastChannel
  `astraterm:termtransfer` claim with a 90 ms window, heartbeats, release). Other views hide the protocol bytes and show
  the shell's lines afterwards. Views in *another browser* cannot be coordinated (both would answer). Read-only viewers
  never answer.
* **trzsz** (`trzsz` 1.1.6, tested against trzsz-go 1.2.0): the library's `TrzszFilter` with its two file-choosing
  handlers replaced (runtime patch, checked at start): downloads go to a folder (File System Access API, streamed;
  remembered per user in IndexedDB) or the browser's downloads (per file; `tsz -d` folders as one STORE ZIP); uploads
  come from AstraTerm's pickers or drops (any browser; `trz -d` folders). `maxDataChunkSize` is capped at 1 MiB so one
  chunk always fits the server's 8 MiB per-session input queue. A magic line split across two WS frames is held back
  briefly.
* **ZMODEM** (`zmodem.js` 0.1.10, tested against lrzsz 0.12.21rc): detection → 150 ms grace (retracted when text
  follows = false positive) → the prompt (or silent start per settings). Fixes applied to zmodem.js at runtime: the
  send session parses every coalesced header (it stalled after the receiver's ZACKs piled up), stray ZACKs are ignored,
  a lost final "OO" (lrzsz flushes its tty on exit) ends the session normally, and the abort sequence is the spec's
  8 × CAN + 10 × BS (5 CANs do not stop a streaming `sz`). Uploads use windowed flow control: every 64 KiB piece ends
  with ZCRCQ and at most 1 MiB is unacknowledged (zmodem.js alone streams the whole file and overflows the 8 MiB input
  queue). After a cancel the sender's in-flight data is hidden until the stream is quiet. Sessions whose `encoding` is
  not UTF-8 refuse ZMODEM (output is transcoded); `backspace: ctrl-h` rewrites DEL in uploads (warned).
* **Drop on a terminal (FILE-22).** Zones: *Upload to <shell folder>* (SSH: `POST /api/fs {sessionId}` + chunked
  `PUT …/upload` with `total`/`final=1`, 8 MiB chunks, resume from `offset_mismatch` / `GET …/upload`, conflict dialog,
  cancel deletes the `.astraterm-part`, progress toast with "Show in SFTP browser" via `files.revealInPanel` when
  registered), *Copy to <folder>* for local shells (`{local:true}`; desktop mode or admins) which then types the
  quoted path, *Upload with trz* / *Upload with rz* (Ctrl+C, then the command — `rzCommand`, default `rz -E` — and the
  dropped files answer it), *Paste contents* (one UTF-8 text file ≤ `pasteMaxBytes`, through the paste pipeline). The
  folder is the terminal's cwd (OSC 7 / `RuntimeSession.cwd`), else the fs handle's `home`. While `rz`/`trz` waits for
  files, a drop anywhere on the pane answers it.
* **Send file (CC-8).** Command `terminal.sendFile {tabId?|sessionId?, file?}` (dialog; terminal context menu "Send
  file…", Terminal menu). Text mode uses the automation module's server pacer `POST /api/automation/send`
  (`enter:false`: Enter after the last line only when the file ends with a newline; its dangerous-command 409 is shown
  for confirmation; progress from `job` events; cancel `POST /api/jobs/{id}/cancel`) when available, the text is ≤ 1 MiB
  and the line ending is CR; otherwise (LF / CRLF, bigger files, RE2-incompatible prompt patterns, module absent) a
  browser pacer on a Worker clock (not throttled in background tabs) with the same options (per-line / per-char
  delay, wait-for-prompt via OSC 133 A or a quiet last line matching the pattern). Binary mode streams raw chunks
  (chunk size + delay; serial sessions offer baud-matched pacing) and stops when the server reports a full input
  buffer.
* **Commands** (category Terminal): `terminal.sendFile`, `termtransfer.upload` (to the shell's folder),
  `termtransfer.uploadTrz`, `termtransfer.uploadRz`, `termtransfer.pasteFile`, `termtransfer.settings`, hidden
  `termtransfer.cancel`. Context menu `terminal`: "Send file…", "Upload files ▸" (order 45); Terminal menu (order 170).
* **Settings** section `termTransfer` ("File transfer (terminal)"): `trzsz`, `zmodem`, `zmodemAutoReceive`,
  `downloadTarget` (ask | folder | downloads), `rzCommand`, `dropConflict`, `dropTypePath`, `pasteMaxBytes`, `sendMode`,
  `lineDelayMs`, `charDelayMs`, `waitPrompt`, `promptPattern`, `promptTimeoutMs`, `lineEnding`, `binaryChunkBytes`,
  `binaryDelayMs`.
* **Deviations from RESEARCH §3.20.** ZMODEM runs in the browser (zmodem.js, the documented fallback) instead of a Go
  `go-zmodem` bridge (module scope is frontend-only): transfers need an attached view and do not work for detached
  sessions; files are saved/streamed by the browser instead of passing through the transfer queue. History replays
  (a new view attaching from offset 0) show earlier in-band transfers as binary noise.
* **Tests.** `node --import ./src/features/termtransfer/tests/register.mjs --test 'src/features/termtransfer/tests/*.test.mjs'`
  (from `web/`): engine unit tests; with `ASTRATERM_TESTENV=1` also lrzsz / trzsz-go protocol tests (container from
  `tests/testenv.Dockerfile`) and, with `ASTRATERM_BIN`, end-to-end transfers through a real AstraTerm server.

### rdp — review addendum (reviewer-fixer, 2026-09-28; supersedes the matching points of the rdp block above)
* **Relay filters (IronRDP).** The relay terminates TLS, so it sees the RDP plaintext and filters it in both
  directions until the connection is set up (then it is pure passthrough; CredSSP / unknown framing is passed through
  untouched). Client → server: the MCS Connect Initial's `clientBuild` is raised to 2600 when below 420 (IronRDP 0.7
  reports 0; xrdp then refuses dynamic resize); channels disabled by policy are renamed `nxoffN` (`cliprdr` when
  `disableClipboard`, `rdpdr` when neither printing nor drive, `rdpsnd` when audio is off) so the server never joins
  them; `INFO_AUTOLOGON` is set in Client Info when the ticket carries a username **and** password (IronRDP only —
  otherwise the server's own logon screen is shown). Server → client: slow-path and fast-path (fragmented or not)
  bitmap updates whose uncompressed rows are padded (xrdp pads widths to a multiple of 4) or whose 16 bpp stride IronRDP
  0.7 mis-renders are re-encoded as interleaved RLE tiles (≤ 8 KiB each; shear/stripes fixed); a truncated 6-byte
  Deactivate All (xrdp) is completed to 13 bytes, and server output during reactivation is held until Font Map
  (≤ 64 MiB, then the filter gives up and passes through) so IronRDP never sees paints between Deactivate All and
  Demand Active. Debug log `rdp relay closed` reports `rewrittenRects`, `reactivations`, `disabledChannels`.
* **Admin read-only shadow (guacd).** `POST /api/sessions/{id}/rdp-ticket {shadow:true}` by an administrator for
  another user's live guacd session → `{…, readOnly:true, recording?}` without credentials; `/ws/guac/{id}` then joins
  the owner's guacd connection (`select $connectionId`, `read-only=true`). Browser → guacd is limited to
  `sync|nop|disconnect|ack`; guacd → shadow drops `clipboard`, `file`, `pipe`, `required`, argv and reserved
  streams. Shadows never change the session state and do not count as viewers. 409 `shadow_unavailable` when the
  session is not a connected guacd session (IronRDP runs in the owner's browser and cannot be shadowed). Non-admins
  still get 404. Audit `session.shadow`. UI: "View only" badge, no input/claimKeys/clipboard/mic, closing a shadow tab
  never ends the owner's session.
* **Recordings (guacd).** AstraTerm records the guacd stream itself (guacd no longer gets `recording-*`; `rdp.guacdDataPath`
  is only used for drives). Connections with `recording:true` through guacd write `<dataDir>/recordings/<id>.guac`
  (0600, Guacamole protocol, code-point lengths; mouse input included, clipboard excluded) and a row in the module
  table `rdp_recordings` (migration `rdp` v1: id, owner_id, session_id, connection_id, title, path, size, width,
  height, started_at, ended_at; width/height = first reported size). IronRDP sessions are not recorded (the viewer
  warns). Endpoints: `GET /api/rdp/recordings[?all=1]` (own; `all` for admins) → `RdpRecording[]`;
  `GET /api/rdp/recordings/{id}/file` (owner or admin; range-capable, `no-store`; 410 `gone` when the file is missing;
  audit `rdp.recording.view` when an admin opens someone else's); `DELETE /api/rdp/recordings/{id}` (owner or admin;
  409 while running; audit `rdp.recording.delete`). The ticket's `recording:true` shows a REC badge. Command
  `rdp.recordings` (also Toolbar menu and Settings → Remote desktop) opens a list with play/download/delete and a
  player (`Guacamole.SessionRecording` over a `StaticHTTPTunnel` — the Blob source is broken in guacamole-common-js
  1.5.0). These recordings are separate from the core terminal recordings UI.
* **Parser hardening.** Guacamole element lengths are digits only (no sign/space; leading zeros tolerated and
  re-encoded canonically); `FuzzParse` / `FuzzRoundTrip` cover both length units.
* **Viewer.** After connect the viewer re-checks the remote size against the container (up to 4 × 1.5 s, 8 px
  tolerance) and resizes if the server kept the old size; hidden tabs (or containers < 50 px) never request a resize.
  IronRDP logon failures show the `STATUS_*` code. Loading indicators follow docs/UX.md (300 ms delay, 400 ms minimum).
* **Tests.** `go test ./internal/rdp/...` (filter vectors captured from xrdp 0.9.24 and IronRDP 0.7 in
  `internal/rdp/testdata`, fuzzers `FuzzRLEDecode`, `FuzzServerFilter`, `FuzzClientFilter`, guac `FuzzParse`,
  `FuzzRoundTrip`); `ASTRATERM_TESTENV=1` adds shadow + recording against the testenv guacd/xrdp.

### netguard — `internal/netguard` (SEC-7 SSRF and destination controls; enforcement in `internal/sshx`, `internal/tunnel`, `internal/tools`)
* **Why.** In server mode AstraTerm opens sockets *from its own host* for users (raw/telnet/rlogin/VNC/RDP/FTP/S3/…
  sockets, SSH targets, proxies, tool probes, remote-forward destinations). Without a guard an ordinary user could aim
  them at `127.0.0.1:<port>` (another user's tunnel listener, AstraTerm's own API, local databases) or at cloud metadata
  (reproduced by the tunnels reviewer with a raw session). Connections that an SSH server, jump host or proxy makes
  for AstraTerm (direct-tcpip on the far side, the proxy's own outbound connection) are that hop's business and are not
  checked; the connection *to* the proxy / first hop is.
* **Who is restricted.** Desktop mode: nobody. Server mode: every non-admin (and an unknown/nil user); admins only when
  the policy's `applyToAdmins` is set. `netguard.For(d).ForUser(user)` → `*Guard`, nil = unrestricted (the guard reads
  the live policy on every check, so saves apply at once; a value written by other means is re-read within 5 s).
  ProxyCommand stays desktop/admin-only and runs outside the guard (a host command), as do the admin-only host
  features (local shell, embedded servers).
* **Enforcement is on the concrete dialed address** (`net.Dialer.Control`, after DNS), so DNS rebinding, HTTP
  redirects and FTP PASV replies cannot bypass it. Host-name checks (`CheckLiteral`, `CheckHostPort`) are advisory
  (early, friendly errors). Unix sockets are refused by a restricted guard.
* **Default policy (server mode, restricted users).** Refused: loopback (127/8, ::1), 0.0.0.0/8 and ::, IPv4-compatible
  ::/96, link-local 169.254/16 (incl. 169.254.169.254) and fe80::/10, link-/interface-local multicast (224.0.0.0/24,
  ff02::/16, ff01::/16), metadata endpoints fd00:ec2::254, fd00:ec2::23 (EKS Pod Identity), 100.100.100.200 (Alibaba),
  168.63.129.16 (Azure WireServer), every non-loopback/non-link-local address of the host's own interfaces
  (`blockHostAddresses`, default **true**: services bound to all interfaces but shielded by a security group are
  otherwise reachable from the host itself), AstraTerm's own listener (`Cfg.Listen` port on the listen IP; on every
  loopback/host/unspecified address for a wildcard bind — **never overridable**), and NAT64 (64:ff9b::/96) / 6to4
  (2002::/16) addresses embedding a refused IPv4 address. IPv4-mapped addresses (::ffff:a.b.c.d) are judged as IPv4.
  Private ranges (10/8, 172.16/12, 192.168/16, fc00::/7, fec0::/10) are **allowed** (bastion use). Everything else is
  allowed. `ParseHostIP` also understands inet_aton host forms (`127.1`, `2130706433`, `0177.0.0.1`, `0x7f.1`).
* **Admin policy** — global settings key `netguard` (scope `global`, so it is also visible in the merged
  `GET /api/settings`; it contains no secrets). Only the global scope is read; an invalid stored value is logged and
  replaced by the default policy (never "allow all").
  ```ts
  interface NetworkPolicy {
    allowPrivate: boolean        // default true
    blockHostAddresses: boolean  // default true
    deny: string[]               // IPs / CIDRs, ≤ 256, normalized (masked, IPv4-mapped → IPv4, deduplicated)
    allow: string[]              // exceptions, same format
    allowedPorts: string         // "" = any; "22,80,443,5900-5999" (normalized: sorted, merged ranges)
    applyToAdmins: boolean       // default false
  }
  ```
  Rule precedence: the **longest matching prefix** among built-ins, private ranges (when disallowed), host addresses
  (/32, /128) and the admin lists wins; on equal length an admin rule beats a built-in one and deny beats allow. So
  `allow: ["127.0.0.1/32"]` exposes one loopback address, `allow: ["0.0.0.0/0"]` opens nothing that is built-in
  refused, `deny: ["10.0.0.0/8"] + allow: ["10.1.2.3"]` allows one host. `allowedPorts` applies to exceptions too;
  AstraTerm's own listener is checked first and cannot be allowed.
* **REST** (`d.Router.Admin()`: 401 anonymous, 403 non-admin; mutating calls need `X-AstraTerm: 1`):
  - `GET /api/admin/network-policy` → `{policy: NetworkPolicy, default: NetworkPolicy, mode, enforced (false in
    desktop mode), listen, builtin: [{cidr, class, reason}], privateRanges: string[], hostAddresses: string[]}`.
  - `PUT /api/admin/network-policy` `Partial<NetworkPolicy>` (omitted keys keep their value, arrays replace, unknown
    keys → 400) → same view; 400 `bad_request` with a readable message on invalid entries. Audited
    `netguard.policy.update` `{old, new}`.
  - `POST /api/admin/network-policy/test {host, port (0 = any), userId?, policy?: NetworkPolicy (unsaved draft)}` →
    `{host, port, decision: 'allow'|'deny'|'unrestricted'|'error', allowed, restricted, reason, user?: {id, username,
    role}, addresses: Decision[], error?}` with `Decision = {ip, port, allowed, class, rule?, reason}` and `class` ∈
    `public private loopback unspecified ipv4-compatible link-local multicast metadata host astraterm deny-rule
    allow-rule port embedded local-socket invalid`. Without `userId` the request is evaluated for an ordinary user;
    host names are resolved (5 s) and every address is listed; unknown user → 404; bad host/port/draft → 400.
  (UI: owned by the security/admin UI engineer — Settings → Security "Network policy" editor + "Test a destination".)
* **Errors.** A refusal is `*netguard.BlockedError` (usually inside a `*net.OpError`); it unwraps to an `*httpx.HTTPError`
  403 `destination_blocked` with a message like `connection to 127.0.0.1:5432 is not allowed in server mode (AstraTerm
  network policy): 127.0.0.1 is loopback (the AstraTerm host itself)`. `netguard.IsBlocked(err)`. sshx marks refusals
  `term.Permanent` (no auto-reconnect loop); terminal sessions show the message as their error state.
* **Go API.** `netguard.For(d) *Manager` (per `*app.Deps`), `ForUser(d, user)`, `(*Manager) ForUser / Policy /
  SetPolicy / Enforced`, `NewGuard(Policy)` (fixed policy, no listener/host knowledge), zero `Guard{}` = default policy
  (tests), `(*Guard) Check(ip, port) Decision`, `CheckAddr(netip.Addr, port)`, `CheckIP(net.IP, port)`,
  `Control(network, address, rawConn)`, `Dialer(timeout) *net.Dialer`, `DialContext(ctx, network, addr, timeout)`,
  `CheckLiteral(host, port)`, `CheckHostPort(ctx, host, port)` (resolves, checks all IPs), `Transport(base
  *http.Transport) *http.Transport` (guarded dials; environment proxies disabled when restricted), `Restricted()`,
  `ParseHostIP`, `ParsePrefix`, `DefaultPolicy`, `BuiltinRules`. All `*Guard` methods are nil-safe (nil = allow).
* **Enforcement points.** sshx: every route carries the user's guard (`route.guard`, `Pool.guardFor`) — direct target
  dials and the first jump hop (`dialDirect` Control), the proxy server of SOCKS4/4a/5 and HTTP CONNECT proxies
  (`dialProxy`), TCP knocks (route) and UDP knocks (guarded UDP dialer), `Pool.Dialer` / `DialConnection` (every
  generic protocol: telnet, rlogin routed, raw TCP/TLS, VNC, RDP relay/probe, FTP incl. PASV, S3/WebDAV/SMB through
  the Dialer, WinRM; mosh's SSH bootstrap goes through the pool), `sshTunnelVia` gateways (dialed via `Pool.Get`). Hops after the
  first and targets behind a gateway are not checked. tunnel: remote (`-R`) forward destinations and reverse-SOCKS /
  HTTP-proxy targets are dialed with the owner's guard per connection (`forward.localDial`); a refused literal
  destination / Unix socket is rejected when a remote forward is saved or started (`Manager.destinationPolicy`);
  refusals land in `status.lastError` / `failedConns`. Unchanged tunnel policy: server-mode non-admins get loopback
  TCP listeners on ports ≥ 1024 only, no remote / reverse forwards, no Unix sockets; AstraTerm's port is never bindable.
  tools: `internal/tools/guard.go` now delegates to netguard (same checks as before plus host addresses, the AstraTerm
  listener, NAT64/6to4, Azure/EKS endpoints and the admin policy; errors are 403 `destination_blocked`).
* **Migration for `internal/vfs` (owner still reviewing; not changed here).** netguard is a superset of
  `vfs/netguard.go` (same classes and the same Control-time enforcement). Steps: (1) delete `internal/vfs/netguard.go`;
  (2) `registry.go` `dialer()`: drop the `if r.restricted(user) && !needsDialer(conn) { return guardedDial, … }` branch —
  `r.c.SSH.Dialer` now guards direct routes *and* the proxy server / first jump hop (which the private guard did not);
  and replace the fallback `var nd net.Dialer` with `nd := netguard.ForUser(r.d, user).Dialer(30 * time.Second)`;
  (3) `open_remote.go`: `r.restricted(user)` → `netguard.ForUser(r.d, user) != nil` (keeps forcing AstraTerm's own
  transport, i.e. no environment proxies, for restricted users). Error codes change from 403 `forbidden` to 403
  `destination_blocked`.
* **Known gaps outside this module's paths (owners please fix).** (1) `internal/proto/rlogin` dials *non-routed*
  connections itself (`dialReserved` + `net.Dialer`): add `Control: netguard.ForUser(d, req.User).Control` to both
  dialers (d = the Mount deps) — until then rlogin to `127.0.0.1:<port>` is unguarded. (2) `internal/proto/rawtcp`
  UDP transport dials directly: use `netguard.ForUser(d, req.User).Dialer(15*time.Second)`. (3) `internal/rdp`
  guacd engine: guacd connects to `hostname` itself — for restricted users call `CheckHostPort` before handing the
  host to guacd, or always route through the loopback forwarder (whose dial goes through the guarded Dialer).
  (4) `internal/proto/mosh`: the UDP leg (`net.DialUDP` in `dialBuiltin`, and the system `mosh-client`) is dialed
  directly; with jump hosts / proxies `udpTarget` resolves the host name *on the AstraTerm host* (a "127.0.0.1" meant for
  the far side hits AstraTerm's own loopback), and the port comes from the server's `MOSH CONNECT` line — call
  `netguard.ForUser(d, req.User).CheckIP(ip, port)` before dialing / spawning. (5) Pooled
  SSH clients are keyed per user and outlive a policy change / demotion until idle (60 s after the last release);
  running tunnels re-check per connection.
* **Tests.** `internal/netguard` (classification incl. `::ffff:127.0.0.1`, `::127.0.0.1`, `0.0.0.0`, `[::]`,
  `fe80::1%zone`, metadata, NAT64/6to4, inet_aton forms; rule precedence, ports, validation, self/host addresses, DNS
  rebinding against a fake DNS server, `Transport`; API 401/403/400, audit, dry runs; end-to-end exploit reproduction in
  server mode: raw/telnet/ssh/proxy/jump-host sessions of a non-admin to a loopback service are refused before any
  connection while an admin and desktop mode still connect), `internal/sshx/netguard_internal_test.go` (direct, 4 proxy
  types, TCP+UDP knocks, permanent errors), `internal/tunnel/netguard_test.go` (remote forwards refused for users; a
  running admin `-R` forward refused per connection after `applyToAdmins`; reverse-SOCKS / Unix host dials).

### netguard — bypasses closed (2026-09-28; `internal/proto/{rlogin,rawtcp,mosh}`, `internal/rdp`; closes gaps (1)–(4) above)
* **rlogin / rsh** (direct routes): both dialers — the reserved-source-port one (`dialReserved`, Control runs before
  the bind, so a refusal never falls back to the unprivileged dialer) and the fallback `net.Dialer` — carry
  `netguard.ForUser(d, req.User).Control`. The rsh stderr back-channel is a listener (the server connects to us; only
  the dialed server's IP is accepted) and needs no guard. Routed connections keep using `Pool.DialConnection`.
* **raw UDP**: dialed with `netguard.ForUser(d, req.User).DialContext("udp", …)`.
* **mosh**: the UDP destination (resolved IP + the port from `MOSH CONNECT`) is checked with `CheckIP` before the
  built-in client dials (its socket is also vetted in Control) or `mosh-client` is spawned (it gets the IP literal).
  With jump hosts / proxies a literal / localhost host is refused (`CheckLiteral`) before mosh-server is started.
* **rdp / guacd**: for restricted users on direct routes the destination is resolved and every address checked
  before guacd is contacted (or the certificate probe runs); guacd receives the **vetted IP as `hostname`** (IPv4
  preferred) — DNS rebinding between check and guacd's lookup cannot redirect it; the name stays in the ticket for
  display, audit and AstraTerm's certificate check. An unresolvable name is an error for restricted users. An RD Gateway
  (`gateway-hostname`, which guacd dials itself) is resolved and vetted the same way but not pinned (HTTPS name check).
  Routed connections go through the loopback forwarder (guarded dial). Ticket requests (both engines) refuse a
  literal / localhost destination early with 403 `destination_blocked`. IronRDP relay: dials via `Pool.DialConnection`
  (guarded; the pool-less fallback now uses the guard's Dialer) and maps refusals to RDCleanPath HTTP 403; guacd
  tunnels report them as Guacamole 0x0303 (CLIENT_FORBIDDEN) with the session in `error`.
* All refusals are `term.Permanent` (terminal protocols) and unwrap to 403 `destination_blocked`. Tests:
  `netguard_test.go` in each of the four packages (server-mode user refused with zero connections / datagrams / guacd
  contacts reaching loopback listeners; admin and desktop allowed; guacd pinning, policy port rules for mosh).


### files-backend — review addendum (reviewer-fixer, 2026-09-28; supersedes the matching points of the files-backend block)
* **Remote listings are sanitized** (`vfs.ListDir`, used by list, walks, deletes, copies, compare, archives and the
  transfer scan): entries named `""`, `.`, `..`, containing `/` or NUL are dropped, a name listed twice keeps its first
  occurrence, and every path is `dir/name`. A malicious or broken server (SFTP/FTP/WebDAV/SMB, S3 keys such as
  `a/../../x`) can no longer make a recursive download write outside the destination; transfers also refuse
  destinations outside `dstDir` or below a symlink they created (the item fails with an error). Search results must
  lie below the searched folder. Windows local paths refuse `\`, `:`, drive-relative forms and device names; SMB
  refuses `\` / `:` in names; FTP refuses CR/LF/NUL in paths (command injection on the control connection).
* **Safe editor saves** (`PUT …/write`): new content goes to `.<name>.astraterm-tmp-<random>` in the same folder
  (fsync when the server offers it), its size is verified, the original's permission bits and owner/group are applied,
  and it atomically replaces the file (rename; symlinks are followed, the link stays). A dropped connection at any step
  leaves the original intact and the temporary file is removed. In-place rewriting (the former behaviour) is kept
  when the file has several hard links or a POSIX ACL (local host, SSH with shell), owner/group/mode cannot be given to
  the temporary file, the folder is not writable, the symlink cannot be resolved (also inside the server-mode jail) or
  the target is not a regular file, and for FTP, SMB, SFTP servers without `posix-rename@openssh.com`, the local host on
  Windows and root browsing (`sudo-sftp`); `sudo:true` saves (sudo tee) stay in place. S3 / WebDAV PUTs are already
  atomic. Every strategy verifies the final size; audit `fs.write` carries `strategy` (`atomic|in-place|put`).
* **sudo**: commands run as `sudo -k -S -p <random marker> …` and the password is written only after sudo's prompt
  appeared on stderr (a PAM prompt = a partial line followed by silence); a second prompt kills the command. Cached
  credentials (e.g. `timestamp_type=global`) can therefore never make the password land in the saved file or in
  sftp-server's stdin (the former code sent it blindly). Validation uses `sudo -k -S -v`.
* **Reads / downloads** of FIFOs, devices and sockets → 400 `fs_error` (opening a FIFO blocked the request and the
  single-threaded sftp-server). Inline previews keep `frame-ancestors 'self'`; `Content-Disposition` `filename*` is
  strictly RFC 5987-encoded.
* **Extraction (Go fallback)**: nothing is written through an existing symlink below `destDir` (such a member fails the
  extraction), an existing symlink at a member's path is replaced, and the expanded size is limited to max(1 GiB,
  200 × archive size) (zip bombs → error). Local staging (WebDAV uploads, zip extraction) keeps max(1 GiB, 5 %) of the
  host disk free (507 `insufficient_storage`). S3 keeps ≤ 16 pending resumable uploads per handle (LRU aborted).
* **SSRF**: direct FTP/S3/WebDAV/SMB connections go through `internal/netguard` (`SSH.Dialer` / guarded fallback
  dialer; restricted users always get AstraTerm's own HTTP transport) → 403 `destination_blocked`.
* **Transfers are recorded** (module table `transfers`, migration transfer/1): after a restart, transfers that were
  queued/running are listed with state `error`, `error:"Interrupted by a server restart"`, `interrupted:true` and
  `resumable:true` when both sides can be reopened (saved connection, local host, running session). New
  `POST /api/transfers/{id}/retry` → 201 `Transfer`: reopens both sides and restarts with overwrite `resume` (finished
  files skipped, part files continued); the new transfer replaces the old record. Finished transfers are kept 24 h
  across restarts. A graceful shutdown does not mark running transfers canceled.
* **FILE-2**: `term.ShellKind(shell)` (`bash|zsh|ksh|posix|fish`) and `term.ShellIntegrationLineFor(kind, host)`;
  the line only carries the login shell's setup, is a fish-safe polyglot (a fish exec'd from a bash login prints just
  the marker), installs the reporter only when `uname -n` equals the session's host (after `ssh other` / `docker
  exec` nothing is followed), and reports an empty OSC 7 for folders with control characters. Full-screen programs are
  detected by tracking the alternate screen over the whole output of the backend generation (the former 256 KiB window
  let long vim/htop sessions look like a prompt). The prompt check models the cursor line (right prompts, zsh
  PROMPT_SP; `50%` rulers refused, oh-my-zsh `➜  dir` accepted). Held output is dropped only if every line of it
  consists of long runs of the injected line or the prompt (an unrelated line — background job, error — releases
  everything verbatim); after the marker the redraw is held line by line and only lines equal to the old prompt's
  lines are dropped. The hold is extended while the echo keeps arriving (slow links, up to 8 × timeout; the timeout
  adapts to the transport RTT). Released output can no longer be overtaken by newer output. On every (re)connect the
  previous connection's `cwd` is dropped (`session.updated` + terminal `{type:'cwd', path:""}`) when the new shell's
  first output arrives, and the integration is injected again. Verified with bash 3.2/5.2, zsh, dash, ksh93, mksh,
  busybox ash and fish 4 (fish reports natively), vim / less refusal.
* **Not done**: search / compare / recursive delete remain synchronous requests (cancellable by aborting the request —
  the UI passes an AbortSignal — and bounded to 5 / 10 min); no `d.Jobs` progress variant.


### webproxy — `internal/webproxy`, `web/src/features/webproxy` (PROTO-28, TUN-8, PROTO-20 Xpra)
* **REST** (own proxies only; others → 404). `POST /api/webproxy {connectionId?|sessionId?|tunnelId?, url? | scheme?,
  host?, port?, path?, insecureTls?, title?, mode?: 'auto'|'path', check? (default true)}` → 201 `Info` with an entry
  `url` (one-time token), `mode` (`host`|`path`) and `base`. Route: `sessionId` = the caller's live SSH session
  (`Pool.ForSession` per upstream connection, so reconnects are followed); `connectionId` = a saved ssh/sftp/mosh
  connection (`Pool.Get`; host defaults to `localhost` = the SSH server) or a saved **`web`** connection
  (`options.url`, `insecureTls`, `basicAuth` (default true: username + `password` secret → HTTP Basic), reached through
  `sshx.Pool.Dialer` = `sshTunnelVia` / `jumpHosts` / `proxy`); `tunnelId` = the caller's tunnel (its connection; host /
  port default to destHost / destPort); none = directly from the AstraTerm host through `Pool.Dialer`, i.e. vetted by
  **internal/netguard** at dial time (server-mode non-admins: loopback / link-local / metadata → 403
  `destination_blocked`; advisory literal check at creation). `check` dials once: unreachable → 422
  `upstream_refused|upstream_timeout|upstream_unreachable`. `GET /api/webproxy` (list), `GET|DELETE /api/webproxy/{id}`,
  `POST /api/webproxy/{id}/url {path?, mode?}` → fresh entry URL (reload, page reload, "open in new window").
  `Info {id, kind:'web'|'xpra', title, target{scheme,host,port,path}, via{kind:direct|ssh|session|tunnel|web, id?,
  label}, insecureTls, connectionId?, sessionId?, tunnelId?, spec, createdAt, lastUsedAt, active, extra?}`. Event
  `{type:'webproxy', change:'created'|'closed', proxy, reason?}` to the owner. In memory: ≤ 32 per user (least recently
  used evicted), closed after `idleMinutes` without requests (open WebSockets count as activity), with their SSH
  session, when the owner is disabled, and on shutdown (upstream streams are cut). Audit `webproxy.open|close`,
  `xpra.start`.
* **Serving.** *Host mode* (UI opened on localhost / *.localhost / a loopback IP): each proxy is its own origin
  `http(s)://p-<id>.localhost:<port>`; with the global admin setting `webproxy.hostSuffix` (wildcard DNS [+ wildcard
  certificate] → AstraTerm) also `p-<id>.<suffix>` for remote UIs. The entry token (`__astraterm_proxy_token`, 2 min,
  single use) is exchanged for a host-only cookie `__astraterm_proxy` (HttpOnly; on secure contexts incl. http
  *.localhost: `Secure; SameSite=None; Partitioned`, so it works in AstraTerm's cross-site iframe even where third-party
  cookies are blocked) and a redirect to the clean URL. *Path mode* (fallback; global `webproxy.pathMode`, default
  true): `/proxy/<id>-<key>/…` on AstraTerm's origin; the random key authorizes, every response carries
  `Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups … ` **without allow-same-origin** (the page
  runs in an opaque origin: no AstraTerm cookies / storage / API), `Service-Worker-Allowed` and `Clear-Site-Data` are
  dropped, absolute paths in HTML attributes / `Location` / `Set-Cookie Path` get the prefix (JS-built URLs cannot be
  fixed: best effort). Both modes: requests get the upstream's `Host` / `Origin` / `Referer`, never AstraTerm's cookies
  (`astraterm_session`, `__astraterm_proxy`) or `X-AstraTerm`; no X-Forwarded-*; upstream `Location`/`Refresh`/URL
  attributes pointing at the upstream origin are mapped to the proxy; upstream cookies lose `Domain` and on secure
  contexts become `Secure; SameSite=None; Partitioned`; `X-Frame-Options` and CSP `frame-ancestors` are replaced by
  `frame-ancestors <AstraTerm UI origins>`. HTML documents (navigations only, never XHR fragments; gzip/deflate decoded,
  documents requested with `Accept-Encoding: gzip`) get an inline **bridge** script (nonce added to the page's CSP only
  where that does not widen it) that reports path / title to the framing AstraTerm tab and executes back / forward /
  reload / navigate (postMessage, restricted to the UI origins). Errors (upstream down, TLS verification, policy) are
  rendered as pages on the proxy origin whose bridge reports `{code}` (`tls`, `auth`, `gone`, `upstream_*`,
  `destination_blocked`, `session_*`).
* **Hooks into the core (no edits outside the module).** (1) `Mount` installs a pre-routing middleware with
  `d.Router.Echo().Pre(...)` (after AstraTerm's request log / recover / security headers / Host guard): requests for
  `p-<id>.localhost|<suffix>` hosts and `/proxy/…` paths are served by the proxy (their paths belong to the upstream
  app, they carry no CSRF header, AstraTerm auth / body limits do not apply); AstraTerm's security headers are removed from
  proxied responses. *Integrator:* if the router ever gets a first-class host-dispatch hook, replace that one `Pre` call
  with it. (2) For all other requests the same middleware appends `http://*.localhost:* https://*.localhost:*` (and the
  `hostSuffix` origins) to the SPA CSP's `frame-src` so web tabs can frame proxy origins; *integrator:* this belongs in
  `httpx.contentSecurityPolicy` (one line) — then drop `extendFrameSrc`. Proxied requests are logged by the core request
  logger like any other (upstream 4xx at warn level).
* **Xpra (PROTO-20).** `GET /api/xpra/check?connectionId=|sessionId=` → `{installed, path?, version?, html5,
  message?}`; `POST /api/xpra/start {connectionId|sessionId, command, mode:'seamless'|'desktop', title?}` → 201 `Info`
  (kind `xpra`, `extra {command, mode, xpraVersion}`): runs `xpra start|start-desktop --bind-ws=127.0.0.1:<free port>
  --html=on --daemon=no --start-child=<cmd> --exit-with-children=yes --terminate-children=yes …` under `sh -c` with a
  stdin watchdog on one exec channel of the SSH connection, waits until the port accepts (≤ 45 s; failures → 422
  `xpra_failed` with xpra's last output lines), then proxies it. Closing the proxy (tab, idle, session) stops xpra and
  the application; the application exiting closes the proxy (`reason: "the application exited"`). 422
  `xpra_not_installed` / `xpra_html5_missing` carry install hints. Attaching to an already running xpra is not offered.
* **Frontend.** Tab kind `web` `{proxyId, url?, kind, title, spec, xpra?, path?, pathMode?, zoom?, target, via,
  connectionId? (saved web sessions only)}` — deliberately no `sessionId` (closing the SSH terminal must still end its
  session); an expired proxy is recreated from `spec` (Xpra tabs offer "Start again"). Commands (category Web):
  `webproxy.open` (§10 contract; complete targets open directly, else the prefilled dialog; `dialog: true` forces it),
  `webproxy.browser`, `webproxy.xpra {connectionId?|sessionId?, command?, mode?}`, `webproxy.manage`,
  `webproxy.back|forward|reload|home|zoomIn|zoomOut|zoomReset|openExternal|focusAddress` (active web tab; Alt+←/→,
  $mod+L). Protocol `web`: editor via `defineProtocol` (URL, untrusted certificates, HTTP Basic, SSH gateway / proxy on
  the Network tab) and `registerProtocolOpener`. Ribbon "Browser" (order 85, drop-down), Tools menu, context menus on
  SSH terminals / SSH connections ("Open web service…", "Run X11 app via Xpra…"), overlay dialogs, settings section
  `webproxy` (user section `webView`: `defaultZoom`, `recent`, `closeWithTab`; admins also edit the global `webproxy`
  section: `hostSuffix`, `pathMode`, `idleMinutes`).

---------------------------------------------------------------------------------------------------------------------

## 10. Build plan & file ownership

Work proceeds in stages. Within a stage, several contributors edit the same working tree **concurrently**; each owns a
disjoint set of paths. **Never edit a path you do not own** (read anything). If you need something from another module,
code against the contract in this document; if the contract is missing something, add the smallest possible
extension *inside your own paths* and document it in §9.

Shared-file rules:
* `go.mod`/`go.sum` and `web/package.json`/`package-lock.json` are owned by the foundation stage. Later stages must not
  run `go get` / `npm install`; all dependencies are pre-installed (see `internal/deps/tools.go` and package.json).
* `internal/server/modules.go` contains one `Mount` call per module; a module owner may edit **only its own line(s)**.
* `web/src/api/types.ts` is owned by the foundation; feature-specific extra types go in `web/src/features/<f>/types.ts`.
* `web/src/features/index.ts` imports every feature's `index.ts` (pre-created); do not edit it.

Terminal plugins: the terminal feature exposes `registerTerminalPlugin({ id, setup(term: Terminal, ctx: TerminalPluginContext) => dispose })`
in `src/app/registry.ts`, so other features (trzsz/zmodem, keyword highlighting, triggers, macro recording) extend
terminals without editing `TerminalView`. `TerminalPluginContext` = `{ tabId, sessionId, session (RuntimeSession getter),
send(data: string | Uint8Array), onInput(cb) , onOutput(cb), settings }`.

Terminal bus (`src/features/terminal/bus.ts`, owned by terminal feature, used by others): `sendToSession(sessionId, data)`,
`broadcast(data, sessionIds?)`, `getActiveTerminal()`, `listTerminals()`, `onTerminalInput(cb)`, `onTerminalOutput(cb)`.

### 10.1 Module wiring (backend)
`internal/core` defines `type Core struct { Sessions *term.Manager; SSH *sshx.Pool }` (imports term, sshx, app).
Every feature package exposes exactly:
```go
func Mount(d *app.Deps, c *core.Core) error {
	h := &handler{d: d, c: c}
	d.Router.API().GET("/tunnels", h.list)                     // GET /api/tunnels (authenticated), handlers per §4
	d.Router.API().POST("/tunnels/:id/start", h.start)         // POST /api/tunnels/{id}/start (+ CSRF header)
	d.Router.Admin().GET("/admin/tunnels", h.listAll)          // admin only
	d.Router.WS("/ws/tunnels/:id", h.watch)                    // authenticated, Origin-checked WebSocket
	return nil
}
```
and `internal/server/modules.go` calls them in order. Feature packages: `proto/telnet`, `proto/rlogin`, `proto/rawtcp`,
`proto/serial`, `proto/docker`, `proto/kube`, `proto/mosh`, `proto/winrm`, `proto/ipmi`, `vfs` (also mounts transfers),
`tunnel`, `keys`, `vnc`, `rdp`, `monitor`, `tools`, `servers`, `automation`, `recording`, `importer`, `ai`, `webauthn`,
`oidc`, `webproxy`. The foundation creates each as a compiling stub (`// STUB — owned by <agent>`), and its owner replaces it.

### 10.2 Frontend feature folders
`src/features/<name>/index.ts` for: `home`, `terminal`, `sessions`, `files`, `editor`, `tunnels`, `keys`, `vnc`, `rdp`,
`monitor`, `tools`, `servers`, `automation`, `recordings`, `admin`, `security`, `importer`, `ai`, `settings`, `webproxy`.
Extension points in `src/app/registry.ts` (all return an unregister function):
```ts
registerTabKind({ kind, title(params): string, icon: LucideIcon, component: React.ComponentType<TabProps>, singleton?: boolean })
registerSidebarPanel({ id, title, icon, order, component })
registerCommand({ id, title, category, icon?, keybinding?: string /* tinykeys syntax, user-overridable */, run(ctx), when?() })
registerRibbonButton({ id, label, icon, order, command?: string, menu?: () => MenuItem[] })
registerMenu({ menu: 'terminal'|'sessions'|'view'|'tools'|'settings'|'help', items: () => MenuItem[], order })
registerContextMenu({ target: 'session-node'|'terminal'|'tab'|'file', items: (ctx) => MenuItem[], order })
registerSettingsSection({ id, title, icon, order, component })
registerProtocolEditor({ protocol, label, icon, defaultPort, group: 'terminal'|'files'|'graphical'|'other', component })
registerStatusItem({ id, align: 'left'|'right', order, component })
registerTerminalPlugin({ id, setup(term, ctx): () => void })
registerOverlay({ id, component: React.ComponentType<{ locked: boolean }>, order?, keepMountedWhileLocked? })  // feature dialogs/drawers
registerProtocolOpener({ protocol, open(conn, opts), openQuick?(spec, opts) })  // custom open for protocols without a terminal backend
```
Feature folders added for Stage 2: `protocols` (pickers, serial/hex tooling, docker/kube UI) and `termtransfer` (trzsz, ZMODEM, drop-to-upload, send-file).

### 10.3 Stages
* **Stage 1 — foundation** (this order, some in parallel):
  B0 backend core · F0 frontend shell → B1 term + sshx + local shell · F1a terminal UI · F1b sessions UI → integration.
* **Stage 2 — feature fan-out** (all in parallel, one owner per row):

| owner | backend paths | frontend paths | RESEARCH IDs (primary) |
|---|---|---|---|
| protocols | `internal/proto/{telnet,rlogin,rawtcp,serial,docker,kube,mosh,winrm,ipmi}` | pickers inside `src/features/sessions/editors/*` for those protocols | PROTO-6..13,15,30,31, CC-1, CC-2, CC-18, TERM-8 |
| files-backend | `internal/vfs/**`, `internal/transfer/**` | — | FILE-3..9,11,14..18, PROTO-23..27 |
| files-ui | — | `src/features/files/**` | FILE-1..5,8,9,12,13,15..17, GFX-16 |
| editor | — | `src/features/editor/**` | TOOL-2, TOOL-3, FILE-10, FILE-23 |
| term-transfer | (none, or `internal/term/zmodem*` only if agreed) | `src/features/terminal/plugins/{trzsz,zmodem,dropupload}*` | FILE-19..22, CC-8 |
| tunnels | `internal/tunnel/**` | `src/features/tunnels/**` | TUN-1..9 |
| keys | `internal/keys/**` | `src/features/keys/**` | TOOL-1, SSH-5,11,12,19,20, SM-7 (identities UI) |
| vnc | `internal/vnc/**` | `src/features/vnc/**` | PROTO-17, GFX-1..5,17,18 |
| rdp | `internal/rdp/**` | `src/features/rdp/**` | PROTO-16, GFX-1..9,11,18, CORE-16, CC-15 |
| monitor | `internal/monitor/**` | `src/features/monitor/**` | MON-1..6 |
| tools | `internal/tools/**` | `src/features/tools/**` | TOOL-4..9, CC-4, CC-17 |
| servers | `internal/servers/**` | `src/features/servers/**` | SRV-1..7,9, CC-5 |
| automation | `internal/automation/**` | `src/features/automation/**` | AUTO-1,4..12, TERM-15, TERM-17 pacing, TERM-33, SEC-21 |
| recording | `internal/recording/**` | `src/features/recordings/**` | REC-1,2,4..7,9, MU-18, MU-19, TERM-30 |
| security | `internal/webauthn/**`, `internal/oidc/**` | `src/features/security/**`, `src/features/admin/**` | MU-5,6,7,15, SEC-4,5,6,20, REC-4 UI, UI-19 |
| importer | `internal/importer/**` | `src/features/importer/**` | IMP-1..4, SSH-36 |
| ai | `internal/ai/**` | `src/features/ai/**` | TOOL-10..12 |
| webproxy | `internal/webproxy/**` | `src/features/webproxy/**` | PROTO-28, TUN-8, PROTO-20 (Xpra) |

* **Stage 3 — integration, e2e (docker targets), adversarial review, fix loops.**

### editor (Stage 2, added by the orchestrator on the editor engineer's behalf)
* `editor.open {fsId, path, label?, source?, mode?: 'text'|'hex', line?, readOnly?}` — focuses an existing tab for the
  same `fsId`+`path`; with no args opens a remote-file picker. Other commands: `editor.new`, `editor.openText {title,
  content, language?}`, `editor.openLocal`, `editor.diff {left?, right?}` (sources `{fsId, path}` | `{title, content}`),
  `editor.save`, `editor.saveAs`, `editor.find`, `editor.replace`, `editor.gotoLine`, zoom/toggle commands.
* Tab kinds: `editor` {fsId, path, …}, `text` (scratch/local docs), `diff`.
* Saves send `expectMtime`; the editor resolves symlinks to their target (realpath) before editing so conflict checks
  compare the target's mtime. Expired fs handles (10 min idle / server restart) are re-opened transparently.

### importer — `internal/importer`, `web/src/features/importer` (IMP-1..4, SSH-36)
* **Import formats** (autodetected; `format:'auto'`): `mobaxterm` (.mxtsessions / MobaXterm.ini `[Bookmarks*]`, full
  session-type decode — SSH/Telnet/Rsh/RDP/VNC/FTP/SFTP/Serial/Mosh/S3, SubRep folders; File/Shell/WSL/XDMCP/Browser
  reported as unsupported), `putty_reg` (.reg export incl. UTF-16LE, KiTTY `Folder`, proxy, port-forwards, serial),
  `ssh_config` (concrete Host aliases, `%h` expansion, first-match-wins across blocks, ProxyJump→jumpHosts,
  Local/Remote/DynamicForward→options.forwards, Include expansion when read from disk), `termius_csv`, `csv` (generic,
  header auto-map + `options.csvMapping`/`csvDelim`), `mremoteng`, `remmina`, `filezilla`, `winscp`, `securecrt`,
  `json` (native export), `known_hosts`. **No passwords are ever read from third-party files** (MobaXterm/PuTTY keep
  them in OS/registry stores) — a warning says so; plaintext secrets are only read from a AstraTerm JSON export the user
  encrypted themselves.
* **Endpoints.** `POST /api/import/preview {format, content|path, base64?, options:{passphrase?,csvMapping?,csvDelim?}}`
  → `{format, folders, connections, keys?, knownHosts?, warnings, duplicates[], counts}`; each preview item has a
  deterministic temp id (`c0`,`f0`,…) so a later commit of the same bytes agrees on the selection. Encrypted JSON
  without a passphrase → 422 `passphrase_required`. `POST /api/import/commit {format, content|path, base64?,
  targetFolderId?, selectedIds?, dedupe:'skip'|'update'|'duplicate', importKeys?, importKnownHosts?, options}` →
  `{created, updated, skipped, foldersCreated, keysImported, knownHostsAdded, warnings}` (folders deduped by name under
  the target; keys deduped by fingerprint and imported only when readable and the vault is unlocked; known hosts are
  global — admin-only in server mode). `GET /api/import/discover` → `{supported, files[]}` (desktop only; lists known
  local config files; only a discovered path may later be read by preview/commit `path`). `GET /api/import/sync` /
  `POST /api/import/sync {enabled}` — SSH-36 ~/.ssh/config live sync into a read-only "~/.ssh/config" folder (desktop).
* **Export.** `GET /api/export?format=json|csv|ssh_config&includeSecrets=0|1&passphrase=&folderId=&connectionIds=`.
  `includeSecrets` is JSON-only and requires a passphrase; the file is then a passphrase-encrypted envelope
  (argon2id + XChaCha20-Poly1305, `{"envelope":"astraterm-encrypted",…}`) carrying connection/identity secrets and
  private-key material. Plaintext exports never contain secrets. CSV/ssh_config never contain secrets.
* **Backup/restore** (admin, IMP-4). `GET /api/admin/backup[?includeSystemKey=1&passphrase=]` → a `VACUUM INTO`
  SQLite snapshot; with a passphrase it is an encrypted zip archive (payload `astraterm-backup`), optionally including
  `system.key`. `POST /api/admin/restore {content(base64), passphrase?}` validates and **stages** the backup under
  `<data>/restore/` (never applied to the live DB); the response lists the manual steps to finish it.
* **Frontend.** Commands `importer.open {format?}` and `importer.export {format?, folderId?, connectionIds?}` (both
  category Sessions), an overlay wizard (source → preview tree with checkboxes/duplicate badges → result) and export
  dialog, Sessions- and Tools-menu entries, and the Settings → "Import & Export" section (SSH-config live sync toggle;
  admin backup/restore).
* (editor review) Additional commands `editor.exportHtml`, `editor.print` ($mod+P while an editor is active) and a
  find-in-files mode in the remote picker (`editor.open` with `{find: true}` / picker "Find" tab; opening a hit selects
  the match). Diff sides may carry an optional `source` descriptor so expired fs handles can be re-opened. Settings
  `editor.indentGuides`, `editor.colorSwatches`. Scratch docs, crash backups and recent files are stored per user in
  the browser (IndexedDB/localStorage namespaced by user id). Writable legacy encodings now include Shift_JIS, EUC-JP,
  GBK, GB18030, Big5, EUC-KR. Mixed line endings are preserved per line unless the user changes the EOL mode.

### importer — review addendum (supersedes the matching points above)
* **MobaXterm** (format verified against the community field tables + real captures; see `internal/importer/mobaxterm.go`):
  type codes 0 SSH · 1 Telnet · 2 Rsh · 3 XDMCP · 4 RDP · 5 VNC · 6 FTP · 7 SFTP · 8 Serial · 9 File · 10 Shell · 11 Browser ·
  12 Mosh · 13 S3 · 14 WSL. Imported: SSH (X11, compression, command → `remoteCommand`, or `startupCommand` when "do not
  exit" is set; gateways + their keys; SOCKS/HTTP proxies, local proxy command → `proxyCommand`, SSH-forwarding proxy →
  first jump hop; key; SSH-browser; agent / agent forwarding), Telnet/FTP/Mosh (host/port/user), Rsh (→ interactive
  `rlogin`, user in field 2), RDP (console, drives, printers, resolution, initial program, audio, mic, clipboard, colour depth,
  RD gateway incl. 24.2 expert string, SSH gateway), VNC (view-only, scaling, proxy, gateways), SFTP (folder, key,
  compression, proxy — its plain-text proxy password is dropped), Serial (device + standard baud), terminal block (term
  type, charset → `encoding`, log, font pt → px, cursor, scrollback; custom colour schemes shift fields by 15), tab colour →
  `color`, customised ImgNum → `lucide:*` icon, comments, `[SSH_Hostkeys]` → known hosts, header-less `.moba` files.
  **S3 is reported, not imported** (layout undocumented; it could carry keys). `options.charset` (WHATWG label) decodes
  legacy code pages (MobaXterm.ini on a Hebrew/Cyrillic/CJK Windows).
* **Jump hosts / gateways.** Parsed hops are resolved at commit: an ssh_config `ProxyJump` / `ssh -W` alias of the same file
  and ids from a AstraTerm JSON export become the **imported connections' ids**; inline hops stay ad-hoc
  `[user@]host[:port]` for ssh/sftp/mosh; for other protocols (RDP/VNC/…) the last hop becomes a **saved SSH connection**
  (`"<spec> (SSH gateway)"`, reused when an equivalent one exists) referenced by `sshTunnelVia` (which only accepts ids).
  A hop with its own readable key (desktop) is also saved to carry the key. `jumpHosts`/`sshTunnelVia`/`viaConnectionId`
  of JSON imports are remapped, never copied raw.
* **Contract additions.** Preview connections add `via[]`, `runsLocalCommand`; known hosts add `conflict` (a different key
  is already trusted — never added or replaced by an import); counts add `identities`, `snippets`. Commit: `selectedIds`
  absent = all, `[]` = none (keys/known hosts only); response adds `connectionIds`, `folderIds`, `gatewaysCreated`,
  `knownHostsConflicts`, `identitiesCreated`, `snippetsCreated`. Secrets/private keys are only read from an **encrypted**
  AstraTerm export (plaintext ones are ignored with a warning). Imported rows get the connections API's validation.
* **Export / backup.** `POST /api/export {format, includeSecrets, passphrase, folderId, connectionIds}` and `POST
  /api/admin/backup {includeSystemKey, passphrase}` (the UI uses these so passphrases never travel in URLs; the GET forms
  stay). New passphrases need ≥ 8 characters; a wrong one is 403 `wrong_password`. Decryption bounds the envelope's KDF
  parameters (≤ 256 MiB / 16 passes / 16 lanes) and runs ≤ 2 argon2 derivations at a time (429 after 20 s). The
  ssh_config export writes one comma-joined `ProxyJump` (aliases for exported hops) and refuses values that could inject
  directives; CSV cells are guarded against formula injection.
* **Restore** accepts only `astraterm.db` (+ `system.key` ≤ 4 KiB) at the archive root, checks declared and actual sizes
  (no silent truncation), opens the database read-only with `PRAGMA quick_check`, warns about a newer schema, and writes
  staged files atomically. Still applied manually (no startup hook: the store is open before modules mount).
* **Live sync (SSH-36).** `POST /api/import/sync` is admin-only. Syncs are serialized and triggered by a content hash of the
  Include-expanded file; synced connections are keyed by `options._sshConfigAlias` (renames survive, user colour /
  favourite / key / secrets are kept), ProxyJump aliases become ids, an emptied file removes the synced hosts, the folder id
  is remembered (`importerSyncFolderId`), keys are linked by the `.pub` fingerprint (private keys never copied). `Match`
  blocks are skipped with a warning everywhere (the parser library mistakes them for Host blocks).
* **Discovery** follows symlinks (dotfile managers), adds OneDrive Documents, exported `*.mxtsessions` in Documents /
  Desktop / Downloads, `~/.putty/sessions` + `sshhostkeys`, and on Windows the PuTTY / KiTTY / WinSCP registry
  (`registry:<id>` pseudo paths).

### servers — review update (reviewer-fixer, 2026-09-28; supersedes the conflicting details of the servers block)
* **Connection limits.** Every server run accepts at most 256 concurrent connections (TFTP: transfers), 32 per client
  address (per /64 for IPv6); further connections are closed at once and logged at most every 10 s. FTP closes
  connections that do not log in within 60 s; the HTTP server cuts a request body read or a response write that stalls
  for 60 s (slowloris / slow read; no overall timeout, big transfers stay possible).
* **Login throttling** counts failures per IPv4 address (IPv4-mapped IPv6 unmapped) and per IPv6 /64; a successful
  login no longer resets the count (a guest account could otherwise be used to brute-force other accounts).
* **Jail.** Symlinks created by clients (SFTP `symlink`, FTP `SITE SYMLINK`) must be relative and contain no `..`
  component (a lexical check is defeated by a parent that is itself a symlink); the shared folder itself cannot be
  chmod-ed; FIFOs / devices are never opened (downloads need a regular file, uploads never target a non-regular file).
  The data-directory overlap check folds case on macOS / Windows.
* **HTTP.** Per-user `readOnly` is enforced (uploads / PUT → 403, no upload form); non-canonical paths (`..`, `//`)
  are redirected (GET/HEAD) or refused (400).
* **Syslog memory.** The buffer is capped at 96 MiB as well as `bufferSize` messages (oldest evicted); a live event
  carries at most 1 MiB (older pending messages → `dropped`, the viewer then re-reads the newest page, ≤ every 2 s);
  sender statistics track at most 10 000 addresses.
* **FTP passive listeners** cannot be bound to the control connection's address with ftpserverlib v0.32.4 (it always
  listens on 0.0.0.0 / [::]; `WrapPassiveListener` receives the bound listener and the accept deadline is set on it).
  Mitigation unchanged: data connections to another local address than the bind address are closed, and the library's
  default `IPMatchRequired` refuses data connections from another peer than the control connection (PORT/EPRT bounce
  included).
* **Frontend.** Saving a configuration that opens a server to the network with risky settings (no login, plain FTP,
  telnet, TFTP, shell) asks for confirmation listing the risks.

### recording — `internal/recording`, `web/src/features/recordings` (§9 module notes; REC-1, REC-2, REC-4 session events, REC-5, REC-7, REC-9, MU-18, MU-19, TERM-30)
* **Recordings REST** (over the core `recordings` table filled by `term`; owner, admins any; others 404).
  `GET /api/recordings?q=&kind=asciicast|log&connectionId=&from=&to=&sort=started|size|title|duration&order=asc&limit=&offset=
  [&all=1&userId=]` → `{items, total, limit, offset}` (**object, not an array**); items = `Recording` + `live` (a session is
  still writing it), `interrupted` (never finished; size = current file size), `durationMs`, `connectionName`, `ownerName`.
  `from`/`to` accept RFC 3339 or `YYYY-MM-DD`; `q` matches title and connection name. `GET /api/recordings/{id}`;
  `GET /api/recordings/{id}/file?format=v3|v2|txt&download=1` (v3 = file as recorded, range-capable; v2 converted on the fly,
  no `x` events; txt = plain transcript of a cast; logs are text/plain; a live file ends at its last complete line; 410
  `gone`; admin access to others' audited `recording.view`); `GET /api/recordings/{id}/search?q=&regex=1&case=1&limit=`
  → `{matches:[{line?, ts?, time?, text}], total, truncated}` (logs: 1-based lines + timestamp prefix; casts: `time` in s);
  `GET /api/recordings/search?q=…` (+ list filters) → `{results:[{recording, count, samples}], scanned, truncated}`
  (≤ 300 files / 1 GiB / 20 s); `DELETE /api/recordings/{id}` (409 `recording_active` while written); `POST
  /api/recordings/bulk-delete {ids}` → `{deleted, failed:[{id, error, code?}], bytes}`; `GET /api/recordings/usage[?all=1]`.
  Audit `recording.delete|bulk_delete|view|search|search_all`.
* **Policy** (global settings key `recording`, read from the global scope only): `{maxAgeDays, maxTotalMB, commandAudit
  (default: server mode on / desktop off), shareEnabled, shareWriteEnabled, shareMaxHours (1–720, default 168), debugLog,
  debugLogLevel}`. `GET /api/recordings/policy` (everyone, + `mode`), `PUT /api/admin/recordings/policy` (partial,
  validated, audited `recording.policy.update`; disabling sharing revokes the affected links). Retention (REC-7) runs 45 s
  after start and every 10 min: finished recordings older than `maxAgeDays`, then the oldest finished ones while the total
  exceeds `maxTotalMB`; active ones never. `POST /api/admin/recordings/cleanup {dryRun?}` → `RetentionResult`. Audit
  `recording.retention` (system). S3 storage / encryption at rest of REC-7 are not implemented. RDP (`.guac`) recordings
  live in the rdp module's table and are not covered by this retention.
* **Instant replay (TERM-30).** Term hooks keep a per-session timing index (stream offset → arrival time, ≤ 25/s, ≤ 20 000
  points). `GET /api/sessions/{id}/replay?minutes=N` (owner/admin; admin audited `session.replay`) → asciicast v3 of the
  scrollback ring with real timing: output older than N minutes is the initial state at 0 s, recognized commands are `m`
  markers (`$ cmd`); `minutes=0` = everything the ring holds. The UI prefers the session's own recording when it records.
* **Command audit (REC-5).** One audit entry `session.command` (user = session owner, target = session id, details
  `{command, source: shell-integration|input, at, cwd?, exitCode?, durationMs?, running?, protocol, host?, username?,
  title?, connectionId?, recordingId?, recordingTime?}`) per command, while `policy.commandAudit`. Shell integration (OSC
  133/633 A/B/C/D, 633;E) gives the displayed command line + exit code (a command still running after 5 s is logged with
  `running:true`, its exit code only in memory); without marks the audited text is what the shell *echoed* after the
  prompt when the Enter's newline came back (so unechoed input — passwords — is never logged; lines typed at prompts
  that look like password/PIN/token/OTP questions are skipped too). Full-screen programs are ignored. Rate limit 5/s
  (burst 30) per session. `GET /api/sessions/{id}/commands` → `{commands, auditing}` (in memory, last 200, always kept).
  Commands typed by interactive share viewers are attributed to the owner (hooks do not identify the typing client).
* **Share links (MU-18).** `POST /api/sessions/{id}/share {mode: read|write, expiresInSec (60 … shareMaxHours·3600,
  default 3600), requireLogin?, maxViewers? (≤ 100, default 20), label?}` (owner, terminal sessions) → 201 `ShareView`
  `{id, sessionId, sessionTitle, ownerId, mode, createdAt, expiresAt, requireLogin, maxViewers, label, uses, lastUsedAt,
  url:'/share/<token>', token, viewers:[{id, ip, username?, since, mode}]}`; tokens are 32 random bytes (base64url),
  stored as SHA-256 in `share_links` plus sealed with the vault system key in module table `recording_share_meta`
  (migration `recording` v1, FK cascade) so the owner can copy the link again. `GET /api/sessions/{id}/shares`,
  `DELETE /api/sessions/{id}/shares` (revoke all), `GET /api/shares[?all=1]` (own active links), `DELETE
  /api/shares/{id}` (owner/admin; disconnects viewers). Public: `GET /api/share/{token}` → `{title, state, cols, rows,
  mode, expiresAt, requireLogin, label?}` (**no session id**, unlike `ShareInfo` in `api/types.ts`; 404
  `share_not_found`, 401 `login_required`, 429), WS `/ws/share/{token}?offset=` = the §6.2 terminal protocol (read-only
  unless `write`). Per-IP limits: 1 req/s (burst 30); invalid tokens: 10, then 1 per 30 s (429 meanwhile). Links die with the session, on
  revoke, at expiry (viewers cut) and at startup (sessions do not survive restarts). Events to the owner:
  `{type:'share.changed', sessionId, viewers}` and a `notify` when a viewer joins. Audit `share.create|revoke|revoke_all|join`.
* **Admin monitoring (MU-19).** `GET /api/admin/sessions` → `AdminSession[]` (RuntimeSession + `owner {id, username,
  displayName, role}`, `bytesOut` (ring head), `bytesIn`, `durationMs`, `lastOutputAt`, `shares`, `shareViewers`);
  `POST /api/admin/sessions/{id}/terminate {reason?}` (notice in the terminal, owner notified, audited
  `admin.session.terminate`); `POST /api/admin/sessions/{id}/message {text}` (notice + notification, audited). Shadowing
  is the existing read-only admin attach of `/ws/terminal/{id}` — in its own tab kind `shadow` (the terminal tab kind
  would DELETE the session when closed). "Lock input" of MU-19 is not implemented (no input gate in `term`).
* **Debug log (REC-9).** `GET /api/admin/logs?level=&q=&module=&after=&limit=` → `{entries:[{id, ts, level, msg, module?,
  attrs:[{k,v}]}], lastId, stored, capacity (5000), dropped, enabled, level, source: root|partial}`, `GET
  /api/admin/logs/export` (text, audited), `DELETE /api/admin/logs`. Secret-looking attributes and share / launch /
  setup / Bearer tokens are redacted. **Integration hook (not applied — `internal/server` is not ours):** build the
  process logger with `slog.New(recording.CaptureLogs(handler))` in `server.NewLogger` (optionally call
  `recording.MarkRootCapture()`) to capture every record; until then Mount wraps `d.Log` (modules mounted later and
  run-time `d.Log` users) and records one `http` entry per `/api`/`/ws` request through an `Echo().Pre` middleware
  (disabled automatically once the root hook is present).
* **Frontend.** Tab kinds `recordings` (singleton `{tab?: recordings|commands|storage}`), `player` `{recordingId |
  sessionId+minutes, startAt? (negative = from the end), line?}` (asciinema-player, lazy; logs open a log viewer),
  `shadow` `{sessionId, title?, owner?}`, `liveSessions`, `debugLogs`. Commands: `recordings.open {tab?}`,
  `recordings.play {id, startAt?}`, `recordings.share {sessionId?}`, `recordings.replay {sessionId?, minutes?}`,
  `admin.sessions`, `admin.debugLogs`. Terminal / tab context menus: "Share session…", "Instant replay ▸"; Terminal and
  Tools menus; status item `recordings.shares` (active links / viewers); settings section `recordings`; overlay
  (share + admin message dialogs). RDP recordings from `GET /api/rdp/recordings` are listed in the browser and open
  through `rdp.recordings`.
* **Public share page.** `web/src/App.tsx` renders the lazy `features/recordings/ShareViewer` instead of the auth gate
  when `location.pathname` starts with `/share/` (5-line change, no login, no app shell).

### security — `internal/auth` (extended), `internal/webauthn`, `internal/oidc`, `web/src/features/{security,admin}`, `web/src/auth/Login.tsx` (MU-1/2 admin UI, MU-5 UI, MU-6, MU-7, MU-15 UI, SEC-4, SEC-5 passkey unlock, SEC-6, SEC-20, REC-4 UI, UI-19)
* **Login flow (backward compatible).** A password login of an account with a second factor answers 401 with the usual
  `{error, code}` plus `{methods: ('totp'|'webauthn')[], mfaToken, expiresIn}`. `code` stays `totp_required` when TOTP is
  the only factor; with passkeys it is `mfa_required`. Old clients may still resend `{username, password, totp}`. New:
  `POST /api/auth/mfa/totp {mfaToken, code}` (TOTP or recovery code) → `{user}`; passkey second factor
  `POST /api/auth/webauthn/mfa/begin {mfaToken}` → `{ceremonyId, options}` then `…/mfa/finish {mfaToken, ceremonyId,
  credential}` → `{user}`. mfaTokens are single use, 5 min, dropped after 5 failures (`401 mfa_expired`). **A passkey
  makes password logins require a second factor** (TOTP or any passkey), like TOTP does.
* **Login policy (SEC-20)** — module-private settings scope `auth`, key `policy` (never merged into `GET /api/settings`):
  `GET/PUT /api/admin/auth/policy` (PUT = partial) `{passwordMinLength (8–128), passwordRequireClasses (0–4),
  passwordDisallowUsername, lockoutThreshold (per IP+user before the 1 s-doubling backoff, default 5),
  lockoutMaxMinutes (15), accountLockThreshold (0 = off), accountLockMinutes, sessionIdleHours (168),
  rememberDays (30; 0 hides "keep me signed in"), sessionMaxDays (0 = no absolute cap), requireMfa off|admins|all,
  passwordLogin (false = SSO/passkeys only for non-admins), allowedNetworks: CIDR[] (loopback always allowed)}` +
  `defaults`. A PUT that would exclude the caller's own address → 409 `self_lockout`. Errors: 403 `account_locked` (only
  revealed with the right password), 403 `login_not_allowed`, 403 `password_login_disabled`. `requireMfa` makes a
  password login of an account without a factor answer 401 `mfa_enrollment_required` + mfaToken; the login screen
  enrols TOTP through `POST /api/auth/mfa/enroll/totp/setup {mfaToken}` → `{secret, otpauthUrl}` and
  `…/enable {mfaToken, code}` → `{user, recoveryCodes}` (session started). SSO logins leave MFA to the IdP.
  `GET /api/auth/state` adds `loginMethods {password, passkey, sso:[{id,name}], remember}` and `passwordPolicy
  {minLength, requireClasses, disallowUsername}`. Module table `auth_user_meta` (migration auth/1: failed logins,
  lock, `password_set`, `password_changed_at`).
* **Recent authentication ("sudo").** Adding / removing passkeys, setting a first password (SSO accounts) and unlinking
  need a sign-in or re-verification (verify-password or passkey verify) within 10 min, else 403 `reauth_required`
  (clients confirm and retry); a `password` in the body also works. Go: `auth.ServiceFor(d)`,
  `Service.RequireRecentAuth(c, pw)`, `MarkReauthenticated`, `CompleteLogin(c, userID, LoginOptions)`,
  `LookupMFA/FinishMFA/MFAFailed`, `CreateExternalUser`, `SyncRole`, `HasPassword`, `PasskeyCount`, `CookieSecure`,
  `SetPasskeyProvider/SetSSOProvider`.
* **Account & admin additions.** `GET /api/auth/me` → `{user, hasPassword, passwordChangedAt?, passkeys,
  recoveryCodesLeft, mfaRequired, reauthFresh, authMethod}`; `PATCH /api/auth/me {displayName}`;
  `POST /api/auth/sessions/revoke-others` → `{revoked}`. `GET /api/admin/users` items add `{hasPassword, passkeys,
  sso: providerName[], locked, lockedUntil?, failedLogins, sessions}`. `POST /api/admin/users/{id}/reset-mfa
  {totp?, passkeys?}` (both default true) → `{totpReset, passkeysRemoved}`, `…/unlock`, `…/revoke-sessions`.
  `GET /api/admin/system` → mode, version, dataDir, listen, TLS, proxies, guacd address, platform, uptime, DB size,
  user / session counts, vault, features. REC-4: `GET /api/admin/audit/search?q=&userId=&action=&since=&until=&limit=
  &before=` → `{entries, nextBefore?}` (case-insensitive text search over action, target, user, IP, details; scans
  ≤ 50 000 rows per call) and `GET /api/admin/audit/export?…` → CSV attachment (≤ 200 000 rows, formula-injection
  safe, audited `admin.audit.export`). Password rules apply to password changes, admin create and reset.
* **Passkeys (MU-6, SEC-4).** RP ID = the request's host name (IP addresses → 409 `webauthn_unavailable`; use
  `localhost` or a DNS name), origin = scheme://Host; an admin may pin both: `GET/PUT /api/admin/webauthn/config
  {rpId, origins[]}` (scope `auth`, key `webauthn`). `GET /api/auth/webauthn/status` (public) → `{available, rpId?,
  origin?, reason?, code?}`. `GET /api/auth/webauthn/credentials` → `[{id, name, rpId, authenticator?, aaguid?,
  discoverable, synced, backupEligible, userVerified, transports, createdAt, lastUsedAt?}]`; `POST …/register/begin
  {password?}` → `{ceremonyId, options}` (PublicKeyCredentialCreationOptionsJSON; resident key preferred, credProps),
  `POST …/register/finish {ceremonyId, name, credential}` → 201; `PATCH/DELETE …/credentials/{id}`; passwordless
  `POST /api/auth/webauthn/login/begin {conditional?}` / `…/login/finish {ceremonyId, credential, remember}` → `{user}`
  (discoverable, user verification required, counts as MFA); re-verification `POST …/verify/begin|finish` (lock
  screen, reauth). Ceremonies: single use, 5 min, bound to the starting origin; anonymous starts limited per IP.
  A signature counter that goes backwards → 403 `webauthn_clone_warning`. Tables `webauthn_users` (random 32-byte
  user handles), `webauthn_credentials` (migration webauthn/1). Admin reset via `reset-mfa`. Not implemented: PRF-based
  vault unlock (the vault has no passkey-wrapped key yet).
* **OIDC SSO (MU-7).** Providers in scope `oidc`, key `providers` (client secret sealed with the vault system key,
  write-only): `GET /api/admin/oidc/providers` → `{providers, redirectUri}`, `POST` (201), `PATCH/DELETE
  /api/admin/oidc/providers/{id}`, `POST /api/admin/oidc/test {id | issuer}` → discovery result. Fields: `name,
  enabled, issuer, clientId, clientSecret (write-only), scopes, usernameClaim, emailClaim, displayNameClaim,
  groupsClaim, adminGroups, allowedGroups, syncRole, autoProvision, linkByUsername, requireVerifiedEmail, prompt`.
  Browser flow: `GET /api/auth/oidc/login?provider=&remember=1&returnTo=/…` → IdP (code flow, PKCE S256, state, nonce;
  the state is bound to the browser by a `SameSite=Lax` cookie on `/api/auth/oidc`) → `GET /api/auth/oidc/callback`
  → 303 to `returnTo`/`/`, or `/?sso_error=<code>` (`invalid_state`, `expired`, `access_denied`, `idp_error`,
  `verification_failed`, `not_provisioned`, `not_allowed`, `email_not_verified`, `already_linked`,
  `provider_unreachable`, `account_disabled`, …; the login screen explains them). Linking from the Security tab:
  `GET /api/auth/oidc/link?provider=` → `/?sso_linked=<name>`; `GET /api/auth/oidc/identities`, `DELETE
  …/identities/{id}` (the last sign-in method cannot be removed). JIT accounts get a free user name derived from the
  claim, no usable password (they may set one right after signing in). Table `oidc_identities` (migration oidc/1).
* **Frontend.** Tabs `security` (singleton `{section: account|twoFactor|passkeys|tokens|devices|linked}`),
  `active-sessions` (UI-19: own runtime sessions, running / detached / ended, open, reconnect, close, bulk close,
  close all detached; live via the sessions cache) and `admin` (singleton `{section: users|authentication|audit|
  network|system}`; the network section uses the netguard API when it answers). Commands (category Security):
  `security.open {section?}`, `security.twoFactor`, `security.passkeys`, `security.tokens`, `security.devices`,
  `security.sessions`, hidden `security.addPasskey`, `security.newToken`, `security.linked`,
  `security.unlockWithPasskey`; (category Administration, admins) `admin.open {section?}`, `admin.users`,
  `admin.authentication`, `admin.audit`, `admin.networkPolicy`, `admin.system`. Settings-menu entries.
  **Lock screen (SEC-4/5):** an overlay kept mounted while locked shows "Unlock with passkey" under the shell's
  LockScreen when the user has a passkey for this address (the shell may instead call `security.unlockWithPasskey`).
  **Login screen:** passkey autofill (conditional UI) + "Sign in with a passkey", one "Continue with …" button per SSO
  provider, second-factor step offering the passkey and/or code, TOTP enrollment step, `?sso_error=` messages.

### ai — `internal/ai`, `web/src/features/ai` (TOOL-10, TOOL-11, TOOL-12)
* **Optional by design.** No provider call happens until a provider is configured; `GET /api/ai/status` never
  contacts the provider. Providers: `anthropic` (Messages API over raw HTTPS + SSE, `x-api-key`,
  `anthropic-version: 2023-06-01`, default model `claude-sonnet-5`; also `claude-opus-5-5`, `claude-fable-5-1`,
  `claude-haiku-4-5-20251001` or any id) and `openai` (OpenAI-compatible `…/chat/completions` streaming: OpenAI,
  Ollama, LM Studio, vLLM, llama.cpp, Gemini's OpenAI endpoint; key optional). `output_config.effort` is sent only to
  models that accept it (auto: `low` for command mode, `medium` otherwise); no `thinking` parameter is sent.
* **Configuration.** Settings key `aiProvider` (global scope = organisation default written by admins; user scope =
  personal override) holds `{enabled?, provider, preset?, baseUrl?, model?, effort?, maxTokens?}` plus admin policy
  (global only): `models[]` (allowed for users; empty = any), `allowUserConfig` (server mode: personal
  provider + key), `allowUserModel` (default true), `rateLimitPerMinute` / `rateLimitPerDay` (0 = 20 / 1000,
  < 0 = unlimited), `redactPatterns[]` (RE2). Values are re-validated on every read (the generic settings endpoint can
  write the key). `enabled` defaults to true in desktop mode and **false in server mode** (admin must enable). A
  user-scope `enabled:false` hides the assistant for that user. Non-admin personal endpoints in server mode are dialed
  through `netguard` (SSRF policy). API keys live in module table `ai_keys(scope, key_enc, updated_at)` (migration
  `ai`/1), sealed with the vault data key — write-only (reads return `hasKey`); a locked vault → 423.
* **Endpoints.** `GET /api/ai/status` → `{available, enabled, configured, reason?: disabled|not_configured|locked,
  provider, preset, model, source: global|user, models[{id,label?,hint?}], canPickModel, canConfigure,
  canConfigureGlobal, mode, locked, limits{perMinute, perDay}}`; `GET /api/ai/config?scope=global|user` (global =
  admins) → Config + `{scope, hasKey, allowed}`; `PUT /api/ai/config {scope, …Config, apiKey? ("" deletes), reset?}`;
  `POST /api/ai/test {scope, …draft, apiKey?}` → `{ok, latencyMs, model, reply}`; `GET /api/ai/models[?scope=]` →
  `{models, source: provider|builtin, error?}`; `POST /api/ai/chat {messages:[{role:user|assistant, content}],
  context?: {sessionId?, terminalText?, selection?, sessionInfo?, file?:{path, language?, content}, command?,
  exitCode?}, mode: chat|command|explain, model?}` → `text/event-stream` (body ≤ 4 MiB): `meta {provider, model, mode,
  redactions, contextChars}`, `thinking {}`, `delta {text}`, `result {command, explanation, risk: low|medium|high,
  riskReason, parsed}` (command mode), `done {stopReason, usage}` or `error {error, code}`; `: ping` every 15 s.
  Closing the request cancels the provider call. Errors before streaming: 403 `ai_disabled` / `model_not_allowed`,
  409 `ai_not_configured`, 423 vault locked, 429 per-user limit, **424** (failed dependency, message kept) with
  `provider_auth | provider_rate_limited | provider_overloaded | provider_not_found | provider_bad_request |
  provider_unreachable | provider_timeout | provider_error`.
* **Redaction.** Before anything is sent, the context *and* the conversation are redacted: exact values of the
  user's stored connection/identity secrets (visible connections incl. shared ones; ≥ 8 chars, or 5–7 chars unless a
  plain lowercase word), the referenced session's live secrets, the provider key, private-key / PuTTY key blocks,
  AWS/GitHub/GitLab/Slack/Stripe/Google/`sk-…` tokens, JWTs, Authorization headers, URL credentials,
  `--password`-style flags, `mysql -p…`, `sshpass -p`, `*password|secret|token|api_key…=value` assignments
  (`$VAR` references kept) and admin patterns. A `sessionId` the caller owns adds server-known facts (protocol, host,
  user, cwd). Context is wrapped in `<context>` and the system prompts tell the model never to follow instructions
  found in it. Audit `ai.request` (target = mode; details `{provider, model, context kinds, messages, redactions}` —
  never content), `ai.config.update|reset`, `ai.test`. Config changes emit `{type:'ai.status'}` (broadcast for
  global, per user otherwise).
* **Frontend.** Tab kind `ai` (singleton) and sidebar panel `ai` (registered only while available) share one chat:
  local history per user (`astraterm:ai:conversations:v1:<userId>`), Markdown answers (GFM, no HTML, images never
  loaded), code blocks with Copy / Insert / Run… (shell languages) or Open in editor, context chips (terminal output
  — rendered xterm lines, else `/scrollback?raw=0` —, selection, editor file via `/api/fs/{id}/read`, failed command),
  model picker, redaction count per message. Target OS facts come from `GET /api/monitor/{sessionId}/host` when
  available. **Never auto-run:** Insert pastes through the terminal's paste pipeline without Enter; Run always opens
  a confirmation showing the risk and the result of `POST /api/automation/guard/check` (404-tolerant), then sends to
  that one terminal. Commands: `ai.open` (Alt+Shift+A), `ai.ask {text, context?: {selection?, tabId?, sessionId?,
  terminal?}, mode?}`, `ai.commandBar` ($mod+I; in a terminal on Windows/Linux the `ai.assist` plugin claims Ctrl+I
  only while the assistant is available — otherwise Ctrl+I stays Tab), `ai.newChat`, `ai.explainSelection`,
  `ai.fixSelection`, `ai.settings`. Terminal plugin `ai.assist`: `# <request>` + Enter at a prompt opens the command
  bar with a suggestion; failed commands (OSC 133 `D` exit ≠ 0/130, else recognisable error output after a typed
  command) get a calm "✦ Explain · Fix" chip. Terminal context menu: Explain / Fix with AI (selection), Ask AI about
  this terminal…, Command from description…; Tools menu entry (or "Set up AI Assistant…"). Settings section `ai`
  ("AI assistant": provider presets, write-only key, model, test, admin access & limits, redaction patterns) and UI
  preferences in settings section `ai` (`hashTrigger`, `offerOnError`, `errorHeuristics`, `autoAttachTerminal`,
  `terminalLines`, `chatModel`).

### automation — review addendum (reviewer-fixer, 2026-09-28; supersedes the matching points of the automation block)
* **Macro steps (AUTO-1).** Steps extend the core `{data, delayMs}` with optional `waitFor` (RE2; the backend replay
  waits, after the delay, until it appears in the ANSI-stripped output; a timeout stops that session's replay with
  `"step N: … did not appear within …"`), `timeoutMs` (default 30 000, max 600 000) and `secret` (a stored secret of
  the session's connection typed before `data`, same access rules as inject-secret; never stored in the macro). They
  live in the same JSON column of the core `macros` table; the automation module reads/writes those rows itself
  (`store.Macros` would drop the extra keys). The recorder never records what is typed at a password prompt: it
  records a `secret` step instead. Snippets and macros can be dragged from the sidebar onto any terminal
  (`application/x-astraterm-snippet|macro` drag types, terminal plugin `automation.dropTarget`).
* **Event triggers (AUTO-7).** `Trigger.event: 'output' (default) | 'connect' | 'disconnect' | 'command'`, plus for
  `command`: `exit: 'any'|'ok'|'error'` and `minDurationSec`. `pattern` is required for output triggers only; for
  event triggers it optionally filters the event text (the command line / session title / state message).
  `command` fires on OSC 133 `D;<exit>` after a `B` (shell integration required); the line typed after `B` is never
  matched by output triggers (fixes triggers firing on the echo of a typed command). `disconnect` fires when a
  connected session drops or its remote side ends (not on close) and allows no typing actions. Event data adds
  `event`, `exitCode?`, `durationMs?`, `paused?`. Migration `automation/2` adds `automation_triggers.event` and
  `event_opts`. Highlight actions take `underline`.
* **Trigger loop guard.** A line that only echoes what the rule itself typed in the last 3 s does not fire it; a rule
  whose typing actions (send / runSnippet / runScript) fire again within cooldown + 3 s of its previous input more
  than 40 times in a row is paused on that session (terminal notice + `notify` event + `paused:true`) until the session
  reconnects or the rule is edited. Saving a trigger whose `send` text trips the dangerous-command guard answers
  409 `dangerous_command` unless `confirmDangerous:true`.
* **Scripts.** A heap watchdog interrupts running scripts when the Go heap grows by more than
  `ASTRATERM_SCRIPT_HEAP_MB` (default 1024) over its level when they started ("the script used too much memory");
  live log events are throttled to 200/s (burst 500, ≤ 20 000 per run; the run log keeps the tail of everything).
* **Batch / schedules.** At most 4 concurrent batch runs (including scheduled ones) per user, 32 in total (429 /
  a failed scheduled run); batch hosts running a script wait for a free script slot instead of failing.
* **Logon actions vs `startupCommand`.** `term` types `startupCommand` the moment a session connects, i.e. BEFORE the
  first logon step. The logon editor states this and offers "Run it after the logon actions", which moves the command
  into a final `send` step (lines joined with `\r`, backslashes escaped) and removes `options.startupCommand`. Minimal
  `term` change for automatic ordering: in `Session.connect`, skip `startupCommand` when the resolved connection has a
  non-empty `logonActions` list (this module would then send it after the last step).
* **Known limitation (term).** Injected secrets (inject-secret, logon/macro/trigger secret steps) go through
  `Manager.Write`, so they reach `OnInput` hooks and — only when `recordInput` is enabled — the asciicast `i` events.
  Minimal `term` change: `Manager.WriteSensitive(id, data)` that queues the bytes but skips `rec.Input` and passes
  hooks a same-length mask (e.g. `*`… keeping a trailing CR), for this module to use for every secret.

### recording — review addendum (share relay, guest input control)
* **Share relay.** `/ws/share/{token}` no longer hands the viewer's socket to `term.Manager.Attach`: the module attaches
  an in-memory WebSocket pair read-only (`internal/recording/wspipe.go`) and relays frames, so policy is enforced per
  frame: read-only viewers can send only `ack`/`ping`; `resize` from any viewer is ignored (viewers follow the owner's
  size); interactive guests' input goes through `Manager.Write` (and `signal`/`break`/`reconnect` through the session)
  only while the link's guest input is not paused. Downstream, `cwd` messages are dropped and `state.message` (error
  details with addresses) is stripped; `readonly` reflects the link mode and the pause state. Viewer admission (limits)
  is atomic with registration, and a link revoked between lookup and registration is closed at once.
* **Close codes.** `4410` = the share link was revoked / expired / disabled by policy (final; reason text says which);
  `1000` = the session ended; `1001` = server shutting down.
* **Guest input control.** `POST /api/sessions/{id}/share` takes `inputPaused?: boolean` (interactive links start with
  guest input paused: guests watch read-only until the owner allows it); `PUT /api/shares/{id}/input {paused}` (owner or
  admin; interactive links only; audited `share.input`) toggles it and switches connected guests instantly
  (`{type:'readonly'}`). `ShareView.inputPaused`. `share.join` audit details add `viewerId`, `ip`.
* **Guest attribution.** `session.command` audit entries of commands typed (partly) by a share guest carry
  `details.guest {shareId, viewerId, ip, username?, label?}` (the command still ran with the owner's permissions);
  `CommandRecord.guest` likewise. Plain-mode attribution is per typed line; with shell integration per prompt.
* **Debug log redaction** also covers `/proxy/<id>-<key>` capability keys, credential-like query parameters,
  `user:password@` URL userinfo, `Basic`/`Bearer` values, and never formats arbitrary structs/maps (type name only).
  Minimal `httpx` follow-up for the process log (outside this module): `safePath` should also redact `/proxy/<id>-<key>`.
* Admin reads of another user's live command list (`GET /api/sessions/{id}/commands`) are audited
  `session.commands.view`.

### webproxy — review addendum
* Host-mode token clean-up redirects never produce network-path references (`//host`, `/\host`).
* Upstream `Alt-Svc` / `Public-Key-Pins*` / `Expect-CT` are dropped in both modes; in path mode (AstraTerm's origin) also
  `Clear-Site-Data`, `Strict-Transport-Security`, `Service-Worker-Allowed`, `NEL`, `Report-To`, `Reporting-Endpoints`,
  `Accept-CH`, `Critical-CH`, `Set-Login`, `Origin-Agent-Cluster`, `Timing-Allow-Origin`, `Set-Cookie2` and upstream
  CORS headers.
* Path mode answers CORS for the sandboxed page itself (`Origin: null`): preflights are answered by the proxy
  (`204`, `Access-Control-Allow-Origin: null`, requested method / headers, no credentials) and responses get
  `Access-Control-Allow-Origin: null` — the capability key in the path is the credential.
* The web tab delegates `clipboard-read` only to Xpra tabs (path mode relies on the response CSP sandbox alone: an
  iframe `sandbox` attribute makes some embedders refuse the frame). Closing the last tab of an Xpra application always
  stops it (independent of `closeWithTab`).

### security — review addendum (added by the orchestrator for the security reviewer)
* `POST /api/auth/tokens` accepts an optional `password` to satisfy the recent-sign-in rule inline.
* Sensitive account/admin endpoints (SSO link, TOTP setup, API token creation, admin password/2FA reset, role changes,
  creating admins, login policy, SSO providers, passkey domain settings) answer **403 `{code:'reauth_required'}`** when
  the session's last sign-in/verification is older than 10 min; the UI shows the "Confirm it's you" dialog and retries.
  SSO link redirects may return `?sso_error=reauth_required`. API-token requests pass the admin check but cannot make
  account-level changes.
* Passkeys: the last sign-in method / last required second factor cannot be removed (409).
* Lockout never applies to clients on the AstraTerm host itself (so remote attackers cannot lock out the only admin);
  with a same-host reverse proxy, configure trusted proxies.

### ai + term-transfer — review addendum (reviewer-fixer, 2026-09-28)
* **ai / redaction.** Also redacted: `FOO_PASS=` / `MYSQL_PWD=` / `*_pw=` assignments, `echo pw | sudo -S …` and
  `sudo -S … <<< pw`, `curl -u user:pw`, any `--*-password` flag, Cookie / Set-Cookie values, bare `Bearer <token>`,
  `<password>…</password>`, AstraTerm API tokens (`nxt_…`), and the user's stored SSH-key private keys + passphrases.
  Stored secrets are re-checked on every request (sealed blobs hashed; decrypted only when they changed), so a secret
  saved a moment ago is redacted at once. Closing tags of the context structure (`</context>`, `</selection>`, … in
  any case/spacing) are neutralised in captured text. Suggested commands lose control / zero-width / bidi characters
  (server and client). Provider error texts are passed through the redactor (the key never reaches the browser).
* **ai / transport.** The provider client never follows redirects (a 3xx → 424 `provider_error` naming the target
  host: Go would forward `x-api-key` and replay the body). At most 4 answers per user stream at once (429). Model
  listings reaching the provider are limited to 20/min per user (then the built-in list with an `error`).
* **ai / frontend.** `# request` + Enter opens the command bar only at a shell prompt: OSC 133 A/B/C state when the
  shell reports it, else the text left of the cursor when the line was started must end like a shell prompt and not
  like a heredoc `> ` or a REPL (`>>>`, `mysql>`, `postgres=#`, …). "Explain · Fix" is never offered for a one-word
  "failed command" that looks like a password typed at the wrong place (a password prompt just above, or a
  password-like token with exit 127). Insert of a multi-line command into a shell without bracketed paste asks first
  ("Insert would run this command"). Context capture starts at the first row of a wrapped line. Command-bar intent
  history is per user (`astraterm:ai:intents:v1:<userId>`). "Explain / Fix" follow-ups send the real prompt (not the
  label) and Retry re-captures the original context.
* **term-transfer / trzsz false positives.** A magic line that is followed by visible text (same chunk, or within
  150 ms — a real `trz` / `tsz` waits silently) is ignored: no prompt, nothing typed into the shell, output shown.
  A confirmed transfer whose server never sends its configuration ends after 15 s instead of blocking the terminal.
  Enter answers a transfer card only after it has been visible for 600 ms (type-ahead protection). A lost connection
  also closes a pending transfer question.
* **term-transfer / other.** Browser downloads fold received data into Blob segments every 8 MiB (no large JS
  arrays); ZIP entries get their CRC while streaming. Typed paths with control characters use `$'…'` quoting (a
  newline never submits the line); Windows paths containing `$` / backtick are single-quoted (PowerShell literal).

### Integration — stage 3 (2026-09-28; shared layer, `term`, `server`, `importer`, `servers`, `store`, `httpx`)
* **Loading-state system** (docs/UX.md "Loading states", binding). One timing implementation `web/src/lib/useDelayedFlag.ts`
  (`DelayedFlag` state machine + `useDelayedFlag`, `useLoadingGate`; unit tests `src/lib/__tests__`), primitives in
  `components/ui`: `Spinner` (delayed by default; `immediate`, `reserve`, `active`), `Delayed`, `LoadingPane`, `BusyIcon`,
  `LazyBoundary` (Suspense with the rule), `LoadingState` / `QueryState` / `ErrorState` (`query-state.tsx`),
  `Skeleton` / `SkeletonRows` / `SkeletonText`, `ProgressBar` (monotonic, eased), `StatusDot` / `statusDotClass` /
  `pendingPulseClass`; `IconButton busy`, `Button loading` (delayed spinner); `useManualRefresh` (lib/hooks). The private
  copies in files / rdp / importer / servers were removed. react-query default `placeholderData: keepPreviousData`
  (entity-scoped queries opt out with `placeholderData: undefined`). Guardrail `scripts/lint-ui.mjs` (`make lint-ui`,
  `npm run lint:ui`, part of `make check`) with the shrink-only allowlist `scripts/lint-ui-allowlist.json` for paths still
  being migrated. Frontend unit tests run with `cd web && npm test` / `make test-web`.
* **Context-menu groups.** `registerContextMenu({…, group})` joins a built-in section of the target menu; the terminal
  menu knows `edit`, `selection` (right after copy/paste), `view`, `layout`, `session`, `files`, `close`; ungrouped
  contributions follow the built-in sections. `getContextMenuSections(target, ctx)` returns the sections with groups.
* **Terminal.** Fit guard: proposals below 5 rows / 20 cols are retried every 100 ms (≤ 1.5 s) before they reach the
  PTY, and the size is re-checked 150 / 600 / 1500 ms after opening, on becoming visible, on `visibilitychange`, window
  resize and font load. Plugin API `ctx.acquireInputLock(reason) → release` (`TerminalPluginContextEx`): while held,
  `handle.send` / `handle.input` / `paste`, MultiExec fan-out from other tabs and `bus.sendToSession` / `broadcast` (no REST
  fallback) are refused with a hint and the terminal shows a badge; the holder keeps using `ctx.send` / `ctx.sendRaw`
  (boolean result);
  `TerminalHandle.inputLocked()`. The MultiExec compose line is checked with `POST /api/automation/guard/check` (confirm on
  matches; 404-tolerant). Owner views get `{type:'shadow', viewers}` (see term below) → badge.
* **term.** `Manager.WriteSensitive(id, data)` (secrets: skips the asciicast input track; hooks get `SensitiveMask(data)`
  — `*` per byte, trailing CR/LF kept — through `Hooks.OnSensitiveInput` when set, else `OnInput`).
  `Manager.SetStartupHandler(fn(s, conn, send) bool)`: on each (re)connect a handler may claim `startupCommand` and call
  `send()` later (e.g. after logon actions; `send` is a no-op after a reconnect / close); unclaimed = sent at once as
  before. `AttachOptions.Shadow` (set for other admins' read-only `/ws/terminal` attaches): the owner's writable views get
  `{type:'shadow', viewers:[username…]}` (also in the attach burst; `[]` when the last shadow leaves) and the owner a
  `notify` event.
* **Restore on restart (IMP-4).** `POST /api/admin/restore` now stages the backup **and** writes
  `<data>/restore/APPLY-ON-RESTART`; the next start applies it in `server.New` before the store opens
  (`importer.ApplyStagedRestore`): the live `astraterm.db` (+ `-wal`/`-shm`) and, when the backup carries one,
  `system.key` move to `<data>/restore/previous-<UTC time>/`, the staged files move into place (rolled back on failure,
  retried next start; a start is refused only if the rollback itself fails). Response adds `applyOnRestart`. New
  `GET /api/admin/restore` → `{pending, stagedAt?, stagedBy?, systemKey}`, `DELETE /api/admin/restore` (discard;
  audited `admin.restore.discarded`). Settings → Import & Export shows the pending restore with "Discard".
* **Shutdown.** `servers.WaitShutdown(ctx, d)` (servers stopped, syslog files flushed) and `store.Store.Close` waiting for
  connections still busy with a query are both part of `server.Server.Close` (fixes lost syslog lines and stray
  `-wal`/`-shm` files after Close).
* **Logs.** `httpx` request logs redact the key of web-proxy path-mode URLs (`/proxy/<id>-…/…`) like share tokens.
* **Tokens.** `--remote-backdrop` / `--remote-backdrop-fullscreen` (`bg-remote-backdrop…`) for the surround of RDP / VNC
  canvases and recording players (replaces hard-coded colours).
* **Tooling.** `vite.config.ts` aliases `monaco-editor/esm/vs` → `node_modules/monaco-editor/esm/vs` so npm `monaco-vim`
  (and other add-ons with deep ESM imports) resolve against monaco-editor ≥ 0.56 (same module instances; verified with a
  production build). Note for the editor owner: the package's `browser` export is its UMD build, and the vendored copy
  also carries the ShiftCommand constructor fix, so switching to the npm package needs that fix kept. `make smoke` runs
  its servers with a temporary `HOME`/`USERPROFILE`.

### editor — Monaco review addendum (added by the orchestrator)
* New command `editor.goToFile` ($mod+O): quick-open sibling files of the active editor's folder. Path bar
  (breadcrumbs) under the editor toolbar with per-folder file lists.
* Settings: `editor.semanticValidation` (type-check JS/TS, default off; missing imports never reported).
* Diff whitespace modes: `ignoreWhitespace: 'none' | 'trim' | 'all'`; copy blocks in both directions.
* Monaco popups/overflow widgets render into a per-window body layer, so editors work in dockview pop-out windows.
* **Keyboard owners.** `registerCommand({…, essential})` (`true` or a list of default bindings): inside an element
  matching `KEYBOARD_OWNER_SELECTOR` (`[data-keyboard-owner], .monaco-editor`) only essential bindings fire — close /
  next / previous / n-th tab, lock, and the palette's ⇧⌘P (⌘K, ⇧⌘O… stay with Monaco). Widgets with their own shortcut
  systems mark their root `data-keyboard-owner="<name>"`.
* **Pop-out portals.** `components/ui/portal.tsx`: every dock panel is wrapped in `<PortalScope>` (PanelHost,
  `useOwnerBody`), and the dropdown / context menu / popover / select / tooltip / dialog / alert-dialog primitives portal
  into `usePortalContainer()` — the body of the document the panel lives in (pop-out windows included).
* **Dock back.** `dockTab` never targets an empty grid group: dockview removes a pop-out's hidden, empty reference group
  together with the pop-out group before the moved panel is added (the panel ended up 0×0 and missing from the tab bar).
* **Boot.** `public/boot.js` (blocking, same-origin — no CSP change) applies the cached appearance (`astraterm:appearance`,
  now carrying the resolved accent colours) before the first paint; index.html holds a static `.nx-splash` identical to
  AuthGate's single, continuously mounted splash (auth state → settings → AppShell chunk, which is preloaded with the
  auth request), which fades out once. Fonts: `@fontsource-variable` CSS is served with `font-display: block` and the
  latin Inter / JetBrains Mono files are preloaded (vite plugin `calmFonts`). `make flash-audit` =
  `scripts/flash-audit.mjs` (headless Chrome, frame-level and DOM-level flash detection, functional checks).

### automation + recording — stage 3 migration (2026-09-28; supersedes "Logon actions vs `startupCommand`" and the "Known limitation (term)" point of the automation review addendum)
* **Secrets.** Every stored secret this module types (inject-secret, logon / macro / trigger secret steps, scripts'
  `session.sendSecret`) goes through `term.Manager.WriteSensitive`: never in the asciicast input track, input hooks see
  a mask. A macro secret step types the secret on its own and the step's `data` (usually Enter) as ordinary input.
* **Start-up command after the logon actions.** The module installs `Manager.SetStartupHandler`: for a connection with
  a valid, non-empty `logonActions` list it claims `startupCommand` on every (re)connect and types it after the last
  step — only when every step succeeded (a stopped sequence adds "The start-up command was not sent." to its notice).
  The logon editor's "Run it after the logon actions" option is gone (the note now says the command runs after the last
  step); connections that used it keep their final `send` step.
* **Command audit.** `recording` sets `Hooks.OnSensitiveInput`: masked secrets count as input, and the line they are
  typed on (also when Enter follows as ordinary input; with shell integration: the next command at the prompt) is
  never audited as a command.

### UX refinement pass — shell, sessions, terminal, files (2026-09-28; F0 + F1a/b + files-ui owners' paths)
* **Zero oscillation (docs/UX.md "Motion & feedback").** `index.css` removes Tailwind's looping animations from the theme
  (`--animate-pulse|ping|bounce|caret-blink: initial`) and the retired `pulse-dot` / `heartbeat` / `indeterminate`
  keyframes; the only continuous motion is `animate-spin` (1.2 s, spinner ring). `StatusDot` / `statusDotClass(tone,
  pending)`: settled = solid dot, pending = steady ring outline (`border-[1.5px]`, transparent centre);
  `pendingPulseClass` is kept for compatibility and is `''` (deprecated). New `useSteadyStatus(value, isPending)`
  (components/ui/status-dot): pending blips < 300 ms keep the previous settled value (used by the dock tab dot, the
  terminal status item and the session tree's running dot). `ProgressBar` without a value is a still, softly tinted
  bar. Terminal: `cursorBlink` defaults to false; the visual bell is a steady bell mark (2.5 s) instead of a flash;
  the MultiExec ring is steady. `scripts/lint-ui.mjs` rule `oscillation` applies to every file (shared layer included).
* **Timing presets.** `lib/useDelayedFlag`: `DELAYED_FLAG_DEFAULTS` = 300 ms / ≥ 600 ms (was 400);
  `DELAY_PRESETS.{EXPLICIT_WAIT, NAVIGATION}` (300/600, 1000/800). Every primitive inherits the new minimum.
* **Files navigation.** No list-wide progress bar, dimming or row fade any more. `FileBrowser` shows one spinner in a
  reserved slot of the location bar (`PathBar busy`, NAVIGATION preset) for a navigation still running after 1 s;
  background refetches show nothing; the Refresh button spins for refreshes the user asked for (`ToolAction.busy`,
  view state `refreshing`). Listings are prefetched on row hover (120 ms dwell), on the keyboard cursor and for the
  parent folder (`actions.prefetchDir`, 10 s fresh; `controller.prefetch`). Path autocomplete keeps its suggestions
  while the next listing loads. The cursor ring never blinks across a navigation. `FileBrowser` root carries
  `data-file-browser`, `data-path`, `data-pending` (read by the flash audit). Transfers badge / status item / status-line
  button follow the 300 / 600 ms rule. SFTP panel "Connecting…" placeholders use `useLoadingGate`.
* **Forgiving close** (`app/closeUndo.ts`, shared by terminal / VNC / RDP tab kinds): closing a running session's tab
  ends the session after 6 s with one "Undo" toast (bursts share it); Undo re-attaches through `reopenClosed`; the page
  going away ends pending sessions at once (`fetch` keepalive DELETE). `scheduleSessionClose(id, title, {onEnd})`,
  `keepSession(id)`, `undoAll()`. `general.confirmCloseRunning` now defaults to false (explicit `true` still asks).
* **Saved workspaces.** Settings section `workspaces` (`{saved: SavedWorkspace[]}`, stores/settings) — name, tabs,
  dockview JSON; `saveWorkspace(name)`, `restoreWorkspace(id)` (closes the open tabs — Undo applies — restores the
  layout and calls the new optional `TabKindDef.revive(tab)`), `deleteWorkspace(id)`, `useSavedWorkspaces()` in
  stores/workspace. Terminal `revive` keeps a still-running session (`keepSession`) or restarts it in the tab.
  Commands `workspace.save {name?}`, `workspace.restore {id}`, `workspace.delete {id}` (Undo toast) and one palette
  entry per saved workspace (`workspace.open.<id>`); View menu / Split button "Workspaces" submenu; Home card.
* **Ribbon.** Grouped (connect · workspace · network · tools · app, thin separators); `RibbonButtonDef.group?` and
  `placeholder?` (shell placeholders a feature replaces — no dev "re-registered" warning; `createRegistry` takes an
  optional `replaceable` predicate); buttons that do not fit move into a "More" overflow menu instead of scrolling out
  of sight; menu chevrons sit next to the label (menu-only) or in a slim column aligned with it (split buttons).
* **Quick connect** suggests saved sessions (fuzzy: name, host, user, tags; favorites / recent when empty) together
  with the quick-connect history; Enter opens the highlighted session, or connects to a typed spec. `size="lg"` for Home.
* **Home**: launcher (large quick connect), actions, Recent sessions with hover actions (open to the right, browse
  files, edit), Running now, Workspaces, Organize (top-level folders with counts → `sessions.revealFolder {folderId}`;
  tags with counts → `sessions.filterByTag {tag}`, both new hidden commands of the sessions feature).
* **Other.** Light-theme `--success` / `--warning` / `--info` darkened to ≥ 4.5:1 on white and on the status bar;
  their `-foreground` is white. Host-key prompt: the server's repeated text is hidden when structured details exist.
  Terminal badges (input lock, transport lost, pasting) and the end-of-session panel / badge are `Delayed`. Dock tab
  progress bar follows the 300 / 600 ms rule with one monotonic run per operation. Terminal scheme `mobaxterm` is now
  `classic` ("Classic (black)"; the old id resolves as an alias). Product names removed from UI text and comments in
  these paths (import-format names excepted).
* **Flash audit** (`scripts/flash-audit.mjs`, see docs/CONTRIBUTING.md): full-resolution frames decoded after each
  recording; small-region A → B → A detector for the file browser; oscillation detector (turning points returning to
  the same levels within 1.6 s, frames within 700 ms of input ignored, spinner / focused caret exempt); scenarios for
  files-tab and SFTP-panel navigation, follow-cd and idle refresh at 0/150/350/700/1500 ms, SSH connect (0/350/1500 ms)
  and reconnect; options `--nav-latencies`, `--ssh user:pass@host:port|auto|off`, `--nav-only`.
### Final polish pass — shell, sessions, settings, loading primitives, host guard (2026-09-28; F0 + F1b + B0 owners' paths)
* **Allowed hosts (B0).** `--allowed-hosts` / `ASTRATERM_ALLOWED_HOSTS` / `config.Config.AllowedHosts`: comma/space
  separated host names or IP literals (a port and a trailing dot are dropped, lower-cased, de-duplicated; schemes,
  paths and wildcards are rejected at start-up). `httpx.Options.AllowedHosts`: the loopback Host guard (DNS rebinding,
  421 `invalid_host`) also accepts exactly these names — for a same-machine reverse proxy that forwards the original
  Host. No effect on a network listener (no guard there). WebSocket Origin checks are unchanged (same origin).
* **Start-up banner (B0).** `server.listenURLs`: a wildcard listener (`0.0.0.0`, `::`, empty host) is shown by its
  primary network address (private IPv4 first, then other IPv4, then unique-local / global IPv6), with `Also at:` lines
  for the host name, the other addresses and loopback (`0.0.0.0` lists IPv4 only; link-local skipped). Before, Go's
  `[::]` report of a `0.0.0.0` listener printed `https://[::1]:port`. The one-time `?setup=` / `?launch=` token is only
  on the primary URL. Loopback and specific-address listeners print one URL as before.
* **Loading primitives.** `Button loading`: blocks clicks (and implicit form submission) at once, but the spinner and
  the disabled look appear together only after the delay (no 250 ms dim on a fast sign-in). `LazyBoundary`,
  `LoadingPane`, `LoadingState`, `QueryState` take `timing` (a `DELAY_PRESETS` entry). `DelayedFlag`: the minimum
  visible time counts from the first painted frame (`Clock.afterPaint`, double rAF), so ≥ 600 / 800 ms really hold.
  `index.css`: steady text caret (`caret-animation: manual`); reduced motion no longer sets `transition-duration:
  0.01ms` on everything (that made every `visibility`/colour change a transition, so a replaced view — e.g. the previous
  tab when a new one opens — stayed painted for a frame, and for the whole long task that followed); it now disables
  transitions (0 s) and runs animations once (the spinner is a still ring).
* **Closed tabs return to their place.** `ClosedTab.placement?: TabPlacement` (`{tabId, groupId, index, prevId?,
  nextId?}`, recorded by `closeTab`); `openTab({…, placement})` inserts after the old left neighbour, else before the
  old right one, else at the old index of the old group, else like a new tab (neighbours that were reopened under a new
  id are followed). `reopenClosed(index)` now returns `Promise<boolean>` and awaits `TabKindDef.reopen`, which may
  return a promise and should pass `placement: closed.placement` (terminal, VNC, RDP do; `features/terminal/open`
  `OpenOptions.placement`). `closeUndo.undoAll()` reopens most-recent-first, one at a time, so a burst of closed tabs
  comes back in its original order.
* **Settings navigation** is grouped: `SETTINGS_GROUPS` (general, appearance, terminal "Terminal & Editor",
  connections, files, security, integrations, about) and `SettingsSectionDef.group?`; sections without `group` use a
  fallback map in `SettingsView` (editor/automation → terminal, termTransfer → files, recordings → security,
  ai/webproxy → integrations, unknown → integrations). `order` now orders within a group. The search also matches group
  titles; section switches use the NAVIGATION timing.
* **One quick-connect field.** `layout/QuickConnect.useToolbarQuickConnect()` (= toolbar shown and not a phone): Home
  renders its large field only when the toolbar has none, else a "Quick connect" action card (`quickConnect.focus`).
* **Phones.** Group header: an "All tabs (n)" button opening a bottom sheet (`layout/workspace/TabsSheet`) — every
  tab with icon, full title, status, switch / close; dockview's overflow chip is hidden below 768 px; split / maximize
  buttons are desktop-only.
* **Status bar.** The running-sessions item reads "2 sessions" on wide screens (count only below `lg`).
* **Session tree (F1b).** Settings `sessions.groupBy: 'folder'|'protocol'|'tag'` (default folder) and
  `collapsedGroups: string[]`; the sort menu ("Sort and group") has a Group by section; the protocol / tag views
  (`panel/GroupedSessions`, `model.groupConnections`) are collapsible groups of the filtered sessions sorted by the sort
  mode (manual → name; a session appears under each of its tags, "Untagged" last). Rows show up to two tag chips on
  hover / keyboard focus / selection (click = filter by that tag). Coloured folders tint their count badge. Context
  menus: "Tags" (check items + "New tag…") for one or several sessions and "Colour" for a multi-selection
  (`actions.addTag / removeTag / addTagInteractive / setConnectionsColor`). Connection APIs unchanged.
* **More shared-layer changes.** `IconButton busyTiming` / `BusyIcon timing` (a `DELAY_PRESETS` entry).
  `DialogContent anchor: 'center' | 'top'` (top: anchored at `max(1rem, 10vh)` for dialogs whose content grows).
  `Tooltip` no longer passes its `data-state` to the child, so it can wrap a `DropdownMenuTrigger asChild` without
  overwriting the trigger's `data-state="open"`. `lib/utils.formatCalendarTime(value, now?)` — "Today 14:10",
  "Tomorrow 03:00", "Yesterday …", weekday within a week, else the date (year when different); used by monitor (boot,
  connection, process start times) and the agent key expiry. Command palette: a fixed height while open (the list
  filters inside a steady box), results whose text contains every typed word come first in each group, and a group
  with such a literal hit comes before groups with only fuzzy resemblances ("Settings" → Open Settings, not a server's
  "Configure …" or a "Recordings" tab). The layout is saved on `pagehide` too (a reload within the 400 ms save
  debounce lost the last change). `closeUndo`: a session pending its end is kept running as soon as any tab shows it
  again (reopen, attach, workspace restore). Settings keeps the previous section on screen until the next (lazy) one is
  ready (`useDeferredValue`; the nav item shows a NAVIGATION-timed spinner for a slow load). Terminal "MultiExec"
  labels read "Broadcast" (ribbon, menus, toolbar, status item, bar, palette title "Toggle Broadcast Input"; command id
  `terminal.multiexec.toggle` unchanged, "multiexec" kept as a palette keyword). `npm test` also runs
  `features/recordings/__tests__`.
* **DOM hooks.** `.nx-panel` and the dock tab carry `data-tab-id` (read by the flash audit).
* **Flash audit.** Every recording flags frames where the active group's panel on screen is not the active tab's
  ("previous tab content"); `--motion reduce|no-preference`; new scenarios / checks: Settings section switches (and the
  grouped navigation), close + reopen a tab and close + Undo a terminal (both must return to their place), the phone
  tabs sheet (lists every tab), grouping the session tree by tag and back, and a steady caret (3.5 s, focused by
  script) in a text field, the terminal, the script editor (CodeMirror) and the file editor (Monaco) — the oscillation
  detector no longer exempts the focused caret. `lint-ui` rule `caret`: `cursorBlink: true`, Monaco `cursorBlinking`
  other than 'solid', and a file importing CodeMirror without `cursorBlinkRate: 0` fail.
