/*
 * File system + path picker for "Open remote file…", "Save as…" and "Find in files…" (GET /api/fs/{id}/list,
 * POST /api/fs/{id}/search). Lists the file system handles already open in the workspace and can open new ones
 * (server-local files, a saved SSH/SFTP/FTP/S3 connection, a running SSH session). In save mode it also offers
 * "this computer" targets (download / File System Access); in find mode it searches the folder shown for files
 * containing a text (case-insensitive) and returns the hit to open.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { ArrowUp, CornerDownLeft, Download, File, Folder, FolderSymlink, HardDrive, House, Laptop, RefreshCw, Server } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { listConnections } from '@/api/connections'
import { queryKeys } from '@/api/queryKeys'
import { listSessions } from '@/api/sessions'
import type { FileEntry, FsListResult, FsOpenRequest } from '@/api/types'
import { suspendKeybindings } from '@/app/keybindings'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { DELAY_PRESETS, useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage, formatBytes, formatDateTime } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { listTabs } from '@/stores/workspace'
import { closeFs, handleLabel, infoLabel, listDir, listFsHandles, openFs, rememberHandle, searchFiles, sourceOf } from './api'
import { baseName, joinPath } from './codec'
import type { PickerOptions, SaveAsResult } from './dialogs'
import { canPickSaveFile, pickSaveFile } from './save'

interface FsChoice {
  id: string
  label: string
  source?: FsOpenRequest
  /** Opened by this picker (closed again unless returned). */
  owned?: boolean
}

const FILE_PROTOCOLS = new Set(['ssh', 'sftp', 'ftp', 's3'])

/** Last folder shown per handle (this page's lifetime): the picker reopens there. */
const lastDirs = new Map<string, string>()
let lastFsId: string | null = null

function knownHandles(preferred?: string, preferredLabel?: string): FsChoice[] {
  const out = new Map<string, FsChoice>()
  if (preferred) out.set(preferred, { id: preferred, label: handleLabel(preferred, preferredLabel) ?? 'Current location' })
  for (const t of listTabs()) {
    const p = t.params as { fsId?: unknown; label?: unknown; source?: FsOpenRequest } | undefined
    if (typeof p?.fsId !== 'string' || out.has(p.fsId)) continue
    out.set(p.fsId, { id: p.fsId, label: handleLabel(p.fsId, typeof p.label === 'string' ? p.label : undefined) ?? `${t.title}`, source: p.source })
  }
  return Array.from(out.values())
}

interface SearchResults {
  dir: string
  text: string
  entries: FileEntry[]
  truncated: boolean
}

const sameYear = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
const otherYear = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' })

/** "Sep 27, 20:02" (this year) or "Sep 27, 2025": compact, for the list column. */
function shortDate(value: string | undefined): string {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  return (d.getFullYear() === new Date().getFullYear() ? sameYear : otherYear).format(d)
}

function relativeTo(dir: string, p: string): string {
  const base = dir.endsWith('/') ? dir : `${dir}/`
  return p.startsWith(base) ? p.slice(base.length) : p
}

