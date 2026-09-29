/*
 * FileBrowser: one complete file view — location bar, toolbar, filter, the virtualized list and a status line —
 * used by the SFTP side panel, files tabs and commander panes. Navigation is optimistic: the location bar shows the
 * requested folder at once, the previous listing stays (dimmed) until the new one arrives, and a failed navigation
 * away from a working folder keeps the old folder and explains why.
 */
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import {
  ArrowLeft,
  ArrowRight,
  ArrowUp,
  ArrowUpDown,
  CircleAlert,
  Download,
  Eye,
  EyeOff,
  FilePlus,
  FolderOpen,
  FolderPlus,
  FolderUp,
  FolderX,
  House,
  ListFilter,
  RefreshCw,
  Search,
  ShieldAlert,
  Trash,
  Upload,
} from 'lucide-react'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import type { FileEntry } from '@/api/types'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { useIsMobile } from '@/lib/hooks'
import { DELAY_PRESETS, useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, formatBytes, formatRate } from '@/lib/utils'
import { describeError } from '../actions'
import { fsApi, fsKeys, isHandleGone, isPermissionDenied } from '../api'
import { acceptDrag, performDrop } from '../dnd'
import { countOf, describeCounts, isDirLike, totalSize } from '../format'
import { reportFsError } from '../fsHandles'
import { dirname, isRoot, normalizePath } from '../paths'
import { filesSettings, pushRecentPath } from '../settings'
import { openTransfers, summarize, useTransferViews } from '../transfers/store'
import type { FsContext, ListRow } from '../types'
import { useFileColumns } from './columns'
import { useBrowserController, type BrowserController, type ViewVariant } from './controller'
import { FileList } from './FileList'
import { PathBar } from './PathBar'
import { hiddenCounts, sortEntries, visibleRows } from './rows'
import { FilterRow, ResponsiveToolbar, type ToolAction } from './Toolbar'
import { getView, hasView, patchView, resetView, useView } from './viewStore'

export interface FileBrowserProps {
  viewId: string
  ctx: FsContext
  variant: ViewVariant
  /** Folder shown when the view has none yet. */
  initialPath?: string
  onPathChange?: (path: string) => void
  /** Commander: the other pane. */
  peer?: () => BrowserController | undefined
  /** Commander: this pane has the focus. */
  active?: boolean
  onActivate?: () => void
  /** Extra toolbar actions (the panel adds "Open in full tab"). */
  extraActions?: ToolAction[]
  /** Above the location bar (the panel's session chip, a pane's source picker). */
  header?: ReactNode
  /** Below the status line (the panel's "Follow terminal folder"). */
  footer?: ReactNode
  /** Right side of the toolbar row. */
  toolbarTrailing?: ReactNode
  className?: string
  /** Not interactive (no focus, no pointer, no drops): the panel's session is offline. */
  inert?: boolean
  /** Receives the controller (the tab keeps refs to its panes). */
  controllerRef?: (c: BrowserController) => void
}

/**
 * The navigation indicator (docs/UX.md, navigation-like: 1000 ms / ≥ 800 ms): folders list well under the delay (cached
 * and prefetched ones instantly), so the user normally sees no indicator at all; a slow one gets a single spinner that
 * then stays long enough to read.
 */
const NAV_INDICATOR = DELAY_PRESETS.NAVIGATION

const EMPTY_ENTRIES: FileEntry[] = []
const EMPTY_ROWS: ListRow[] = []

function useWidth<T extends HTMLElement>(): [React.RefObject<T | null>, number] {
  const ref = useRef<T>(null)
  const [w, setW] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(() => setW(el.clientWidth))
    ro.observe(el)
    setW(el.clientWidth)
    return () => ro.disconnect()
  }, [])
  return [ref, w]
}

