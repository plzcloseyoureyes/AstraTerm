/*
 * Whitespace-insensitive diffs (pure helpers for monaco/services.ts). The line diff is computed on the texts with all
 * whitespace removed from every line (same line structure, so its line ranges apply to the real texts); the character
 * changes highlighted inside a changed block come from a normal diff of the real texts, minus those that differ only
 * in whitespace.
 */

interface LineSpan {
  startLineNumber: number
  /** Exclusive; equal to the start for an empty span (insertion point). */
  endLineNumberExclusive: number
}

export interface Rng {
  startLineNumber: number
  startColumn: number
  endLineNumber: number
  endColumn: number
}

export interface InnerChange {
  originalRange: Rng
  modifiedRange: Rng
}

export interface DiffBlock {
  original: LineSpan
  modified: LineSpan
}

/** Every line without whitespace, joined with "\n" (the line count is kept). */
export function stripWhitespaceLines(lines: readonly string[]): string {
  return lines.map((l) => l.replace(/\s+/g, '')).join('\n')
}

/** Does a range start inside the span (an empty span: at its insertion point, i.e. its line or the end of the one before)? */
function starts(r: Rng, span: LineSpan): boolean {
  if (span.endLineNumberExclusive > span.startLineNumber) return r.startLineNumber >= span.startLineNumber && r.startLineNumber < span.endLineNumberExclusive
  return r.startLineNumber >= span.startLineNumber - 1 && r.startLineNumber <= span.startLineNumber
}

const squash = (s: string) => s.replace(/\s+/g, '')

/** The character changes (of a normal diff) that lie in `block` and differ in more than whitespace. */
export function blockInnerChanges(
  block: DiffBlock,
  inner: readonly InnerChange[],
  originalText: (r: Rng) => string,
  modifiedText: (r: Rng) => string,
): InnerChange[] {
  return inner.filter(
    (c) =>
      starts(c.originalRange, block.original) &&
      starts(c.modifiedRange, block.modified) &&
      squash(originalText(c.originalRange)) !== squash(modifiedText(c.modifiedRange)),
  )
}
