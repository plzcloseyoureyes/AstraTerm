# Contributing to NexTerm

This is the practical guide. The binding contracts are [SPEC.md](SPEC.md) (API shapes, WebSocket protocols, package
DAG, per-module notes in §9) and [UX.md](UX.md) (interaction and loading-state rules). When the two disagree with this
file, they win.

## Project structure

```text
cmd/nexterm/                 CLI: flags, serve / version / reset-password
internal/
  model config httpx store vault events audit app     core services (SPEC §4); app.Deps wires them
  auth webauthn oidc                                    accounts, sessions, 2FA, passkeys, SSO
  term sshx proto/*                                     runtime sessions (ring buffer, WS), SSH pool, other protocols
  netguard                                              outbound destination policy (SSRF guard)
  vfs transfer tunnel keys vnc rdp monitor tools        feature modules (one Mount each)
  servers automation recording importer ai webproxy
  server                                                builds Deps, mounts every module (modules.go), serves the SPA
  webui                                                 go:embed of the built frontend
web/src/
  api/ stores/ lib/ components/ui/ layout/ app/         shared shell: REST client + react-query, zustand stores,
                                                        hooks, design-system primitives, app shell, registries
  features/<name>/index.ts                              one folder per feature; self-registers through app/registry
scripts/                                                smoke test, Docker lab (testenv), lint-ui guardrail
```

## Module ownership and extension points

- Every backend feature package exposes `func Mount(d *app.Deps, c *core.Core) error` and is listed once in
  `internal/server/modules.go` (edit only your own line). Migrations: `store.RegisterMigration(module, version, sql)`.
- Every frontend feature registers itself from `features/<name>/index.ts` through `web/src/app/registry.ts`: tab
  kinds, sidebar panels, commands (`registerCommand`, run with `runCommand(id, args)`), menus, context menus (`group`
  places items into a target's built-in sections, e.g. the terminal menu's `selection`), settings sections, protocol
  editors, status items, terminal plugins, overlays. Never import another feature's internals: use its commands or
  documented bus (e.g. `features/terminal/bus.ts`).
- Shared JSON types live in `web/src/api/types.ts` and mirror `internal/model` field for field; feature-only types go
  in `features/<f>/types.ts`.
- Document every deviation or extra endpoint of your module in SPEC §9 (append-only).

## Backend conventions

- **Echo handlers** (`internal/httpx`): register on `d.Router.API()` (signed in), `.Admin()` or `.Public()`; WebSockets
  with `d.Router.WS` and accept with `httpx.AcceptWS`. Handlers are `func(c *echo.Context) error`: bind with
  `httpx.Bind` / `BindOptional`, read the caller with `httpx.UserFrom(c)`, reply with `c.JSON(status, v)` or
  `httpx.OK(c)`. Never keep `*echo.Context` after the handler returns (pass `c.Request().Context()` down).
- **Errors**: just `return err`. Use the typed errors (`httpx.ErrNotFound`, `httpx.BadRequest(msg)`,
  `httpx.Conflict(msg)`, `httpx.Forbidden(msg)`, `httpx.NewError(status, code, msg)`, `httpx.ErrLocked`, `model.Err*`);
  they render as `{error, code}`. Anything else becomes a logged 500 without details — and 5xx messages are replaced,
  so user-facing failures must be 4xx.
- **Outbound connections** made for users go through `internal/netguard` — `sshx.Pool.Dialer` / `DialConnection` for
  protocol traffic (proxies, jump hosts, `sshTunnelVia` included), or `netguard.ForUser(d, user).Dialer(...)` /
  `.Transport(...)` / `.CheckIP(...)` for anything else. Never dial with a bare `net.Dialer` or `http.DefaultClient`.
