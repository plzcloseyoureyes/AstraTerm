/*
 * Path bar (breadcrumbs) of a remote file: host › folder › … › file. Every segment opens a quick-open list of that
 * folder — type to filter, ↑/↓ and Enter to open a file (or step into a folder), Backspace on an empty filter to go
 * up. The file's own segment lists its siblings ("Go to File in This Folder…", Ctrl/⌘+O).
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { ChevronRight, CornerLeftUp, File, FileCode2, Folder, FolderOpen, Server } from 'lucide-react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Spinner } from '@/components/ui/spinner'
import { DELAY_PRESETS } from '@/lib/useDelayedFlag'
import { cn, copyText, errorMessage } from '@/lib/utils'
import { listDir } from './api'
import { crumbs, dirName } from './paths'

export interface PathBarProps {
  fsId: string
  path: string
  host?: string
  /** Index of the segment whose list is open (crumbs(path) index; the file is the last one), null = none. */
  open: number | null
  onOpenChange: (index: number | null) => void
  onOpenFile: (path: string) => void
  /** Show a folder in the file browser (when the tab knows how). */
  onBrowse?: (dir: string) => void
  /** A list closed without opening anything: give the focus back (to the editor). */
  onReturnFocus?: () => void
}

export function PathBar({ fsId, path, host, open, onOpenChange, onOpenFile, onBrowse, onReturnFocus }: PathBarProps) {
  const segs = useMemo(() => crumbs(path), [path])
  /** Something was opened from the list: the new tab takes the focus. */
  const opened = useRef(false)
  return (
    <nav
      aria-label="File path"
      className="flex h-7 shrink-0 items-center gap-0.5 overflow-hidden border-b bg-panel px-2 text-xs text-muted-foreground select-none"
    >
      {host && (
        <span className="inline-flex shrink-0 items-center gap-1 pr-0.5 font-medium text-foreground/80">
          <Server className="size-3" aria-hidden />
          {host}
        </span>
      )}
      {segs.map((s, i) => {
        const last = i === segs.length - 1
        return (
          <span key={s.path} className={cn('inline-flex min-w-0 items-center', last ? 'shrink-0' : 'shrink')}>
            {(i > 0 || host) && <ChevronRight className="size-3 shrink-0 opacity-50" aria-hidden />}
            <Popover open={open === i} onOpenChange={(o) => onOpenChange(o ? i : null)}>
              <PopoverTrigger asChild>
                <button
                  type="button"
                  title={last ? `${s.path} — other files in this folder` : `${s.path} — open a file from this folder`}
                  className={cn(
                    'inline-flex h-5 min-w-[2.5ch] items-center gap-1 rounded-sm px-1 outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 data-[state=open]:bg-accent data-[state=open]:text-foreground',
                    last && 'font-medium text-foreground',
                  )}
                >
                  {last && <FileCode2 className="size-3 shrink-0" aria-hidden />}
                  <span className={cn('truncate', last && 'font-mono')}>{s.name}</span>
                </button>
              </PopoverTrigger>
              {/* PopoverContent portals into the panel's document: a dockview pop-out window shows it too. */}
              <PopoverContent
                align="start"
                className="w-80 p-0"
                onOpenAutoFocus={(e) => {
                  e.preventDefault()
                  opened.current = false
                }}
                onCloseAutoFocus={(e) => {
                  e.preventDefault()
                  if (!opened.current) onReturnFocus?.()
                }}
              >
                <FolderList
                  fsId={fsId}
                  dir={last ? dirName(path) : s.path}
                  current={path}
                  copyText={last ? (host ? `${host}:${path}` : path) : undefined}
                  onOpenFile={(p) => {
                    opened.current = true
                    onOpenFile(p)
                  }}
                  onBrowse={
                    onBrowse &&
                    ((d) => {
                      opened.current = true
                      onBrowse(d)
                    })
                  }
                  onClose={() => onOpenChange(null)}
                />
              </PopoverContent>
            </Popover>
          </span>
        )
      })}
    </nav>
  )
}

type ListState = {
  dir: string
  entries: FileEntry[] | null
  error: string | null
  loading: boolean
}

/** The folder above `dir` (null at a root: "/", "C:/"). */
function parentDir(dir: string): string | null {
  if (!dir || dir === '/' || dir === '.' || /^[A-Za-z]:\/?$/.test(dir)) return null
  const p = dirName(dir)
  if (p === '.' || p === dir) return null
  return /^[A-Za-z]:$/.test(p) ? `${p}/` : p
}

function isDir(e: FileEntry): boolean {
  return e.type === 'dir' || (e.type === 'symlink' && e.linkType === 'dir')
}

function sortEntries(list: FileEntry[]): FileEntry[] {
  return [...list].sort(
    (a, b) =>
      Number(isDir(b)) - Number(isDir(a)) ||
      a.name.localeCompare(b.name, undefined, {
        numeric: true,
        sensitivity: 'base',
      }),
  )
}

