/*
 * Mixed line endings, preserved exactly. The editor model always uses "\n" (Monaco models have a single EOL, and every
 * text is normalised before it goes in), so a file with mixed line endings would be normalised on save. The
 * BreakTracker records every line break whose original sequence differs from the document's main line ending
 * ("marks", usually none) and maps them through the model's content changes:
 *
 *   - a break keeps its sequence as long as the break itself is not deleted (typing on its line, inserting lines
 *     around it, ... leave it alone);
 *   - new breaks (Enter, pasted text) get the main line ending;
 *   - undo / redo bring the marks of restored breaks back: the marks are remembered per model version (Monaco's
 *     "alternative version id" returns to earlier values on undo / redo), so an undone deletion gets its break back.
 *
 * `joinWithBreaks(text, marks, main)` rebuilds the text for saving. Positions are offsets of the break character in the
 * "\n"-joined document (= the end offset of the line it ends).
 */
import { eachBreak, EOL_SEQ, type Eol } from './codec'
import { textEdits } from './textdiff'

/** A line break (position in the "\n"-joined document) and its original sequence. */
export interface BreakInfo {
  pos: number
  eol: Eol
}

/** One content change as Monaco reports it (offsets in the document before the change). */
export interface TextChange {
  rangeOffset: number
  rangeLength: number
  text: string
}

/**
 * Map marks through a list of changes applied one after another (Monaco's event order: descending offsets within
 * one edit, merged edits in the order they were applied). A mark disappears when its break character is deleted;
 * text inserted right before a break goes in front of it.
 */
export function mapBreaks(list: readonly BreakInfo[], changes: readonly TextChange[]): BreakInfo[] {
  let cur: readonly BreakInfo[] = list
  for (const c of changes) {
    if (!cur.length) break
    const from = c.rangeOffset
    const to = c.rangeOffset + c.rangeLength
    const delta = c.text.length - c.rangeLength
    if (delta === 0 && c.rangeLength === 0) continue
    const next: BreakInfo[] = []
    for (const b of cur) {
      if (b.pos < from) next.push(b)
      else if (b.pos >= to) next.push(delta ? { pos: b.pos + delta, eol: b.eol } : b)
      // from <= pos < to: the break itself was deleted
    }
    cur = next
  }
  return cur === list ? list.slice() : (cur as BreakInfo[])
}

/** Keep only marks that sit on a line break of `text` (sorted, deduplicated). */
export function validBreaks(list: readonly BreakInfo[], text: string): BreakInfo[] {
  const out: BreakInfo[] = []
  const sorted = [...list].sort((a, b) => a.pos - b.pos)
  let last = -1
  for (const b of sorted) {
    if (b.pos === last || b.pos < 0 || b.pos >= text.length || text.charCodeAt(b.pos) !== 10) continue
    out.push(b)
    last = b.pos
  }
  return out
}

/** Versions remembered for undo / redo (older ones fall back to mapping). */
const MAX_VERSIONS = 4000

/**
 * Line-break marks of one editor model, following its edits, undo and redo. Feed it every content change event with
 * the model's alternative version id after the change.
 */
export class BreakTracker {
  private marks: readonly BreakInfo[]
  private readonly versions = new Map<number, readonly BreakInfo[]>()
  /** Nothing to track as long as the document never had a mark (the common case: one kind of line ending). */
  private active: boolean

  constructor(initial: readonly BreakInfo[] = [], version = 1) {
    this.marks = initial.slice()
    this.active = initial.length > 0
    if (this.active) this.versions.set(version, this.marks)
  }

  get(): readonly BreakInfo[] {
    return this.marks
  }

  count(): number {
    return this.marks.length
  }

  /**
   * Follow a content change. `undoRedo`: the change came from undo / redo — the marks of the version it returns to
   * are restored exactly (when still remembered).
   */
  update(changes: readonly TextChange[], version: number, undoRedo = false): void {
    if (!this.active) return
    const stored = undoRedo ? this.versions.get(version) : undefined
    this.marks = stored ?? mapBreaks(this.marks, changes)
    this.remember(version)
  }

  /** Replace every mark (explicit reset: new content, backup restore). Undo returns to the previous version's marks. */
  reset(marks: readonly BreakInfo[], version: number, clearHistory = false): void {
    if (clearHistory) this.versions.clear()
    this.marks = marks.slice()
    if (this.marks.length) this.active = true
    if (this.active) this.remember(version)
  }

  private remember(version: number): void {
    this.versions.delete(version)
    this.versions.set(version, this.marks)
    if (this.versions.size > MAX_VERSIONS) {
      const oldest = this.versions.keys().next().value
      if (oldest !== undefined) this.versions.delete(oldest)
    }
  }
}

