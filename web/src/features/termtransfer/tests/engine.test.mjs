/*
 * Unit tests of the term-transfer engine (pure modules, no DOM):
 *   cd web && node --import ./src/features/termtransfer/tests/register.mjs --test src/features/termtransfer/tests/
 */
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { describe, it } from 'node:test'
import Zmodem from 'zmodem.js'

import { concatBytes, indexOfBytes, lastIndexOfBytes, latin1, toBytes } from '../engine/bytes.ts'
import { displayRemotePath, joinRemotePath, quoteShellPath, quoteWindowsPath, sanitizeFileName, splitSafePath, uniqueName } from '../engine/names.ts'
import { crc32, ZipTooLargeError, ZipWriter } from '../engine/zip.ts'
import { DownloadTarget, FolderTarget } from '../engine/save.ts'
import { advance, overallFraction } from '../engine/progress.ts'
import {
  AbortedError,
  NotConnectedError,
  PromptWatcher,
  compilePromptPattern,
  sendBinaryPaced,
  sendTextPaced,
  serialPacing,
  splitLines,
  stripAnsi,
  timeoutClock,
} from '../engine/pacer.ts'
import { TransferArbiter } from '../engine/lock.ts'
import { OutputMux, textTail } from '../engine/mux.ts'
import { formatSaved, isTrzszStopped, TrzszStage, trzszHoldIndex, uploadReaders } from '../engine/trzsz.ts'
import { stripSessionHeader, ZmodemStage } from '../engine/zmodem.ts'

const enc = (s) => new TextEncoder().encode(s)
const dec = (b) => new TextDecoder().decode(b)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// ---------------------------------------------------------------------------------------------------------------------

describe('bytes', () => {
  it('finds subarrays', () => {
    const hay = enc('abcabcXYZabc')
    assert.equal(indexOfBytes(hay, enc('XYZ')), 6)
    assert.equal(indexOfBytes(hay, enc('abc'), 1), 3)
    assert.equal(indexOfBytes(hay, enc('nope')), -1)
    assert.equal(indexOfBytes(hay, enc('')), 0)
    assert.equal(lastIndexOfBytes(hay, enc('abc')), 9)
    assert.equal(lastIndexOfBytes(hay, enc('zzz')), -1)
    assert.equal(lastIndexOfBytes(enc('ab'), enc('abc')), -1)
  })
  it('concatenates and converts', () => {
    assert.equal(dec(concatBytes([enc('a'), enc('bc'), enc('')])), 'abc')
    const one = enc('x')
    assert.equal(concatBytes([one]), one)
    assert.deepEqual(Array.from(toBytes('é')), [0xc3, 0xa9])
    assert.deepEqual(Array.from(toBytes([1, 2, 255])), [1, 2, 255])
    const buf = new Uint8Array([9, 8, 7, 6])
    assert.deepEqual(Array.from(toBytes(new DataView(buf.buffer, 1, 2))), [8, 7])
    assert.equal(latin1(new Uint8Array([0x41, 0xe9, 0x42])), 'A\u00e9B')
  })
})

// ---------------------------------------------------------------------------------------------------------------------