/** Filterable, keyboard-driven listing of a folder. */
function FolderList({
  fsId,
  dir: initialDir,
  current,
  onOpenFile,
  onBrowse,
  onClose,
  copyText: pathText,
}: {
  fsId: string
  dir: string
  current: string
  onOpenFile: (path: string) => void
  onBrowse?: (dir: string) => void
  onClose: () => void
  /** Offer "Copy path" (the file's own list). */
  copyText?: string
}) {
  const [state, setState] = useState<ListState>({
    dir: initialDir,
    entries: null,
    error: null,
    loading: true,
  })
  const [filter, setFilter] = useState('')
  const [active, setActive] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const dir = state.dir

  useEffect(() => {
    const ctl = new AbortController()
    setState((s) => ({ ...s, loading: true, error: null }))
    listDir(fsId, dir, ctl.signal).then(
      (r) => {
        const entries = sortEntries(r.entries.filter((e) => e.name !== '.' && e.name !== '..' && e.name !== ''))
        setState({ dir, entries, error: null, loading: false })
        // start on the file being edited
        setActive(
          Math.max(
            0,
            entries.findIndex((e) => e.path === current),
          ),
        )
      },
      (err: unknown) => {
        if (!ctl.signal.aborted)
          setState({
            dir,
            entries: [],
            error: errorMessage(err),
            loading: false,
          })
      },
    )
    return () => ctl.abort()
  }, [fsId, dir, current])

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const shown = useMemo(() => {
    const all = state.entries ?? []
    const q = filter.trim().toLowerCase()
    if (!q) return all
    // prefix matches first, then substring matches
    const pre = all.filter((e) => e.name.toLowerCase().startsWith(q))
    const sub = all.filter((e) => !e.name.toLowerCase().startsWith(q) && e.name.toLowerCase().includes(q))
    return [...pre, ...sub]
  }, [state.entries, filter])

  useEffect(() => {
    listRef.current?.querySelector('[data-active]')?.scrollIntoView({ block: 'nearest' })
  }, [active, shown])

  const up = parentDir(dir)
  const go = (d: string) => {
    setFilter('')
    // The current listing stays until the next one is there (no blank list in between).
    setState((st) => ({ ...st, dir: d, error: null, loading: true }))
  }
  const choose = (e: FileEntry) => {
    if (isDir(e)) go(e.path)
    else {
      onClose()
      onOpenFile(e.path)
    }
  }

  const onKeyDown = (ev: KeyboardEvent<HTMLInputElement>) => {
    if (ev.key === 'ArrowDown') {
      ev.preventDefault()
      setActive((a) => Math.min(shown.length - 1, a + 1))
    } else if (ev.key === 'ArrowUp') {
      ev.preventDefault()
      setActive((a) => Math.max(0, a - 1))
    } else if (ev.key === 'Enter') {
      ev.preventDefault()
      const e = shown[Math.min(active, shown.length - 1)]
      if (e) choose(e)
    } else if (ev.key === 'Backspace' && !filter && up) {
      ev.preventDefault()
      go(up)
    }
  }

  return (
    <div className="flex max-h-[min(60vh,420px)] flex-col">
      <div className="flex items-center gap-1.5 border-b px-2 py-1.5">
        <FolderOpen className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground" title={dir}>
          {dir}
        </span>
        <Spinner active={state.loading} {...DELAY_PRESETS.NAVIGATION} className="size-3" label={`Loading ${dir}`} />
      </div>
      <input
        ref={inputRef}
        value={filter}
        onChange={(e) => {
          setFilter(e.target.value)
          setActive(0)
        }}
        onKeyDown={onKeyDown}
        placeholder="Filter files…"
        aria-label="Filter files"
        aria-controls="nx-pathbar-list"
        aria-activedescendant={shown[active] ? `nx-pb-${active}` : undefined}
        className="h-8 w-full border-b bg-transparent px-2.5 text-sm outline-none placeholder:text-muted-foreground"
      />
      <div ref={listRef} id="nx-pathbar-list" role="listbox" aria-label={`Files in ${dir}`} className="min-h-10 flex-1 overflow-y-auto p-1">
        {up && !filter && (
          <button
            type="button"
            tabIndex={-1}
            className="flex h-7 w-full items-center gap-2 rounded-sm px-2 text-left text-sm text-muted-foreground hover:bg-accent"
            onClick={() => go(up)}
          >
            <CornerLeftUp className="size-3.5" aria-hidden /> ..
          </button>
        )}
        {state.error && <div className="px-2 py-2 text-sm text-destructive">{state.error}</div>}
        {!state.loading && !state.error && shown.length === 0 && (
          <div className="px-2 py-2 text-sm text-muted-foreground">{filter ? 'No matching files' : 'This folder is empty'}</div>
        )}
        {shown.map((e, i) => {
          const folder = isDir(e)
          return (
            <div
              key={e.path}
              id={`nx-pb-${i}`}
              role="option"
              aria-selected={i === active}
              data-active={i === active ? '' : undefined}
              className={cn(
                'flex h-7 cursor-default items-center gap-2 rounded-sm px-2 text-sm',
                i === active ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/60',
                e.path === current && 'font-medium',
                e.hidden && 'text-muted-foreground',
              )}
              onMouseMove={() => setActive(i)}
              onMouseDown={(ev) => ev.preventDefault()}
              onClick={() => choose(e)}
            >
              {folder ? (
                <Folder className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
              ) : (
                <File className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
              )}
              <span className="min-w-0 flex-1 truncate">{e.name}</span>
              {e.path === current && <span className="shrink-0 text-xs text-muted-foreground">open</span>}
            </div>
          )
        })}
      </div>
      <div className="flex items-center gap-2 border-t px-2.5 py-1 text-[11px] text-muted-foreground">
        <span>↵ open · ⌫ up</span>
        <div className="flex-1" />
        {pathText && (
          <FooterLink
            onClick={() => {
              onClose()
              void copyText(pathText).then((ok) => ok && toast.success('Path copied'))
            }}
          >
            Copy path
          </FooterLink>
        )}
        {onBrowse && (
          <FooterLink
            onClick={() => {
              onClose()
              onBrowse(dir)
            }}
          >
            Show in file browser
          </FooterLink>
        )}
      </div>
    </div>
  )
}

function FooterLink({ onClick, children }: { onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" className="rounded-sm px-1 outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50" onClick={onClick}>
      {children}
    </button>
  )
}
