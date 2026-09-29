/*
 * Minimal edits that turn one "\n"-joined text into another, used to replace a document's content as one undoable
 * change that touches only what differs (applying a merge, restoring a backup): cursors, folds and the line endings of
 * untouched lines survive. Line-level Myers diff; changed blocks of the same size are edited line by line, others as
 * one block; every edit is narrowed to the characters that really differ. Pure.
 */

export interface TextEdit {
  /** Offset in the old text. */
  offset: number
  /** Characters replaced. */
  length: number
  text: string
}

/** Differences scanned before giving up and replacing the changed middle as one block. */
const MAX_D = 1200

interface Hunk {
  a1: number
  a2: number
  b1: number
  b2: number
}

/** Myers O((N+M)·D) diff of two integer sequences; null when more than `maxD` edits are needed. */
function myers(a: Int32Array, b: Int32Array, maxD: number): Hunk[] | null {
  const n = a.length
  const m = b.length
  const max = n + m
  const off = max + 1
  const v = new Int32Array(2 * max + 3)
  const trace: Int32Array[] = []
  let found = -1
  outer: for (let d = 0; d <= Math.min(max, maxD); d++) {
    trace.push(v.slice(off - d - 1, off + d + 2))
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[off + k - 1] < v[off + k + 1]) ? v[off + k + 1] : v[off + k - 1] + 1
      let y = x - k
      while (x < n && y < m && a[x] === b[y]) {
        x++
        y++
      }
      v[off + k] = x
      if (x >= n && y >= m) {
        found = d
        break outer
      }
    }
  }
  if (found < 0) return null
  // backtrack: collect the (x, y) path, then group non-diagonal steps into hunks
  const hunks: Hunk[] = []
  let x = n
  let y = m
  for (let d = found; d > 0; d--) {
    const vv = trace[d] // v before step d, window [-(d)-1 .. d+1] → index k + d + 1
    const at = (k: number) => vv[k + d + 1]
    const k = x - y
    const down = k === -d || (k !== d && at(k - 1) < at(k + 1))
    const prevK = down ? k + 1 : k - 1
    const prevX = at(prevK)
    const prevY = prevX - prevK
    // diagonal (equal) run from the step's end back to (xStart, yStart)
    const xStart = down ? prevX : prevX + 1
    const yStart = down ? prevY + 1 : prevY
    while (x > xStart && y > yStart) {
      x--
      y--
    }
    // the step itself: one deletion (right) or insertion (down)
    const h: Hunk = down ? { a1: prevX, a2: prevX, b1: prevY, b2: prevY + 1 } : { a1: prevX, a2: prevX + 1, b1: prevY, b2: prevY }
    const last = hunks[hunks.length - 1]
    if (last && last.a1 === h.a2 && last.b1 === h.b2) {
      last.a1 = h.a1
      last.b1 = h.b1
    } else hunks.push(h)
    x = prevX
    y = prevY
  }
  return hunks.reverse()
}

function lineStarts(s: string): number[] {
  const out = [0]
  for (let i = s.indexOf('\n'); i >= 0; i = s.indexOf('\n', i + 1)) out.push(i + 1)
  return out
}

/** Narrow a replacement of a[aFrom, aTo) by b[bFrom, bTo) to the part that differs (suffix first, then prefix). */
function narrow(a: string, aFrom: number, aTo: number, b: string, bFrom: number, bTo: number, out: TextEdit[]): void {
  while (aTo > aFrom && bTo > bFrom && a.charCodeAt(aTo - 1) === b.charCodeAt(bTo - 1)) {
    aTo--
    bTo--
  }
  while (aFrom < aTo && bFrom < bTo && a.charCodeAt(aFrom) === b.charCodeAt(bFrom)) {
    aFrom++
    bFrom++
  }
  if (aFrom === aTo && bFrom === bTo) return
  out.push({ offset: aFrom, length: aTo - aFrom, text: b.slice(bFrom, bTo) })
}

/** Edits (ascending, non-overlapping, offsets in `oldText`) that turn `oldText` into `newText`. */
export function textEdits(oldText: string, newText: string): TextEdit[] {
  if (oldText === newText) return []
  // A terminating sentinel gives every line a "\n": block replacements then never need a special last line, and the
  // shared sentinel is always trimmed off again by `narrow`.
  const a = oldText + '\n'
  const b = newText + '\n'
  const sa = lineStarts(a)
  const sb = lineStarts(b)
  const na = sa.length - 1
  const nb = sb.length - 1
  const lineA = (i: number) => a.slice(sa[i], sa[i + 1])
  const lineB = (i: number) => b.slice(sb[i], sb[i + 1])
  // common prefix / suffix lines
  let p = 0
  while (p < na && p < nb && lineA(p) === lineB(p)) p++
  let q = 0
  while (q < na - p && q < nb - p && lineA(na - 1 - q) === lineB(nb - 1 - q)) q++
  const ids = new Map<string, number>()
  const id = (s: string) => {
    let v = ids.get(s)
    if (v === undefined) {
      v = ids.size
      ids.set(s, v)
    }
    return v
  }
  const ma = new Int32Array(na - p - q)
  const mb = new Int32Array(nb - p - q)
  for (let i = 0; i < ma.length; i++) ma[i] = id(lineA(p + i))
  for (let i = 0; i < mb.length; i++) mb[i] = id(lineB(p + i))
  const hunks = myers(ma, mb, MAX_D) ?? [{ a1: 0, a2: ma.length, b1: 0, b2: mb.length }]
  const out: TextEdit[] = []
  for (const h of hunks) {
    const a1 = h.a1 + p
    const a2 = h.a2 + p
    const b1 = h.b1 + p
    const b2 = h.b2 + p
    if (a2 - a1 === b2 - b1) {
      // same number of lines: edit each line's content, keeping every line break (and its original sequence)
      for (let i = 0; i < a2 - a1; i++) narrow(a, sa[a1 + i], sa[a1 + i + 1] - 1, b, sb[b1 + i], sb[b1 + i + 1] - 1, out)
    } else if (a2 === na && b2 === nb) {
      // Block at the end: it must not include the sentinel. Take the line break before it instead (a block at the
      // start of both texts has none: then it is simply everything up to the sentinel).
      const lead = a1 > 0 && b1 > 0 ? 1 : 0
      narrow(a, sa[a1] - lead, a.length - 1, b, sb[b1] - lead, b.length - 1, out)
    } else narrow(a, sa[a1], sa[a2], b, sb[b1], sb[b2], out)
  }
  return out
}

/** Apply edits (ascending, from `textEdits`) to a text — the reference for tests. */
export function applyEdits(text: string, edits: readonly TextEdit[]): string {
  let out = ''
  let pos = 0
  for (const e of edits) {
    out += text.slice(pos, e.offset) + e.text
    pos = e.offset + e.length
  }
  return out + text.slice(pos)
}
