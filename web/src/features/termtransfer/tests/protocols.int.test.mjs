/*
 * Integration tests of the transfer engines against the real tools: lrzsz (sz / rz) and trzsz-go (tsz / trz), run in
 * a throwaway container through `docker exec -i … script` (a PTY, like an SSH session). Guarded by NEXTERM_TESTENV=1.
 *
 *   docker build -t nexterm-termtransfer-ssh:test -f src/features/termtransfer/tests/testenv.Dockerfile src/features/termtransfer/tests
 *   docker run -d --name nexterm-termtransfer-ssh -p 127.0.0.1:23050:22 nexterm-termtransfer-ssh:test
 *   cd web && NEXTERM_TESTENV=1 node --import ./src/features/termtransfer/tests/register.mjs --test \
 *     src/features/termtransfer/tests/protocols.int.test.mjs
 *
 * NEXTERM_TT_CONTAINER overrides the container name.
 */
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import crypto from 'node:crypto'
import { describe, it } from 'node:test'
import { DownloadTarget } from '../engine/save.ts'
import { TrzszStage } from '../engine/trzsz.ts'
import { ZmodemStage, zmodemReceive, zmodemSend } from '../engine/zmodem.ts'

const ENABLED = process.env.NEXTERM_TESTENV === '1'
const C = process.env.NEXTERM_TT_CONTAINER || 'nexterm-termtransfer-ssh'
const sha = (b) => crypto.createHash('sha256').update(b).digest('hex')
const sh = (cmd) => execFileSync('docker', ['exec', C, 'sh', '-c', cmd]).toString()

/** Run `cmd` in the container on a PTY; returns {proc, write}. */
function pty(cmd) {
  const proc = spawn('docker', ['exec', '-i', C, 'script', '-qfc', cmd, '/dev/null'])
  return { proc, write: (b) => proc.stdin.write(typeof b === 'string' ? b : Buffer.from(b)) }
}

function withTimeout(p, ms, what) {
  return Promise.race([p, new Promise((_, rej) => setTimeout(() => rej(new Error(`timeout: ${what}`)), ms))])
}

