// File list rows (browser/rows.ts): sorting, filters, partial uploads hidden, ".." row, and the big-folder contract
// (filtering never re-sorts; sorting 100k entries stays fast).
import assert from 'node:assert/strict'
import test from 'node:test'
import { hiddenCounts, makeFilter, parentRow, sortEntries, visibleRows } from '../browser/rows'
import { countOf, describeCounts, isPartialUpload, PART_SUFFIX } from '../format'

function file(name, extra = {}) {
  return { name, path: `/d/${name}`, type: 'file', size: 0, mode: 0o100644, perm: '-rw-r--r--', mtime: '2026-01-01T00:00:00Z', hidden: name.startsWith('.'), ...extra }
}
function dir(name, extra = {}) {
  return file(name, { type: 'dir', mode: 0o40755, perm: 'drwxr-xr-x', ...extra })
}
const names = (list) => list.map((x) => (x.entry ? (x.parent ? '..' : x.entry.name) : x.name))

const SORT = { sortBy: 'name', sortDesc: false, foldersFirst: true }
const SHOW = { showHidden: false, showPartial: false, filter: '' }

test('partial uploads: recognised by suffix, files only', () => {
  assert.equal(PART_SUFFIX, '.astraterm-part')
  assert.equal(isPartialUpload(file('big.iso.astraterm-part')), true)
  assert.equal(isPartialUpload(file('.bashrc.astraterm-part')), true)
  assert.equal(isPartialUpload(file('.astraterm-part')), false) // no target name: a regular (hidden) file
  assert.equal(isPartialUpload(file('astraterm-part')), false)
  assert.equal(isPartialUpload(dir('odd.astraterm-part')), false)
})

test('visibleRows hides .astraterm-part files unless shown, and hidden files unless shown', () => {
  const sorted = sortEntries([file('a.txt'), file('a.txt.astraterm-part'), file('.env'), dir('sub'), file('.x.astraterm-part')], SORT)
  assert.deepEqual(names(visibleRows(sorted, '/d', SHOW)), ['..', 'sub', 'a.txt'])
  assert.deepEqual(names(visibleRows(sorted, '/d', { ...SHOW, showPartial: true })), ['..', 'sub', 'a.txt', 'a.txt.astraterm-part'])
  assert.deepEqual(names(visibleRows(sorted, '/d', { ...SHOW, showHidden: true })), ['..', 'sub', '.env', 'a.txt'])
  assert.deepEqual(names(visibleRows(sorted, '/d', { ...SHOW, showHidden: true, showPartial: true })), ['..', 'sub', '.env', '.x.astraterm-part', 'a.txt', 'a.txt.astraterm-part'])
  // What an "empty" view explains: partial uploads are counted apart from dot files.
  assert.deepEqual(hiddenCounts(sorted, SHOW), { hidden: 1, partial: 2 })
  assert.deepEqual(hiddenCounts(sorted, { showHidden: true, showPartial: false }), { hidden: 0, partial: 2 })
})

test('the ".." row: not at the root, not while filtering', () => {
  const sorted = sortEntries([file('x')], SORT)
  assert.deepEqual(names(visibleRows(sorted, '/', SHOW)), ['x'])
  assert.deepEqual(names(visibleRows(sorted, 'C:/', SHOW)), ['x'])
  assert.deepEqual(names(visibleRows(sorted, '/a/b', SHOW)), ['..', 'x'])
  assert.equal(visibleRows(sorted, '/a/b', SHOW)[0].entry.path, '/a')
  assert.deepEqual(names(visibleRows(sorted, '/a/b', { ...SHOW, filter: 'x' })), ['x'])
  assert.equal(parentRow(null), null)
})

test('filter: substring (case-insensitive) or globs separated by spaces / commas', () => {
  const f = makeFilter(' LOG ')
  assert.equal(f({ name: 'syslog.1' }), true)
  assert.equal(f({ name: 'readme' }), false)
  const g = makeFilter('*.log, a?.txt')
  assert.equal(g({ name: 'x.LOG' }), true)
  assert.equal(g({ name: 'ab.txt' }), true)
  assert.equal(g({ name: 'abc.txt' }), false)
  assert.equal(g({ name: 'x.log.gz' }), false)
  // Regex metacharacters in globs are literal.
  assert.equal(makeFilter('a+b(1)*')({ name: 'a+b(1).txt' }), true)
  assert.equal(makeFilter('   '), null)
})

