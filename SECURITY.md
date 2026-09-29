# Security policy

NexTerm holds the keys to your infrastructure — credentials, private keys and live sessions — so security reports
get priority over everything else. Thank you for helping keep its users safe.

## Reporting a vulnerability

**Please do not report security issues in public issues, pull requests or discussions.**

- Preferred: GitHub private vulnerability reporting — *Security → Report a vulnerability* on the repository
  (`https://github.com/OWNER/nexterm/security/advisories/new`).
- Or e-mail **security@TBD.example** <!-- TODO(owner): replace with the real address before publishing -->
  (encrypt with our PGP key if you like: *TBD*).

Include what you can: the affected version (`nexterm version`), run mode (desktop/server), configuration flags,
steps to reproduce or a proof of concept, and the impact you expect. Please use your own test instance and throwaway
data, never other people's systems.

What to expect:

| | Target |
|---|---|
| Acknowledgement | within 3 business days |
| Initial assessment and severity | within 10 business days |
| Fix or mitigation for high/critical issues | as fast as possible, usually within 30 days |
| Public disclosure | coordinated with you once a fixed release is available (at the latest 90 days after the report, unless we agree otherwise) |

We credit reporters in the release notes and the advisory unless you prefer to stay anonymous. There is no bug
bounty at this time.

## Supported versions

NexTerm is pre-1.0. Security fixes are made on `main` and shipped in a new release; only the **latest release** is
supported. Please upgrade before reporting and check whether the issue still reproduces.

| Version | Supported |
|---|---|
| latest release | yes |
| older releases | no |

## Security model

**Trust boundaries**

- **The NexTerm process is the security boundary.** The browser only talks to NexTerm; all protocol traffic (SSH, RDP,
  VNC, file transfers, tunnels, tools) originates from the machine NexTerm runs on, with that machine's network
  position.
- **Desktop mode** (default) serves one person on their own computer: it binds `127.0.0.1` only, rejects requests
  whose `Host` header is not a loopback name (DNS rebinding), and signs in through a single-use launch token printed
  in the start-up banner. Host features (local shells, serial ports, embedded servers, local files, the host SSH
  agent) are available because the user is the machine's owner.
- **Server mode** serves several accounts. Administrators are fully trusted: they can run commands on the NexTerm
  host (local shells, ProxyCommand), change the security policy and read the audit log. Ordinary users are isolated
  from the host: host features are admin-only, their outbound connections pass the network policy (below), shared
  connections are usable but their secrets are never revealed, and private rows of other users are invisible.
- **Remote hosts are untrusted input.** Terminal output, file names, remote file contents and remote-desktop streams
  are treated as hostile (paste safety, host-key verification, sanitized file names).

**Protections**

- Authentication: argon2id password hashes, login back-off and lockout policy, TOTP with recovery codes, passkeys
  (WebAuthn), OIDC single sign-on, API tokens, re-authentication for sensitive account and admin actions.
- Web: SameSite=Strict session cookies plus a required `X-NexTerm` header on mutations (CSRF), WebSocket Origin
  checks, a strict Content Security Policy (no inline scripts, no remote origins), `nosniff`, `no-referrer`,
  frame-ancestors restrictions, HSTS over TLS.
- Secrets: connection and identity secrets, private keys and passphrases are write-only in the API and sealed with
  XChaCha20-Poly1305 under a data key. Without a master password the data key is wrapped by a system key in
  `<data dir>/system.key`; with a master password (argon2id) the vault starts locked after every restart until an
  admin unlocks it. Typed secrets are kept out of input recordings; the AI assistant redacts secrets before sending
  anything to a provider.
- Outbound network policy (`internal/netguard`, server mode): every connection made for a non-admin user is checked
  on the concrete dialed address (after DNS, so rebinding and redirects cannot bypass it). By default loopback,
  link-local, cloud metadata endpoints, the host's own addresses and NexTerm's own listener are refused; private
  ranges are allowed for bastion use. Admins can tighten the policy and optionally apply it to admins too.
- SSH host keys: unknown keys are confirmed by the user, changed keys are refused unless confirmed (in server mode
  only by an admin), `@cert-authority` and `@revoked` markers are honoured.
- Accountability: an audit log of security-relevant actions, optional command audit and session recordings.

**Data at rest.** Everything lives in the data directory (SQLite database, system key, recordings, logs), created
with `0700`/`0600` permissions. Anyone who can read that directory *and* the system key can decrypt stored secrets
unless a master password is set. Protect it like an SSH key directory, and keep backups encrypted.

**Out of scope.** Attacks that require an already compromised NexTerm host or administrator account; actions an
administrator is allowed to perform; missing hardening on instances that run with `--insecure-http` on untrusted
networks; denial of service by authenticated administrators; vulnerabilities in remote systems you connect to.

## Hardening

**Desktop mode**

- Keep the default `127.0.0.1` bind. Anyone who can reach the port still needs to sign in, but a loopback-only
  listener keeps the attack surface on your machine.
- On shared multi-user computers, set a vault master password and use the lock screen with auto-lock.

**Server mode**

- **TLS is mandatory on network addresses.** Use `--tls-cert/--tls-key` with a certificate from your CA or ACME
  client; `--tls-self-signed` is for trials only. `--insecure-http` exists solely for a TLS-terminating reverse proxy
  on a trusted network segment.
- **Create the first administrator immediately** through the one-time `?setup=` link in the banner (it is the only way
  to create one), then require TOTP or passkeys for administrators.
- **Reverse proxies.** Forward the original `Host` header and WebSocket upgrades (`/ws/…`), and set
  `--trusted-proxies <proxy IP/CIDR>` so client IPs (lockout, audit) and `X-Forwarded-Proto: https` (Secure cookies)
  are honoured. NexTerm bound to a loopback address accepts only loopback/`localhost` host names (DNS-rebinding
  protection, `421` otherwise). For a proxy on the same machine, keep NexTerm on loopback and name the proxy's public
  host names with `--allowed-hosts nexterm.example.com` (`NEXTERM_ALLOWED_HOSTS`, comma-separated, exact names, no
  wildcards) — the guard then accepts exactly those names in addition to loopback. A proxy on another machine must reach
  NexTerm on a non-loopback address (e.g. a private interface or container network, firewalled to the proxy, with
  `--insecure-http`) or NexTerm terminates TLS itself.
- **Network policy.** Review Settings → Security → network policy: add the internal ranges ordinary users must not
  reach (databases, management networks), and enable it for administrators if they do not need exceptions.
- **Master password** for the vault if the data directory could be read by others (backups, shared storage); remember
  that the vault then stays locked after restarts until an admin unlocks it.
- **Run as an unprivileged service user** with the data directory owned by it (see the systemd example in the
  README), keep the binary updated, and leave the optional helpers (guacd, Docker socket, `kubectl`) off when unused
  — guacd in particular should listen only on loopback.
- **Monitor** the audit log and consider command audit and session recordings for privileged access.

## Verifying releases

Every release publishes `SHA256SUMS`, an SPDX SBOM per archive and a GitHub build-provenance attestation:

```sh
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify nexterm_<version>_<os>_<arch>.tar.gz --repo OWNER/nexterm
```
