// Mixed line endings are preserved exactly (eol.ts): scanning, edits, undo / redo, resets, and randomized model tests —
// against a model that reports changes and version ids the way Monaco's TextModel does (fakemodel.mjs).
import assert from 'node:assert/strict'
import test from 'node:test'
import { BreakTracker, joinWithBreaks, mapBreaks, normalizeBreaks, scanBreaks, validBreaks } from '../eol'
import { FakeModel } from './fakemodel.mjs'

const SEQ = { lf: '\n', crlf: '\r\n', cr: '\r' }

/** A model + tracker as the editor session creates them: raw text normalised, marks relative to the main EOL. */
function open(raw, main) {
  const scan = scanBreaks(raw, 'lf', main)
  const model = new FakeModel(normalizeBreaks(raw))
  const tracker = new BreakTracker(scan.marks, model.getAlternativeVersionId())
  model.onDidChangeContent((e) => tracker.update(e.changes, model.getAlternativeVersionId(), e.isUndoing || e.isRedoing))
  const save = () => joinWithBreaks(model.getValue(), tracker.get(), scan.main)
  return { model, tracker, main: scan.main, save }
}

/** Offset of the end of 1-based line n in the model text. */
function lineEnd(text, n) {
  let pos = -1
  for (let i = 0; i < n; i++) {
    pos = text.indexOf('\n', pos + 1)
    if (pos < 0) return text.length
  }
  return pos
}

test('scanBreaks: positions in the joined text, main line ending, marks', () => {
  const s = scanBreaks('a\r\nb\nc\rd\n', 'lf')
  assert.equal(s.main, 'lf')
  assert.equal(s.mixed, true)
  assert.deepEqual(s.marks, [
    { pos: 1, eol: 'crlf' },
    { pos: 5, eol: 'cr' },
  ])
  assert.deepEqual(scanBreaks('x\r\ny\r\nz\n', 'lf'), { main: 'crlf', mixed: true, marks: [{ pos: 5, eol: 'lf' }] })
  assert.deepEqual(scanBreaks('one\ntwo\n', 'crlf'), { main: 'lf', mixed: false, marks: [] })
  assert.deepEqual(scanBreaks('no breaks', 'crlf'), { main: 'crlf', mixed: false, marks: [] })
  // tie → fallback
  assert.equal(scanBreaks('a\r\nb\n', 'crlf').main, 'crlf')
  // forced main line ending: every other break is a mark
  assert.deepEqual(scanBreaks('a\nb\n', 'lf', 'crlf').marks, [
    { pos: 1, eol: 'lf' },
    { pos: 3, eol: 'lf' },
  ])
  // "\r" at the very end, "\r\r\n"
  assert.deepEqual(scanBreaks('a\r', 'lf').main, 'cr')
  const w = scanBreaks('a\r\r\nb\n', 'lf')
  assert.deepEqual(w.marks.map((m) => m.eol).sort(), ['cr', 'crlf'])
})

test('round trip: unchanged documents are written back byte for byte', () => {
  for (const raw of ['line one\r\nline two\nline three\r\nline four\nline five\n', 'a\rb\r\nc\nd', '\r\n\n\r\r\n', 'x', '', 'crlf only\r\nyes\r\n']) {
    const { save } = open(raw)
    assert.equal(save(), raw, JSON.stringify(raw))
  }
})

test('typing on a line keeps its line ending; Enter uses the main one', () => {
  const raw = 'line one\r\nline two\nline three\r\nline four\nline five\n'
  const { model, tracker, main, save } = open(raw)
  assert.equal(main, 'lf')
  // append to "line one" (right before its CRLF)
  model.edit([{ offset: lineEnd(model.getValue(), 1), length: 0, text: ' edited' }])
  assert.equal(save(), 'line one edited\r\nline two\nline three\r\nline four\nline five\n')
  // split "line three" → the new break is LF, "three" keeps its CRLF
  const l3 = lineEnd(model.getValue(), 2) + 1
  model.edit([{ offset: l3 + 5, length: 0, text: '\n' }])
  assert.equal(save(), 'line one edited\r\nline two\nline \nthree\r\nline four\nline five\n')
  // Enter at the end of "line one edited": new empty line gets LF, the CRLF moves with the original break
  model.edit([{ offset: lineEnd(model.getValue(), 1), length: 0, text: '\nX' }])
  assert.equal(save(), 'line one edited\nX\r\nline two\nline \nthree\r\nline four\nline five\n')
  assert.equal(tracker.count(), 2)
})

test('deleting a break drops its mark; undo brings it back, redo removes it again', () => {
  const raw = 'a\r\nb\nc\r\nd\n'
  const { model, save } = open(raw)
  // join "a" and "b" (delete the CRLF break)
  model.edit([{ offset: 1, length: 1, text: '' }])
  assert.equal(save(), 'ab\nc\r\nd\n')
  model.undo()
  assert.equal(save(), raw)
  model.redo()
  assert.equal(save(), 'ab\nc\r\nd\n')
  model.undo()
  assert.equal(save(), raw)
})

