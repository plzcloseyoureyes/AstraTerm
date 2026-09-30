# Changelog

All notable changes to AstraTerm are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the version is below 1.0.0, minor releases may
contain breaking changes; they are called out under **Changed** or **Removed**.

## [Unreleased]

## [0.1.2] - 2026-09-30

### Fixed

- **Desktop app on Windows:** dragging tabs (to reorder them or split them into another tab) works again, and files
  can be dropped onto the file browser and terminals. Tauri's native drag-and-drop handler swallowed the page's own
  drag events in WebView2.

## [0.1.1] - 2026-09-30

First public release: an organized, self-hosted remote-management workspace, as a server and a desktop app.

v0.1.0 was tagged but never published: its Windows desktop app sent the remote-monitoring scripts with Windows line
endings, which broke monitoring of Linux, macOS and BSD hosts. That is fixed here.

### Added

- **Workspace and organization.** Session manager with folders, tags, favorites, recents, fuzzy search and drag &
  drop; reusable identities; shared (admin-managed) connections; tabs in the title bar, each with its own split
  layout (panes can move between tabs), floating panes, pop-out windows and a restored layout per user; command palette with rebindable shortcuts; quick
  connect (`ssh -p 2222 -J bastion user@host`, `telnet host 23`, URLs); dark, light and system themes.
- **Connections and sessions.** SSH (pooled transports, jump-host chains, SOCKS/HTTP proxies, ProxyCommand, port
  knocking, keys and certificates, keyboard-interactive and 2FA prompts, known hosts with CA/revocation markers, agent
  and X11 forwarding), telnet (+TLS), rlogin/rsh, raw TCP/TLS/UDP, serial, local shells (PTY, ConPTY, WSL), Docker and
  Kubernetes exec/logs, Mosh, WinRM, IPMI Serial-over-LAN. Sessions live in the server and survive reloads, closed
  tabs and network drops, with scrollback replay.
- **Terminal.** xterm.js with WebGL, search, links, inline images, OSC 52 clipboard, shell integration, paste safety,
  37 colour schemes and an editor, MultiExec with a dangerous-command guard, activity/silence monitors, instant replay,
  ZMODEM and trzsz transfers, drag-and-drop upload.
- **Files.** SFTP (SCP/shell fallback, sudo), FTP/FTPS, S3, WebDAV, SMB and local files; dual-pane commander,
  resumable chunked uploads, a transfer queue that survives restarts, archives, checksums, search, folder compare,
  previews and "follow terminal folder"; Monaco editor (bundled, no CDN) with diff, find in files, Vim mode, encodings
  and conflict detection.
- **Remote desktops.** RDP through IronRDP (WebAssembly, default) or an optional guacd sidecar (recordings, admin
  shadowing); VNC through noVNC with server-side security termination (VeNCrypt, ARD) and listening/repeater mode;
  web sessions through a proxy and Xpra X11 applications.
- **Network and tunnels.** Port-forwarding manager (local, remote, dynamic SOCKS/HTTP, on-demand); network tools
  (ping, traceroute/mtr, port and network scan, DNS, whois, Wake-on-LAN, HTTP and TLS checks, SNMP, ssh-audit,
  throughput); embedded HTTP, FTP, SFTP, TFTP, telnet and syslog servers.
- **Monitoring.** Remote CPU, memory, disk and network, processes, services, listening ports, disk usage and a log
  follower, without agents on the target.
- **Automation.** Snippets, macros, button bars, triggers, logon actions, sandboxed JavaScript scripts, batch runs and
  schedules; optional AI assistant (Anthropic or OpenAI-compatible) with secret redaction.
- **Keys and credentials.** SSH key manager (generate, import, convert, PuTTY PPK, certificates, install to hosts),
  built-in SSH agent, write-only secrets sealed with XChaCha20-Poly1305 and an optional master password (argon2id).
- **Security and teams.** Desktop mode (loopback, one-time launch link) and multi-user server mode (TLS, passwords,
  TOTP, passkeys, OIDC single sign-on, API tokens, lockout policy); outbound network policy for non-admin users;
  audit log; recordings (asciicast, logs, RDP) with player, search and retention; session sharing links.
- **Import and backup.** Import from MobaXterm, PuTTY, `~/.ssh/config` (live sync), Termius, mRemoteNG, Remmina,
  FileZilla, WinSCP, SecureCRT, CSV and JSON; encrypted exports; admin backup and restore.
- **Desktop app.** AstraTerm in a native window (Tauri, the system web engine) with the unchanged server running
  inside as a sidecar: installers for macOS (`.dmg`), Windows (setup `.exe`, `.msi`) and Linux (`.deb`, `.rpm`,
  `.AppImage`), single instance, remembered window size, downloads to the Downloads folder, links opened in the
  browser. It keeps listening on its port, moves to a free port when 7822 is taken, and reports startup problems in a
  dialog.
- **Distribution.** CGO-free executables for macOS, Linux and Windows on amd64/arm64, Linux on 32-bit ARMv7 and
  FreeBSD amd64, with the web UI embedded and precompressed; `astraterm version` prints version, commit and build date; reproducible
  release archives with SHA256SUMS, SBOMs and build-provenance attestations; third-party notices.

[Unreleased]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/plzcloseyoureyes/AstraTerm/releases/tag/v0.1.1
