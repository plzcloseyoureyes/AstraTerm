/*
 * Tab kind "diff" — the text compare tool (TOOL-3): compare remote files, local files, pasted text, the clipboard or open editor
 * tabs; side by side or inline; next/previous change (F7 / Shift+F7); swap; ignore whitespace; collapse unchanged;
 * copy a change to the other side; edit and save either side back.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ArrowLeftRight,
  ChevronDown,
  ChevronUp,
  ClipboardPaste,
  Columns2,
  FileText,
  FileUp,
  FolderOpen,
  MoveLeft,
  MoveRight,
  Pilcrow,
  RefreshCw,
  Rows2,
  Save,
  Server,
  Type,
} from 'lucide-react'
import { toast } from 'sonner'
import type { FsOpenRequest } from '@/api/types'
import type { TabInfo, TabProps } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { prompt } from '@/components/ui/dialog-host'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { IconButton } from '@/components/ui/icon-button'
import { LoadingState } from '@/components/ui/query-state'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { Tooltip } from '@/components/ui/tooltip'
import { useLatest } from '@/lib/hooks'
import { cn, errorMessage, formatDateTime } from '@/lib/utils'
import { listTabs, setTabTitle, updateTabParams } from '@/stores/workspace'
import { downloadBlob, handleLabel, isConflict, isHandleGone, openFs, readFile, rememberHandle, writeFile } from './api'
import { base64ToBytes, baseName, decodeBytes, sniffBytes } from './codec'
import { scanBreaks, type BreakInfo } from './eol'
import { applyDirtyTitle, EDITOR_KINDS, getController, registerController, setTabDirty } from './controllers'
import { choose, pickRemoteFile, saveAsDialog } from './dialogs'
import { getDoc, putDoc } from './docstore'
import { MergePane, type MergePaneHandle, type Side } from './MergePane'
import { pickLocalFile, toDiffSource } from './open'
import { textBytes, writeBody, writeLocalHandle, type TextMeta } from './save'
import { editorSettings, FONT_MAX, FONT_MIN } from './settings'
import type { DiffSource, DiffTabParams } from './types'

const TEXT_MAX = 64 * 1024 * 1024

interface SideState {
  status: 'empty' | 'loading' | 'ready' | 'error'
  /** "\n"-joined text. */
  text: string
  savedText: string
  /** Line breaks that differ from the file's main line ending (kept when saving, see eol.ts). */
  breaks?: BreakInfo[]
  error?: string
  mtime?: string
  meta?: TextMeta
}

const EMPTY: SideState = { status: 'empty', text: '', savedText: '' }

function sourceLabel(src: DiffSource | undefined): string {
  if (!src) return 'Choose…'
  return src.kind === 'remote' ? baseName(src.path) : src.title
}

function sourceDetail(src: DiffSource | undefined): string {
  if (!src) return ''
  if (src.kind === 'remote') {
    const host = handleLabel(src.fsId, src.label)
    return host ? `${host}:${src.path}` : src.path
  }
  return 'Text'
}

export function diffTitle(p: DiffTabParams | undefined): string {
  if (!p?.left && !p?.right) return 'Text diff'
  return `${sourceLabel(p?.left)} ↔ ${sourceLabel(p?.right)}`
}

async function loadSource(src: DiffSource): Promise<SideState> {
  if (src.kind === 'text') {
    const rec = await getDoc(src.docId)
    const text = (rec?.content ?? '').replace(/\r\n?/g, '\n')
    return { status: 'ready', text, savedText: text }
  }
  const res = await readFile(src.fsId, src.path, TEXT_MAX)
  let text: string
  let encoding = 'utf-8'
  let bom = false
  if (res.encoding === 'base64') {
    const bytes = base64ToBytes(res.content ?? '')
    const sn = sniffBytes(bytes)
    if (sn.kind === 'binary') return { status: 'error', text: '', savedText: '', error: `${baseName(src.path)} is a binary file (${sn.reason.toLowerCase()})` }
    const d = decodeBytes(bytes, sn.encoding)
    text = d.text
    encoding = sn.encoding
    bom = d.bom
  } else {
    text = res.content ?? ''
    bom = text.charCodeAt(0) === 0xfeff
    if (bom) text = text.slice(1)
  }
  const scan = scanBreaks(text, 'lf')
  const meta: TextMeta = { encoding, bom, eol: scan.main, mixedEol: scan.mixed }
  text = text.replace(/\r\n?/g, '\n')
  return { status: 'ready', text, savedText: text, breaks: scan.marks, mtime: res.mtime, meta }
}