test('replacing a range with breaks: removed marks come back on undo', () => {
  const raw = 'one\r\ntwo\r\nthree\nfour\r\n'
  const { model, main, save } = open(raw)
  assert.equal(main, 'crlf')
  const from = 1
  const to = lineEnd(model.getValue(), 2) + 1 + 2
  // "ne⏎two⏎th" → "X⏎Y": the new break gets CRLF, "three"'s LF survives
  model.edit([{ offset: from, length: to - from, text: 'X\nY' }])
  assert.equal(save(), 'oX\r\nYree\nfour\r\n')
  model.undo()
  assert.equal(save(), raw)
})

test('multi-cursor edits (several changes in one event, descending offsets) map every mark', () => {
  const raw = 'a1\r\nb2\nc3\r\nd4\ne5\r\n'
  const { model, save } = open(raw)
  const t = model.getValue()
  // type "!" at the end of every line at once, and delete the LF break after "b2" in the same operation
  const ends = [1, 2, 3, 4, 5].map((n) => lineEnd(t, n))
  model.edit([
    { offset: ends[0], length: 0, text: '!' },
    { offset: ends[1], length: 1, text: '!' },
    { offset: ends[2], length: 0, text: '!' },
    { offset: ends[4], length: 0, text: '!' },
  ])
  assert.equal(save(), 'a1!\r\nb2!c3!\r\nd4\ne5!\r\n')
  model.undo()
  assert.equal(save(), raw)
  // mapBreaks directly: descending changes with offsets in the original text
  assert.deepEqual(
    mapBreaks([{ pos: 2, eol: 'crlf' }, { pos: 8, eol: 'crlf' }], [
      { rangeOffset: 8, rangeLength: 0, text: 'xx' },
      { rangeOffset: 5, rangeLength: 1, text: '' },
      { rangeOffset: 0, rangeLength: 0, text: 'y' },
    ]),
    [
      { pos: 3, eol: 'crlf' },
      { pos: 10, eol: 'crlf' },
    ],
  )
})

test('reset marks (backup restore) is undoable; invalid positions are dropped', () => {
  const raw = 'a\r\nb\nc\n'
  const { model, tracker, main, save } = open(raw)
  // a content edit, then the marks of another text taken over for the new version
  model.edit([{ offset: 2, length: 1, text: 'B' }])
  tracker.reset([], model.getAlternativeVersionId())
  assert.equal(save(), 'a\nB\nc\n')
  model.undo()
  assert.equal(save(), raw)
  model.redo()
  assert.equal(save(), 'a\nB\nc\n')
  assert.equal(main, 'lf')
  assert.deepEqual(validBreaks([{ pos: 0, eol: 'cr' }, { pos: 3, eol: 'cr' }, { pos: 3, eol: 'crlf' }, { pos: 999, eol: 'cr' }], 'a\nb\nc'), [{ pos: 3, eol: 'cr' }])
})

test('joinWithBreaks: explicit marks over a text', () => {
  assert.equal(joinWithBreaks('a\nb\nc', [{ pos: 3, eol: 'crlf' }], 'cr'), 'a\rb\r\nc')
  assert.equal(joinWithBreaks('a\nb\nc', [], 'crlf'), 'a\r\nb\r\nc')
  assert.equal(joinWithBreaks('a\nb\n', [{ pos: 1, eol: 'cr' }], 'lf'), 'a\rb\n')
  assert.equal(normalizeBreaks('a\r\nb\rc\n'), 'a\nb\nc\n')
})

// ---------------------------------------------------------------------------------------------------------------------
// randomized: edits against a model that stores every line with its own line ending
// ---------------------------------------------------------------------------------------------------------------------

function rng(seed) {
  let x = seed >>> 0 || 1
  return () => {
    x ^= x << 13
    x ^= x >>> 17
    x ^= x << 5
    return (x >>> 0) / 4294967296
  }
}

/** Reference: [{text, eol}] (last line: eol null). Positions are in the "\n"-joined document. */
function refOf(raw) {
  const lines = []
  let start = 0
  for (let i = 0; i < raw.length; i++) {
    const c = raw[i]
    if (c === '\n' || c === '\r') {
      const eol = c === '\r' && raw[i + 1] === '\n' ? 'crlf' : c === '\r' ? 'cr' : 'lf'
      lines.push({ text: raw.slice(start, i), eol })
      if (eol === 'crlf') i++
      start = i + 1
    }
  }
  lines.push({ text: raw.slice(start), eol: null })
  return lines
}

function refText(lines) {
  return lines.map((l) => l.text + (l.eol ? SEQ[l.eol] : '')).join('')
}

function locate(lines, pos) {
  let p = pos
  for (let i = 0; i < lines.length; i++) {
    if (p <= lines[i].text.length) return [i, p]
    p -= lines[i].text.length + 1
  }
  throw new Error('bad pos ' + pos)
}