describe('ZMODEM against lrzsz', { skip: !ENABLED && 'set NEXTERM_TESTENV=1' }, () => {
  it('receives files from sz (binary content, several files, mtime)', async () => {
    sh('rm -rf /tmp/tt-z && mkdir -p /tmp/tt-z && head -c 2500000 /dev/urandom > /tmp/tt-z/a.bin && printf "hello\\n" > /tmp/tt-z/b.txt && touch -d "2020-01-02 03:04:05" /tmp/tt-z/b.txt')
    const want = { 'a.bin': sh('sha256sum /tmp/tt-z/a.bin').split(' ')[0], 'b.txt': sha('hello\n') }
    const { proc, write } = pty('cd /tmp/tt-z && sz a.bin b.txt')
    const got = {}
    const offers = []
    let rendered = ''
    const done = new Promise((resolve, reject) => {
      const stage = new ZmodemStage({
        send: write,
        writeAsync: (b) => (rendered += Buffer.from(b).toString('latin1')),
        detect: (d) => {
          assert.equal(d.get_session_role(), 'receive')
          setTimeout(() => {
            zmodemReceive(d.confirm(), {
              open: async (info) => {
                offers.push(info)
                const parts = []
                return {
                  localName: info.path[0],
                  write: async (c) => void parts.push(Buffer.from(c)),
                  close: async () => void (got[info.name] = sha(Buffer.concat(parts))),
                  abort: async () => undefined,
                }
              },
            }).then(resolve, reject)
          }, 50)
        },
        retract: () => undefined,
        failed: reject,
      })
      proc.stdout.on('data', (d) => (rendered += Buffer.from(stage.filter(new Uint8Array(d))).toString('latin1')))
    })
    try {
      const res = await withTimeout(done, 60_000, 'sz')
      assert.deepEqual(res.files, ['a.bin', 'b.txt'])
      assert.equal(res.bytes, 2500006)
      assert.deepEqual(got, want)
      assert.equal(offers[1].mtimeMs, Date.UTC(2020, 0, 2, 3, 4, 5))
      assert.ok(!rendered.includes('B00000000000000'), 'the ZRQINIT header is hidden')
    } finally {
      proc.kill()
    }
  })

  it('sends files to rz with windowed ZCRCQ/ZACK flow control (bigger than the 1 MiB window)', async () => {
    sh('rm -rf /tmp/tt-zu && mkdir -p /tmp/tt-zu')
    const big = crypto.randomBytes(5 * 1024 * 1024 + 123)
    const { proc, write } = pty('cd /tmp/tt-zu && rz')
    let inflightMax = 0
    let sent = 0
    const done = new Promise((resolve, reject) => {
      const stage = new ZmodemStage({
        send: (b) => {
          sent += b.length
          write(b)
        },
        writeAsync: () => undefined,
        detect: (d) => {
          assert.equal(d.get_session_role(), 'send')
          setTimeout(() => {
            zmodemSend(
              d.confirm(),
              [
                { name: 'big.bin', blob: new Blob([big]), mtimeMs: Date.UTC(2021, 5, 6) },
                { name: 'empty', blob: new Blob([]) },
                { name: 'ünï code.txt', blob: new Blob(['x']) },
              ],
              { progress: (p) => (inflightMax = Math.max(inflightMax, sent - p.bytes)) },
            ).then(resolve, reject)
          }, 50)
        },
        retract: () => undefined,
        failed: reject,
      })
      proc.stdout.on('data', (d) => stage.filter(new Uint8Array(d)))
    })
    try {
      const res = await withTimeout(done, 90_000, 'rz')
      assert.deepEqual(res.files, ['big.bin', 'empty', 'ünï code.txt'])
      await new Promise((r) => setTimeout(r, 300))
      assert.equal(sh('sha256sum /tmp/tt-zu/big.bin').split(' ')[0], sha(big))
      assert.equal(sh('stat -c %s /tmp/tt-zu/empty').trim(), '0')
      assert.equal(sh('cat "/tmp/tt-zu/ünï code.txt"'), 'x')
      // Wire bytes in flight stay near the window (ZDLE escaping adds a little).
      assert.ok(inflightMax < 3 * 1024 * 1024, `in flight ${inflightMax}`)
    } finally {
      proc.kill()
    }
  })

  it('cancels a running receive with the CAN sequence (sz exits)', async () => {
    sh('rm -rf /tmp/tt-zc && mkdir -p /tmp/tt-zc && head -c 30000000 /dev/zero > /tmp/tt-zc/big')
    const { proc, write } = pty('cd /tmp/tt-zc && sz big; echo "SZ-EXIT=$?"')
    let text = ''
    const ac = new AbortController()
    const done = new Promise((resolve) => {
      const stage = new ZmodemStage({
        send: write,
        writeAsync: (b) => (text += Buffer.from(b).toString('latin1')),
        detect: (d) => {
          setTimeout(() => {
            zmodemReceive(d.confirm(), {
              signal: ac.signal,
              open: async () => ({ localName: 'big', write: async () => undefined, close: async () => undefined, abort: async () => undefined }),
              progress: (p) => {
                if (p.bytes > 500_000) ac.abort()
              },
            }).then(
              () => resolve('resolved'),
              (e) => resolve(e.name),
            )
          }, 50)
        },
        retract: () => undefined,
        failed: () => undefined,
      })
      proc.stdout.on('data', (d) => (text += Buffer.from(stage.filter(new Uint8Array(d))).toString('latin1')))
    })
    try {
      assert.equal(await withTimeout(done, 30_000, 'cancel'), 'AbortError')
      await withTimeout(
        new Promise((r) => {
          const t = setInterval(() => {
            if (/SZ-EXIT=\d+/.test(text)) {
              clearInterval(t)
              r()
            }
          }, 100)
        }),
        20_000,
        'sz exit',
      )
      assert.match(text, /SZ-EXIT=[1-9]/, 'sz reports the aborted transfer')
    } finally {
      proc.kill()
    }
  })
})

