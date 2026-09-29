// Review fixes: line endings after a conflict merge (adoptBreaks), whitespace-insensitive diffs (monaco/diffws.ts) and
// path-bar breadcrumbs (paths.ts).
import assert from 'node:assert/strict'
import test from 'node:test'
import { adoptBreaks, joinWithBreaks, normalizeBreaks, scanBreaks } from '../eol'
import { blockInnerChanges, stripWhitespaceLines } from '../monaco/diffws'
import { crumbs } from '../paths'

/** What the editor saves after merging `merged` with the server version `serverRaw` (mine: `mineRaw`). */
function saveMerged(mineRaw, serverRaw, merged) {
  const scan = scanBreaks(mineRaw, 'lf')
  // mine's marks, mapped to the merged text the simple way: only valid when mine and merged share their prefix lines
  const marks = scan.marks.filter((m) => m.pos < merged.length && merged[m.pos] === '\n' && normalizeBreaks(mineRaw).slice(0, m.pos) === merged.slice(0, m.pos))
  return joinWithBreaks(merged, adoptBreaks(merged, marks, serverRaw, scan.main), scan.main)
}

test('adoptBreaks: lines taken from the server keep the server line endings', () => {
  // mine: first line edited; server: appended a CRLF line. Merged = mine + the server's line.
  const out = saveMerged('a\r\nb\nc\r\n', 'a\r\nb\nc\r\nremote\r\n', 'A\nb\nc\nremote\n')
  assert.equal(out, 'A\r\nb\nc\r\nremote\r\n')
})

test('adoptBreaks: untouched lines follow the server, new lines of mine get the main line ending', () => {
  const server = 'one\r\ntwo\r\nthree\n'
  const merged = 'one\ntwo\nmine\nthree\n'
  const marks = adoptBreaks(merged, [], server, 'lf')
  assert.equal(joinWithBreaks(merged, marks, 'lf'), 'one\r\ntwo\r\nmine\nthree\n')
})

test('adoptBreaks: marks of mine survive where the server has no such line', () => {
  const merged = 'x\ny\nz'
  // mine had a CR break after "y" (pos 3); the server has only "x"
  const marks = adoptBreaks(merged, [{ pos: 3, eol: 'cr' }], 'x\r\n', 'lf')
  assert.deepEqual(marks, [
    { pos: 1, eol: 'crlf' },
    { pos: 3, eol: 'cr' },
  ])
})

test('adoptBreaks: a deleted server line takes its break along', () => {
  const merged = 'a\nc\n'
  const marks = adoptBreaks(merged, [], 'a\r\nb\r\nc\n', 'lf')
  assert.equal(joinWithBreaks(merged, marks, 'lf'), 'a\r\nc\n')
})

test('adoptBreaks: identical texts reproduce the server bytes', () => {
  for (const raw of ['a\r\nb\nc\rd', '\r\n\r\n', 'no breaks', '', 'x\n\r\ny\r']) {
    const text = normalizeBreaks(raw)
    const main = scanBreaks(raw, 'lf').main
    assert.equal(joinWithBreaks(text, adoptBreaks(text, [], raw, main), main), raw, JSON.stringify(raw))
  }
})

test('stripWhitespaceLines keeps the line structure', () => {
  assert.equal(stripWhitespaceLines(['  a b ', '\t', 'c']), 'ab\n\nc')
})

test('blockInnerChanges drops whitespace-only character changes and changes of other blocks', () => {
  const A = ['one', '  two', 'three']
  const B = ['one', 'two  ', 'three changed']
  const text = (lines) => (r) => {
    // single-line ranges only in this test
    return lines[r.startLineNumber - 1].slice(r.startColumn - 1, r.endColumn - 1)
  }
  const inner = [
    { originalRange: { startLineNumber: 2, startColumn: 1, endLineNumber: 2, endColumn: 3 }, modifiedRange: { startLineNumber: 2, startColumn: 1, endLineNumber: 2, endColumn: 1 } },
    { originalRange: { startLineNumber: 2, startColumn: 6, endLineNumber: 2, endColumn: 6 }, modifiedRange: { startLineNumber: 2, startColumn: 4, endLineNumber: 2, endColumn: 6 } },
    { originalRange: { startLineNumber: 3, startColumn: 6, endLineNumber: 3, endColumn: 6 }, modifiedRange: { startLineNumber: 3, startColumn: 6, endLineNumber: 3, endColumn: 14 } },
  ]
  const block = { original: { startLineNumber: 3, endLineNumberExclusive: 4 }, modified: { startLineNumber: 3, endLineNumberExclusive: 4 } }
  assert.deepEqual(blockInnerChanges(block, inner, text(A), text(B)), [inner[2]])
  const wsBlock = { original: { startLineNumber: 2, endLineNumberExclusive: 3 }, modified: { startLineNumber: 2, endLineNumberExclusive: 3 } }
  assert.deepEqual(blockInnerChanges(wsBlock, inner, text(A), text(B)), [])
})

test('crumbs: POSIX, relative and drive paths', () => {
  assert.deepEqual(crumbs('/config/edrev/app.js'), [
    { name: '/', path: '/' },
    { name: 'config', path: '/config' },
    { name: 'edrev', path: '/config/edrev' },
    { name: 'app.js', path: '/config/edrev/app.js' },
  ])
  assert.deepEqual(crumbs('/etc'), [
    { name: '/', path: '/' },
    { name: 'etc', path: '/etc' },
  ])
  assert.deepEqual(crumbs('C:\\Users\\me\\a.txt').map((c) => c.path), ['C:/', 'C:/Users', 'C:/Users/me', 'C:/Users/me/a.txt'])
  assert.deepEqual(crumbs('bucket/key.txt').map((c) => c.path), ['bucket', 'bucket/key.txt'])
  assert.deepEqual(crumbs(''), [])
})
