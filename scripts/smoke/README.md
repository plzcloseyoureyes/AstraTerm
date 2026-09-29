# End-to-end smoke test

`smoke.mjs` drives the **built binary** exactly like the browser does: REST calls with the session cookie and CSRF
header, the `/ws/events` socket (prompts are answered there), and `/ws/terminal/{id}` with byte-offset acks. It needs
no npm packages.

```sh
make build                      # bin/nexterm with the frontend embedded
make smoke                      # = make build + node scripts/smoke/smoke.mjs
node scripts/smoke/smoke.mjs -h # options
```

Requirements: Node ≥ 22 (global `fetch` and `WebSocket` with custom headers; tested with Node 26) and Docker. Docker
is only needed for the SSH target, which you can replace with `--ssh HOST:PORT --ssh-user U --ssh-password P`, or skip
entirely with `--no-ssh`.

## What it does

1. Starts `lscr.io/linuxserver/openssh-server` with password auth on a random loopback port (`-p 127.0.0.1::2222`,
   random password) and waits for the SSH banner.
2. Starts `bin/nexterm serve --listen 127.0.0.1:0 --data-dir <tmp>/data --scrollback-bytes 2MiB --log-level debug`,
   reads the URL and launch token from the banner, and keeps the server log in `<tmp>/server.log`.
3. Runs the checks (each prints `PASS`/`FAIL`/`SKIP`; a failed critical step skips the dependent ones):

| Area | Checks |
|---|---|
| Static | `/`, every JS/CSS chunk reachable from the index (lazy chunks included) with the right type and immutable caching, CSP / nosniff, SPA fallback, JSON 404 for unknown `/api/*`, `popout.html` |
| Auth | setup required → launch returns 409 → CSRF enforced → setup → cookie flags → logout / bad login 401 / login → launch token single use |
| Events | `hello`, ping/pong, cross-origin and anonymous sockets rejected |
| CRUD | folders (nesting, `parentId:null`, non-recursive delete re-parents), connections (write-only secrets, merge/delete of one secret, duplicate, reorder, bulk delete, validation), identities, settings merge-patch (user + global) |
| SSH | saved connection → host-key prompt over events (SHA256 + MD5) → connected with the stored password; attach (delta from 0), I/O, resize (`stty size`), scrollback raw/stripped, rename, `ssh-info`, delta re-attach after detached REST input, ring overflow → `attach(reset)` re-attach, `signal INT`, close → `session.closed` |
| Quick connect | new transport (other loopback name) → host-key + password prompts → remote exit status |
| Local shell | `/api/local/shells`, I/O, exit code and "Session ended" notice |
| Vault | master password → restart → locked (423, `wrong_password`) → unlock via API → `vault` event → saved session connects with no prompts → lock/unlock → remove master password |
| Server mode | a second instance with `--mode server`: setup needs the banner's one-time `?setup=` token (403 without or with a wrong one, 409 once used), no launch token (launch → 404), a non-admin user gets 403 for local shells, admin APIs and `shared:true`, cannot see or touch the admin's private connections or sessions, sees shared connections without their secrets |
| Shutdown | graceful stop **with a live attached session**; the server log must contain no `ERROR`/panic lines; data dir is 0700/0600 |

4. Removes the container and the temp directory. Use `--keep` (or `--work-dir DIR`) to keep the data dir and the
   server log, and `--hold` to leave the server and container running afterwards (it prints a launch URL for manual
   browser testing; Ctrl-C cleans up).

The expected `WARN` lines in the server log are the 4xx responses the test provokes on purpose.

## Shared test targets

`scripts/testenv/docker-compose.yml` starts the full lab (two SSH hosts for jump tests, telnet, FTP, VNC, RDP, guacd,
S3, WebDAV, SMB) on fixed loopback ports for feature work; the smoke test does not depend on it.