describe('names', () => {
  it('sanitizes untrusted names', () => {
    assert.equal(sanitizeFileName('report.pdf'), 'report.pdf')
    assert.equal(sanitizeFileName('../../etc/passwd'), '.._.._etc_passwd')
    assert.equal(sanitizeFileName('..'), 'file')
    assert.equal(sanitizeFileName('.'), 'file')
    assert.equal(sanitizeFileName(''), 'file')
    assert.equal(sanitizeFileName('a\u0000b\u001bc\u009bd'), 'abcd')
    assert.equal(sanitizeFileName('x:y*z?"<>|'), 'x_y_z_____')
    assert.equal(sanitizeFileName('name. . .'), 'name')
    assert.equal(sanitizeFileName('CON'), '_CON')
    assert.equal(sanitizeFileName('lpt1.txt'), '_lpt1.txt')
    assert.equal(sanitizeFileName('evil\u202egnp.exe'), 'evilgnp.exe')
    const long = sanitizeFileName(`${'a'.repeat(300)}.tar.gz`)
    assert.ok(long.length <= 200 && long.endsWith('.gz'), long)
  })
  it('splits remote paths without escaping the destination', () => {
    assert.deepEqual(splitSafePath('/abs/dir/file.txt'), ['abs', 'dir', 'file.txt'])
    assert.deepEqual(splitSafePath('../../x'), ['x'])
    assert.deepEqual(splitSafePath('a\\b/./c'), ['a', 'b', 'c'])
    assert.deepEqual(splitSafePath('../..'), ['file'])
    assert.deepEqual(splitSafePath(''), ['file'])
  })
  it('makes names unique', () => {
    const taken = new Set(['a.txt', 'a (1).txt', 'dir', '.bashrc'])
    assert.equal(uniqueName('b.txt', (n) => taken.has(n)), 'b.txt')
    assert.equal(uniqueName('a.txt', (n) => taken.has(n)), 'a (2).txt')
    assert.equal(uniqueName('dir', (n) => taken.has(n), true), 'dir (1)')
    assert.equal(uniqueName('.bashrc', (n) => taken.has(n)), '.bashrc (1)')
    assert.equal(uniqueName('v1.2.tar', (n) => n === 'v1.2.tar'), 'v1.2 (1).tar')
  })
  it('quotes paths for shells', () => {
    assert.equal(quoteShellPath('/home/u/file.txt'), '/home/u/file.txt')
    assert.equal(quoteShellPath('/home/u/my file'), "'/home/u/my file'")
    assert.equal(quoteShellPath("/tmp/it's"), "'/tmp/it'\\''s'")
    assert.equal(quoteShellPath('/tmp/$(rm -rf ~)'), "'/tmp/$(rm -rf ~)'")
    assert.equal(quoteWindowsPath('C:\\Users\\me\\a.txt'), 'C:\\Users\\me\\a.txt')
    assert.equal(quoteWindowsPath('C:\\My Files\\a.txt'), '"C:\\My Files\\a.txt"')
  })
  it('joins and displays remote paths', () => {
    assert.equal(joinRemotePath('/home/u', 'a/b'), '/home/u/a/b')
    assert.equal(joinRemotePath('/home/u/', '/a'), '/home/u/a')
    assert.equal(joinRemotePath('/', 'x'), '/x')
    assert.equal(displayRemotePath('/home/u/src', '/home/u'), '~/src')
    assert.equal(displayRemotePath('/home/u', '/home/u'), '~')
    assert.equal(displayRemotePath('/home/uu', '/home/u'), '/home/uu')
    assert.equal(displayRemotePath('/etc'), '/etc')
  })
})

// ---------------------------------------------------------------------------------------------------------------------

