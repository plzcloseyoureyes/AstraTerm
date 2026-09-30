# Running AstraTerm as a server

```sh
astraterm --mode server --listen 0.0.0.0:7822 --tls-cert cert.pem --tls-key key.pem
```

The banner prints a one-time `https://…/?setup=<token>` link; only that link can create the first admin. Add users,
TOTP, passkeys and OIDC in the admin settings.

## Commands

| Command | |
|---|---|
| `astraterm [serve] [flags]` | start AstraTerm (default) |
| `astraterm version` | version, commit, build date |
| `astraterm reset-password <user>` | set a password offline (prompts; revokes sessions) |
| `astraterm help` | all flags |

## Flags

Every flag can also be set as `ASTRATERM_<NAME>` (e.g. `ASTRATERM_LISTEN`).

| Flag | Default | |
|---|---|---|
| `--listen` | `127.0.0.1:7822` | `host:port`; port `0` picks a free one |
| `--mode` | `desktop` | `desktop` or `server` |
| `--data-dir` | per-user config dir | database, keys, recordings, logs |
| `--portable` | off | data in `astraterm-data/` beside the executable (also on when that folder or an `astraterm.portable` file exists) |
| `--tls-cert`, `--tls-key`, `--tls-self-signed` | off | serve HTTPS |
| `--insecure-http` | off | plain HTTP on a network address (only behind a TLS proxy on a trusted network) |
| `--trusted-proxies` | none | CIDRs whose `X-Forwarded-For` / `-Proto` are trusted |
| `--allowed-hosts` | none | extra host names a loopback listener accepts (a same-machine reverse proxy) |
| `--open` / `--no-open` | open in desktop mode | open the browser on start |
| `--scrollback-bytes` | `4MiB` | per-session replay buffer |
| `--detached-ttl` | `24h` | close unattached sessions after this long (`0` = never) |
| `--guacd` | `127.0.0.1:4822` | optional guacd for RDP recordings and shadowing (`off` disables) |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |

Optional helpers are detected at run time: guacd, a Docker socket, `kubectl`.

## Data

Files are created `0600`/`0700`. Back up the data directory while AstraTerm is stopped, or use the admin backup in
Settings.

## systemd

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

Behind a reverse proxy: see [SECURITY.md → Hardening](../SECURITY.md#hardening).
