/*
 * A text model that reports its edits the way Monaco's TextModel does, for testing the BreakTracker without a DOM:
 *
 *   - one content-change event per edit operation; its changes are sorted by descending offset and every offset
 *     refers to the text before the operation (Monaco's piece tree applies them back to front);
 *   - operations grouped into one undo element are undone / redone as ONE event whose changes are the per-operation
 *     groups one after another (Monaco merges deferred events), each group again descending;
 *   - the alternative version id: a new id for every edit; undo returns to the id before the element, redo to the id
 *     after it (TextModel._overwriteAlternativeVersionId);
 *   - "\n" is the only line break (the editor normalises text before it goes in).
 */

function applyDesc(text, changes) {
  let t = text
  for (const c of changes) t = t.slice(0, c.rangeOffset) + c.text + t.slice(c.rangeOffset + c.rangeLength)
  return t
}

/** Edits [{offset, length, text}] (non-overlapping, offsets in `text`) → descending changes + their inverse. */
function prepare(text, edits) {
  const desc = [...edits].sort((a, b) => b.offset - a.offset).map((e) => ({ rangeOffset: e.offset, rangeLength: e.length, text: e.text }))
  const asc = [...edits].sort((a, b) => a.offset - b.offset)
  let delta = 0
  const inverse = []
  for (const e of asc) {
    inverse.push({ rangeOffset: e.offset + delta, rangeLength: e.text.length, text: text.slice(e.offset, e.offset + e.length) })
    delta += e.text.length - e.length
  }
  inverse.sort((a, b) => b.rangeOffset - a.rangeOffset)
  return { forward: desc, inverse }
}

export class FakeModel {
  constructor(text) {
    this.text = text
    this.version = 1
    this.alt = 1
    this.undoStack = []
    this.redoStack = []
    this.listeners = []
  }

  getValue() {
    return this.text
  }

  getAlternativeVersionId() {
    return this.alt
  }

  onDidChangeContent(fn) {
    this.listeners.push(fn)
  }

  emit(changes, flags) {
    for (const l of this.listeners) l({ changes, isFlush: false, isUndoing: false, isRedoing: false, ...flags })
  }

  /** One edit operation (several ranges = multi-cursor). `group`: part of the previous undo element. */
  edit(edits, { group = false } = {}) {
    const step = prepare(this.text, edits)
    const beforeAlt = this.alt
    this.text = applyDesc(this.text, step.forward)
    this.version++
    this.alt = this.version
    const top = this.undoStack[this.undoStack.length - 1]
    if (group && top) {
      top.steps.push(step)
      top.afterAlt = this.alt
    } else this.undoStack.push({ beforeAlt, afterAlt: this.alt, steps: [step] })
    this.redoStack = []
    this.emit(step.forward, {})
  }

  undo() {
    const el = this.undoStack.pop()
    if (!el) return false
    const changes = []
    for (let i = el.steps.length - 1; i >= 0; i--) {
      this.text = applyDesc(this.text, el.steps[i].inverse)
      changes.push(...el.steps[i].inverse)
    }
    this.version++
    this.alt = el.beforeAlt
    this.redoStack.push(el)
    this.emit(changes, { isUndoing: true })
    return true
  }

  redo() {
    const el = this.redoStack.pop()
    if (!el) return false
    const changes = []
    for (const s of el.steps) {
      this.text = applyDesc(this.text, s.forward)
      changes.push(...s.forward)
    }
    this.version++
    this.alt = el.afterAlt
    this.undoStack.push(el)
    this.emit(changes, { isRedoing: true })
    return true
  }
}
