/*
 * Reviewer regression tests (false-positive detection, cancel, drop paths):
 *   cd web && node --import ./src/features/termtransfer/tests/register.mjs --test 'src/features/termtransfer/tests/*.test.mjs'
 */
import assert from 'node:assert/strict'
import { describe, it } from 'node:test'
import Zmodem from 'zmodem.js'
import { TrzszStage, hasVisibleText } from '../engine/trzsz.ts'
import { ZmodemStage } from '../engine/zmodem.ts'
import { OutputMux } from '../engine/mux.ts'
import { concatBytes } from '../engine/bytes.ts'

const enc = (s) => new TextEncoder().encode(s)
const dec = (b) => new TextDecoder().decode(b)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function trzszStage(ev, sent, late) {
  return new TrzszStage({
    send: (d) => sent.push(typeof d === 'string' ? d : dec(d)),
    writeAsync: (b) => late.push(dec(b)),
    columns: 80,
    hooks: {
      claim: async () => {
        ev.push('claim')
        return true
      },
      chooseTarget: async () => {
        ev.push('choose')
        await sleep(1000)
        return null
      },
      chooseFiles: async () => null,
      start() {},
      progress() {},
      end: () => ev.push('end'),
    },
  })
}

describe('trzsz false positives', () => {
  it('a printed magic followed by text (cat of a log) never starts a transfer or types into the shell', async () => {
    const ev = []
    const sent = []
    const late = []
    const st = trzszStage(ev, sent, late)
    const out1 = dec(st.filter(enc('$ cat notes.txt\r\nsee ::TRZSZ:TRANSFER:S:1.1.6:1700000000000\r\n')))
    await sleep(10)
    const out2 = dec(st.filter(enc('more lines\r\nbob@host:~$ ')))
    await sleep(300)
    assert.match(out1, /cat notes\.txt/)
    assert.deepEqual(ev, [], 'no claim, no prompt')
    assert.deepEqual(sent, [], 'nothing typed into the shell')
    assert.equal(out2 + late.join(''), 'more lines\r\nbob@host:~$ ', 'the following output is shown')
    assert.equal(st.busy, false)
    // The terminal keeps working normally afterwards.
    assert.equal(dec(st.filter(enc('ls\r\n'))), 'ls\r\n')
    st.dispose()
  })

  it('a real trz / tsz (silence after the magic, maybe cursor control) is answered', async () => {
    const ev = []
    const sent = []
    const late = []
    const st = trzszStage(ev, sent, late)
    st.filter(enc('\x1b7\x07::TRZSZ:TRANSFER:S:1.1.6:1700000000001\r\n'))
    await sleep(10)
    st.filter(enc('\x1b8\x1b[0J'))
    await sleep(250)
    assert.deepEqual(ev, ['claim', 'choose'])
    st.dispose()
  })

  it('a magic followed by the prompt in the same chunk (printf / cat) is ignored', async () => {
    const ev = []
    const sent = []
    const late = []
    const st = trzszStage(ev, sent, late)
    const out = dec(st.filter(enc('::TRZSZ:TRANSFER:S:1.1.6:1700000000002\nbob@host:~$ ')))
    await sleep(250)
    assert.match(out, /bob@host/)
    assert.deepEqual(ev, [])
    assert.deepEqual(sent, [])
    assert.equal(st.busy, false)
    st.dispose()
  })

  it('a confirmed transfer the server never starts ends instead of blocking the terminal', async () => {
    const ended = []
    const sent = []
    const st = new TrzszStage({
      send: (d) => sent.push(d),
      writeAsync: () => undefined,
      columns: 80,
      detectGraceMs: 0,
      negotiationTimeoutMs: 150,
      hooks: {
        claim: async () => true,
        chooseTarget: async () => ({ create: () => ({ kind: 'downloads', label: 'x', reserve: async (n) => n, mkdir: async () => undefined, openFile: async () => ({}), finish: async () => undefined, abort: async () => undefined }) }),
        chooseFiles: async () => null,
        start() {},
        progress() {},
        end: (r) => ended.push(r),
      },
    })
    st.filter(enc('::TRZSZ:TRANSFER:S:1.1.6:1700000000003\r\n'))
    await sleep(300)
    assert.equal(sent.length >= 1, true, 'the answer was sent')
    // The library waits for the input to go quiet before it reports the failure to the server.
    for (let i = 0; i < 40 && st.busy; i++) await sleep(100)
    assert.equal(ended.length, 1)
    assert.equal(ended[0].ok, false)
    assert.equal(st.busy, false, 'the terminal is released')
    st.dispose()
  })

  it('recognises visible text', () => {
    assert.equal(hasVisibleText(enc('\x1b8\x1b[0J\r\n\x07')), false)
    assert.equal(hasVisibleText(enc('\x1b]0;title\x07')), false)
    assert.equal(hasVisibleText(enc('$ ')), true)
  })
})