test('sort: natural name order, folders first, stable secondary name order', () => {
  const list = [file('file10'), file('file2'), dir('zdir'), file('File1'), dir('adir'), file('b', { size: 5 }), file('a', { size: 5 })]
  assert.deepEqual(names(sortEntries(list, SORT)), ['adir', 'zdir', 'a', 'b', 'File1', 'file2', 'file10'])
  assert.deepEqual(names(sortEntries(list, { ...SORT, sortDesc: true })), ['zdir', 'adir', 'file10', 'file2', 'File1', 'b', 'a'])
  assert.deepEqual(names(sortEntries(list, { ...SORT, foldersFirst: false })), ['a', 'adir', 'b', 'File1', 'file2', 'file10', 'zdir'])
  // By size, descending: equal sizes fall back to the name (ascending).
  const bySize = sortEntries(list, { sortBy: 'size', sortDesc: true, foldersFirst: true })
  assert.deepEqual(names(bySize).slice(0, 4), ['adir', 'zdir', 'a', 'b'])
  // Symlinks to folders sort with the folders.
  const withLink = sortEntries([file('b'), file('alink', { type: 'symlink', linkType: 'dir' }), dir('c')], SORT)
  assert.deepEqual(names(withLink), ['alink', 'c', 'b'])
})

test('sort by modification time and by permissions', () => {
  const a = file('a', { mtime: '2026-03-01T00:00:00Z' })
  const b = file('b', { mtime: '2025-01-01T00:00:00Z' })
  const c = file('c', { mtime: 'not a date' })
  assert.deepEqual(names(sortEntries([a, b, c], { sortBy: 'mtime', sortDesc: false, foldersFirst: true })), ['c', 'b', 'a'])
  assert.deepEqual(names(sortEntries([a, b, c], { sortBy: 'mtime', sortDesc: true, foldersFirst: true })), ['a', 'b', 'c'])
  const x = file('x', { perm: '-rwxr-xr-x' })
  const y = file('y', { perm: '-rw-------' })
  assert.deepEqual(names(sortEntries([x, y], { sortBy: 'perm', sortDesc: false, foldersFirst: true })), ['y', 'x'])
})

test('big folders: sorting 30k entries once is fast, filtering never re-sorts', () => {
  const N = 30_000
  const list = []
  for (let i = 0; i < N; i++) list.push(file(`file-${(i * 7919) % N}.log`, { size: i, mtime: new Date(1_700_000_000_000 + i * 1000).toISOString() }))
  let t0 = performance.now()
  const sorted = sortEntries(list, SORT)
  const sortMs = performance.now() - t0
  assert.equal(sorted[0].name, 'file-0.log')
  assert.equal(sorted[N - 1].name, `file-${N - 1}.log`)
  t0 = performance.now()
  const rows = visibleRows(sorted, '/d', { ...SHOW, filter: 'file-2999' })
  const filterMs = performance.now() - t0
  assert.deepEqual(names(rows), ['file-2999.log', 'file-29990.log', 'file-29991.log', 'file-29992.log', 'file-29993.log', 'file-29994.log', 'file-29995.log', 'file-29996.log', 'file-29997.log', 'file-29998.log', 'file-29999.log'])
  // Generous bounds (loaded CI machines): the point is O(n log n) once per listing and O(n) per keystroke.
  assert.ok(sortMs < 10_000, `sort took ${sortMs} ms`)
  assert.ok(filterMs < 1_000, `filter took ${filterMs} ms`)
  // The listing itself is not modified (react-query keeps it).
  assert.equal(list[1].name, 'file-7919.log')
})

test('counts use thousands separators', () => {
  assert.equal(countOf(1, 'file'), '1 file')
  assert.equal(countOf(30000, 'file'), `${(30000).toLocaleString()} files`)
  assert.equal(describeCounts([dir('a'), file('b'), file('c')]), '1 folder, 2 files')
  assert.equal(describeCounts([]), 'empty')
})
