/* Indentation detection (tabs vs spaces, width) and conversion, over any line source (the editor model, a string). */

export interface IndentStyle {
  useTabs: boolean
  size: number
}

/** Read access to the lines of a document (1-based). */
export interface LineSource {
  lineCount: number
  line(n: number): string
}

export function linesOf(text: string): LineSource {
  const lines = text.split('\n')
  return { lineCount: lines.length, line: (n) => lines[n - 1] ?? '' }
}

/**
 * Guess the indentation of a document from its first lines: tabs vs spaces by majority of indented lines, width by the
 * most frequent indentation step between consecutive lines. Null when the document has no indented lines.
 */
export function detectIndentation(doc: LineSource, maxLines = 5000): IndentStyle | null {
  let tabs = 0
  let spaces = 0
  const deltas = new Map<number, number>()
  let prevSpaces = 0
  const lines = Math.min(doc.lineCount, maxLines)
  for (let i = 1; i <= lines; i++) {
    const text = doc.line(i)
    if (!text.trim()) continue
    const c = text.charCodeAt(0)
    if (c === 9) {
      tabs++
      prevSpaces = 0
      continue
    }
    let n = 0
    while (n < text.length && text.charCodeAt(n) === 32) n++
    if (n > 0 && text.charCodeAt(n) !== 9) {
      // Skip JSDoc-style " * " continuation lines (odd single-space indents).
      if (!(text[n] === '*' && n % 2 === 1)) spaces++
    }
    const d = Math.abs(n - prevSpaces)
    if (d >= 2 && d <= 8) deltas.set(d, (deltas.get(d) ?? 0) + 1)
    prevSpaces = n
  }
  if (!tabs && !spaces) return null
  if (tabs > spaces) return { useTabs: true, size: 0 }
  let best = 0
  let bestCount = 0
  for (const [d, count] of deltas) {
    // Prefer the smaller width on near-ties (4-space files also produce 8-space steps).
    if (count > bestCount * 1.1 || (count >= bestCount * 0.9 && d < best)) {
      best = d
      bestCount = count
    }
  }
  return { useTabs: false, size: best || 4 }
}

/** One line's leading whitespace to replace: `length` characters from the start of line `line` become `insert`. */
export interface IndentEdit {
  line: number
  length: number
  insert: string
}

/** Edits that re-indent the leading whitespace of every line (spaces ↔ tabs); empty when nothing changes. */
export function convertIndentation(doc: LineSource, toTabs: boolean, size: number): IndentEdit[] {
  const edits: IndentEdit[] = []
  for (let i = 1; i <= doc.lineCount; i++) {
    const m = /^[ \t]+/.exec(doc.line(i))
    if (!m) continue
    let col = 0
    for (const ch of m[0]) col = ch === '\t' ? col + size - (col % size) : col + 1
    const insert = toTabs ? '\t'.repeat(Math.floor(col / size)) + ' '.repeat(col % size) : ' '.repeat(col)
    if (insert !== m[0]) edits.push({ line: i, length: m[0].length, insert })
  }
  return edits
}

/** Visual (1-based) column of a character offset in a line, with tabs expanded to tab stops. */
export function visualColumn(line: string, offset: number, tabSize: number): number {
  let col = 0
  const end = Math.min(offset, line.length)
  for (let i = 0; i < end; i++) col = line.charCodeAt(i) === 9 ? col + tabSize - (col % tabSize) : col + 1
  return col + 1
}
