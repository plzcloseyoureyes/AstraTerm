# NexTerm: Consolidated Feature Matrix, Stack and Protocol Strategy

_Synthesized on 2026-09-27 from five research reports: the MobaXterm inventory (baseline MobaXterm v26.5, released 2026-09-11), modern desktop clients, web-served clients and bastions, the verified stack, and the SSH/SFTP deep-dive. All versions were re-checked against proxy.golang.org and registry.npmjs.org on 2026-09-27. Duplicate entries across reports were merged, and each merged row keeps the richest description._

---

## 0. Reading guide and architectural decisions

**Priority**
- **must**: MobaXterm parity or a table-stakes feature for v1.
- **should**: a power user expects it.
- **nice**: a differentiator, or something for later.
- **·srv**: the priority applies only to multi-user server mode. These rows are hidden in desktop mode.

**Feasibility**
- **frontend-only**: runs entirely in the browser.
- **backend-go**: runs entirely in the Go backend.
- **backend-go+frontend**: split between the two.
- **external-dependency**: needs a component that is not in the single binary (guacd, Xpra, a host tool, an external CLI).
- **infeasible**: not achievable as specified. The row gives the reason and the closest alternative.

**"Local" or "host"** always means the machine running the NexTerm binary, not the browser's machine. This matters for local shells, serial ports, tunnel listeners and embedded servers.

**Decisions that many rows depend on**
1. **Server-owned sessions.** A Go `SessionManager` owns every connection. Each session has a ring buffer with byte offsets, a headless VT emulator and a subscriber fan-out. This one design gives reload persistence, popouts, sharing, admin shadowing, logging, triggers and macros that keep running with no browser attached.
2. **Transport.** Each browser window has one multiplexed binary WebSocket that carries terminals, control messages and events. Each graphical stream gets its own WebSocket, because noVNC, IronRDP and Guacamole open their own sockets. File transfers use plain HTTP (PUT with offset, GET with Range).
3. **One `Dialer` abstraction** for every protocol: proxy, then SSH jump chain, then target. It includes an SSRF and destination guard.
4. **One `VFS` interface** (local, SFTP, SCP, FTP, S3, WebDAV, SMB, container) behind a single React file-browser component.
5. **One prompt broker.** Host-key, keyboard-interactive, passphrase, PIN and sudo prompts go to the UI over the WebSocket. The SSH handshake deadline is paused while a prompt is open.
6. **One frontend command registry** feeds menus, shortcuts, the command palette, the button bar and macros.
7. **`CGO_ENABLED=0` by default.** Features that need cgo, or that are heavy, sit behind build tags (CORE-7).
8. **Server-side policy enforcement.** Hiding something in the UI never counts as enforcement.

---

## 1. Feature Matrix

### 1.1 Core runtime and deployment (CORE)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| CORE-1 | Server-owned session manager | Every live connection (terminal, tunnel, transfer, graphical relay) belongs to the Go process, not to a WebSocket. States are connecting, connected, lost, reconnecting and closed. A session has N read-write or read-only subscribers and a detach timeout (default 30 min, up to forever). | must | backend-go | `map[id]*Session` with a 4–16 MiB ring buffer and monotonically increasing byte offsets, plus a subscriber fan-out and per-session context cancellation. |
| CORE-2 | Reattach after reload, tab close, sleep or network change | Reload, tab close, laptop sleep or opening on another device reattaches to running sessions with screen, scrollback and terminal modes intact. A "Detached sessions" list is available. Only an explicit Close ends a session. | must | backend-go+frontend | Client sends `{attach,id,fromOffset}`. Server sends the delta, or `reset` plus a snapshot from a headless VT (`charmbracelet/x/vt`, pinned pseudo-version). Client stores layout and IDs in localStorage and an `@xterm/addon-serialize` snapshot in IndexedDB for instant paint. VNC and RDP reconnect because the server keeps the desktop. guacd rejoins via `select $<conn-id>`. |
| CORE-3 | Streaming transport with flow control | Typing stays low-latency. A `cat` of a huge file never freezes the tab. Ctrl+C stays responsive. Resume is lossless. | must | backend-go+frontend | Binary frames via coder/websocket and `term.write(Uint8Array)`. Client acks every 64 KiB after the write callback. Server stops reading the channel above 1 MiB unacked and resumes below 256 KiB, so the SSH window throttles the remote. Output is coalesced to ≤8 ms or 32 KiB. See §3.1. |
| CORE-4 | Remote-side persistence (tmux/screen wrap) | An optional per-session mode survives NexTerm restarts. Includes a remote tmux session browser. | should | backend-go+frontend | `tmux new -A -s nexterm-<id>`, falling back to `screen -xRR -S nexterm-<id>`. List sessions with `tmux ls -F`. |
| CORE-5 | Desktop mode vs server mode | **Desktop mode:** 127.0.0.1, random port, one-time launch token, no login, local shell on. **Server mode:** accounts plus TLS required. Local shell, ProxyCommand, host agent and local X11 are admin-only or off. | must | backend-go+frontend | `--mode=desktop` or `--mode=server`. The launch token in the URL is swapped for an HttpOnly SameSite=Strict cookie (Jupyter-style). `pkg/browser` opens the page. |
| CORE-6 | Single self-contained binary | One executable per OS and architecture (windows/linux/darwin × amd64/arm64) serves the UI, REST and WebSockets, with no runtime dependencies. | must | backend-go | `//go:embed all:dist`. Brotli/gzip precompressed with vite-plugin-compression2 and served by `vearutop/statigz`. SPA fallback to index.html; `Cache-Control: immutable` on `/assets/*`. goreleaser matrix. Go 1.26+ requires macOS 13+ and Windows 10+. |
| CORE-7 | Build-tag feature tiers | The default build is static. Optional extras are compiled in by tag. | must | backend-go | Tags: `pcap`, `pkcs11`, `piv`, `fido2`, `tray` (cgo on darwin), `serialusb` (darwin USB descriptors), `webview`, `ghotkey`. The `lite` tag drops AWS, Docker and client-go (about 12 MB). Build the macOS "full" artifact with cgo on a macOS runner. |
| CORE-8 | Portable mode and data location | Portable mode uses `nexterm-data/` beside the exe. Installed mode uses the per-user config dir. `--data-dir` overrides both. Paths are stored relative; the keychain is disabled in portable mode. | must | backend-go | Resolution order: flag, then marker beside `os.Executable()`, then `os.UserConfigDir()`. |
| CORE-9 | Launcher, single instance, app window | Launching opens the UI in the default browser, or in a chromeless Chrome/Edge `--app=` window. A second launch forwards "open tab" to the running instance (`-newtab`). `-hideterm` equivalent. | should | backend-go+frontend | Lock file plus a local control socket or named pipe. Detect Chromium for `--app`. |
| CORE-10 | Tray icon and optional native window | Tray menu: open UI, recent sessions, tunnels, servers, quit. Optional native webview window. | nice | backend-go | `fyne.io/systray` v1.12.2 (cgo on darwin, so under the `tray` tag). `webview_go` (cgo, `webview` tag). |
| CORE-11 | OS service and headless server | `nexterm service install` for systemd, launchd or a Windows service. Flags `--listen` and `--no-browser`. | should | backend-go | `kardianos/service` v1.3.0 (cgo-free). |
| CORE-12 | TLS, ACME and reverse-proxy awareness | Self-signed certificate on first run, Let's Encrypt, or a custom cert, with HSTS. Correct client IP behind trusted proxies. Sub-path deployment. HTTPS is required off-localhost because clipboard, WebAuthn, service workers, Web Serial and noVNC all need a secure context. | must | backend-go+frontend | `x/crypto/acme/autocert` (certmagic only if DNS-01 is needed). Trusted-proxy CIDRs gate X-Forwarded-For, X-Real-IP and CF-Connecting-IP. Vite `base` plus router basename for sub-paths. |
| CORE-13 | Installable PWA with protocol and file handlers | Standalone window, where more shortcuts reach the page. App shortcuts, badge counts, `ssh://` and `sftp://` links, and file handlers for .pem/.pub/.cast/.mxtsessions. | should | frontend-only | vite-plugin-pwa with network-first index.html (avoids stale UIs). `navigator.registerProtocolHandler('ssh','/open?uri=%s')`. Manifest `protocol_handlers` and `file_handlers` (Chromium). |
| CORE-14 | Self-update | Checks releases, shows the changelog, verifies the signature, swaps the binary atomically, and warns before restarting over live sessions. | nice | backend-go | `creativeprojects/go-selfupdate` v1.6.0 (GitHub releases, checksum and signature validation, Windows rename trick). |
| CORE-15 | Observability | `/healthz` and `/readyz`. Prometheus `/metrics` (sessions per protocol, bytes, auth failures, guacd reachability). JSON logs. pprof for admins. | should | backend-go | `prometheus/client_golang`, `log/slog`, `net/http/pprof` behind admin auth. |
| CORE-16 | guacd sidecar management | Auto-detects guacd on :4822. In desktop mode, offers to pull and start `guacamole/guacd:1.6.0` through local Docker. Features that need guacd are greyed out when it is missing. | should | external-dependency | Docker Engine API (`moby/moby/client`) runs the container bound to 127.0.0.1:4822; health probe. |
| CORE-17 | Clustering and HA | Several nodes share Postgres. A cluster-wide session registry routes WebSockets to the node that owns the session. | nice·srv | external-dependency | `jackc/pgx/v5`, NATS (`nats-server/v2` can be embedded) for pub/sub, node-to-node WS proxying. |
| CORE-18 | Fully offline operation | Fonts, icons, docs, editor language packs and WASM are all embedded, so nothing is fetched from a CDN at runtime. | must | frontend-only | Self-host every asset. Never use Monaco's CDN loader. |