- **Secrets** are write-only in the API (responses list `secretKeys`), stored sealed by the vault, resolved server-side
  with `d.ResolveConnection` / `Session.Resolve`. Never log them, never put them in URLs, never return them (the RDP
  ticket for IronRDP's in-browser CredSSP is the documented exception). Typed secrets go through
  `term.Manager.WriteSensitive` (kept out of input recordings and masked for hooks). Audit security-relevant actions
  with `d.Audit.Log(c, action, target, details)` — details never contain secrets.
- Long-lived goroutines end with `d.Ctx`; anything that writes to the store during shutdown exposes a wait that
  `server.Close` calls before closing the database. Logging: `d.Log` (`log/slog`), no `fmt.Println`.
- Pure Go only: `CGO_ENABLED=0`. Dependencies are pinned (`go.mod`, `web/package-lock.json`); do not add any.

## Frontend conventions

- UI primitives only (`web/src/components/ui/*`) and design tokens (`web/src/index.css`): no one-off colours, shadows,
  radii or animations. Icons from lucide.
- **Loading states** follow [UX.md → Loading states](UX.md#loading-states-binding-enforced-by-make-lint-ui): `Spinner`,
  `Delayed`, `LoadingPane`, `BusyIcon`, `LoadingState`, `QueryState`, `Skeleton*`, `ProgressBar`, `StatusDot`,
  `useDelayedFlag` / `useLoadingGate` / `useManualRefresh`. `make lint-ui` fails on raw `animate-*` loading classes,
  ad-hoc skeletons and private timing hooks.
- REST calls go through `web/src/api/client.ts` (adds the CSRF header, typed `ApiError`); server state lives in
  react-query, UI state in zustand stores. Heavy libraries are lazy-loaded (`React.lazy` / dynamic `import()`).
- Every action is a command (palette, rebindable shortcut); menus reference commands.

## Tests

Each module ships tests next to its code:
- Go: unit tests plus API tests on the in-process harness `internal/server/servertest` (`servertest.New(t)`,
  `env.Setup`, `env.Login`, `Client.MustJSON`). Network targets are opt-in through environment variables (e.g.
  `NEXTERM_TEST_DOCKER=1`, `NEXTERM_TESTENV=1` with the Docker lab in `scripts/testenv`).
- Frontend: pure modules are tested with `node --test` (`web/src/<area>/__tests__/*.test.mjs`, registered in
  `web/package.json` → `npm test`).
- End to end: `make smoke` (built binary, desktop + server mode, Docker SSH target).

## Running the checks

```sh
make check          # gofmt, go vet, Go tests, TypeScript, oxlint + lint-ui, frontend unit tests
make test-race      # Go tests with the race detector
make cross-check    # compile for darwin/linux/windows × amd64/arm64
make smoke          # end-to-end smoke test (needs Docker)
make flash-audit    # boot / loading flash measurement in headless Chrome (needs Chrome): scripts/flash-audit.mjs
```

`scripts/flash-audit.mjs` records every painted frame of a throwaway instance (temporary HOME and data dir) while it
loads, reloads, opens a terminal and several tabs, switches the theme and pops a tab out and back, navigates folders in
the files tab and — with an SSH target (`--ssh`, default: the Docker lab's ssh1 when it is up) — connects and reconnects
SSH and navigates / follows `cd` in the SFTP side panel under 0 / 150 / 350 / 700 / 1500 ms latency. It reports
"visual flashes" (a screen region that changes and changes back within 400 ms; in the file browser also small regions
— a 2 px bar, a spinner, a badge — within 1 s), oscillation anywhere on screen (a small element pulsing or blinking on
its own), indicators shown for less than 500 ms (750 ms for folder navigation) or more often than once per navigation,
and fails with `--strict`. Run it (also with `--latency 250`, which exposes indicators fast loopback hides) after UI
changes to loading, boot or layout code; `--keep` keeps the frames of every flash as PNGs, `--nav-only` runs just the
navigation / SSH scenarios.

Any throwaway NexTerm you start for manual testing must run with `HOME` (and `USERPROFILE` on Windows) pointing at a
temporary directory and its own `--data-dir`, so the Local files tab and importers never see a real home folder.
