/*
 * Sessions sidebar panel: toolbar, fuzzy search (type-to-connect), filter chips, Favorites / Recent, and the session
 * tree — or ranked results while searching. One context menu serves every row (registry target `session-node`).
 */
import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import Fuse from 'fuse.js'
import {
  ArrowDownUp,
  ChevronsDownUp,
  ChevronsUpDown,
  Clock,
  Ellipsis,
  FolderPlus,
  FolderTree,
  Import,
  ListFilter,
  Plus,
  Search,
  Star,
  Tag,
  UserRoundCog,
  Users,
  Activity,
  X,
  Zap,
} from 'lucide-react'
import { getContextMenuItems } from '@/app/registry'
import { runCommand, useCommand } from '@/app/commands'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { recentConnections, useConnections } from '@/api/connections'
import { useFolders } from '@/api/folders'
import { useSessions } from '@/api/sessions'
import type { Connection } from '@/api/types'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingPane } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { useIsMobile } from '@/lib/hooks'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, errorMessage } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { connectSafely } from '../connect'
import { openFolderDialog, openSessionEditor } from '../dialogs/store'
import type { SessionMenuContext } from '../menus'
import { buildTreeIndex, collectTags, folderPathLabel, groupRunningSessions, parseNodeId } from '../model'
import { sessionsSettings } from '../settings'
import type { GroupBy, SortMode } from '../types'
import { registerTreeController, useTreeFocus } from './controller'
import { SearchResults, type SearchHit } from './SearchResults'
import { SessionContextMenu } from './SessionContextMenu'
import { GroupedSessions } from './GroupedSessions'
import { SessionSection } from './Sections'
import { SessionTree, type TreeHandle } from './SessionTree'
import { clearPanelFilters, filterPredicate, filtersActive, setPanelFilters, setPanelQuery, usePanelState } from './state'

/** Recent shows at most this many sessions, and only once the library has at least RECENT_MIN_LIBRARY sessions. */
const RECENT_LIMIT = 5
const RECENT_MIN_LIBRARY = 6

const SORT_MODES: { value: SortMode; label: string }[] = [
  { value: 'manual', label: 'Manual (drag to reorder)' },
  { value: 'name', label: 'Name' },
  { value: 'recent', label: 'Recently used' },
  { value: 'protocol', label: 'Protocol' },
  { value: 'host', label: 'Host' },
]

const GROUP_MODES: { value: GroupBy; label: string }[] = [
  { value: 'folder', label: 'Folders' },
  { value: 'protocol', label: 'Protocol' },
  { value: 'tag', label: 'Tag' },
]

/** Folder a new item should go into, from the tree's focused node. */
function focusedFolderId(index: ReturnType<typeof buildTreeIndex>): string | null {
  const nodeId = useTreeFocus.getState().nodeId
  if (!nodeId) return null
  const p = parseNodeId(nodeId)
  if (p.kind === 'folder') return index.folders.has(p.id) ? p.id : null
  if (p.kind === 'connection') {
    const c = index.connections.get(p.id)
    return c?.folderId && index.folders.has(c.folderId) ? c.folderId : null
  }
  return null
}

function Chip({ active, onClick, icon: Icon, children, label }: { active: boolean; onClick?: () => void; icon: typeof Star; children: string; label?: string }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      aria-label={label}
      onClick={onClick}
      className={cn(
        'inline-flex h-5 items-center gap-1 rounded-full border px-2 text-xs text-muted-foreground outline-none transition-colors',
        'hover:border-ring/50 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50',
        active && 'border-primary/50 bg-primary/15 text-primary',
      )}
    >
      <Icon className="size-3" />
      {children}
    </button>
  )
}

