/*
 * Hex editor (FILE-23): virtualised offset / hex / ASCII grid with overwrite and insert modes, selection, undo/redo,
 * hex or text search, go to offset, copy/paste, and a data inspector. Rows are virtualised with scroll scaling, so
 * files far beyond browsers' maximum element height (tens of MiB) still scroll correctly.
 *
 * Keyboard: arrows / PgUp / PgDn / Home / End (+Shift selects, Ctrl+Home/End = start/end), Tab switches hex ↔ text
 * column, Insert toggles insert/overwrite, 0-9 a-f (hex column) or any character (text column) edits, Backspace /
 * Delete remove bytes, Ctrl+Z / Ctrl+Y undo/redo, Ctrl+A select all, Ctrl+C / Ctrl+V copy/paste, Esc clears.
 */
import {
  useCallback,
  useEffect,
  useId,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
  type Ref,
} from 'react'
import { ArrowDown, ArrowUp, X } from 'lucide-react'
import { toast } from 'sonner'
import { prompt } from '@/components/ui/dialog-host'
import { cn, copyText, formatBytes } from '@/lib/utils'
import { StatusBar, StatusButton, StatusSpacer, StatusText } from './ui'

/** The bytes at one point of the edit history (what a save writes). */
export interface HexSnapshot {
  bytes: Uint8Array
  /** Edit the snapshot was taken after (0 = as loaded). */
  top: number
}

export interface HexEditorHandle {
  getBytes(): Uint8Array
  /** Current bytes + history position; later edits never modify `bytes` (they copy first). */
  snapshot(): HexSnapshot
  isDirty(): boolean
  /** `snap` (default: the current bytes) is what is saved on disk now; edits made after it stay unsaved. */
  markSaved(snap?: HexSnapshot): void
  find(): void
  gotoOffset(): void
  undo(): void
  redo(): void
  toggleInsert(): void
  focus(): void
}

export interface HexEditorProps {
  data: Uint8Array
  readOnly: boolean
  fontSize: number
  onDirtyChange?: (dirty: boolean) => void
  /** Status bar content rendered on the left (path, host...). */
  statusLeft?: ReactNode
  ref?: Ref<HexEditorHandle>
  label?: string
  autoFocus?: boolean
}

const COLS = 16
const MAX_SCROLL_PX = 8_000_000

interface Op {
  /** Unique, increasing: identifies a history position even across undo branches. */
  id: number
  at: number
  removed: Uint8Array
  inserted: Uint8Array
  cursorBefore: number
  cursorAfter: number
}

type Pane = 'hex' | 'text'

const HEX = Array.from({ length: 256 }, (_, i) => i.toString(16).padStart(2, '0').toUpperCase())

function printable(b: number): string {
  return b >= 0x20 && b < 0x7f ? String.fromCharCode(b) : b >= 0xa0 ? String.fromCharCode(b) : '.'
}

function parseHexString(s: string): Uint8Array | null {
  const clean = s.replace(/0x/gi, '').replace(/[\s,:;-]+/g, '')
  if (!clean || clean.length % 2 || /[^0-9a-f]/i.test(clean)) return null
  const out = new Uint8Array(clean.length / 2)
  for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16)
  return out
}

function parseOffset(s: string): number | null {
  const t = s.trim().toLowerCase()
  if (!t) return null
  if (/^0x[0-9a-f]+$/.test(t)) return parseInt(t.slice(2), 16)
  if (/^[0-9a-f]+h$/.test(t)) return parseInt(t.slice(0, -1), 16)
  if (/^\d+$/.test(t)) return parseInt(t, 10)
  if (/^[0-9a-f]+$/.test(t)) return parseInt(t, 16)
  return null
}

