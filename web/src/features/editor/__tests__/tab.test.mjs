// The "editor" tab's non-React logic: reading a remote file (fileread.ts), change-detection banners and crash-backup
// records (tabstate.ts).
import assert from 'node:assert/strict'
import test from 'node:test'
import { ApiError } from '../../../api/client'
import { bytesToBase64 } from '../codec'
import { decodeServerVersion, readDocument } from '../fileread'
import { backupFromDoc, backupKeys, TEXT_MAX, watchBanner } from '../tabstate'

const MTIME = '2026-09-28T10:00:00Z'
const OPTS = { mode: 'text', largeBytes: 4 * 1024 * 1024, defaultEol: 'lf' }

/** A fake file system: `files` maps paths to {entry, read} (or an Error to throw). */
function fakeFs(files, links = {}) {
  const calls = []
  const get = (kind, path) => {
    calls.push(`${kind} ${path}`)
    const f = files[path]
    if (!f) throw new ApiError(404, 'not_found', `${path}: no such file`)
    if (f instanceof Error) throw f
    return f
  }
  return {
    calls,
    stat: async (path) => {
      const f = get('stat', path)
      if (f.statError) throw f.statError
      return { name: path.split('/').pop(), path, type: 'file', size: f.size ?? f.content?.length ?? 0, mtime: MTIME, ...f.entry }
    },
    realpath: async (path) => ({ path: links[path] ?? path }),
    read: async (path, maxBytes) => {
      const f = get('read', path)
      if (f.readError) throw f.readError
      return { content: f.content, encoding: f.encoding ?? 'utf-8', size: f.size ?? f.content.length, mtime: MTIME, mode: 0o644, maxBytes }
    },
  }
}

test('readDocument: a UTF-8 text file becomes a text document with detected line endings and the read stat', async () => {
  const fs = fakeFs({ '/etc/motd': { content: 'hello\r\nworld\r\n' } })
  const { outcome, statPath } = await readDocument(fs, '/etc/motd', OPTS)
  assert.equal(statPath, '/etc/motd')
  assert.equal(outcome.kind, 'ready')
  assert.equal(outcome.doc.kind, 'text')
  assert.equal(outcome.doc.text, 'hello\r\nworld\r\n')
  assert.deepEqual(outcome.doc.meta, { encoding: 'utf-8', bom: false, eol: 'crlf', mixedEol: false })
  assert.equal(outcome.doc.truncated, false)
  assert.equal(outcome.stat.mtime, MTIME)
  assert.equal(outcome.stat.size, 14)
})

test('readDocument: a BOM is taken off the text and remembered', async () => {
  const fs = fakeFs({ '/a.txt': { content: '﻿hi\n' } })
  const { outcome } = await readDocument(fs, '/a.txt', OPTS)
  assert.equal(outcome.doc.text, 'hi\n')
  assert.equal(outcome.doc.meta.bom, true)
})

test('readDocument: symlinks are followed for change detection, the link path stays the document path', async () => {
  const fs = fakeFs({ '/link': { entry: { type: 'symlink' }, content: 'x' }, '/real/target': { content: 'x' } }, { '/link': '/real/target' })
  const { outcome, statPath } = await readDocument(fs, '/link', OPTS)
  assert.equal(statPath, '/real/target')
  assert.equal(outcome.kind, 'ready')
  assert.deepEqual(fs.calls, ['stat /link', 'stat /real/target', 'read /link'])
})

test('readDocument: directories, missing files, lost handles and hard stat failures are errors', async () => {
  const dir = await readDocument(fakeFs({ '/d': { entry: { type: 'dir' } } }), '/d', OPTS)
  assert.equal(dir.outcome.kind, 'error')
  assert.equal(dir.outcome.isDir, true)

  const missing = await readDocument(fakeFs({}), '/nope', OPTS)
  assert.equal(missing.outcome.kind, 'error')
  assert.equal(missing.outcome.notFound, true)
  assert.equal(missing.outcome.handleGone, false)

  const gone = await readDocument(fakeFs({ '/x': new ApiError(404, 'fs_not_found', 'file system handle not found') }), '/x', OPTS)
  assert.equal(gone.outcome.handleGone, true)
  assert.equal(gone.outcome.notFound, false)

  const denied = await readDocument(fakeFs({ '/x': { statError: new ApiError(403, 'forbidden', 'nope'), content: '' } }), '/x', OPTS)
  assert.equal(denied.outcome.kind, 'error')
  assert.equal(denied.outcome.message, 'nope')
})

test('readDocument: a file system without stat is read anyway', async () => {
  const fs = fakeFs({ '/x': { statError: new ApiError(400, 'not_supported', 'stat is not supported'), content: 'ok' } })
  const { outcome } = await readDocument(fs, '/x', OPTS)
  assert.equal(outcome.kind, 'ready')
  assert.equal(outcome.doc.text, 'ok')
})

test('readDocument: large files ask first (unless forced); files over the editor limit are refused without reading', async () => {
  const big = { '/big': { size: 5 * 1024 * 1024, content: 'a' } }
  assert.deepEqual((await readDocument(fakeFs(big), '/big', OPTS)).outcome, { kind: 'large', size: 5 * 1024 * 1024 })
  assert.equal((await readDocument(fakeFs(big), '/big', { ...OPTS, force: true })).outcome.kind, 'ready')

  const huge = fakeFs({ '/huge': { size: TEXT_MAX + 1, content: '' } })
  const r = await readDocument(huge, '/huge', { ...OPTS, force: true })
  assert.equal(r.outcome.kind, 'error')
  assert.equal(r.outcome.tooLarge, true)
  assert.deepEqual(huge.calls, ['stat /huge'])

  const refused = await readDocument(fakeFs({ '/y': { readError: new ApiError(413, 'too_large', 'too large', { size: 123 }), content: '' } }), '/y', OPTS)
  assert.equal(refused.outcome.tooLarge, true)
})

