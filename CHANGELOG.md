# Changelog

All notable changes to AstraTerm are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the version is below 1.0.0, minor releases may
contain breaking changes; they are called out under **Changed** or **Removed**.

## [Unreleased]

### Changed

- **Desktop app: no account setup.** A fresh install opens straight into AstraTerm; the local account is created
  automatically, without a password. Set one in Settings → Security to sign in from a browser or to use the lock
  screen. Server mode keeps its users and one-time setup link.
- **Local shell tabs** show this computer's files in the files side panel, following the shell's folder, like SSH
  sessions show SFTP.

## [0.1.12] - 2026-10-02

### Security

- Updated DOMPurify (bundled by the editor) to 3.4.16, which fixes a low-severity issue. AstraTerm does not use the
  affected option.

## [0.1.11] - 2026-10-02

### Added

- **Download location** (desktop app; Settings → General → Downloads): choose the download folder, and whether to be
  asked where to save each file (on by default).
- **Selection rectangle** in file lists: drag from empty space, or from the blank part of a row, to select the rows it
  covers (Ctrl / ⌘ / Shift adds to the selection). Dragging a file's icon or name still moves or copies it.

### Changed

- **Tab titles** show the session name by default instead of the shell's title (Settings → Terminal → Tab title).
- **Window opacity** goes down to 10% on Windows too.

### Fixed

- **Listening ports** (Tools) reports an error after 15 seconds instead of waiting forever when the system's lookup
  hangs.

## [0.1.10] - 2026-10-02

### Fixed

- **Loading bar on tabs:** the progress bar of a tab (opening or reloading a file over SFTP or locally, terminal
  progress) was hidden behind the active tab's accent line in the title bar. It now takes the accent line's place
  while the tab is working, and a bar of unknown length slides instead of standing still (still with reduced motion).

## [0.1.9] - 2026-09-30

### Added

- **Restore history** (on by default; Settings → Terminal → Conveniences): after a restart, reopened terminal tabs
  start a new session that shows their previous output, marked "History restored". Closing a tab deletes its saved
  history.

## [0.1.8] - 2026-09-30

### Changed

- **Window opacity on macOS** is clearly see-through now, the terminal included: a clearer blur (the HUD material,
  which also stays active when the window is in the background), a lighter tint on the content area, and a 10% minimum
  (Windows keeps 30%).

## [0.1.7] - 2026-09-30

### Fixed

- **Windows title bar:** the system title bar came back on top of AstraTerm's own (duplicate window buttons) when the
  window had been used with an earlier version: restoring the window's size and position also restored its frame.

## [0.1.6] - 2026-09-30

### Added

- **Clean title bar on Windows:** no system title bar; the tab row moves the window (double-click maximizes) and ends
  with AstraTerm's own minimize, maximize and close buttons.

## [0.1.5] - 2026-09-30

### Fixed

- **Window opacity on Windows** now shows what is behind the window, blurred (Acrylic), instead of an almost solid
  Mica tint. The blur is only on while the window is see-through, so moving and resizing stay smooth at 100%.

## [0.1.4] - 2026-09-30

### Added

- **Window opacity** (desktop app on macOS and Windows): Settings → Appearance. Below 100%, the desktop shows through,
  blurred (macOS vibrancy, Windows 11 Mica).
- **Clean title bar on macOS:** the system title bar is gone; the window buttons sit in AstraTerm's tab row, which
  moves the window (double-click zooms).
- **Ctrl+Shift+W** closes the current tab on Windows and Linux.

### Fixed

- **FTP:** connection errors that happened to contain "530" (for example a port like 5300) were reported as a failed
  login.

### Changed

- Code cleanup: modern Go 1.26 idioms, removed unused code and 9 unused npm packages.

## [0.1.3] - 2026-09-30

### Added

- **Utilities are back** in Tools: password generator, subnet, hash, chmod and UUID tools, encode/decode, JWT decoder,
  JSON formatter and timestamp converter. They run entirely in the browser.

### Fixed

- The Tools tab title follows the selected tool.

### Changed

- Shorter README; server flags and the systemd unit moved to `docs/SERVER.md`.

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

[Unreleased]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.12...HEAD
[0.1.12]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.11...v0.1.12
[0.1.11]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.10...v0.1.11
[0.1.10]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.9...v0.1.10
[0.1.9]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/plzcloseyoureyes/AstraTerm/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/plzcloseyoureyes/AstraTerm/releases/tag/v0.1.1
