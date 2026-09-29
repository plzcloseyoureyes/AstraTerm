# Changelog

All notable changes to Termstead are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the version is below 1.0.0, minor releases may
contain breaking changes; they are called out under **Changed** or **Removed**.

## [Unreleased]

First public release candidate: an organized, self-hosted remote-management workspace in a single binary.

### Added

- **Workspace and organization.** Session manager with folders, tags, favorites, recents, fuzzy search and drag &
  drop; reusable identities; shared (admin-managed) connections; tabbed workspace with splits, 1/2/4 layouts,
  floating groups, pop-out windows and a restored layout per user; command palette with rebindable shortcuts; quick
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
- **Distribution.** One CGO-free executable per platform (macOS, Linux and Windows on amd64/arm64, FreeBSD amd64) with the
  web UI embedded and precompressed; `termstead version` prints version, commit and build date; reproducible release
  archives with SHA256SUMS, SBOMs and build-provenance attestations; third-party notices.

[Unreleased]: https://github.com/OWNER/termstead/commits/main