export default function DiffTab({ tabId, params }: TabProps<DiffTabParams>) {
  const settings = editorSettings.use()
  const [a, setA] = useState<SideState>(EMPTY)
  const [b, setB] = useState<SideState>(EMPTY)
  const [dirty, setDirty] = useState({ a: false, b: false })
  const [chunks, setChunks] = useState(-1)
  const [generation, setGeneration] = useState(0)
  const [saving, setSaving] = useState<Side | null>(null)
  const paneRef = useRef<MergePaneHandle>(null)
  const dirtyFrame = useRef(0)
  const unified = !!params.unified
  const ignoreWs: boolean | 'all' = params.ignoreWhitespace === 'all' ? 'all' : !!params.ignoreWhitespace
  const collapse = params.collapse !== false
  const zoom = params.zoom ?? 0
  const live = useLatest({ a, b, dirty, params, zoom })

  const setSide = (side: Side, v: SideState | ((s: SideState) => SideState)) => (side === 'a' ? setA(v) : setB(v))

  /** Latest load per side: an older, slower load must not overwrite a newer one (or rebuild the pane again). */
  const loadTokens = useRef({ a: 0, b: 0 })

  const load = useCallback(async (side: Side, src: DiffSource | undefined) => {
    const token = ++loadTokens.current[side]
    // Keep the other side's unsaved edits: the pane goes away while this side loads and is rebuilt afterwards.
    const other: Side = side === 'a' ? 'b' : 'a'
    const pane = paneRef.current
    if (pane) {
      const t = pane.getText(other)
      const br = pane.getBreaks(other)
      setSide(other, (s) => (s.status === 'ready' ? { ...s, text: t, breaks: br } : s))
    }
    if (!src) {
      setSide(side, EMPTY)
      return
    }
    setSide(side, { ...EMPTY, status: 'loading' })
    let next: SideState
    try {
      next = await withHandle(side, src, token, (s) => loadSource(s))
    } catch (err) {
      next = { ...EMPTY, status: 'error', error: errorMessage(err) }
    }
    if (token !== loadTokens.current[side]) return
    setSide(side, next)
    setDirty((d) => ({ ...d, [side]: false }))
    setGeneration((g) => g + 1)
  }, [])

  /**
   * Run `fn` on a side's source; when its file system handle expired (idle timeout, server restart) and the source
   * knows how to re-open it, re-open it once, point the side at the new handle and retry.
   */
  const withHandle = async <T,>(side: Side, src: DiffSource, token: number | null, fn: (s: DiffSource) => Promise<T>): Promise<T> => {
    try {
      return await fn(src)
    } catch (err) {
      if (src.kind !== 'remote' || !src.source || !isHandleGone(err)) throw err
      const h = await openFs(src.source)
      rememberHandle(h)
      const fresh: DiffSource = { ...src, fsId: h.id }
      if (token === null || token === loadTokens.current[side]) {
        // the new params must not trigger another load of this side
        skipLoad.current = { ...skipLoad.current, [side]: JSON.stringify(fresh) }
        setSource(side, fresh)
      }
      return fn(fresh)
    }
  }

  const leftKey = JSON.stringify(params.left ?? null)
  const rightKey = JSON.stringify(params.right ?? null)
  /** Source keys a swap produced: their contents are already loaded (swapped in memory). */
  const skipLoad = useRef<{ a?: string; b?: string }>({})
  useEffect(() => {
    if (skipLoad.current.a === leftKey) {
      skipLoad.current.a = undefined
      return
    }
    void load('a', params.left)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [leftKey])
  useEffect(() => {
    if (skipLoad.current.b === rightKey) {
      skipLoad.current.b = undefined
      return
    }
    void load('b', params.right)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rightKey])

  useEffect(() => {
    setTabTitle(tabId, diffTitle(params))
    applyDirtyTitle(tabId, dirty.a || dirty.b)
  }, [tabId, leftKey, rightKey, dirty.a, dirty.b, params])

  useEffect(() => {
    setTabDirty(tabId, dirty.a || dirty.b)
  }, [tabId, dirty.a, dirty.b])

  useEffect(
    () => () => {
      setTabDirty(tabId, false)
      if (dirtyFrame.current) cancelAnimationFrame(dirtyFrame.current)
    },
    [tabId],
  )

  const onPaneChange = () => {
    if (dirtyFrame.current) return
    dirtyFrame.current = requestAnimationFrame(() => {
      dirtyFrame.current = 0
      const p = paneRef.current
      if (!p) return
      const L = live.current
      const next = { a: L.a.status === 'ready' && p.isModified('a', L.a.savedText), b: L.b.status === 'ready' && p.isModified('b', L.b.savedText) }
      setDirty((d) => (d.a === next.a && d.b === next.b ? d : next))
    })
  }

  /** Current texts from the pane into state (before the pane is rebuilt). */
  const captureTexts = () => {
    const p = paneRef.current
    if (!p) return
    const ta = p.getText('a')
    const tb = p.getText('b')
    const ba = p.getBreaks('a')
    const bb = p.getBreaks('b')
    setA((s) => ({ ...s, text: ta, breaks: ba }))
    setB((s) => ({ ...s, text: tb, breaks: bb }))
  }

  const setSource = (side: Side, src: DiffSource | undefined) => {
    updateTabParams<DiffTabParams>(tabId, side === 'a' ? { left: src } : { right: src })
  }

  const swap = () => {
    const L = live.current
    // Loads still running belong to the old sides; a side that was not loaded yet is loaded again after the swap.
    loadTokens.current.a++
    loadTokens.current.b++
    skipLoad.current = {
      a: L.b.status === 'ready' ? JSON.stringify(L.params.right ?? null) : undefined,
      b: L.a.status === 'ready' ? JSON.stringify(L.params.left ?? null) : undefined,
    }
    updateTabParams<DiffTabParams>(tabId, { left: L.params.right, right: L.params.left })
    // Keep the loaded (and edited) contents: swap the states instead of reloading.
    const p = paneRef.current
    setA(L.b.status === 'ready' ? { ...L.b, text: p?.getText('b') ?? L.b.text, breaks: p?.getBreaks('b') ?? L.b.breaks } : L.b)
    setB(L.a.status === 'ready' ? { ...L.a, text: p?.getText('a') ?? L.a.text, breaks: p?.getBreaks('a') ?? L.a.breaks } : L.a)
    setDirty((d) => ({ a: d.b, b: d.a }))
    setGeneration((g) => g + 1)
  }

  const pickSource = async (side: Side, how: 'paste' | 'local' | 'remote' | 'clipboard' | { tab: TabInfo }) => {
    try {
      if (how === 'paste') {
        const text = await prompt({ title: `Paste text (${side === 'a' ? 'left' : 'right'} side)`, multiline: true, confirmLabel: 'Compare', selectOnOpen: false })
        if (text === null) return
        setSource(side, toDiffSource({ content: text, title: 'Pasted text' }))
      } else if (how === 'clipboard') {
        const text = await navigator.clipboard.readText()
        setSource(side, toDiffSource({ content: text, title: 'Clipboard' }))
      } else if (how === 'local') {
        const picked = await pickLocalFile()
        if (!picked) return
        const bytes = new Uint8Array(await picked.file.arrayBuffer())
        const sn = sniffBytes(bytes)
        if (sn.kind === 'binary') {
          toast.error(`${picked.file.name} is a binary file`, { description: sn.reason })
          return
        }
        setSource(side, toDiffSource({ content: decodeBytes(bytes, sn.encoding).text, title: picked.file.name }))
      } else if (how === 'remote') {
        const f = await pickRemoteFile({ title: `Compare: ${side === 'a' ? 'left' : 'right'} file` })
        if (f) setSource(side, { kind: 'remote', fsId: f.fsId, path: f.path, label: f.label, source: f.source })
      } else {
        const text = getController(how.tab.id)?.getText?.()
        if (text === undefined) {
          const p = how.tab.params as { fsId?: string; path?: string; label?: string; source?: FsOpenRequest } | undefined
          if (p?.fsId && p.path) setSource(side, { kind: 'remote', fsId: p.fsId, path: p.path, label: p.label, source: p.source })
          return
        }
        setSource(side, toDiffSource({ content: text, title: `${how.tab.title.replace(/^● /, '')} (editor)` }))
      }
    } catch (err) {
      toast.error('Could not load the text', { description: errorMessage(err) })
    }
  }

  const saveSide = async (side: Side, opts: { overwrite?: boolean } = {}): Promise<boolean> => {
    const L = live.current
    const src = side === 'a' ? L.params.left : L.params.right
    const st = side === 'a' ? L.a : L.b
    const p = paneRef.current
    if (!src || !p || st.status !== 'ready') return false
    const text = p.getText(side)
    if (src.kind === 'text') {
      await putDoc({ id: src.docId, content: text })
      setSide(side, (s) => ({ ...s, text, savedText: text }))
      setDirty((d) => ({ ...d, [side]: false }))
      return true
    }
    const meta = st.meta ?? { encoding: 'utf-8', bom: false, eol: 'lf', mixedEol: false }
    setSaving(side)
    try {
      const body = writeBody(p.getTextForSave(side, meta), meta)
      const breaks = p.getBreaks(side)
      const entry = await withHandle(side, src, null, (s) =>
        writeFile((s as Extract<DiffSource, { kind: 'remote' }>).fsId, { path: src.path, content: body.content, encoding: body.encoding, expectMtime: opts.overwrite ? undefined : st.mtime }),
      )
      setSide(side, (s) => ({ ...s, text, savedText: text, breaks, mtime: entry?.mtime ?? s.mtime }))
      setDirty((d) => ({ ...d, [side]: false }))
      toast.success(`Saved ${baseName(src.path)}`)
      return true
    } catch (err) {
      if (isConflict(err)) {
        const r = await choose({
          title: `${baseName(src.path)} was changed on the server`,
          description: 'The file was modified after it was loaded into this comparison.',
          tone: 'warning',
          choices: [
            { value: 'cancel', label: 'Cancel' },
            { value: 'reload', label: 'Reload that side' },
            { value: 'overwrite', label: 'Overwrite', variant: 'destructive' },
          ],
          cancel: 'cancel',
        })
        if (r === 'overwrite') {
          setSaving(null)
          return saveSide(side, { overwrite: true })
        }
        if (r === 'reload') void load(side, src)
        return false
      }
      toast.error(`Could not save ${baseName(src.path)}`, { description: errorMessage(err) })
      return false
    } finally {
      setSaving(null)
    }
  }

  const saveSideAs = async (side: Side) => {
    const L = live.current
    const src = side === 'a' ? L.params.left : L.params.right
    const p = paneRef.current
    if (!p) return
    const r = await saveAsDialog({ suggestedName: src ? sourceLabel(src) : 'compare.txt', allowLocal: true })
    if (!r) return
    let meta: TextMeta = (side === 'a' ? L.a.meta : L.b.meta) ?? { encoding: 'utf-8', bom: false, eol: 'lf', mixedEol: false }
    const text = p.getTextForSave(side, meta)
    try {
      textBytes(text, meta)
    } catch (err) {
      toast.error('Saved as UTF-8', { description: errorMessage(err) })
      meta = { ...meta, encoding: 'utf-8', bom: false }
    }
    try {
      if (r.target === 'remote') {
        const body = writeBody(text, meta)
        await writeFile(r.fsId, { path: r.path, content: body.content, encoding: body.encoding })
        setSource(side, { kind: 'remote', fsId: r.fsId, path: r.path, label: r.label, source: r.source })
      } else if (r.target === 'download') downloadBlob(textBytes(text, meta) as unknown as BlobPart, r.filename)
      else await writeLocalHandle(r.handle, textBytes(text, meta))
      toast.success('Saved')
    } catch (err) {
      toast.error('Could not save', { description: errorMessage(err) })
    }
  }

  /** Unsaved sides right now (the rendered flags lag a frame behind typing). */
  const dirtyNow = (): { a: boolean; b: boolean } => {
    const L = live.current
    const p = paneRef.current
    if (!p) return L.dirty
    return {
      a: L.a.status === 'ready' && p.isModified('a', L.a.savedText),
      b: L.b.status === 'ready' && p.isModified('b', L.b.savedText),
    }
  }

  const save = async (): Promise<boolean> => {
    const focused = paneRef.current?.focusedSide() ?? 'b'
    const order: Side[] = focused === 'a' ? ['a', 'b'] : ['b', 'a']
    const d = dirtyNow()
    const targets = order.filter((s) => d[s])
    if (!targets.length) return true
    for (const s of targets) if (!(await saveSide(s))) return false
    return true
  }

  const setZoom = (delta: number) => {
    const base = editorSettings.get().fontSize
    const next = delta === 0 ? 0 : Math.min(FONT_MAX - base, Math.max(FONT_MIN - base, live.current.zoom + delta))
    updateTabParams<DiffTabParams>(tabId, { zoom: next || undefined })
  }

  const actions = useLatest({ save, setZoom, dirtyNow })

  useEffect(
    () =>
      registerController({
        tabId,
        kind: 'diff',
        isDirty: () => {
          const d = actions.current.dirtyNow()
          return d.a || d.b
        },
        save: () => actions.current.save(),
        find: () => paneRef.current?.openSearch(),
        gotoLine: () => paneRef.current?.gotoLine(),
        nextChange: (dir) => paneRef.current?.nextChange(dir),
        zoom: (d) => actions.current.setZoom(d),
        focus: () => paneRef.current?.focus(),
      }),
    [actions, live, tabId],
  )

  const ready = a.status === 'ready' && b.status === 'ready'
  const tabs = listTabs().filter((t) => EDITOR_KINDS.has(t.kind) && t.kind !== 'diff' && t.id !== tabId)

  const sourceMenu = (side: Side) => {
    const src = side === 'a' ? params.left : params.right
    const st = side === 'a' ? a : b
    return (
      <DropdownMenu>
        <Tooltip content={sourceDetail(src) || 'Choose what to compare'}>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="sm" className="h-7 max-w-64 min-w-0 gap-1.5 px-2 font-normal">
              {src?.kind === 'remote' ? <Server className="size-3.5 shrink-0" /> : <FileText className="size-3.5 shrink-0" />}
              <span className="truncate">{sourceLabel(src)}</span>
              {(side === 'a' ? dirty.a : dirty.b) && <span className="text-warning" aria-label="modified">●</span>}
              <Spinner active={st.status === 'loading'} className="size-3" />
              <ChevronDown className="size-3 shrink-0 opacity-60" />
            </Button>
          </DropdownMenuTrigger>
        </Tooltip>
        <DropdownMenuContent align="start" className="min-w-56">
          <DropdownMenuLabel>{side === 'a' ? 'Left side' : 'Right side'}</DropdownMenuLabel>
          <DropdownMenuItem onSelect={() => void pickSource(side, 'remote')}>
            <FolderOpen /> Remote file…
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => void pickSource(side, 'local')}>
            <FileUp /> File on this computer…
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => void pickSource(side, 'paste')}>
            <Type /> Paste text…
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => void pickSource(side, 'clipboard')}>
            <ClipboardPaste /> Clipboard
          </DropdownMenuItem>
          {tabs.length > 0 && <DropdownMenuSeparator />}
          {tabs.length > 0 && <DropdownMenuLabel>Open editor tabs</DropdownMenuLabel>}
          {tabs.slice(0, 12).map((t) => (
            <DropdownMenuItem key={t.id} onSelect={() => void pickSource(side, { tab: t })}>
              <FileText /> <span className="truncate">{t.title}</span>
            </DropdownMenuItem>
          ))}
          {src && (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem onSelect={() => void load(side, src)}>
                <RefreshCw /> Reload
              </DropdownMenuItem>
              <DropdownMenuItem disabled={st.status !== 'ready'} onSelect={() => void saveSide(side)}>
                <Save /> Save
              </DropdownMenuItem>
              <DropdownMenuItem disabled={st.status !== 'ready'} onSelect={() => void saveSideAs(side)}>
                <Save /> Save as…
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
    )
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div role="toolbar" aria-label="Compare" className="flex min-h-9 shrink-0 flex-wrap items-center gap-1 border-b bg-toolbar px-1.5 py-1">
        {sourceMenu('a')}
        <IconButton icon={ArrowLeftRight} label="Swap sides" size="sm" className="size-7" onClick={swap} disabled={!params.left && !params.right} />
        {sourceMenu('b')}
        <div className="mx-1 h-4 w-px bg-border" aria-hidden />
        <IconButton icon={ChevronUp} label="Previous change" shortcut="Shift+F7" size="sm" className="size-7" disabled={!ready || chunks <= 0} onClick={() => paneRef.current?.nextChange(-1)} />
        <IconButton icon={ChevronDown} label="Next change" shortcut="F7" size="sm" className="size-7" disabled={!ready || chunks <= 0} onClick={() => paneRef.current?.nextChange(1)} />
        <span className="min-w-20 px-1 text-xs text-muted-foreground tabular" aria-live="polite">
          {ready ? (chunks < 0 ? 'Comparing…' : chunks ? `${chunks} change${chunks === 1 ? '' : 's'}` : 'Identical') : ''}
        </span>
        <div className="flex-1" />
        {!unified && (
          <IconButton
            icon={MoveLeft}
            label="Copy the change at the cursor to the left"
            size="sm"
            className="size-7"
            disabled={!ready || chunks <= 0}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => {
              if (!paneRef.current?.copyChunk('toA')) toast.info('Place the cursor in a change first', { description: 'The left side may also be read-only.' })
            }}
          />
        )}
        <IconButton
          icon={MoveRight}
          label={unified ? 'Revert the change at the cursor' : 'Copy the change at the cursor to the right (or use the arrows in the margin)'}
          size="sm"
          className="size-7"
          disabled={!ready || chunks <= 0}
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => {
            if (!paneRef.current?.copyChunk('toB')) toast.info('Place the cursor in a change first')
          }}
        />
        <DropdownMenu>
          <Tooltip content="How whitespace differences are compared">
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="sm" className="hidden h-7 gap-1.5 px-2 text-xs font-normal text-muted-foreground @md:inline-flex">
                <Pilcrow className="size-3.5" />
                {ignoreWs === 'all' ? 'Whitespace ignored' : ignoreWs ? 'Line-end whitespace ignored' : 'Whitespace compared'}
                <ChevronDown className="size-3 opacity-60" />
              </Button>
            </DropdownMenuTrigger>
          </Tooltip>
          <DropdownMenuContent align="end" className="min-w-60">
            <DropdownMenuLabel>Whitespace</DropdownMenuLabel>
            <DropdownMenuRadioGroup
              value={ignoreWs === 'all' ? 'all' : ignoreWs ? 'trim' : 'none'}
              onValueChange={(v) => {
                captureTexts()
                updateTabParams<DiffTabParams>(tabId, { ignoreWhitespace: v === 'all' ? 'all' : v === 'trim' ? true : undefined })
              }}
            >
              <DropdownMenuRadioItem value="none">Compare all whitespace</DropdownMenuRadioItem>
              <DropdownMenuRadioItem value="trim">Ignore leading and trailing whitespace</DropdownMenuRadioItem>
              <DropdownMenuRadioItem value="all">Ignore all whitespace</DropdownMenuRadioItem>
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
        <label className="hidden items-center gap-1.5 px-1.5 text-xs text-muted-foreground @lg:flex">
          <Switch
            size="sm"
            checked={collapse}
            onCheckedChange={(v) => {
              captureTexts()
              updateTabParams<DiffTabParams>(tabId, { collapse: v ? undefined : false })
            }}
          />
          Collapse unchanged
        </label>
        <div className="flex items-center rounded-md border bg-muted/60 p-0.5" role="group" aria-label="Layout">
          <IconButton
            icon={Columns2}
            label="Side by side"
            size="xs"
            active={!unified}
            onClick={() => {
              captureTexts()
              updateTabParams<DiffTabParams>(tabId, { unified: undefined })
            }}
          />
          <IconButton
            icon={Rows2}
            label="Inline (unified)"
            size="xs"
            active={unified}
            onClick={() => {
              captureTexts()
              updateTabParams<DiffTabParams>(tabId, { unified: true })
            }}
          />
        </div>
        {(dirty.a || dirty.b) && (
          <Button size="sm" className="ml-1 h-7" loading={!!saving} onClick={() => void save()}>
            {!saving && <Save className="size-3.5" />} Save
          </Button>
        )}
      </div>
      {ready ? (
        <MergePane
          key={`${generation}`}
          ref={paneRef}
          tabId={tabId}
          a={{ text: a.text, breaks: a.breaks, label: <SideLabel src={params.left} mtime={a.mtime} /> }}
          b={{ text: b.text, breaks: b.breaks, label: <SideLabel src={params.right} mtime={b.mtime} /> }}
          path={params.right?.kind === 'remote' ? params.right.path : params.left?.kind === 'remote' ? params.left.path : sourceLabel(params.right)}
          unified={unified}
          ignoreWhitespace={ignoreWs}
          collapse={collapse}
          revert="a-to-b"
          zoom={zoom}
          wrap={settings.wordWrap}
          onChange={onPaneChange}
          onChunks={setChunks}
        />
      ) : (
        <div className="grid min-h-0 flex-1 grid-cols-1 content-start gap-3 overflow-auto p-4 @xl:grid-cols-2">
          {(['a', 'b'] as Side[]).map((side) => (
            <SidePlaceholder
              key={side}
              side={side}
              state={side === 'a' ? a : b}
              src={side === 'a' ? params.left : params.right}
              onPick={(how) => void pickSource(side, how)}
              onRetry={() => void load(side, side === 'a' ? params.left : params.right)}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function lineCount(text: string): number {
  let n = 1
  for (let i = text.indexOf('\n'); i >= 0; i = text.indexOf('\n', i + 1)) n++
  return n
}

function SideLabel({ src, mtime }: { src: DiffSource | undefined; mtime?: string }) {
  if (!src) return null
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5" title={sourceDetail(src)}>
      {src.kind === 'remote' ? <Server className="size-3 shrink-0" /> : <FileText className="size-3 shrink-0" />}
      <span className={cn('truncate', src.kind === 'remote' && 'font-mono')}>{src.kind === 'remote' ? sourceDetail(src) : src.title}</span>
      {mtime && <span className="shrink-0 opacity-70">· {formatDateTime(mtime)}</span>}
    </span>
  )
}

function SidePlaceholder({
  side,
  state,
  src,
  onPick,
  onRetry,
}: {
  side: Side
  state: SideState
  src: DiffSource | undefined
  onPick: (how: 'paste' | 'local' | 'remote' | 'clipboard') => void
  onRetry: () => void
}) {
  const label = side === 'a' ? 'Left (original)' : 'Right (modified)'
  return (
    <section className={cn('flex min-h-48 flex-col gap-3 rounded-lg border bg-card p-4', state.status === 'error' && 'border-destructive/50')} aria-label={label}>
      <div className="text-sm font-medium text-muted-foreground">{label}</div>
      <LoadingState busy={state.status === 'loading'} label={`Loading ${sourceLabel(src)}…`} className="min-h-0 flex-1">
        {state.status === 'ready' ? (
          <div className="flex flex-1 flex-col justify-center gap-1">
            <div className="truncate font-medium">{sourceLabel(src)}</div>
            <div className="truncate font-mono text-xs text-muted-foreground">{sourceDetail(src)}</div>
            <div className="text-xs text-muted-foreground">{lineCount(state.text).toLocaleString()} lines · ready</div>
          </div>
        ) : (
          <>
            {state.status === 'error' && (
              <div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm" role="alert">
                {state.error}
                <Button size="xs" variant="secondary" className="ml-2" onClick={onRetry}>
                  Retry
                </Button>
              </div>
            )}
            <div className="grid gap-2 @md:grid-cols-2">
              <Button variant="secondary" className="justify-start" onClick={() => onPick('remote')}>
                <FolderOpen /> Remote file…
              </Button>
              <Button variant="secondary" className="justify-start" onClick={() => onPick('local')}>
                <FileUp /> File on this computer…
              </Button>
              <Button variant="secondary" className="justify-start" onClick={() => onPick('paste')}>
                <Type /> Paste text…
              </Button>
              <Button variant="secondary" className="justify-start" onClick={() => onPick('clipboard')}>
                <ClipboardPaste /> From clipboard
              </Button>
            </div>
          </>
        )}
      </LoadingState>
    </section>
  )
}