describe('trzsz against trzsz-go', { skip: !ENABLED && 'set NEXTERM_TESTENV=1' }, () => {
  function run(cmd, hooks) {
    const { proc, write } = pty(cmd)
    let rendered = ''
    let stage
    const ended = new Promise((resolve) => {
      stage = new TrzszStage({
        send: write,
        writeAsync: (b) => (rendered += Buffer.from(b).toString('utf8')),
        columns: 100,
        hooks: { claim: async () => true, chooseTarget: async () => null, chooseFiles: async () => null, start() {}, progress() {}, ...hooks, end: (r) => resolve(r) },
      })
      proc.stdout.on('data', (d) => (rendered += Buffer.from(stage.filter(new Uint8Array(d))).toString('utf8')))
    })
    return { proc, ended, rendered: () => rendered }
  }

  for (const mode of ['', '-b']) {
    it(`downloads files with tsz ${mode || '(base64)'}`, async () => {
      sh(`rm -rf /tmp/tt-t && mkdir -p /tmp/tt-t && head -c 3000000 /dev/urandom > /tmp/tt-t/x.bin && echo hi > /tmp/tt-t/y.txt`)
      const want = sh('cd /tmp/tt-t && sha256sum x.bin y.txt')
      const delivered = []
      const r = run(`cd /tmp/tt-t && tsz ${mode} x.bin y.txt`, {
        chooseTarget: async () => ({ create: () => new DownloadTarget((blob, name) => delivered.push({ blob, name })) }),
      })
      try {
        const res = await withTimeout(r.ended, 60_000, 'tsz')
        assert.equal(res.ok, true, res.error)
        assert.deepEqual(res.names, ['x.bin', 'y.txt'])
        for (const d of delivered) {
          const h = sha(Buffer.from(await d.blob.arrayBuffer()))
          assert.ok(want.includes(`${h}  ${d.name}`), `${d.name} matches`)
        }
        // tsz prints the summary we sent after 500 ms of silence.
        await withTimeout(
          new Promise((res2) => {
            const t = setInterval(() => {
              if (/Saved 2 files\/directories to Downloads/.test(r.rendered())) {
                clearInterval(t)
                res2()
              }
            }, 100)
          }),
          10_000,
          'tsz summary',
        )
      } finally {
        r.proc.kill()
      }
    })
  }

  it('downloads a folder with tsz -d as one ZIP', async () => {
    sh('rm -rf /tmp/tt-td && mkdir -p /tmp/tt-td/proj/src /tmp/tt-td/proj/empty && echo code > /tmp/tt-td/proj/src/m.c')
    const delivered = []
    const r = run('cd /tmp/tt-td && tsz -d proj', {
      chooseTarget: async () => ({ create: ({ directory }) => new DownloadTarget((blob, name) => delivered.push({ blob, name }), directory ? (t) => `${t[0]}.zip` : undefined) }),
    })
    try {
      const res = await withTimeout(r.ended, 60_000, 'tsz -d')
      assert.equal(res.ok, true, res.error)
      assert.equal(delivered.length, 1)
      assert.equal(delivered[0].name, 'proj.zip')
      const zip = Buffer.from(await delivered[0].blob.arrayBuffer()).toString('latin1')
      assert.ok(zip.includes('proj/src/m.c') && zip.includes('proj/empty/') && zip.includes('code\n'))
    } finally {
      r.proc.kill()
    }
  })

  it('uploads files and folders with trz -d (chunks capped below the server input queue)', async () => {
    sh('rm -rf /tmp/tt-tu && mkdir -p /tmp/tt-tu')
    const big = crypto.randomBytes(12 * 1024 * 1024)
    let maxWrite = 0
    const { proc, write } = pty('cd /tmp/tt-tu && trz -d')
    const ended = new Promise((resolve) => {
      const stage = new TrzszStage({
        send: (b) => {
          maxWrite = Math.max(maxWrite, typeof b === 'string' ? b.length : b.length)
          write(b)
        },
        writeAsync: () => undefined,
        columns: 80,
        hooks: {
          claim: async () => true,
          chooseTarget: async () => null,
          chooseFiles: async ({ directory }) => {
            assert.equal(directory, true)
            return [
              { relPath: ['up', 'big.bin'], isDir: false, blob: new Blob([big]) },
              { relPath: ['up', 'deep', 'n.txt'], isDir: false, blob: new Blob(['nested\n']) },
              { relPath: ['up', 'hollow'], isDir: true },
            ]
          },
          start() {},
          progress() {},
          end: resolve,
        },
      })
      proc.stdout.on('data', (d) => stage.filter(new Uint8Array(d)))
    })
    try {
      const res = await withTimeout(ended, 120_000, 'trz -d')
      assert.equal(res.ok, true, res.error)
      assert.deepEqual(res.names, ['up'])
      assert.equal(sh('sha256sum /tmp/tt-tu/up/big.bin').split(' ')[0], sha(big))
      assert.equal(sh('cat /tmp/tt-tu/up/deep/n.txt'), 'nested\n')
      assert.ok(sh('test -d /tmp/tt-tu/up/hollow && echo yes').includes('yes'))
      // 1 MiB chunks, base64 (+ deflate header) — well below the server's 8 MiB input queue.
      assert.ok(maxWrite < 2 * 1024 * 1024, `largest write ${maxWrite}`)
    } finally {
      proc.kill()
    }
  })

  it('stops a transfer with stop() (Ctrl+C semantics)', async () => {
    sh('rm -rf /tmp/tt-ts && mkdir -p /tmp/tt-ts && head -c 40000000 /dev/urandom > /tmp/tt-ts/huge')
    let stage
    const { proc, write } = pty('cd /tmp/tt-ts && tsz huge; echo "TSZ-EXIT=$?"')
    let text = ''
    const ended = new Promise((resolve) => {
      stage = new TrzszStage({
        send: write,
        writeAsync: (b) => (text += Buffer.from(b).toString('utf8')),
        columns: 80,
        hooks: {
          claim: async () => true,
          chooseTarget: async () => ({ create: () => new DownloadTarget(() => undefined) }),
          chooseFiles: async () => null,
          start() {},
          progress: (p) => {
            if (p.bytes > 1_000_000) stage.stop()
          },
          end: resolve,
        },
      })
      proc.stdout.on('data', (d) => (text += Buffer.from(stage.filter(new Uint8Array(d))).toString('utf8')))
    })
    try {
      const res = await withTimeout(ended, 60_000, 'stop')
      assert.equal(res.ok, false)
      assert.equal(res.canceled, true)
      await withTimeout(
        new Promise((r) => {
          const t = setInterval(() => {
            if (/TSZ-EXIT=\d+/.test(text)) {
              clearInterval(t)
              r()
            }
          }, 100)
        }),
        30_000,
        'tsz exit',
      )
    } finally {
      proc.kill()
    }
  })
})