export default function SessionsPanel() {
  const conns = useConnections()
  const folders = useFolders()
  const sessions = useSessions()
  const settings = sessionsSettings.use()
  const user = useCurrentUser()
  const mobile = useIsMobile()
  const query = usePanelState((s) => s.query)
  const filters = usePanelState((s) => s.filters)
  const rowHeight = mobile ? 34 : 26

  const searchRef = useRef<HTMLInputElement>(null)
  const treeRef = useRef<TreeHandle>(null)
  const ctxRef = useRef<SessionMenuContext>({ area: 'root', selection: [], folderIds: [] })
  const pendingReveal = useRef<string | null>(null)
  const [, forceRender] = useState(0)
  const resultsId = useId()
  const [active, setActive] = useState(-1)

  const connections = useMemo(() => conns.data ?? [], [conns.data])
  const folderList = useMemo(() => folders.data ?? [], [folders.data])
  const running = useMemo(() => groupRunningSessions(sessions.data), [sessions.data])
  const grouped = settings.groupBy === 'protocol' || settings.groupBy === 'tag' ? settings.groupBy : null
  const filtering = filtersActive(filters)
  const predicate = useMemo(() => (filtering ? filterPredicate(filters, running) : undefined), [filtering, filters, running])
  const index = useMemo(() => buildTreeIndex(folderList, connections, { sortMode: settings.sortMode, filter: predicate }), [folderList, connections, settings.sortMode, predicate])
  const fullIndex = useMemo(() => (predicate ? buildTreeIndex(folderList, connections, { sortMode: settings.sortMode }) : index), [predicate, folderList, connections, settings.sortMode, index])

  // --- search --------------------------------------------------------------------------------------------------------
  const searching = query.trim().length > 0
  const fuse = useMemo(() => {
    const byId = new Map(folderList.map((f) => [f.id, f]))
    return new Fuse(connections, {
      keys: [
        { name: 'name', weight: 3 },
        { name: 'host', weight: 2 },
        { name: 'username', weight: 1 },
        { name: 'tags', weight: 1.5 },
        { name: 'notes', weight: 0.4 },
        { name: 'protocol', weight: 0.5 },
        { name: 'protocolLabel', weight: 0.5, getFn: (c: Connection) => protocolLabel(c.protocol) },
        { name: 'folder', weight: 0.4, getFn: (c: Connection) => folderPathLabel(byId, c.folderId) },
      ],
      threshold: 0.35,
      ignoreLocation: true,
      includeMatches: true,
      minMatchCharLength: 1,
    })
  }, [connections, folderList])

  const hits: SearchHit[] = useMemo(() => {
    if (!searching) return []
    const byId = new Map(folderList.map((f) => [f.id, f]))
    return fuse
      .search(query.trim(), { limit: 400 })
      .filter((r) => !predicate || predicate(r.item))
      .slice(0, 200)
      .map((r) => ({ conn: r.item, matches: r.matches ?? [], folderPath: folderPathLabel(byId, r.item.folderId) }))
  }, [searching, fuse, query, predicate, folderList])

  useEffect(() => setActive(hits.length ? 0 : -1), [hits])

  const quickConnectQuery = () => {
    const text = query.trim()
    if (!text) return
    void runCommand('sessions.quickConnect', text, { source: 'api' }).then((ok) => {
      if (ok) setPanelQuery('')
    })
  }

  const onSearchKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (searching) setActive((a) => Math.min(hits.length - 1, a + 1))
      else treeRef.current?.focus()
    } else if (e.key === 'ArrowUp') {
      if (searching) {
        e.preventDefault()
        setActive((a) => Math.max(0, a - 1))
      }
    } else if (e.key === 'Enter') {
      if (!searching) return
      e.preventDefault()
      const hit = hits[active >= 0 ? active : 0]
      if (!hit) quickConnectQuery()
      else if (e.altKey) openSessionEditor({ mode: 'edit', connectionId: hit.conn.id })
      else void connectSafely(hit.conn).then((ok) => ok && setPanelQuery(''))
    } else if (e.key === 'Escape') {
      if (query) {
        e.preventDefault()
        e.stopPropagation()
        setPanelQuery('')
      } else {
        treeRef.current?.focus()
      }
    }
  }

  // --- controller ------------------------------------------------------------------------------------------------
  useEffect(
    () =>
      registerTreeController({
        reveal(nodeId) {
          const st = usePanelState.getState()
          if (st.query) setPanelQuery('')
          if (filtersActive(st.filters)) clearPanelFilters()
          pendingReveal.current = nodeId
          forceRender((n) => n + 1)
        },
        startRename(nodeId) {
          if (usePanelState.getState().query) return false
          return treeRef.current?.startRename(nodeId) ?? false
        },
        focusSearch(text) {
          if (text !== undefined) setPanelQuery(text)
          requestAnimationFrame(() => {
            searchRef.current?.focus()
            if (text === undefined) searchRef.current?.select()
          })
        },
        focusTree() {
          treeRef.current?.focus()
        },
        expandAll() {
          treeRef.current?.expandAll()
        },
        collapseAll() {
          treeRef.current?.collapseAll()
        },
      }),
    [],
  )

  // Hand pending reveals to the tree once it is mounted (it is replaced by results while searching).
  useEffect(() => {
    if (pendingReveal.current && treeRef.current) {
      treeRef.current.reveal(pendingReveal.current)
      pendingReveal.current = null
    }
  })

  // --- toolbar actions -------------------------------------------------------------------------------------------
  const newSession = useCommand('sessions.new')
  const importer = useCommand('importer.open')
  const identities = useCommand('keys.identities')
  const favorites = useMemo(
    () => connections.filter((c) => c.favorite).sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base', numeric: true })),
    [connections],
  )
  const recent = useMemo(() => recentConnections(connections, RECENT_LIMIT), [connections])
  // Quick-access sections duplicate rows of the tree by design; in a small library that is just noise (every session is
  // already visible), so Recent only appears once the library is big enough for a shortcut list to help.
  const showFavoritesSection = settings.showFavorites && favorites.length > 0
  const showRecentSection = settings.showRecent && recent.length > 0 && connections.length >= RECENT_MIN_LIBRARY
  const protocolsInUse = useMemo(() => [...new Set(connections.map((c) => c.protocol))].sort(), [connections])
  const tags = useMemo(() => collectTags(connections), [connections])

  const onNewSession = () => openSessionEditor({ mode: 'create', folderId: focusedFolderId(fullIndex) })
  const onNewFolder = () => void openFolderDialog({ mode: 'create', parentId: focusedFolderId(fullIndex) })

  // --- render ----------------------------------------------------------------------------------------------------
  const loading = (conns.isLoading || folders.isLoading) && !conns.data
  const firstLoad = useLoadingGate(loading)
  const loadError = conns.error ?? folders.error
  const empty = !loading && !loadError && connections.length === 0 && folderList.length === 0

  let body: ReactNode
  if (firstLoad.hold) body = firstLoad.show ? <LoadingPane immediate label="Loading sessions…" /> : null
  else if (loadError && !conns.data) {
    body = (
      <EmptyState
        size="sm"
        title="Sessions could not be loaded"
        description={errorMessage(loadError)}
        action={
          <Button
            size="sm"
            variant="secondary"
            onClick={() => {
              void conns.refetch()
              void folders.refetch()
            }}
          >
            Retry
          </Button>
        }
      />
    )
  } else if (searching) {
    body = (
      <SearchResults id={resultsId} hits={hits} query={query.trim()} active={active} onActiveChange={setActive} running={running} rowHeight={rowHeight} onQuickConnect={quickConnectQuery} />
    )
  } else if (empty) {
    body = (
      <EmptyState
        size="sm"
        icon={FolderTree}
        title="No saved sessions"
        description="Save SSH, RDP, VNC, serial, SFTP and other connections here, or use quick connect."
        action={
          <>
            <Button size="sm" onClick={onNewSession} disabled={!newSession.enabled}>
              <Plus /> New session
            </Button>
            {importer.enabled && (
              <Button size="sm" variant="secondary" onClick={() => void importer.run(undefined, 'menu')}>
                <Import /> Import
              </Button>
            )}
          </>
        }
      />
    )
  } else if (filtering && index.counts.get('root') === 0) {
    body = (
      <EmptyState
        size="sm"
        icon={ListFilter}
        title="No sessions match the filters"
        action={
          <Button size="sm" variant="secondary" onClick={clearPanelFilters}>
            Clear filters
          </Button>
        }
      />
    )
  } else {
    body = (
      <div className="flex h-full min-h-0 flex-col">
        {!filtering && showFavoritesSection && (
          <SessionSection
            id="nx-sessions-favorites"
            title="Favorites"
            icon={<Star className="size-3" />}
            connections={favorites}
            open={settings.favoritesOpen}
            onOpenChange={(o) => sessionsSettings.set({ favoritesOpen: o })}
            running={running}
            rowHeight={rowHeight}
          />
        )}
        {!filtering && showRecentSection && (
          <SessionSection
            id="nx-sessions-recent"
            title="Recent"
            icon={<Clock className="size-3" />}
            connections={recent}
            open={settings.recentOpen}
            onOpenChange={(o) => sessionsSettings.set({ recentOpen: o })}
            running={running}
            rowHeight={rowHeight}
            showLastUsed
          />
        )}
        {!filtering && (showFavoritesSection || showRecentSection) && (
          // Label the tree so the quick-access rows above read as shortcuts, not duplicates.
          <div className="flex h-6 shrink-0 items-center gap-1 px-1.5 text-2xs font-semibold tracking-wider text-muted-foreground uppercase" id="nx-sessions-all-title">
            <FolderTree className="ml-4 size-3" aria-hidden />
            <span>{grouped === 'protocol' ? 'All sessions by protocol' : grouped === 'tag' ? 'All sessions by tag' : 'All sessions'}</span>
            <span className="ml-auto font-normal tabular-nums">{connections.length}</span>
          </div>
        )}
        <div className="min-h-0 flex-1">
          {grouped ? (
            <GroupedSessions
              connections={predicate ? connections.filter(predicate) : connections}
              by={grouped}
              sortMode={settings.sortMode}
              running={running}
              rowHeight={rowHeight}
            />
          ) : (
            <SessionTree
              handleRef={treeRef}
              index={index}
              running={running}
              user={user}
              filtered={filtering}
              manualOrder={settings.sortMode === 'manual'}
              expandedFolders={settings.expanded}
              onExpandedChange={(expanded) => sessionsSettings.set({ expanded })}
              rowHeight={rowHeight}
              onContextTarget={(ctx) => {
                ctxRef.current = ctx
              }}
              onTypeToSearch={(text) => {
                setPanelQuery(text)
                requestAnimationFrame(() => searchRef.current?.focus())
              }}
            />
          )}
        </div>
      </div>
    )
  }

  return (
    <div className="@container flex h-full min-h-0 flex-col">
      <div role="toolbar" aria-label="Session actions" className="flex h-8 shrink-0 items-center gap-0.5 border-b px-1">
        <IconButton icon={Plus} label="New session" shortcut={newSession.keybindings[0]} size="xs" onClick={onNewSession} disabled={!newSession.enabled} />
        <IconButton icon={FolderPlus} label="New folder" size="xs" onClick={onNewFolder} />
        <span className="mx-0.5 h-4 w-px bg-border" aria-hidden />
        <IconButton icon={ChevronsUpDown} label="Expand all" size="xs" onClick={() => treeRef.current?.expandAll()} disabled={searching || !!grouped} />
        <IconButton icon={ChevronsDownUp} label="Collapse all" size="xs" onClick={() => treeRef.current?.collapseAll()} disabled={searching || !!grouped} />
        <DropdownMenu>
          <Tooltip content="Sort and group">
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-xs"
                aria-label="Sort and group sessions"
                className={cn('text-muted-foreground hover:text-foreground', (settings.sortMode !== 'manual' || grouped) && 'text-primary')}
              >
                <ArrowDownUp />
              </Button>
            </DropdownMenuTrigger>
          </Tooltip>
          <DropdownMenuContent align="start">
            <DropdownMenuLabel>Sort sessions by</DropdownMenuLabel>
            <DropdownMenuRadioGroup value={settings.sortMode} onValueChange={(v) => sessionsSettings.set({ sortMode: v as SortMode })}>
              {SORT_MODES.map((m) => (
                <DropdownMenuRadioItem key={m.value} value={m.value}>
                  {m.label}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
            <DropdownMenuSeparator />
            <DropdownMenuLabel>Group by</DropdownMenuLabel>
            <DropdownMenuRadioGroup value={settings.groupBy} onValueChange={(v) => sessionsSettings.set({ groupBy: v as GroupBy })}>
              {GROUP_MODES.map((m) => (
                <DropdownMenuRadioItem key={m.value} value={m.value}>
                  {m.label}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
        <IconButton
          icon={ListFilter}
          label={settings.showFilters ? 'Hide filters' : 'Show filters'}
          size="xs"
          active={settings.showFilters || filtering}
          onClick={() => sessionsSettings.set({ showFilters: !settings.showFilters })}
        />
        <span className="flex-1" />
        <DropdownMenu>
          <Tooltip content="More">
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-xs" aria-label="More session actions" className="text-muted-foreground hover:text-foreground">
                <Ellipsis />
              </Button>
            </DropdownMenuTrigger>
          </Tooltip>
          <DropdownMenuContent align="end">
            <DropdownMenuItem disabled={!importer.enabled} onSelect={() => void importer.run(undefined, 'menu')}>
              <Import /> Import / export…
            </DropdownMenuItem>
            <DropdownMenuItem disabled={!identities.enabled} onSelect={() => void identities.run(undefined, 'menu')}>
              <UserRoundCog /> Manage identities…
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuCheckboxItem checked={settings.showFavorites} onCheckedChange={(v) => sessionsSettings.set({ showFavorites: v === true })}>
              Show favorites
            </DropdownMenuCheckboxItem>
            <DropdownMenuCheckboxItem checked={settings.showRecent} onCheckedChange={(v) => sessionsSettings.set({ showRecent: v === true })}>
              Show recent
              <span className="ml-auto pl-3 text-2xs text-muted-foreground">{RECENT_MIN_LIBRARY}+ sessions</span>
            </DropdownMenuCheckboxItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => void runCommand('settings.open', { section: 'sessions' }, { source: 'menu' })}>Session settings…</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <div className="shrink-0 px-2 pt-1.5 pb-1.5">
        <Input
          ref={searchRef}
          inputSize="sm"
          leading={<Search />}
          trailing={
            query ? (
              <button
                type="button"
                aria-label="Clear search"
                className="flex size-5 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
                onClick={() => {
                  setPanelQuery('')
                  searchRef.current?.focus()
                }}
              >
                <X className="size-3" />
              </button>
            ) : undefined
          }
          value={query}
          onChange={(e) => setPanelQuery(e.target.value)}
          onKeyDown={onSearchKeyDown}
          placeholder="Search sessions"
          aria-label="Search sessions"
          role="combobox"
          aria-expanded={searching}
          aria-controls={searching ? resultsId : undefined}
          aria-activedescendant={searching && active >= 0 ? `${resultsId}-${active}` : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          spellCheck={false}
        />
      </div>

      {(settings.showFilters || filtering) && (
        <div className="flex shrink-0 flex-wrap items-center gap-1 px-2 pb-1.5" role="group" aria-label="Filters">
          <Chip active={filters.favorites} icon={Star} onClick={() => setPanelFilters({ favorites: !filters.favorites })}>
            Favorites
          </Chip>
          <Chip active={filters.running} icon={Activity} onClick={() => setPanelFilters({ running: !filters.running })}>
            Running
          </Chip>
          <Chip active={filters.shared} icon={Users} onClick={() => setPanelFilters({ shared: !filters.shared })}>
            Shared
          </Chip>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <button
                type="button"
                className={cn(
                  'inline-flex h-5 items-center gap-1 rounded-full border px-2 text-xs text-muted-foreground outline-none hover:border-ring/50 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50',
                  filters.protocols.length > 0 && 'border-primary/50 bg-primary/15 text-primary',
                )}
              >
                <Zap className="size-3" />
                {filters.protocols.length ? filters.protocols.map(protocolLabel).join(', ') : 'Protocol'}
              </button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {protocolsInUse.length === 0 && <DropdownMenuLabel>No sessions yet</DropdownMenuLabel>}
              {protocolsInUse.map((p) => {
                const Icon = protocolIcon(p)
                const on = filters.protocols.includes(p)
                return (
                  <DropdownMenuCheckboxItem
                    key={p}
                    checked={on}
                    onSelect={(e) => e.preventDefault()}
                    onCheckedChange={() => setPanelFilters({ protocols: on ? filters.protocols.filter((x) => x !== p) : [...filters.protocols, p] })}
                  >
                    <Icon className="size-3.5" /> {protocolLabel(p)}
                  </DropdownMenuCheckboxItem>
                )
              })}
            </DropdownMenuContent>
          </DropdownMenu>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <button
                type="button"
                className={cn(
                  'inline-flex h-5 max-w-40 items-center gap-1 rounded-full border px-2 text-xs text-muted-foreground outline-none hover:border-ring/50 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50',
                  filters.tags.length > 0 && 'border-primary/50 bg-primary/15 text-primary',
                )}
              >
                <Tag className="size-3 shrink-0" />
                <span className="truncate">{filters.tags.length ? filters.tags.join(', ') : 'Tag'}</span>
              </button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="max-w-64">
              {tags.length === 0 && <DropdownMenuLabel>No tags yet</DropdownMenuLabel>}
              {tags.map((t) => {
                const on = filters.tags.some((x) => x.toLowerCase() === t.toLowerCase())
                return (
                  <DropdownMenuCheckboxItem
                    key={t}
                    checked={on}
                    onSelect={(e) => e.preventDefault()}
                    onCheckedChange={() =>
                      setPanelFilters({ tags: on ? filters.tags.filter((x) => x.toLowerCase() !== t.toLowerCase()) : [...filters.tags, t] })
                    }
                  >
                    <span className="truncate">{t}</span>
                  </DropdownMenuCheckboxItem>
                )
              })}
            </DropdownMenuContent>
          </DropdownMenu>
          {filtering && (
            <button type="button" onClick={clearPanelFilters} className="inline-flex h-5 items-center gap-0.5 px-1 text-xs text-muted-foreground hover:text-foreground">
              <X className="size-3" /> Clear
            </button>
          )}
        </div>
      )}

      <SessionContextMenu items={() => getContextMenuItems('session-node', ctxRef.current)}>
        <div
          className="min-h-0 flex-1 border-t"
          onContextMenu={(e) => {
            const el = e.target as Element
            const connEl = el.closest<HTMLElement>('[data-conn-id]')
            if (connEl?.dataset.connId) {
              const c = connections.find((x) => x.id === connEl.dataset.connId)
              ctxRef.current = c ? { connection: c, selection: [c.id], folderIds: [] } : { selection: [], folderIds: [] }
            } else if (!el.closest('[data-node-id]')) {
              ctxRef.current = { area: 'root', selection: [], folderIds: [] }
            }
          }}
        >
          {body}
        </div>
      </SessionContextMenu>
    </div>
  )
}
