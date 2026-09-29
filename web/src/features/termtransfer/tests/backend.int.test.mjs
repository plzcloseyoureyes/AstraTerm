/*
 * End-to-end: the transfer engines driven over Termstead's real terminal WebSocket (SPEC §6.2: binary output frames,
 * acks, binary input into the server's 8 MiB per-session input queue) to an SSH session on the term-transfer test
 * container (lrzsz + trzsz-go, see testenv.Dockerfile and protocols.int.test.mjs). Guarded by TERMSTEAD_TESTENV=1;
 * needs a Termstead binary:
 *
 *   TERMSTEAD_TESTENV=1 TERMSTEAD_BIN=../bin/termstead [TERMSTEAD_TT_SSH=127.0.0.1:23050] \
 *     node --import ./src/features/termtransfer/tests/register.mjs --test src/features/termtransfer/tests/backend.int.test.mjs
 *
 * The server runs on a random loopback port with a temporary data dir; the SSH user/password default to test/test
 * (TERMSTEAD_TT_SSH_USER / TERMSTEAD_TT_SSH_PASSWORD) — the container's throwaway credentials.
 */
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import crypto from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, before, describe, it } from 'node:test'
import { DownloadTarget } from '../engine/save.ts'
import { sendBinaryPaced, sendTextPaced, PromptWatcher, stripAnsi, timeoutClock } from '../engine/pacer.ts'
import { TrzszStage } from '../engine/trzsz.ts'
import { ZmodemStage, zmodemReceive, zmodemSend } from '../engine/zmodem.ts'

const ENABLED = process.env.TERMSTEAD_TESTENV === '1' && !!process.env.TERMSTEAD_BIN
const SSH = process.env.TERMSTEAD_TT_SSH || '127.0.0.1:23050'
const SSH_USER = process.env.TERMSTEAD_TT_SSH_USER || 'test'
const SSH_PASSWORD = process.env.TERMSTEAD_TT_SSH_PASSWORD || 'test'
const CONTAINER = process.env.TERMSTEAD_TT_CONTAINER || 'termstead-termtransfer-ssh'
const sha = (b) => crypto.createHash('sha256').update(b).digest('hex')
const enc = (s) => new TextEncoder().encode(s)
const sh = (cmd) => execFileSync('docker', ['exec', '-u', SSH_USER, CONTAINER, 'sh', '-c', cmd]).toString()

function until(what, pred, ms = 30_000) {
  return new Promise((resolve, reject) => {
    const t0 = Date.now()
    const t = setInterval(() => {
      let v
      try {
        v = pred()
      } catch (e) {
        clearInterval(t)
        reject(e)
        return
      }
      if (v) {
        clearInterval(t)
        resolve(v)
      } else if (Date.now() - t0 > ms) {
        clearInterval(t)
        reject(new Error(`timeout: ${what}`))
      }
    }, 25)
  })
}

class Server {
  async start() {
    this.dir = fs.mkdtempSync(path.join(os.tmpdir(), 'termstead-tt-e2e-'))
    this.proc = spawn(process.env.TERMSTEAD_BIN, ['serve', '--listen', '127.0.0.1:0', '--data-dir', this.dir, '--no-open', '--guacd', 'off'], { stdio: ['ignore', 'pipe', 'pipe'] })
    let out = ''
    this.base = await new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error('server did not start')), 30_000)
      this.proc.stdout.on('data', (d) => {
        out += d
        const m = /URL:\s+(\S+)/.exec(out)
        if (m) {
          clearTimeout(t)
          resolve(new URL(m[1]).origin)
        }
      })
      this.proc.on('exit', (c) => reject(new Error(`server exited ${c}`)))
    })
    this.cookie = ''
    const pw = crypto.randomBytes(18).toString('base64url')
    await this.req('POST', '/api/auth/setup', { username: 'e2e', password: pw })
    return this
  }
  async req(method, p, body) {
    const h = { Accept: 'application/json', 'X-Termstead': '1' }
    if (this.cookie) h.Cookie = this.cookie
    if (body !== undefined) h['Content-Type'] = 'application/json'
    const init = { method, headers: h }
    if (body !== undefined) init.body = JSON.stringify(body)
    const res = await fetch(this.base + p, init)
    for (const sc of res.headers.getSetCookie?.() ?? []) {
      const [pair] = sc.split(';')
      if (pair.startsWith('termstead_session=')) this.cookie = pair
    }
    const text = await res.text()
    if (!res.ok) throw new Error(`${method} ${p} → ${res.status} ${text.slice(0, 200)}`)
    return text ? JSON.parse(text) : null
  }
  ws(p) {
    return new WebSocket(this.base.replace(/^http/, 'ws') + p, { headers: { Origin: this.base, Cookie: this.cookie } })
  }
  async stop() {
    this.proc?.kill('SIGINT')
    await new Promise((r) => setTimeout(r, 500))
    fs.rmSync(this.dir, { recursive: true, force: true })
  }
}