export default function RemotePickerDialog({ opts, onDone }: { opts: PickerOptions; onDone: (r: SaveAsResult | null) => void }) {
  const saveMode = opts.mode === 'save'
  const findMode = opts.mode === 'find'
  const [query, setQuery] = useState(opts.query ?? '')
  const [glob, setGlob] = useState('')
  const [results, setResults] = useState<SearchResults | null>(null)
  const [searching, setSearching] = useState(false)
  const searchCtl = useRef<AbortController | null>(null)
  const [choices, setChoices] = useState<FsChoice[]>(() => knownHandles(opts.fsId, opts.label))
  const [fsId, setFsId] = useState<string | null>(() => opts.fsId ?? lastFsId ?? knownHandles().at(0)?.id ?? null)
  const [path, setPath] = useState(() => opts.dir ?? (fsId ? lastDirs.get(fsId) : undefined) ?? '')
  const [pathInput, setPathInput] = useState(path)
  const [listing, setListing] = useState<FsListResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [active, setActive] = useState(0)
  const [name, setName] = useState(opts.suggestedName ?? '')
  const [connecting, setConnecting] = useState(false)
  const [refresh, setRefresh] = useState(0)
  const done = useRef(false)
  const ownedRef = useRef(new Set<string>())
  const listRef = useRef<HTMLDivElement>(null)
  const nameRef = useRef<HTMLInputElement>(null)
  const isAdmin = useAuthStore((s) => s.user?.role === 'admin')
  const mode = useAuthStore((s) => s.state?.mode)

  useEffect(() => suspendKeybindings(), [])
  useEffect(() => () => searchCtl.current?.abort(), [])

  const handles = useQuery({ queryKey: ['editor', 'fs-handles'], queryFn: listFsHandles, staleTime: 5_000 })
  const conns = useQuery({ queryKey: queryKeys.connections, queryFn: listConnections, staleTime: 30_000 })
  const sessions = useQuery({ queryKey: queryKeys.sessions, queryFn: () => listSessions(), staleTime: 10_000 })
  const fileConns = useMemo(() => (conns.data ?? []).filter((c) => FILE_PROTOCOLS.has(c.protocol)).slice(0, 100), [conns.data])
  const sshSessions = useMemo(
    () => (sessions.data ?? []).filter((s) => (s.protocol === 'ssh' || s.protocol === 'sftp') && s.state === 'connected'),
    [sessions.data],
  )

  // Every handle the user has open on the server (file browsers, other editors); expired ones disappear.
  useEffect(() => {
    const list = handles.data
    if (!Array.isArray(list)) return
    const live = new Set(list.map((h) => h.id))
    setChoices((cur) => {
      const byId = new Map(cur.filter((c) => live.has(c.id) || c.owned).map((c) => [c.id, c]))
      for (const h of list) {
        rememberHandle(h)
        const prev = byId.get(h.id)
        byId.set(h.id, { id: h.id, label: infoLabel(h) ?? prev?.label ?? h.id, source: prev?.source ?? sourceOf(h), owned: prev?.owned })
      }
      return Array.from(byId.values())
    })
    setFsId((cur) => {
      const next = cur && (live.has(cur) || ownedRef.current.has(cur)) ? cur : (list[0]?.id ?? null)
      if (next !== cur) setPath('') // the start folder belonged to the expired handle
      return next
    })
  }, [handles.data])

  const finish = (r: SaveAsResult | null) => {
    if (done.current) return
    done.current = true
    // Close handles we opened that are not handed back.
    const keep = r && r.target === 'remote' ? r.fsId : null
    for (const id of ownedRef.current) if (id !== keep) void closeFs(id).catch(() => undefined)
    onDone(r)
  }

  // listing
  useEffect(() => {
    if (!fsId) return
    const ctl = new AbortController()
    setLoading(true)
    setError(null)
    // another folder / location: earlier search results no longer apply
    searchCtl.current?.abort()
    setSearching(false)
    setResults(null)
    listDir(fsId, path, ctl.signal)
      .then((res) => {
        const sorted = [...res.entries].sort((x, y) => {
          const dx = x.type === 'dir' || (x.type === 'symlink' && x.linkType === 'dir') ? 0 : 1
          const dy = y.type === 'dir' || (y.type === 'symlink' && y.linkType === 'dir') ? 0 : 1
          return dx - dy || x.name.localeCompare(y.name, undefined, { numeric: true, sensitivity: 'base' })
        })
        setListing({ ...res, entries: sorted })
        setPathInput(res.path)
        // Keyboard-first: the list takes the focus once there is one (not away from a field being typed in).
        if (!saveMode && !findMode && !(document.activeElement instanceof HTMLInputElement)) listRef.current?.focus()
        lastDirs.set(fsId, res.path)
        lastFsId = fsId
        setActive(0)
      })
      .catch((err: unknown) => {
        if (ctl.signal.aborted) return
        setError(errorMessage(err))
        setListing(null)
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false)
      })
    return () => ctl.abort()
  }, [fsId, path, refresh])

  const connect = async (req: FsOpenRequest, label: string) => {
    setConnecting(true)
    setError(null)
    try {
      const h = await openFs(req)
      rememberHandle(h)
      ownedRef.current.add(h.id)
      const choice: FsChoice = { id: h.id, label: h.label || label, source: req, owned: true }
      setChoices((c) => [...c.filter((x) => x.id !== h.id), choice])
      setFsId(h.id)
      setPath(h.home || '')
    } catch (err) {
      setError(`Could not open ${label}: ${errorMessage(err)}`)
    } finally {
      setConnecting(false)
    }
  }

  const busy = loading || connecting || searching
  // Listing a folder is navigation (quiet for a second); connecting and searching are waits the user asked for.
  const listingShown = useDelayedFlag(loading && !connecting && !searching, DELAY_PRESETS.NAVIGATION)
  const waitShown = useDelayedFlag(connecting || searching)
  const busyShown = listingShown || waitShown

  const isDir = (e: FileEntry) => e.type === 'dir' || (e.type === 'symlink' && e.linkType === 'dir')
  const entries = results ? results.entries : (listing?.entries ?? [])
  const current = choices.find((c) => c.id === fsId)

  const runSearch = async () => {
    const text = query.trim()
    if (!fsId || !listing || !text) return
    searchCtl.current?.abort()
    const ctl = new AbortController()
    searchCtl.current = ctl
    setSearching(true)
    setError(null)
    try {
      const res = await searchFiles(fsId, { path: listing.path, content: text, pattern: glob.trim() }, ctl.signal)
      if (ctl.signal.aborted) return
      const files = res.entries.filter((e) => !isDir(e)).sort((x, y) => x.path.localeCompare(y.path, undefined, { numeric: true }))
      setResults({ dir: listing.path, text, entries: files, truncated: res.truncated })
      setActive(0)
    } catch (err) {
      if (!ctl.signal.aborted) setError(errorMessage(err))
    } finally {
      if (searchCtl.current === ctl) setSearching(false)
    }
  }

  const enter = (e: FileEntry) => {
    if (isDir(e)) {
      setPath(e.path)
      return
    }
    if (saveMode) {
      setName(e.name)
      nameRef.current?.focus()
    } else confirmOpen(e)
  }

  /** The highlighted row, when it is a file (what "Open" opens). */
  const activeFile = entries[active] && !isDir(entries[active]) ? entries[active] : null

  const confirmOpen = (e: FileEntry | null = activeFile) => {
    if (!fsId || !e || isDir(e)) return
    finish({ target: 'remote', fsId, path: e.path, label: current?.label, source: current?.source, ownsFs: current?.owned, find: results?.text })
  }

  const confirmSave = () => {
    if (!fsId || !listing) return
    const n = name.trim()
    if (!n || n.includes('/')) return
    finish({ target: 'remote', fsId, path: joinPath(listing.path, n), label: current?.label, source: current?.source, ownsFs: current?.owned })
  }

  const goUp = () => {
    if (listing?.parent && listing.parent !== listing.path) setPath(listing.parent)
  }

  useEffect(() => {
    listRef.current?.querySelector<HTMLElement>(`[data-idx="${active}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [active])

  const onListKey = (e: ReactKeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActive((i) => Math.min(entries.length - 1, i + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((i) => Math.max(0, i - 1))
    } else if (e.key === 'Home') {
      e.preventDefault()
      setActive(0)
    } else if (e.key === 'End') {
      e.preventDefault()
      setActive(Math.max(0, entries.length - 1))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      const it = entries[active]
      if (it) enter(it)
    } else if (e.key === 'Backspace') {
      e.preventDefault()
      goUp()
    } else if (e.key.length === 1 && /\S/.test(e.key)) {
      // type-ahead
      const ch = e.key.toLowerCase()
      const start = active + 1
      const idx = [...entries.slice(start), ...entries.slice(0, start)].findIndex((x) => x.name.toLowerCase().startsWith(ch))
      if (idx >= 0) setActive((start + idx) % entries.length)
    }
  }

  const localAllowed = saveMode && opts.allowLocal !== false
  const title = opts.title ?? (saveMode ? 'Save as' : findMode ? 'Find in files' : 'Open remote file')
  const canLocalFs = mode !== 'server' || isAdmin

  return (
    <Dialog open onOpenChange={(o) => !o && finish(null)}>
      <DialogContent
        size="xl"
        className="max-h-[min(44rem,calc(100dvh-2rem))] gap-3"
        onOpenAutoFocus={(e) => {
          // Keyboard-first: open straight into the file list (not the location menu) when there is a location.
          if (saveMode || findMode || !fsId) return
          e.preventDefault()
          listRef.current?.focus()
        }}
      >
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            {findMode
              ? 'Search the folder shown (and its subfolders) on a connected server for files containing a text. Case is ignored.'
              : saveMode
                ? 'Choose a folder on a connected server and a file name'
                : 'Choose a file on a connected server'}
            {findMode ? '' : localAllowed ? ', or save to this computer.' : '.'}
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-wrap items-center gap-1.5">
          <LocationSelect
            value={fsId}
            disabled={connecting}
            choices={choices}
            sessions={sshSessions.map((x) => ({ id: x.id, label: x.title || `${x.username ?? ''}@${x.host ?? ''}` }))}
            connections={fileConns.map((c) => ({ id: c.id, label: `${c.name} (${c.protocol.toUpperCase()})` }))}
            serverFiles={canLocalFs}
            onChange={(v) => {
              if (v.startsWith('open:')) {
                const [, kind, id] = v.split(':')
                if (kind === 'local') void connect({ local: true }, 'Server files')
                else if (kind === 'conn') void connect({ connectionId: id }, fileConns.find((c) => c.id === id)?.name ?? 'connection')
                else if (kind === 'sess') void connect({ sessionId: id }, sshSessions.find((x) => x.id === id)?.title ?? 'session')
                return
              }
              setFsId(v)
              setPath(lastDirs.get(v) || '')
            }}
          />
          <form
            className="flex min-w-48 flex-[2] items-center gap-1"
            onSubmit={(e) => {
              e.preventDefault()
              if (fsId) setPath(pathInput.trim())
            }}
          >
            <Input inputSize="md" value={pathInput} onChange={(e) => setPathInput(e.target.value)} aria-label="Folder path" className="font-mono text-sm" disabled={!fsId} spellCheck={false} />
            <Button type="submit" size="icon" variant="secondary" aria-label="Go to folder" disabled={!fsId}>
              <CornerDownLeft />
            </Button>
          </form>
          <Button size="icon" variant="ghost" aria-label="Parent folder" onClick={goUp} disabled={!listing || !listing.parent || listing.parent === listing.path}>
            <ArrowUp />
          </Button>
          <Button
            size="icon"
            variant="ghost"
            aria-label="Home folder"
            onClick={() => {
              setPath('')
              setRefresh((n) => n + 1)
            }}
            disabled={!fsId}
          >
            <House />
          </Button>
          <Button size="icon" variant="ghost" aria-label="Refresh" onClick={() => setRefresh((n) => n + 1)} disabled={!fsId}>
            <RefreshCw />
          </Button>
        </div>

        {findMode && (
          <form
            className="flex flex-wrap items-center gap-1.5"
            role="search"
            onSubmit={(e) => {
              e.preventDefault()
              void runSearch()
            }}
          >
            <Input
              inputSize="md"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Text to find"
              aria-label="Text to find"
              className="min-w-48 flex-[2]"
              spellCheck={false}
              autoFocus
            />
            <Input
              inputSize="md"
              value={glob}
              onChange={(e) => setGlob(e.target.value)}
              placeholder="Files, e.g. *.conf"
              aria-label="Only files named (glob)"
              className="w-44 font-mono text-sm"
              spellCheck={false}
            />
            <Button type="submit" disabled={!fsId || !listing || !query.trim()} loading={searching}>
              Search
            </Button>
          </form>
        )}
        {results && (
          <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground" aria-live="polite">
            <span className="min-w-0 flex-1 truncate">
              {results.entries.length === 0 ? 'No files' : `${results.entries.length.toLocaleString()} file${results.entries.length === 1 ? '' : 's'}`} containing
              “<span className="text-foreground">{results.text}</span>” in <span className="font-mono">{results.dir}</span>
              {results.truncated ? ' (the first ones only — narrow the search)' : ''}
            </span>
            <Button
              size="xs"
              variant="secondary"
              onClick={() => {
                setResults(null)
                setActive(0)
              }}
            >
              Back to folder
            </Button>
          </div>
        )}

        <div
          ref={listRef}
          role="listbox"
          aria-label={results ? 'Search results' : 'Files'}
          tabIndex={0}
          aria-activedescendant={entries[active] ? `nx-pick-${active}` : undefined}
          onKeyDown={onListKey}
          // A fixed height: the dialog keeps its size and place whatever the folder holds.
          className="h-[min(20rem,45dvh)] shrink-0 overflow-y-auto rounded-md border bg-background/40 p-1 outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
        >
          {!fsId ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 text-center text-sm text-muted-foreground">
              <Server className="size-5" />
              Choose an open file system, a running SSH session or a saved connection above.
            </div>
          ) : busyShown || (busy && !listing && !error) ? (
            // Calm (docs/UX.md): the previous listing stays until the next one is there, unless that takes > 300 ms.
            <LoadingPane active={busyShown} immediate label={connecting ? 'Connecting…' : searching ? 'Searching…' : 'Loading…'} />
          ) : error ? (
            <div className="m-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm" role="alert">
              {error}
            </div>
          ) : entries.length === 0 ? (
            <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
              {results ? 'No file contains this text' : 'This folder is empty'}
            </div>
          ) : (
            entries.map((e, i) => {
              const dir = isDir(e)
              const Icon = e.type === 'symlink' ? FolderSymlink : dir ? Folder : File
              return (
                <div
                  key={e.path}
                  id={`nx-pick-${i}`}
                  data-idx={i}
                  role="option"
                  aria-selected={i === active}
                  className={cn(
                    'flex h-7 cursor-default items-center gap-2 rounded-sm px-2 text-sm',
                    i === active ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/50',
                  )}
                  onClick={() => {
                    setActive(i)
                    if (!dir && saveMode) setName(e.name)
                  }}
                  onDoubleClick={() => enter(e)}
                >
                  <Icon className={cn('size-3.5 shrink-0', dir ? 'text-primary' : 'text-muted-foreground')} />
                  <span className={cn('min-w-0 flex-1 truncate', e.hidden && 'opacity-60', results && 'font-mono text-xs')} title={e.path}>
                    {results ? relativeTo(results.dir, e.path) : e.name}
                  </span>
                  {!dir && <span className="shrink-0 text-xs text-muted-foreground tabular">{formatBytes(e.size)}</span>}
                  <span className="hidden w-28 shrink-0 text-right text-xs text-muted-foreground tabular sm:inline" title={formatDateTime(e.mtime)}>
                    {shortDate(e.mtime)}
                  </span>
                </div>
              )
            })
          )}
        </div>

        {saveMode ? (
          <form
            className="flex items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              confirmSave()
            }}
          >
            <label htmlFor="nx-saveas-name" className="shrink-0 text-sm font-medium">
              File name
            </label>
            <Input id="nx-saveas-name" ref={nameRef} value={name} onChange={(e) => setName(e.target.value)} className="font-mono" spellCheck={false} autoFocus />
            <Button type="submit" disabled={!fsId || !listing || !name.trim() || name.includes('/')}>
              Save
            </Button>
          </form>
        ) : null}

        <DialogFooter className="items-center sm:justify-between">
          <div className="flex flex-wrap items-center gap-2">
            {localAllowed && (
              <>
                <span className="flex items-center gap-1 text-xs text-muted-foreground">
                  <Laptop className="size-3.5" /> This computer:
                </span>
                {canPickSaveFile() && (
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={async () => {
                      try {
                        const h = await pickSaveFile(name.trim() || opts.suggestedName || 'untitled.txt')
                        if (h) finish({ target: 'handle', handle: h, filename: h.name })
                      } catch (err) {
                        setError(errorMessage(err))
                      }
                    }}
                  >
                    <HardDrive /> Save to file…
                  </Button>
                )}
                <Button size="sm" variant="secondary" onClick={() => finish({ target: 'download', filename: baseName(name.trim()) || opts.suggestedName || 'untitled.txt' })}>
                  <Download /> Download
                </Button>
              </>
            )}
          </div>
          <div className="flex items-center gap-2">
            <Button variant="secondary" onClick={() => finish(null)}>
              Cancel
            </Button>
            {!saveMode && (
              <Button onClick={() => confirmOpen()} disabled={!activeFile}>
                {opts.confirmLabel ?? 'Open'}
              </Button>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

interface LocationOption {
  id: string
  label: string
}

/** Where to browse: the open file systems, or a running session / saved connection / the server's files to open. */
function LocationSelect({
  value,
  disabled,
  choices,
  sessions,
  connections,
  serverFiles,
  onChange,
}: {
  value: string | null
  disabled: boolean
  choices: FsChoice[]
  sessions: LocationOption[]
  connections: LocationOption[]
  serverFiles: boolean
  onChange: (value: string) => void
}) {
  return (
    <Select value={value ?? undefined} onValueChange={onChange} disabled={disabled}>
      <SelectTrigger aria-label="Location" className="max-w-72 min-w-44 flex-1">
        <SelectValue placeholder="Choose a location…" />
      </SelectTrigger>
      <SelectContent>
        {choices.length > 0 && (
          <SelectGroup>
            <SelectLabel>Open file systems</SelectLabel>
            {choices.map((c) => (
              <SelectItem key={c.id} value={c.id}>
                {c.label}
              </SelectItem>
            ))}
          </SelectGroup>
        )}
        {sessions.length > 0 && (
          <SelectGroup>
            <SelectLabel>Running sessions</SelectLabel>
            {sessions.map((x) => (
              <SelectItem key={x.id} value={`open:sess:${x.id}`}>
                {x.label}
              </SelectItem>
            ))}
          </SelectGroup>
        )}
        {connections.length > 0 && (
          <SelectGroup>
            <SelectLabel>Saved connections</SelectLabel>
            {connections.map((c) => (
              <SelectItem key={c.id} value={`open:conn:${c.id}`}>
                {c.label}
              </SelectItem>
            ))}
          </SelectGroup>
        )}
        {serverFiles && (
          <SelectGroup>
            <SelectLabel>NexTerm server</SelectLabel>
            <SelectItem value="open:local:">Server files (the machine running NexTerm)</SelectItem>
          </SelectGroup>
        )}
      </SelectContent>
    </Select>
  )
}