function indexOf(hay: Uint8Array, needle: Uint8Array, from: number, backwards: boolean): number {
  const n = needle.length
  if (!n || n > hay.length) return -1
  const first = needle[0]
  if (!backwards) {
    for (let i = Math.max(0, from); i <= hay.length - n; i++) {
      if (hay[i] !== first) continue
      let j = 1
      while (j < n && hay[i + j] === needle[j]) j++
      if (j === n) return i
    }
  } else {
    for (let i = Math.min(from, hay.length - n); i >= 0; i--) {
      if (hay[i] !== first) continue
      let j = 1
      while (j < n && hay[i + j] === needle[j]) j++
      if (j === n) return i
    }
  }
  return -1
}

export function HexEditor({ data, readOnly, fontSize, onDirtyChange, statusLeft, ref, label, autoFocus }: HexEditorProps) {
  const helpId = useId()
  const buf = useRef<Uint8Array>(data)
  const orig = useRef<Uint8Array>(data)
  /** `buf` is a private copy that may be edited in place (never `data` itself nor the saved snapshot). */
  const owned = useRef(false)
  const undoStack = useRef<Op[]>([])
  const redoStack = useRef<Op[]>([])
  const opSeq = useRef(0)
  /** History position (top op id) of the bytes on disk. */
  const savedTop = useRef(0)
  const mods = useRef<Set<number>>(new Set())
  const [rev, setRev] = useState(0)
  const [cursor, setCursor] = useState(0)
  const [nibble, setNibble] = useState(0)
  const [anchor, setAnchor] = useState<number | null>(null)
  const [pane, setPane] = useState<Pane>('hex')
  const [insert, setInsert] = useState(false)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewH, setViewH] = useState(400)
  const [findOpen, setFindOpen] = useState(false)
  const [findText, setFindText] = useState('')
  const [findMode, setFindMode] = useState<'hex' | 'text'>('hex')
  const [match, setMatch] = useState<{ at: number; len: number } | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const gridRef = useRef<HTMLDivElement>(null)
  const findRef = useRef<HTMLInputElement>(null)
  const dragging = useRef(false)
  const dragCleanup = useRef<(() => void) | null>(null)
  const onDirtyRef = useRef(onDirtyChange)
  onDirtyRef.current = onDirtyChange

  const rowH = Math.max(14, Math.round(fontSize * 1.55))
  const len = buf.current.length
  const maxCursor = insert ? len : Math.max(0, len - 1)
  const rows = Math.max(1, Math.floor((insert ? len : Math.max(0, len - 1)) / COLS) + 1)
  const totalPx = rows * rowH
  const spacerPx = Math.min(totalPx, MAX_SCROLL_PX)
  const ratio = spacerPx > viewH ? Math.max(1, (totalPx - viewH) / Math.max(1, spacerPx - viewH)) : 1
  const virtualTop = scrollTop * ratio
  const firstRow = Math.max(0, Math.floor(virtualTop / rowH))
  const visibleRows = Math.ceil(viewH / rowH) + 1
  const lastRow = Math.min(rows - 1, firstRow + visibleRows)

  const selFrom = anchor === null ? -1 : Math.min(anchor, cursor)
  const selTo = anchor === null ? -1 : Math.max(anchor, cursor)

  const bump = () => setRev((r) => r + 1)

  const topId = () => undoStack.current[undoStack.current.length - 1]?.id ?? 0
  const dirtyNow = () => topId() !== savedTop.current
  const notifyDirty = useCallback(() => onDirtyRef.current?.((undoStack.current[undoStack.current.length - 1]?.id ?? 0) !== savedTop.current), [])

  // ---------------------------------------------------------------------------------------------------------------
  // scrolling

  useEffect(() => {
    if (!autoFocus) return
    const t = setTimeout(() => {
      const ae = document.activeElement
      if (!ae || ae === document.body) gridRef.current?.focus()
    }, 50)
    return () => clearTimeout(t)
    // mount only
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const ro = new ResizeObserver(() => setViewH(el.clientHeight))
    ro.observe(el)
    setViewH(el.clientHeight)
    return () => ro.disconnect()
  }, [])

  const scrollToRow = useCallback(
    (row: number, align: 'nearest' | 'center' = 'nearest') => {
      const el = scrollRef.current
      if (!el) return
      const top = row * rowH
      let vt = virtualTop
      if (align === 'center') vt = top - viewH / 2 + rowH / 2
      else if (top < vt) vt = top
      else if (top + rowH > vt + viewH) vt = top + rowH - viewH
      vt = Math.max(0, Math.min(vt, totalPx - viewH))
      const st = vt / ratio
      if (Math.abs(st - el.scrollTop) > 0.5) {
        el.scrollTop = st
        setScrollTop(st)
      }
    },
    [rowH, virtualTop, viewH, totalPx, ratio],
  )

  const moveTo = useCallback(
    (pos: number, opts: { select?: boolean; keepNibble?: boolean; align?: 'nearest' | 'center' } = {}) => {
      const p = Math.max(0, Math.min(maxCursor, pos))
      if (opts.select) setAnchor((a) => (a === null ? cursor : a))
      else setAnchor(null)
      setCursor(p)
      if (!opts.keepNibble) setNibble(0)
      scrollToRow(Math.floor(p / COLS), opts.align)
    },
    [cursor, maxCursor, scrollToRow],
  )

  // ---------------------------------------------------------------------------------------------------------------
  // editing

  const apply = useCallback(
    (op: Op, record: 'new' | 'undo' | 'redo') => {
      const b = buf.current
      if (op.removed.length === op.inserted.length) {
        // Overwrite: edit in place (copy once first — the buffer may still be the loaded / saved data).
        const target = owned.current ? b : b.slice()
        target.set(op.inserted, op.at)
        buf.current = target
      } else {
        const next = new Uint8Array(b.length - op.removed.length + op.inserted.length)
        next.set(b.subarray(0, op.at))
        next.set(op.inserted, op.at)
        next.set(b.subarray(op.at + op.removed.length), op.at + op.inserted.length)
        buf.current = next
      }
      owned.current = true
      // shift modification marks
      const delta = op.inserted.length - op.removed.length
      if (delta !== 0) {
        const shifted = new Set<number>()
        for (const i of mods.current) {
          if (i < op.at) shifted.add(i)
          else if (i >= op.at + op.removed.length) shifted.add(i + delta)
        }
        mods.current = shifted
      }
      for (let i = 0; i < op.inserted.length; i++) mods.current.add(op.at + i)
      if (record === 'new') {
        undoStack.current.push(op)
        redoStack.current = []
      }
      bump()
      notifyDirty()
    },
    [notifyDirty],
  )

  const replace = useCallback(
    (at: number, removeLen: number, inserted: Uint8Array, cursorAfter: number) => {
      if (readOnly) return
      const removed = buf.current.slice(at, at + removeLen)
      apply({ id: ++opSeq.current, at, removed, inserted, cursorBefore: cursor, cursorAfter }, 'new')
    },
    [apply, cursor, readOnly],
  )

  const undo = useCallback(() => {
    const op = undoStack.current.pop()
    if (!op) return
    apply({ id: op.id, at: op.at, removed: op.inserted, inserted: op.removed, cursorBefore: op.cursorAfter, cursorAfter: op.cursorBefore }, 'undo')
    redoStack.current.push(op)
    setAnchor(null)
    setCursor(Math.min(op.cursorBefore, Math.max(0, buf.current.length - (insert ? 0 : 1))))
    notifyDirty()
  }, [apply, insert, notifyDirty])

  const redo = useCallback(() => {
    const op = redoStack.current.pop()
    if (!op) return
    apply(op, 'redo')
    undoStack.current.push(op)
    setAnchor(null)
    setCursor(Math.min(op.cursorAfter, Math.max(0, buf.current.length - (insert ? 0 : 1))))
    notifyDirty()
  }, [apply, insert, notifyDirty])

  const deleteRange = useCallback(
    (from: number, to: number) => {
      if (to <= from) return
      replace(from, to - from, new Uint8Array(0), from)
      setAnchor(null)
      setCursor(Math.max(0, Math.min(from, buf.current.length - (insert ? 0 : 1))))
      setNibble(0)
    },
    [insert, replace],
  )

  const writeBytes = useCallback(
    (bytes: Uint8Array) => {
      if (readOnly || !bytes.length) return
      let at = cursor
      let removeLen = 0
      if (anchor !== null) {
        at = selFrom
        removeLen = selTo - selFrom + 1
      } else if (!insert) {
        removeLen = Math.min(bytes.length, buf.current.length - at)
      }
      replace(at, removeLen, bytes, at + bytes.length)
      setAnchor(null)
      setNibble(0)
      const end = at + bytes.length
      setCursor(Math.max(0, Math.min(end, buf.current.length - (insert ? 0 : 1))))
      scrollToRow(Math.floor(end / COLS))
    },
    [anchor, cursor, insert, readOnly, replace, scrollToRow, selFrom, selTo],
  )

  const typeNibble = useCallback(
    (d: number) => {
      if (readOnly) return
      const b = buf.current
      if (anchor !== null) {
        // Typing over a selection replaces it with the new byte.
        replace(selFrom, selTo - selFrom + 1, Uint8Array.of(d << 4), selFrom)
        setAnchor(null)
        setCursor(selFrom)
        setNibble(1)
        return
      }
      if (nibble === 0) {
        if (insert || cursor >= b.length) {
          replace(cursor, 0, Uint8Array.of(d << 4), cursor)
        } else {
          replace(cursor, 1, Uint8Array.of((d << 4) | (b[cursor] & 0x0f)), cursor)
        }
        setNibble(1)
      } else {
        replace(cursor, 1, Uint8Array.of((b[cursor] & 0xf0) | d), cursor + 1)
        setNibble(0)
        const next = Math.min(cursor + 1, insert ? buf.current.length : Math.max(0, buf.current.length - 1))
        setCursor(next)
        scrollToRow(Math.floor(next / COLS))
      }
    },
    [anchor, cursor, insert, nibble, readOnly, replace, scrollToRow, selFrom, selTo],
  )

  // ---------------------------------------------------------------------------------------------------------------
  // search / goto

  const needle = useMemo((): Uint8Array | null => {
    if (!findText) return null
    if (findMode === 'hex') return parseHexString(findText)
    return new TextEncoder().encode(findText)
  }, [findText, findMode])

  const runFind = useCallback(
    (dir: 1 | -1) => {
      if (!needle) {
        if (findText) toast.error(findMode === 'hex' ? 'Enter hex bytes, e.g. "DE AD BE EF"' : 'Nothing to search for')
        return
      }
      const b = buf.current
      const start = match ? (dir > 0 ? match.at + 1 : match.at - 1) : dir > 0 ? cursor : cursor - 1
      let at = indexOf(b, needle, start, dir < 0)
      if (at < 0) at = indexOf(b, needle, dir > 0 ? 0 : b.length - needle.length, dir < 0)
      if (at < 0) {
        setMatch(null)
        toast.info('No matches')
        return
      }
      setMatch({ at, len: needle.length })
      setAnchor(at)
      setCursor(at + needle.length - 1)
      setNibble(0)
      scrollToRow(Math.floor(at / COLS), 'center')
    },
    [cursor, findMode, findText, match, needle, scrollToRow],
  )

  const openFind = useCallback(() => {
    setFindOpen(true)
    requestAnimationFrame(() => {
      findRef.current?.focus()
      findRef.current?.select()
    })
  }, [])

  const gotoOffset = useCallback(async () => {
    const v = await prompt({
      title: 'Go to offset',
      label: 'Offset (decimal, 0x hex, or 1Fh)',
      defaultValue: `0x${cursor.toString(16).toUpperCase()}`,
      confirmLabel: 'Go',
      validate: (s) => {
        const n = parseOffset(s)
        if (n === null) return 'Enter a number, e.g. 4096 or 0x1000'
        if (n > buf.current.length) return `Beyond the end of the data (${buf.current.length} bytes)`
        return null
      },
    })
    const n = v === null ? null : parseOffset(v)
    if (n !== null) {
      moveTo(n, { align: 'center' })
      gridRef.current?.focus()
    }
  }, [cursor, moveTo])

  const copySelection = useCallback(async () => {
    const b = buf.current
    const from = anchor === null ? cursor : selFrom
    const to = anchor === null ? cursor : selTo
    if (from >= b.length) return
    const slice = b.subarray(from, Math.min(b.length, to + 1))
    const text = pane === 'hex' ? Array.from(slice, (x) => HEX[x]).join(' ') : Array.from(slice, printable).join('')
    if (await copyText(text)) toast.success(`Copied ${slice.length} byte${slice.length === 1 ? '' : 's'}${pane === 'hex' ? ' as hex' : ' as text'}`)
  }, [anchor, cursor, pane, selFrom, selTo])

  const paste = useCallback(
    (text: string) => {
      if (readOnly) return
      let bytes: Uint8Array | null
      if (pane === 'hex') {
        bytes = parseHexString(text)
        if (!bytes) {
          toast.error('The clipboard does not contain hex bytes', { description: 'Paste into the text column to insert text.' })
          return
        }
      } else {
        bytes = new TextEncoder().encode(text)
      }
      writeBytes(bytes)
    },
    [pane, readOnly, writeBytes],
  )

  // ---------------------------------------------------------------------------------------------------------------
  // imperative handle

  useImperativeHandle(
    ref,
    () => ({
      getBytes: () => buf.current,
      snapshot: () => {
        // The snapshot must not change under the caller: the next edit copies the buffer first.
        owned.current = false
        return { bytes: buf.current, top: topId() }
      },
      isDirty: dirtyNow,
      markSaved: (snap) => {
        savedTop.current = snap ? snap.top : topId()
        orig.current = snap ? snap.bytes : buf.current
        if (!snap) owned.current = false
        mods.current = new Set()
        bump()
        notifyDirty()
      },
      find: openFind,
      gotoOffset: () => void gotoOffset(),
      undo,
      redo,
      toggleInsert: () => setInsert((v) => !v),
      focus: () => gridRef.current?.focus(),
    }),
    [gotoOffset, notifyDirty, openFind, redo, undo],
  )

  useEffect(() => () => dragCleanup.current?.(), [])

  useEffect(() => {
    // Keep the cursor valid when leaving insert mode at EOF.
    if (!insert && cursor > Math.max(0, buf.current.length - 1)) setCursor(Math.max(0, buf.current.length - 1))
  }, [insert, cursor])

  // ---------------------------------------------------------------------------------------------------------------
  // keyboard

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const mod = e.ctrlKey || e.metaKey
    const k = e.key
    const pageRows = Math.max(1, Math.floor(viewH / rowH) - 1)
    const nav = (pos: number) => {
      e.preventDefault()
      moveTo(pos, { select: e.shiftKey })
    }
    if (mod && !e.altKey) {
      const lower = k.toLowerCase()
      if (lower === 'z' && !e.shiftKey) {
        e.preventDefault()
        undo()
      } else if ((lower === 'z' && e.shiftKey) || lower === 'y') {
        e.preventDefault()
        redo()
      } else if (lower === 'a') {
        e.preventDefault()
        setAnchor(0)
        setCursor(Math.max(0, buf.current.length - 1))
      } else if (lower === 'c' || lower === 'x') {
        e.preventDefault()
        void copySelection()
        if (lower === 'x' && anchor !== null) deleteRange(selFrom, selTo + 1)
      } else if (k === 'Home') nav(0)
      else if (k === 'End') nav(maxCursor)
      return
    }
    switch (k) {
      case 'ArrowLeft':
        return nav(cursor - 1)
      case 'ArrowRight':
        return nav(cursor + 1)
      case 'ArrowUp':
        return nav(cursor - COLS)
      case 'ArrowDown':
        return nav(Math.min(maxCursor, cursor + COLS))
      case 'PageUp':
        return nav(cursor - pageRows * COLS)
      case 'PageDown':
        return nav(Math.min(maxCursor, cursor + pageRows * COLS))
      case 'Home':
        return nav(cursor - (cursor % COLS))
      case 'End':
        return nav(Math.min(maxCursor, cursor - (cursor % COLS) + COLS - 1))
      case 'Tab':
        e.preventDefault()
        setPane((p) => (p === 'hex' ? 'text' : 'hex'))
        setNibble(0)
        return
      case 'Insert':
        e.preventDefault()
        setInsert((v) => !v)
        return
      case 'Escape':
        if (anchor !== null || match) {
          e.preventDefault()
          setAnchor(null)
          setMatch(null)
        }
        return
      case 'Backspace':
        e.preventDefault()
        if (readOnly) return
        if (anchor !== null) deleteRange(selFrom, selTo + 1)
        else if (insert && cursor > 0) deleteRange(cursor - 1, cursor)
        else moveTo(cursor - 1)
        return
      case 'Delete':
        e.preventDefault()
        if (readOnly) return
        if (anchor !== null) deleteRange(selFrom, selTo + 1)
        else if (cursor < buf.current.length) deleteRange(cursor, cursor + 1)
        return
    }
    if (e.altKey || k.length !== 1) return
    if (readOnly) {
      if (/^[\x20-\x7e]$/.test(k)) toast.info('The file is read-only', { id: 'hex-readonly' })
      return
    }
    if (pane === 'hex') {
      if (/^[0-9a-f]$/i.test(k)) {
        e.preventDefault()
        typeNibble(parseInt(k, 16))
      }
      return
    }
    const code = k.charCodeAt(0)
    if (code <= 0xff) {
      e.preventDefault()
      writeBytes(Uint8Array.of(code))
    }
  }

  // ---------------------------------------------------------------------------------------------------------------
  // mouse

  const posFromEvent = (e: ReactMouseEvent): { pos: number; pane: Pane } | null => {
    const el = (e.target as HTMLElement).closest<HTMLElement>('[data-pos]')
    if (!el) return null
    return { pos: Number(el.dataset.pos), pane: el.dataset.pane === 'text' ? 'text' : 'hex' }
  }

  // ---------------------------------------------------------------------------------------------------------------
  // render

  const b = buf.current
  const sameLen = b.length === orig.current.length
  const rowEls = []
  for (let r = firstRow; r <= lastRow; r++) {
    const start = r * COLS
    const top = scrollTop + (r * rowH - virtualTop)
    const hexCells = []
    const textCells = []
    for (let c = 0; c < COLS; c++) {
      const i = start + c
      const exists = i < b.length
      const isCursorCell = i === cursor
      if (!exists && !(insert && i === b.length)) {
        hexCells.push(<span key={c} className={cn('nx-hex-cell', c === 7 && 'mr-[1ch]')} style={{ width: '2.6ch' }} />)
        textCells.push(<span key={c} className="nx-hex-cell" style={{ width: '1.2ch' }} />)
        continue
      }
      const v = exists ? b[i] : -1
      const sel = anchor !== null && i >= selFrom && i <= selTo
      const modified = exists && (sameLen ? v !== orig.current[i] : mods.current.has(i))
      const inMatch = !!match && i >= match.at && i < match.at + match.len
      const common = {
        'data-pos': i,
        'data-sel': sel || undefined,
        'data-mod': modified || undefined,
        'data-match': inMatch && !sel ? true : undefined,
        'data-cursor': isCursorCell || undefined,
      }
      hexCells.push(
        <span
          key={c}
          {...common}
          data-pane="hex"
          data-pane-active={isCursorCell && pane === 'hex' ? true : undefined}
          data-zero={v === 0 ? true : undefined}
          className={cn('nx-hex-cell', c === 7 && 'mr-[1ch]')}
          style={{ width: '2.6ch' }}
        >
          {v < 0 ? '··' : isCursorCell && nibble === 1 && pane === 'hex' ? <><u>{HEX[v][0]}</u>{HEX[v][1]}</> : HEX[v]}
        </span>,
      )
      textCells.push(
        <span
          key={c}
          {...common}
          data-pane="text"
          data-pane-active={isCursorCell && pane === 'text' ? true : undefined}
          data-np={v >= 0 && printable(v) === '.' && v !== 0x2e ? true : undefined}
          className="nx-hex-cell"
          style={{ width: '1.2ch' }}
        >
          {v < 0 ? ' ' : printable(v)}
        </span>,
      )
    }
    rowEls.push(
      <div key={r} className="absolute left-0 flex items-center whitespace-pre" style={{ top, height: rowH }} aria-hidden>
        <span className="w-[10ch] shrink-0 pl-3 text-muted-foreground/80 select-none">
          {start.toString(16).toUpperCase().padStart(8, '0')}
        </span>
        <span className="ml-[1ch] shrink-0">{hexCells}</span>
        <span className="ml-[2ch] shrink-0 border-l pl-[1ch]">{textCells}</span>
      </div>,
    )
  }

  const value = cursor < b.length ? b[cursor] : null
  const inspector = useMemo(() => {
    if (value === null) return ''
    const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
    const parts = [`i8 ${(value << 24) >> 24}`]
    if (cursor + 2 <= b.length) parts.push(`u16le ${dv.getUint16(cursor, true)}`)
    if (cursor + 4 <= b.length) parts.push(`u32le ${dv.getUint32(cursor, true)}`, `i32le ${dv.getInt32(cursor, true)}`, `f32le ${dv.getFloat32(cursor, true).toPrecision(6)}`)
    if (cursor + 8 <= b.length) parts.push(`u64le ${dv.getBigUint64(cursor, true)}`)
    return parts.join(' · ')
    // rev: bytes after the cursor may change in place
  }, [b, cursor, value, rev])

  const selLen = anchor === null ? 0 : selTo - selFrom + 1

  return (
    <div className="flex min-h-0 flex-1 flex-col" style={{ fontSize }}>
      {findOpen && (
        <div className="flex shrink-0 flex-wrap items-center gap-1.5 border-b bg-toolbar px-2 py-1.5 text-sm" role="search">
          <select
            aria-label="Search as"
            className="h-7 rounded-sm border border-input bg-background px-1.5 text-sm"
            value={findMode}
            onChange={(e) => {
              setFindMode(e.target.value as 'hex' | 'text')
              setMatch(null)
            }}
          >
            <option value="hex">Hex bytes</option>
            <option value="text">Text (UTF-8)</option>
          </select>
          <input
            ref={findRef}
            value={findText}
            onChange={(e) => {
              setFindText(e.target.value)
              setMatch(null)
            }}
            placeholder={findMode === 'hex' ? 'e.g. 7F 45 4C 46' : 'Text to find'}
            aria-label="Find"
            className="h-7 min-w-40 flex-1 rounded-sm border border-input bg-background px-2 font-mono text-sm outline-none focus-visible:border-ring"
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                runFind(e.shiftKey ? -1 : 1)
              } else if (e.key === 'Escape') {
                e.preventDefault()
                setFindOpen(false)
                setMatch(null)
                gridRef.current?.focus()
              }
            }}
          />
          <button type="button" className="rounded-sm p-1 text-muted-foreground hover:bg-accent hover:text-foreground" aria-label="Previous match" onClick={() => runFind(-1)}>
            <ArrowUp className="size-3.5" />
          </button>
          <button type="button" className="rounded-sm p-1 text-muted-foreground hover:bg-accent hover:text-foreground" aria-label="Next match" onClick={() => runFind(1)}>
            <ArrowDown className="size-3.5" />
          </button>
          <span className="min-w-24 text-xs text-muted-foreground" aria-live="polite">
            {match ? `at 0x${match.at.toString(16).toUpperCase()}` : ''}
          </span>
          <button
            type="button"
            className="rounded-sm p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-label="Close search"
            onClick={() => {
              setFindOpen(false)
              setMatch(null)
              gridRef.current?.focus()
            }}
          >
            <X className="size-3.5" />
          </button>
        </div>
      )}
      <div
        ref={scrollRef}
        className="relative min-h-0 flex-1 overflow-auto bg-panel"
        onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
      >
        <div style={{ height: spacerPx, minWidth: '82ch' }} className="relative">
          <div
            ref={gridRef}
            tabIndex={0}
            role="application"
            aria-roledescription="hex editor"
            aria-label={label ? `Hex editor: ${label}` : 'Hex editor'}
            aria-describedby={helpId}
            className="nx-hex absolute inset-0 outline-none select-none"
            onKeyDown={onKeyDown}
            onPaste={(e) => {
              const text = e.clipboardData.getData('text/plain')
              if (text) {
                e.preventDefault()
                paste(text)
              }
            }}
            onMouseDown={(e) => {
              if (e.button !== 0) return
              const hit = posFromEvent(e)
              if (!hit) return
              e.preventDefault()
              gridRef.current?.focus()
              setPane(hit.pane)
              if (e.shiftKey) {
                setAnchor((a) => (a === null ? cursor : a))
                setCursor(Math.min(hit.pos, maxCursor))
              } else {
                setAnchor(null)
                setCursor(Math.min(hit.pos, maxCursor))
              }
              setNibble(0)
              dragging.current = true
              const up = () => {
                dragging.current = false
                window.removeEventListener('mouseup', up)
                dragCleanup.current = null
              }
              dragCleanup.current?.()
              dragCleanup.current = up
              window.addEventListener('mouseup', up)
            }}
            onMouseMove={(e) => {
              if (!dragging.current) return
              const hit = posFromEvent(e)
              if (!hit) return
              setAnchor((a) => (a === null ? cursor : a))
              setCursor(Math.min(hit.pos, maxCursor))
            }}
          >
            {rowEls}
          </div>
        </div>
      </div>
      <StatusBar>
        {statusLeft}
        <StatusSpacer />
        <StatusText className="tabular hidden @sm:inline-flex" title="Cursor offset">
          0x{cursor.toString(16).toUpperCase().padStart(8, '0')} ({cursor})
        </StatusText>
        {selLen > 0 && <StatusText className="tabular">{selLen} selected</StatusText>}
        {value !== null && (
          <StatusText className="tabular hidden @md:inline-flex" title={inspector}>
            {HEX[value]} · {value} · 0b{value.toString(2).padStart(8, '0')}
          </StatusText>
        )}
        <StatusButton
          onClick={() => setInsert((v) => !v)}
          tooltip="Insert / overwrite (Insert key)"
          aria-label={insert ? 'Insert mode, switch to overwrite' : 'Overwrite mode, switch to insert'}
          disabled={readOnly}
        >
          {readOnly ? 'READ' : insert ? 'INS' : 'OVR'}
        </StatusButton>
        <StatusText className="tabular" title={`${b.length.toLocaleString()} bytes`}>
          {formatBytes(b.length)}
        </StatusText>
      </StatusBar>
      <span className="sr-only" aria-live="polite">
        {value !== null ? `Offset ${cursor}, value ${HEX[value]}${anchor !== null ? `, ${selLen} selected` : ''}` : ''}
      </span>
      <span id={helpId} className="sr-only">
        {readOnly ? 'Read-only. ' : ''}Arrow keys, Page Up and Page Down move; Shift selects. Type hex digits in the hex column or characters in
        the text column to edit; Tab switches columns; Insert toggles insert mode; Control+Z undoes.
      </span>
    </div>
  )
}