/** A terminal view: output → filter(bytes) → rendered text, acks like the browser does. */
class View {
  constructor(server, sessionId) {
    this.server = server
    this.sessionId = sessionId
    this.text = ''
    this.offset = 0
    this.filter = (b) => b
    this.watcher = new PromptWatcher()
    this.errors = []
  }
  open() {
    return new Promise((resolve, reject) => {
      const ws = this.server.ws(`/ws/terminal/${this.sessionId}?offset=0`)
      ws.binaryType = 'arraybuffer'
      this.ws = ws
      ws.onopen = () => resolve(this)
      ws.onerror = (e) => reject(new Error(`ws: ${e.message ?? 'error'}`))
      ws.onmessage = (e) => {
        if (typeof e.data === 'string') {
          const m = JSON.parse(e.data)
          if (m.type === 'attach') this.offset = m.from
          if (m.type === 'error') this.errors.push(m.message)
          return
        }
        const bytes = new Uint8Array(e.data)
        if (process.env.TT_DEBUG) {
          ;(this.recent ??= []).push(Buffer.from(bytes.slice(-120)).toString('latin1'))
          if (this.recent.length > 8) this.recent.shift()
        }
        this.offset += bytes.length
        this.watcher.feed(bytes)
        const out = this.filter(bytes)
        if (out?.length) this.text += Buffer.from(out).toString('latin1')
        ws.send(JSON.stringify({ type: 'ack', offset: this.offset }))
      }
    })
  }
  send(data) {
    if (this.ws.readyState !== WebSocket.OPEN) return false
    this.ws.send(typeof data === 'string' ? enc(data) : data)
    return true
  }
  async run(cmd, ms = 20_000) {
    const tag = `DONE${crypto.randomBytes(3).toString('hex')}`
    this.send(`${cmd}; echo ${tag.slice(0, 4)}"${tag.slice(4)}"\r`)
    await until(`command ${cmd}`, () => this.text.includes(tag), ms).catch((e) => {
      throw new Error(`${e.message}; terminal ends with ${JSON.stringify(this.text.slice(-400))}`)
    })
  }
  /** After a transfer: wait until the shell prompt is back (the tools read stdin for a moment after the session). */
  async settle() {
    const mark = this.text.length
    await new Promise((r) => setTimeout(r, 800))
    this.send('\r')
    await until('prompt', () => stripAnsi(this.text.slice(mark)).endsWith('$ '), 10_000)
  }
  close() {
    this.ws?.close()
  }
}