test('readDocument: bytes are sniffed — binary files stop, legacy encodings decode with a note', async () => {
  const bin = await readDocument(fakeFs({ '/bin': { content: bytesToBase64(Uint8Array.of(0x7f, 0x45, 0x4c, 0x46, 0, 1, 2)), encoding: 'base64', size: 7 } }), '/bin', OPTS)
  assert.equal(bin.outcome.kind, 'binary')
  assert.equal(bin.outcome.size, 7)
  assert.match(bin.outcome.reason, /NUL/)

  const latin = await readDocument(fakeFs({ '/l1': { content: bytesToBase64(Uint8Array.of(0x63, 0x61, 0x66, 0xe9, 0x0a)), encoding: 'base64', size: 5 } }), '/l1', OPTS)
  assert.equal(latin.outcome.kind, 'ready')
  assert.equal(latin.outcome.doc.text, 'café\n')
  assert.equal(latin.outcome.doc.meta.encoding, 'windows-1252')
  assert.match(latin.outcome.doc.note, /not valid UTF-8/)
  assert.equal(latin.outcome.doc.lossy, false)
})

test('readDocument: the hex view gets the raw bytes; a short read is marked truncated', async () => {
  const hex = await readDocument(fakeFs({ '/h': { content: 'AB' } }), '/h', { ...OPTS, mode: 'hex' })
  assert.equal(hex.outcome.doc.kind, 'hex')
  assert.deepEqual(Array.from(hex.outcome.doc.bytes), [0x41, 0x42])

  const cut = await readDocument(fakeFs({ '/t': { content: 'abc', size: 10 } }), '/t', OPTS)
  assert.equal(cut.outcome.doc.truncated, true)
})

test('decodeServerVersion: BOM dropped, line endings kept in raw and normalised in text', () => {
  assert.deepEqual(decodeServerVersion({ content: '﻿a\r\nb\rc\n', encoding: 'utf-8' }, 'utf-8'), { raw: 'a\r\nb\rc\n', text: 'a\nb\nc\n' })
  const b64 = bytesToBase64(Uint8Array.of(0x63, 0x61, 0x66, 0xe9, 0x0d, 0x0a))
  assert.deepEqual(decodeServerVersion({ content: b64, encoding: 'base64' }, 'windows-1252'), { raw: 'café\r\n', text: 'café\n' })
})

test('watchBanner: server changes, ignored versions, deletions, and banners the user still has to act on', () => {
  const base = { mtime: MTIME, size: 10 }
  const later = { mtime: '2026-09-28T10:05:00Z', size: 12 }
  const stat = (s, ignoredMtime = null) => ({ kind: 'stat', stat: s, base, ignoredMtime })

  assert.deepEqual(watchBanner(null, stat(later)), { kind: 'changed', mtime: later.mtime, size: 12 })
  assert.equal(watchBanner(null, stat(later, later.mtime)), null, 'ignored version')
  assert.deepEqual(watchBanner({ kind: 'conflict' }, stat(later)), { kind: 'conflict' })
  assert.deepEqual(watchBanner({ kind: 'denied', message: 'x' }, stat(later)), { kind: 'denied', message: 'x' })
  // Same instant written differently, same size: not a change.
  assert.equal(watchBanner({ kind: 'changed' }, stat({ mtime: '2026-09-28T12:00:00+02:00', size: 10 })), null)
  assert.equal(watchBanner({ kind: 'deleted' }, stat(base)), null)
  assert.equal(watchBanner({ kind: 'gone' }, stat(base)), null)
  assert.deepEqual(watchBanner({ kind: 'error', message: 'e' }, stat(base)), { kind: 'error', message: 'e' })

  assert.deepEqual(watchBanner(null, { kind: 'missing', handleGone: false }), { kind: 'deleted' })
  assert.deepEqual(watchBanner({ kind: 'changed' }, { kind: 'missing', handleGone: true }), { kind: 'gone' })
  assert.deepEqual(watchBanner({ kind: 'conflict' }, { kind: 'missing', handleGone: false }), { kind: 'conflict' })
})

test('backupKeys / backupFromDoc: stable key first, defaults for old records', () => {
  assert.deepEqual(backupKeys('/a', 'own', { connectionId: 'c1', label: 'web' }), ['backup:conn:c1:/a', 'own'])
  assert.deepEqual(backupKeys('/a', 'own', { label: 'root@web' }), ['backup:label:root@web:/a', 'own'])
  assert.deepEqual(backupKeys('/a', 'own', {}), ['own'])

  assert.equal(backupFromDoc(undefined), null)
  assert.equal(backupFromDoc({ updatedAt: 1 }), null)
  assert.deepEqual(backupFromDoc({ content: 'x', updatedAt: 5 }), {
    content: 'x',
    meta: { encoding: 'utf-8', bom: false, eol: 'lf', mixedEol: false },
    baseMtime: undefined,
    savedAt: 5,
  })
  assert.deepEqual(backupFromDoc({ content: 'y', updatedAt: 5, meta: { encoding: 'utf-16le', bom: true, eol: 'crlf', baseMtime: MTIME, savedAt: 9 } }), {
    content: 'y',
    meta: { encoding: 'utf-16le', bom: true, eol: 'crlf', mixedEol: false },
    baseMtime: MTIME,
    savedAt: 9,
  })
})