/** `text` ("\n"-joined): breaks listed in `marks` (sorted by position) get their own sequence, all others `main`. */
export function joinWithBreaks(text: string, marks: readonly BreakInfo[], main: Eol): string {
  const sep = EOL_SEQ[main]
  if (!marks.length) return sep === '\n' ? text : text.replace(/\n/g, sep)
  const out: string[] = []
  let k = 0
  let start = 0
  for (let nl = text.indexOf('\n'); nl >= 0; nl = text.indexOf('\n', start)) {
    out.push(text.slice(start, nl))
    while (k < marks.length && marks[k].pos < nl) k++
    out.push(k < marks.length && marks[k].pos === nl ? EOL_SEQ[marks[k].eol] : sep)
    start = nl + 1
  }
  out.push(text.slice(start))
  return out.join('')
}

/** Normalise every line break of raw text to "\n" (what the editor model holds). */
export function normalizeBreaks(raw: string): string {
  return raw.includes('\r') ? raw.replace(/\r\n?/g, '\n') : raw
}

// ---------------------------------------------------------------------------------------------------------------------
// scanning raw text
// ---------------------------------------------------------------------------------------------------------------------

export interface ScannedBreaks {
  /** The most frequent line ending (ties and texts without breaks: `fallback`). */
  main: Eol
  /** More than one kind of line ending. */
  mixed: boolean
  /** Breaks that differ from `main`, as positions in the "\n"-joined text. */
  marks: BreakInfo[]
}

/**
 * Find the line endings of raw text (as read from a file). `main` forces the main line ending (marks are then every
 * break that differs from it); by default it is the most frequent one.
 */
export function scanBreaks(raw: string, fallback: Eol = 'lf', main?: Eol): ScannedBreaks {
  const counts: Record<Eol, number> = { lf: 0, crlf: 0, cr: 0 }
  // First pass counts; marks are only collected (second pass) for the breaks that differ from the main line ending.
  eachBreak(raw, (_pos, eol) => {
    counts[eol]++
  })
  const kinds = (['lf', 'crlf', 'cr'] as Eol[]).filter((k) => counts[k] > 0)
  if (!kinds.length) return { main: main ?? fallback, mixed: false, marks: [] }
  let dominant = kinds[0]
  for (const k of kinds) if (counts[k] > counts[dominant]) dominant = k
  if (counts[fallback] === counts[dominant]) dominant = fallback
  const m = main ?? dominant
  const marks: BreakInfo[] = []
  if (!(kinds.length === 1 && kinds[0] === m)) {
    eachBreak(raw, (pos, eol) => {
      if (eol !== m) marks.push({ pos, eol })
    })
  }
  return { main: m, mixed: kinds.length > 1, marks }
}

// ---------------------------------------------------------------------------------------------------------------------
// merging: line endings of another version of the document
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Marks for `text` ("\n"-joined) after it was merged with another version of the document (`otherRaw`, raw line
 * endings — e.g. the server's copy after a conflict). Every line break of `text` that is an untouched line break of
 * `otherRaw` (same place in the unchanged text around it) takes the other version's sequence; all other breaks keep
 * their current mark (`marks`, e.g. the editor's BreakTracker) or the main line ending. So after "take the server's
 * change" the server's lines keep the server's line endings, and the user's lines keep theirs.
 */
export function adoptBreaks(text: string, marks: readonly BreakInfo[], otherRaw: string, main: Eol): BreakInfo[] {
  const other: BreakInfo[] = []
  eachBreak(otherRaw, (pos, eol) => other.push({ pos, eol }))
  const edits = textEdits(normalizeBreaks(otherRaw), text)
  // Map the other version's breaks into `text` (edits are ascending and do not overlap).
  const theirs = new Map<number, Eol>()
  let k = 0
  let delta = 0
  for (const b of other) {
    // Edits that end at or before the break shift it (an insertion right in front of it included) ...
    while (k < edits.length && edits[k].offset + edits[k].length <= b.pos) {
      delta += edits[k].text.length - edits[k].length
      k++
    }
    // ... one that covers it deleted it.
    if (k < edits.length && edits[k].offset <= b.pos) continue
    theirs.set(b.pos + delta, b.eol)
  }
  const mine = new Map<number, Eol>()
  for (const m of marks) mine.set(m.pos, m.eol)
  const out: BreakInfo[] = []
  for (let nl = text.indexOf('\n'); nl >= 0; nl = text.indexOf('\n', nl + 1)) {
    const eol = theirs.get(nl) ?? mine.get(nl) ?? main
    if (eol !== main) out.push({ pos: nl, eol })
  }
  return out
}