export function FileBrowser(props: FileBrowserProps) {
  const { viewId, ctx, variant, initialPath, onPathChange, active = true } = props
  const fsId = ctx.handle.id
  const mobile = useIsMobile()
  const settings = filesSettings.use()
  const path = useView(viewId, (v) => v.path)
  const pending = useView(viewId, (v) => v.pending)
  const refreshing = useView(viewId, (v) => v.refreshing > 0)
  const cursor = useView(viewId, (v) => v.cursor)
  const filter = useView(viewId, (v) => v.filter)
  const backLen = useView(viewId, (v) => v.back.length)
  const fwdLen = useView(viewId, (v) => v.forward.length)
  const selectedCount = useView(viewId, (v) => v.selected.size)
  const filterRef = useRef<HTMLInputElement>(null)
  const [rootRef, width] = useWidth<HTMLDivElement>()
  const onPathChangeRef = useRef(onPathChange)
  onPathChangeRef.current = onPathChange

  // First visit of this view (or the view now shows another file system): go to the initial folder.
  useLayoutEffect(() => {
    const v = getView(viewId)
    const start = normalizePath(initialPath || ctx.handle.home || '/')
    if (!hasView(viewId) || (v.fsKey !== null && v.fsKey !== ctx.key)) resetView(viewId, ctx.key, start)
    else if (v.path === null && v.pending === null) patchView(viewId, { fsKey: ctx.key, pending: start, pendingMode: 'replace', pendingSelect: null })
    else if (v.fsKey === null) patchView(viewId, { fsKey: ctx.key })
  }, [viewId, initialPath, ctx.handle.home, ctx.key])

  const requested = pending ?? path
  const q = useQuery({
    queryKey: fsKeys.list(fsId, requested ?? ''),
    queryFn: ({ signal }) => fsApi.list(fsId, requested!, signal),
    enabled: !!requested,
    placeholderData: keepPreviousData,
    staleTime: 2500,
    retry: false,
    // Keep the folder live (files created in the terminal, saved from the editor…) without hammering huge folders.
    refetchInterval: (query) => ((query.state.data?.entries.length ?? 0) > 5000 || query.state.status === 'error' ? false : 8000),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
  })

  // A listing arrived: confirm the folder, update the history, select what was asked for.
  useEffect(() => {
    if (!q.data || q.isPlaceholderData || !requested) return
    const canonical = normalizePath(q.data.path || requested)
    if (canonical !== requested) queryClient.setQueryData(fsKeys.list(fsId, canonical), q.data)
    const v = getView(viewId)
    if (v.pending === null && v.path === canonical) return
    if (v.pending !== null && v.pending !== requested) return
    const old = v.path
    let back = v.back
    let forward = v.forward
    switch (v.pendingMode) {
      case 'back':
        back = back.slice(0, -1)
        forward = old ? [old, ...forward].slice(0, 50) : forward
        break
      case 'forward':
        forward = forward.slice(1)
        back = old ? [...back, old].slice(-50) : back
        break
      case 'replace':
        break
      default:
        if (old && old !== canonical) {
          back = [...back, old].slice(-50)
          forward = []
        }
    }
    const sel = v.pendingSelect
    patchView(viewId, (s) => ({
      path: canonical,
      pending: null,
      pendingMode: null,
      pendingSelect: null,
      pendingQuiet: false,
      back,
      forward,
      filter: old && old !== canonical ? '' : s.filter,
      selected: sel ? new Set([sel]) : old !== canonical ? new Set<string>() : s.selected,
      anchor: sel ?? (old !== canonical ? null : s.anchor),
      cursor: sel ?? (old !== canonical ? null : s.cursor),
      renaming: null,
      revealTick: sel ? s.revealTick + 1 : s.revealTick,
    }))
    pushRecentPath(ctx.placeKey, canonical)
    onPathChangeRef.current?.(canonical)
  }, [q.data, q.isPlaceholderData, requested, fsId, viewId, ctx.placeKey])

  // A navigation failed: stay where we were (when there is such a folder) and explain.
  useEffect(() => {
    if (!q.isError || !requested) return
    if (isHandleGone(q.error)) {
      reportFsError(ctx.key, q.error)
      return
    }
    const v = getView(viewId)
    if (!v.pending || v.pending !== requested) return
    const target = requested
    const error = q.error
    const revert = () => {
      const cur = getView(viewId)
      // First visit: the error placeholder explains (with Retry / Parent / Home). Elsewhere meanwhile: nothing to do.
      if (cur.pending !== target || !cur.path) return
      const d = describeError(error)
      toast.error(`${d.title}: ${target}`, { description: d.permission ? undefined : d.description })
      patchView(viewId, { pending: null, pendingMode: null, pendingSelect: null, pendingQuiet: false })
    }
    if (isPermissionDenied(error) || isRoot(target)) {
      revert()
      return
    }
    // Not a folder (a file typed in the location bar or passed by "Open files here"): show its folder, file selected.
    let cancelled = false
    fsApi.stat(fsId, target).then(
      (e) => {
        if (cancelled) return
        if (e && !isDirLike(e) && getView(viewId).pending === target) {
          patchView(viewId, { pending: dirname(target), pendingMode: v.pendingMode === 'replace' ? 'replace' : 'push', pendingSelect: target })
        } else revert()
      },
      () => {
        if (!cancelled) revert()
      },
    )
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q.isError, q.errorUpdatedAt, requested])

  const entries = q.data && !q.isPlaceholderData ? q.data.entries : (q.data?.entries ?? EMPTY_ENTRIES)
  const shownDir = q.data && !q.isPlaceholderData ? normalizePath(q.data.path || requested || '/') : path
  // Sort once per listing / sort setting; the filters (typing in the filter box) only walk the sorted list.
  const sorted = useMemo(
    () => sortEntries(entries, { sortBy: settings.sortBy, sortDesc: settings.sortDesc, foldersFirst: settings.foldersFirst }),
    [entries, settings.sortBy, settings.sortDesc, settings.foldersFirst],
  )
  const showPartial = settings.showPartialUploads === true
  const rows = useMemo(
    () => visibleRows(sorted, shownDir, { showHidden: settings.showHidden, showPartial, filter }),
    [sorted, shownDir, settings.showHidden, showPartial, filter],
  )

  const controller = useBrowserController({
    viewId,
    ctx,
    rows,
    entries,
    variant,
    peer: props.peer,
    onActivate: props.onActivate,
  })
  const { controllerRef } = props
  useEffect(() => controllerRef?.(controller), [controller, controllerRef])

  // Warm the folders the user is likely to open next: the parent once a folder is shown ("Up", breadcrumbs), and a
  // folder the cursor rests on (keyboard navigation; the list prefetches on hover).
  useEffect(() => {
    if (path && !isRoot(path)) controller.prefetch(dirname(path))
  }, [path, controller])
  const cursorDir = useMemo(() => {
    const e = cursor ? entries.find((x) => x.path === cursor) : undefined
    return e && isDirLike(e) ? e.path : null
  }, [cursor, entries])
  useEffect(() => {
    if (!cursorDir) return
    const t = setTimeout(() => controller.prefetch(cursorDir), 150)
    return () => clearTimeout(t)
  }, [cursorDir, controller])

  const columns = useFileColumns(width)
  const rowHeight = mobile ? 32 : 22

  // --- toolbar -------------------------------------------------------------------------------------------------------
  const atRoot = !path || isRoot(path)
  const tabLike = variant !== 'panel'
  const actions: ToolAction[] = [
    ...(tabLike
      ? [
          { id: 'back', label: 'Back', icon: ArrowLeft, shortcut: 'Alt+ArrowLeft', run: () => controller.back(), disabled: !backLen, priority: 30, group: 0 },
          { id: 'forward', label: 'Forward', icon: ArrowRight, shortcut: 'Alt+ArrowRight', run: () => controller.forward(), disabled: !fwdLen, priority: 29, group: 0 },
        ]
      : []),
    { id: 'up', label: 'Parent folder', icon: ArrowUp, shortcut: 'Backspace', run: () => controller.up(), disabled: atRoot, priority: 100, group: 0 },
    { id: 'refresh', label: 'Refresh', icon: RefreshCw, shortcut: '$mod+r', run: () => controller.refresh(), busy: refreshing, priority: 96, group: 0 },
    { id: 'home', label: 'Home folder', icon: House, run: () => controller.home(), priority: 90, group: 0 },
    { id: 'mkdir', label: 'New folder', icon: FolderPlus, shortcut: 'F7', run: () => controller.newFolder(), priority: 70, group: 1 },
    { id: 'touch', label: 'New file', icon: FilePlus, run: () => controller.newFile(), priority: 45, group: 1 },
    { id: 'upload', label: 'Upload files', icon: Upload, run: () => controller.upload(false), priority: 88, group: 2 },
    { id: 'uploadDir', label: 'Upload folder', icon: FolderUp, run: () => controller.upload(true), priority: 40, group: 2 },
    { id: 'download', label: selectedCount > 1 ? 'Download selection (zip)' : 'Download', icon: Download, run: () => controller.download(), disabled: !selectedCount, priority: 86, group: 2 },
    { id: 'delete', label: 'Delete', icon: Trash, shortcut: 'Delete', run: () => void controller.deleteSelection(), disabled: !selectedCount, priority: 60, group: 3 },
    { id: 'search', label: 'Search files…', icon: Search, run: () => controller.search(), priority: 25, group: 4 },
    ...(props.extraActions ?? []),
  ]

  // --- body ----------------------------------------------------------------------------------------------------------
  let placeholder: ReactNode = null
  // A navigation away from a working folder failed: the effect above returns to that folder (no error flash).
  const reverting = q.isError && !!pending && !!path && pending === requested && !isHandleGone(q.error)
  const reopening = q.isError && isHandleGone(q.error)
  const firstLoad = !q.data && (q.isPending || !requested || reverting || reopening)
  // Calm loading (docs/UX.md): a folder that lists quickly never shows a skeleton; old rows stay while another loads.
  const showSkeleton = useDelayedFlag(firstLoad)
  // Navigation feels instant: the location bar shows the new folder at once, the previous rows stay until the new ones
  // are there (cached listings show immediately and revalidate silently). Only a navigation that is still going after
  // NAV_INDICATOR.delay gets one small spinner, in a reserved slot of the location bar; a burst of navigations keeps
  // the same one. Background refreshes (polling, window focus, other views' changes) never show anything.
  const navigating = !!pending && !firstLoad && (q.isPlaceholderData || q.isFetching) && !q.isError
  const showNav = useDelayedFlag(navigating, NAV_INDICATOR)
  if (firstLoad) placeholder = showSkeleton ? <SkeletonRows rows={9} rowHeight={rowHeight} className="pt-1" /> : <div aria-hidden className="h-full" />
  else if (!q.data && q.isError) {
    const d = describeError(q.error)
    placeholder = (
      <EmptyState
        size="sm"
        icon={d.permission ? ShieldAlert : d.notFound ? FolderX : CircleAlert}
        title={d.title}
        description={
          <>
            <span className="font-mono break-all">{requested}</span>
            {!d.permission && d.description && <span className="mt-1 block">{d.description}</span>}
          </>
        }
        action={
          <>
            <Button size="xs" variant="secondary" onClick={() => void q.refetch()}>
              <RefreshCw /> Retry
            </Button>
            {requested && !isRoot(requested) && (
              <Button size="xs" variant="secondary" onClick={() => patchView(viewId, { pending: dirname(requested), pendingMode: 'replace' })}>
                <ArrowUp /> Parent
              </Button>
            )}
            <Button size="xs" variant="secondary" onClick={() => patchView(viewId, { pending: ctx.handle.home || '/', pendingMode: 'replace' })}>
              <House /> Home
            </Button>
          </>
        }
      />
    )
  }
  // An empty folder keeps its ".." row (the list stays usable) with a hint below it.
  let after: ReactNode = null
  if (!placeholder && q.data && !rows.some((r) => !r.parent)) {
    const concealed = hiddenCounts(entries, { showHidden: settings.showHidden, showPartial })
    after = filter ? (
      <EmptyState
        size="sm"
        icon={ListFilter}
        title="No matching items"
        description={`Nothing in this folder matches “${filter}”.`}
        action={
          <Button size="xs" variant="secondary" onClick={() => patchView(viewId, { filter: '' })}>
            Clear filter
          </Button>
        }
      />
    ) : concealed.hidden > 0 ? (
      <EmptyState
        size="sm"
        icon={EyeOff}
        title="Only hidden items"
        description={`${countOf(concealed.hidden, 'hidden item')} (names starting with a dot).`}
        action={
          <Button size="xs" variant="secondary" onClick={() => filesSettings.set({ showHidden: true })}>
            <Eye /> Show hidden files
          </Button>
        }
      />
    ) : (
      <EmptyState
        size="sm"
        icon={FolderOpen}
        title="This folder is empty"
        description={
          concealed.partial > 0
            ? `Drop files or folders here to upload them. ${countOf(concealed.partial, 'unfinished upload')} (.nexterm-part) ${concealed.partial === 1 ? 'is' : 'are'} hidden.`
            : 'Drop files or folders here to upload them.'
        }
        action={
          <Button size="xs" variant="secondary" onClick={() => controller.upload(false)}>
            <Upload /> Upload files
          </Button>
        }
      />
    )
  }

  const label = `Files in ${path ?? requested ?? ''}`
  const filterRow = (
    <FilterRow
      value={filter}
      onChange={(v) => patchView(viewId, { filter: v })}
      columns={columns}
      inputRef={filterRef}
      onEnterList={() => controller.focusList()}
      className={variant === 'panel' ? 'px-1.5' : 'w-56 @4xl:w-72'}
    />
  )

  return (
    <div
      ref={rootRef}
      className={cn('@container flex h-full min-h-0 min-w-0 flex-col', props.className)}
      inert={props.inert || undefined}
      // Read by scripts/flash-audit.mjs (region of interest, "navigation settled").
      data-file-browser={variant}
      data-path={path ?? undefined}
      data-pending={pending ?? undefined}
      // Files dropped anywhere else in the view (location bar gaps, toolbar, status line, footer) go to the folder
      // shown — never to the browser, which would open the file instead of NexTerm.
      onDragOver={(e) => {
        if (path && !props.inert) acceptDrag(e, ctx, path)
      }}
      onDrop={(e) => {
        if (!path || props.inert) return
        controller.markActive()
        performDrop(e, ctx, path)
      }}
      onKeyDown={(e) => {
        const mod = e.ctrlKey || e.metaKey
        if (mod && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'f') {
          e.preventDefault()
          filterRef.current?.focus()
          filterRef.current?.select()
        }
      }}
      onMouseDownCapture={() => controller.markActive()}
    >
      {props.header}
      {variant === 'panel' ? (
        <>
          <div className="px-1.5 pt-1.5">
            <PathBar controller={controller} ctx={ctx} busy={showNav} />
          </div>
          <ResponsiveToolbar actions={actions} label="File actions" className="px-1" trailing={props.toolbarTrailing} />
          {filterRow}
        </>
      ) : (
        <>
          <div className="flex items-center gap-1 px-1.5 pt-1.5">
            <PathBar controller={controller} ctx={ctx} busy={showNav} className="flex-1" />
          </div>
          <div className="flex min-w-0 items-center gap-1 pr-1.5">
            <ResponsiveToolbar actions={actions} label="File actions" className="min-w-0 flex-1 px-1" trailing={props.toolbarTrailing} />
            {filterRow}
          </div>
        </>
      )}
      <div className="mt-1 flex min-h-0 flex-1 flex-col border-t">
        <FileList
          controller={controller}
          ctx={ctx}
          rows={placeholder ? EMPTY_ROWS : rows}
          columns={columns}
          rowHeight={rowHeight}
          busy={navigating}
          contentKey={shownDir ?? ''}
          placeholder={placeholder}
          after={after}
          label={label}
          commander={variant === 'pane'}
          active={active}
        />
      </div>
      <StatusLine viewId={viewId} entries={entries} rows={rows} controller={controller} filter={filter} loading={firstLoad ? (showSkeleton ? 'shown' : 'hidden') : undefined} />
      {props.footer}
    </div>
  )
}

