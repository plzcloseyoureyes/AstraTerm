# AstraTerm

**An organized remote-management workspace.** Your servers, sessions, file systems, tunnels and remote desktops in one
self-hosted place, organized by folders, tags, identities and saved layouts. Credentials sit in an encrypted vault and
every action lands in an audit log. Run it as a desktop app on your laptop, or as a server for your whole team, from
the same self-contained download for macOS, Linux, Windows and FreeBSD.

[![CI](https://github.com/plzcloseyoureyes/astraterm/actions/workflows/ci.yml/badge.svg)](https://github.com/plzcloseyoureyes/astraterm/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/plzcloseyoureyes/astraterm?sort=semver&include_prereleases)](https://github.com/plzcloseyoureyes/astraterm/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

- **Everything in one place.** SSH, RDP, VNC, telnet, serial, containers, SFTP/FTP/S3/WebDAV/SMB, tunnels, monitoring
  and network tools side by side in one tabbed workspace. No juggling a terminal app, a file client, a remote-desktop
  client and a notes file of hosts.
- **Organized.** Folders, tags, favorites, fuzzy search, reusable identities, per-connection overrides and restored
  layouts. Import what you already have from MobaXterm, PuTTY, `~/.ssh/config` and more.
- **Sessions that stay alive.** Connections live in the AstraTerm process, not in the window: reload, close it or
  lose the network, and the terminal re-attaches and replays what you missed.
- **Secure by default.** Loopback-only desktop mode, write-only secrets sealed in a vault (optional master password),
  TOTP, passkeys and single sign-on in server mode, an outbound network policy and a full audit trail.
- **Nothing to install.** Pure Go (no CGO) with the UI embedded, and all data in one directory, or beside the
  executable in portable mode.

<!-- Screenshots: add the images to docs/images/ and uncomment.
![Workspace with an SSH terminal, the SFTP side panel and remote monitoring](docs/images/workspace.png)
![Session manager with folders and tags](docs/images/sessions.png)
![Dual-pane file manager with the transfer queue](docs/images/files.png)
![RDP and VNC sessions in split tabs](docs/images/remote-desktop.png)
![Server mode: users, passkeys and audit log](docs/images/admin.png)
-->

## Contents

[Quick start](#quick-start) · [Desktop app](#the-desktop-app) · [Desktop and server mode](#desktop-mode-and-server-mode) ·
[Features](#features) · [Command line](#command-line) · [Build from source](#build-from-source) ·
[Development](#development) · [Architecture](#architecture) · [Security](#security) ·
[Roadmap](#roadmap-and-known-limitations) · [Contributing](#contributing) · [License](#license)

## Quick start

### 1. Download

Get the files for your platform from the [Releases](https://github.com/plzcloseyoureyes/astraterm/releases) page:

| Platform | Desktop app (start here) | Server / command line |
|---|---|---|
| macOS | `AstraTerm_<version>_aarch64.dmg` (Apple silicon) · `AstraTerm_<version>_x64.dmg` (Intel) | `astraterm_<version>_darwin_arm64.tar.gz` · `…_darwin_amd64.tar.gz` |
| Windows | `AstraTerm_<version>_x64-setup.exe` (or the `.msi`) | `astraterm_<version>_windows_amd64.zip` · `…_windows_arm64.zip` |
| Linux | `AstraTerm_<version>_amd64.AppImage` / `.deb` / `.rpm` (x64 and ARM64) | `astraterm_<version>_linux_amd64.tar.gz` · `…_arm64` · `…_armv7` |
| FreeBSD | | `astraterm_<version>_freebsd_amd64.tar.gz` |

- **The desktop app** is AstraTerm in its own window, with a normal installer. Use it on your own computer.
- **`astraterm`** is the server and command-line tool: AstraTerm in a browser tab, for a team, as a service, and admin
  commands. Each archive also holds the README, license, changelog and third-party notices.

Optionally, verify a download against `SHA256SUMS` and its build-provenance attestation:

```sh
sha256sum --ignore-missing -c SHA256SUMS          # macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
gh attestation verify <file> --repo plzcloseyoureyes/astraterm
```

### 2. Install the desktop app

- **macOS:** open the `.dmg` and drag AstraTerm into Applications. The app is not notarized yet, so macOS blocks the
  first start: open **System Settings → Privacy & Security** and click **Open Anyway** (once).
- **Windows:** run `AstraTerm_<version>_x64-setup.exe` (installs for your user, no admin rights needed; it offers to
  install the WebView2 runtime if Windows lacks it). SmartScreen may warn about the unsigned installer: **More info →
  Run anyway**.
- **Linux:** install the `.deb` (`sudo apt install ./AstraTerm_<version>_amd64.deb`) or `.rpm`, or make the
  `.AppImage` executable and run it.

AstraTerm opens in its own window. The first start shows a short setup wizard that creates your account; after that
you are signed in automatically. See [The desktop app](#the-desktop-app) for details.

Prefer a browser tab? Extract the server archive and run `./astraterm`: it listens on `http://127.0.0.1:7822` and
opens your default browser. Use a current Chrome, Edge, Firefox or Safari.

### 3. Or run it for a team (server mode)

```sh
./astraterm --mode server --listen 0.0.0.0:7822 --tls-cert cert.pem --tls-key key.pem
# quick trial with a generated certificate: --tls-self-signed
```

The banner prints a one-time `https://…/?setup=<token>` link, and only that link can create the first administrator.
From there, add users, require TOTP or passkeys, or connect an OIDC identity provider in the admin settings.

### Where your data lives

| | Data directory (database, keys, recordings, logs) |
|---|---|
| macOS | `~/Library/Application Support/astraterm` |
| Linux / FreeBSD | `$XDG_CONFIG_HOME/astraterm` (usually `~/.config/astraterm`) |
| Windows | `%AppData%\astraterm` |
| `--data-dir DIR` | `DIR` |
| Portable mode | `astraterm-data/` beside the executable. Turned on by `--portable`, or automatically when a `astraterm.portable` file or a `astraterm-data/` folder sits next to it (USB sticks, per-project copies). |

Files are created with `0600`/`0700` permissions. Back the directory up while AstraTerm is stopped, or use the admin
backup in Settings. Forgot your password? Run `astraterm reset-password <user>`: it works offline and prompts for the
new one.

### Running as a service (server mode)

A minimal systemd unit; put the TLS files and data directory where the service user can read them:

```ini
[Unit]
Description=AstraTerm
After=network-online.target

[Service]
User=astraterm
ExecStart=/usr/local/bin/astraterm --mode server --listen 0.0.0.0:7822 --data-dir /var/lib/astraterm \
  --tls-cert /etc/astraterm/cert.pem --tls-key /etc/astraterm/key.pem --no-open
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/astraterm

[Install]
WantedBy=multi-user.target
```

If it runs behind a reverse proxy, see [SECURITY.md → Hardening](SECURITY.md#hardening).

## The desktop app

The desktop app (`desktop/`, built with [Tauri](https://tauri.app/)) is AstraTerm in a native window. Inside it runs
the same `astraterm` server as everywhere else, unchanged.

- **A native window** on the operating system's own web engine (WebView2 on Windows, WebKit on macOS and Linux),
  remembering its size and position. Downloads go to your Downloads folder; links to other sites open in your
  browser; pop-out tabs become native windows.
- **It is still a server.** While the app runs, AstraTerm keeps listening on `127.0.0.1:7822` (or on a free port when
  another program already uses 7822), so you can also open it in a browser tab.
- **One instance.** Starting it again brings the open window to the front. If `astraterm` is already running on the
  same port, the app shows that instance instead of starting a second one.
- **Closing the window quits AstraTerm**, and the server shuts down cleanly. Its log is written to
  `astraterm-desktop.log` in the data directory, and a startup problem is shown in a dialog.

How it works, how it is built and what comes next (signing, auto-update): [docs/DESKTOP.md](docs/DESKTOP.md).

## Desktop mode and server mode

| | Desktop (default) | Server (`--mode server`) |
|---|---|---|
| Users | one person on their own machine | several accounts on a network |
| Bind | `127.0.0.1` only | any address; non-loopback addresses need TLS (`--tls-cert/--tls-key` or `--tls-self-signed`) unless `--insecure-http` |
| Login | one-time launch link | username/password, TOTP, passkeys, OIDC SSO, API tokens |
| First admin | setup wizard | only through the one-time `https://…/?setup=<token>` link from the banner |
| Host features | local shells, serial ports, embedded servers, local files, host SSH agent, X11 | admin-only (or off for users) |
| Outbound connections | unrestricted | checked against the network policy (loopback, link-local, cloud metadata and the host's own addresses refused for non-admins; admins can edit it) |

## Features

**Connections & sessions**
- Session manager: folders, tags, drag & drop, favorites, recents, fuzzy search, shared (admin-managed) connections,
  reusable identities, per-connection terminal settings. Quick connect accepts `ssh -p 2222 -J bastion user@host`,
  `telnet host 23` or a URL.
- SSH with pooled connections, jump-host chains, SOCKS/HTTP proxies, ProxyCommand, port knocking, keys and
  certificates, 2FA / keyboard-interactive prompts in the UI, known hosts with CA and revocation markers, agent and X11
  forwarding.
- Telnet (+TLS), rlogin/rsh, raw TCP/TLS/UDP, serial (flow control, autobaud, hex monitor), local shells (PTY, ConPTY,
  WSL), Docker and Kubernetes exec/logs, Mosh (built-in client), WinRM, IPMI Serial-over-LAN and power control.
- Terminal: xterm.js with WebGL, search, links, inline images, OSC 52 clipboard, shell integration (prompt marks and
  command status), paste safety, 37 colour schemes and a scheme editor, MultiExec across sessions with a
  dangerous-command guard, activity/silence monitors, instant replay, ZMODEM and trzsz transfers.
- Workspace: tabs in the title bar, splits, 1/2/4 layouts, floating groups, pop-out windows, restored layout per user,
  command palette with rebindable shortcuts, dark/light/system themes, lock screen.
- Import from MobaXterm, PuTTY, `~/.ssh/config` (live sync), Termius, mRemoteNG, Remmina, FileZilla, WinSCP, SecureCRT,
  CSV and JSON; encrypted exports.

**Files**
- File browser for SFTP (SCP/shell fallback, sudo), FTP/FTPS, S3, WebDAV, SMB and the local machine; dual-pane mode,
  and an SFTP side panel that follows the terminal's working folder.
- Resumable chunked uploads, a transfer queue that survives restarts, archives, checksums, search, folder compare,
  previews, drag & drop onto terminals.
- Built-in editor (Monaco, bundled with no CDN) with diff, remote file picker, find in files, Vim mode, encodings and
  conflict detection.

**Remote desktops**
- RDP through IronRDP (WebAssembly, in the UI) or an optional guacd sidecar (recordings, admin shadowing).
- VNC through noVNC with server-side security (VeNCrypt, Apple Remote Desktop), listening/repeater mode.
- Web sessions: a proxy for web UIs behind SSH hosts or tunnels, and Xpra X11 applications.

**Network & tunnels**
- Port-forwarding manager: local, remote, dynamic SOCKS/HTTP, on-demand tunnels, remote-port detection.
- Network tools: ping, traceroute/mtr, port and network scan, DNS, whois, Wake-on-LAN, HTTP and TLS checks, SNMP,
  ssh-audit, throughput test.
- Embedded servers for quick jobs: HTTP, FTP, SFTP, TFTP, telnet and syslog.

**Monitoring & automation**
- Agentless remote monitoring over SSH: CPU, memory, disk and network, processes, services, listening ports, disk
  usage, log follower; live status bar for the active session.
- Snippets, macros, button bars, triggers, logon actions, sandboxed JavaScript scripts, batch runs and schedules.
- Optional AI assistant (Anthropic or any OpenAI-compatible endpoint, including local models). It stays off until
  configured, redacts secrets before anything leaves the server, and admins control the models and rate limits.

**Security & teams**
- Credential vault: secrets are write-only in the API and sealed with XChaCha20-Poly1305; optional master password
  (argon2id). SSH key manager (generate, import, convert, PuTTY PPK, certificates, install on hosts) and a built-in
  SSH agent.
- Server mode: multiple users, TOTP, passkeys, OIDC single sign-on, API tokens, login back-off and lockout policy,
  admin-managed shared connections, outbound network policy for non-admin users.
- Recordings (asciicast, text logs, RDP through guacd) with player, search and retention; command audit; audit log;
  read-only or interactive session sharing links; admin backup and restore.

## Command line

| Command | |
|---|---|
| `astraterm [serve] [flags]` | start AstraTerm for a browser (the default command) |
| `astraterm version` | print the version, commit, build date and Go version |
| `astraterm reset-password <user> [--data-dir D]` | set a user's password offline (prompts; revokes their sessions) |
| `astraterm help` | all flags |

Every flag can also be set with an environment variable `ASTRATERM_<NAME>` (e.g. `ASTRATERM_LISTEN`):

| Flag | Default | |
|---|---|---|
| `--listen` | `127.0.0.1:7822` | `host:port`; port `0` picks a free one |
| `--mode` | `desktop` | `desktop` or `server` |
| `--data-dir` | per-user config dir `/astraterm` | SQLite database, keys, recordings, logs |
| `--portable` | off | keep data in `astraterm-data/` beside the executable |
| `--tls-cert`, `--tls-key`, `--tls-self-signed` | off | serve HTTPS |
| `--insecure-http` | off | allow plain HTTP on a network address (only behind a TLS-terminating proxy on a trusted network) |
| `--trusted-proxies` | none | CIDRs whose `X-Forwarded-For` / `X-Forwarded-Proto` are trusted |
| `--allowed-hosts` | none | extra host names a loopback listener accepts (a same-machine reverse proxy's public name, e.g. `astraterm.example.com`) |
| `--open` / `--no-open` | open in desktop mode | open the browser on start |
| `--scrollback-bytes` | `4MiB` | per-session replay buffer (minimum 2 MiB) |
| `--detached-ttl` | `24h` | close sessions no window has attached to for this long (`0` = never) |
| `--guacd` | `127.0.0.1:4822` | optional guacd for the Guacamole RDP engine (`off` disables) |
| `--log-level` | `info` | `debug` · `info` · `warn` · `error` (stderr) |
| `--dev` | off | allow the Vite dev-server origins (`localhost:5173`) |

Optional helpers are detected at run time, and features degrade gracefully without them: guacd (RDP recordings and
shadowing), a Docker socket, `kubectl`.

## Build from source

Requirements: Go 1.26+, Node.js 22+ with npm, and `make`. Details, cross-compilation and reproducible builds are in
[docs/BUILDING.md](docs/BUILDING.md).

```sh
make build        # UI (npm deps from the lockfile, vite build, precompression), then bin/astraterm
make desktop      # the desktop app for this machine (needs Rust; see docs/DESKTOP.md)
./bin/astraterm version
make cross-check  # compile every package for every release platform
make release      # the release archives for every platform + SHA256SUMS in dist/
```

Releases are built by GitHub Actions from a version tag; see [docs/RELEASING.md](docs/RELEASING.md).

## Development

```sh
make dev-backend   # go run ./cmd/astraterm --dev --no-open --data-dir ./.astraterm-data   (API on :7822)
make dev-web       # Vite on http://localhost:5173 with HMR; proxies /api and /ws to :7822 (ASTRATERM_BACKEND overrides)
```

Open `http://localhost:5173/`, appending the backend banner's `?launch=<token>` to skip the login in desktop mode. For
experiments, give AstraTerm its own `--data-dir`. For throwaway instances, also point `HOME` (and `USERPROFILE` on
Windows) at a temporary directory, so the Local files tab and the importers never see your real home folder.

| Command | What it runs |
|---|---|
| `make check` | gofmt check, `go vet`, Go tests, TypeScript typecheck, oxlint + UI guardrail, frontend unit tests |
| `make test-race` | Go tests with the race detector |
| `make smoke` | build, then the end-to-end smoke test against a Docker SSH server ([scripts/smoke](scripts/smoke/README.md)) |
| `make flash-audit` | boot and loading-state flash measurement in headless Chrome |
| `make notices-check` | `THIRD_PARTY_NOTICES.md` matches the current dependencies |

Tests that need network targets are opt-in. The Docker lab starts SSH (+ jump host), telnet, FTP, VNC, RDP (xrdp),
guacd, S3, WebDAV and SMB on fixed loopback ports with throwaway credentials (listed in the compose file header):

```sh
docker compose -f scripts/testenv/docker-compose.yml up -d
ASTRATERM_TESTENV=1 go test ./...                  # tests that use the lab
ASTRATERM_TEST_DOCKER=1 go test ./...              # tests that start their own containers
docker compose -f scripts/testenv/docker-compose.yml down
```

## Architecture

```text
 UI: desktop window or browser tab                                  Go 1.26 backend (one process, one binary)
 (React 19 · Vite 8 · TS 7 · Tailwind 4)
 ┌─────────────────────────────────────────────┐   REST /api/*     ┌──────────────────────────────────────────────┐
 │ shell: title bar with tabs · rail · sidebar  │ ───────────────▶ │ httpx (Echo v5): auth · CSRF · Origin · Host  │
 │ dockview workspace (tabs/splits/pop-outs)    │                  │ auth · vault · store (SQLite) · audit         │
 │ features/* self-register via app/registry    │  /ws/events      │ events hub: prompts · jobs · topics           │
 │ react-query (REST) · zustand (UI state)      │ ◀──────────────▶ │ term: sessions · replay ring · recorder       │
 │ components/ui design system                  │  /ws/terminal …  │ sshx pool · proto/* · vfs · rdp · vnc · …     │
 └─────────────────────────────────────────────┘ ◀══ bytes+acks ══▶ │ netguard: outbound destination policy         │
                                                                    └──────────────────────────────────────────────┘
                                                                        │ SSH / TCP / UDP / PTY (the UI never
                                                                        ▼ talks to remote hosts)
```

- The UI only talks to AstraTerm; every protocol runs in the Go backend. Sessions are owned by the server and survive
  reloads (byte-offset replay with acknowledgement-based flow control).
- The desktop app (`desktop/`, Tauri) runs the unchanged `astraterm` binary as a sidecar (`astraterm sidecar`) and
  shows its UI in a native window.
- Backend packages follow a strict import DAG (`model → config → httpx → store → vault → events → audit → app → auth →
  term → sshx → proto/* / feature modules → server → cmd`). `internal/server` builds `app.Deps`, mounts every module
  (`internal/server/modules.go`) and serves the embedded UI (`internal/webui`, precompressed at build time).
- Frontend features live in `web/src/features/<name>/` and plug into registries (tab kinds, sidebar panels, commands,
  menus, settings sections, protocol editors, status items, terminal plugins). Shared JSON types in
  `web/src/api/types.ts` mirror `internal/model`.

Further reading: [docs/SPEC.md](docs/SPEC.md) (the binding contract: API shapes, WebSocket protocols, package DAG,
module notes), [docs/UX.md](docs/UX.md) (binding UX rules), [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) (structure
and conventions), [docs/BUILDING.md](docs/BUILDING.md), [docs/RELEASING.md](docs/RELEASING.md).

## Security

- **Desktop mode** binds loopback only, rejects non-local `Host` headers (DNS rebinding) and signs you in with a
  single-use launch token. **Server mode** requires TLS on network addresses, and only the one-time `?setup=` link
  can create the first administrator.
- **Secrets never leave the backend.** Connection and identity secrets are write-only in the API and sealed with
  XChaCha20-Poly1305 (optional master password, argon2id); typed secrets are kept out of recordings.
- **Web hardening.** SameSite=Strict cookies plus a CSRF header, WebSocket Origin checks, a strict Content Security
  Policy and security headers, login back-off and lockout, TOTP and passkeys, audit log.
- **Outbound network policy.** In server mode, every connection made for non-admin users is checked when it is dialed
  (loopback, link-local, cloud metadata and the host's own addresses are refused by default).
- Changed host keys need confirmation. In server mode only admins may accept a changed key, use the host SSH agent or
  run ProxyCommand.

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md) for the process, supported versions and
deployment hardening.

## Roadmap and known limitations

- **Pre-1.0.** Configuration and APIs may still change between minor versions. Read the [changelog](CHANGELOG.md)
  before upgrading and keep a backup of the data directory.
- **Unsigned binaries.** macOS notarization and Windows Authenticode signing are planned; until then macOS and
  Windows ask for a one-time confirmation.
- **Desktop app.** Planned: signed and notarized builds, automatic updates, package-manager distribution (Homebrew,
  winget) and a tray icon to keep AstraTerm running with the window closed. Passkey sign-in is not available inside
  the macOS and Linux app windows (the desktop app signs you in automatically; use a browser for passkeys).
- **Platforms.** FreeBSD and 32-bit ARM builds are compiled and packaged but not yet tested on real systems.
- **Optional helpers.** RDP recordings and admin shadowing need a guacd sidecar; Kubernetes sessions need `kubectl`.
- **Planned:** a container image.

Ideas and bug reports are welcome in the issue tracker.

## Contributing

Contributions are welcome. Read [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) first (structure, conventions, tests),
run `make check` before opening a pull request, and follow the [Code of Conduct](CODE_OF_CONDUCT.md). Security issues
go through [SECURITY.md](SECURITY.md), not public issues.

## License

AstraTerm is licensed under the [Apache License 2.0](LICENSE).

Third-party components keep their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Acknowledgements

AstraTerm stands on the shoulders of excellent open-source projects, among them
[xterm.js](https://xtermjs.org/), [Monaco Editor](https://microsoft.github.io/monaco-editor/),
[noVNC](https://novnc.com/), [IronRDP](https://github.com/Devolutions/IronRDP),
[Apache Guacamole](https://guacamole.apache.org/), [dockview](https://dockview.dev/), [React](https://react.dev/),
[Vite](https://vite.dev/), [Tailwind CSS](https://tailwindcss.com/), [Radix UI](https://www.radix-ui.com/),
[Echo](https://echo.labstack.com/), [golang.org/x/crypto/ssh](https://pkg.go.dev/golang.org/x/crypto/ssh),
[pkg/sftp](https://github.com/pkg/sftp), [modernc.org/sqlite](https://gitlab.com/cznic/sqlite),
[goja](https://github.com/dop251/goja) and many more. The complete list with licenses is in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
