#!/usr/bin/env node
/*
 * AstraTerm end-to-end smoke test — drives the real binary like the browser does (REST + /ws/events + /ws/terminal).
 *
 *   make build                                  # builds bin/astraterm (frontend embedded)
 *   node scripts/smoke/smoke.mjs                # starts its own SSH container + a throwaway server, runs everything
 *   node scripts/smoke/smoke.mjs --ssh 127.0.0.1:22022 --ssh-user test --ssh-password test   # reuse a running target
 *   node scripts/smoke/smoke.mjs --hold         # keep server + container up afterwards (prints a browser launch URL)
 *
 * Requirements: Node >= 22 (global fetch + WebSocket with custom headers), Docker (unless --ssh or --no-ssh).
 * Everything it creates is disposable: a temp data dir, a server on a random loopback port, and one
 * lscr.io/linuxserver/openssh-server container (removed on exit). Exit code 0 = all checks passed.
 *
 * Coverage: SPA + assets (every lazy chunk), security headers, launch token / setup / login / logout / CSRF, events
 * socket (hello, ping, Origin + auth checks), folders / connections / identities CRUD with write-only secrets, settings
 * merge-patch round trip (user + global), SSH session from a saved connection (host-key prompt answered over the events
 * socket), terminal I/O, resize, delta and reset re-attach, scrollback, rename, ssh-info, close; quick-connect SSH with
 * a password prompt; local shell; vault master password → restart → locked (423) → unlock; graceful shutdown with a
 * clean server log.
 */
import { spawn, execFile as execFileCb } from 'node:child_process'
import crypto from 'node:crypto'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'
import { parseArgs, promisify } from 'node:util'

const execFile = promisify(execFileCb)
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')

// ---------------------------------------------------------------------------------------------------------------------
// options
// ---------------------------------------------------------------------------------------------------------------------

const { values: opt } = parseArgs({
  options: {
    bin: { type: 'string', default: path.join(ROOT, 'bin', process.platform === 'win32' ? 'astraterm.exe' : 'astraterm') },
    ssh: { type: 'string' }, // host:port of an existing SSH target (skips Docker)
    'ssh-user': { type: 'string', default: 'smoke' },
    'ssh-password': { type: 'string' },
    'ssh-image': { type: 'string', default: 'lscr.io/linuxserver/openssh-server:latest' },
    'no-ssh': { type: 'boolean', default: false },
    'work-dir': { type: 'string' },
    keep: { type: 'boolean', default: false },
    hold: { type: 'boolean', default: false },
    verbose: { type: 'boolean', short: 'v', default: false },
    help: { type: 'boolean', short: 'h', default: false },
  },
})

if (opt.help) {
  console.log(`Usage: node scripts/smoke/smoke.mjs [options]

  --bin PATH            AstraTerm binary (default: bin/astraterm)
  --ssh HOST:PORT       use an existing SSH server instead of starting a container
  --ssh-user USER       SSH user (default: smoke; the container is created with it)
  --ssh-password PW     SSH password (default: random for the container; required with --ssh)
  --ssh-image IMAGE     container image (default: lscr.io/linuxserver/openssh-server:latest)
  --no-ssh              skip every SSH check
  --work-dir DIR        where the data dir and server log go (default: a new temp dir)
  --keep                keep the work dir afterwards
  --hold                after the checks, keep the server (and container) running until Ctrl-C
  -v, --verbose         print server log lines and protocol traffic`)
  process.exit(0)
}
if (opt.ssh && !opt['ssh-password']) {
  console.error('--ssh needs --ssh-password')
  process.exit(2)
}

const color = process.stdout.isTTY ? (c, s) => `\x1b[${c}m${s}\x1b[0m` : (_c, s) => s
const log = (...a) => console.log(color('2', '·'), ...a)
const vlog = (...a) => opt.verbose && console.log(color('2', '  »'), ...a)
const rnd = (n = 12) => crypto.randomBytes(n).toString('base64url')

// ---------------------------------------------------------------------------------------------------------------------
// step runner
// ---------------------------------------------------------------------------------------------------------------------

const results = []
let aborted = false

class SkipError extends Error {}

/** Runs one named check. Critical failures abort the remaining steps (they depend on it). */
async function step(name, fn, { critical = false } = {}) {
  if (aborted) {
    results.push({ name, status: 'SKIP', ms: 0, note: 'aborted' })
    return undefined
  }
  const t0 = Date.now()
  try {
    const v = await fn()
    const ms = Date.now() - t0
    results.push({ name, status: 'PASS', ms })
    console.log(`${color('32', 'PASS')} ${name} ${color('2', `(${ms} ms)`)}`)
    return v
  } catch (err) {
    const ms = Date.now() - t0
    if (err instanceof SkipError) {
      results.push({ name, status: 'SKIP', ms, note: err.message })
      console.log(`${color('33', 'SKIP')} ${name}: ${err.message}`)
      return undefined
    }
    results.push({ name, status: 'FAIL', ms, note: err?.message ?? String(err) })
    console.log(`${color('31', 'FAIL')} ${name}: ${err?.stack ?? err}`)
    if (critical) aborted = true
    return undefined
  }
}

function assert(cond, msg) {
  if (!cond) throw new Error(`assertion failed: ${msg}`)
}
function assertEq(actual, expected, msg) {
  if (actual !== expected) throw new Error(`${msg}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`)
}

async function waitUntil(what, fn, timeoutMs = 15_000, intervalMs = 200) {
  const deadline = Date.now() + timeoutMs
  let last
  while (Date.now() < deadline) {
    try {
      const v = await fn()
      if (v) return v
    } catch (err) {
      last = err
    }
    await sleep(intervalMs)
  }
  throw new Error(`timed out waiting for ${what}${last ? ` (last error: ${last.message})` : ''}`)
}

// ---------------------------------------------------------------------------------------------------------------------
// cleanup
// ---------------------------------------------------------------------------------------------------------------------

const cleanups = []
let cleaning = false
async function cleanup() {
  if (cleaning) return
  cleaning = true
  for (const fn of cleanups.reverse()) {
    try {
      await fn()
    } catch (err) {
      console.error('cleanup:', err?.message ?? err)
    }
  }
}
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, async () => {
    console.log(`\n${sig}: cleaning up…`)
    await cleanup()
    process.exit(130)
  })
}

// ---------------------------------------------------------------------------------------------------------------------
// docker SSH target
// ---------------------------------------------------------------------------------------------------------------------

async function docker(...args) {
  const { stdout } = await execFile('docker', args, { timeout: 180_000, maxBuffer: 16 << 20 })
  return stdout.trim()
}

/** Resolves true once host:port speaks SSH (the container's port proxy accepts TCP before sshd is up). */
function sshBanner(host, port, timeoutMs = 3000) {
  return new Promise((resolve) => {
    const sock = net.connect({ host, port })
    let buf = ''
    const done = (ok) => {
      sock.destroy()
      resolve(ok)
    }
    sock.setTimeout(timeoutMs, () => done(false))
    sock.on('data', (d) => {
      buf += d.toString('latin1')
      if (buf.includes('SSH-2.0')) done(true)
    })
    sock.on('error', () => done(false))
    sock.on('close', () => done(buf.includes('SSH-2.0')))
  })
}