describe('zmodem false positives', () => {
  const ZRQINIT = Uint8Array.from(Zmodem.Header.build('ZRQINIT').to_hex())
  it('binary output with an embedded header (cat of a capture) never hijacks the terminal', () => {
    const ev = []
    const st = new ZmodemStage({
      send: (b) => ev.push(['send', b.length]),
      writeAsync: () => undefined,
      detect: (d) => ev.push(['detect', d]),
      retract: () => ev.push(['retract']),
      failed: (e) => ev.push(['failed', String(e)]),
    })
    const junk = new Uint8Array(4096).map((_, i) => (i * 131) & 0xff)
    const chunk = concatBytes([junk, ZRQINIT, junk])
    const out = st.filter(chunk)
    assert.equal(out.length, chunk.length, 'every byte is rendered')
    assert.equal(ev.filter((e) => e[0] === 'detect' || e[0] === 'send').length, 0)
    assert.equal(st.busy, false)
    st.dispose()
  })

  it('a header at a chunk end followed by the prompt is retracted', () => {
    const ev = []
    const st = new ZmodemStage({
      send: (b) => ev.push(['send', b.length]),
      writeAsync: () => undefined,
      detect: (d) => ev.push(['detect', d]),
      retract: () => ev.push(['retract']),
      failed: () => undefined,
    })
    st.filter(concatBytes([enc('garbage '), ZRQINIT]))
    assert.equal(dec(st.filter(enc('bob@host:~$ '))), 'bob@host:~$ ')
    assert.deepEqual(ev.map((e) => e[0]), ['detect', 'retract'])
    assert.equal(ev[0][1].is_valid(), false)
    assert.equal(ev.some((e) => e[0] === 'send'), false, 'nothing sent to the shell')
    st.dispose()
  })
})

describe('mux', () => {
  it('replayed history is never inspected (an old sz in the scrollback does not start a transfer)', () => {
    const mux = new OutputMux()
    let fed = 0
    const stage = {
      busy: false,
      filter(b) {
        fed += b.length
        return b
      },
    }
    const b = enc('rz\r**\x18B00000000000000\r\n')
    assert.equal(mux.filter(b, { replay: true, follower: false }, [stage]).length, b.length)
    assert.equal(fed, 0)
  })
})

describe('typing dropped paths', async () => {
  const { quoteShellPath, quoteWindowsPath } = await import('../engine/names.ts')
  it('never types a raw newline or escape (the line would run)', () => {
    const q = quoteShellPath('/tmp/a\nrm -rf ~\x1b[A')
    // oxlint-disable-next-line no-control-regex
    assert.equal(/[\n\r\x1b]/.test(q), false)
    assert.equal(q, "$'/tmp/a\\x0arm -rf ~\\x1b[A'")
    assert.equal(quoteShellPath("/tmp/x'\ty"), "$'/tmp/x\\'\\x09y'")
  })
  it('keeps PowerShell from expanding $ and backticks', () => {
    assert.equal(quoteWindowsPath('C:\\x\\$(calc).txt'), "'C:\\x\\$(calc).txt'")
    assert.equal(quoteWindowsPath("C:\\it's `x`"), "'C:\\it''s `x`'")
    assert.equal(quoteWindowsPath('C:\\My Files\\a.txt'), '"C:\\My Files\\a.txt"')
  })
})

describe('browser downloads', async () => {
  const { DownloadTarget } = await import('../engine/save.ts')
  it('large downloads are folded into Blob segments and stay byte-exact (plain and ZIP)', async () => {
    const got = []
    const t = new DownloadTarget((blob, name) => got.push({ blob, name }))
    const f = await t.openFile(['big.bin'], { size: 20 * 1024 * 1024 })
    const buf = new Uint8Array(1024 * 1024)
    let expect = 0
    for (let i = 0; i < 20; i++) {
      buf.fill(i)
      await f.write(buf) // the same buffer is reused, like a parser would
      expect += buf.length
    }
    await f.close()
    await t.finish()
    assert.equal(got.length, 1)
    const data = new Uint8Array(await got[0].blob.arrayBuffer())
    assert.equal(data.length, expect)
    for (let i = 0; i < 20; i++) assert.equal(data[i * 1024 * 1024 + 7], i)

    const zipped = []
    const z = new DownloadTarget((blob, name) => zipped.push({ blob, name }), () => 'x.zip')
    const zf = await z.openFile(['dir', 'a.bin'], {})
    for (let i = 0; i < 10; i++) {
      buf.fill(i + 1)
      await zf.write(buf)
    }
    await zf.close()
    await z.finish()
    const zbytes = new Uint8Array(await zipped[0].blob.arrayBuffer())
    assert.ok(zbytes.length > 10 * 1024 * 1024)
    // Local header CRC must equal the CRC of the data (unzip -t is exercised in engine.test.mjs; here: header size).
    const dv = new DataView(zbytes.buffer)
    assert.equal(dv.getUint32(0, true), 0x04034b50)
  })
})