### 1.2 Sessions and protocols (PROTO)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| PROTO-1 | SSH terminal session | Tabbed SSH2 shell: host, port 22, optional user (prompts "login as:" when empty; `[credential]` syntax references the vault), PTY, live resize, exit status on close. The one connection also carries the SSH-browser, monitoring, tunnels and X11. | must | backend-go+frontend | `golang.org/x/crypto/ssh` v0.57.0 and xterm.js 6. See §3.2. |
| PROTO-2 | SSH advanced session settings | Options: X11 forwarding; compression (shown as unsupported, see SSH-26); remote environment (shell, or LXDE/GNOME/KDE/XFCE/MATE/Openbox/Fluxbox/IceWM/custom); execute command(s) with "do not exit after command"; SSH-browser type (SFTP, SCP enhanced, SCP normal, none); follow SSH path; private key; adapt locales; macro at session start; terminal type (includes `linux`). | must | backend-go+frontend | Maps to ClientConfig and session calls. "Stay connected" becomes `cmd; exec $SHELL -l`. Forms use react-hook-form and zod. |
| PROTO-3 | Remote command / exec sessions | Modes: shell; command with PTY (`ssh -t`); command without PTY, with stdout and stderr split and stderr in red. Honors RemoteCommand and RequestTTY. "Keep tab open after exit" shows the exit-code banner. Login-shell wrapping. | must | backend-go+frontend | `session.Start` with `StdoutPipe`/`StderrPipe` drained concurrently. `$SHELL -lc '<posix-quoted>'`. |
| PROTO-4 | Channel lifecycle, signals and BREAK | Handles exit code, signal and missing-exit (network gear). Drains both streams. Grace timeout for devices that never close. Sends INT/TERM/KILL/HUP and RFC 4335 BREAK for console servers. | must | backend-go+frontend | `*ssh.ExitError` and `*ssh.ExitMissingError`; wait for stdout EOF before reporting "exited". `session.Signal`. `SendRequest("break",true,{500})`. Under a PTY, 0x03 is the reliable interrupt. |
| PROTO-5 | Subsystem sessions (NETCONF, vendor) | Connect to non-SFTP subsystems (NETCONF on port 830) in an XML-aware view with send and receive panes. | nice | backend-go+frontend | `RequestSubsystem("netconf")`; `]]>]]>` framing (1.0) or chunked framing (1.1); XML pretty-printer. |
| PROTO-6 | Telnet | Host and port 23. Auto-login (answers `login:` and password prompts from the vault). Return sends CR, CR-LF or ^M. Forced passive negotiation, TCP keepalive, TTYPE/NAWS, local echo and line mode, optional telnets (992). Can route through an SSH gateway or proxy. Admins can disable it. | must | backend-go+frontend | Hand-rolled RFC 854/855 IAC state machine (~300 LOC). See §3.4. |
| PROTO-7 | Rlogin | Port 513. Terminal type and speed sent in the handshake. Window-size updates. `.rhosts` trust login with a fallback to a password prompt. | nice | backend-go | RFC 1282, see §3.5. Privileged source port needs root or CAP_NET_BIND_SERVICE. Behind a feature flag. |
| PROTO-8 | Rsh / RCP | Runs a single remote command on port 514 and shows its output; RCP copies files. Off by default. | nice | backend-go | Small BSD rsh client with the stderr-port handshake. Admin policy can block it. |
| PROTO-9 | Raw TCP / TLS / UDP socket | Byte stream to host:port. Options: local echo, CRLF translation, hex dump, TLS with an "insecure" toggle. Can go through a jump host. | should | backend-go+frontend | `net.Dialer`, `tls.Client` or UDP. In server mode, per-user destination ACLs because this is an SSRF primitive. |
| PROTO-10 | Serial console (host ports) | Auto-detected COM, /dev/tty* and cu.* ports with USB VID/PID labels and hotplug. Baud presets 110–230400 plus custom (460800, 921600, 3000000). Data bits 5–8, parity N/O/E/M/S, stop bits 1/1.5/2, flow none/XON-XOFF/RTS-CTS/DSR-DTR. DTR/RTS toggles, BREAK, local echo, CR/LF translation, macro at start, reconnect on replug. | must | backend-go+frontend | `go.bug.st/serial` v1.8.0. USB descriptors on linux, windows and darwin with cgo; otherwise `GetPortsList`. RTS/CTS may need termios CRTSCTS or a Windows DCB via x/sys. Linux needs the `dialout` group. |
| PROTO-11 | Serial via Web Serial (viewer's machine) | When NexTerm runs remotely, a device plugged into the viewer's laptop opens in the browser and pipes into xterm. The backend only gets logs and recordings. | should | frontend-only | `navigator.serial.requestPort()`. Chromium, and Firefox 151+ desktop (May 2026). Not Safari. Secure context required. |
| PROTO-12 | Hex view and hex send (serial / raw) | Dual hex and ASCII dump with timestamps, TX/RX coloring and byte counters. Send hex strings such as `01 03 00 00 00 0A C5 CD`. | should | backend-go+frontend | Backend tees raw bytes to a hex stream. Virtualized grid (TanStack Virtual). Hex parser in the frontend. |
| PROTO-13 | Mosh | Fields: host, user, SSH port, key. Bootstraps mosh-server over SSH, then switches to UDP with roaming and survives IP changes and sleep. | should | backend-go | `unixshells/mosh-go` v0.5.2 (pure Go SSP, AES-OCB). See §3.8. Roaming helps only the backend-to-server leg. |
| PROTO-14 | Local shell | bash/zsh/fish/sh/pwsh/PowerShell/cmd/Git Bash/MSYS2/Cygwin on the host. Settings: default shell, start directory, env vars, command at start, login-shell toggle. | must | backend-go+frontend | `charmbracelet/x/xpty` v0.1.4 (Unix PTY or ConPTY). Shells discovered from /etc/shells, $SHELL, PATH and the registry. "Run as admin" on Windows means relaunching an elevated helper through UAC on the host. Admin-only in server mode. |
| PROTO-15 | WSL session | Pick distro, user, start dir, command and desktop environment; DISPLAY set automatically for WSLg. Windows backends only. | should | backend-go | Distros from the `HKCU\…\Lxss` registry key (not UTF-16 `wsl -l -v`). Spawn `wsl.exe -d <d> --cd <dir>` under ConPTY. Files via `\\wsl.localhost\<d>\`. |
| PROTO-16 | RDP (core) | Host, port 3389, user (blank shows the Windows logon screen), password from vault, NLA/CredSSP and security level, console/admin session. Resolution fit, fixed, fullscreen or dynamic; color depth; clipboard; shortcut forwarding; Ctrl+Alt+Del; auto-reconnect; SSH-gateway tunneling. | must | backend-go+frontend | Default path: IronRDP WASM (`@devolutions/iron-remote-desktop` 0.11.0 + `-rdp` 0.7.0, lazy-loaded) with a Go RDCleanPath relay. Opt-in guacd 1.6.0 path for full fidelity. See §3.10–3.11. |
| PROTO-17 | VNC | Host, port 5900 or display :N. Encodings Raw, CopyRect, RRE, Hextile, Zlib, Tight, TightPNG, ZRLE, JPEG, H.264. Auth None, VNC, Tight, VeNCrypt, MSLogonII, ARD, RA2ne, UnixLogon, Plain. View-only, scaling, auto-resize, clipboard, special keys, quality and compression, IPv6, SSH gateway. | must | backend-go+frontend | noVNC 1.7.0 plus a Go WS-to-TCP relay. Go terminates RFB security (VNCAuth, VeNCrypt with real TLS) and presents None to noVNC, so the password never reaches the browser. TRLE and SASL are not supported by noVNC; servers fall back to other encodings. See §3.12. |
| PROTO-18 | XDMCP | Broadcast or query a host for a full Unix desktop. Options: screen number, NumLock off (AIX/Solaris/HP-UX), clipboard mode. | nice | external-dependency | Linux/macOS backend spawns `Xephyr :N -query host` or `Xvfb`, then shows it through x11vnc and noVNC, or Xpra HTML5. Not available on Windows backends. |
| PROTO-19 | X11 forwarding to a local X server (desktop mode) | Remote GUI apps appear on the host's X server (XQuartz, VcXsrv/X410/Xming, Xorg/XWayland, WSLg). Trusted or untrusted mode, display number, works through gateways, guidance for X after su/sudo. | should | backend-go | Send `x11-req` with a fake MIT-MAGIC-COOKIE-1. Register `HandleChannelOpen("x11")` once per client. Parse the X11 setup packet and swap in the real cookie from `xauth list`. Only useful when the viewer sits at the backend machine. |
| PROTO-20 | Remote GUI apps in the browser (Xpra) | "Open GUI app" runs X11 apps inside a NexTerm tab with no local X server: seamless per-app windows, clipboard, audio, file transfer. | should | external-dependency | Xpra is installed on the remote host. NexTerm starts it over SSH bound to loopback, reaches it through a direct-tcpip channel, and reverse-proxies xpra's HTML5 client (MPL-2.0). See §3.13. |
| PROTO-21 | Embedded X server (MobaXterm Xorg) | Multiwindow, rootless, windowed or fullscreen X server with GLX, RandR, keyboard layouts and font server. | should | **infeasible** | **Reason:** there is no usable X11 server in a browser or in pure Go. **Alternative:** PROTO-20 (Xpra), Xvfb+x11vnc+noVNC for full desktops, or PROTO-19 with a local X server in desktop mode, auto-launched when installed. |
| PROTO-22 | Remote desktop environment over SSH | The "Remote environment" option starts LXDE/GNOME/KDE/XFCE/… in its own window; common on a Raspberry Pi. | nice | external-dependency | `xpra start-desktop` over SSH, or Xvnc/TigerVNC with the VNC path, or xrdp with the RDP path. |
| PROTO-23 | FTP / FTPS | File-browser tab: port 21, user or anonymous, explicit AUTH TLS or implicit 990, encrypted data channel, ASCII mode, UTF-8 auto-detect, proxy. | should | backend-go+frontend | `jlaffaye/ftp` v0.2.4. It is passive-only, so active mode (PORT/EPRT) must be hand-rolled. See §3.14. |
| PROTO-24 | SFTP standalone session | Full-tab SFTP file manager: host, port, user, key, 2FA, jump host, ASCII mode, preserve dates, transfer queue. | must | backend-go+frontend | `pkg/sftp` v1.13.11 over the shared `ssh.Client`, using the same VFS and React browser as the SSH-browser. |
| PROTO-25 | SCP mode and shell-only fallback | The "SCP enhanced/normal speed" browser engines work where the SFTP subsystem is missing (Dropbear, CyberArk PSM, restricted shells), with automatic fallback. The last resort is `ls`/`cat`/`base64` over the shell. | should | backend-go+frontend | SCP source/sink over exec (`scp -f`/`scp -t`, with -r/-p implemented in-house). `bramvdbogaerde/go-scp` v1.6.1 for single files. See §3.3. |
| PROTO-26 | S3 / S3-compatible | Key, secret, region or SSO profile, custom endpoint (MinIO, R2, Wasabi, B2). Buckets and prefixes as folders; upload, download, delete; presigned URL. | should | backend-go+frontend | aws-sdk-go-v2 `service/s3` v1.113.4 + `feature/s3/transfermanager` v0.4.10. Checksums set to `WhenRequired` for compatible stores. See §3.15. |
| PROTO-27 | WebDAV and SMB backends | Same file-manager UI for WebDAV and SMB shares. | nice | backend-go | `studio-b12/gowebdav` v0.13.0, `hirochachacha/go-smb2` v1.1.0 (stale but works). |
| PROTO-28 | Web / HTTP session tab | Saved URL (router or iDRAC UI, Grafana) opens in a tab, optionally through an SSH tunnel. | nice | backend-go+frontend | Iframe when the site allows framing. Otherwise a reverse proxy (`httputil.ReverseProxy`, `Transport.DialContext` = tunnel) strips X-Frame-Options and frame-ancestors, rewrites cookies, and serves under `<id>.localhost`. Falls back to a new window. |
| PROTO-29 | File session | A host file or folder opens directly as an editor, viewer or browser tab. | nice | backend-go+frontend | Local VFS; admin policy scopes roots. |
| PROTO-30 | Docker / Podman containers | List containers; exec with TTY; logs (follow); stats; file browsing inside containers. Works locally and on remote engines over SSH. | should | backend-go+frontend | `moby/moby/client` v0.6.0. Remote engines dial `sshClient.Dial("unix","/var/run/docker.sock")`. See §3.16. |
| PROTO-31 | Kubernetes exec / attach / logs | Pick context, namespace, pod and container; exec or attach with resize; follow logs; port-forward. | should | backend-go+frontend | `k8s.io/client-go` v0.37.1, using remotecommand's fallback executor (WebSocket v5, then SPDY). No typed clientset. See §3.17. |
| PROTO-32 | AWS SSM Session Manager | Shell or port-forward to instances with no open SSH port. Target by tag, IP or DNS; optional EC2 Instance Connect key push. | nice | backend-go+frontend | `mmmorris1975/ssm-session-client` v0.500.1 + aws-sdk-go-v2 `config` (SSO, assume-role). See §3.18. |
| PROTO-33 | Other cloud access (GCP IAP, Azure Bastion, Cloudflare Access) | Connection templates that reach hosts through cloud identity-aware proxies. | nice | external-dependency | ProxyCommand templates (`gcloud compute start-iap-tunnel %h %p --listen-on-stdin`, `cloudflared access ssh --hostname %h`). The CLIs must be installed on the host. |
| PROTO-34 | SPICE | Viewer for QEMU/Proxmox SPICE consoles. | nice | backend-go+frontend | spice-html5 client with a Go WS-to-TCP relay; experimental. |
| PROTO-35 | Database console and DB proxy | Browser SQL console for MySQL and PostgreSQL: read-only mode, query audit, blocking rules, approval work orders. Optional wire proxy so native clients connect through NexTerm. | nice·srv | backend-go+frontend | `go-sql-driver/mysql`, `jackc/pgx/v5` (pgproto3 for the proxy); CodeMirror SQL editor with a virtualized grid. |
| PROTO-36 | tmux control-mode integration | `tmux -CC` windows and panes render as native NexTerm tabs and splits, with scrollback from tmux and reattach. | nice | backend-go+frontend | Go parser for `%output`, `%layout-change` and `%window-add`; panes map to virtual channels; the layout engine renders tmux layout strings. |
| PROTO-37 | Proxmox and hypervisor consoles | List, start and stop VMs and LXC; open termproxy or vncproxy consoles; import guests as connections. | nice | backend-go+frontend | `luthermonson/go-proxmox` v0.8.1; consoles relayed over Go WebSockets. |
| PROTO-38 | Browser-side end-to-end SSH mode | SSH runs in the browser and the server is only a TCP relay, so it never sees plaintext. This loses recording, persistence and filtering. | nice | backend-go+frontend | x/crypto/ssh compiled to WASM (`GOOS=js`); `/relay` WS with a destination allow-list; credentials from the zero-knowledge vault (SEC-12). |
| PROTO-39 | Auto-reconnect and end-of-session prompt | On disconnect the terminal shows "R = restart, S = save output, Enter/Q = close" (keys configurable, message can be toggled). Auto-reconnect with backoff and jitter for SSH, RDP (re-establishing its tunnel first), VNC and serial (waits for replug). Scrollback is kept and a separator is printed. Never auto-retries after an auth failure or host-key mismatch. | must | backend-go+frontend | Per-session state machine in Go. Backoff 1 s to 60 s, retrying immediately on the browser `online` event. New PTY with the same size, TERM and env; re-request agent and X11; rebuild SFTP; restart tunnels; optionally `cd` back to the last OSC 7 cwd. |
| PROTO-40 | Duplicate session and cross-open | Duplicate tab (Ctrl+Shift+U); new shell on the same connection; open an SFTP session with the same parameters; open the SFTP browser at the terminal's cwd; "Open terminal here" from a folder; reconnect in a new tab. | must | backend-go+frontend | Reuse the ref-counted `*ssh.Client` (SSH-35). cwd comes from OSC 7. |
| PROTO-41 | Concurrency limits and load-balancing groups | Maximum concurrent sessions per connection and per user. Balancing groups route users to the least-busy RDS farm member, with weights and failover-only members. | nice·srv | backend-go | Counters in the SessionManager; balancer picks the member at connect time. |

### 1.3 SSH (SSH)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| SSH-1 | Password auth | Three modes: saved in the vault, asked every time, or asked once and kept in memory so auto-reconnect works. A wrong password re-prompts up to N times. The stored password also answers keyboard-interactive "Password:" prompts, since PAM often runs through KI. | must | backend-go+frontend | `ssh.PasswordCallback` (prompts only when the server offers the method) wrapped in `RetryableAuthMethod(m,3)`. Secrets kept as `[]byte` and zeroed after use. |
| SSH-2 | Public-key auth from files | Formats: OpenSSH, PEM PKCS#1/#8/SEC1, legacy encrypted PEM, PuTTY PPK v2/v3. Several identities per session, IdentitiesOnly, key ordering that stays under the server's MaxAuthTries. | must | backend-go+frontend | `ssh.ParseRawPrivateKey`; PPK via `kayrus/putty` v1.0.5. **Gotcha:** only the first AuthMethod of each type is used, so merge file keys and agent keys into one `PublicKeysCallback`. x/crypto sends an unsigned query per key before signing. |
| SSH-3 | Deferred passphrase prompt | The passphrase is asked only for the key the server accepted. It can be cached for the session or saved in the vault, and re-prompts on a wrong entry. | must | backend-go+frontend | Handle `*ssh.PassphraseMissingError` (its `PublicKey` is set for OpenSSH keys; for PEM, read the sibling .pub). A lazy `MultiAlgorithmSigner` prompts inside `SignWithAlgorithm`. |
| SSH-4 | Signature algorithm control | Per-session PubkeyAcceptedAlgorithms: force legacy `ssh-rsa` (SHA-1) for old servers, or allow only SHA-2 for strict policies. | should | backend-go | `ssh.NewSignerWithAlgorithms`; the server-sig-algs extension (RFC 8308) is honored automatically; parse ssh_config `+ - ^` syntax. |
| SSH-5 | OpenSSH user certificates | Attach `id_*-cert.pub` to a key. Shows principals, validity, key ID, critical options and extensions, with an expiry countdown. Actions the certificate forbids are greyed out. A pre-connect hook refreshes short-lived certs from Vault, step-ca or Teleport. | should | backend-go+frontend | `ssh.NewCertSigner`; auto-discover `<IdentityFile>-cert.pub` and CertificateFile. The hook runs a command in desktop mode or calls HTTP when `ValidBefore < now+60s`. |
| SSH-6 | Keyboard-interactive relay (2FA, OTP, Duo, PAM) | Every challenge round (name, instruction, several prompts with per-prompt echo) appears as a browser modal. Info-only rounds (e.g. "Duo push sent") and multi-round flows are supported. | must | backend-go+frontend | `ssh.KeyboardInteractive`. A zero-question round returns immediately. The handshake deadline is paused, since a push can take over 60 s. Server text is rendered as plain text, never HTML. |
| SSH-7 | TOTP auto-fill | A per-session TOTP seed in the vault answers matching prompts automatically. The current code is shown with a countdown. After one rejection it falls back to asking the user, to avoid lockout. | nice | backend-go+frontend | `pquerna/otp` `totp.GenerateCode`. Default prompt regex `(verification code\|one-time\|otp\|token\|authenticator)`, editable per session. |
| SSH-8 | Auth orchestration | Follows PreferredAuthentications order and AuthenticationMethods chains (partial success). Prompts only for methods the server allows. A live per-attempt log reads like `ssh -v`, with a clear "Permission denied (methods)" error. | must | backend-go+frontend | `ClientConfig.AuthCallback(ctx *ssh.ClientAuthContext)` exposes AllowedMethods, PartialSuccessMethods and TriedMethods. The library caps attempts at 64. |
| SSH-9 | Host ssh-agent integration | Uses SSH_AUTH_SOCK or a per-session IdentityAgent, lists agent keys, and has presets for 1Password, Secretive, gpg-agent, Bitwarden and KeePassXC. FIDO and PKCS#11 keys work transparently through the agent. | must | backend-go+frontend | `agent.NewClient(net.Dial("unix",sock))`. In server mode the host's agent is not the user's, so it is off by default. |
| SSH-10 | Windows agents | Auto-detects the Windows OpenSSH agent pipe (also served by 1Password) and Pageant. | must | backend-go | `winio.DialPipe(\\.\pipe\openssh-ssh-agent)` (go-winio v0.6.2); `davidmz/go-pageant` v1.0.2; build-tagged `agent_windows.go`. |
| SSH-11 | Built-in agent (MobAgent) | Vault keys load into an internal agent when the vault unlocks ("load at startup"). Show, add and remove keys; lifetimes; confirm-before-use prompts naming the requesting host; auto-lock. Can optionally be exposed as a local socket or pipe for git and ssh. | should | backend-go+frontend | Custom `agent.ExtendedAgent` wrapping `agent.NewKeyring()`, because the keyring rejects ConfirmBeforeUse. Unix socket mode 0600, or `winio.ListenPipe` with a current-user SDDL. |
| SSH-12 | Agent forwarding with consent | Per session or per hop. Modes: confirm each use, forward only selected keys, and a tab indicator while forwarding is active. | should | backend-go+frontend | `agent.ForwardToAgent` once per client, then `RequestAgentForwarding` for each session before `Shell()`. Pipe-based agents need a custom `auth-agent@openssh.com` handler, because `ForwardToRemote` only dials Unix sockets. |
| SSH-13 | GSSAPI / Kerberos | Single sign-on to AD/Kerberos hosts with the current ticket; optional delegation. | nice | backend-go | `ssh.GSSAPIWithMICAuthMethod`. Windows: `alexbrainman/sspi` (the most valuable case). Unix: `jcmturner/gokrb5/v8` v8.4.4, which reads only FILE: ccaches, so an in-app kinit (password or keytab) is needed for KEYRING/KCM caches. |
| SSH-14 | FIDO2 security-key SSH keys (sk-*) | Log in with YubiKey sk-ed25519 and sk-ecdsa keys, with touch and PIN prompts. | nice | external-dependency | **Recommended:** sign through an agent (OpenSSH agent, Secretive, 1Password). **Native:** `keys-pub/go-libfido2` behind the `fido2` cgo tag. **Browser WebAuthn cannot sign for `ssh:` keys:** it only works for keys enrolled under NexTerm's rpId in the `webauthn-sk-ecdsa` format, which is experimental. |
| SSH-15 | Smart cards, PIV, PKCS#11, TPM, CAPI | Sign with smart-card, HSM or TPM keys, or Windows cert-store certificates (MobaXterm 26.4). PIN prompt in the UI; retrieve the OpenSSH public key; add the card to the agent. | nice | external-dependency | `ssh.NewSignerFromSigner`. Options: `go-piv/piv-go/v2` (tag `piv`), `ThalesIgnite/crypto11` (tag `pkcs11`), `google/go-tpm` (pure Go), Windows CNG via x/sys. The portable path is an agent with a PKCS11Provider. |
| SSH-16 | Banner, server version, client version override | Shows the pre-auth banner (legal notice) in the connect overlay, the server version string, and lets the client version string be overridden for devices that fingerprint clients. | should | backend-go+frontend | `BannerCallback` output is buffered and control-stripped. `ClientVersion` must start with `SSH-2.0-`. |
| SSH-17 | Host-key TOFU dialog | Shows key type and bits, SHA256 and MD5 fingerprints and randomart. Choices: accept once, accept and save, cancel. Records first-seen time and who accepted. Also applies to SFTP-only connections and each jump hop. | must | backend-go+frontend | `HostKeyCallback` blocks on the prompt broker. Randomart is a port of OpenSSH's "drunken bishop". Keys are normalized with `knownhosts.Normalize` (`[host]:port`). |
| SSH-18 | Changed or revoked key and StrictHostKeyChecking | A blocking "HOST KEY CHANGED" screen shows old and new fingerprints and the file:line of the saved entry. Actions: abort (default), replace (explicit confirmation) or connect once. Policies: ask, accept-new, strict, off (off shows a red badge). `@revoked` is always fatal. | must | backend-go+frontend | Handle `*knownhosts.KeyError` and `*RevokedError`. Avoid false "changed" alarms by setting `HostKeyAlgorithms` from the stored key types. |
| SSH-19 | known_hosts store and manager | NexTerm keeps its own OpenSSH-format file, optionally merged with `~/.ssh/known_hosts`. List, search, delete; hashed entries; imports from OpenSSH, the PuTTY registry and `MobaXterm.ini [SSH_Hostkeys]`. | must | backend-go+frontend | `skeema/knownhosts` v1.3.3: `NewDB`, `HostKeyAlgorithms`, `WriteKnownHost`, `HashHostname`. PuTTY hex key parameters are converted to wire format. |
| SSH-20 | Host certificates and @cert-authority | Trusts CA-signed host keys for a domain pattern and shows cert details and expiry. | should | backend-go | `ssh.CertChecker{IsHostAuthority, IsRevoked, HostKeyFallback}`; cert host algorithms go first. |
| SSH-21 | SSHFP DNS verification | Auto-trusts on a DNSSEC-validated SSHFP match; otherwise shows the match as a hint in the TOFU dialog. | nice | backend-go | `miekg/dns` v1.1.73 with the DO bit; trusted only when the response has the AD flag. |
| SSH-22 | Host-key rotation (UpdateHostKeys) | Learns all of the server's host keys after authentication, so a migration does not raise false alarms. | nice | backend-go | Intercept `hostkeys-00@openssh.com` using `NewClientConn`, then prove the keys with `hostkeys-prove-00@openssh.com`. |
| SSH-23 | Algorithm selection and negotiated display | Ordered per-session lists of KEX, cipher, MAC, host-key and pubkey algorithms, with Modern, Compatible and Legacy presets (drag-sortable). After connect, shows the negotiated set with a post-quantum badge, or a warning when the KEX is classical. | must | backend-go+frontend | `ssh.Config{KeyExchanges,Ciphers,MACs}` and `HostKeyAlgorithms`. The UI is built from `SupportedAlgorithms()` and `InsecureAlgorithms()`. `mlkem768x25519-sha256` is the default first choice. Negotiated set via `conn.(ssh.AlgorithmsConnMetadata).Algorithms()`. Lists use dnd-kit. |
| SSH-24 | Legacy device compatibility | One-click profile for old Cisco, HPE, Juniper and Dropbear gear: DH group1 and group-exchange-sha1, aes128-cbc, 3des-cbc, arcfour, hmac-sha1-96, ssh-rsa, ssh-dss. Warning badge. On handshake failure, retries with progressively wider algorithm sets and remembers what worked per host. Terrapin hints. | should | backend-go | Uses `InsecureAlgorithms()`; strict KEX is automatic. Devices needing aes256-cbc, hmac-md5 or umac are flagged unsupported (no fix without a fork). |
| SSH-25 | sntrup761x25519 KEX | MobaXterm offers NTRU Prime hybrid KEX. | nice | **infeasible** | **Reason:** not implemented in x/crypto. **Alternative:** `mlkem768x25519-sha256`, the OpenSSH 10 default, which is also post-quantum. |
| SSH-26 | SSH transport compression (zlib) | The "Compression" option for slow links. | nice | **infeasible** | **Reason:** x/crypto only implements compression `none`. **Alternatives:** compress bulk transfers in the application (remote `tar czf -` or `gzip` over exec), or use `ssh -C -W` as a ProxyCommand when OpenSSH is present. Imported configs with Compression=yes show a notice. |
| SSH-27 | Keepalives, dead-link detection and anti-idle | ServerAliveInterval/CountMax (default 30 s), TCP keepalive, WebSocket ping, an anti-idle string after N idle seconds, and Telnet keepalive. RTT from keepalive replies feeds a latency display. | must | backend-go+frontend | `SendRequest("keepalive@openssh.com",true)` in a goroutine selected against a timeout (it blocks forever on a dead link). `net.KeepAliveConfig`. `c.Ping` every 20 s. |
| SSH-28 | Connection establishment | Connect and handshake timeouts, IPv4/IPv6 preference, source bind address, retry count, and a live verbose log (resolve, TCP, banner, KEX, auth) with a Cancel button. | must | backend-go+frontend | `ClientConfig.Timeout` covers only the TCP dial, so use `conn.SetDeadline` plus closing the conn on ctx cancel, and pause the deadline during prompts. `Dialer.LocalAddr`, `FallbackDelay`. |
| SSH-29 | Jump-host / ProxyJump chains | Any number of gateways, each with its own user, auth, 2FA, host-key check, algorithms and agent setting. Saved sessions can act as gateways. Hop connections are pooled across tabs. Errors name the failing hop. X11, SFTP, tunnels, RDP and VNC route through the chain. Drag-reorderable chain editor; compatible with Teleport and CyberArk (forces the SCP browser). | must | backend-go+frontend | Per hop: `prev.DialContext` then `NewClientConn` then `NewClient`. Close in reverse order. Ref-counted hop pool. Parses `u@j1:22,u@j2`. |
| SSH-30 | SOCKS4/4a/5 proxies | Per-session or global proxy with auth and remote DNS; proxy sits in front of the jump chain. System proxy detection. Saved proxy passwords. | must | backend-go | `x/net/proxy.SOCKS5`; SOCKS4/4a hand-rolled (~60 LOC); detection from `ALL_PROXY`, `scutil --proxy` or the registry. |
| SSH-31 | HTTP CONNECT proxies (Basic, NTLM, Negotiate) | Corporate proxies, including HTTPS proxies. | should | backend-go | Hand-rolled CONNECT with `http.ReadResponse`; drain the bufio buffer first because it may hold the SSH banner. NTLM via `Azure/go-ntlmssp` v0.1.1 on the same TCP connection. Negotiate via SSPI. |
| SSH-32 | Telnet proxy and proxy via an existing tunnel | MobaXterm's Telnet-proxy type, and routing through an already-running port forward. | nice | backend-go | Proxy dialer variants in the Dialer abstraction. |
| SSH-33 | ProxyCommand | Any local command's stdin and stdout become the transport (`nc`, `socat`, `cloudflared`, `aws ssm`, `gcloud iap`) with `%h %p %r %n` tokens. | should | backend-go | `exec.CommandContext` pipes wrapped as a `net.Conn`; stderr goes to the connect log. Desktop mode, or an admin allow-list in server mode. |
| SSH-34 | Reuse an OpenSSH ControlMaster socket | Opens terminals and SFTP over the user's existing `ssh -M` master connection. | nice | backend-go | `ssh.NewControlClientConn` (new in x/crypto v0.57.0). Unix only; expands ControlPath tokens. |
| SSH-35 | Connection multiplexing | Terminal, duplicates, SSH-browser, monitor, tunnels and X11 share one authenticated connection, so 2FA and PINs are entered once. The connection stays warm for N seconds after the last tab closes. | must | backend-go | Ref-counted `*ssh.Client`. On `OpenChannelError` (MaxSessions reached) or `no-more-sessions`, opens a second connection transparently. Channel-open handlers are registered once and dispatched internally. |
| SSH-36 | ~/.ssh/config import and live sync | Imports hosts (with Include) and resolves effective options. Optional live read-only folder that updates on file change. Export back to ssh_config. | must | backend-go+frontend | `kevinburke/ssh_config` v1.6.0 (no Match support), plus `ssh -G <alias>` when the binary exists for exact resolution. `fsnotify` watches the file. Maps HostName, Port, User, IdentityFile, CertificateFile, ProxyJump, ProxyCommand, forwards, ForwardAgent, ForwardX11, algorithm lists, StrictHostKeyChecking, SetEnv, RequestTTY, RemoteCommand, ControlPath. |
| SSH-37 | Environment, locale and timezone | Per-session env table. "Adapt locales" sends LANG and LC_* (from the browser locale). Also COLORTERM=truecolor, TERM_PROGRAM, TZ. | should | backend-go+frontend | `session.Setenv` before `Shell()`. When the server's AcceptEnv rejects a variable, it is exported from the shell-integration rc instead. |
| SSH-38 | Connection details and latency | Info panel: server version, negotiated algorithms, PQ status, host key and cert, auth method used, jump chain, addresses, uptime, bytes, RTT graph, reconnect count. | nice | backend-go+frontend | `ConnMetadata`, `Algorithms()`, byte counters wrapping the `net.Conn`. |
| SSH-39 | Port knocking before connect | TCP or UDP knock sequence with delays (optionally an fwknop SPA packet), routed through the proxy or jump dialer. | nice | backend-go | Pre-connect pipeline step. |
| SSH-40 | Expired-password change | Handles "password expired, change now". | should | backend-go+frontend | **Partial.** x/crypto lacks `SSH_MSG_USERAUTH_PASSWD_CHANGEREQ` (infeasible via the password method). It works through keyboard-interactive/PAM (SSH-6) or in-shell `passwd`, and shows a clear error otherwise. |
| SSH-41 | Destination-constrained agent keys | Honors `ssh-add -h` restrictions. | nice | **infeasible** | **Reason:** x/crypto never sends `session-bind@openssh.com`. **Alternative:** use unconstrained keys with confirm-on-use (SSH-11). |
| SSH-42 | NexTerm SSH CA and short-lived certificates | NexTerm signs a user certificate valid for minutes per session, with principals from RBAC. Targets trust the CA through TrustedUserCAKeys. Provides a downloadable CA public key and sshd_config snippet. | nice·srv | backend-go | `ssh.Certificate.SignCert`; CA key held in the vault. |

### 1.4 Tunnels (TUN)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| TUN-1 | Tunnel manager (MobaSSHTunnel) | Table of saved tunnels: name, type, listen address, SSH server (with jump chain), destination. Start/stop each or all; autostart at launch; auto-reconnect; on-demand mode (SSH connects on the first client); duplicate; drag-reorder; icons; live state, active connections, bytes and last error. Graphical client→SSH server→destination diagram. | must | backend-go+frontend | One supervised goroutine per tunnel with a state machine and backoff, sharing the connection pool. Events stream over the events WebSocket. Pre-flight check for EADDRINUSE. Diagram with `@xyflow/react` (lazy). |
| TUN-2 | Local forwarding (-L) | Bind to 127.0.0.1, 0.0.0.0 or an interface, or pick a free port automatically. IPv6. Several per session. | must | backend-go | `net.Listen`, then `client.DialContext` or `DialTCP` per accept. Half-close with `CloseWrite`. Friendly messages for "administratively prohibited" and "connect failed". Warns on non-loopback binds. |
| TUN-3 | Remote forwarding (-R) | Remote bind and port forwarded to a local target, with server-allocated ports and GatewayPorts awareness. | must | backend-go | `client.Listen("tcp",addr)`; port 0 returns the assigned port. Recreated after reconnect. `ListenUnix` for sockets. |
| TUN-4 | Dynamic SOCKS (-D) | Local SOCKS4/4a/5 proxy with remote DNS, auth when not on loopback, optional HTTP CONNECT and absolute-URI mode on the same port, and a served PAC file. | must | backend-go | `things-go/go-socks5` v0.1.3 with `WithDial(client.DialContext)` and no local resolution. First-byte sniffing for SOCKS4. UDP ASSOCIATE is rejected. |
| TUN-5 | Remote dynamic (reverse SOCKS) | A SOCKS proxy listens on the remote host and exits through the NexTerm host's network. | nice | backend-go | `client.Listen` with the SOCKS server using the local dialer. |
| TUN-6 | Unix socket forwarding | Remote docker.sock or postgres socket to a local port or socket, and the reverse. "Docker socket" preset prints `DOCKER_HOST`. | nice | backend-go | `client.Dial("unix",…)` and `ListenUnix`. |
| TUN-7 | Session-integrated tunnels | Per-session forwards start and stop with the session. RDP, VNC and FTP to targets behind SSH are tunneled automatically and re-established before the dependent session reconnects. | must | backend-go | Session definitions hold forwards; relays use `ssh.Client.Dial` through the Dialer. |
| TUN-8 | Open forwarded web services in the browser | "Open in browser" for HTTP(S) targets behind SSH works even when NexTerm is remote. WebSockets are supported (Jupyter, Grafana, Xpra). | should | backend-go+frontend | `ReverseProxy` with `DialContext=sshClient.DialContext`. Host routing via `<port>-<id>.localhost`, which also isolates the app's origin. A scoped auth cookie is stripped before forwarding. |
| TUN-9 | Remote listening-port detection | Toast "port 8080 opened — Forward / Open" when something starts listening remotely. | nice | backend-go+frontend | Poll `ss -ltnH` or `/proc/net/tcp` on the monitor channel and diff. |
| TUN-10 | Kubernetes and SSM port-forwards in the manager | Tunnel types backed by `kubectl port-forward` and SSM port sessions. | nice | backend-go | client-go `tools/portforward`; ssm-session-client `PortForwardingSession`. |

### 1.5 Terminal (TERM)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| TERM-1 | Emulation core and expert terminal settings | TERM types xterm-256color, xterm-direct, linux, vt100, vt220, rxvt, screen, tmux, ansi. 256 colors and truecolor. Settings: allow alternate screen, application cursor and keypad, answerback (ENQ), local echo, implicit CR/LF, bracketed paste on/off, Ctrl+Alt as AltGr, mouse reporting, DEC 2026 synchronized output, window-ops reports. | must | frontend-only | `@xterm/xterm` 6.0.0. Answerback and alt-screen blocking via `parser.registerCsiHandler`/`registerEscHandler`. `windowOptions`. Local echo and line mode in a frontend input layer. |
| TERM-2 | PTY request details | Correct TERM, terminal modes consistent with keyboard settings (VERASE, IUTF8), and real pixel size for image-capable apps. | must | backend-go | `RequestPty(term, rows, cols, modes)` takes **height first**. For real pixel sizes, send a manual `pty-req` or `window-change` with a marshalled struct. Order: pty, then agent/X11, then setenv, then shell. |
| TERM-3 | Resize propagation | Resizing a tab, pane, zoom or font resizes the remote PTY (including pixels). Never sends 0×0 for hidden tabs. Multi-viewer policy: smallest size or last active viewer. | must | backend-go+frontend | `@xterm/addon-fit` 0.11 in a ResizeObserver, debounced 50 ms. `WindowChange(rows, cols)`. Also updates the server-side VT and trzsz columns. |
| TERM-4 | Rendering performance | GPU rendering, flicker-free output, dozens of terminals open, graceful behavior in background tabs. | must | frontend-only | `@xterm/addon-webgl` 0.19 attached only to visible panes, because Chromium allows about 16 WebGL contexts. `onContextLoss` disposes and falls back to the DOM renderer. xterm instances are kept when panes move. |
| TERM-5 | Fonts | Family, size, weight, line height, letter spacing; bold as font or bright color. Bundled programming font plus a Nerd Font for powerline/p10k. Installed-font picker. Per-script fallback chain (CJK, emoji). Fixed rows and columns option. | must | frontend-only | Embed `@fontsource-variable/jetbrains-mono` and a Nerd Font symbols woff2. `await document.fonts.load()` before `term.open()`. Local Font Access API (`queryLocalFonts`, Chromium). |
| TERM-6 | Font ligatures | Programming ligatures. | nice | frontend-only | **Experimental.** `@xterm/addon-ligatures` 0.10.0 depends on `font-finder`/`font-ligatures` (Node APIs). Check whether the xterm 6.0 fontFeatureSettings path works in a browser Vite build before shipping; otherwise leave it disabled. |
| TERM-7 | Unicode and IME | Wide and CJK characters, emoji and ZWJ graphemes, East-Asian ambiguous-width setting, CJK IME composition. | must | frontend-only | `@xterm/addon-unicode11` by default; `@xterm/addon-unicode-graphemes` 0.4 as opt-in (needs allowProposedApi). IME uses xterm's helper textarea. |
| TERM-8 | Character set conversion | Per-session charset: UTF-8, ISO-8859-x, CP1251/1252/437/850/866, KOI8-R, Shift_JIS, EUC-JP, GBK/GB18030, Big5, EUC-KR. | should | backend-go | `golang.org/x/text/encoding` streaming transforms in both directions, so xterm always receives UTF-8. IUTF8=0 and a matching LANG. |
| TERM-9 | Color schemes and theme editor | Preset gallery (MobaXterm default, Solarized, Monokai, Dracula, Gruvbox, Nord, Tango, …). Editor for 16 ANSI colors plus foreground, background, cursor, selection and bold. Live preview; per-session override; minimum-contrast option. Import/export: iTerm2 .itermcolors, Windows Terminal JSON, Xresources, MobaXterm INI. Auto light/dark switching. | must | frontend-only | xterm `ITheme` and `minimumContrastRatio`. Bundle the MIT iTerm2-Color-Schemes JSON. Converters (plist parser) in TypeScript. |
| TERM-10 | Per-host appearance and environment coloring | Per-host or folder theme, font and cursor overrides. Tab color. Red border or banner for production. iTerm2-style badge (hostname or env with variables). | should | frontend-only | Settings cascade (SM-3). Badge overlay with `${host} ${user} ${cwd}` templates. |
| TERM-11 | Background image and transparency | Opacity, background image with dimming and blur. | nice | frontend-only | `allowTransparency`, CSS `backdrop-filter`. |
| TERM-12 | Cursor settings | Block, underline or bar; blink; hollow outline when unfocused; cursor color. | should | frontend-only | `cursorStyle`, `cursorBlink`, `cursorInactiveStyle:'outline'`. |
| TERM-13 | Scrollback and scrolling | Configurable size (MobaXterm defaults to 360 000; warn about memory). Scrollbar toggle. Scroll modifier (Shift, Ctrl or Alt with PgUp/PgDn/arrows/Home/End). Scroll on output and on keypress. Wheel speed. Clear scrollback. | must | frontend-only | xterm `scrollback`, `scrollOnUserInput`, `smoothScrollDuration`; key handler for modifiers. The backend ring buffer serves reattach. |
| TERM-14 | Find in terminal | Ctrl+Shift+F, next and previous, regex, case-sensitive, whole word, highlight all, match count, overview-ruler marks. Optional server-side search of the full session log beyond memory. | must | backend-go+frontend | `@xterm/addon-search` 0.16. Session logs indexed in SQLite FTS5; clicking a hit opens a viewer. |
| TERM-15 | Keyword and syntax highlighting | Named rule sets (keyword or regex mapped to foreground, background, bold, underline; case toggle). Built-in sets: standard keywords, shell, Cisco/network (up/down, errors), Perl, SQL, Python, R, logs (ERROR/WARN, IP, MAC, dates, URLs, numbers). Selected per session, kept in selections, fast under heavy output, skipped in alternate screen. | must | backend-go+frontend | **Default:** a non-destructive viewport-scoped highlighter. On render and scroll it scans only visible rows (in a Web Worker for large panes) and applies `registerDecoration`, so copy, logs, recordings and reattach are untouched. **Optional:** a Go SGR-injection mode (ANSI tokenizer from `charmbracelet/x/ansi`, plain-text runs only, bypassing alt-screen) for users who want colors in logs. |
| TERM-16 | Selection, copy and paste | Copy on select (toggle). Right-click paste or context menu (Shift or Ctrl+right-click opens the menu). Middle-click paste, Shift+Insert, Ctrl+Shift+C/V, Cmd+C/V. Word delimiters; triple-click line; rectangular select (Ctrl+Alt+drag); trim trailing whitespace; copy as HTML with colors. | must | frontend-only | `onSelectionChange` with `navigator.clipboard.writeText` (secure context). Falls back to the native paste event where `readText` is restricted (Firefox, Safari). `wordSeparator`. `serializeAsHTML({onlySelection:true})`. |
| TERM-17 | Paste safety and paced paste | Multi-line confirmation with an editable preview, Enter to accept, "don't ask again". Malicious-paste analysis: homographs, `curl … \| sh`, `>> ~/.bashrc`, unpack to root, zero-width and NBSP characters, hidden control characters, ESC stripping to block `ESC[201~` injection. Checked once for MultiExec. Per-line or per-character delay, or wait-for-prompt, for slow devices; large pastes are chunked. | must | backend-go+frontend | Analyzer uses the Unicode TR39 confusables table and heuristics. Pacing runs in Go, because browser timers throttle in background tabs. Bracketed paste follows mode 2004. |
| TERM-18 | OSC 52 clipboard | Remote tmux, vim or helix copy to the local clipboard. Per-session policy: off, write-only, or read and write. Reads are denied or prompted by default. Size cap. | should | frontend-only | `@xterm/addon-clipboard` 0.2 with a custom provider. Shows "Remote copied N chars — click" when a user gesture is required. |
| TERM-19 | Keyboard mapping and browser-reserved keys | Backspace sends ^? or ^H; Delete, Home and End variants; function-key modes (xterm, VT100+, Linux, SCO); keypad application mode; Alt as Meta; Ctrl+Alt as AltGr; custom key-to-sequence maps; Alt+arrow mapping (removed from xterm 6); send-special-keys menu. Ctrl+W, T, N and Tab are made reachable. | must | frontend-only | `attachCustomKeyEventHandler`, `macOptionIsMeta`. Recommend the PWA install. `navigator.keyboard.lock()` in fullscreen (Chromium). Hint when a key is lost. |
| TERM-20 | Bell | None, beep, visual flash, tab highlight or desktop notification; rate-limited. | should | frontend-only | `onBell`, Web Audio, CSS flash. |
| TERM-21 | Activity and silence tracking | Blue dot or blue title on background tabs with new output; alert after N seconds of silence or on activity. | should | backend-go+frontend | Backend emits activity events even for unrendered tabs. |
| TERM-22 | Notifications and progress | OSC 9 and OSC 777 notifications. "Command finished" after a duration threshold (OSC 133). OSC 9;4 progress ring on the tab. Title badge counter. | should | frontend-only | Notification API (permission requested on a gesture); `@xterm/addon-progress` 0.2; Badging API. |
| TERM-23 | Links: URLs, OSC 8, paths, IPs | Ctrl+click URLs (including wrapped ones) and OSC 8 hyperlinks (tooltip shows the real URI). File paths and `path:line` open in SFTP or the editor, resolved against the cwd. IPs and hostnames open quick connect. Custom regex links (JIRA-123). | should | frontend-only | `@xterm/addon-web-links` 0.12, `linkHandler` (non-http schemes need confirmation), `registerLinkProvider`. |
| TERM-24 | Title handling | OSC 0/2 titles unless "Lock terminal title" is on. Manual rename; templates such as `{user}@{host}: {title}`. | must | frontend-only | `onTitleChange`. |
| TERM-25 | Zoom | Ctrl+wheel and Ctrl+Plus/Minus/0 per terminal, zoom all, and the same for RDP and VNC. | must | frontend-only | Change `fontSize`, refit, then resize the PTY. |
| TERM-26 | Save, print, clear, reset | Save output as text or colored HTML (Ctrl+Shift+S). Print with colors (Ctrl+Shift+P). Clear screen or scrollback, reset, copy all. | should | frontend-only | `addon-serialize`; Blob download or `showSaveFilePicker`; hidden iframe with `@media print`. |
| TERM-27 | Shell integration (OSC 133/633/7/1337) | Prompt and command marks, jump to previous or next prompt (Ctrl+Up/Down), select or copy a command's output, exit-status gutter, duration, separator lines (MobaXterm), cwd tracking, safe command injection. Opt-in automatic injection for bash, zsh, fish and PowerShell. | should | backend-go+frontend | Upload rc snippets and start `bash --rcfile`, zsh `ZDOTDIR`, or fish `--init-command`. Frontend `registerOscHandler`, markers and decorations. The backend mirrors at-prompt state. Fallback: prompt regex after idle output. |
| TERM-28 | Command blocks view | Each command and its output in a collapsible block with copy, re-run and bookmark. True terminal semantics underneath. | nice | frontend-only | Overlays on OSC 133 marker ranges. |
| TERM-29 | Per-line timestamps | A gutter shows when each line arrived, on hover or always. | should | backend-go+frontend | Go attaches chunk timestamps; frontend maps lines to times through markers. |
| TERM-30 | Instant replay | Scrub back to any moment, including screens later cleared by full-screen apps. | nice | backend-go+frontend | Continuous asciicast (REC-2) replayed into a hidden xterm, with periodic serialize keyframes. |
| TERM-31 | Marks, annotations, scratchpad | Bookmark lines, add notes on ranges, per-host scratchpad. | nice | frontend-only | `registerMarker` and decorations; notes stored via the API. |
| TERM-32 | Inline images | Sixel and iTerm2 IIP (img2sixel, chafa, lsix, yazi); Kitty graphics later. | should | frontend-only | `@xterm/addon-image` 0.9 with reduced `pixelLimit` and `storageLimit`; size reports; real pixel sizes (TERM-2); Kitty behind a setting (6.1 betas). |
| TERM-33 | Password-prompt detection and credential paste | Detects `Password:` and `[sudo] password` and offers one-click injection of the stored secret (never displayed). Hotkey to type the connection password. | should | backend-go+frontend | Regex over recent output raises an event; Go writes the secret; audit records it without the value. |
| TERM-34 | Accessibility | Screen-reader mode, high-contrast theme, full keyboard operation, reduced motion. | nice | frontend-only | xterm `screenReaderMode`; Radix primitives. |
| TERM-35 | Local Unix toolset (MobaXterm Cygwin, MobApt) | Bundled bash, grep, rsync and friends on Windows; package manager; persistent HOME; /drives; `open` and `cygpath`. | nice | **infeasible** | **Reason:** a 1:1 Cygwin/MobApt port is Windows-specific and huge. **Alternative:** detect and offer WSL, Git Bash, MSYS2 and busybox-w32 profiles (optional explicit download); persistent HOME and startup scripts via PTY env. |
| TERM-36 | Command history and autocomplete | Per-host and global history from shell integration, a Ctrl+R-style fuzzy overlay, and ghost-text suggestions (history, snippets, remote paths, CLI specs). Active only at a detected prompt. | should | backend-go+frontend | OSC 133 records in SQLite FTS5; decoration at the cursor; `withfig/autocomplete` specs (MIT); SFTP ReadDir for paths. |
| TERM-37 | Mobile extra-keys row | Esc, Tab, Ctrl, Alt, arrows, `\|`, `~` with sticky Ctrl; swipe between tabs; pinch for font size; correct layout with the virtual keyboard open. | should | frontend-only | VisualViewport; input injected through the `onData` path. |

### 1.6 Files: SFTP, transfers, editing (FILE)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| FILE-1 | SSH-browser side panel | Opens automatically in the sidebar for SSH sessions on the same connection, with a host-key prompt. Columns: name, size, date, permissions, owner, group, symlink target. Hidden-file toggle, saved column widths, compact mode, free space, fast virtualized listing of 100k-entry folders. | must | backend-go+frontend | `pkg/sftp` `ReadDirContext`. Longnames are dropped, so owner names come from one cached `getent passwd; getent group`. `Stat` and `ReadLink` for symlinks. `StatVFS` when supported. TanStack Virtual and Table. |
| FILE-2 | Follow terminal folder | The panel follows the shell's cwd (on by default, still works after macros). Manual sync in both directions: "cd to this folder" sends a quoted `cd` only when the shell is at a prompt. | must | backend-go+frontend | Backend byte scanner for OSC 7, OSC 633 P Cwd and OSC 1337 CurrentDir, fed by shell integration. Linux fallback: `readlink /proc/<pid>/cwd`. |
| FILE-3 | Navigation and file operations | Editable location bar, back, forward, up (mouse button 4), home, bookmarks, filter-as-you-type, sort, refresh (F5), multi-select, keyboard navigation. New file or folder, rename (F2), move, symlink, hardlink, safe recursive delete with progress, copy path, properties, optional trash. | must | backend-go+frontend | `PosixRename` when available (plain Rename fails if the target exists). **Gotcha:** `RemoveAll` follows a root symlink, so use a custom Lstat-based post-order delete. |
| FILE-4 | Permissions and ownership | chmod grid plus octal with setuid, setgid and sticky; recursive with separate file and directory modes (capital X); chown and chgrp pickers; sudo fallback. | should | backend-go+frontend | `Chmod` (raw POSIX bits), numeric `Chown`; `sudo -S chown -R` or `chmod` over exec when denied. |
| FILE-5 | Drag-and-drop uploads and downloads | Drop files or folders from the OS onto the folder or a subfolder; drag between two remote panels or sessions; paste files from the clipboard. Drag-out is limited: see FILE-7. | must | backend-go+frontend | `webkitGetAsEntry` or `getAsFileSystemHandle`, `<input webkitdirectory>`. Chromium `DownloadURL` drag-out for single files. |
| FILE-6 | Upload engine | Shows true remote progress, speed and ETA. Resume after drop. Atomic replace (temp name then rename). Preserves mtime and permissions. Parallel requests; bandwidth cap. | must | backend-go+frontend | Offset-based `PUT /api/fs/:conn/upload?path&offset` streaming `file.slice(n)` into `sftp.File.ReadFrom(&io.LimitedReader{…})`, which enables concurrent writes, with no temp file on the backend. `Truncate` after errors. `PosixRename`, `Chtimes`. `x/time/rate` for limits. |
| FILE-7 | Download engine | Browser download manager with HTTP Range resume. On Chromium, direct writes into a chosen folder, including whole trees without zipping. | must | backend-go+frontend | `GET` with Content-Disposition (RFC 6266), ETag and Range. `f.WriteTo` uses concurrent reads. `showSaveFilePicker`/`showDirectoryPicker` with `pipeTo(createWritable())`. |
| FILE-8 | Transfer queue | Server-side persistent queue that survives reload and tab close. Per-file and total progress, ETA, pause/resume/cancel/retry, reorder, concurrency limit. Total size computed first. Conflict policy (ask, overwrite, skip, rename, resume, newer-only, size-differs) with "apply to all". Folder merge. ASCII line-ending conversion. Optional checksum verification. Completion notification. | must | backend-go+frontend | Jobs in SQLite with a worker pool (N files × `MaxConcurrentRequestsPerFile`); progress events throttled. |
| FILE-9 | Archive download and remote extract | Download a selection as a streamed ZIP or tar.gz with no temp files; "upload archive and extract here"; folder size. | should | backend-go+frontend | `archive/zip` into the ResponseWriter (Store for already-compressed types; Zip64 automatic). Fast path: `tar -cf - \| gzip -1` over exec. `tar -xf` or `unzip -o`. `du -sb`. |
| FILE-10 | Remote file editing | Double-click opens the file in the built-in editor. Save uploads automatically, with mtime/size conflict detection and a diff/merge dialog. Encoding and line endings preserved. Warnings for binary or large files. In desktop mode, an external editor with a watch loop re-uploads on save. | must | backend-go+frontend | CodeMirror 6 (TOOL-2). In-place `O_TRUNC` write keeps the inode, or temp plus `PosixRename` then restore owner and mode. External editor: temp file, `open`/`xdg-open`/`start`, `fsnotify` with debounce. |
| FILE-11 | Sudo save and elevated SFTP | "Save with sudo" when permission is denied. Browse as root. | should | backend-go+frontend | Upload to `/tmp/.nexterm-*`, then `sudo -S sh -c 'cat "$0" > "$1"'` (keeps owner and inode). Root browsing: `sudo sftp-server` (probe distro paths) with `sftp.NewClientPipe`. `requiretty` falls back to temp plus cat. |
| FILE-12 | Preview and picture viewer | Images (zoom, fullscreen, next/previous, slideshow), PDF, audio, video, Markdown, CSV, hex view, large-log tail mode, thumbnails. | should | backend-go+frontend | `http.ServeContent` over `sftp.File` (a ReadSeeker, so Range works); native `<img>`, `<video>` and PDF iframe; `react-zoom-pan-pinch`; `react-markdown`; virtualized hex view. |
| FILE-13 | Dual-pane commander | Two panes in any combination (host FS, SFTP, FTP, S3, container). F5 copy, F6 move, breadcrumbs. The browser-side panel uses the File System Access API where available. | should | backend-go+frontend | VFS interface; admin policy restricts local roots. |
| FILE-14 | Server-to-server transfer | Drag between two hosts' panes. Data flows backend to backend, never through the browser, with progress and SHA-256 verification. | should | backend-go | `srcA.Open` piped to `dstB.ReadFrom(LimitedReader)` in the queue. Same-host duplicate via `cp -a`. |
| FILE-15 | Folder compare and sync | Compare local or remote against another side by size, mtime or hash (MobaFoldersDiff view). Upload, download, mirror (with deletes) or two-way. Exclusion globs, dry run, preview, keep a remote dir in sync. | should | backend-go+frontend | Go dual-VFS walker producing a streamed diff model. Remote `sha256sum` batches. `rsync` over exec when present. `fsnotify` for watch mode. |
| FILE-16 | Checksums and integrity | MD5, SHA-1, SHA-256 or SHA-512 of remote files; compare with a local file. | nice | backend-go+frontend | Remote `sha256sum`, `shasum -a 256` or `Get-FileHash`. Local file in the browser via `hash-wasm` streaming. |
| FILE-17 | Remote file search | Search by name, glob or regex and by content, with streamed, cancellable, clickable results (MobaFind). | nice | backend-go+frontend | `find -xdev -iname`, `grep -rIl` over exec; SFTP walk fallback. |
| FILE-18 | SFTP tuning and compatibility | Options: max packet (auto-fallback), requests per file, concurrent reads off for read-once servers, UseFstat, custom sftp-server command, server extension display. Exec features disabled gracefully on chrooted internal-sftp accounts. | should | backend-go+frontend | `pkg/sftp` v1 ClientOptions (v2 is alpha, so do not use it). |
| FILE-19 | ZMODEM (rz/sz) | `sz file` downloads it; `rz` opens the browser file picker. Progress overlay, cancel, ZRPOS resume. Works over SSH, telnet and serial. | should | backend-go+frontend | Server-side detection of `**\x18B00`/`B01` headers, then `xx25/go-zmodem` (pinned pseudo-version, tested against lrzsz). Files go through the transfer queue over HTTP. Cancel is 8×CAN plus 8×BS. Fallback: `zmodem.js` 0.1.10 in the browser. |
| FILE-20 | trzsz (trz/tsz) | tmux-friendly transfers of files and folders with resume, plus drag-drop upload into the terminal. | should | frontend-only | `trzsz` npm 1.1.6 `TrzszFilter` between the WebSocket and xterm; `uploadFiles(dataTransfer.items)`. Needs trz/tsz on the server. |
| FILE-21 | XMODEM, YMODEM, Kermit | Serial bootloader and firmware uploads and receives. | should | backend-go+frontend | Small Go implementations (XMODEM-CRC/1K, YMODEM batch; Kermit basic) on the serial stream. |
| FILE-22 | Drop a file onto the terminal | Local shell: pastes the quoted path. Remote: uploads to the cwd (SFTP or trzsz) and optionally types the resulting path. | nice | backend-go+frontend | Drop handler on the xterm container; cwd from OSC 7. |
| FILE-23 | Hex editor | Edit binary remote files with overwrite, insert and search. | nice | backend-go+frontend | Virtualized hex grid; write back with the conflict check from FILE-10. |

### 1.7 Graphical sessions: RDP and VNC specifics (GFX)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| GFX-1 | Dynamic resize, resolution, HiDPI | Remote desktop follows the tab size. Fixed resolution; fit, 1:1 or scrollbars; devicePixelRatio-aware; auto-resize after remote resolution changes. | must | frontend-only | IronRDP `displayControl`; noVNC `resizeSession`, `scaleViewport`, `clipViewport`; guacd `size` with `resize-method=display-update`. Debounced ResizeObserver. |
| GFX-2 | Clipboard sync | Two-way text, plus images and files where the protocol allows. Automatic on focus in Chromium; elsewhere a manual clipboard panel (hidden by default). Direction controlled by policy. | must | backend-go+frontend | IronRDP CLIPRDR (text and files); noVNC `clipboardPasteFrom` and the `clipboard` event; guacamole `createClipboardStream`. `navigator.clipboard.read()` on focus. Go drops streams per policy (SEC-15). |
| GFX-3 | Keyboard: special combos, layouts, capture | Ctrl+Alt+Del, Win, Alt+Tab, Ctrl+Alt+F1–F12, PrintScreen, sticky modifiers, on-screen keyboard, per-connection server layout, "type clipboard as keystrokes" for iLO/iDRAC/BIOS consoles, Ctrl+wheel forwarding. Keyboard Lock in fullscreen. | must | frontend-only | Map `KeyboardEvent.code` to scancodes or keysyms; `Guacamole.OnScreenKeyboard`; noVNC `sendKey`; paced typing; `navigator.keyboard.lock()`. |
| GFX-4 | Touch and mobile input | Absolute (tap) or touchpad mouse modes, two-finger scroll and right-click, pinch zoom and pan, long-press drag, text-input mode for mobile IMEs. | should | frontend-only | `Guacamole.Mouse.Touchscreen`/`Touchpad`; noVNC gestures; hidden textarea plus VisualViewport. |
| GFX-5 | Zoom, scale, quality, view-only | 10% zoom steps; local or remote cursor; view-only toggle; color depth, quality and compression sliders. | should | frontend-only | noVNC `qualityLevel`/`compressionLevel`/`viewOnly`; `Guacamole.Display.scale`. |
| GFX-6 | Multi-monitor | Spread an RDP session across several browser windows, one per monitor. | nice | backend-go+frontend | Window Management API (`getScreenDetails`, Chromium) places fullscreen popups. Needs RDP monitor-layout negotiation, which IronRDP supports and a single guacd surface cannot. |
| GFX-7 | RDP drive redirection and virtual drive | A "NexTerm Drive" appears in Explorer. Dropping files on the canvas uploads them; files placed in `Download` are pushed to the browser. Per-user or per-connection drive with upload/download flags and quotas; specific host drives selectable. | should | external-dependency | guacd path: `enable-drive`, `drive-path`, `create-drive-path`, file streams (`createFileStream`, `Guacamole.Object`), Go intercepts put/get for audit. IronRDP: RDPDR is not in the published npm API, so this is a later phase. |
| GFX-8 | Share a real local folder into RDP | Exposes a folder on the viewer's machine read-write without uploading it first. | nice | backend-go+frontend | `showDirectoryPicker` mapped onto RDPDR I/O (Chromium only; move is copy plus delete). Needs RDPDR in the client (IronRDP later); impossible through guacd, which keeps drives on the server. |
| GFX-9 | Audio output, microphone, printing | Remote sound, mic input, and a virtual printer that produces a PDF download. | nice | external-dependency | guacd: `Guacamole.AudioPlayer` and `AudioRecorder` (`enable-audio-input`), `enable-printing` (GhostScript) sent as a PDF file stream. IronRDP web: print callbacks (`printJobStreamCallbacks`) available; audio not exposed yet. |
| GFX-10 | RemoteApp, RD Gateway, RDS load-balance info | Launch published apps (`\|\|notepad`), connect through an RD Gateway with separate credentials, pass load-balance info. | should | external-dependency | guacd parameters `remote-app*`, `gateway-*`, `load-balance-info`. IronRDP: not in the web API yet. |
| GFX-11 | Hyper-V console (vmconnect) | Connect to a VM console through the Hyper-V host. | nice | backend-go+frontend | IronRDP `preConnectionBlob`; guacd `security=vmconnect` with `preconnection-blob`. |
| GFX-12 | H.264 / WebCodecs decoding | Low bandwidth and low CPU for high-motion desktops. | nice | frontend-only | noVNC 1.7 H.264 via WebCodecs `VideoDecoder`; decode in a Worker with OffscreenCanvas. |
| GFX-13 | Smart-card, serial-port, USB and WebAuthn/Windows Hello redirection (RDP) | MobaXterm 26.5 redirects WebAuthn, smart cards and ports. | nice | **infeasible** | **Reason:** a browser cannot expose PC/SC readers, COM ports or platform authenticators to a remote RDP channel. **Alternative:** in desktop mode, launch native `mstsc`/`xfreerdp` (CC-15); for smart-card login, use NLA passwords or RD Gateway; for WebAuthn, sign in on the remote with a phone passkey. |
| GFX-14 | Azure Virtual Desktop / Entra auth, Restricted Admin, Remote Credential Guard | Enterprise RDP auth modes. | nice | external-dependency | Not exposed by the IronRDP web API or guacd 1.6. Later phase (MSAL/Entra flows); native-client launch (CC-15) meanwhile. |
| GFX-15 | CredSSP with the current Windows login (SSO) | Automatic RDP sign-in with the logged-on user's credentials. | nice | **infeasible** | **Reason:** a browser has no access to the OS login token. **Alternative:** vault credentials, `kdcProxyUrl` Kerberos (IronRDP) with explicit credentials, or native-client launch. |
| GFX-16 | SFTP companion for graphical sessions | VNC or RDP sessions to Linux hosts attach an SFTP panel through a companion SSH endpoint. | should | backend-go+frontend | Reuse FILE-1 with a linked SSH session (Guacamole's `enable-sftp` equivalent). |
| GFX-17 | VNC reverse (listening) mode and repeater | Accept incoming VNC connections (UltraVNC SC) and connect through a repeater ID. | nice | backend-go+frontend | Go listener on 5500 hands the TCP connection to a waiting noVNC tab; repeater ID sent before RFB. |
| GFX-18 | Screenshot of a graphical session | Save or copy the current RDP or VNC frame as PNG. | nice | frontend-only | `canvas.toBlob`; clipboard write of the image. |

### 1.8 UI/UX and workspace (UI)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| UI-1 | Quick connect bar | Accepts `ssh user@host:port`, `ssh -p 2222 -J bastion u@h`, `telnet://`, `rdp host`, `vnc`, `sftp://`, `serial:COM3@115200` and ssh_config aliases. Opens an unsaved live session, with "save as session". Completion menu (saved sessions, recent hosts, protocols, options), a `help` command, fuzzy search that expands matching folders, Ctrl+Shift+Q focus. | must | backend-go+frontend | Frontend tokenizer (shell-quote syntax plus a URL parser) inside a cmdk combobox. The backend resolves aliases and creates an ephemeral session (MobaXterm `createsession` and `livesession` semantics). |
| UI-2 | Tabs | New, close and cycle (Ctrl+Tab, Ctrl+Alt+arrows, Ctrl+Alt+F# for tab N). Drag reorder, pin, colors, activity dot, middle-click close, wheel-scrolling tab strip, close others/inactive/to the right, tab-overflow search, tab groups, optional vertical tabs, tab shapes. | must | frontend-only | dockview-react 8.3.1 custom headers; zustand store. Alternate bindings for keys the browser reserves (TERM-19). |
| UI-3 | Split panes and tiling | Layouts: 1, 2 vertical, 2 horizontal, 4-grid (Ctrl+Alt+1–4). Arbitrary nested splits, drag a tab into a pane, resizable dividers, maximize, equalize, swap, move a pane to a tab or window. Graphical sessions can be tiled. | must | frontend-only | dockview groups; reparent the xterm DOM, never recreate it. `react-resizable-panels` v4 for the app shell. |
| UI-4 | Detach, popout, fullscreen, always-on-top | Detach a tab into its own window (multi-monitor) and redock it. The session stays live and window state stays in sync. F11 fullscreen of any tab. Always-on-top PiP for a log or monitor. | should | frontend-only | dockview popout groups or `window.open('/popout/:id')` reattaching to the server session; BroadcastChannel; Fullscreen API; Document Picture-in-Picture (Chromium). |
| UI-5 | Keyboard Lock and unload guard | Fullscreen "capture all keys" mode; stops F5 and Ctrl+S reaching the browser; `beforeunload` warning while sessions are active (in desktop mode they survive anyway). | should | frontend-only | `navigator.keyboard.lock()` (Chromium); `preventDefault` on keys that can be captured. |
| UI-6 | Chrome toggles and compact mode | Show or hide the sidebar (Ctrl+Shift+B), toolbar, menu bar and status bar; compact mode; fullscreen main window. | should | frontend-only | Persisted per user in the layout store. |
| UI-7 | Home tab, hints and built-in help | Welcome tab with recent sessions, "New session" and quick start. Contextual hints, embedded docs, keyboard reference, `help` in quick connect. | should | frontend-only | Markdown docs embedded and rendered with react-markdown; Radix Tooltip. |
| UI-8 | Configurable shortcuts | Full hotkey map with MobaXterm defaults: Ctrl+Alt+T new terminal, Ctrl+Alt+Q close, Ctrl+Shift+F find, Ctrl+Shift+S save, Ctrl+Shift+P print, Ctrl+Shift+D detach, Ctrl+Shift+U duplicate, Ctrl+Shift+N new session, Ctrl+Shift+M editor, Ctrl+Shift+H help, Ctrl+Alt+M popup console, F11. Chords, conflict detection, pass-through to the terminal, presets (MobaXterm, PuTTY, iTerm2, VS Code), per-platform Cmd/Ctrl, import/export. | must | frontend-only | `tinykeys` 4.0.1 with a scope layer (terminal-focused vs UI). The xterm key handler decides app vs pass-through. One command registry. |
| UI-9 | Command palette | Ctrl+Shift+Space or Ctrl+K fuzzy launcher for every action, session, macro, snippet, tunnel, setting, tab and recent file. Prefixes `>` commands, `@` hosts, `#` snippets, `/` files. Ranked by recent use. | must | frontend-only | cmdk 1.1.1 over the command registry. |
| UI-10 | Menus, toolbar, context menus, status bar | MobaXterm-style toolbar (Session, Servers, Tools, Sessions, View, Split, MultiExec, Tunneling, Settings, Help, X server). Right-click menus everywhere (terminal: copy, paste, find, clear, reset, save, print, settings). Status bar: user@host, protocol, cipher, latency, encoding, size, transfers, broadcast state, lock state. | should | frontend-only | Radix Menubar and ContextMenu driven by the registry. |
| UI-11 | Session and tab context actions | Duplicate, reconnect, edit session, rename tab, tab color, open in a new window, "Ping host", copy host or IP, open SFTP with the same parameters, close others. | must | frontend-only | Radix ContextMenu; backend clone endpoint. |
| UI-12 | Themes, dark mode, UI scale | Light, dark or system (dark in every dialog); accent colors; icon themes; tab shapes; UI zoom that replaces DPI adaptation. The terminal theme is independent of the UI theme. | should | frontend-only | Tailwind 4 CSS variables; UI zoom via the root rem size. |
| UI-13 | Workspaces and layout restore | Restores the last tabs, splits, SFTP panes and tunnels at startup. Named workspaces open a set of sessions in a layout (for example 4 servers in a grid). Autostart sessions at launch. Synced per user. | should | backend-go+frontend | Serialize dockview JSON and session specs to the DB. Lazy reconnect when a panel becomes visible, respecting MFA prompts. |
| UI-14 | Settings hierarchy, profiles and raw editor | Global defaults, templates (PuTTY "Default Settings"), folder and host overrides, terminal profiles, reset per field, GUI plus a raw JSON editor with schema validation, and a search box. | should | backend-go+frontend | JSON Schema generated from Go structs (`invopop/jsonschema`) drives both the forms and CodeMirror JSON linting. |
| UI-15 | Popup (Quake-style) console | A semi-transparent drop-down terminal toggled by Ctrl+Alt+M, with its own paste confirmation. | nice | backend-go+frontend | In-page overlay drawer running a local shell. A **system-wide** hotkey from a browser is **infeasible**; the closest alternative is the tray process registering one (`golang.design/x/hotkey`, `ghotkey` tag) to focus or open the app window. |
| UI-16 | Desktop notifications | Disconnect, transfer done, bell or activity in a background tab, trigger match, long command finished, share joined, approval requested. | nice | frontend-only | Notification API, Badging API; Web Push (VAPID) in server mode. |
| UI-17 | Internationalization | Translated UI, custom translation files from the data dir, locale-aware dates and sizes, RTL-safe layout. | nice | frontend-only | `i18next` 26 and `react-i18next` 17, lazy-loaded bundles; `Intl`. |
| UI-18 | Responsive / mobile UI | Usable from phone or tablet in server mode: drawer sidebar, quick connect, extra keys (TERM-37), touch input for desktops (GFX-4). | should | frontend-only | CSS container queries; test on iOS Safari (no Keyboard Lock, limited clipboard). |
| UI-19 | Active sessions manager | Table of all open sessions with status, host, duration and bytes; filter; bulk reconnect, close or broadcast selection. | should | backend-go+frontend | SessionManager stats over the events WebSocket. |
| UI-20 | Console saver and lock blur | MobaXterm's console saver after N seconds; terminals blurred while locked (SEC-5). | nice | frontend-only | Idle timer overlay. |

### 1.9 Session library / connection manager (SM)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| SM-1 | Session tree | Nested folders, drag-drop move and reorder, per-session icon (built-in set plus custom PNG/ICO/SVG uploads that travel with exports), tab color, rename, duplicate, delete, bulk edit, collapse all, open all sessions in a folder, sort, open-session green dot (click jumps to the tab), host reachability dot. Stays fast with thousands of sessions. | must | backend-go+frontend | `@headless-tree/react` 1.7.0 with TanStack Virtual; adjacency list in SQLite; icons in the data dir. |
| SM-2 | Per-session bookmark settings | Name, icon, notes, tab color. Start mode (tab, detached, fullscreen, minimized). Macro at start. "Prevent closing while active". Reconnection message toggle. Lock title. Desktop shortcut. Per-session font, colors and highlighting overrides. | must | backend-go+frontend | Tabbed form (Basic, Advanced, Terminal, Network, Bookmark) with react-hook-form and zod. |
| SM-3 | Inheritance, templates, parameter tokens | Folder defaults (identity, proxy, jump host, theme, triggers) inherited by children, with override indicators. Reusable templates. Tokens `${USERNAME}`, `${CLIENT_IP}`, `${DATE}`, `${NAME}` in usernames, paths and recording names. | should | backend-go | Merge along the ancestor chain; tokens expanded at connect time. |
| SM-4 | Tags, notes, custom fields, OS icon | Multiple tags, color labels, Markdown notes, custom fields (asset ID, owner). OS icon auto-detected on first connect. | should | backend-go+frontend | `cat /etc/os-release; uname -s` after login; bundled SVG icon set; react-markdown with remark-gfm. |
| SM-5 | Fuzzy search and filters | Instant search across name, hostname, user, tags and notes; filter by tag, protocol or status; type-to-connect. | must | frontend-only | `fuse.js` 7.5.0 over a client index shared with the palette. |
| SM-6 | Favorites, recents, frecency | Pinned favorites, recent connections with timestamps, frequency sorting, jump list in the palette. | should | backend-go+frontend | Connection-history table with a frecency score. |
| SM-7 | Identities (reusable credentials) | Named identity (user plus password and/or key plus cert plus TOTP) linked to many hosts: change once, apply everywhere. Multiple credentials per host and login. `[cred]` username syntax. | must | backend-go+frontend | Identity entity resolved in Go at connect time through the cascade. |
| SM-8 | Dynamic folders / cloud inventory | Folders filled from AWS EC2, Azure, GCP, DigitalOcean, Proxmox, Ansible inventory, Consul, a script's JSON output or a CSV URL. Refresh on open or on an interval; field mapping (user, key, jump host). | nice | backend-go+frontend | Provider interface: aws-sdk-go-v2 ec2 `DescribeInstances`, Azure armcompute, go-proxmox, Ansible parser, goja or exec script providers. |
| SM-9 | Session trash, undo and history | Deleted sessions go to a trash; undo edits; per-session change history. | nice | backend-go+frontend | Soft-delete and revision table. |

### 1.10 Automation (AUTO)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| AUTO-1 | Macros | Record keystrokes, save as a named macro, and replay on the current, selected or all terminals. Editor with text, special keys and delays; waitFor-pattern steps; variables (session, host, IP); include other macros; folders with drag-drop; macros sidebar; Alt+Space contextual menu; run at session start or from the CLI; assign to a hotkey or button. Secrets are stored in the vault. | must | backend-go+frontend | Steps: `send`, `key`, `wait ms`, `waitFor regex timeout`, `include`. Recorded from `onData` with timestamps. Executed in Go so they run in hidden tabs and read the raw stream. |
| AUTO-2 | Graphical macros | Record and replay mouse and keyboard in RDP, VNC and X11 sessions, with a warning about resolution changes. | nice | frontend-only | Normalized framebuffer coordinates through the noVNC and IronRDP input APIs. |
| AUTO-3 | Snippets / command library | Folders, tags, descriptions, placeholders (`{{var\|default}}`, dropdowns, secret vars, `${host}`, `${date}`), preview before running. Run in the current, selected or all sessions, with or without Enter, or headless on many hosts with a per-host result table. Personal and shared libraries. | must | backend-go+frontend | Snippets table; a variable form generated from the template; headless fan-out over exec channels. |
| AUTO-4 | Button bar / quick bar | Global, per-host and per-group button rows that send strings with escapes (`\r`, `\t`, `\x03`), run snippets or scripts, send keys or open sessions. Several named bars. | should | frontend-only | Bar definitions stored server-side; shared escape parser. |
| AUTO-5 | MultiExec / broadcast | Grid of all terminals with keystrokes broadcast to all or selected; include or exclude per tab; named colored sync channels; visual ring on synced panes; paste checked once; macros replay to all. "Connect folder as grid" (cluster SSH). Also works for graphical sessions. | must | backend-go+frontend | One WebSocket message `{targets,data}` that Go fans out in order (and audits as one action). Guacamole or noVNC key events forwarded to each focused client. |
| AUTO-6 | Compose / send bar | Multi-line composer that sends to the current, selected or all sessions. History; Enter vs Ctrl+Enter; line-by-line with a delay or wait-for-prompt; snippet insertion. | should | backend-go+frontend | CodeMirror input; Go paced sending (delay, prompt regex, or OSC 133 A). |
| AUTO-7 | Triggers | A regex on output, or an event (connect, disconnect, command finished), fires actions: highlight, notify, sound, send text, run a snippet or macro, inject a vault secret, capture to a panel, set tab color or badge, start logging, open a URL. Per host or group, with rate limits, a once flag and loop guards. | should | backend-go+frontend | Evaluated in Go on the ANSI-stripped line stream (RE2), so triggers work with no browser attached. Visual actions are sent as events. |
| AUTO-8 | Logon actions (expect/send) | Per-host ordered steps: wait for a prompt, send a response (user, enable password, `terminal length 0`), auto su or sudo, then startup commands. Also works for telnet and serial. | must | backend-go | Go expect engine with timeouts; secrets pulled from the vault and masked in logs. |
| AUTO-9 | Secret injection hotkey | Sends a vault secret to the active session on demand; never echoed or logged. | should | backend-go+frontend | UI sends a `secretRef`; Go writes it; the logger masks the next input. High-sensitivity secrets require re-auth. |
| AUTO-10 | Scripting engine | JS scripts with an API: `session.open`, `send`, `expect(regex, timeout)`, `readScreen`, `exec`, `sftp.get/put`, dialogs, tab control, loops over hosts. Run from a menu, button, trigger or the CLI; startup scripts; script recorder. | should | backend-go+frontend | `dop251/goja` (pure Go, pinned pseudo-version), sandboxed by permission; optional `gopher-lua` v1.1.2; CodeMirror script editor. |
| AUTO-11 | Batch workflows | Multi-step workflows (connect, commands, transfers, waits, conditions) across host groups with a parallelism limit, stop-on-error and a CSV report. | should | backend-go+frontend | errgroup plus a semaphore; results persisted; live progress grid. |
| AUTO-12 | Scheduled tasks (cron) | Crontab-like schedules for snippets, workflows, tunnels, transfers and backups, with run history. Runs only while NexTerm runs (or as a service). | nice | backend-go+frontend | `robfig/cron/v3` v3.0.1 parser and scheduler; cron-expression helper UI. |
| AUTO-13 | Pre/post-connect local commands and external tools | Runs host programs before, after or on demand with `%HOST% %PORT% %USER%` (VPN connect, Wireshark, a custom client). | nice | backend-go | `exec.Command` with templating; admin-only in server mode. |
| AUTO-14 | CLI and local control API | `nexterm open ssh://u@h`, `session create`, `macro run X`, `-bookmark`, `-exec`, `-runmacro`, `-openfolder`, `-exitwhendone`, `-edit`, `-compfiles`, `-compfolders`, `-log`, `-config`. Talks to the running instance, or starts one. | should | backend-go | `spf13/cobra` v1.10.2; local HTTP API authenticated by a 0600 token file, a Unix socket or a named pipe. |
| AUTO-15 | Remote helper (wsh-style) | Inside a remote shell, a tiny helper opens a file in the editor, downloads or uploads, sets badge or tab color, or notifies. Works through any hop. | nice | backend-go+frontend | OSC 1337-style escapes (no network path needed); helper script uploaded on demand. |
| AUTO-16 | Notifications and webhooks | Email, webhook, Slack, Teams, Discord, ntfy or Gotify on events (new-IP login, blocked command, access request, host down, recording ready). | nice·srv | backend-go | Event bus with notifier adapters; `wneessen/go-mail` v0.8.1. |

### 1.11 Security and vault (SEC)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| SEC-1 | Master password and encrypted store | A master password protects every password, passphrase, key, TOTP seed and secret macro. Complexity levels; delay after wrong attempts; change with re-encryption; "remember master password" (admins can disable it). | must | backend-go+frontend | Argon2id (t=3, m=64 MiB, p=4) derives a KEK that wraps a random DEK. Each record is sealed with XChaCha20-Poly1305, with the record ID and schema version as AAD. KDF parameters are stored so they can be upgraded. The DEK stays in Go memory only and is zeroed on lock. |
| SEC-2 | Password and credential manager | Lists saved passwords per host, user and protocol, plus named credentials. Add, edit, delete, copy (clipboard auto-clears). Save policy Always/Ask/Never and "save passphrases too". Reveal requires re-auth. | must | backend-go+frontend | Secrets reach the browser only on explicit reveal or copy. |
| SEC-3 | OS keychain unlock | Passwordless unlock on trusted machines. | should | backend-go | `zalando/go-keyring` v0.2.8 (macOS `security`, wincred, Secret Service; cgo-free; unavailable on headless Linux). Stores only the 32-byte DEK or wrap key. |
| SEC-4 | Biometric / passkey unlock and confirmation | Windows Hello, Touch ID or a passkey to unlock the vault and confirm before using or revealing secrets. | nice | backend-go+frontend | WebAuthn user verification (`go-webauthn` and `@simplewebauthn/browser`). The PRF extension derives the unwrap key. |
| SEC-5 | Auto-lock and lock screen | Manual lock hotkey and idle auto-lock. Terminals are blurred; sessions keep running; the backend refuses data until unlock. | should | backend-go+frontend | Frontend idle timer plus server-side timeout and token revocation. |
| SEC-6 | Web UI access security | 127.0.0.1 by default with a one-time launch token. Strict Origin and Host checks on every HTTP and WebSocket request (cross-site WebSocket hijacking, DNS rebinding). CSRF protection, CSP with nonces, secure cookies, rate limiting, per-feature permission gates. | must | backend-go+frontend | `http.CrossOriginProtection` (exempts GET, so WebSocket upgrades need coder/websocket `OriginPatterns`); Host allow-list; HttpOnly SameSite=Strict cookies. |
| SEC-7 | SSRF and destination controls | Destination allow and deny lists (CIDRs, hostnames). "Preset connections only" mode. Link-local and metadata addresses (169.254.169.254) blocked by default. Applies to every dialer. | must·srv | backend-go | `net.Dialer.Control` checks the resolved IP after DNS, which defeats rebinding. |
| SEC-8 | Admin policy and hardening presets | Disable embedded servers, packet capture, scanners, insecure protocols (telnet, rlogin, rsh, ftp), session types, local terminals and WSL, WOL, monitoring, "remember master password", RDP reconnect, debug logs, port listing. Security presets. | should | backend-go | YAML policy loaded at startup (optionally signed and embedded by the Customizer), enforced in handlers. Build tags can remove features entirely. |
| SEC-9 | External secret managers | Credentials resolved at connect time from HashiCorp Vault (including SSH CA signing), 1Password, Bitwarden, KeePass(XC) or Keeper. | nice | external-dependency | `vault://kv/path#field` references; `hashicorp/vault/api`; `op` and `bw` CLIs; KeePass natively via `tobischo/gokeepasslib/v3` v3.7.0. |
| SEC-10 | Vault sync across devices | End-to-end encrypted sync of hosts, snippets, keys and settings via Git, Gist, WebDAV, S3 or another NexTerm. Conflict resolution; option to exclude keys. | should | backend-go | Encrypted records with `updated_at`/vector clocks; adapters (go-git, gowebdav, S3). |
| SEC-11 | Secret masking and redaction | Passwords typed at no-echo prompts are excluded from logs and recordings. Regex redaction for logs and AI context. | should | backend-go | Echo-off prompt heuristic suppresses input capture; redaction filter in the log writer. |
| SEC-12 | Zero-knowledge personal vault | An optional per-user vault encrypted in the browser, so server admins cannot read it. Unlocks with a master password or passkey PRF. Cannot be used by unattended jobs. | nice·srv | backend-go+frontend | WebCrypto AES-GCM with HKDF over the PRF output; server stores ciphertext only. |
| SEC-13 | Credential rotation and key distribution | Scheduled password or key rotation; push NexTerm-managed keys to authorized_keys; central revocation; per-host results. | nice·srv | backend-go | Over SSH (`chpasswd`, atomic authorized_keys edit via SFTP); `robfig/cron`; the vault is updated only after verification. |
| SEC-14 | Command filtering and blocking | Rules (exact, prefix, regex) grouped in sets and bound to users, assets and accounts. Actions: deny, allow, review (hold until approved), alert. A guardrail, not a hard security boundary. | should·srv | backend-go+frontend | At Enter, Go takes the command line (OSC 133 or the VT buffer). Deny sends Ctrl+U or Ctrl+C with a warning; review holds the CR. |
| SEC-15 | Data-transfer policies | Per role, asset or grant: copy, paste, upload, download, print, drive mapping, sharing; clipboard size cap. | must·srv | backend-go | Drop Guacamole clipboard, file and pipe streams; set guacd `disable-copy`/`disable-paste`/`disable-upload`/`disable-download`; flags in the SFTP handlers. Terminal copy blocking is UI-only (documented). |
| SEC-16 | Watermarking | Tiled overlay with user, IP, time and asset on session views, carried into recordings. | nice·srv | frontend-only | Canvas overlay (`pointer-events:none`) with a MutationObserver that restores it; metadata stored with recordings. |
| SEC-17 | Session time limits | Idle timeout, maximum duration, access windows, warning banner; sessions killed when a grant expires or a user is disabled. | should·srv | backend-go+frontend | Per-session timers; a reaper goroutine. |
| SEC-18 | Step-up MFA | Fresh WebAuthn or TOTP before opening sensitive assets, revealing secrets or creating share links. | should·srv | backend-go+frontend | `mfa_fresh_until` on the web session; the WebSocket open returns `mfa_required`. |
| SEC-19 | Access requests (just-in-time) | Request temporary access with a reason; approvers approve or deny; the grant expires automatically. | nice·srv | backend-go+frontend | Request state machine plus notifiers; a reaper revokes grants and kills their sessions. |
| SEC-20 | Login access policies and brute-force protection | Allow or deny by IP set, GeoIP country or time window. Rate limits, temporary bans, account lockout, login ACLs (accept, reject, review). | should·srv | backend-go | Token buckets (`x/time/rate`); optional MaxMind mmdb (`oschwald/maxminddb-golang/v2`). |
| SEC-21 | Dangerous-command guard | Confirmation before `rm -rf`, `reboot` or `DROP TABLE` on production-tagged hosts, including broadcasts. | nice | backend-go+frontend | Evaluated at Enter using shell-integration text; best-effort. |
| SEC-22 | Prevent sleep ("Caffeine") | Keeps the host from sleeping or locking during long jobs, and keeps the client screen awake. | should | backend-go+frontend | `caffeinate -dimsu` (macOS), `SetThreadExecutionState` (Windows), `systemd-inhibit` (Linux); Screen Wake Lock API in the browser. |

### 1.12 Multi-user / server mode (MU)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| MU-1 | Accounts, first-run setup, web sessions | First-run admin wizard (one-time setup token printed to stdout), password policy, forced reset, disable and expiry, lockout. Users can list and revoke their active browser sessions. Idle logout. | must·srv | backend-go+frontend | Argon2id hashes; opaque server-side session tokens in cookies (instant revocation). |
| MU-2 | RBAC | Built-in roles (super-admin, admin, auditor, operator, user) plus custom roles from fine-grained permissions (connect, view-only, share, reveal, monitor, kill, audit). | must·srv | backend-go+frontend | Permission bitset checked on every route and every WebSocket open; `/api/me` capabilities only drive the UI. |
| MU-3 | Asset authorization grants | User or group × asset, folder or tag × target account, with a validity window and action flags (connect, upload, download, clipboard, share, record-exempt). Live sessions end when a grant lapses. | must·srv | backend-go | Authorizations table resolved with one query and cached; expiry callbacks. |
| MU-4 | Groups and organizations | Teams and departments; optional orgs with their own connections, vault and audit scope; switch between orgs. | should·srv | backend-go+frontend | `org_id` filtering in the repository layer; LDAP/OIDC group sync. |
| MU-5 | TOTP 2FA | QR enrolment, recovery codes, required per role, admin reset, IP-based bypass or enforce. | must·srv | backend-go+frontend | `pquerna/otp` v1.5.0; replay guard on the last used step. |
| MU-6 | WebAuthn / passkeys | Several keys per user; passwordless login with conditional UI (autofill), or as a second factor. Rename, delete, admin reset. | must·srv | backend-go+frontend | `go-webauthn/webauthn` v0.18.2 and `@simplewebauthn/browser` 14. The RP ID must be a domain (not an IP), so TOTP is the fallback. |
| MU-7 | OIDC SSO | Any OIDC provider; JIT provisioning; claim-to-role mapping; SSO-only mode; several providers; claims usable as connection tokens. | must·srv | backend-go+frontend | `coreos/go-oidc/v3` v3.21.0 with PKCE. |
| MU-8 | LDAP / Active Directory | Bind against one or more servers; scheduled or at-login group sync; nested AD groups. | should·srv | backend-go | `go-ldap/ldap/v3` v3.4.14; matching rule 1.2.840.113556.1.4.1941 for nesting. |
| MU-9 | SAML 2.0 | SP metadata, IdP-initiated login, attribute mapping. | nice·srv | backend-go | `crewjam/saml` v0.5.1. |
| MU-10 | Trusted-header / forward-auth SSO | Accepts `Remote-User` or a Cloudflare Access JWT, but only from trusted proxy CIDRs. | should·srv | backend-go | Middleware check; JWKS verification. |
| MU-11 | mTLS client-certificate login | Requires or accepts client certificates; subject or SAN maps to a user. | nice·srv | backend-go | `tls.VerifyClientCertIfGiven` with a CA pool. |
| MU-12 | Built-in OIDC identity provider | NexTerm provides SSO for other internal apps. | nice·srv | backend-go | `zitadel/oidc/v3` (op package). |
| MU-13 | Trusted devices | "Remember this device" skips 2FA for N days; device list with revoke. | nice·srv | backend-go+frontend | HMAC device cookie plus a table. |
| MU-14 | QR login and device flow | Approve a phone login from an existing session; RFC 8628 device flow for CLIs. | nice·srv | backend-go+frontend | Pending-login record; SSE completion. |
| MU-15 | API tokens and REST API | Scoped, expiring personal and service tokens; every UI action available over REST with an OpenAPI spec; token use audited. | should | backend-go+frontend | SHA-256-hashed `nxt_` tokens; `danielgtaylor/huma/v2` generates OpenAPI 3.1; `openapi-typescript` generates the React client. |
| MU-16 | Shared session files (MobaXterm-style) | Publish a folder of sessions as a file on a network share, HTTP(S), FTP(S), SSH or SFTP. Subscribers auto-refresh (update icon); allowed editors modify it in place; named credentials make each user enter their own login; icons travel with the sessions. | should | backend-go+frontend | Shared-library JSON with ETag or mtime polling; SMB via go-smb2. Never contains credentials. |
| MU-17 | Shared connection trees and team sharing | Team folders with ACLs; personal vs shared; "copy to my connections"; per-user credential override; shared identities usable without revealing them; shared snippets and workflows. | should·srv | backend-go+frontend | Downward-inherited ACLs; credential resolution order: user override, then shared identity, then prompt. |
| MU-18 | Live session sharing links | Read-only or interactive link with expiry, maximum uses, optional code or login required; revoke; see viewers; join notification. One controller at a time with request and grant. | should | backend-go+frontend | 128-bit share token mapped to session and mode. Viewers get a VT snapshot then the live stream. guacd sessions via `select $<id>` read-only. |
| MU-19 | Admin live monitoring | Dashboard of active sessions (user, asset, protocol, IP, duration, bytes). Watch live, lock input, message the user, force-disconnect, block the user. | must·srv | backend-go+frontend | SessionManager registry over SSE; read-only subscription; input gate in Go. |
| MU-20 | Moderated sessions | Observer, peer and moderator roles; a moderator must be present before a production session starts (four-eyes). | nice·srv | backend-go+frontend | `pending_moderator` state; input gated by role. |
| MU-21 | Presence, cursors and chat | Who is watching or typing, colored cursors and selections, chat panel, "follow me". | nice | backend-go+frontend | Presence messages on the session WebSocket; decorations. |
| MU-22 | Reverse-connect gateway agents | The same binary in `agent` mode dials out from a private network; assets are reached "via gateway X" with no inbound ports; HA groups; one-line install with a token. | should·srv | backend-go | coder/websocket `NetConn` with `hashicorp/yamux` v0.1.2 streams carrying dial requests; token or mTLS; self-update. |
| MU-23 | Built-in SSH bastion for native clients | `ssh alice@nexterm -p 2022` opens an asset picker TUI, or `ssh alice:asset@nexterm` goes direct. Same RBAC, recording, filters, SFTP and forwarding policy. | should·srv | backend-go | `charmbracelet/ssh` server; `bubbletea/v2` picker; routes into the shared session engine. |
| MU-24 | Native RDP through a proxy with one-time tickets | Download a .rdp file whose username carries a single-use ticket; mstsc connects through the NexTerm RDP proxy with vaulted credentials and recording. | nice·srv | backend-go | Needs a two-sided TLS/CredSSP RDP proxy, which is substantial work (possibly IronRDP-based). |
| MU-25 | Published web assets | Internal web UIs reverse-proxied behind NexTerm login, authorization and audit, by subdomain or path, with rewriting and routing through gateway agents. | nice·srv | backend-go | `httputil.ReverseProxy` Rewrite hook; certmagic wildcard certificates; `x/net/html` link rewriting. |
| MU-26 | Per-user preference sync | Settings, themes, workspaces and keymaps follow the user across devices. | should·srv | backend-go+frontend | JSON blob per user. |
| MU-27 | Branding and legal banner | White-label logo, name and colors; pre-login consent banner; custom PWA name and icon. | should·srv | frontend-only | Branding assets in the DB; manifest generated from branding. |
| MU-28 | Embedding and signed launch links | Ticketing or CMDB systems iframe or deep-link a session via a signed short-lived token; postMessage events. | nice·srv | backend-go+frontend | `go-jose/v4` JWS/JWE; CSP `frame-ancestors` allow-list. |

### 1.13 Recording and audit (REC)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| REC-1 | Terminal output logging | "Log terminal output to" a folder, per session or globally. Filename pattern (`%session %host %date %time`). Per-line timestamps with milliseconds. Plain (ANSI stripped), raw or HTML. Optional input capture. Append, overwrite or rotate with size limits. Start and stop on the fly. In-app log browser with search; open the log folder. | must | backend-go+frontend | Go tee writer; `charmbracelet/x/ansi` Strip with `\r` overwrite handling; `lumberjack.v2`; FTS5 index. |
| REC-2 | Session recording and playback | Records SSH, telnet, serial and local sessions: output, optional input, resizes, markers from OSC 133. Web player with 0.5–16× speed, idle skip, seek, jump to a command, search across recordings, copy text from a paused frame, `.cast` download, read-only share link. | should (must·srv) | backend-go+frontend | Go writes **asciicast v3** NDJSON (o, i, r, m, x events), compressed with zstd (`klauspost/compress`); v2 export for older tools. `asciinema-player` 3.17.0 mounted imperatively. |
| REC-3 | Graphical recording (RDP / VNC) | Recording with mouse and keys; timeline with activity histogram and key log; optional MP4 export. | should·srv | external-dependency | guacd `recording-path`, `recording-name`, `recording-include-keys`, played with `Guacamole.SessionRecording`; `guacenc`/ffmpeg external for MP4. noVNC path: Go records the post-handshake RFB server stream and replays it into noVNC through a fake socket. Client-side fallback: `canvas.captureStream` with MediaRecorder to WebM. |
| REC-4 | Audit log | Append-only log of logins (method, IP, geo), session start and end, admin changes with diffs, secret reveals, transfers, shares, broadcasts and AI actions. Searchable, CSV export, retention, forwarding to syslog, webhook or OpenTelemetry. | should (must·srv) | backend-go+frontend | Hash-chained rows (SHA-256 of the previous row); slog handlers (RFC 5424 syslog). |
| REC-5 | Command audit | One record per command: user, asset, account, cwd, command, exit code, time, risk level. Links to the recording timestamp. | should·srv | backend-go+frontend | OSC 133 boundaries, falling back to VT line reconstruction at Enter. Blind spots (editors, scripts) documented. |
| REC-6 | File-transfer audit | Every upload, download, delete and rename with path, size, SHA-256, user and asset; optional forensic copies. | should·srv | backend-go | Hooks in the SFTP and Guacamole stream handlers; `io.TeeReader` hashing. |
| REC-7 | Recording storage and retention | Local disk or S3-compatible storage, multipart uploads, per-org retention, encryption at rest, playback streamed from S3, watch while still recording. | should·srv | backend-go | aws-sdk-go-v2 S3 in the background; chunked AES-GCM; range reads. |
| REC-8 | Recording insights | Activity heatmap under the scrubber; optional AI summaries for reviewers. | nice·srv | backend-go+frontend | Per-second byte and event buckets written next to the file; LLM job (TOOL-13). |
| REC-9 | Debug log and event viewer | Per-session connection log (DNS, proxy, hops, auth attempts, algorithms), app event viewer with level filter, diagnostics bundle export (secrets redacted). Admins can disable it. | should | backend-go+frontend | slog ring-buffer handler per session; instrumented dialers and SSH callbacks. |

### 1.14 Monitoring (MON)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| MON-1 | Remote monitoring bar (Linux) | Bar under each SSH terminal: host and OS, uptime, load, CPU %, RAM and swap, per-mount disk (optionally NFS), network rx/tx, processes, file descriptors, logged-in users. Refreshes every 1–2 s. Orange and red thresholds, tooltips, sparkline popover. On by default. | must | backend-go+frontend | One long-lived non-PTY exec loop (§3.21), so it doesn't appear in `who`. Go computes deltas and pushes JSON. Paused when the tab is hidden. uPlot sparklines. |
| MON-2 | Cross-OS monitoring backends | macOS, FreeBSD/OpenBSD, BusyBox and Windows OpenSSH, detected automatically; degrades gracefully when exec is forbidden. | should | backend-go | Per-OS scripts embedded with go:embed: `sysctl`, `vm_stat`, `top -l 2`, `netstat -ibn`; `kern.cp_time`; PowerShell CIM JSON loop. |
| MON-3 | Remote process and service manager | Sortable, filterable process table and tree; kill, renice or signal (optional sudo); systemd service list with start, stop and restart; listening ports (links to tunnel creation); disk-usage drilldown. | should | backend-go+frontend | `ps -eo … --sort=-pcpu`, `systemctl list-units --output=json`, `ss -ltnup`, `du -xk -d1`; refreshed only while the panel is open. |
| MON-4 | Connection health graph | Latency history (browser to backend and backend to target measured separately), stall detection, throughput. | nice | backend-go+frontend | Keepalive RTT ring buffer plus WebSocket ping RTT. |
| MON-5 | Log tail viewer | Follow several remote log files merged, with level coloring, filter, pause and jump to time. | nice | backend-go+frontend | `tail -F` over exec; reuses highlight sets (TERM-15). |
| MON-6 | Local host monitor and system info | NexTerm host CPU, RAM, disks, network, OS and hardware info (MobaSwInfo/MobaHwInfo), process list and kill (MobaTaskList/MobaKillTask). | nice | backend-go+frontend | `shirou/gopsutil/v4` v4.26.8 (cgo-free via purego). |
| MON-7 | Fleet dashboard and alerts | Cross-host dashboard with downsampled history and alert rules (threshold, host down). | nice | backend-go+frontend | Series in SQLite; uPlot; notifiers (AUTO-16). |

### 1.15 Tools (TOOL)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| TOOL-1 | Key generator (MobaKeyGen) | Generate RSA (2048–4096+), ECDSA (256/384/521) and Ed25519 with comment and passphrase (DSA import only). Load and convert PPK, OpenSSH and PEM. Export authorized_keys and RFC 4716 public keys and PPK v2/v3 or OpenSSH private keys. SHA256 and MD5 fingerprints with randomart. One-click install on a server (ssh-copy-id). Sign certificates. | must | backend-go+frontend | Stdlib crypto; `ssh.MarshalPrivateKeyWithPassphrase`; own PPK v3 writer (Argon2); copy-id over SFTP (dedupe by blob, 0700/0600, atomic rename); `SignCert`. |
| TOOL-2 | Text editor (MobaTextEditor) | Tabbed editor: syntax highlighting for 100+ languages, autocomplete, regex find/replace, search in files, go to line, move lines, toggle comment, bracket matching, indent guides, same-word highlight, color swatches, folding, zoom, encoding and line-ending detect/convert, HTML export, print with colors, recent files, vim mode, large-file handling. | must | frontend-only | CodeMirror 6 (`@uiw/react-codemirror` 4.25.12, `@codemirror/language-data`, search, optional `@replit/codemirror-vim`); file I/O through the VFS. |
| TOOL-3 | Text and folder diff (MobaTextDiff, MobaFoldersDiff) | Side-by-side or inline diff with intra-line highlighting, left/right swap, synced scrolling, change navigation, chunk merge, in-place editing. Compare local, remote or clipboard. Folder compare (same, different, left-only, right-only). | should | backend-go+frontend | `@codemirror/merge` 6.12.2; folder diff reuses FILE-15. |
| TOOL-4 | Network scanner | Scan a range or CIDR for live hosts: IP, hostname (reverse DNS, NetBIOS, mDNS), MAC with vendor, open common ports (SSH, Telnet, RDP, VNC, FTP, HTTP/S). One-click create or open a session; bulk import; export results; optionally scan from a jump host or gateway. | should | backend-go+frontend | Worker pool; `pro-bing` v0.9.1 with TCP-connect fallback; ARP table from `/proc/net/arp` or `arp -a`; embedded OUI database; `hashicorp/mdns` v1.0.7 (`_ssh._tcp`, `_rfb._tcp`, `_smb._tcp`). |
| TOOL-5 | Port scanner | TCP port range or list with timeout and concurrency: open, closed or filtered; service name; banner grab; optional UDP. | should | backend-go+frontend | `net.Dialer` with an `x/sync` semaphore; embedded services map. Admin-only in server mode. |
| TOOL-6 | Local open ports (MobaListPorts) | Host listening and established sockets with PID, process and state; kill the owner. | nice | backend-go+frontend | gopsutil `net.Connections("all")`. |
| TOOL-7 | Packet capture (TCPCapture) | Pick an interface and BPF filter; live decoded list (Ethernet, IP, TCP, UDP, DNS, HTTP) with a hex pane and statistics; save pcap. Remote capture over SSH. | nice | backend-go+frontend | Best path: remote `tcpdump -U -w -` parsed by pure-Go `gopacket` pcapgo. Local capture needs libpcap or Npcap (`pcap` cgo tag) or Linux AF_PACKET. Virtualized table; pcap download. |
| TOOL-8 | Network diagnostics | Ping with latency graph; traceroute/mtr; DNS lookup (A, AAAA, MX, TXT, PTR; choose resolver); whois; httping; TLS certificate inspector; TCP connect test; iperf client; IP/subnet calculator. Run from the host, a gateway, or a remote host over SSH. | should | backend-go+frontend | `pro-bing`, `x/net/icmp` with TTL (privileged) or system traceroute, `miekg/dns`, `httptrace`, `crypto/tls`; uPlot. |
| TOOL-9 | Wake-on-LAN | Magic packet (broadcast, subnet-directed or unicast; UDP 7 or 9; SecureOn password). Saved targets. A session can hold a MAC so it wakes the host and waits for the port before connecting. Remote-subnet wake via SSH. | should | backend-go+frontend | 6×0xFF followed by 16×MAC; `SO_BROADCAST`; `wakeonlan` or `etherwake` over SSH. |
| TOOL-10 | AI assistant | Chat panel with context (selection, last output, scrollback, host OS). Explains errors, suggests commands or scripts that are inserted for review and never auto-run. Providers: Anthropic, OpenAI-compatible (Ollama, LM Studio, vLLM), Gemini. Opt-in, admin-gated, redaction pass. | should | backend-go+frontend | Go proxies the providers (keys in the vault); SSE streaming. |
| TOOL-11 | Natural language to command | `# find files over 1GB` at the prompt or in the palette gives an editable suggestion for the current shell and OS. | should | backend-go+frontend | Prefix detected via shell integration; inline decoration to accept. |
| TOOL-12 | AI error explain and fix | On a non-zero exit, offers "explain/fix" prefilled with the command, output and exit code. | nice | backend-go+frontend | OSC 133 D hook; output range from marks. |
| TOOL-13 | AI agent mode and MCP server | An agent runs commands and reads files on selected sessions with per-step approval, allow-lists, dry run and full audit. NexTerm sessions and SFTP are exposed as an MCP server. | nice | backend-go+frontend | Tool-calling loop in Go; `modelcontextprotocol/go-sdk` v1.8.0 (streamable HTTP, localhost with a token). |
| TOOL-14 | Plugin system | UI plugins (panels, commands, themes, protocol handlers) and backend extensions (VFS backends, trigger actions). Signed manifests. | nice | backend-go+frontend | ESM plugins via dynamic import from the data dir; backend WASM via `tetratelabs/wazero` v1.12.0, or JSON-RPC subprocesses. |
| TOOL-15 | Misc utilities | Games, calculator and base converter, duplicate finder (fdupes-style over the VFS), png2ico, man-page viewer. | nice | frontend-only | Low priority; hash-based duplicate finder. |

### 1.16 Embedded servers (SRV)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| SRV-1 | Servers manager | Start/stop panel with status. Per server: port, root, users and passwords, anonymous, read-only or write, bind interface, "stop after N seconds", autostart. Live connection log; firewall and privileged-port warnings. | should | backend-go+frontend | Service registry with context lifecycles; `os.Root` jails; slog streamed over WebSocket. Admin policy can disable it. |
| SRV-2 | TFTP server | Root, port 69 (configurable), read/write permissions, block and buffer size, timeouts. For firmware and config backups. | should | backend-go | `pin/tftp/v3` v3.2.0 (blksize, tsize, windowsize). |
| SRV-3 | FTP server | Root, port 21, anonymous or user, passive port range, FTPS. | should | backend-go | `fclairamb/ftpserverlib` v0.32.4 + afero `BasePathFs`. |
| SRV-4 | HTTP file server | Serve a folder with listing, optional upload, basic auth, HTTPS. | should | backend-go | `http.FileServer` over `os.Root`. |
| SRV-5 | SSH/SFTP server | Local shell and SFTP for remote clients; users with password or key; chrooted SFTP; port 22 or 2222. | should | backend-go | `charmbracelet/ssh` v0.4.3 + `pkg/sftp` RequestServer + xpty. |
| SRV-6 | Telnet server | Local shell over telnet for legacy devices. Off by default. | nice | backend-go | Option negotiation with a PTY. |
| SRV-7 | NFS server | Export a folder (NFSv3; clients mount with an explicit port and mountport). | nice | backend-go | `willscott/go-nfs` v0.0.4 with go-billy osfs. |
| SRV-8 | VNC server | Share the host desktop with a password and view-only mode. | nice | external-dependency | Delegate to OS services (macOS Screen Sharing, TigerVNC, x11vnc). A native Go server would need `kbinani/screenshot` and a cgo input injector, plus macOS permissions. |
| SRV-9 | Cron server | See AUTO-12. | nice | backend-go | Same engine. |
| SRV-10 | iperf server | Bandwidth-test endpoint for iperf3 clients. | nice | backend-go | iperf3-compatible JSON control plus data streams in Go, or launch a system `iperf3`. |

### 1.17 Import, export and OS integration (IMP)

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| IMP-1 | Import MobaXterm | `MobaXterm.ini` `[Bookmarks*]` and `.mxtsessions` for every session type, with folders and icons, plus color schemes, macros, MobaSSHTunnel tunnels, known host keys and keyboard settings. Saved passwords are decrypted only with the user's MobaXterm master password. Report of unmapped fields. | must | backend-go+frontend | `gopkg.in/ini.v1` v1.67.3; positional `#type#icon%field%…` parsing per type, covered by golden tests from sample exports; wizard with preview. |
| IMP-2 | Import other clients | PuTTY and KiTTY (registry or .reg, including proxy, certs, host CA), SuperPuTTY XML, SecureCRT XML, Xshell .xsh, mRemoteNG confCons.xml, Royal, Remote Desktop Manager, Termius, Tabby, electerm, WinSCP ini, Remmina, .rdp and .vnc files, Guacamole CSV/JSON/YAML, generic CSV with auto-detected columns and manual mapping. Dedupe and conflict preview. | should | backend-go+frontend | One parser per format into a normalized model; `x/sys/windows/registry`; `encoding/xml`; `papaparse` for CSV preview. |
| IMP-3 | Export sessions | Export to JSON, CSV, ssh_config and .mxtsessions; single-session and single-theme export. | should | backend-go+frontend | Reverse mappers. |
| IMP-4 | Backup, restore, reset | Full config (sessions, settings, macros, tunnels, themes, keys; recordings optional) as one archive, optionally password-encrypted. Scheduled backups with retention. Restore on another machine. Reset all or selected defaults. | should | backend-go+frontend | SQLite `VACUUM INTO` snapshot encrypted with `filippo.io/age` v1.3.2 (passphrase); schema migrations. |
| IMP-5 | Customizer / white-label build | `nexterm customize` produces a branded binary: logo, banners, preset sessions and folders, default settings, feature toggles, security presets, bundled plugins; no passwords allowed. | nice | backend-go+frontend | Original exe plus an appended signed zip overlay read via `os.Executable()` and `zip.NewReader` at an offset, layered over the embed FS. |
| IMP-6 | OS integration | "Open NexTerm here" in the Explorer or Finder context menu; ssh://, telnet://, rdp://, vnc:// and nexterm:// URL handlers; desktop shortcuts per session; file associations. | nice | backend-go+frontend | Windows `HKCU\Software\Classes`; macOS `.app` wrapper with CFBundleURLTypes; Linux `.desktop` plus xdg-mime; PWA protocol handlers; shortcuts to `http://127.0.0.1:<port>/#/open/<id>`. |

### 1.18 Infeasible and constrained features

| Feature | Reason | Closest alternative |
|---|---|---|
| SSH compression (SSH-26), sntrup761 (SSH-25), hmac-md5, umac, aes192/256-cbc | Not implemented in x/crypto | App-level compression; ML-KEM PQ KEX; mark the device unsupported (a fork would be possible) |
| Expired-password change message (SSH-40) | x/crypto lacks PASSWD_CHANGEREQ | Keyboard-interactive/PAM flow or in-shell `passwd` |
| Destination-constrained agent keys (SSH-41) | No `session-bind@openssh.com` | Confirm-on-use built-in agent |
| FIDO `ssh:` keys through browser WebAuthn (SSH-14) | The browser won't sign for the `ssh:` rpId; different signed payload | Agent-backed signing; `fido2` cgo tag |
| Embedded X server (PROTO-21) | No usable X server in a browser or pure Go | Xpra HTML5, Xvfb+VNC, local X server in desktop mode |
| Global hotkey / Quake window from a browser (UI-15) | Browsers can't register OS hotkeys | Tray-process hotkey (`ghotkey` tag) focusing the app window |
| Capturing Ctrl+W, T, N in a normal tab (TERM-19) | Reserved by the browser | PWA install; fullscreen Keyboard Lock (Chromium); alternate bindings |
| Dragging a folder out of the browser to the desktop (FILE-5) | No web API for this | Streamed ZIP; `showDirectoryPicker` direct write (Chromium) |
| RDP smart-card, serial, USB and WebAuthn redirection (GFX-13) and current-user SSO (GFX-15) | No browser access to PC/SC, ports, platform authenticators or login tokens | Native-client launch in desktop mode (CC-15) |
| VeNCrypt TLS or X509 with noVNC alone | A browser has no raw TLS socket | Go-side RFB security termination (§3.12) or guacd |
| VNC SASL auth; TRLE encoding | Not in noVNC | guacd VNC; server falls back to another encoding |
| VNC file transfer (UltraVNC or TightVNC) | Not in noVNC | SFTP companion (GFX-16) (CC-16) |
| Mosh predictive local echo in the browser | Mosh runs in the backend; the browser leg is TCP | Run NexTerm locally; xterm latency is low on localhost |
| Cygwin/MobApt toolset (TERM-35) | Windows-specific and huge | WSL, Git Bash, MSYS2, busybox-w32 profiles |
| Tunnel listeners on the viewer's machine in server mode | Listeners bind on the NexTerm host | "Open in browser" reverse proxy (TUN-8); run NexTerm locally |
| Font ligatures (TERM-6) | Ligatures addon needs Node APIs | Verify the xterm 6 feature path; otherwise leave off |
| Web Serial, Keyboard Lock, File System Access, Window Management, Local Fonts on Safari | Not implemented in Safari (Web Serial also absent in Firefox < 151) | Graceful fallbacks; recommend Chromium or Firefox |

---

## 2. Recommended Stack

**Toolchain:** `go 1.26` directive with `toolchain go1.27.1`. go-webauthn, mosh-go and ncruces-sqlite already need Go 1.26 or later. Resulting binaries need macOS 13+ and Windows 10+. Frontend: Node LTS, Vite 8 (Rolldown/Oxc) and React 19.3.

### 2.1 Disagreements between reports and how they were resolved

| Topic | Decision | Why |
|---|---|---|
| Editor | **CodeMirror 6** over Monaco | Measured size: Monaco 0.57 is 3.4 MB gz and 14.6 MB raw; CodeMirror with all lazy languages is 0.85 MB gz. Monaco's React wrapper loads from a CDN by default, which breaks offline use. `@codemirror/merge` covers diff. |
| Session tree | **@headless-tree/react** over react-arborist | react-arborist pins react-dnd 14, react-window 1 and redux. Headless-tree is headless, virtualizes with TanStack and is styled with shadcn. |
| PTY | **charmbracelet/x/xpty** | One API over Unix PTY and ConPTY, pure Go. creack/pty alone doesn't support Windows; go-pty pulls a huge dependency graph; UserExistsError/conpty has been idle since 2024. |
| WebSocket library | **coder/websocket** | Context-aware, concurrent writes, `NetConn()`. gorilla's last tag was 2024. |
| Transport layout | One multiplexed WebSocket per window for terminals and control; dedicated WebSockets for graphical streams; HTTP for files | Avoids the per-profile socket limit with many terminals. noVNC, IronRDP and Guacamole each insist on their own socket. HTTP gives Range and resume for free. |
| Keyword highlighting | Viewport decorations by default; Go SGR injection optional | Non-destructive: copy, logs, recordings and reattach stay exact, and cost stays bounded to visible rows. |
| ZMODEM | Server-side `xx25/go-zmodem`; `zmodem.js` as fallback | Works with server-owned sessions, detached browsers, serial ports and the HTTP transfer queue. |
| Recording format | asciicast **v3** native, v2 export | asciinema-player 3.17 plays v3; v3 adds exit events and relative intervals, which suit append-streaming. |
| RDP | IronRDP WASM + Go RDCleanPath by default; guacd opt-in | Keeps the single binary. guacd adds recording, drives, audio, RemoteApp and gateway. Pure-Go RDP libraries are immature and GPL. |
| VNC credentials | Go-side RFB security termination | Password stays server-side and VeNCrypt TLS becomes possible in the browser. |
| Headless VT for reattach | `charmbracelet/x/vt` (pinned) over `hinshun/vt10x` | vt10x is idle since 2022. x/vt is active and shares the `x/ansi` parser used for log stripping. |
| Hotkeys | **tinykeys** 4.0.1 plus our own scope layer | Tiny, framework-agnostic, fits the command registry. react-hotkeys-hook is acceptable too. |
| SSH server library (embedded server, bastion) | **charmbracelet/ssh** v0.4.3 | Same API as gliderlabs (v0.3.8, slow-moving) but actively maintained. |
| Self-update | **creativeprojects/go-selfupdate** v1.6.0 | Active (2026); minio/selfupdate was last tagged in 2022. |
| S3 SDK | aws-sdk-go-v2 | Also needed for SSM and EC2; gives the SSO/assume-role credential chain. minio-go lacks SSO. |
| Uploads | Hand-rolled offset-based resumable PUT | Streams directly into `sftp.File` with no temp file; tusd is heavier. |
| ACME | `x/crypto/acme/autocert` | Already a dependency; certmagic only if DNS-01 or wildcards are needed. |
| Docker client | `github.com/moby/moby/client` | `github.com/docker/docker` is frozen at v28.5.2+incompatible. |
| Kubernetes | client-go without the typed clientset | +9.5 MB instead of +23 MB stripped. |
| Lint and typecheck | TypeScript 7.0.2 (native `tsc`) + **oxlint** | typescript-eslint still needs the TS 6 API; oxlint avoids the dual install. |
| Unicode addon | `unicode11` by default; `unicode-graphemes` opt-in | graphemes 0.4 is experimental. |

### 2.2 Go modules

| Purpose | Module | Version | cgo-free | Rationale |
|---|---|---|---|---|
| HTTP, routing, CSRF, FS jails | `net/http` ServeMux, `http.CrossOriginProtection`, `os.Root` | stdlib | yes | Method and wildcard patterns are enough; chi v5.3.2 is the fallback |
| WebSocket | `github.com/coder/websocket` | v1.8.15 | yes | Context API, `NetConn` for relays. Raise `SetReadLimit` from 32 KiB; set `OriginPatterns`; subprotocol `guacamole` for the Guacamole tunnel |
| SSH, agent, known_hosts, Argon2, XChaCha | `golang.org/x/crypto` | v0.57.0 | yes | ML-KEM KEX, AuthCallback, NewControlClientConn, SupportedAlgorithms, autocert |
| known_hosts with algorithm negotiation | `github.com/skeema/knownhosts` | v1.3.3 | yes | Fixes false key-mismatch alarms; handles @cert-authority |
| ssh_config | `github.com/kevinburke/ssh_config` | v1.6.0 | yes | Include support; use `ssh -G` for Match |
| PPK import | `github.com/kayrus/putty` | v1.0.5 | yes | PPK v2 and v3; own writer for export |
| Windows pipes and agent | `github.com/Microsoft/go-winio` | v0.6.2 | yes | OpenSSH agent pipe, Docker npipe |
| Pageant | `github.com/davidmz/go-pageant` | v1.0.2 | yes | WM_COPYDATA protocol |
| NTLM proxies | `github.com/Azure/go-ntlmssp` | v0.1.1 | yes | CONNECT with NTLM |
| Kerberos | `github.com/alexbrainman/sspi` (Windows); `github.com/jcmturner/gokrb5/v8` | 2025-09 pseudo; v8.4.4 | yes | SSPI for AD; gokrb5 reads FILE: ccaches only |
| DNS (SSHFP, lookup tool) | `github.com/miekg/dns` | v1.1.73 | yes | |
| SFTP client and server | `github.com/pkg/sftp` | v1.13.11 | yes | Avoid v2 (alpha) |
| SCP | `github.com/bramvdbogaerde/go-scp` | v1.6.1 | yes | Single files; -r/-p in-house |
| FTP/FTPS client | `github.com/jlaffaye/ftp` | v0.2.4 | yes | Passive only; active mode hand-rolled |
| Serial | `go.bug.st/serial` | v1.8.0 | core yes | Enumerator on darwin needs cgo (`serialusb` tag) |
| PTY and ConPTY | `github.com/charmbracelet/x/xpty` | v0.1.4 | yes | Pulls creack/pty v1.1.24 and x/conpty |
| Headless VT (reattach snapshots) | `github.com/charmbracelet/x/vt` | pseudo 2026-09-27 (pin) | yes | Experimental; pin a commit |
| ANSI parse and strip | `github.com/charmbracelet/x/ansi` | v0.11.8 | yes | Log stripping, SGR highlighter |
| Charsets | `golang.org/x/text` | v0.42.0 | yes | Legacy encodings |
| Misc x/ packages | `golang.org/x/net` v0.59.0 (proxy, icmp, html), `x/sys` v0.48.0, `x/time` v0.16.0, `x/sync` v0.23.0 | — | yes | |
| Database | `modernc.org/sqlite` | v1.59.0 (SQLite 3.53.4) | yes | Pure Go, FTS5, WAL; one writer connection. `jackc/pgx/v5` v5.11.0 optional for server/HA |
| OS keychain | `github.com/zalando/go-keyring` | v0.2.8 | yes | |
| TOTP | `github.com/pquerna/otp` | v1.5.0 | yes | |
| WebAuthn | `github.com/go-webauthn/webauthn` | v0.18.2 | yes | Pre-1.0: pin |
| SOCKS5 server | `github.com/things-go/go-socks5` | v0.1.3 | yes | Maintained fork of armon |
| Mosh | `github.com/unixshells/mosh-go` | v0.5.2 | yes | Young project: pin and test against mosh-server 1.4 |
| ZMODEM | `github.com/xx25/go-zmodem` | pseudo 2026-09-10 (pin) | yes | Tested against lrzsz |
| Embedded servers | `pin/tftp/v3` v3.2.0; `fclairamb/ftpserverlib` v0.32.4 + `spf13/afero` v1.15.0; `charmbracelet/ssh` v0.4.3; `willscott/go-nfs` v0.0.4 | — | yes | |
| Discovery and ping | `hashicorp/mdns` v1.0.7; `prometheus-community/pro-bing` v0.9.1 | — | yes | Unprivileged ping needs `ping_group_range` on Linux |
| Host metrics | `github.com/shirou/gopsutil/v4` | v4.26.8 | yes | purego on darwin |
| Packet decode | `github.com/gopacket/gopacket` | v1.7.3 | pcapgo yes; pcap no | Local live capture only under the `pcap` tag |
| S3, SSM, EC2 | `aws-sdk-go-v2` `service/s3` v1.113.4, `config` v1.33.6, `feature/s3/transfermanager` v0.4.10, `service/ssm` v1.78.1, `service/ec2` v1.336.1, `service/ec2instanceconnect` v1.42.0 | — | yes | +~4.3 MB; excluded by the `lite` tag |
| SSM sessions | `github.com/mmmorris1975/ssm-session-client` | v0.500.1 | yes | Pulls session-manager-plugin and gorilla 1.4.2 |
| Docker | `github.com/moby/moby/client` v0.6.0 + `moby/moby/api` v1.56.0 | — | yes | +~2.4 MB (otel) |
| Kubernetes | `k8s.io/client-go` + `k8s.io/api` | v0.37.1 | yes | clientcmd, rest, remotecommand, portforward only |
| WebDAV, SMB | `studio-b12/gowebdav` v0.13.0; `hirochachacha/go-smb2` v1.1.0 | — | yes | |
| Proxmox | `github.com/luthermonson/go-proxmox` | v0.8.1 | yes | |
| Scripting | `github.com/dop251/goja` (pin); `github.com/yuin/gopher-lua` v1.1.2 | — | yes | |
| Scheduling | `github.com/robfig/cron/v3` | v3.0.1 | yes | Stable parser; gocron v2.22.0 as an alternative |
| File watching | `github.com/fsnotify/fsnotify` | v1.10.1 | yes | |
| Log rotation | `gopkg.in/natefinch/lumberjack.v2` | v2.2.1 | yes | |
| Compression (recordings) | `github.com/klauspost/compress` (zstd) | v1.20.1 | yes | |
| Backup encryption | `filippo.io/age` | v1.3.2 | yes | |
| INI (MobaXterm) | `gopkg.in/ini.v1` | v1.67.3 | yes | |
| KeePass | `github.com/tobischo/gokeepasslib/v3` | v3.7.0 | yes | |
| CLI | `github.com/spf13/cobra` | v1.10.2 | yes | |
| Service install | `github.com/kardianos/service` | v1.3.0 | yes | |
| Open browser | `github.com/pkg/browser` | pseudo 2024-01-02 | yes | |
| Tray / global hotkey | `fyne.io/systray` v1.12.2; `golang.design/x/hotkey` v0.6.4 | — | no (darwin) | `tray` and `ghotkey` tags |
| Self-update | `github.com/creativeprojects/go-selfupdate` | v1.6.0 | yes | |
| Precompressed static files | `github.com/vearutop/statigz` | v1.5.0 | yes | |
| OIDC, LDAP, SAML, JOSE | `coreos/go-oidc/v3` v3.21.0 + `x/oauth2`; `go-ldap/ldap/v3` v3.4.14; `crewjam/saml` v0.5.1; `go-jose/go-jose/v4` v4.1.5; `zitadel/oidc/v3` v3.51.6 (IdP) | — | yes | Server mode |
| OpenAPI | `github.com/danielgtaylor/huma/v2` | v2.39.1 | yes | Typed REST with OpenAPI 3.1 |
| Metrics | `github.com/prometheus/client_golang` | v1.24.1 | yes | |
| JSON Schema | `github.com/invopop/jsonschema` | v0.14.0 | yes | Settings forms and validation |
| Gateway agents | `github.com/hashicorp/yamux` | v0.1.2 | yes | |
| Plugins (WASM) | `github.com/tetratelabs/wazero` | v1.12.0 | yes | |
| MCP server | `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | yes | |
| Bastion TUI | `github.com/charmbracelet/bubbletea/v2` | v2.0.10 | yes | |
| Mail, push, GeoIP | `wneessen/go-mail` v0.8.1; `SherClockHolmes/webpush-go` v1.4.0; `oschwald/maxminddb-golang/v2` v2.6.0 | — | yes | |
| Clustering | `nats-io/nats-server/v2` | v2.15.0 | yes | Optional |
| Completeness extras (§4) | `gosnmp/gosnmp` v1.45.0; `masterzen/winrm` (pseudo 2026-04); `bougou/go-ipmi` v0.9.1 | — | yes | |
| Optional hardware (tags) | `go-piv/piv-go/v2` v2.6.0; `ThalesIgnite/crypto11` v1.6.8; `keys-pub/go-libfido2` v1.5.3; `google/go-tpm` v0.9.8 (pure Go) | — | mostly no | `piv`, `pkcs11`, `fido2` tags |
| Dev logging | `github.com/lmittmann/tint` | v1.2.0 | yes | Colored console output in development |

**Rejected Go options:** gorilla/websocket (stale); github.com/docker/docker (frozen); pkg/sftp/v2 (alpha); aymanbagabas/go-pty (heavy dependency graph); grdp and other pure-Go RDP (GPL, immature); 99designs/keyring (cgo on macOS); casbin (a bitset is enough); trzsz-go server-side filter (native dialogs; the frontend `trzsz` is used instead); guacd embedded (C, impossible).

### 2.3 npm packages

| Purpose | Package | Version | Rationale |
|---|---|---|---|
| UI runtime | `react`, `react-dom`, `@types/react` | 19.3.0 | All chosen UI libraries declare React 19 peer support |
| Build | `vite`, `@vitejs/plugin-react` | 8.3.1, 6.1.1 | Keep the default target (≥ es2022) for noVNC's top-level await |
| Types and lint | `typescript`, `oxlint` | 7.0.2, 1.85.0 | TS 7 bans `baseUrl`, so use `paths` for shadcn's `@/*` |
| Terminal | `@xterm/xterm` | 6.0.0 | Sync output, OSC 52, ESM. Canvas addon removed |
| xterm addons | `@xterm/addon-fit` 0.11.0, `-webgl` 0.19.0, `-search` 0.16.0, `-web-links` 0.12.0, `-unicode11` 0.9.0, `-serialize` 0.14.0, `-clipboard` 0.2.0, `-image` 0.9.0, `-progress` 0.2.0; opt-in `-unicode-graphemes` 0.4.0 | — | ~166 KB gz for the core set. `addon-ligatures` 0.10.0 experimental (Node deps); never install `addon-canvas` |
| In-terminal transfer | `trzsz` 1.1.6; fallback `zmodem.js` 0.1.10 | — | |
| Editor and diff | `@uiw/react-codemirror` 4.25.12, `codemirror` 6.0.2, `@codemirror/view` 6.43.13, `@codemirror/state` 6.7.6, `@codemirror/language-data` 6.5.2, `@codemirror/merge` 6.12.2, `@codemirror/search` 6.7.2, opt `@replit/codemirror-vim` 6.4.0 | — | 0.85 MB gz with lazy languages |
| Docking | `dockview-react` | 8.3.1 | Install `dockview-react`, not `dockview`. CSS at `dockview-react/dist/styles/dockview.css` |
| Shell splits | `react-resizable-panels` | 4.14.1 | v4 API: Group, Panel, Separator |
| Tree | `@headless-tree/react` + `@headless-tree/core` | 1.7.0 | |
| State | `zustand` 5.0.15; `@tanstack/react-query` 5.104.0 | — | Terminal data never goes through React state |
| Virtualization and tables | `@tanstack/react-virtual` 3.14.13; `@tanstack/react-table` 9.2.4 | — | |
| UI kit | `tailwindcss` + `@tailwindcss/vite` 4.3.3; `shadcn` CLI 4.21.0; `radix-ui` 1.6.7; `clsx` 2.1.1; `tailwind-merge` 3.7.0; `class-variance-authority` 0.7.1; `tw-animate-css` 1.4.0 | — | |
| Palette, icons, toasts | `cmdk` 1.1.1; `lucide-react` 1.48.0 (named imports only); `sonner` 2.0.8 | — | |
| Drag and drop | `@dnd-kit/core` 6.3.1, `@dnd-kit/sortable` 10.0.0 | — | Tab strip, algorithm lists, macro steps |
| Diagrams | `@xyflow/react` | 12.12.0 | Tunnel and jump-chain diagrams (lazy-loaded) |
| Shortcuts | `tinykeys` | 4.0.1 | |
| Forms | `react-hook-form` 7.89.0, `zod` 4.6.5, `@hookform/resolvers` 5.9.1 | — | |
| Search | `fuse.js` | 7.5.0 | |
| Markdown | `react-markdown` 10.1.0, `remark-gfm` 4.0.1 | — | Notes and docs |
| i18n | `i18next` 26.4.2, `react-i18next` 17.0.15 | — | |
| PWA | `vite-plugin-pwa` | 1.3.0 | Network-first index |
| Precompression | `vite-plugin-compression2` 2.5.3; dev `rollup-plugin-visualizer` 7.1.1 | — | |
| Charts | `uplot` 1.6.32 (+ `uplot-react` 1.2.4) | — | Tiny streaming charts. recharts rejected (size) |
| Recording player | `asciinema-player` | 3.17.0 | Plays v1, v2 and v3; mounted imperatively |
| VNC | `@novnc/novnc` 1.7.0 (+ `@types/novnc__novnc` 1.6.0) | — | Native ESM; lazy `import()` |
| RDP (IronRDP) | `@devolutions/iron-remote-desktop` 0.11.0, `@devolutions/iron-remote-desktop-rdp` 0.7.0 | — | 2.2 MB gz WASM, lazy-loaded; pin exact versions |
| Guacamole client | vendored official `guacamole-common-js` **1.6.0** (Maven Central zip, ESM wrapper) + `@types/guacamole-common-js` 1.5.5 | — | The npm package is a stale 1.5.0 community repack |
| WebAuthn | `@simplewebauthn/browser` | 14.0.0 | |
| Fonts | `@fontsource-variable/jetbrains-mono` 5.3.0 + vendored Nerd Font Symbols woff2 (MIT/OFL) | — | Offline |
| Viewers and utilities | `react-zoom-pan-pinch` 4.2.0; `hash-wasm` 4.12.0; `papaparse` 5.7.0 | — | Picture viewer, local hashes, CSV import preview |
| Codegen and test | `openapi-typescript` 7.13.0; `vitest` 5.0.2; `@playwright/test` 1.63.0 | — | Playwright drives the real embedded binary |

**Rejected npm options:** Monaco (size, CDN loader); react-arborist (old dependencies); recharts (size); `@xterm/addon-canvas` (removed); npm `guacamole-common-js` 1.5.0 (stale); `pdfjs-dist` (use the browser's native PDF viewer).

---

## 3. Protocol Strategies

### 3.1 Browser-to-backend transport
- **Terminal and control socket**, one per browser window at `/ws`. Frame = `[type:u8][chan:u32][payload]`. Types: `0x00` data (raw bytes), `0x01` resize (cols, rows, pxW, pxH as u16/u32), `0x02` ack (u32 bytes consumed), `0x03` JSON control (attach, detach, prompts, status, events), `0x04` input for a broadcast target set.
- **Attach** sends `{attach, id, fromOffset}`. The server replies with the delta, or with `reset` plus a VT snapshot.
- **Flow control:** the client acks after `term.write(data, cb)` every 64 KiB. The server stops reading the channel above 1 MiB unacked and resumes below 256 KiB. Output is flushed at 8 ms or 32 KiB. permessage-deflate is optional and only for remote deployments.
- **Dedicated sockets:**
  - `/ws/vnc/{token}`: binary, no subprotocol.
  - `/ws/rdp`: RDCleanPath, binary, no subprotocol.
  - `/ws/guac?{token,w,h,dpi,audio,image}`: subprotocol **`guacamole`**, text frames.
- **Files:** `PUT /api/fs/{conn}/upload?path&offset` and `GET …/download` with Range.
- **Auth:** every socket authenticates by session cookie plus a strict Origin check, and every WebSocket raises `SetReadLimit` above 32 KiB. Liveness uses WS ping every 20 s.

### 3.2 SSH
- **Library:** x/crypto/ssh v0.57.0.
- **Dial order:**
  1. Resolve the Dialer: proxy (SOCKS, HTTP, ProxyCommand), then the jump chain (`prev.DialContext` → `NewClientConn` → `NewClient` per hop, pooled and ref-counted).
  2. `conn.SetDeadline(handshake)` with a goroutine that closes the conn on ctx cancel.
  3. `NewClientConn` with:
     - `HostKeyCallback`: skeema/knownhosts DB, with the TOFU prompt via the broker.
     - `HostKeyAlgorithms` taken from the stored key types.
     - `AuthCallback` ordering gssapi, publickey, KI and password, with lazy prompts.
     - A single `PublicKeysCallback` merging file, agent and vault keys, in MaxAuthTries order.
     - `BannerCallback`.
  4. Clear the deadline.
- **Channels on the shared client:**
  - PTY session: `RequestPty` (height first; a manual `pty-req` for real pixel sizes), then `x11-req` and agent forwarding, then `Setenv`, then `Shell` or `Start`.
  - SFTP subsystem.
  - Monitor exec loop.
  - direct-tcpip for tunnels and relays.
  - `forwarded-tcpip`, `x11` and `auth-agent@openssh.com` handlers, registered **once** per client.
- **MaxSessions:** on exhaustion, open a second connection transparently.
- **Keepalive:** `keepalive@openssh.com` with wantReply, in a goroutine with a timeout. After CountMax misses, close and reconnect.
- **Legacy devices:** algorithm lists come from `InsecureAlgorithms()`; on failure, retry progressively and cache what worked per host.
- **Negotiated algorithms:** `AlgorithmsConnMetadata`, shown with a PQ badge.

### 3.3 SFTP, SCP and shell fallback
- **SFTP client options:** `sftp.NewClient(c, UseConcurrentReads(true), UseConcurrentWrites(true), MaxConcurrentRequestsPerFile(64))`.
- **Uploads:** feed an `io.LimitedReader` so concurrent writes engage, and `PosixRename` from a `.part` file.
- **Deletes:** a custom Lstat-based recursive delete.
- **Owner names:** resolved via `getent`.
- **Fallback when `subsystem request failed`:**
  1. SCP over exec. Download: `scp -f -- p` (read `C0644 size name`, ack 0x00, read data and a trailing 0x00). Upload: `scp -t -- dir` (`T` mtime line, `C`, data, 0x00; `D`/`E` for -r).
  2. Busybox mode: `ls -la` parsing, `cat` or `base64` for download, chunked `base64 -d >>` for upload.
- **Root access:** `sudo -S <sftp-server>` with `sftp.NewClientPipe`.

### 3.4 Telnet
- **State machine:** hand-rolled IAC handling of WILL/WONT/DO/DONT.
- **Options accepted:** BINARY (0), ECHO (1), SGA (3), TTYPE (24; answer from the TERM setting), NAWS (31; `IAC SB 31 w16 h16 IAC SE` on every resize, doubling 0xFF), NEW-ENVIRON (39, USER).
- **Refused:** LINEMODE (34), unless line mode is on.
- **Data handling:** outgoing 0xFF is doubled. CR is sent as CR NUL outside BINARY mode, and the Return mapping is configurable.
- **Options:** optional TLS (992) via `crypto/tls`; charset transcoding from x/text; auto-login expect on `login:` and `Password:`.
- **Echo:** echo toggling drives local-echo suppression so password prompts stay hidden.

### 3.5 Rlogin and Rsh
- **Rlogin:** dial 513 from a privileged port (<1024, requires root or CAP_NET_BIND_SERVICE). Send `\0 localuser\0 remoteuser\0 term/38400\0` and expect `\0`.
- **Rlogin window size:** send `FF FF 73 73` followed by rows, cols, xpx, ypx as big-endian u16, both proactively and on resize. The server's OOB window-size request can't be read with `net.Conn`, so either set `SO_OOBINLINE` via `RawConn.Control` or rely on the proactive sends.
- **Rsh:** port 514 with the stderr-port handshake.
- Both are plaintext, carry a warning, and admins can disable them.

### 3.6 Raw TCP, TLS and UDP
- `net.Dialer` or `tls.Client` (insecure toggle), or UDP.
- Line discipline runs entirely client-side (echo, CRLF, hex).
- Routed through the Dialer; ACL-enforced in server mode.

### 3.7 Serial and Web Serial
- **Host ports** via `go.bug.st/serial` v1.8.0:
  - `Open(&Mode{BaudRate, DataBits, Parity, StopBits})`, plus `Break`, `SetDTR`, `SetRTS` and modem status bits for line-status indicators.
  - Port list: `enumerator.GetDetailedPortsList` on linux, windows and darwin with cgo; otherwise `serial.GetPortsList`. Poll for hotplug and reconnect.
- **Viewer's own ports:** `navigator.serial` in the browser (Chromium, and Firefox 151+ desktop), piped into xterm. The backend only receives logs.
- **Extras:** XMODEM, YMODEM and Kermit implemented in Go on the stream; hex view is a teed stream.

### 3.8 Mosh
1. Over SSH exec: `mosh-server new -s -c 256 -l LANG=en_US.UTF-8 [-p 60000:61000]`.
2. Parse `MOSH CONNECT <port> <key>`, then close the exec session.
3. `mosh.Dial(host, port, key)` (unixshells/mosh-go): `Recv` gives ANSI output for xterm, `Send` carries keys, `Resize` follows fit.

- UDP 60000–61000 must be reachable from the backend.
- Roaming benefits only the backend↔server leg.
- Fallback: the system `mosh-client` in xpty, where installed.

### 3.9 Local shell and WSL
- **PTY:** `xpty.NewPty(w, h)` → `Start(cmd)`, with `Resize` and `WaitProcess` (needed on Windows).
- **Unix:** `$SHELL -l`. **Windows:** ConPTY (Windows 10 1809+) running pwsh, powershell, cmd, Git Bash or `wsl.exe -d <d> --cd <dir>`.
- **WSL distros:** read from `HKCU\Software\Microsoft\Windows\CurrentVersion\Lxss` (`DistributionName`, `BasePath`, `Version`).
- **ConPTY caveat:** it re-renders the VT stream, so small escape and mouse differences are expected.
- **Server mode:** disabled or admin-only.

### 3.10 RDP

**Path A (default, single binary): IronRDP WASM in the browser plus a Go RDCleanPath relay (~300 LOC, stdlib only)**
- **Frontend:**
  - Lazy-load `@devolutions/iron-remote-desktop` and `-rdp`, then register `<iron-remote-desktop>` (attributes `scale`, `flexcenter`).
  - Configure with `ConfigBuilder`: username, password, destination, `proxyAddress` (wss URL), `authToken` (one-time), `desktopSize`, `enableCredssp`, optional `kdcProxyUrl` (Kerberos) and `preConnectionBlob` (Hyper-V).
  - Features: `displayControl` for dynamic resize, CLIPRDR text and files, print callbacks.
  - Not in the published API: audio and drive redirection.
- **Initial PDU:** the first WS message is DER `RDCleanPathPdu ::= SEQUENCE`, every field `EXPLICIT` and optional except the version:

  | Tag | Field | Type |
  |---|---|---|
  | [0] | version | INTEGER = 3390 (3389+1) |
  | [1] | error | RDCleanPathErr |
  | [2] | destination | UTF8String |
  | [3] | proxy_auth | UTF8String; carries the `authToken` |
  | [4] | server_auth | unused |
  | [5] | preconnection_blob | |
  | [6] | x224_connection_pdu | OCTET STRING |
  | [7] | server_cert_chain | SEQUENCE OF OCTET STRING |
  | [9] | server_addr | UTF8String |

  `RDCleanPathErr ::= { error_code [0] (1 general, 2 negotiation), http_status_code [1], wsa_last_error [2], tls_alert_code [3] }`. In Go, use `asn1:"tag:N,explicit,optional"`.
- **Relay steps:**
  1. Verify the version and the `proxy_auth` token. The token is single-use, short-lived and **bound server-side to one destination**; NetBird's reference trusts `pdu.Destination`, which makes an open relay, so do not.
  2. Dial the target through the Dialer (jump chain, SSRF guard).
  3. Write `x224_connection_pdu`, then read the X.224 Connection Confirm by parsing the TPKT header (`03 00 len16`) and reading exactly `len` bytes.
  4. Run `tls.Client` with `InsecureSkipVerify` and your own `VerifyConnection` that checks the certificate fingerprint via TOFU and the prompt broker. Cap `MaxVersion` at TLS 1.2 when the selected protocol includes HYBRID or HYBRID_EX, as NetBird does.
  5. Reply `{version, x224_connection_pdu = confirm, server_cert_chain = DER certs, server_addr}`.
  6. Pump WS binary frames to and from the TLS plaintext.
  7. On failure, reply with the `error` field (WSA 10060/10061, TLS alert).
- **Division of work:** the browser runs CredSSP/NLA, using the returned certificate chain for public-key binding, and all of RDP.
- **Very old servers:** Go ≥1.22 disables RSA key exchange, and Go 1.27 removed the `tlsrsakex` GODEBUG. Explicitly list `TLS_RSA_*` suites and `MinVersion: tls.VersionTLS10` per connection when a "legacy RDP" toggle is set (verify on Go 1.27).
- **Trade-offs:** credentials reach the browser, but only at connect time, over the authenticated WebSocket, and are never persisted. There is no server-side recording or sharing. The WASM is 2.2 MB gz. The API is 0.x, so pin versions.

**Path B (full-fidelity, opt-in): guacd 1.6.0** (FreeRDP 3, new display optimizer, RDP certificate fingerprint validation). Used when credentials must stay server-side, or for recording, sharing, audio in and out, drive, printing, RemoteApp or RD Gateway.
- **Parameters:** `hostname`, `port`, `username`, `password`, `domain`, `security` (`any`/`nla`/`nla-ext`/`tls`/`rdp`/`vmconnect`), `ignore-cert`, `resize-method=display-update`, `console`, `color-depth`, `server-layout`, `timezone`, `enable-drive`, `drive-path`, `create-drive-path`, `enable-audio-input`, `disable-audio`, `enable-printing`, `remote-app`, `remote-app-dir`, `remote-app-args`, `gateway-hostname`, `gateway-port`, `gateway-username`, `gateway-password`, `load-balance-info`, `preconnection-blob`, `recording-path`, `recording-name`, `recording-include-keys`, `disable-copy`, `disable-paste`, `disable-upload`, `disable-download`, `wol-send-packet`, `wol-mac-addr`. Performance flags: `enable-wallpaper`, `enable-font-smoothing`, `enable-desktop-composition`, and others.
- **Deployment:** guacd is Linux C, so it runs as a Docker sidecar that NexTerm can manage (CORE-16), or under WSL on Windows.

**Rejected (C): pure-Go RDP.** tomatome/grdp and nakagami/grdp are GPL-3.0. rcarmo/go-rdp and gopher-rdp are tiny projects. kulaginds/rdp-html5 is idle. None has production NLA, GFX/H.264, clipboard, audio or drive support.

### 3.11 Guacamole protocol (guacd path, for RDP, VNC, SSH, telnet or kubernetes via guacd)
- **Encoding:** instructions look like `LEN.VALUE,LEN.VALUE,…;`. **LEN counts Unicode code points** (`utf8.RuneCountInString`), not bytes.
- **Handshake (Go to guacd TCP 4822):**
  1. Send `6.select,3.rdp;`, or `6.select,37.$<connection-id>;` to join or shadow an existing connection.
  2. Receive `4.args,13.VERSION_1_5_0,8.hostname,…;`. If the first element is not a `VERSION_x_y_z` string, treat the server as 1.0.0.
  3. Send `4.size,<w>,<h>,<dpi>;`.
  4. Send `5.audio,<mimetypes…>;` (from `Guacamole.AudioPlayer.getSupportedTypes()`).
  5. Send `5.video;`.
  6. Send `5.image,9.image/png,10.image/jpeg,10.image/webp;`.
  7. Send `8.timezone,<IANA>;` (1.1.0+).
  8. Send `4.name,<user>;` (1.5.0+).
  9. Send `7.connect,13.VERSION_1_5_0,<one value per advertised arg, in order; 0. for empty>;`. Echo the lower of the two versions.
  10. Receive `5.ready,37.$<uuid>;`, or `5.error,<msg>,<code>;`.
- **Version notes:** 1.3.0 added `required` (mid-session credential prompts answered with argv streams). 1.5.0 added `msg` and `name`. The 1.6.0 docs still top out at VERSION_1_5_0. In 1.6.0, `sync` carries a frame count.
- **WebSocket tunnel, as `Guacamole.WebSocketTunnel` (Tunnel.js) expects:**
  - The browser opens `new WebSocket(url + '?' + data, 'guacamole')`. `data` is whatever was passed to `client.connect()`; put a one-time token, width, height, dpi and supported audio/image types there so Go can build the handshake. The browser never talks to guacd directly.
  - The server **must echo the `guacamole` subprotocol**.
  - Use **text frames only**, each containing whole instructions; never split an instruction across frames.
  - The first server instruction must be the internal-opcode UUID `0.,36.<tunnel-uuid>;`. The tunnel stays CONNECTING until then.
  - The client sends `0.,4.ping,<ms>;` every 500 ms. Echo it, and never forward internal-opcode messages to guacd.
  - Client timers are 1500 ms (the tunnel goes UNSTABLE) and 15000 ms (receive timeout), so traffic must keep flowing; guacd `sync` plus the ping echoes cover it.
  - Forward the client's `4.sync,<ts>;` to guacd.
  - To close with an error, put a Guacamole status code in the WebSocket close reason (for example `519` UPSTREAM_NOT_FOUND, `769` CLIENT_UNAUTHORIZED).
- **Go-side filtering:** parse instructions so policy can drop `clipboard`, `file` and `pipe` streams and audit `put`/`get`. Shares and admin watchers are extra guacd users joined via `select $id` with read-only arguments.
- **Playback:** `Guacamole.SessionRecording`.
- **Client packaging:** vendor the official guacamole-common-js 1.6.0 (Apache-2.0) behind an ESM wrapper (`export default Guacamole`).

### 3.12 VNC
- **Frontend:** `const { default: RFB } = await import('@novnc/novnc')`.
  - 1.7.0 is native ESM (`exports ./core/rfb.js`).
  - `core/util/browser.js` has a top-level await (the WebCodecs H.264 probe). Keep Vite `build.target` at its default or ≥ es2022, **never** emit it in iife, umd or worker format. Vite issue #22314 hit the 1.6.0 CJS build under Rolldown; 1.7.0 builds and pre-bundles cleanly on Vite 8.3.1 (~59 KB gz).
  - `wsProtocols` defaults to `[]`, so the Go endpoint must not require a subprotocol.
  - RFB options: `scaleViewport`, `clipViewport`, `resizeSession`, `viewOnly`, `qualityLevel`, `compressionLevel`, `clipboardPasteFrom`, and the `clipboard`, `credentialsrequired` and `disconnect` events. RFB can take a custom channel object instead of a URL, if streams are multiplexed later.
  - A secure context is required (localhost or HTTPS).
  - `@types` lags at 1.6, so add local d.ts augmentations.
  - Licensing: MPL-2.0 is file-level copyleft, so publish any modified noVNC files.
- **Backend relay:** websockify equivalent. `websocket.NetConn` ↔ TCP 5900+N (or `ssh.Client.Dial` through a gateway), `io.Copy` both ways.
- **Recommended: Go-side RFB security termination.**
  1. Go reads `RFB 003.008` and the server's security types.
  2. It authenticates with vault credentials. VNCAuth: DES over the 16-byte challenge, using the password's bit-reversed bytes as the key (`crypto/des`). VeNCrypt: version 0.2 and subtype, then **real TLS** via `crypto/tls` for TLSVnc, X509Vnc and X509Plain, then the inner auth.
  3. Go reads SecurityResult.
  4. Go presents `RFB 003.008` with security type list `[1 None]` and SecurityResult OK to noVNC.
  5. Go relays ClientInit and ServerInit onward, inside the TLS tunnel where applicable.

  The password never reaches the browser, and VeNCrypt TLS, which a browser can't do alone, works. Types Go doesn't implement (ARD, MSLogonII, RA2ne, UnixLogon) pass through, and noVNC handles them with credentials sent on `credentialsrequired`.
- **Limits:** SASL and TRLE are unsupported, so use guacd or let the server fall back.
- **Reattach:** reconnect, since the server keeps the desktop.
- **Recording (without guacd):** tee the post-handshake server-to-client stream with timestamps and replay it into noVNC through a fake socket.

### 3.13 X11 and XDMCP
- **Mode 1: remote apps in the browser (Xpra).**
  - Over SSH, exec `xpra start --bind-ws=127.0.0.1:0 --html=on --start-child=<app> --exit-with-children`, or `xpra seamless` / `start-desktop`.
  - Parse the port and reach it via direct-tcpip.
  - Reverse-proxy xpra's own HTML5 client under `/x11/{id}/` or `<id>.localhost`, including the WebSocket upgrade.
  - Keep Xpra bound to loopback.
- **Mode 2: full virtual desktop.** Start Xvnc/TigerVNC, or Xvfb with x11vnc, over SSH, then use §3.12.
- **Mode 3: classic forwarding** (desktop mode with a local X server).
  1. Send `x11-req` with `{single=false, "MIT-MAGIC-COOKIE-1", hex(16 random), screen 0}`.
  2. `HandleChannelOpen("x11")`, then dial `/tmp/.X11-unix/X<n>`, the XQuartz socket, or `127.0.0.1:6000+n`.
  3. Parse the X11 setup packet (byte order, auth-name and auth-data lengths, 4-byte padding), verify the fake cookie and substitute the real one from `xauth list $DISPLAY`.
  4. Pipe both ways. Untrusted cookies by default.
- **XDMCP:** Linux or macOS host spawns `Xephyr :N -query host` (or broadcast), then shows it via x11vnc and §3.12 or Xpra.

### 3.14 FTP / FTPS
- **Connect:** `jlaffaye/ftp` `DialWithExplicitTLS` (AUTH TLS) or `DialWithTLS` (implicit, 990). Pass a `tls.Config` with `ClientSessionCache` for servers that require data-channel TLS session reuse (vsftpd `require_ssl_reuse`), and fall back to TLS 1.2 max if resumption fails.
- **Data channel:** EPSV then PASV, with `DialWithDisabledEPSV` and trust-PASV-IP toggles for NAT'd servers.
- **Listings and resume:** MLSD (can be disabled), `RetrFrom` and `StorFrom` resume, NOOP every ~60 s.
- **Active mode:** PORT/EPRT hand-rolled when needed, because the library is passive-only.
- **Transcoding:** ASCII mode transcodes line endings.

### 3.15 S3, WebDAV, SMB
- **S3:**
  - `config.LoadDefaultConfig` gives profiles, SSO, assume-role and `credential_process`.
  - S3-compatible stores: `BaseEndpoint`, `UsePathStyle`, and `RequestChecksumCalculation`/`ResponseChecksumValidation = WhenRequired`.
  - `ListObjectsV2` paginator with `/` delimiter for folders.
  - transfermanager multipart uploads fed by browser chunks; streaming `GetObject`; presigned URLs for "copy link" and optional direct browser-to-S3 uploads.
- **WebDAV and SMB:** gowebdav and go-smb2 VFS adapters.

### 3.16 Docker
- **Client:** `client.New(client.FromEnv, client.WithAPIVersionNegotiation())`.
- **Exec:** `ContainerExecCreate{Tty:true, Cmd: ["/bin/sh","-c","command -v bash >/dev/null && exec bash \|\| exec sh"]}`, then `ExecAttach` (hijacked conn) to the WebSocket, with `ExecResize` on fit. Without a TTY, demux with stdcopy.
- **Other features:** logs (follow), stats for charts, and `CopyFromContainer`/`CopyToContainer` tar streams for a container VFS.
- **Remote engines:** a custom `DialContext` returns `sshClient.Dial("unix","/var/run/docker.sock")` (Podman compatible).
- **Windows:** `npipe:////./pipe/docker_engine`.
- **Access control:** RBAC-gated, because the socket is effectively root.

### 3.17 Kubernetes
- **Config:** `clientcmd` loads kubeconfig and contexts.
- **Exec:** `remotecommand.NewFallbackExecutor(NewWebSocketExecutor(cfg,"GET",url) /* v5.channel.k8s.io */, NewSPDYExecutor(...), httpstream.IsUpgradeFailure)` with a `TerminalSizeQueue` fed by resize frames.
- **Pod pickers:** REST or dynamic client. No typed clientset (saves ~14 MB).
- **Port-forward:** `tools/portforward` for the tunnel manager.
- **Credentials:** exec credential plugins must exist on the host. Rebuild the executor on 401, since tokens expire.
- **Size-sensitive builds:** the `lite` tag drops K8s.

### 3.18 AWS SSM and cloud CLIs
- **Library:** ssm-session-client with the SDK config.
- **Session types:** `ShellSession` or `ShellPluginSession` (KMS) streamed to xterm; `PortForwardingSession` for tunnels, including RDP and VNC to private instances; `SshSession` for SSH over SSM.
- **Targeting:** `ResolveTarget` accepts a tag, IP or DNS name; optional EC2 Instance Connect key push.
- **Limits:** Windows-target shells are a TODO upstream. Sessions idle out server-side after about 20 min, so surface reconnects.
- **GCP IAP, Azure Bastion and Cloudflare Access:** ProxyCommand templates using the vendor CLIs.

### 3.19 HTTP: web sessions, tunnel "Open in browser", Xpra, web assets
- **Proxy:** `httputil.ReverseProxy{Rewrite, Transport: &http.Transport{DialContext: sshClient.DialContext}}`.
- **WebSockets:** upgrades pass through natively.
- **Routing:** host-based `<port>-<id>.localhost:<nextermPort>`, so each proxied app is its own origin, isolated from NexTerm cookies. Server mode needs wildcard DNS and TLS.
- **Framing:** strip `X-Frame-Options` and CSP `frame-ancestors` only for iframe tabs.
- **Auth:** a scoped auth cookie is required and stripped before forwarding.
- **Links:** HTML rewriting with `x/net/html` only when using path prefixes.

### 3.20 In-terminal file transfer
- **ZMODEM:**
  - Go scans PTY output for `**\x18B00` (the remote ran `sz`) and `**\x18B01` (the remote ran `rz`), pauses xterm forwarding, and runs `xx25/go-zmodem` over the channel's stdin and stdout.
  - `sz`: files land in staging and are offered as downloads through the queue.
  - `rz`: the UI shows a file picker, uploads to staging over HTTP, then Go sends.
  - Cancel is 8×CAN plus 8×BS. ZMODEM breaks under tmux and screen, so suggest trzsz there.
- **trzsz:** the frontend `TrzszFilter({writeToTerminal, sendToServer, terminalColumns})` sits between the WebSocket and xterm. `processServerOutput` and `processTerminalInput`; `uploadFiles` on drop; `setTerminalColumns` on resize. Needs trz/tsz on the server.
- **XMODEM, YMODEM, Kermit:** Go implementations triggered from the serial or terminal menu.

### 3.21 Remote monitoring protocol
- One non-PTY exec channel on the shared client runs:

  ```
  export LC_ALL=C
  while :; do
    echo @@S
    head -1 /proc/stat
    grep -E '^(MemTotal|MemAvailable|SwapTotal|SwapFree):' /proc/meminfo
    cat /proc/loadavg /proc/uptime /proc/net/dev /proc/sys/fs/file-nr
    df -kP -x tmpfs -x devtmpfs 2>/dev/null
    who | wc -l
    echo @@E
    sleep 2
  done
  ```

- **Parsing in Go:**
  - CPU % from `/proc/stat` deltas: busy = user+nice+system+irq+softirq+steal; total also adds idle+iowait.
  - Memory used = MemTotal − MemAvailable.
  - Network rates from byte deltas, excluding `lo`.
- **One-shot probe at connect:** `uname -srm; cat /etc/os-release; nproc`.
- **Other OSes:** macOS, BSD and Windows scripts (MON-2) are embedded and selected by `uname -s`.
- **Resource use:** the channel closes when the tab is hidden, because it counts toward MaxSessions.

---

## 4. Completeness check

A MobaXterm power user (sysadmin, network engineer, datacenter operator) would still expect the items below; none of the five reports covered them. They are added here as matrix rows (IDs `CC-*`). Also confirmed covered elsewhere: every MobaXterm session type (SSH, Telnet, Rsh, XDMCP, RDP, VNC, FTP, SFTP, Serial, File, Shell, Browser, Mosh, S3, WSL), every toolbar area (Session, Servers, Tools, Games, Sessions, View, Split, MultiExec, Tunneling, Packages, Settings, Help, X server), and the MobaXterm tool set (KeyGen, TextEditor, TextDiff, FoldersDiff, ListPorts, network/ports scanner, TCPCapture, PictureViewer, SwInfo/HwInfo, TaskList/KillTask, Caffeine, WOL, SSHTunnel).

| ID | Feature | Behavior spec | Pri | Feasibility | Implementation |
|---|---|---|---|---|---|
| CC-1 | WinRM / PowerShell Remoting session | Interactive PowerShell session to Windows hosts without OpenSSH (HTTP 5985 or HTTPS 5986; NTLM, Kerberos or Basic), as in Royal TS and RDM. | should | backend-go+frontend | `masterzen/winrm` (pseudo 2026-04) with a shell-and-command loop rendered in xterm; not a full PTY, so treated as line mode. |
| CC-2 | IPMI Serial-over-LAN / BMC console | Server console over IPMI SOL (iLO, iDRAC, Supermicro), plus power on, off, cycle and status. | should | backend-go+frontend | `bougou/go-ipmi` v0.9.1 (RMCP+ SOL payload) streamed to xterm; the BMC web UI opens through PROTO-28. |
| CC-3 | TN3270 / TN5250 mainframe terminals | IBM 3270 and 5250 sessions with field-based screens, PF/PA keys, EBCDIC codepages and TLS. | nice | backend-go+frontend | Go TN3270E negotiation plus a 3270 datastream parser, rendered to a fixed-grid canvas component (not xterm). Large effort; alternative is external `c3270` in a PTY. |
| CC-4 | SNMP tool | get, getnext, walk and bulkwalk (v1, v2c, v3), MIB name lookup, trap receiver. | nice | backend-go+frontend | `gosnmp/gosnmp` v1.45.0; embedded core MIBs. |
| CC-5 | Syslog server | UDP/TCP 514 (configurable) receiver with a live viewer, filters, highlight sets and file logging, for network gear (Tftpd64 parity). | nice | backend-go+frontend | RFC 3164 and 5424 parser; reuses the log viewer (MON-5) and SRV-1 lifecycle. |
| CC-6 | DHCP server (lab and field use) | Minimal DHCPv4 server for staging devices on an isolated link (Tftpd64 parity). Off by default; admin-only; loud warning. | nice | backend-go | `insomniacslk/dhcp` server4 (verify version), wired into the SRV-1 manager. |
| CC-7 | Reopen closed tab and session history | Ctrl+Shift+T reopens the last closed session (with the same parameters); history list of closed tabs. | should | frontend-only | Closed-tab stack in the layout store. |
| CC-8 | Send file to session (ASCII or binary, paced) | "Send text file" or "send binary file" into a terminal or serial port with line or character delay and wait-for-prompt; plus "capture raw to file" (SecureCRT and Tera Term parity). | should | backend-go+frontend | Upload into the Go pacer shared with TERM-17 and AUTO-6. |
| CC-9 | Printer passthrough (VT media copy) | Honors `ESC[5i` … `ESC[4i` so host applications can print to the local printer or a PDF. | nice | backend-go+frontend | Go captures the passthrough block and the browser prints it via a hidden iframe or saves it. |
| CC-10 | Pause output / Scroll Lock | Freeze display (Scroll Lock or a toolbar toggle) while output keeps buffering, then resume; optionally send XOFF/XON. | nice | frontend-only | Stop `term.write` and queue with the flow-control ack held. |
| CC-11 | Bidirectional (RTL) text | Correct Arabic and Hebrew rendering in terminals. | nice | **infeasible** | **Reason:** xterm.js has no BiDi support. **Alternative:** rely on the remote app's own reordering (e.g. `bicon`); the editor (CodeMirror) handles BiDi for files. |
| CC-12 | Global search across open terminals | One query searches every open session's scrollback and logs, with results grouped by tab. | nice | backend-go+frontend | Server-side search over ring buffers plus the FTS5 log index. |
| CC-13 | Password generator | Configurable generator (length, classes, passphrase words) in the vault UI and credential forms. | nice | frontend-only | `crypto.getRandomValues`; embedded EFF wordlist. |
| CC-14 | Local Windows services and disk tools | List, start and stop Windows services; disk and volume info (MobaXterm Windows tools parity). | nice | backend-go+frontend | x/sys/windows `svc/mgr`; gopsutil disk. Windows backends only; admin-gated. |
| CC-15 | Native client launch and connection file export | Export `.rdp` and `.vnc` files, and in desktop mode launch `mstsc`, `xfreerdp` or the macOS "Windows App" with the vault password injected where possible. Covers features a browser can't redirect (smart card, USB, WebAuthn). | should | backend-go | Generate files from the session model; `exec.Command` on the host; credentials via `cmdkey` on Windows. |
| CC-16 | VNC file transfer (UltraVNC/TightVNC extensions) | Transfer files through the VNC session. | nice | **infeasible** | **Reason:** noVNC does not implement these extensions. **Alternative:** SFTP companion (GFX-16) or the guacd VNC SFTP option. |
| CC-17 | SSH server audit (ssh-audit style) | Lists the server's offered KEX, host-key, cipher and MAC algorithms, flags weak or legacy ones and Terrapin exposure, and suggests an sshd_config fix. | nice | backend-go+frontend | Read the server KEXINIT from a raw handshake (a small parser before handing off to x/crypto), rated against built-in policy tables. |
| CC-18 | Serial power-user extras | Auto-baud detection, a live line-status panel (CTS, DSR, DCD, RI), timestamped TX/RX logging, and a two-port monitor/bridge (spy) mode. | nice | backend-go+frontend | `GetModemStatusBits` polling; auto-baud tries common rates while scoring printable output; bridge copies between two ports and tees a log. |
| CC-19 | Terminal convenience options | Hide the mouse pointer while typing, focus-follows-mouse between panes, a per-tab mini toolbar (split, MultiExec toggle, log toggle), and a "keep terminal size fixed" (80×24) mode for legacy apps. | nice | frontend-only | CSS cursor toggles; pointerenter focus; fixed cols/rows letterboxing (TERM-5). |
| CC-20 | Clipboard history | Recent copies from terminals and graphical sessions, re-pasteable, stored only in memory, excluding secrets. | nice | frontend-only | In-memory ring buffer; exclusion of vault-sourced copies. |
| CC-21 | Graphical session screenshot and "copy terminal as image" | Save an RDP/VNC frame or a terminal selection as PNG for tickets and docs. | nice | frontend-only | `canvas.toBlob` (GFX-18); render the selection via serialize-to-HTML plus an offscreen canvas. |

**Checks run for this synthesis:**
- Every Go and npm version in §2 was re-queried from proxy.golang.org and registry.npmjs.org on 2026-09-27. Those versions are current as of that date, except the ones marked "pin" (pseudo-versions) and the CC-6 `insomniacslk/dhcp` version, which was not queried.
- Firefox 151 (2026-05-19) shipping Web Serial on desktop was confirmed.
- xterm.js 6.0.0 release notes were checked: synchronized output (DEC 2026), removal of the canvas renderer, and the new progress addon. The ligatures addon still depends on the Node-only `font-finder` and `font-ligatures`, so TERM-6 stays experimental.