/*
 * Remote file search (FILE-17, "MobaFind"): name glob / substring, optionally containing text, below a folder;
 * cancellable; results reveal in the browser (double-click / Enter), open, download or copy their path.
 */
import { useEffect, useId, useRef, useState } from 'react'
import { Copy, Download, FolderSearch, Search, Square } from 'lucide-react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { Delayed, Spinner } from '@/components/ui/spinner'
import { cn, copyText, errorMessage, formatBytes } from '@/lib/utils'
import { openFile } from '../actions'
import { fsApi } from '../api'
import { getController } from '../browser/controller'
import { downloadEntries } from '../download'
import { formatMtime, isDirLike, isPartialUpload } from '../format'
import { withFs } from '../fsHandles'
import { FileIcon } from '../icons'
import { openFilesTab } from '../open'
import { dirname, isInside } from '../paths'
import { filesSettings } from '../settings'
import type { FsContext } from '../types'

interface State {
  status: 'idle' | 'running' | 'done' | 'error'
  entries: FileEntry[]
  truncated: boolean
  error?: string
  ms?: number
}

export default function SearchDialog({ ctx, path, viewId, onClose }: { ctx: FsContext; path: string; viewId?: string; onClose: () => void }) {
  const [pattern, setPattern] = useState('')
  const [content, setContent] = useState('')
  const [root, setRoot] = useState(path)
  const [max, setMax] = useState(500)
  const [st, setSt] = useState<State>({ status: 'idle', entries: [], truncated: false })
  const [active, setActive] = useState(-1)
  const abort = useRef<AbortController | null>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const id = useId()
  useEffect(() => () => abort.current?.abort(), [])

  const run = async () => {
    const pat = pattern.trim() || (content.trim() ? '*' : '')
    if (!pat) return
    abort.current?.abort()
    const ac = new AbortController()
    abort.current = ac
    const t0 = performance.now()
    setSt({ status: 'running', entries: [], truncated: false })
    setActive(-1)
    try {
      const r = await withFs(ctx.key, (fsId) => fsApi.search(fsId, root.trim() || path, pat, max, { content: content.trim() || undefined, signal: ac.signal }))
      if (ac.signal.aborted) return
      // Like the listings: partial uploads (".nexterm-part") only when the setting shows them.
      const entries = filesSettings.get().showPartialUploads ? r.entries : r.entries.filter((e) => !isPartialUpload(e))
      setSt({ status: 'done', entries, truncated: r.truncated, ms: performance.now() - t0 })
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') {
        setSt((s) => ({ ...s, status: 'done' }))
        return
      }
      setSt({ status: 'error', entries: [], truncated: false, error: errorMessage(err) })
    }
  }

  const stop = () => {
    abort.current?.abort()
    setSt((s) => ({ ...s, status: 'done' }))
  }

  const reveal = (e: FileEntry) => {
    const parent = dirname(e.path)
    const c = getController(viewId)
    onClose()
    if (c && c.ctx?.key === ctx.key) c.navigate(parent, { select: e.path })
    else openFilesTab(ctx.source, { path: parent })
  }

  const openEntry = (e: FileEntry) => {
    if (isDirLike(e)) {
      const c = getController(viewId)
      onClose()
      if (c && c.ctx?.key === ctx.key) c.navigate(e.path)
      else openFilesTab(ctx.source, { path: e.path })
      return
    }
    openFile(ctx, e, st.entries.filter((x) => dirname(x.path) === dirname(e.path)))
  }

  const base = root.trim() || path
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="xl" className="h-[min(40rem,85vh)]">
        <DialogHeader>
          <DialogTitle>
            <FolderSearch className="size-4 text-muted-foreground" /> Search files
          </DialogTitle>
          <DialogDescription>On {ctx.label}. Names match globs (*.log, report-??.pdf) or plain text.</DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-2 @container"
          onSubmit={(e) => {
            e.preventDefault()
            void run()
          }}
        >
          <div className="grid gap-2 sm:grid-cols-2">
            <div className="grid gap-1">
              <label htmlFor={`${id}-pat`} className="text-sm font-medium">
                Name
              </label>
              <Input id={`${id}-pat`} value={pattern} onChange={(e) => setPattern(e.target.value)} placeholder="*.conf" autoFocus spellCheck={false} className="font-mono" />
            </div>
            <div className="grid gap-1">
              <label htmlFor={`${id}-content`} className="text-sm font-medium">
                Containing text <span className="font-normal text-muted-foreground">(optional)</span>
              </label>
              <Input
                id={`${id}-content`}
                value={content}
                onChange={(e) => setContent(e.target.value)}
                placeholder="e.g. ListenAddress"
                spellCheck={false}
                disabled={!ctx.handle.capabilities.exec && ctx.handle.capabilities.search === false}
              />
            </div>
          </div>
          <div className="flex items-end gap-2">
            <div className="grid min-w-0 flex-1 gap-1">
              <label htmlFor={`${id}-root`} className="text-sm font-medium">
                In folder
              </label>
              <Input id={`${id}-root`} value={root} onChange={(e) => setRoot(e.target.value)} className="font-mono" spellCheck={false} />
            </div>
            <div className="grid gap-1">
              <label htmlFor={`${id}-max`} className="text-sm font-medium">
                Max results
              </label>
              <NumberInput id={`${id}-max`} value={max} onChange={(n) => setMax(n ?? 500)} min={10} max={5000} step={100} className="w-28" />
            </div>
            {st.status === 'running' ? (
              <Button type="button" variant="secondary" onClick={stop}>
                <Square /> Stop
              </Button>
            ) : (
              <Button type="submit" disabled={!pattern.trim() && !content.trim()}>
                <Search /> Search
              </Button>
            )}
          </div>
        </form>
        <div className="flex min-h-0 flex-1 flex-col rounded-md border">
          <div className="flex h-7 shrink-0 items-center gap-2 border-b px-2 text-xs text-muted-foreground" aria-live="polite">
            {st.status === 'running' && (
              <Delayed fallback={<span>Searching…</span>}>
                <Spinner className="size-3.5" /> Searching…
              </Delayed>
            )}
            {st.status === 'done' && (
              <span>
                {st.entries.length.toLocaleString()} result{st.entries.length === 1 ? '' : 's'}
                {st.truncated && ' (limit reached — refine the search)'}
                {st.ms != null && ` · ${(st.ms / 1000).toFixed(1)} s`}
              </span>
            )}
            {st.status === 'error' && <span className="text-destructive">{st.error}</span>}
            {st.status === 'idle' && <span>Enter a name pattern and press Search.</span>}
          </div>
          <div
            ref={listRef}
            role="listbox"
            aria-label="Search results"
            tabIndex={0}
            className="min-h-0 flex-1 overflow-y-auto outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') {
                e.preventDefault()
                setActive((a) => Math.min(st.entries.length - 1, a + 1))
              } else if (e.key === 'ArrowUp') {
                e.preventDefault()
                setActive((a) => Math.max(0, a - 1))
              } else if (e.key === 'Enter' && st.entries[active]) {
                e.preventDefault()
                reveal(st.entries[active])
              }
            }}
          >
            {st.entries.map((e, i) => {
              const rel = isInside(e.path, base) && base !== '/' ? e.path.slice(base.length).replace(/^\//, '') : e.path
              const folder = dirname(rel)
              return (
                <div
                  key={e.path}
                  role="option"
                  aria-selected={i === active}
                  onClick={() => setActive(i)}
                  onDoubleClick={() => reveal(e)}
                  className={cn('group flex h-7 cursor-default items-center gap-2 px-2 text-sm', i === active ? 'bg-primary/15' : 'hover:bg-accent/60')}
                  title={e.path}
                >
                  <FileIcon entry={e} />
                  <span className="shrink-0 truncate font-medium">{e.name}</span>
                  <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">{folder === '.' ? '' : folder}</span>
                  <span className="hidden w-16 shrink-0 text-right text-xs tabular text-muted-foreground sm:block">{isDirLike(e) ? '' : formatBytes(e.size)}</span>
                  <span className="hidden w-28 shrink-0 text-xs tabular text-muted-foreground md:block">{formatMtime(e.mtime)}</span>
                  <span className="flex shrink-0 opacity-0 group-hover:opacity-100 group-aria-selected:opacity-100">
                    <IconButton icon={FolderSearch} label="Show in folder" size="xs" onClick={() => reveal(e)} />
                    <IconButton icon={Download} label="Download" size="xs" onClick={() => downloadEntries(ctx, [e], dirname(e.path))} />
                    <IconButton
                      icon={Copy}
                      label="Copy path"
                      size="xs"
                      onClick={async () => {
                        if (await copyText(e.path)) toast.success('Path copied')
                      }}
                    />
                  </span>
                  <button type="button" className="sr-only" onClick={() => openEntry(e)}>
                    Open {e.name}
                  </button>
                </div>
              )
            })}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