function StatusLine({
  viewId,
  entries,
  rows,
  controller,
  filter,
  loading,
}: {
  viewId: string
  entries: FileEntry[]
  rows: ListRow[]
  controller: BrowserController
  filter: string
  /** No listing to describe yet (first load of a folder): "Loading…" once the delay passed, blank before. */
  loading?: 'shown' | 'hidden'
}) {
  const selected = useView(viewId, (v) => v.selected)
  const views = useTransferViews()
  const sum = summarize(views)
  // Quick transfers (saving an edited file) never flash the transfers button (300 ms / ≥ 600 ms).
  const transfersShown = useDelayedFlag(sum.active > 0)
  const shown = rows.filter((r) => !r.parent).map((r) => r.entry)
  const selEntries = selected.size ? controller.selectedEntries() : []
  const selBytes = totalSize(selEntries)
  return (
    <div className="flex h-6 min-w-0 shrink-0 items-center gap-2 border-t px-2 text-xs text-muted-foreground" aria-live="polite">
      <span className="truncate">
        {loading ? (loading === 'shown' ? 'Loading…' : '') : filter ? `${shown.length.toLocaleString()} of ${countOf(entries.length, 'item')}` : describeCounts(shown)}
        {!loading && !filter && shown.length > 0 && <span className="hidden @xs:inline"> · {formatBytes(totalSize(shown))}</span>}
      </span>
      {selEntries.length > 0 && (
        <span className="truncate text-foreground/80">
          · {selEntries.length.toLocaleString()} selected{selBytes > 0 ? ` (${formatBytes(selBytes)})` : ''}
        </span>
      )}
      <span className="flex-1" />
      {transfersShown && (
        <button
          type="button"
          onClick={openTransfers}
          className="flex shrink-0 items-center gap-1 rounded-sm px-1 text-primary outline-none hover:bg-accent focus-visible:ring-1 focus-visible:ring-ring"
          title="Show transfers"
        >
          <ArrowUpDown className="size-3" />
          <span className="tabular">
            {sum.active ? `${sum.active} · ${Math.round((sum.progress ?? 0) * 100)}%` : '100%'}
          </span>
          {sum.bytesPerSec > 0 && <span className="hidden tabular @sm:inline">· {formatRate(sum.bytesPerSec)}</span>}
        </button>
      )}
    </div>
  )
}
