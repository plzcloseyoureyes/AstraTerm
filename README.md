# Termstead

**An organized remote-management workspace.** All your servers, sessions, file systems, tunnels and remote desktops in
one self-hosted place — structured by folders, tags, identities and saved layouts, protected by an encrypted
credential vault and an audit log, and shared with your team when you want to. One self-contained binary for macOS,
Linux and Windows: run it on your laptop as a personal workspace, or on a server for your whole team.

<!-- Badges: enable after the repository is published (replace OWNER).
[![CI](https://github.com/OWNER/termstead/actions/workflows/ci.yml/badge.svg)](https://github.com/OWNER/termstead/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/OWNER/termstead?sort=semver)](https://github.com/OWNER/termstead/releases)
-->

- **Everything in one place.** SSH, RDP, VNC, telnet, serial, containers, SFTP/FTP/S3/WebDAV/SMB, tunnels, monitoring
  and network tools side by side in one tabbed workspace — no juggling a terminal app, a file client, a remote-desktop
  client and a notes file of hosts.
- **Organized.** Folders, tags, favorites, fuzzy search, reusable identities, per-connection overrides and restored
  layouts. Import what you already have from MobaXterm, PuTTY, `~/.ssh/config` and more.
- **Sessions that stay alive.** Connections live in the Termstead process, not in the browser tab: reload, close the tab
  or lose the network and the terminal re-attaches and replays what you missed.
- **Secure by default.** Loopback-only desktop mode, write-only secrets sealed in a vault (optional master password),
  TOTP, passkeys and single sign-on in server mode, an outbound network policy and a full audit trail.
- **One binary, no services to install.** Pure Go (no CGO), UI embedded, data in one directory (or beside the binary
  in portable mode).

<!-- Screenshots: add the images to docs/images/ and uncomment.
![Workspace with an SSH terminal, the SFTP side panel and remote monitoring](docs/images/workspace.png)
![Session manager with folders and tags](docs/images/sessions.png)
![Dual-pane file manager with the transfer queue](docs/images/files.png)
![RDP and VNC sessions in split tabs](docs/images/remote-desktop.png)
![Server mode: users, passkeys and audit log](docs/images/admin.png)
-->

## Contents

[Features](#features) · [Quick start](#quick-start) · [Desktop and server mode](#desktop-mode-and-server-mode) ·
[Command line](#command-line) · [Build from source](#build-from-source) · [Development](#development) ·
[Testing](#testing) · [Architecture](#architecture) · [Security](#security) ·
[Roadmap and limitations](#roadmap-and-known-limitations) · [Contributing](#contributing) · [License](#license)

## Features

**Connections & sessions**
- Session manager: folders, tags, drag & drop, favorites, recents, fuzzy search, shared (admin-managed) connections,
  reusable identities, per-connection terminal settings. Quick connect accepts `ssh -p 2222 -J bastion user@host`,
  `telnet host 23` or a URL.
- SSH with pooled connections, jump-host chains, SOCKS/HTTP proxies, ProxyCommand, port knocking, keys and
  certificates, 2FA / keyboard-interactive prompts in the browser, known hosts with CA and revocation markers, agent
  and X11 forwarding.
- Telnet (+TLS), rlogin/rsh, raw TCP/TLS/UDP, serial (flow control, autobaud, hex monitor), local shells (PTY, ConPTY,
  WSL), Docker and Kubernetes exec/logs, Mosh (built-in client), WinRM, IPMI Serial-over-LAN and power control.
- Terminal: xterm.js with WebGL, search, links, inline images, OSC 52 clipboard, shell integration (prompt marks and
  command status), paste safety, 37 colour schemes and a scheme editor, MultiExec across sessions with a
  dangerous-command guard, activity/silence monitors, instant replay, ZMODEM and trzsz transfers.
- Workspace: tabs, splits, 1/2/4 layouts, floating groups, pop-out windows, restored layout per user, command palette
  with rebindable shortcuts, dark/light/system themes, lock screen.
- Import from MobaXterm, PuTTY, `~/.ssh/config` (live sync), Termius, mRemoteNG, Remmina, FileZilla, WinSCP, SecureCRT,
  CSV and JSON; encrypted exports.

**Files**
- File browser for SFTP (SCP/shell fallback, sudo), FTP/FTPS, S3, WebDAV, SMB and the local machine; dual-pane mode,
  SFTP side panel that follows the terminal's working folder.
- Resumable chunked uploads, a transfer queue that survives restarts, archives, checksums, search, folder compare,
  previews, drag & drop onto terminals.
- Built-in editor (Monaco, bundled — no CDN) with diff, remote file picker, find in files, Vim mode, encodings and
  conflict detection.

**Remote desktops**
- RDP through IronRDP (WebAssembly, in the browser) or an optional guacd sidecar (recordings, admin shadowing).
- VNC through noVNC with server-side security (VeNCrypt, Apple Remote Desktop), listening/repeater mode.
- Web sessions: a proxy for web UIs behind SSH hosts or tunnels, and Xpra X11 applications.

**Network & tunnels**
- Port-forwarding manager: local, remote, dynamic SOCKS/HTTP, on-demand tunnels, remote-port detection.
- Network tools: ping, traceroute/mtr, port and network scan, DNS, whois, Wake-on-LAN, HTTP and TLS checks, SNMP,
  ssh-audit, throughput test.
- Embedded servers for quick jobs: HTTP, FTP, SFTP, TFTP, telnet and syslog.

**Monitoring**
- Agentless remote monitoring over SSH: CPU, memory, disk and network, processes, services, listening ports, disk
  usage, log follower; live status bar for the active session.

**Automation**
- Snippets, macros, button bars, triggers, logon actions, sandboxed JavaScript scripts, batch runs and schedules.
- Optional AI assistant (Anthropic or any OpenAI-compatible endpoint, including local models), off until configured,
  with secret redaction before anything leaves the server and admin-controlled models and rate limits.

**Security & teams**
- Credential vault: secrets are write-only in the API and sealed with XChaCha20-Poly1305; optional master password
  (argon2id). SSH key manager (generate, import, convert, PuTTY PPK, certificates, install on hosts) and a built-in
  SSH agent.
- Server mode: multiple users, TOTP, passkeys, OIDC single sign-on, API tokens, login back-off and lockout policy,
  admin-managed shared connections, outbound network policy for non-admin users.
- Recordings (asciicast, text logs, RDP through guacd) with player, search and retention; command audit; audit log;
  read-only or interactive session sharing links; admin backup and restore.

## Quick start

### 1. Download

Pick the archive for your platform from the [Releases](https://github.com/OWNER/termstead/releases) page:

| Platform | Archive |
|---|---|
| macOS (Apple silicon / Intel) | `termstead_<version>_darwin_arm64.tar.gz` / `termstead_<version>_darwin_amd64.tar.gz` |
| Linux (x86-64 / ARM64) | `termstead_<version>_linux_amd64.tar.gz` / `termstead_<version>_linux_arm64.tar.gz` |
| Windows (x64 / ARM64) | `termstead_<version>_windows_amd64.zip` / `termstead_<version>_windows_arm64.zip` |
| FreeBSD (x86-64) | `termstead_<version>_freebsd_amd64.tar.gz` |

Verify it against `SHA256SUMS` (and, optionally, the build-provenance attestation):

```sh
sha256sum --ignore-missing -c SHA256SUMS          # macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
gh attestation verify termstead_<version>_linux_amd64.tar.gz --repo OWNER/termstead
tar -xzf termstead_<version>_linux_amd64.tar.gz     # Windows: extract the .zip
```

Each archive holds the `termstead` executable, this README, the license, the changelog and the third-party notices.

> **Unsigned binaries.** Release binaries are not code-signed yet. On macOS, clear the download quarantine once with
> `xattr -d com.apple.quarantine ./termstead`; on Windows, SmartScreen may ask you to confirm the first start.

### 2. Run it on your machine (desktop mode)

```sh
./termstead            # Windows: termstead.exe
```

Termstead listens on `http://127.0.0.1:7822` and opens your browser. The first start shows a short setup wizard that
creates your account; later starts sign you in automatically through a one-time launch link. Use a current Chrome,
Edge, Firefox or Safari.

### 3. Or run it for a team (server mode)

```sh
./termstead --mode server --listen 0.0.0.0:7822 --tls-cert cert.pem --tls-key key.pem
# quick trial with a generated certificate: --tls-self-signed
```

The banner prints a one-time `https://…/?setup=<token>` link: only that link can create the first administrator.
Then add users, require TOTP or passkeys, or connect an OIDC identity provider from the admin settings.

### Where your data lives

| | Data directory (database, keys, recordings, logs) |
|---|---|
| macOS | `~/Library/Application Support/termstead` |
| Linux / FreeBSD | `$XDG_CONFIG_HOME/termstead` (usually `~/.config/termstead`) |
| Windows | `%AppData%\termstead` |
| `--data-dir DIR` | `DIR` |
| Portable mode | `termstead-data/` beside the executable — enabled by `--portable`, or automatically when a `termstead.portable` file or a `termstead-data/` folder sits next to it (USB sticks, per-project copies) |

Files are created with `0600`/`0700` permissions. Back the directory up while Termstead is stopped, or use the admin
backup in Settings. Forgot the password? `termstead reset-password <user>` (offline, prompts for the new one).

### Running as a service (server mode)

A minimal systemd unit; put the TLS files and data directory where the service user can read them:

```ini
[Unit]
Description=Termstead
After=network-online.target

[Service]
User=termstead
ExecStart=/usr/local/bin/termstead --mode server --listen 0.0.0.0:7822 --data-dir /var/lib/termstead \
  --tls-cert /etc/termstead/cert.pem --tls-key /etc/termstead/key.pem --no-open
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/termstead

[Install]
WantedBy=multi-user.target
```

Behind a reverse proxy, see [SECURITY.md → Hardening](SECURITY.md#hardening).

## Desktop mode and server mode

| | Desktop (default) | Server (`--mode server`) |
|---|---|---|
| Users | one person on their own machine | several accounts on a network |
| Bind | `127.0.0.1` only | any address; non-loopback needs TLS (`--tls-cert/--tls-key` or `--tls-self-signed`) unless `--insecure-http` |
| Login | one-time launch link | username/password, TOTP, passkeys, OIDC SSO, API tokens |
| First admin | setup wizard | only through the one-time `https://…/?setup=<token>` link from the banner |
| Host features | local shells, serial ports, embedded servers, local files, host SSH agent, X11 | admin-only (or off for users) |
| Outbound connections | unrestricted | vetted by the network policy (loopback, link-local, cloud metadata and the host's own addresses refused for non-admins; admin-editable) |

## Command line

| Command | |
|---|---|
| `termstead [serve] [flags]` | start the server (default) |
| `termstead version` | print the version, commit, build date and Go version |
| `termstead reset-password <user> [--data-dir D]` | set a user's password offline (prompts; revokes their sessions) |
| `termstead help` | all flags |

Every flag also has an environment variable `TERMSTEAD_<NAME>` (e.g. `TERMSTEAD_LISTEN`):

| Flag | Default | |
|---|---|---|
| `--listen` | `127.0.0.1:7822` | `host:port`; port `0` picks a free one |
| `--mode` | `desktop` | `desktop` or `server` |
| `--data-dir` | per-user config dir `/termstead` | SQLite database, keys, recordings, logs |
| `--portable` | off | keep data in `termstead-data/` beside the executable |
| `--tls-cert`, `--tls-key`, `--tls-self-signed` | off | serve HTTPS |
| `--insecure-http` | off | allow plain HTTP on a network address (only behind a TLS-terminating proxy on a trusted network) |
| `--trusted-proxies` | none | CIDRs whose `X-Forwarded-For` / `X-Forwarded-Proto` are trusted |
| `--allowed-hosts` | none | extra host names a loopback listener accepts (a same-machine reverse proxy's public name, e.g. `termstead.example.com`) |
| `--open` / `--no-open` | open in desktop mode | open the browser on start |
| `--scrollback-bytes` | `4MiB` | per-session replay buffer (minimum 2 MiB) |
| `--detached-ttl` | `24h` | close sessions no browser has attached to for this long (`0` = never) |
| `--guacd` | `127.0.0.1:4822` | optional guacd for the Guacamole RDP engine (`off` disables) |
| `--log-level` | `info` | `debug` · `info` · `warn` · `error` (stderr) |
| `--dev` | off | allow the Vite dev-server origins (`localhost:5173`) |

Optional helpers are detected at run time and features degrade gracefully without them: guacd (RDP recordings and
shadowing), a Docker socket, `kubectl`.

## Build from source

Requirements: Go 1.26+, Node.js 22+ with npm, `make`. Details, cross-compilation and reproducible builds:
[docs/BUILDING.md](docs/BUILDING.md).

```sh
make build        # web deps from the lockfile (first time), UI build + precompression, then bin/termstead
./bin/termstead version
make release      # archives for every platform + SHA256SUMS in dist/
```

## Development

```sh
make dev-backend   # go run ./cmd/termstead --dev --no-open --data-dir ./.termstead-data   (API on :7822)
make dev-web       # Vite on http://localhost:5173 with HMR; proxies /api and /ws to :7822 (TERMSTEAD_BACKEND overrides)
```

Open `http://localhost:5173/` (append the backend banner's `?launch=<token>` to skip the login in desktop mode).
When you try Termstead out, give it its own `--data-dir`; for throwaway instances also point `HOME` (and `USERPROFILE`
on Windows) at a temporary directory so the Local files tab and importers never see your real home folder.

## Testing

| Command | What it runs |
|---|---|
| `make check` | gofmt check, `go vet`, Go tests, TypeScript typecheck, oxlint + UI guardrail, frontend unit tests |
| `make test` / `make test-race` | Go tests (`CGO_ENABLED=0`) / with the race detector |
| `make test-web` | frontend unit tests (`node --test`) |
| `make typecheck`, `make lint`, `make lint-ui` | TypeScript / oxlint + UI guardrail / loading-state guardrail only |
| `make cross-check` | compile every package for every release platform |
| `make smoke` | build, then the end-to-end smoke test against a Docker SSH server ([scripts/smoke](scripts/smoke/README.md)) |
| `make flash-audit` | boot and loading-state flash measurement in headless Chrome |
| `make notices-check` | `THIRD_PARTY_NOTICES.md` matches the current dependencies |

Tests that need network targets are opt-in. The Docker lab starts SSH (+ jump host), telnet, FTP, VNC, RDP (xrdp),
guacd, S3, WebDAV and SMB on fixed loopback ports with throwaway credentials (listed in the compose file header):

```sh
docker compose -f scripts/testenv/docker-compose.yml up -d
TERMSTEAD_TESTENV=1 go test ./...                  # tests that use the lab
TERMSTEAD_TEST_DOCKER=1 go test ./...              # tests that start their own containers
docker compose -f scripts/testenv/docker-compose.yml down
```

## Architecture

```text
 Browser (React 19 · Vite 8 · TS 7 · Tailwind 4)                    Go 1.26 backend (one process, one binary)
 ┌─────────────────────────────────────────────┐   REST /api/*     ┌──────────────────────────────────────────────┐
 │ shell: menu · toolbar · sidebar · status bar │ ───────────────▶ │ httpx (Echo v5): auth · CSRF · Origin · Host  │
 │ dockview workspace (tabs/splits/pop-outs)    │                  │ auth · vault · store (SQLite) · audit         │
 │ features/* self-register via app/registry    │  /ws/events      │ events hub: prompts · jobs · topics           │
 │ react-query (REST) · zustand (UI state)      │ ◀──────────────▶ │ term: sessions · replay ring · recorder       │
 │ components/ui design system                  │  /ws/terminal …  │ sshx pool · proto/* · vfs · rdp · vnc · …     │
 └─────────────────────────────────────────────┘ ◀══ bytes+acks ══▶ │ netguard: outbound destination policy         │
                                                                    └──────────────────────────────────────────────┘
                                                                        │ SSH / TCP / UDP / PTY (the browser never
                                                                        ▼ talks to remote hosts)
```

- The browser only talks to Termstead; every protocol runs in the Go backend. Sessions are owned by the server and
  survive reloads (byte-offset replay with acknowledgement-based flow control).
- Backend packages follow a strict import DAG (`model → config → httpx → store → vault → events → audit → app → auth →
  term → sshx → proto/* / feature modules → server → cmd`). `internal/server` builds `app.Deps`, mounts every module
  (`internal/server/modules.go`) and serves the embedded UI (`internal/webui`, precompressed at build time).
- Frontend features live in `web/src/features/<name>/` and plug into registries (tab kinds, sidebar panels, commands,
  menus, settings sections, protocol editors, status items, terminal plugins). Shared JSON types in
  `web/src/api/types.ts` mirror `internal/model`.

Further reading: [docs/SPEC.md](docs/SPEC.md) (binding contract: API shapes, WebSocket protocols, package DAG, module
notes), [docs/UX.md](docs/UX.md) (binding UX rules), [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) (structure and
conventions), [docs/BUILDING.md](docs/BUILDING.md), [docs/RELEASING.md](docs/RELEASING.md).

## Security

- **Desktop mode** binds loopback only, rejects non-local `Host` headers (DNS rebinding) and signs you in with a
  single-use launch token. **Server mode** requires TLS on network addresses, and only the one-time `?setup=` link
  can create the first administrator.
- **Secrets never leave the backend**: connection and identity secrets are write-only in the API and sealed with
  XChaCha20-Poly1305 (optional master password, argon2id); typed secrets are kept out of recordings.
- **Web hardening**: SameSite=Strict cookies plus a CSRF header, WebSocket Origin checks, a strict Content Security
  Policy and security headers, login back-off and lockout, TOTP and passkeys, audit log.
- **Outbound network policy**: in server mode every connection made for non-admin users is vetted at dial time
  (loopback, link-local, cloud metadata and the host's own addresses are refused by default).
- Changed host keys need confirmation; in server mode only admins may accept a changed key, use the host SSH agent or
  run ProxyCommand.

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md) for the process, supported versions and
deployment hardening.

## Roadmap and known limitations

- **Pre-1.0.** Configuration and APIs may still change between minor versions; read the [changelog](CHANGELOG.md)
  before upgrading and keep a backup of the data directory.
- **Unsigned binaries.** macOS notarization and Windows Authenticode signing are planned.
- **Platforms.** 32-bit ARM (e.g. older Raspberry Pi OS) is not built yet; FreeBSD builds are compiled and packaged
  but not tested on real systems.
- **Optional helpers.** RDP recordings and admin shadowing need a guacd sidecar; Kubernetes sessions need `kubectl`.
- **Planned:** signed and notarized binaries, package-manager distribution (Homebrew, winget, deb/rpm), a container
  image and 32-bit ARM builds.

Ideas and bug reports are welcome in the issue tracker.

## Contributing

Contributions are welcome. Read [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) first (structure, conventions, tests),
run `make check` before opening a pull request, and follow the [Code of Conduct](CODE_OF_CONDUCT.md). Security issues go
through [SECURITY.md](SECURITY.md), not public issues.

## License

> **License to be decided.** The project owner has not chosen a license yet, so no open-source license is granted
> until a `LICENSE` file is added to this repository. The release workflow refuses to publish a release without one.

Third-party components keep their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Acknowledgements

Termstead stands on the shoulders of excellent open-source projects, among them
[xterm.js](https://xtermjs.org/), [Monaco Editor](https://microsoft.github.io/monaco-editor/),
[noVNC](https://novnc.com/), [IronRDP](https://github.com/Devolutions/IronRDP),
[Apache Guacamole](https://guacamole.apache.org/), [dockview](https://dockview.dev/), [React](https://react.dev/),
[Vite](https://vite.dev/), [Tailwind CSS](https://tailwindcss.com/), [Radix UI](https://www.radix-ui.com/),
[Echo](https://echo.labstack.com/), [golang.org/x/crypto/ssh](https://pkg.go.dev/golang.org/x/crypto/ssh),
[pkg/sftp](https://github.com/pkg/sftp), [modernc.org/sqlite](https://gitlab.com/cznic/sqlite),
[goja](https://github.com/dop251/goja) and many more — the complete list with licenses is in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