/** Replace [from, to) with `insert` ("\n" = new break with the main line ending). */
function refReplace(lines, from, to, insert, main) {
  const [li, ci] = locate(lines, from)
  const [lj, cj] = locate(lines, to)
  const head = lines[li].text.slice(0, ci)
  const tail = lines[lj].text.slice(cj)
  const tailEol = lines[lj].eol
  const parts = insert.split('\n')
  const repl = parts.map((t) => ({ text: t, eol: main }))
  repl[0].text = head + repl[0].text
  repl[repl.length - 1].text += tail
  repl[repl.length - 1].eol = tailEol
  lines.splice(li, lj - li + 1, ...repl)
}

/** Random non-overlapping edits (1-3 ranges), applied to the reference back to front (like Monaco). */
function randomEdits(r, len) {
  const n = r() < 0.7 ? 1 : 1 + Math.floor(r() * 3)
  const edits = []
  let floor = 0
  const points = Array.from({ length: n }, () => Math.floor(r() * (len + 1))).sort((a, b) => a - b)
  for (const p of points) {
    const from = Math.max(p, floor)
    if (from > len) break
    const to = Math.min(len, from + (r() < 0.5 ? 0 : Math.floor(r() * 6)))
    const insert = r() < 0.3 ? '' : r() < 0.4 ? '\n' : r() < 0.5 ? 'ab\ncd' : 'z'
    if (from === to && !insert) continue
    edits.push({ offset: from, length: to - from, text: insert })
    floor = to + 1
  }
  return edits
}

test('randomized edits (single and multi-range, grouped undo elements), then undo everything and redo everything', () => {
  const kinds = ['lf', 'crlf', 'cr']
  for (let seed = 1; seed <= 80; seed++) {
    const r = rng(seed * 7919)
    let raw = ''
    const n = 3 + Math.floor(r() * 12)
    for (let i = 0; i < n; i++) raw += 'w' + i + 'x'.repeat(Math.floor(r() * 4)) + (i < n - 1 || r() < 0.5 ? SEQ[kinds[Math.floor(r() * 3)]] : '')
    const { model, main, save } = open(raw)
    const ref = refOf(raw)
    const steps = 1 + Math.floor(r() * 25)
    for (let s = 0; s < steps; s++) {
      const edits = randomEdits(r, model.getValue().length)
      if (!edits.length) continue
      model.edit(edits, { group: r() < 0.3 })
      for (const e of [...edits].sort((a, b) => b.offset - a.offset)) refReplace(ref, e.offset, e.offset + e.length, e.text, main)
      assert.equal(save(), refText(ref), `seed ${seed} step ${s}`)
    }
    const final = save()
    while (model.undo()) {
      /* undo everything */
    }
    assert.equal(save(), raw, `seed ${seed}: undo all`)
    while (model.redo()) {
      /* redo everything */
    }
    assert.equal(save(), final, `seed ${seed}: redo all`)
  }
})

test('randomized edits: every single undo step restores the exact bytes of that point', () => {
  const kinds = ['lf', 'crlf', 'cr']
  for (let seed = 1; seed <= 50; seed++) {
    const r = rng(seed * 104729)
    let raw = ''
    const n = 2 + Math.floor(r() * 10)
    for (let i = 0; i < n; i++) raw += 'L' + i + (i < n - 1 ? SEQ[kinds[Math.floor(r() * 3)]] : '')
    const { model, main, save } = open(raw)
    const ref = refOf(raw)
    const snapshots = [raw]
    for (let s = 0; s < 15; s++) {
      const len = model.getValue().length
      const from = Math.floor(r() * (len + 1))
      const to = Math.min(len, from + Math.floor(r() * 5))
      const insert = r() < 0.35 ? '' : r() < 0.5 ? '\n' : 'q\nr'
      if (from === to && !insert) continue
      model.edit([{ offset: from, length: to - from, text: insert }])
      refReplace(ref, from, to, insert, main)
      snapshots.push(refText(ref))
      assert.equal(save(), snapshots[snapshots.length - 1])
    }
    for (let k = snapshots.length - 2; k >= 0; k--) {
      model.undo()
      assert.equal(save(), snapshots[k], `seed ${seed}: undo to step ${k}`)
    }
    for (let k = 1; k < snapshots.length; k++) {
      model.redo()
      assert.equal(save(), snapshots[k], `seed ${seed}: redo to step ${k}`)
    }
  }
})

test('documents with one kind of line ending track nothing and save converted or unchanged', () => {
  const { model, tracker, save } = open('a\r\nb\r\nc')
  model.edit([{ offset: 1, length: 0, text: '\nnew' }])
  assert.equal(tracker.count(), 0)
  assert.equal(save(), 'a\r\nnew\r\nb\r\nc')
  assert.equal(joinWithBreaks(model.getValue(), tracker.get(), 'lf'), 'a\nnew\nb\nc')
})