async function startSSHContainer() {
  const name = `astraterm-smoke-ssh-${rnd(4).toLowerCase().replace(/[^a-z0-9]/g, 'x')}`
  const password = opt['ssh-password'] || rnd(12)
  const user = opt['ssh-user']
  log(`starting SSH container ${name} (${opt['ssh-image']})`)
  await docker('run', '-d', '--rm', '--name', name,
    '-e', 'PUID=1000', '-e', 'PGID=1000', '-e', 'TZ=Etc/UTC',
    '-e', 'PASSWORD_ACCESS=true', '-e', `USER_NAME=${user}`, '-e', `USER_PASSWORD=${password}`,
    '-p', '127.0.0.1::2222', opt['ssh-image'])
  cleanups.push(async () => {
    log(`removing container ${name}`)
    await docker('rm', '-f', name).catch(() => undefined)
  })
  const mapped = await docker('port', name, '2222/tcp')
  const m = /:(\d+)\s*$/m.exec(mapped)
  if (!m) throw new Error(`cannot parse docker port output: ${mapped}`)
  const port = Number(m[1])
  await waitUntil('sshd in the container', () => sshBanner('127.0.0.1', port), 120_000, 1000)
  // The image creates the user and applies the password a little after sshd starts accepting; give it a moment.
  await sleep(1500)
  return { host: '127.0.0.1', port, user, password, container: name }
}

// ---------------------------------------------------------------------------------------------------------------------
// AstraTerm server process
// ---------------------------------------------------------------------------------------------------------------------

class Server {
  constructor(bin, dataDir, logPath, mode = 'desktop') {
    this.bin = bin
    this.dataDir = dataDir
    this.logPath = logPath
    this.mode = mode
    this.proc = null
    this.base = ''
    this.launchToken = ''
  }