describe('zip', () => {
  it('computes CRC-32', () => {
    assert.equal(crc32(enc('123456789')), 0xcbf43926)
    assert.equal(crc32(new Uint8Array(0)), 0)
  })
  it('writes an archive unzip accepts (UTF-8 names, folders, empty files)', async (t) => {
    let unzip = true
    try {
      execFileSync('unzip', ['-v'], { stdio: 'ignore' })
    } catch {
      unzip = false
    }
    const z = new ZipWriter()
    z.addDirectory('top')
    z.addDirectory('top/empty')
    z.addFile('top/a.txt', [enc('hello '), enc('world\n')], Date.UTC(2024, 1, 29, 12, 30, 44))
    z.addFile('top/sub/ünïcode.bin', [new Uint8Array([0, 1, 2, 255])])
    z.addFile('top/zero', [])
    assert.equal(z.count, 5)
    const blob = z.toBlob()
    const bytes = new Uint8Array(await blob.arrayBuffer())
    assert.equal(bytes.length, z.size + 22)
    assert.equal(new DataView(bytes.buffer).getUint32(0, true), 0x04034b50)
    if (!unzip) {
      t.skip('unzip not installed')
      return
    }
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tt-zip-'))
    try {
      const file = path.join(dir, 't.zip')
      fs.writeFileSync(file, bytes)
      const out = execFileSync('unzip', ['-t', file]).toString()
      assert.match(out, /No errors detected/)
      execFileSync('unzip', ['-q', file, '-d', path.join(dir, 'x')])
      assert.equal(fs.readFileSync(path.join(dir, 'x/top/a.txt'), 'utf8'), 'hello world\n')
      assert.deepEqual(Array.from(fs.readFileSync(path.join(dir, 'x/top/sub/ünïcode.bin'))), [0, 1, 2, 255])
      assert.equal(fs.statSync(path.join(dir, 'x/top/zero')).size, 0)
      assert.ok(fs.statSync(path.join(dir, 'x/top/empty')).isDirectory())
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })
  it('refuses more than 65535 entries', () => {
    const z = new ZipWriter()
    for (let i = 0; i < 0xffff; i++) z.addDirectory(`d${i}`)
    assert.throws(() => z.addDirectory('one-more'), ZipTooLargeError)
  })
})

// ---------------------------------------------------------------------------------------------------------------------
// save targets

class FakeFile {
  constructor(dir, name) {
    this.kind = 'file'
    this.dir = dir
    this.name = name
  }
  async createWritable() {
    const entry = this.dir.entries.get(this.name)
    const parts = []
    return {
      write: async (d) => {
        parts.push(Uint8Array.from(d))
      },
      close: async () => {
        entry.data = concatBytes(parts)
      },
      abort: async () => {
        parts.length = 0
      },
    }
  }
}

class FakeDir {
  constructor(name) {
    this.kind = 'directory'
    this.name = name
    this.entries = new Map()
  }
  async getDirectoryHandle(name, opts = {}) {
    assert.ok(!/[\\/]/.test(name) && name !== '..' && name !== '.', `unsafe name reached the file system: ${name}`)
    const e = this.entries.get(name)
    if (e) {
      if (e.kind !== 'directory') throw Object.assign(new Error('type mismatch'), { name: 'TypeMismatchError' })
      return e
    }
    if (!opts.create) throw Object.assign(new Error('not found'), { name: 'NotFoundError' })
    const d = new FakeDir(name)
    this.entries.set(name, d)
    return d
  }
  async getFileHandle(name, opts = {}) {
    assert.ok(!/[\\/]/.test(name) && name !== '..' && name !== '.', `unsafe name reached the file system: ${name}`)
    const e = this.entries.get(name)
    if (e) {
      if (e.kind !== 'file') throw Object.assign(new Error('type mismatch'), { name: 'TypeMismatchError' })
      return new FakeFile(this, name)
    }
    if (!opts.create) throw Object.assign(new Error('not found'), { name: 'NotFoundError' })
    this.entries.set(name, { kind: 'file', data: new Uint8Array(0) })
    return new FakeFile(this, name)
  }
  async removeEntry(name) {
    this.entries.delete(name)
  }
  async *keys() {
    yield* this.entries.keys()
  }
}

describe('save targets', () => {
  it('writes into a folder, renames on conflicts, creates nested folders', async () => {
    const root = new FakeDir('Downloads')
    root.entries.set('a.txt', { kind: 'file', data: enc('old') })
    const t = new FolderTarget(root)
    assert.equal(t.label, 'Downloads')
    const n1 = await t.reserve('a.txt', { dir: false, overwrite: false })
    assert.equal(n1, 'a (1).txt')
    const f1 = await t.openFile([n1])
    await f1.write(enc('new'))
    await f1.close()
    const top = await t.reserve('tree', { dir: true, overwrite: false })
    await t.mkdir([top, 'empty'])
    const f2 = await t.openFile([top, 'x', 'y.bin'])
    await f2.write(new Uint8Array([1, 2]))
    await f2.close()
    await t.finish()
    assert.equal(dec(root.entries.get('a.txt').data), 'old')
    assert.equal(dec(root.entries.get('a (1).txt').data), 'new')
    const tree = root.entries.get('tree')
    assert.ok(tree.entries.get('empty') instanceof FakeDir)
    assert.deepEqual(Array.from(tree.entries.get('x').entries.get('y.bin').data), [1, 2])
    assert.deepEqual(t.saved, ['a (1).txt', 'tree'])
  })
  it('overwrites when asked and removes what a failed transfer created', async () => {
    const root = new FakeDir('R')
    root.entries.set('keep.txt', { kind: 'file', data: enc('keep') })
    const t = new FolderTarget(root)
    assert.equal(await t.reserve('keep.txt', { dir: false, overwrite: true }), 'keep.txt')
    const fresh = await t.reserve('fresh', { dir: true, overwrite: false })
    const f = await t.openFile([fresh, 'part.bin'])
    await f.write(enc('partial'))
    await t.abort()
    assert.ok(root.entries.has('keep.txt'), 'pre-existing entries survive an abort')
    assert.ok(!root.entries.has('fresh'), 'entries created by the transfer are removed')
  })
  it('delivers browser downloads per file and copies chunks (parsers reuse buffers)', async () => {
    const got = []
    const t = new DownloadTarget((blob, name) => got.push({ blob, name }))
    const n = await t.reserve('a.bin', { dir: false, overwrite: false })
    const n2 = await t.reserve('a.bin', { dir: false, overwrite: false })
    assert.equal(n2, 'a (1).bin')
    const f = await t.openFile([n])
    const reused = new Uint8Array([1, 2, 3])
    await f.write(reused)
    reused.set([9, 9, 9])
    await f.write(reused)
    await f.close()
    await t.finish()
    assert.equal(got.length, 1)
    assert.equal(got[0].name, 'a.bin')
    assert.deepEqual(Array.from(new Uint8Array(await got[0].blob.arrayBuffer())), [1, 2, 3, 9, 9, 9])
  })
  it('zips folder downloads into one archive', async () => {
    const got = []
    const t = new DownloadTarget((blob, name) => got.push({ blob, name }), (tops) => `${tops[0]}.zip`)
    const top = await t.reserve('proj', { dir: true, overwrite: false })
    await t.mkdir([top, 'empty'])
    const f = await t.openFile([top, 'src', 'main.c'])
    await f.write(enc('int main(){}\n'))
    await f.close()
    assert.equal(got.length, 0, 'nothing is delivered before the transfer finished')
    await t.finish()
    assert.equal(got.length, 1)
    assert.equal(got[0].name, 'proj.zip')
    const bytes = new Uint8Array(await got[0].blob.arrayBuffer())
    const text = latin1(bytes)
    assert.ok(text.includes('proj/src/main.c') && text.includes('proj/empty/') && text.includes('int main(){}'))
  })
  it('rejects files too large for memory', async () => {
    const t = new DownloadTarget(() => undefined)
    await assert.rejects(t.openFile(['huge'], { size: 3 * 1024 ** 3 }), /too large/)
  })
})

// ---------------------------------------------------------------------------------------------------------------------

describe('pacer', () => {
  it('splits lines like the server pacer', () => {
    assert.deepEqual(splitLines('a\nb\r\nc'), { lines: ['a', 'b', 'c'], endsWithNewline: false })
    assert.deepEqual(splitLines('a\n\nb\n'), { lines: ['a', '', 'b'], endsWithNewline: true })
    assert.deepEqual(splitLines('x\ry\r'), { lines: ['x', 'y'], endsWithNewline: true })
    assert.deepEqual(splitLines(''), { lines: [], endsWithNewline: false })
    assert.deepEqual(splitLines('\n'), { lines: [''], endsWithNewline: true })
  })
  it('strips escape sequences', () => {
    assert.equal(stripAnsi('\x1b[1;32muser@h\x1b[0m:\x1b]0;title\x07~$ '), 'user@h:~$ ')
    assert.equal(stripAnsi('a\x1bPq#0;2;0;0;0\x1b\\b\x1b=c\x1b(Bd\x1b7e'), 'abcde')
  })
  it('compiles prompt patterns', () => {
    assert.equal(compilePromptPattern('  '), null)
    assert.ok(compilePromptPattern('\\$ $').test('user@h:~$ '))
    assert.throws(() => compilePromptPattern('('))
  })
  it('waits for OSC 133 prompt marks (even split across chunks)', async () => {
    const w = new PromptWatcher()
    const since = w.mark()
    setTimeout(() => {
      w.feed(enc('output\r\n\x1b]13'))
      w.feed(enc('3;A\x07'))
    }, 20)
    assert.equal(await w.wait(since, { re: /NEVER/, timeoutMs: 2000, clock: timeoutClock() }), true)
  })
  it('waits for a quiet prompt-looking last line, not for the echo of the old one', async () => {
    const w = new PromptWatcher()
    w.feed(enc('host$ '))
    const since = w.mark()
    // Nothing new yet: the old prompt must not count.
    assert.equal(await w.wait(since, { re: /\$\s*$/, timeoutMs: 120, quietMs: 30, clock: timeoutClock() }), false)
    setTimeout(() => w.feed(enc('echo 1\r\n1\r\n\x1b[32mhost\x1b[0m$ ')), 10)
    assert.equal(await w.wait(since, { re: /\$\s*$/, timeoutMs: 2000, quietMs: 30, clock: timeoutClock() }), true)
  })
  it('types text line by line with delays, prompt waits and Enter handling', async () => {
    const sent = []
    const w = new PromptWatcher()
    const t0 = Date.now()
    const r = await sendTextPaced(
      'ls\npwd\n',
      { eol: '\r', lineDelayMs: 30, charDelayMs: 0, prompt: { re: /\$ $/, timeoutMs: 1000, quietMs: 20 } },
      {
        send: (s) => {
          sent.push(s)
          if (s === '\r') setTimeout(() => w.feed(enc('out\r\n$ ')), 5)
          return true
        },
        clock: timeoutClock(),
        watcher: w,
      },
    )
    assert.deepEqual(sent, ['ls', '\r', 'pwd', '\r'])
    assert.deepEqual(r, { lines: 2, warnings: 0 })
    assert.ok(Date.now() - t0 >= 30)
  })
  it('types per character and leaves the last line without Enter unless the text ends with one', async () => {
    const sent = []
    await sendTextPaced('ab\ncd', { eol: '\r\n', lineDelayMs: 0, charDelayMs: 1, prompt: null }, { send: (s) => sent.push(s) > 0, clock: timeoutClock() })
    assert.deepEqual(sent, ['a', 'b', '\r\n', 'c', 'd'])
  })
  it('reports prompt timeouts as warnings and stops on cancel / disconnect', async () => {
    const warnings = []
    const r = await sendTextPaced('a\nb\n', { eol: '\r', lineDelayMs: 0, charDelayMs: 0, prompt: { re: /\$ $/, timeoutMs: 40 } }, {
      send: () => true,
      clock: timeoutClock(),
      watcher: new PromptWatcher(),
      onWarning: (m) => warnings.push(m),
    })
    assert.equal(r.warnings, 1)
    assert.match(warnings[0], /Line 1: no prompt/)
    const ac = new AbortController()
    const p = sendTextPaced('1\n2\n3\n', { eol: '\r', lineDelayMs: 1000, charDelayMs: 0, prompt: null }, { send: () => true, clock: timeoutClock(), signal: ac.signal })
    setTimeout(() => ac.abort(), 20)
    await assert.rejects(p, AbortedError)
    await assert.rejects(sendTextPaced('x\n', { eol: '\r', lineDelayMs: 0, charDelayMs: 0, prompt: null }, { send: () => false, clock: timeoutClock() }), NotConnectedError)
  })
  it('streams binary in paced chunks', async () => {
    const data = new Uint8Array(2500).map((_, i) => i & 0xff)
    const chunks = []
    const progress = []
    const n = await sendBinaryPaced(new Blob([data]), { chunkBytes: 1000, delayMs: 1 }, {
      send: (b) => chunks.push(Uint8Array.from(b)) > 0,
      clock: timeoutClock(),
      onProgress: (d, t) => progress.push([d, t]),
    })
    assert.equal(n, 2500)
    assert.deepEqual(chunks.map((c) => c.length), [1000, 1000, 500])
    assert.deepEqual(Array.from(concatBytes(chunks)), Array.from(data))
    assert.deepEqual(progress.at(-1), [2500, 2500])
    await assert.rejects(
      sendBinaryPaced(new Blob([data]), { chunkBytes: 100, delayMs: 0 }, { send: () => true, clock: timeoutClock(), problem: () => 'input buffer full' }),
      /input buffer full/,
    )
    assert.deepEqual(serialPacing(115200), { chunkBytes: 64, delayMs: 6 })
  })
})

// ---------------------------------------------------------------------------------------------------------------------

class Bus {
  constructor() {
    this.members = new Set()
  }
  channel() {
    const bus = this
    const listeners = new Set()
    const ch = {
      postMessage(msg) {
        for (const m of bus.members) if (m !== ch) for (const l of m.listeners) setTimeout(() => l({ data: structuredClone(msg) }), 1)
      },
      addEventListener(_t, cb) {
        listeners.add(cb)
      },
      removeEventListener(_t, cb) {
        listeners.delete(cb)
      },
      close() {
        bus.members.delete(ch)
      },
      listeners,
    }
    bus.members.add(ch)
    return ch
  }
}

describe('transfer arbiter', () => {
  it('lets the preferred view of a page answer', async () => {
    const a = new TransferArbiter({ channel: null })
    let prioB = 0
    const v1 = { id: 'v1', sessionId: 's', priority: () => 1 }
    const v2 = { id: 'v2', sessionId: 's', priority: () => prioB }
    a.register(v1)
    a.register(v2)
    assert.equal(a.leaderInPage('s'), v1)
    prioB = 4
    assert.equal(await a.claim(v1), false, 'the active tab (v2) is preferred')
    assert.equal(await a.claim(v2), true)
    assert.equal(a.heldElsewhere(v1), true)
    assert.equal(await a.claim(v1), false)
    a.release(v2)
    assert.equal(a.heldElsewhere(v1), false)
    a.dispose()
  })
  it('picks one page across windows and hides the transfer from the others', async () => {
    const bus = new Bus()
    const p1 = new TransferArbiter({ channel: bus.channel(), pageId: 'aaa', claimWindowMs: 30, heartbeatMs: 20, staleMs: 80 })
    const p2 = new TransferArbiter({ channel: bus.channel(), pageId: 'bbb', claimWindowMs: 30, heartbeatMs: 20, staleMs: 80 })
    const o1 = { id: 'o1', sessionId: 'S', priority: () => 2 }
    const o2 = { id: 'o2', sessionId: 'S', priority: () => 2 }
    p1.register(o1)
    p2.register(o2)
    const [r1, r2] = await Promise.all([p1.claim(o1), p2.claim(o2)])
    assert.deepEqual([r1, r2], [true, false], 'equal priority: lowest page id wins')
    await sleep(40)
    assert.equal(p2.heldElsewhere(o2), true, 'heartbeats mark the session busy on the other page')
    p1.release(o1)
    await sleep(15)
    assert.equal(p2.heldElsewhere(o2), false, 'released')
    // Higher priority wins regardless of the page id.
    const o2hi = { id: 'o2hi', sessionId: 'T', priority: () => 7 }
    const o1lo = { id: 'o1lo', sessionId: 'T', priority: () => 1 }
    p1.register(o1lo)
    p2.register(o2hi)
    const [a1, a2] = await Promise.all([p1.claim(o1lo), p2.claim(o2hi)])
    assert.deepEqual([a1, a2], [false, true])
    // A page that vanishes without releasing goes stale.
    p2.dispose()
    await sleep(120)
    assert.equal(p1.heldElsewhere(o1lo), false)
    p1.dispose()
  })
})

// ---------------------------------------------------------------------------------------------------------------------

describe('output mux', () => {
  const upper = { busy: false, filter: (b) => enc(dec(b).toUpperCase()) }
  it('passes replayed history and swallows followers', () => {
    const m = new OutputMux()
    assert.equal(dec(m.filter(enc('hi'), { replay: true, follower: true }, [upper])), 'hi')
    assert.equal(m.filter(enc('hi'), { replay: false, follower: true }, [upper]).length, 0)
    assert.equal(dec(m.filter(enc('hi'), { replay: false, follower: false }, [upper, null])), 'HI')
  })
  it('keeps the text tail hidden while following another view', () => {
    const m = new OutputMux()
    assert.equal(m.filter(enc('\x18B0100\x00\x01data'), { replay: false, follower: true }, []).length, 0)
    assert.equal(m.filter(enc('\r\n$ '), { replay: false, follower: true }, []).length, 0)
    assert.equal(dec(m.takeFollowerTail()), '$ ')
    assert.equal(m.takeFollowerTail().length, 0)
  })
  it('gives a busy stage every byte', () => {
    const seen = []
    const busy = { busy: true, filter: (b) => (seen.push(dec(b)), new Uint8Array(0)) }
    const m = new OutputMux()
    assert.equal(m.filter(enc('data'), { replay: false, follower: false }, [upper, busy]).length, 0)
    assert.deepEqual(seen, ['data'])
  })
  it('keeps the text lines that follow the sender’s garbage', async () => {
    assert.equal(dec(textTail(enc('\x18i\x00junk\x18CXYZ\r\nexit=128\r\n\x1b[32mhost$ '))), 'exit=128\r\n\x1b[32mhost$ ')
    assert.equal(dec(textTail(enc('plain text'))), 'plain text')
    assert.equal(dec(textTail(Uint8Array.from([...enc('**\x18B0800000000022d\r'), 0x8a, ...enc('OO$ ')]))), '$ ')
    assert.equal(textTail(enc('abc\x00')).length, 0)
    const m = new OutputMux()
    const tails = []
    m.drain(30, 3000, (t) => tails.push(dec(t)))
    assert.equal(m.filter(enc('\x18\x00binary-zmodem-data'), { replay: false, follower: false }, []).length, 0)
    assert.equal(m.filter(enc('\r\nexit=128\r\n$ '), { replay: false, follower: false }, []).length, 0)
    await sleep(120)
    assert.deepEqual(tails, ['exit=128\r\n$ '])
    assert.equal(m.draining, false)
    m.dispose()
  })
  it('drains until the stream is quiet', () => {
    let now = 1000
    const m = new OutputMux(() => now)
    m.drain(400, 3000)
    now += 100
    assert.equal(m.filter(enc('junk'), { replay: false, follower: false }, []).length, 0)
    now += 300
    assert.equal(m.filter(enc('junk'), { replay: false, follower: false }, []).length, 0)
    now += 500
    assert.equal(dec(m.filter(enc('prompt$ '), { replay: false, follower: false }, [])), 'prompt$ ')
    assert.equal(m.draining, false)
  })
})

// ---------------------------------------------------------------------------------------------------------------------

describe('trzsz helpers', () => {
  it('holds back a magic line split across chunks', () => {
    assert.equal(trzszHoldIndex(enc('plain output\r\n')), 14)
    const partial = enc('x\x1b[s::TRZSZ:TRANSFER:S:1.2.0:90530')
    assert.equal(trzszHoldIndex(partial), 4)
    assert.equal(trzszHoldIndex(enc('abc::TRZ')), 3)
    assert.equal(trzszHoldIndex(enc('abc:')), 4, 'a lone colon is not held')
    assert.equal(trzszHoldIndex(enc('::TRZSZ:TRANSFER:R:1.2.0:123\r\n')), 30)
  })
  it('formats the summary like trzsz and detects stops', () => {
    assert.equal(formatSaved(['a'], 'Downloads'), 'Saved 1 file/directory to Downloads\r\n- a')
    assert.equal(formatSaved(['a', 'b'], ''), 'Saved 2 files/directories\r\n- a\r\n- b')
    assert.equal(isTrzszStopped(new Error('Stopped')), true)
    assert.equal(isTrzszStopped(new Error('Receive data timeout')), false)
  })
  it('orders upload readers parents first and flattens plain trz uploads', async () => {
    const b = (s) => new Blob([s])
    const items = [
      { relPath: ['proj', 'src', 'a.c'], isDir: false, blob: b('A') },
      { relPath: ['proj', 'empty'], isDir: true },
      { relPath: ['z.txt'], isDir: false, blob: b('Z') },
      { relPath: ['proj', 'b..', 'c'], isDir: false, blob: b('C') },
    ]
    const dirs = uploadReaders(items, true).map((r) => [r.getPathId(), r.getRelPath().join('/'), r.isDir()])
    assert.deepEqual(dirs, [
      [0, 'proj', true],
      [0, 'proj/b', true],
      [0, 'proj/b/c', false],
      [0, 'proj/empty', true],
      [0, 'proj/src', true],
      [0, 'proj/src/a.c', false],
      [1, 'z.txt', false],
    ])
    const flat = uploadReaders(items, false)
    assert.deepEqual(flat.map((r) => r.getRelPath()), [['a.c'], ['z.txt'], ['c']])
    const r = flat[1]
    assert.equal(r.getSize(), 1)
    assert.equal(dec(await r.readFile(new ArrayBuffer(10))), 'Z')
    assert.equal((await r.readFile(new ArrayBuffer(10))).length, 0)
  })
  it('renders output and releases held bytes after a pause', async () => {
    const late = []
    const stage = new TrzszStage({
      send: () => undefined,
      writeAsync: (b) => late.push(dec(b)),
      columns: 80,
      hooks: { claim: async () => false, chooseTarget: async () => null, chooseFiles: async () => null, start() {}, progress() {}, end() {} },
    })
    assert.equal(dec(stage.filter(enc('hello::TR'))), 'hello')
    await sleep(80)
    assert.deepEqual(late, ['::TR'])
    assert.equal(dec(stage.filter(enc('more'))), 'more')
    stage.dispose()
  })
})

// ---------------------------------------------------------------------------------------------------------------------

describe('zmodem stage', () => {
  const ZRQINIT = Uint8Array.from(Zmodem.Header.build('ZRQINIT').to_hex())
  const ZRINIT = Uint8Array.from(Zmodem.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex())

  it('hides the session header and sz’s "rz\\r" prefix', () => {
    assert.equal(dec(stripSessionHeader(concatBytes([enc('$ sz f\r\nrz\r'), ZRQINIT]))), '$ sz f\r\n')
    assert.equal(dec(stripSessionHeader(concatBytes([enc('rz waiting to receive.'), ZRINIT]))), 'rz waiting to receive.')
    assert.equal(dec(stripSessionHeader(enc('no header'))), 'no header')
  })

  function stageWith(events) {
    return new ZmodemStage({
      send: (b) => events.push(['send', b.length]),
      writeAsync: (b) => events.push(['late', dec(b)]),
      detect: (d) => events.push(['detect', d.get_session_role(), d]),
      retract: () => events.push(['retract']),
      failed: (e) => events.push(['failed', String(e)]),
    })
  }

  it('hides "rz\\r" written in a frame before the header', async () => {
    const ev = []
    const st = stageWith(ev)
    assert.equal(dec(st.filter(enc('$ sz f\r\nrz\r'))), '$ sz f\r\n')
    assert.equal(st.filter(ZRQINIT).length, 0)
    assert.equal(ev[0][0], 'detect')
    // A plain "rz\r" (no header following) is shown after a short hold.
    const ev2 = []
    const st2 = stageWith(ev2)
    assert.equal(dec(st2.filter(enc('x rz\r'))), 'x ')
    await sleep(100)
    assert.deepEqual(ev2, [['late', 'rz\r']])
    st.dispose()
    st2.dispose()
  })

  it('detects sz / rz and retracts false positives', () => {
    const ev = []
    const st = stageWith(ev)
    const out = st.filter(concatBytes([enc('rz\r'), ZRQINIT]))
    assert.equal(out.length, 0)
    assert.equal(ev[0][0], 'detect')
    assert.equal(ev[0][1], 'receive')
    // More text after the header: it was only text (e.g. `cat` of a capture).
    assert.equal(dec(st.filter(enc('just text\r\n'))), 'just text\r\n')
    assert.deepEqual(ev.slice(1).map((e) => e[0]), ['retract'])
    assert.equal(ev[0][2].is_valid(), false)

    const ev2 = []
    const st2 = stageWith(ev2)
    assert.equal(dec(st2.filter(concatBytes([enc('rz waiting to receive.'), ZRINIT]))), 'rz waiting to receive.')
    assert.equal(ev2[0][1], 'send')
    assert.equal(st2.busy, false, 'not busy until confirmed')
  })

  it('detects a header split across frames (fast path keeps feeding after a ZDLE)', () => {
    const ev = []
    const st = stageWith(ev)
    assert.equal(dec(st.filter(enc('plain output without zdle\r\n'))), 'plain output without zdle\r\n')
    const whole = concatBytes([enc('rz\r'), ZRQINIT])
    st.filter(whole.subarray(0, 10))
    st.filter(whole.subarray(10))
    assert.equal(ev.filter((e) => e[0] === 'detect').length, 1)
  })

  it('does not detect a header in the middle of text', () => {
    const ev = []
    const st = stageWith(ev)
    const text = concatBytes([enc('before '), ZRQINIT, enc(' after')])
    assert.equal(st.filter(text).length, text.length)
    assert.equal(ev.length, 0)
  })

  it('denying a detection sends the abort sequence', () => {
    const ev = []
    const st = stageWith(ev)
    st.filter(ZRQINIT)
    ev[0][2].deny()
    assert.deepEqual(ev[1], ['send', 18], 'the full 8 × CAN + 10 × BS sequence')
  })
})

describe('progress (docs/UX.md: monotonic, no reset between files, delayed + min-visible)', () => {
  it('overall fraction uses the byte total, else the file count, else is unknown', () => {
    assert.equal(overallFraction({ done: 50, total: 200 }), 0.25)
    assert.equal(overallFraction({ done: 0, total: 0 }), 1)
    // 3 files, second file half done: (1 + 0.5) / 3.
    assert.equal(overallFraction({ done: 0, total: null, fileIndex: 2, fileCount: 3, fileBytes: 5, fileSize: 10 }), 0.5)
    // Next file starting at 0 does not reset the overall bar.
    const endOf1 = overallFraction({ done: 0, total: null, fileIndex: 1, fileCount: 2, fileBytes: 10, fileSize: 10 })
    const startOf2 = overallFraction({ done: 0, total: null, fileIndex: 2, fileCount: 2, fileBytes: 0, fileSize: 10 })
    assert.equal(endOf1, 0.5)
    assert.equal(startOf2, 0.5)
    assert.equal(overallFraction({ done: 10, total: null, fileIndex: 1, fileCount: null, fileBytes: 10, fileSize: 20 }), null)
  })

  it('advance never goes back and never returns to unknown', () => {
    assert.equal(advance(null, null), null)
    assert.equal(advance(null, 0.2), 0.2)
    assert.equal(advance(0.4, 0.3), 0.4, 'ZRPOS rewind')
    assert.equal(advance(0.4, null), 0.4)
    assert.equal(advance(0.4, 0.9), 0.9)
  })
})