describe('transfers through the Termstead backend', { skip: !ENABLED && 'set TERMSTEAD_TESTENV=1 and TERMSTEAD_BIN' }, () => {
  let server
  let view
  let events

  before(async () => {
    server = await new Server().start()
    events = server.ws('/ws/events')
    events.onmessage = (e) => {
      const m = JSON.parse(e.data)
      if (m.type === 'prompt' && m.prompt.kind === 'hostkey') events.send(JSON.stringify({ type: 'prompt.response', id: m.prompt.id, accept: true, save: true }))
    }
    await new Promise((r) => (events.onopen = r))
    const [host, port] = SSH.split(':')
    const s = await server.req('POST', '/api/sessions', {
      quick: { protocol: 'ssh', host, port: Number(port), username: SSH_USER, authMethod: 'password', password: SSH_PASSWORD },
      cols: 120,
      rows: 30,
    })
    for (let i = 0; ; i++) {
      if ((await server.req('GET', `/api/sessions/${s.id}`)).state === 'connected') break
      if (i > 60) throw new Error('the SSH session did not connect')
      await new Promise((r) => setTimeout(r, 500))
    }
    view = await new View(server, s.id).open()
    await view.run('cd /tmp && export PS1="$ "')
  })

  after(async () => {
    view?.close()
    events?.close()
    await server?.stop()
  })

  it('sz → ZMODEM receive (4 MB) is byte-exact', async () => {
    sh('head -c 4000000 /dev/urandom > /tmp/e2e-z.bin')
    const want = sh('sha256sum /tmp/e2e-z.bin').split(' ')[0]
    const parts = []
    const done = new Promise((resolve, reject) => {
      const stage = new ZmodemStage({
        send: (b) => view.send(b),
        writeAsync: () => undefined,
        detect: (d) =>
          setTimeout(
            () =>
              zmodemReceive(d.confirm(), {
                open: async (info) => ({ localName: info.path[0], write: async (c) => void parts.push(Buffer.from(c)), close: async () => undefined, abort: async () => undefined }),
              }).then(resolve, reject),
            150,
          ),
        retract: () => undefined,
        failed: reject,
      })
      view.filter = (b) => stage.filter(b)
    })
    view.send('sz /tmp/e2e-z.bin\r')
    const res = await done.catch((e) => {
      if (process.env.TT_DEBUG) console.log('recent chunks', view.recent?.map((c) => JSON.stringify(c)))
      throw e
    })
    view.filter = (b) => b
    assert.deepEqual(res.files, ['e2e-z.bin'])
    assert.equal(sha(Buffer.concat(parts)), want)
    await view.settle()
  })

  it('rz ← ZMODEM send of 20 MB (more than the server’s 8 MiB input queue) arrives intact', async () => {
    sh('rm -rf /tmp/e2e-rz && mkdir -p /tmp/e2e-rz')
    const big = crypto.randomBytes(20 * 1024 * 1024)
    const done = new Promise((resolve, reject) => {
      const stage = new ZmodemStage({
        send: (b) => view.send(b),
        writeAsync: () => undefined,
        detect: (d) => setTimeout(() => zmodemSend(d.confirm(), [{ name: 'big.bin', blob: new Blob([big]) }]).then(resolve, reject), 150),
        retract: () => undefined,
        failed: reject,
      })
      view.filter = (b) => stage.filter(b)
    })
    view.send('cd /tmp/e2e-rz && rz; cd /tmp\r')
    const res = await done
    view.filter = (b) => b
    assert.deepEqual(res.files, ['big.bin'])
    await view.settle()
    assert.equal(sh('sha256sum /tmp/e2e-rz/big.bin').split(' ')[0], sha(big))
    assert.deepEqual(view.errors, [], 'no "input buffer full" from the server')
  })

  it('tsz → trzsz download and trz ← 12 MB trzsz upload', async () => {
    sh('head -c 3000000 /dev/urandom > /tmp/e2e-t.bin && rm -rf /tmp/e2e-tu && mkdir -p /tmp/e2e-tu')
    const want = sh('sha256sum /tmp/e2e-t.bin').split(' ')[0]
    const delivered = []
    let ended
    const stage = new TrzszStage({
      send: (d) => view.send(d),
      writeAsync: (b) => (view.text += Buffer.from(b).toString('latin1')),
      columns: 120,
      hooks: {
        claim: async () => true,
        chooseTarget: async () => ({ create: () => new DownloadTarget((blob, name) => delivered.push({ blob, name })) }),
        chooseFiles: async () => [{ relPath: ['up.bin'], isDir: false, blob: new Blob([upload]) }],
        start() {},
        progress() {},
        end: (r) => ended?.(r),
      },
    })
    const upload = crypto.randomBytes(12 * 1024 * 1024)
    view.filter = (b) => stage.filter(b)
    let r = await new Promise((resolve) => {
      ended = resolve
      view.send('tsz /tmp/e2e-t.bin\r')
    })
    assert.equal(r.ok, true, r.error)
    assert.equal(sha(Buffer.from(await delivered[0].blob.arrayBuffer())), want)
    await view.settle()
    r = await new Promise((resolve) => {
      ended = resolve
      view.send('cd /tmp/e2e-tu && trz; cd /tmp\r')
    })
    assert.equal(r.ok, true, r.error)
    await view.settle()
    view.filter = (b) => b
    stage.dispose()
    assert.equal(sh('sha256sum /tmp/e2e-tu/up.bin').split(' ')[0], sha(upload))
    assert.deepEqual(view.errors, [])
  })

  it('send file: text line by line with wait-for-prompt, and raw binary through a raw tty', async () => {
    // Text: typed into cat, each line after the shell-like prompt of `read`.
    const lines = Array.from({ length: 40 }, (_, i) => `line ${i} ${'x'.repeat(i)}`)
    sh('rm -f /tmp/e2e-text.txt /tmp/e2e-raw.bin')
    view.send('while IFS= read -r -p "> " l; do printf "%s\\n" "$l" >> /tmp/e2e-text.txt; done; echo END"TEXT"\r')
    await until('read prompt', () => view.text.endsWith('> '))
    await sendTextPaced(`${lines.join('\n')}\n`, { eol: '\r', lineDelayMs: 0, charDelayMs: 0, prompt: { re: /> $/, timeoutMs: 5000, quietMs: 30 } }, {
      send: (s) => view.send(s),
      clock: timeoutClock(),
      watcher: view.watcher,
    })
    view.send('\x04')
    await until('text done', () => view.text.includes('ENDTEXT'))
    assert.equal(sh('cat /tmp/e2e-text.txt'), `${lines.join('\n')}\n`)

    // Binary: a raw tty passes every byte value unchanged.
    const bin = crypto.randomBytes(200_000)
    view.send('stty raw -echo; head -c 200000 > /tmp/e2e-raw.bin; stty sane; echo END"RAW"\r')
    await new Promise((r) => setTimeout(r, 500))
    await sendBinaryPaced(new Blob([bin]), { chunkBytes: 4096, delayMs: 2 }, { send: (b) => view.send(b), clock: timeoutClock() })
    await until('raw done', () => view.text.includes('ENDRAW'))
    assert.equal(sh('sha256sum /tmp/e2e-raw.bin').split(' ')[0], sha(bin))
  })
})