  async start() {
    const logFd = fs.openSync(this.logPath, 'a')
    const args = ['serve', '--listen', '127.0.0.1:0', '--data-dir', this.dataDir, '--no-open', '--mode', this.mode,
      '--scrollback-bytes', '2MiB', '--guacd', 'off', '--log-level', 'debug']
    vlog('spawn', this.bin, args.join(' '))
    // A throwaway home: local shells, the Local files tab and importers must never see the real one.
    const home = path.join(path.dirname(this.dataDir), `home-${path.basename(this.dataDir)}`)
    fs.mkdirSync(home, { recursive: true, mode: 0o700 })
    const env = { ...process.env, ASTRATERM_NO_OPEN: '1', HOME: home, USERPROFILE: home }
    const proc = spawn(this.bin, args, { stdio: ['ignore', 'pipe', logFd], env })
    fs.closeSync(logFd)
    this.proc = proc
    this.exited = new Promise((resolve) => proc.on('exit', (code, signal) => resolve({ code, signal })))
    let out = ''
    const url = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`server did not print its URL; log: ${this.logPath}`)), 20_000)
      proc.stdout.on('data', (d) => {
        out += d.toString()
        const m = /URL:\s+(\S+)/.exec(out)
        if (m) {
          clearTimeout(timer)
          resolve(m[1])
        }
      })
      proc.on('exit', (code) => {
        clearTimeout(timer)
        reject(new Error(`server exited early (code ${code}); log: ${this.logPath}`))
      })
    })
    const u = new URL(url)
    this.launchToken = u.searchParams.get('launch') || ''
    this.setupToken = u.searchParams.get('setup') || ''
    this.base = u.origin
    this.launchURL = url
    vlog('server at', this.base)
    return this
  }

  async stop() {
    if (!this.proc || this.proc.exitCode !== null) return this.exited
    this.proc.kill('SIGINT')
    const res = await Promise.race([this.exited, sleep(15_000).then(() => null)])
    if (!res) {
      this.proc.kill('SIGKILL')
      throw new Error('server did not shut down within 15 s (killed)')
    }
    return res
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// HTTP client with a one-cookie jar
// ---------------------------------------------------------------------------------------------------------------------

class Client {
  constructor(server) {
    this.server = server
    this.cookie = ''
  }

  get base() {
    return this.server.base
  }

  async req(method, p, body, { csrf = true, headers = {}, raw = false } = {}) {
    const h = { Accept: 'application/json', ...headers }
    if (csrf && method !== 'GET') h['X-AstraTerm'] = '1'
    if (this.cookie) h.Cookie = this.cookie
    let payload
    if (body !== undefined) {
      h['Content-Type'] = 'application/json'
      payload = JSON.stringify(body)
    }
    const res = await fetch(this.base + p, { method, headers: h, body: payload, redirect: 'manual' })
    for (const sc of res.headers.getSetCookie?.() ?? []) {
      const [pair] = sc.split(';')
      const [k, v] = pair.split('=')
      if (k.trim() === 'astraterm_session') this.cookie = v ? `astraterm_session=${v}` : ''
      if (/max-age=0|expires=thu, 01 jan 1970/i.test(sc)) this.cookie = ''
      this.lastSetCookie = sc
    }
    const text = raw ? '' : await res.text()
    let json
    if (!raw && text && (res.headers.get('content-type') || '').includes('application/json')) {
      try {
        json = JSON.parse(text)
      } catch {
        /* not JSON */
      }
    }
    vlog(method, p, res.status)
    return { status: res.status, headers: res.headers, text, json, res }
  }

  async ok(method, p, body, expectStatus = [200, 201]) {
    const r = await this.req(method, p, body)
    const allowed = Array.isArray(expectStatus) ? expectStatus : [expectStatus]
    if (!allowed.includes(r.status)) throw new Error(`${method} ${p} → ${r.status} ${r.text.slice(0, 300)}`)
    return r.json
  }

  async fails(method, p, body, status, code) {
    const r = await this.req(method, p, body)
    if (r.status !== status) throw new Error(`${method} ${p}: expected ${status}, got ${r.status} ${r.text.slice(0, 300)}`)
    if (code && r.json?.code !== code) throw new Error(`${method} ${p}: expected code ${code}, got ${JSON.stringify(r.json)}`)
    return r.json
  }

  wsURL(p) {
    return this.base.replace(/^http/, 'ws') + p
  }

  wsHeaders(origin = this.base) {
    const h = { Origin: origin }
    if (this.cookie) h.Cookie = this.cookie
    return h
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// /ws/events client
// ---------------------------------------------------------------------------------------------------------------------

class Events {
  constructor(client) {
    this.client = client
    this.events = []
    this.waiters = new Set()
    /** Called for every prompt: return {accept, values?, save?} or null to leave it unanswered. */
    this.onPrompt = null
    this.prompts = []
  }

  open() {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(this.client.wsURL('/ws/events'), { headers: this.client.wsHeaders() })
      this.ws = ws
      let opened = false
      ws.onopen = () => {
        opened = true
        resolve(this)
      }
      ws.onerror = (e) => {
        if (!opened) reject(new Error(`events socket failed: ${e.message ?? 'error'}`))
      }
      ws.onclose = (e) => {
        this.closed = { code: e.code, reason: e.reason }
        this.notify()
      }
      ws.onmessage = (e) => {
        let ev
        try {
          ev = JSON.parse(e.data)
        } catch {
          return
        }
        vlog('event', ev.type, ev.type === 'session.updated' ? `${ev.session.id} ${ev.session.state}` : '')
        this.events.push(ev)
        if (ev.type === 'prompt') this.handlePrompt(ev.prompt)
        this.notify()
      }
    })
  }

  handlePrompt(p) {
    this.prompts.push(p)
    const answer = this.onPrompt?.(p)
    if (answer) this.send({ type: 'prompt.response', id: p.id, ...answer })
  }

  send(msg) {
    this.ws.send(JSON.stringify(msg))
  }

  notify() {
    for (const w of this.waiters) w()
  }

  /** Waits for an event matching pred that arrived at or after index `since` (default: any). */
  waitFor(what, pred, { timeout = 30_000, since = 0 } = {}) {
    return new Promise((resolve, reject) => {
      let timer
      const check = () => {
        for (let i = since; i < this.events.length; i++) {
          if (pred(this.events[i])) {
            clearTimeout(timer)
            this.waiters.delete(check)
            resolve(this.events[i])
            return
          }
        }
        if (this.closed) {
          clearTimeout(timer)
          this.waiters.delete(check)
          reject(new Error(`events socket closed (${this.closed.code}) while waiting for ${what}`))
        }
      }
      timer = setTimeout(() => {
        this.waiters.delete(check)
        reject(new Error(`timed out waiting for event: ${what}`))
      }, timeout)
      this.waiters.add(check)
      check()
    })
  }

  close() {
    try {
      this.ws?.close(1000)
    } catch {
      /* ignore */
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// /ws/terminal client (SPEC §6.2)
// ---------------------------------------------------------------------------------------------------------------------

class Term {
  constructor(client, sessionId, offset = 0) {
    this.client = client
    this.sessionId = sessionId
    this.startOffset = offset
    this.offset = offset
    this.control = []
    this.attaches = []
    this.text = ''
    this.received = 0
    this.decoder = new TextDecoder('utf-8')
    this.waiters = new Set()
    this.autoAck = true
  }

  open() {
    return new Promise((resolve, reject) => {
      const url = this.client.wsURL(`/ws/terminal/${encodeURIComponent(this.sessionId)}?offset=${this.startOffset}`)
      const ws = new WebSocket(url, { headers: this.client.wsHeaders() })
      ws.binaryType = 'arraybuffer'
      this.ws = ws
      let opened = false
      ws.onopen = () => {
        opened = true
        resolve(this)
      }
      ws.onerror = (e) => {
        if (!opened) reject(new Error(`terminal socket failed: ${e.message ?? 'error'}`))
      }
      ws.onclose = (e) => {
        this.closed = { code: e.code, reason: e.reason }
        this.notify()
      }
      ws.onmessage = (e) => {
        if (typeof e.data === 'string') {
          const msg = JSON.parse(e.data)
          vlog('term ctl', e.data.slice(0, 160))
          this.control.push(msg)
          if (msg.type === 'attach') {
            this.attaches.push(msg)
            this.offset = msg.from
            if (msg.mode === 'reset') this.text = ''
          }
        } else {
          const bytes = new Uint8Array(e.data)
          this.offset += bytes.length
          this.received += bytes.length
          this.text += this.decoder.decode(bytes, { stream: true })
          if (this.text.length > 8_000_000) this.text = this.text.slice(-4_000_000)
          if (this.autoAck) this.ack()
        }
        this.notify()
      }
    })
  }

  ack() {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify({ type: 'ack', offset: this.offset }))
  }

  input(s) {
    this.ws.send(new TextEncoder().encode(s))
  }

  send(msg) {
    this.ws.send(JSON.stringify(msg))
  }

  notify() {
    for (const w of this.waiters) w()
  }

  waitFor(what, pred, timeout = 20_000) {
    return new Promise((resolve, reject) => {
      let timer
      const check = () => {
        const v = pred(this)
        if (v) {
          clearTimeout(timer)
          this.waiters.delete(check)
          resolve(v)
        } else if (this.closed) {
          clearTimeout(timer)
          this.waiters.delete(check)
          reject(new Error(`terminal socket closed (${this.closed.code} ${this.closed.reason}) while waiting for ${what}`))
        }
      }
      timer = setTimeout(() => {
        this.waiters.delete(check)
        reject(new Error(`timed out waiting for ${what}; last output: ${JSON.stringify(this.text.slice(-300))}`))
      }, timeout)
      this.waiters.add(check)
      check()
    })
  }

  waitText(re, timeout) {
    const rx = typeof re === 'string' ? new RegExp(re.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')) : re
    return this.waitFor(`output ${rx}`, (t) => rx.test(t.text) && t.text.match(rx), timeout)
  }

  waitControl(what, pred, timeout) {
    return this.waitFor(what, (t) => t.control.find(pred), timeout)
  }

  close() {
    try {
      this.ws?.close(1000)
    } catch {
      /* ignore */
    }
  }
}

/** Runs `cmd` in a shell attached through t and waits for a unique marker computed by the shell itself. */
async function run(t, cmd, timeout = 20_000) {
  const a = Math.floor(Math.random() * 9000) + 1000
  const b = Math.floor(Math.random() * 9000) + 1000
  const marker = `__done_${a * b}__`
  const before = t.text.length
  t.input(`${cmd}; echo __done_$((${a}*${b}))__\r`)
  await t.waitFor(`marker for ${JSON.stringify(cmd)}`, (x) => x.text.indexOf(marker, before) >= 0, timeout)
  const out = t.text.slice(before)
  return out.slice(0, out.indexOf(marker))
}

// ---------------------------------------------------------------------------------------------------------------------
// the test plan
// ---------------------------------------------------------------------------------------------------------------------

async function main() {
  const bin = path.resolve(opt.bin)
  if (!fs.existsSync(bin)) {
    console.error(`binary not found: ${bin} (run "make build" first)`)
    process.exit(2)
  }
  const workDir = opt['work-dir'] ? path.resolve(opt['work-dir']) : fs.mkdtempSync(path.join(os.tmpdir(), 'astraterm-smoke-'))
  fs.mkdirSync(workDir, { recursive: true })
  const dataDir = path.join(workDir, 'data')
  const logPath = path.join(workDir, 'server.log')
  if (!opt.keep && !opt['work-dir']) cleanups.push(() => fs.rmSync(workDir, { recursive: true, force: true }))
  log(`work dir ${workDir}`)

  // SSH target first (it takes the longest to boot).
  let ssh = null
  if (!opt['no-ssh']) {
    ssh = await step('ssh target is reachable', async () => {
      if (opt.ssh) {
        const [host, port] = opt.ssh.split(':')
        const t = { host, port: Number(port), user: opt['ssh-user'], password: opt['ssh-password'] }
        assert(await sshBanner(t.host, t.port), `no SSH banner from ${opt.ssh}`)
        return t
      }
      return startSSHContainer()
    })
  }

  let server = new Server(bin, dataDir, logPath)
  cleanups.push(async () => {
    await server.stop().catch(() => undefined)
  })
  await step('server starts (desktop mode, random port)', async () => {
    await server.start()
    assert(server.launchToken, 'banner has a ?launch= token')
  }, { critical: true })

  const anon = new Client(server)
  const c = new Client(server)
  const adminName = 'smoke'
  const adminPass = `Smoke-${rnd(12)}`

  // ---- SPA & static assets --------------------------------------------------------------------------------------
  await step('SPA index, every JS/CSS chunk, security headers', async () => {
    const r = await anon.req('GET', '/', undefined, { headers: { Accept: 'text/html' } })
    assertEq(r.status, 200, 'GET / status')
    assert((r.headers.get('content-type') || '').startsWith('text/html'), 'index is HTML')
    assert(r.headers.get('content-security-policy'), 'CSP header present')
    assertEq(r.headers.get('x-content-type-options'), 'nosniff', 'nosniff')
    const assets = new Set([...r.text.matchAll(/(?:src|href)="\/?(assets\/[^"]+)"/g)].map((m) => m[1]))
    assert(assets.size >= 2, 'index references assets')
    // Follow every chunk reference inside JS/CSS (lazy chunks included) so a missing file is caught.
    const seen = new Set()
    const queue = [...assets]
    while (queue.length) {
      const a = queue.shift()
      if (seen.has(a)) continue
      seen.add(a)
      const res = await fetch(`${server.base}/${a}`)
      assertEq(res.status, 200, `GET /${a}`)
      if (/\.(js|css)$/.test(a)) {
        const ct = res.headers.get('content-type') || ''
        assert(a.endsWith('.js') ? /javascript/.test(ct) : /text\/css/.test(ct), `${a} content-type ${ct}`)
        assert(/immutable/.test(res.headers.get('cache-control') || ''), `${a} is cached immutable`)
        const body = await res.text()
        for (const m of body.matchAll(/["'`(/]((?:\.\/|assets\/)?[\w.-]+-[\w-]{8}\.(?:js|css|woff2|wasm|svg|png))["'`)]/g)) {
          const f = m[1].replace(/^\.\//, '').replace(/^assets\//, '')
          queue.push(`assets/${f}`)
        }
      } else {
        await res.arrayBuffer()
      }
    }
    log(`  ${seen.size} static assets verified`)
    const spa = await anon.req('GET', '/connections/some/route', undefined, { headers: { Accept: 'text/html' } })
    assertEq(spa.status, 200, 'SPA fallback status')
    assert(spa.text.includes('<div id="root"'), 'SPA fallback serves index.html')
    const api404 = await anon.req('GET', '/api/does-not-exist')
    assertEq(api404.status, 404, 'unknown API path')
    assert(api404.json?.code, 'unknown API path answers JSON {error, code}')
    const pop = await anon.req('GET', '/popout.html', undefined, { headers: { Accept: 'text/html' } })
    assertEq(pop.status, 200, 'popout.html is served')
  })

  // ---- auth ---------------------------------------------------------------------------------------------------------
  await step('auth: fresh state requires setup; launch before setup → 409', async () => {
    const st = await anon.ok('GET', '/api/auth/state')
    assertEq(st.setupRequired, true, 'setupRequired')
    assertEq(st.authenticated, false, 'authenticated')
    assertEq(st.mode, 'desktop', 'mode')
    assert(typeof st.features === 'object', 'features object')
    await anon.fails('POST', '/api/auth/launch', { token: server.launchToken }, 409, 'setup_required')
  }, { critical: true })

  await step('auth: CSRF header required on mutations', async () => {
    const r = await anon.req('POST', '/api/auth/setup', { username: adminName, password: adminPass }, { csrf: false })
    assertEq(r.status, 403, 'setup without X-AstraTerm')
    assertEq(r.json?.code, 'csrf', 'error code')
  })

  await step('auth: setup creates the admin and signs in', async () => {
    await c.fails('POST', '/api/auth/setup', { username: adminName, password: 'short' }, 400)
    const r = await c.ok('POST', '/api/auth/setup', { username: adminName, password: adminPass, displayName: 'Smoke Admin' })
    assertEq(r.user.username, adminName, 'user')
    assertEq(r.user.role, 'admin', 'role')
    assert(c.cookie, 'session cookie set')
    assert(/httponly/i.test(c.lastSetCookie) && /samesite=strict/i.test(c.lastSetCookie), `cookie flags: ${c.lastSetCookie}`)
    const st = await c.ok('GET', '/api/auth/state')
    assertEq(st.authenticated, true, 'authenticated after setup')
    assertEq(st.setupRequired, false, 'setupRequired after setup')
    await c.fails('POST', '/api/auth/setup', { username: 'x2', password: adminPass }, 409)
  }, { critical: true })

  await step('auth: logout, bad login 401, login', async () => {
    await c.ok('POST', '/api/auth/logout')
    const st = await c.ok('GET', '/api/auth/state')
    assertEq(st.authenticated, false, 'logged out')
    await c.fails('GET', '/api/connections', undefined, 401)
    await c.fails('POST', '/api/auth/login', { username: adminName, password: 'wrong-password-123' }, 401, 'invalid_credentials')
    const r = await c.ok('POST', '/api/auth/login', { username: adminName, password: adminPass, remember: true })
    assertEq(r.user.username, adminName, 'login user')
    assert(c.cookie, 'cookie after login')
  }, { critical: true })

  await step('auth: launch token is single use', async () => {
    const l = new Client(server)
    const r = await l.ok('POST', '/api/auth/launch', { token: server.launchToken })
    assertEq(r.user.username, adminName, 'launch logs in as the first admin')
    assert(l.cookie, 'launch cookie')
    await new Client(server).fails('POST', '/api/auth/launch', { token: server.launchToken }, 401, 'invalid_token')
    const st = await l.ok('GET', '/api/auth/state')
    assertEq(st.authenticated, true, 'launch session works')
  })

  // ---- events -------------------------------------------------------------------------------------------------------
  const ev = new Events(c)
  await step('events: hello + ping/pong; Origin and auth enforced', async () => {
    await ev.open()
    const hello = await ev.waitFor('hello', (e) => e.type === 'hello')
    assert(hello.clientId, 'hello.clientId')
    assertEq(hello.user?.username, adminName, 'hello.user')
    const n = ev.events.length
    ev.send({ type: 'ping' })
    await ev.waitFor('pong', (e) => e.type === 'pong', { since: n, timeout: 5000 })
    // Cross-site WebSocket hijacking guard + auth.
    const evil = new WebSocket(c.wsURL('/ws/events'), { headers: c.wsHeaders('http://evil.example') })
    const evilOpened = await new Promise((resolve) => {
      evil.onopen = () => resolve(true)
      evil.onerror = () => resolve(false)
    })
    assertEq(evilOpened, false, 'cross-origin events socket rejected')
    const nocookie = new WebSocket(c.wsURL('/ws/events'), { headers: { Origin: server.base } })
    const nocookieOpened = await new Promise((resolve) => {
      nocookie.onopen = () => resolve(true)
      nocookie.onerror = () => resolve(false)
    })
    assertEq(nocookieOpened, false, 'anonymous events socket rejected')
  }, { critical: true })
  cleanups.push(() => ev.close())

  // ---- CRUD -----------------------------------------------------------------------------------------------------------
  const ctx = {}
  await step('folders: create, rename, list', async () => {
    const f = await c.ok('POST', '/api/folders', { name: 'Smoke' }, 201)
    assert(f.id && f.name === 'Smoke' && f.ownerId, 'folder shape')
    const sub = await c.ok('POST', '/api/folders', { name: 'Child', parentId: f.id }, 201)
    assertEq(sub.parentId, f.id, 'child parentId')
    const p = await c.ok('PATCH', `/api/folders/${f.id}`, { name: 'Smoke Tests', color: '#22c55e' })
    assertEq(p.name, 'Smoke Tests', 'renamed')
    const list = await c.ok('GET', '/api/folders')
    assert(list.some((x) => x.id === f.id) && list.some((x) => x.id === sub.id), 'folders listed')
    const moved = await c.ok('PATCH', `/api/folders/${sub.id}`, { parentId: null })
    assert(!moved.parentId, 'child moved to root with parentId:null')
    await c.ok('PATCH', `/api/folders/${sub.id}`, { parentId: f.id })
    // Non-recursive delete moves the folder's children up to its parent (the UI offers "delete contents" separately).
    const tmp = await c.ok('POST', '/api/connections', { name: 'tmp', protocol: 'telnet', host: 'example.invalid', folderId: sub.id }, 201)
    await c.ok('DELETE', `/api/folders/${sub.id}`)
    const orphan = await c.ok('GET', `/api/connections/${tmp.id}`)
    assertEq(orphan.folderId, f.id, 'non-recursive delete re-parents the connection')
    await c.ok('DELETE', `/api/connections/${tmp.id}`)
    ctx.folder = f
  }, { critical: true })

  const secretMarker = `pw-${rnd(9)}`
  await step('connections: create with write-only secret, patch, duplicate, reorder, bulk-delete', async () => {
    const conn = await c.ok('POST', '/api/connections', {
      name: 'Smoke SSH', protocol: 'ssh', host: ssh?.host || '127.0.0.1', port: ssh?.port || 22, username: ssh?.user || 'smoke',
      folderId: ctx.folder.id, tags: ['smoke'], options: { term: 'xterm-256color', keepAliveSec: 15 },
      secrets: { password: ssh?.password || secretMarker },
    }, 201)
    assertEq(conn.protocol, 'ssh', 'protocol')
    assertEq(conn.folderId, ctx.folder.id, 'folderId')
    assert(Array.isArray(conn.secretKeys) && conn.secretKeys.includes('password'), 'secretKeys lists password')
    assert(!('secrets' in conn), 'secrets never returned')
    const listRes = await c.req('GET', '/api/connections')
    assert(!listRes.text.includes(ssh?.password || secretMarker), 'secret value absent from list response')
    const one = await c.ok('GET', `/api/connections/${conn.id}`)
    assertEq(one.name, 'Smoke SSH', 'get by id')
    const patched = await c.ok('PATCH', `/api/connections/${conn.id}`, { notes: 'patched', favorite: true })
    assertEq(patched.notes, 'patched', 'notes patched')
    assert(patched.secretKeys.includes('password'), 'omitted secrets stay unchanged')
    // secrets: "" deletes; a new value sets
    const withTmp = await c.ok('PATCH', `/api/connections/${conn.id}`, { secrets: { sudoPassword: 'x' } })
    assert(withTmp.secretKeys.includes('sudoPassword') && withTmp.secretKeys.includes('password'), 'secret merge adds')
    const noTmp = await c.ok('PATCH', `/api/connections/${conn.id}`, { secrets: { sudoPassword: '' } })
    assert(!noTmp.secretKeys.includes('sudoPassword') && noTmp.secretKeys.includes('password'), '"" deletes one secret')
    const dup = await c.ok('POST', `/api/connections/${conn.id}/duplicate`, undefined, 201)
    assert(dup.id !== conn.id, 'duplicate has a new id')
    await c.ok('POST', '/api/connections/reorder', {
      items: [{ id: dup.id, folderId: ctx.folder.id, sortOrder: 0 }, { id: conn.id, folderId: ctx.folder.id, sortOrder: 1 }],
    })
    const after = await c.ok('GET', '/api/connections')
    const so = Object.fromEntries(after.map((x) => [x.id, x.sortOrder]))
    assert(so[dup.id] < so[conn.id], 'reorder applied')
    await c.ok('POST', '/api/connections/bulk-delete', { ids: [dup.id] })
    await c.fails('GET', `/api/connections/${dup.id}`, undefined, 404)
    await c.fails('POST', '/api/connections', { name: 'bad', protocol: 'ssh', host: 'x', port: 70000 }, 400)
    ctx.conn = conn
  }, { critical: true })

  await step('identities: create (write-only secret), patch, delete', async () => {
    const id = await c.ok('POST', '/api/identities', { name: 'Smoke identity', username: 'ops', secrets: { password: secretMarker } }, 201)
    assert(id.secretKeys.includes('password') && !('secrets' in id), 'identity secret write-only')
    const p = await c.ok('PATCH', `/api/identities/${id.id}`, { username: 'ops2' })
    assertEq(p.username, 'ops2', 'identity patched')
    const list = await c.req('GET', '/api/identities')
    assert(!list.text.includes(secretMarker), 'identity secret absent from list')
    await c.ok('DELETE', `/api/identities/${id.id}`)
  })

  await step('settings: RFC 7396 merge-patch round trip (user + global)', async () => {
    await c.ok('PUT', '/api/settings', { smoke: { a: 1, nested: { b: 2, c: 3 }, list: [1, 2] } })
    let s = await c.ok('GET', '/api/settings')
    assertEq(JSON.stringify(s.smoke), JSON.stringify({ a: 1, list: [1, 2], nested: { b: 2, c: 3 } }), 'stored')
    await c.ok('PUT', '/api/settings', { smoke: { a: null, nested: { c: 4 } } })
    s = await c.ok('GET', '/api/settings')
    assertEq(JSON.stringify(s.smoke), JSON.stringify({ list: [1, 2], nested: { b: 2, c: 4 } }), 'merged')
    await c.ok('PUT', '/api/admin/settings', { smokeGlobal: { x: 1 }, smoke: { fromGlobal: true } })
    s = await c.ok('GET', '/api/settings')
    assertEq(s.smokeGlobal?.x, 1, 'global value visible to the user')
    assertEq(s.smoke?.nested?.c, 4, 'user value wins over global')
    await c.ok('PUT', '/api/settings', { smoke: null })
    await c.ok('PUT', '/api/admin/settings', { smokeGlobal: null, smoke: null })
    s = await c.ok('GET', '/api/settings')
    assert(!('smoke' in s) && !('smokeGlobal' in s), 'null deletes sections')
  })

  // ---- SSH session from the saved connection ------------------------------------------------------------------------
  if (ssh) {
    let term
    await step('ssh: session from saved connection; host-key prompt answered over /ws/events', async () => {
      ev.onPrompt = (p) => (p.kind === 'hostkey' ? { accept: true, save: true } : null)
      const since = ev.events.length
      const s = await c.ok('POST', '/api/sessions', { connectionId: ctx.conn.id, cols: 80, rows: 24 }, 201)
      assertEq(s.kind, 'terminal', 'kind')
      assertEq(s.protocol, 'ssh', 'protocol')
      assertEq(s.connectionId, ctx.conn.id, 'connectionId')
      assert(['connecting', 'authenticating', 'connected'].includes(s.state), `initial state ${s.state}`)
      const pe = await ev.waitFor('hostkey prompt', (e) => e.type === 'prompt' && e.prompt.kind === 'hostkey', { since })
      const hk = pe.prompt.hostKey
      assert(hk && /^SHA256:/.test(hk.fingerprint) && /^MD5:/.test(hk.fingerprintMd5), 'host key fingerprints')
      assertEq(hk.status, 'unknown', 'host key status')
      assertEq(hk.port, ssh.port, 'host key port')
      assertEq(pe.prompt.sessionId, s.id, 'prompt.sessionId')
      await ev.waitFor('session connected', (e) => e.type === 'session.updated' && e.session.id === s.id && e.session.state === 'connected', { since })
      assertEq(ev.prompts.filter((p) => p.sessionId === s.id && p.kind !== 'hostkey').length, 0, 'stored password used (no password prompt)')
      ctx.sid = s.id
    }, { critical: false })

    if (ctx.sid) {
      await step('ssh: terminal attach, command I/O, acks', async () => {
        term = await new Term(c, ctx.sid, 0).open()
        await term.waitControl('attach', (m) => m.type === 'attach')
        const att = term.attaches[0]
        assertEq(att.mode, 'delta', 'fresh attach mode')
        assertEq(att.from, 0, 'fresh attach from')
        await term.waitControl('attach-end', (m) => m.type === 'attach-end')
        assert(term.control.some((m) => m.type === 'readonly' && m.value === false), 'readonly:false')
        assert(term.control.some((m) => m.type === 'state' && m.state === 'connected'), 'state connected')
        assert(term.control.some((m) => m.type === 'resize' && m.cols === 80 && m.rows === 24), 'initial size 80x24')
        const out = await run(term, 'echo smoke-$((6*7))-ok; whoami')
        assert(out.includes('smoke-42-ok'), `echo output: ${JSON.stringify(out)}`)
        assert(out.includes(ssh.user), 'whoami')
      })

      await step('ssh: resize reaches the PTY (stty size) and the session info', async () => {
        term.send({ type: 'resize', cols: 101, rows: 31 })
        await sleep(300)
        const out = await run(term, 'stty size')
        assert(/31 101/.test(out), `stty size: ${JSON.stringify(out)}`)
        const info = await c.ok('GET', `/api/sessions/${ctx.sid}`)
        assertEq(info.cols, 101, 'session cols')
        assertEq(info.rows, 31, 'session rows')
      })

      await step('ssh: scrollback (raw + stripped), rename, ssh-info, list', async () => {
        const raw = await c.req('GET', `/api/sessions/${ctx.sid}/scrollback`)
        assertEq(raw.status, 200, 'scrollback status')
        assert(raw.text.includes('smoke-42-ok'), 'raw scrollback content')
        const plain = await c.req('GET', `/api/sessions/${ctx.sid}/scrollback?raw=0`)
        assert(plain.text.includes('smoke-42-ok') && !plain.text.includes('\x1b['), 'stripped scrollback')
        const r = await c.ok('PATCH', `/api/sessions/${ctx.sid}`, { title: 'Renamed smoke' })
        assertEq(r.title, 'Renamed smoke', 'rename')
        const info = await c.ok('GET', `/api/sessions/${ctx.sid}/ssh-info`)
        assert(/OpenSSH/i.test(info.serverVersion), `ssh-info serverVersion ${info.serverVersion}`)
        assert(info.kex && info.cipher && /^SHA256:/.test(info.hostKeyFingerprint), 'ssh-info algorithms')
        const list = await c.ok('GET', '/api/sessions')
        const me = list.find((x) => x.id === ctx.sid)
        assert(me && me.clients === 1 && me.state === 'connected', 'listed with 1 client')
        const conn = await c.ok('GET', `/api/connections/${ctx.conn.id}`)
        assert(conn.lastUsedAt, 'connection lastUsedAt touched')
      })

      await step('ssh: detach, REST input, re-attach with delta', async () => {
        await run(term, 'true')
        const offset = term.offset
        term.close()
        await term.waitFor('socket closed', (t) => t.closed, 5000)
        await c.ok('POST', `/api/sessions/${ctx.sid}/input`, { data: 'echo delta-$((3*3))-marker\r' })
        await waitUntil('delta output in scrollback', async () => (await c.req('GET', `/api/sessions/${ctx.sid}/scrollback?raw=0`)).text.includes('delta-9-marker'))
        term = await new Term(c, ctx.sid, offset).open()
        const end = await term.waitControl('attach-end', (m) => m.type === 'attach-end')
        const att = term.attaches[0]
        assertEq(att.mode, 'delta', 'attach mode')
        assertEq(att.from, offset, 'attach from')
        assertEq(term.received, end.head - offset, 'replayed exactly the missing bytes')
        assert(term.text.includes('delta-9-marker'), 'delta contains the detached output')
      })

      await step('ssh: output while detached overflows the 2 MiB ring → reset re-attach', async () => {
        const offset = term.offset
        term.close()
        await term.waitFor('socket closed', (t) => t.closed, 5000)
        await c.ok('POST', `/api/sessions/${ctx.sid}/input`, { data: 'seq 1 450000; echo ring-$((5*5))-wrapped\r' })
        await waitUntil('flood to finish', async () => (await c.req('GET', `/api/sessions/${ctx.sid}/scrollback?raw=0`)).text.includes('ring-25-wrapped'), 60_000, 500)
        term = await new Term(c, ctx.sid, offset).open()
        const end = await term.waitControl('attach-end', (m) => m.type === 'attach-end', 30_000)
        const att = term.attaches[0]
        assertEq(att.mode, 'reset', 'attach mode after wrap')
        assert(att.from > offset, `reset from ${att.from} > old offset ${offset}`)
        assert(end.head - att.from <= 2 * 1024 * 1024, 'ring holds at most 2 MiB')
        await term.waitFor('full ring replay', (t) => t.received >= end.head - att.from, 30_000)
        assertEq(term.received, end.head - att.from, 'reset replays tail..head')
        assert(term.text.includes('ring-25-wrapped'), 'tail of the flood present')
        const again = await run(term, 'echo alive-$((2*21))')
        assert(again.includes('alive-42'), 'session usable after the reset attach')
      })

      await step('ssh: signal INT interrupts a running command', async () => {
        const before = term.text.length
        term.input('sleep 30; echo slept-$((1+1))\r')
        await sleep(700)
        await c.ok('POST', `/api/sessions/${ctx.sid}/signal`, { name: 'INT' })
        const out = await run(term, 'echo after-int', 10_000)
        assert(!term.text.slice(before).includes('slept-2'), 'sleep was interrupted')
        assert(out.includes('after-int'), 'shell still usable')
      })

      await step('ssh: close → session.closed event, socket closed, 404 afterwards', async () => {
        const since = ev.events.length
        await c.ok('DELETE', `/api/sessions/${ctx.sid}`)
        await ev.waitFor('session.closed', (e) => e.type === 'session.closed' && e.id === ctx.sid, { since })
        await term.waitFor('terminal socket closed', (t) => t.closed, 5000)
        assert(term.control.some((m) => m.type === 'state' && m.state === 'closed'), 'state closed sent before close')
        await c.fails('GET', `/api/sessions/${ctx.sid}`, undefined, 404)
        const reopened = await new Promise((resolve) => {
          const w = new WebSocket(c.wsURL(`/ws/terminal/${ctx.sid}`), { headers: c.wsHeaders() })
          w.onopen = () => {
            w.close()
            resolve(true)
          }
          w.onerror = () => resolve(false)
        })
        assertEq(reopened, false, 'terminal socket for a closed session is refused')
      })
    }

    // The pool shares one SSH transport per user+host+port+username (SPEC §4), so a quick connect to the same address
    // would silently reuse the saved connection's authenticated transport. Use the other loopback name to force a new
    // transport: that host name is not in known_hosts yet, and no password is stored → both prompts must appear.
    const altHost = ssh.host === '127.0.0.1' ? 'localhost' : ssh.host === 'localhost' ? '127.0.0.1' : null
    await step('ssh: quick connect on a new transport → host-key + password prompts, exit status', async () => {
      const since = ev.events.length
      const pSince = ev.prompts.length
      ev.onPrompt = (p) => {
        if (p.kind === 'hostkey') return { accept: true, save: false }
        if (p.kind === 'password' || p.kind === 'keyboard-interactive') return { accept: true, values: p.fields.map(() => ssh.password), save: false }
        return { accept: false }
      }
      const host = altHost || ssh.host
      const s = await c.ok('POST', '/api/sessions', { quick: { protocol: 'ssh', host, port: ssh.port, username: ssh.user }, cols: 90, rows: 20 }, 201)
      assert(!s.connectionId, 'quick session has no connectionId')
      await ev.waitFor('quick session connected', (e) => e.type === 'session.updated' && e.session.id === s.id && e.session.state === 'connected', { since })
      const mine = ev.prompts.slice(pSince).filter((p) => p.sessionId === s.id)
      if (altHost) {
        assert(mine.some((p) => p.kind === 'hostkey' && p.hostKey?.host === altHost), `host-key prompt for ${altHost}`)
        assert(mine.some((p) => p.kind === 'password' || p.kind === 'keyboard-interactive'), 'asked for the password')
      } else {
        log('  (non-loopback target: the pooled transport may be reused, prompts not asserted)')
      }
      const t = await new Term(c, s.id, 0).open()
      await t.waitControl('attach-end', (m) => m.type === 'attach-end')
      const out = await run(t, 'echo quick-$((7*7))')
      assert(out.includes('quick-49'), 'quick session I/O')
      t.input('exit 3\r')
      await t.waitControl('exit state', (m) => m.type === 'state' && m.state === 'disconnected')
      const st = t.control.findLast((m) => m.type === 'state')
      assertEq(st.exitCode, 3, 'remote exit code')
      await c.ok('DELETE', `/api/sessions/${s.id}`)
      t.close()
    })
  }

  // ---- local shell ------------------------------------------------------------------------------------------------------
  await step('local shell: list shells, open, I/O, exit code', async () => {
    const shells = await c.ok('GET', '/api/local/shells')
    assert(Array.isArray(shells) && shells.length > 0 && shells[0].id && shells[0].path, 'shells listed')
    const since = ev.events.length
    const s = await c.ok('POST', '/api/sessions', { quick: { protocol: 'local' }, cols: 80, rows: 24 }, 201)
    assertEq(s.protocol, 'local', 'protocol')
    await ev.waitFor('local connected', (e) => e.type === 'session.updated' && e.session.id === s.id && e.session.state === 'connected', { since })
    const t = await new Term(c, s.id, 0).open()
    await t.waitControl('attach-end', (m) => m.type === 'attach-end')
    const out = await run(t, 'echo local-$((2+3))')
    assert(out.includes('local-5'), `local output ${JSON.stringify(out)}`)
    t.input('exit 0\r')
    await t.waitControl('exit state', (m) => m.type === 'state' && m.state === 'disconnected')
    assertEq(t.control.findLast((m) => m.type === 'state').exitCode, 0, 'exit code')
    await t.waitText('Session ended (exit code 0)')
    await c.ok('DELETE', `/api/sessions/${s.id}`)
    t.close()
  })

  await step('sessions: validation errors', async () => {
    await c.fails('POST', '/api/sessions', { quick: { protocol: 'nope' }, cols: 80, rows: 24 }, 400)
    await c.fails('POST', '/api/sessions', { quick: { protocol: 'ssh' }, cols: 80, rows: 24 }, 400)
    await c.fails('POST', '/api/sessions', { connectionId: 'doesnotexist', cols: 80, rows: 24 }, 404)
  })

  await step('admin: users and audit log', async () => {
    const users = await c.ok('GET', '/api/admin/users')
    assert(users.some((u) => u.username === adminName), 'admin user listed')
    const audit = await c.ok('GET', '/api/admin/audit?limit=200')
    const actions = new Set(audit.map((a) => a.action))
    for (const a of ['auth.setup', 'auth.login']) assert(actions.has(a), `audit has ${a} (got ${[...actions].join(', ')})`)
  })

  // ---- vault master password + restart -------------------------------------------------------------------------------
  const masterPw = `Master-${rnd(10)}`
  await step('vault: set master password', async () => {
    let st = await c.ok('GET', '/api/vault/status')
    assertEq(st.locked, false, 'initially unlocked')
    assertEq(st.hasMasterPassword, false, 'no master password')
    await c.ok('POST', '/api/vault/master-password', { newPassword: masterPw })
    st = await c.ok('GET', '/api/vault/status')
    assertEq(st.hasMasterPassword, true, 'has master password')
    assertEq(st.locked, false, 'still unlocked')
  }, { critical: true })

  await step('vault: restart → locked; secrets unusable (423) until unlock', async () => {
    ev.close()
    const res = await server.stop()
    assertEq(res.code, 0, 'graceful exit code')
    server = new Server(bin, dataDir, logPath)
    await server.start()
    c.server = server
    const st = await c.ok('GET', '/api/auth/state')
    assertEq(st.authenticated, true, 'login session survives the restart')
    assertEq(st.vaultLocked, true, 'vault locked after restart')
    assertEq(st.vaultHasMasterPassword, true, 'vaultHasMasterPassword')
    const list = await c.ok('GET', '/api/connections')
    assert(list.find((x) => x.id === ctx.conn.id)?.secretKeys.includes('password'), 'connections list works while locked')
    await c.fails('POST', '/api/sessions', { connectionId: ctx.conn.id, cols: 80, rows: 24 }, 423, 'locked')
    await c.fails('POST', '/api/vault/unlock', { password: 'not-the-password' }, 403, 'wrong_password')
  }, { critical: true })

  const ev2 = new Events(c)
  cleanups.push(() => ev2.close())
  await step('vault: unlock → vault event; saved SSH session connects without prompts', async () => {
    await ev2.open()
    await ev2.waitFor('hello', (e) => e.type === 'hello')
    const since = ev2.events.length
    await c.ok('POST', '/api/vault/unlock', { password: masterPw })
    await ev2.waitFor('vault unlocked event', (e) => e.type === 'vault' && e.locked === false, { since })
    const st = await c.ok('GET', '/api/vault/status')
    assertEq(st.locked, false, 'unlocked')
    if (!ssh) return
    ev2.onPrompt = () => ({ accept: false })
    const s = await c.ok('POST', '/api/sessions', { connectionId: ctx.conn.id, cols: 80, rows: 24 }, 201)
    await ev2.waitFor('connected', (e) => e.type === 'session.updated' && e.session.id === s.id && e.session.state === 'connected', { since })
    assertEq(ev2.prompts.length, 0, 'no prompts (known host + stored password)')
    const t = await new Term(c, s.id, 0).open()
    await t.waitControl('attach-end', (m) => m.type === 'attach-end')
    const out = await run(t, 'echo vault-$((8*8))')
    assert(out.includes('vault-64'), 'I/O after unlock')
    await c.ok('DELETE', `/api/sessions/${s.id}`)
    t.close()
  })

  await step('vault: lock/unlock round trip, remove master password', async () => {
    await c.ok('POST', '/api/vault/lock')
    assertEq((await c.ok('GET', '/api/vault/status')).locked, true, 'locked')
    await c.ok('POST', '/api/vault/unlock', { password: masterPw })
    await c.ok('POST', '/api/vault/master-password', { currentPassword: masterPw, newPassword: '' })
    const st = await c.ok('GET', '/api/vault/status')
    assertEq(st.hasMasterPassword, false, 'master password removed')
    assertEq(st.locked, false, 'unlocked')
  })

  await step('cleanup: recursive folder delete removes its connections', async () => {
    await c.ok('DELETE', `/api/folders/${ctx.folder.id}?recursive=1`)
    assert(!(await c.ok('GET', '/api/folders')).some((f) => f.id === ctx.folder.id), 'folder gone')
    await c.fails('GET', `/api/connections/${ctx.conn.id}`, undefined, 404)
  })

  // ---- server (multi-user) mode: a second, independent instance ------------------------------------------------------
  const srvLog = path.join(workDir, 'server-mode.log')
  const srv = new Server(bin, path.join(workDir, 'data-server'), srvLog, 'server')
  cleanups.push(async () => {
    await srv.stop().catch(() => undefined)
  })
  await step('server mode: setup token; no launch token; local shells admin-only; per-user privacy; admin-only sharing', async () => {
    await srv.start()
    assertEq(srv.launchToken, '', 'server mode prints no launch token')
    assert(srv.setupToken.length >= 32, 'banner carries the one-time ?setup= token')
    const admin = new Client(srv)
    const st = await admin.ok('GET', '/api/auth/state')
    assertEq(st.mode, 'server', 'mode')
    assertEq(st.setupRequired, true, 'setup required')
    assertEq(st.setupTokenRequired, true, 'setup token required')
    await admin.fails('POST', '/api/auth/launch', { token: 'x'.repeat(43) }, 404)
    // A stranger reaching a fresh instance cannot claim it without the token from the operator's console.
    await new Client(srv).fails('POST', '/api/auth/setup', { username: 'intruder', password: adminPass }, 403, 'setup_token_required')
    await new Client(srv).fails('POST', '/api/auth/setup', { username: 'intruder', password: adminPass, setupToken: 'guess' }, 403, 'setup_token_required')
    await admin.ok('POST', '/api/auth/setup', { username: 'root', password: adminPass, setupToken: srv.setupToken })
    await new Client(srv).fails('POST', '/api/auth/setup', { username: 'again', password: adminPass, setupToken: srv.setupToken }, 409)
    const alicePass = `Alice-${rnd(12)}`
    const created = await admin.ok('POST', '/api/admin/users', { username: 'alice', password: alicePass, role: 'user' }, 201)
    assertEq(created.role, 'user', 'new user role')
    const alice = new Client(srv)
    await alice.ok('POST', '/api/auth/login', { username: 'alice', password: alicePass })
    await alice.fails('GET', '/api/admin/users', undefined, 403)
    await alice.fails('GET', '/api/local/shells', undefined, 403)
    await alice.fails('POST', '/api/sessions', { quick: { protocol: 'local' }, cols: 80, rows: 24 }, 403)
    await alice.fails('POST', '/api/connections', { name: 'x', protocol: 'ssh', host: 'h', shared: true }, 403)
    const own = await alice.ok('POST', '/api/connections', { name: 'alice-private', protocol: 'ssh', host: '10.0.0.1', secrets: { password: secretMarker } }, 201)
    const shared = await admin.ok('POST', '/api/connections', { name: 'team-shared', protocol: 'ssh', host: '10.0.0.2', shared: true, secrets: { password: secretMarker } }, 201)
    const adminList = await admin.ok('GET', '/api/connections')
    assert(!adminList.some((x) => x.id === own.id), "admin does not see alice's private connection")
    const aliceList = await alice.req('GET', '/api/connections')
    assert(aliceList.json.some((x) => x.id === shared.id && x.secretKeys.includes('password')), 'shared connection visible to alice')
    assert(!aliceList.text.includes(secretMarker), 'shared secret never returned')
    await alice.fails('PATCH', `/api/connections/${shared.id}`, { name: 'hijack' }, 403)
    const s = await admin.ok('POST', '/api/sessions', { quick: { protocol: 'local' }, cols: 80, rows: 24 }, 201)
    await waitUntil('admin local session connected', async () => (await admin.ok('GET', `/api/sessions/${s.id}`)).state === 'connected')
    assert(!(await alice.ok('GET', '/api/sessions')).some((x) => x.id === s.id), "alice cannot list the admin's session")
    await alice.fails('GET', `/api/sessions/${s.id}`, undefined, 404)
    await alice.fails('POST', `/api/sessions/${s.id}/input`, { data: 'id\r' }, 404)
    await admin.ok('DELETE', `/api/sessions/${s.id}`)
    const res = await srv.stop()
    assertEq(res.code, 0, 'graceful exit')
    const bad = fs.readFileSync(srvLog, 'utf8').split('\n').filter((l) => /level=ERROR|panic/.test(l))
    assert(bad.length === 0, `server-mode log errors:\n${bad.join('\n')}`)
  })

  await step('data dir permissions (0700 dirs, 0600 files)', async () => {
    if (process.platform === 'win32') throw new SkipError('not applicable on Windows')
    const bad = []
    const walk = (p) => {
      const st = fs.statSync(p)
      const mode = st.mode & 0o777
      if (st.isDirectory()) {
        if (mode & 0o077) bad.push(`${p} ${mode.toString(8)}`)
        for (const e of fs.readdirSync(p)) walk(path.join(p, e))
      } else if (mode & 0o077) bad.push(`${p} ${mode.toString(8)}`)
    }
    walk(dataDir)
    assert(bad.length === 0, `group/other-accessible paths: ${bad.join(', ')}`)
  })

  if (opt.hold) {
    const url = server.launchURL
    console.log(`\n${color('36', 'HOLD')} server: ${url}`)
    if (ssh) console.log(`${color('36', 'HOLD')} ssh target: ${ssh.host}:${ssh.port} user ${ssh.user} (saved password connection was deleted)`)
    console.log(`${color('36', 'HOLD')} data dir: ${dataDir}\n${color('36', 'HOLD')} Ctrl-C to stop`)
    await new Promise(() => {})
  }

  await step('graceful shutdown with live sessions; server log has no errors or panics', async () => {
    // Leave a session running with an attached terminal: closing it during shutdown writes audit/recording rows,
    // which must happen before the database is closed.
    const s = await c.ok('POST', '/api/sessions', { quick: { protocol: 'local' }, cols: 80, rows: 24 }, 201)
    await waitUntil('local session connected', async () => (await c.ok('GET', `/api/sessions/${s.id}`)).state === 'connected')
    const t = await new Term(c, s.id, 0).open()
    await t.waitControl('attach-end', (m) => m.type === 'attach-end')
    const res = await server.stop()
    await t.waitFor('terminal socket closed by shutdown', (x) => x.closed, 5000)
    ev2.close()
    assertEq(res.code, 0, 'exit code')
    const text = fs.readFileSync(logPath, 'utf8')
    const bad = text.split('\n').filter((l) => /level=ERROR|panic|DATA RACE/.test(l))
    const warns = text.split('\n').filter((l) => /level=WARN/.test(l))
    if (warns.length) log(`  ${warns.length} WARN line(s):\n    ${warns.slice(0, 10).join('\n    ')}`)
    assert(bad.length === 0, `error lines:\n${bad.slice(0, 20).join('\n')}`)
  })
}

// ---------------------------------------------------------------------------------------------------------------------

let exitCode = 0
try {
  await main()
} catch (err) {
  console.error(color('31', 'fatal:'), err?.stack ?? err)
  exitCode = 1
} finally {
  await cleanup()
}
const failed = results.filter((r) => r.status === 'FAIL')
const skipped = results.filter((r) => r.status === 'SKIP')
console.log(`\n${results.length - failed.length - skipped.length} passed, ${failed.length} failed, ${skipped.length} skipped`)
for (const f of failed) console.log(`  ${color('31', 'FAIL')} ${f.name}: ${f.note.split('\n')[0]}`)
if (failed.length) exitCode = 1
process.exit(exitCode)
