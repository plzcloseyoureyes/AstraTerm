# AstraTerm

**An organized remote-management workspace.** SSH, RDP, VNC, files, tunnels, monitoring and network tools in one
self-hosted app. Run it as a desktop app, or as a server for your team.

[![CI](https://github.com/plzcloseyoureyes/astraterm/actions/workflows/ci.yml/badge.svg)](https://github.com/plzcloseyoureyes/astraterm/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/plzcloseyoureyes/astraterm?sort=semver)](https://github.com/plzcloseyoureyes/astraterm/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

- **One workspace.** Every connection type in title-bar tabs, each with its own split layout.
- **Organized.** Folders, tags, favorites, fuzzy search, reusable identities. Import from MobaXterm, PuTTY,
  `~/.ssh/config` and more.
- **Sessions survive.** Reload, close the window or drop the network: the terminal re-attaches and replays what you
  missed.
- **Secure by default.** Encrypted credential vault, loopback-only desktop mode, TOTP/passkeys/SSO in server mode,
  audit log.
- **One binary.** Pure Go with the UI embedded. macOS, Windows, Linux and FreeBSD.

## Install

Download from [Releases](https://github.com/plzcloseyoureyes/astraterm/releases):

| Platform | Desktop app | Server / CLI |
|---|---|---|
| macOS | `.dmg` (Apple silicon or Intel) | `astraterm_*_darwin_*.tar.gz` |
| Windows | `_x64-setup.exe` or `.msi` | `astraterm_*_windows_*.zip` |
| Linux | `.AppImage`, `.deb`, `.rpm` (x64, ARM64) | `astraterm_*_linux_*.tar.gz` |
| FreeBSD | | `astraterm_*_freebsd_amd64.tar.gz` |

Builds are not signed yet, so the first start needs one confirmation:

- **macOS:** System Settings → Privacy & Security → **Open Anyway**.
- **Windows:** SmartScreen → **More info → Run anyway**.

The first start shows a short setup wizard. The desktop app also serves AstraTerm at `http://127.0.0.1:7822`, so a
browser tab works too.

Verify a download (optional):

```sh
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify <file> --repo plzcloseyoureyes/astraterm
```

## Server mode

```sh
./astraterm --mode server --listen 0.0.0.0:7822 --tls-cert cert.pem --tls-key key.pem   # or --tls-self-signed
```

Open the one-time `https://…/?setup=<token>` link from the banner to create the admin. Flags, systemd and reverse
proxies: [docs/SERVER.md](docs/SERVER.md).

| | Desktop (default) | Server |
|---|---|---|
| Users | just you | many accounts |
| Listens on | `127.0.0.1` | any address (TLS required) |
| Sign-in | automatic | password, TOTP, passkeys, OIDC, API tokens |
| Local shells, serial, host files | yes | admins only |

**Data** lives in `~/Library/Application Support/astraterm` (macOS), `~/.config/astraterm` (Linux/FreeBSD) or
`%AppData%\astraterm` (Windows). Use `--data-dir DIR`, or `--portable` to keep it beside the executable. Forgot your
password? `astraterm reset-password <user>`.

## Features

- **Connections:** SSH (jump hosts, proxies, agent and X11 forwarding, certificates), telnet, rlogin, raw TCP/UDP,
  serial, local shells, WSL, Docker, Kubernetes, Mosh, WinRM, IPMI.
- **Terminal:** WebGL rendering, search, inline images, shell integration, 37 color schemes, MultiExec, ZMODEM/trzsz.
- **Files:** SFTP, FTP/FTPS, S3, WebDAV, SMB and local; dual pane, resumable transfers, built-in editor with diff.
- **Remote desktops:** RDP (IronRDP or guacd), VNC (noVNC), web and Xpra sessions.
- **Network:** port forwarding, ping, traceroute/mtr, port and network scan, DNS, whois, TLS, SNMP, ssh-audit,
  embedded HTTP/FTP/SFTP/TFTP/syslog servers.
- **Utilities:** password, hash, subnet, chmod and UUID tools; encode/decode, JWT, JSON and timestamp converters.
- **Automation:** agentless monitoring, snippets, macros, triggers, scripts, schedules, optional AI assistant.
- **Teams:** shared connections, recordings, session sharing, audit log, backup and restore.

## Build

Needs Go 1.26+, Node.js 22+ and `make`.

```sh
make build     # bin/astraterm
make desktop   # desktop app (needs Rust)
make check     # all tests and linters
```

Development: `make dev-backend` and `make dev-web`, then open `http://localhost:5173`.

More: [BUILDING](docs/BUILDING.md) · [DESKTOP](docs/DESKTOP.md) · [RELEASING](docs/RELEASING.md) ·
[CONTRIBUTING](docs/CONTRIBUTING.md) · [SPEC](docs/SPEC.md) · [SECURITY](SECURITY.md) · [CHANGELOG](CHANGELOG.md)

## License

[Apache 2.0](LICENSE). Third-party licenses: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
