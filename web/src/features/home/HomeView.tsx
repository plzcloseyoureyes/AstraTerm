import { useMemo, type ReactNode } from 'react'
import {
  ArrowRight,
  Bookmark,
  Clapperboard,
  Folder as FolderIcon,
  FolderOpen,
  FolderPlus,
  Import,
  KeyRound,
  LayoutDashboard,
  Lightbulb,
  PanelRight,
  Pencil,
  Plus,
  Search,
  Server,
  Settings,
  SquareTerminal,
  Star,
  Tag,
  Waypoints,
  Wrench,
  X,
  Zap,
  type LucideIcon,
} from 'lucide-react'
import { commands, type TabProps } from '@/app/registry'
import { getKeybindings, runCommand, useCommand } from '@/app/commands'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { useConnections } from '@/api/connections'
import { useFolders } from '@/api/folders'
import { isSessionRunning, useSessions } from '@/api/sessions'
import type { Connection, Folder, RuntimeSession, SessionState } from '@/api/types'
import { LoadingState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { statusDotClass } from '@/components/ui/status-dot'
import { QuickConnect, useToolbarQuickConnect } from '@/layout/QuickConnect'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Kbd } from '@/components/ui/kbd'
import { Tooltip } from '@/components/ui/tooltip'
import { useNow } from '@/lib/hooks'
import { cn, formatDuration, formatRelativeTime } from '@/lib/utils'
import { useCurrentUser, useVaultLocked } from '@/stores/auth'
import type { SavedWorkspace } from '@/stores/settings'
import { openPalette } from '@/stores/ui'
import { focusTab, listTabs, useSavedWorkspaces } from '@/stores/workspace'

function greeting(): string {
  const h = new Date().getHours()
  if (h < 5) return 'Working late'
  if (h < 12) return 'Good morning'
  if (h < 18) return 'Good afternoon'
  return 'Good evening'
}

const STATE_STYLE: Record<SessionState, { dot: string; label: string }> = {
  connecting: { dot: statusDotClass('warning', true), label: 'Connecting' },
  authenticating: { dot: statusDotClass('warning', true), label: 'Authenticating' },
  connected: { dot: 'bg-success', label: 'Connected' },
  disconnected: { dot: 'bg-muted-foreground/60', label: 'Disconnected' },
  closed: { dot: 'bg-muted-foreground/40', label: 'Closed' },
  error: { dot: 'bg-destructive', label: 'Error' },
}

/** Focus the tab showing a session, or ask the owning feature to attach it. */
function openRunningSession(s: RuntimeSession) {
  const tab = listTabs().find((t) => (t.params as { sessionId?: string } | undefined)?.sessionId === s.id)
  if (tab) {
    focusTab(tab.id)
    return
  }
  const specific = `${s.kind}.attach`
  const id = s.kind !== 'terminal' && commands.get(specific) ? specific : 'terminal.attach'
  void runCommand(id, { sessionId: s.id }, { source: 'home' })
}

function ActionCard({
  icon: Icon,
  title,
  description,
  command,
  args,
  accent,
}: {
  icon: LucideIcon
  title: string
  description: string
  command: string
  args?: unknown
  accent: string
}) {
  const cmd = useCommand(command)
  const shortcut = cmd.keybindings[0]
  const card = (
    <button
      type="button"
      disabled={!cmd.enabled}
      onClick={() => void runCommand(command, args, { source: 'home' })}
      className={cn(
        // Borderless surface (the transparent border still outlines it in forced-colors / high-contrast mode).
        'group flex items-center gap-3 rounded-lg border border-transparent bg-card px-3.5 py-3 text-left outline-none transition-colors duration-150',
        'hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring/60',
        'disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-card',
      )}
    >
      <span className={cn('flex size-8 shrink-0 items-center justify-center rounded-md', accent)}>
        <Icon className="size-4" />
      </span>
      <span className="grid min-w-0 flex-1 gap-0.5">
        <span className="flex items-center gap-2 font-medium">
          <span className="truncate">{title}</span>
          {shortcut && <Kbd keys={shortcut} className="ml-auto hidden @6xl:inline-flex" />}
        </span>
        <span className="truncate text-sm text-muted-foreground">{description}</span>
      </span>
    </button>
  )
  return cmd.enabled ? card : <Tooltip content="Not available in this build">{<span className="grid">{card}</span>}</Tooltip>
}

function SectionCard({ title, action, children, className }: { title: ReactNode; action?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cn('flex min-w-0 flex-col rounded-lg border border-transparent bg-card', className)}>
      <header className="flex h-10 shrink-0 items-center justify-between gap-2 border-b border-border/50 px-3.5">
        <h2 className="flex items-center gap-2 text-sm font-semibold">{title}</h2>
        {action}
      </header>
      <div className="min-h-0 flex-1">{children}</div>
    </section>
  )
}

/** Row with a primary action (the whole row) and quick secondary actions revealed on hover / keyboard focus. */
function ActionRow({ onOpen, disabled, children, actions }: { onOpen: () => void; disabled?: boolean; children: ReactNode; actions?: ReactNode }) {
  return (
    <li className="group/row relative flex items-center">
      <button
        type="button"
        disabled={disabled}
        onClick={onOpen}
        className="flex min-w-0 flex-1 items-center gap-3 px-3.5 py-2 text-left outline-none transition-colors duration-150 hover:bg-accent/50 focus-visible:bg-accent/60 disabled:opacity-60"
      >
        {children}
      </button>
      {actions && (
        // Always laid out (no shift); only their opacity changes.
        <span className="absolute right-2 flex items-center gap-0.5 rounded-md bg-card opacity-0 shadow-xs transition-opacity duration-150 group-focus-within/row:opacity-100 group-hover/row:opacity-100">
          {actions}
        </span>
      )}
    </li>
  )
}

function RecentSessions() {
  const { data, isLoading } = useConnections()
  const connectEnabled = useCommand('sessions.connect').enabled
  const newSession = useCommand('sessions.new')
  const filesEnabled = useCommand('files.openForConnection').enabled
  const items = useMemo(() => {
    const list = data ?? []
    const used = list.filter((c) => c.lastUsedAt).sort((a, b) => Date.parse(b.lastUsedAt!) - Date.parse(a.lastUsedAt!))
    const favs = list.filter((c) => c.favorite && !c.lastUsedAt)
    return [...used, ...favs].slice(0, 8)
  }, [data])

  return (
    <SectionCard
      title="Recent sessions"
      action={
        data && data.length > 0 ? (
          <Button variant="ghost" size="xs" onClick={() => openPalette('connections')}>
            All sessions <ArrowRight />
          </Button>
        ) : undefined
      }
    >
      <LoadingState busy={isLoading} skeleton={<SkeletonRows rows={4} rowHeight={44} className="p-2" />}>
        {items.length === 0 ? (
          <EmptyState
            size="sm"
            icon={SquareTerminal}
            title={data?.length ? 'No recent sessions yet' : 'No saved sessions yet'}
            description={data?.length ? 'Sessions you open appear here.' : 'Save SSH, RDP, VNC, SFTP and serial connections — or bring them in from another tool.'}
            action={
              <>
                {newSession.enabled && (
                  <Button size="sm" onClick={() => void newSession.run(undefined, 'home')}>
                    <Plus /> New session
                  </Button>
                )}
                {!data?.length && (
                  <Button size="sm" variant="secondary" onClick={() => void runCommand('importer.open', undefined, { source: 'home' })}>
                    <Import /> Import
                  </Button>
                )}
              </>
            }
          />
        ) : (
          <ul className="divide-y" role="list">
            {items.map((c) => (
              <RecentRow key={c.id} conn={c} disabled={!connectEnabled} files={filesEnabled} />
            ))}
          </ul>
        )}
      </LoadingState>
    </SectionCard>
  )
}

function RecentRow({ conn, disabled, files }: { conn: Connection; disabled: boolean; files: boolean }) {
  const Icon = protocolIcon(conn.protocol)
  const target = `${conn.username ? `${conn.username}@` : ''}${conn.host}${conn.port ? `:${conn.port}` : ''}`
  const fileCapable = conn.protocol === 'ssh' || conn.protocol === 'sftp' || conn.protocol === 'ftp' || conn.protocol === 's3'
  return (
    <ActionRow
      disabled={disabled}
      onOpen={() => void runCommand('sessions.connect', { id: conn.id }, { source: 'home' })}
      actions={
        <>
          <IconButton icon={PanelRight} label="Open to the right" size="xs" onClick={() => void runCommand('sessions.connect', { id: conn.id, position: 'right' }, { source: 'home' })} />
          {files && fileCapable && (
            <IconButton icon={FolderOpen} label="Browse files" size="xs" onClick={() => void runCommand('files.openForConnection', { connectionId: conn.id }, { source: 'home' })} />
          )}
          <IconButton icon={Pencil} label="Edit session" size="xs" onClick={() => void runCommand('sessions.edit', { id: conn.id }, { source: 'home' })} />
        </>
      }
    >
      <span
        className="flex size-7 shrink-0 items-center justify-center rounded-md border bg-muted/50 text-muted-foreground"
        style={conn.color ? { color: conn.color, borderColor: `color-mix(in oklab, ${conn.color} 40%, transparent)` } : undefined}
      >
        <Icon className="size-3.5" />
      </span>
      <span className="grid min-w-0 flex-1">
        <span className="flex items-center gap-1.5 truncate font-medium">
          {conn.name}
          {conn.favorite && <Star className="size-3 fill-warning text-warning" aria-label="Favorite" />}
        </span>
        <span className="truncate font-mono text-xs text-muted-foreground">{target || protocolLabel(conn.protocol)}</span>
      </span>
      <span className="hidden shrink-0 text-xs text-muted-foreground @lg:block">{conn.lastUsedAt ? formatRelativeTime(conn.lastUsedAt) : 'favorite'}</span>
      <Badge variant="outline" className="hidden shrink-0 @2xl:inline-flex">
        {protocolLabel(conn.protocol)}
      </Badge>
    </ActionRow>
  )
}

function RunningSessions() {
  const { data, isLoading } = useSessions()
  const now = useNow(15_000)
  const running = (data ?? []).filter(isSessionRunning).sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
  return (
    <SectionCard
      title={
        <>
          Running now
          {running.length > 0 && <Badge variant="default">{running.length}</Badge>}
        </>
      }
    >
      <LoadingState busy={isLoading} skeleton={<SkeletonRows rows={2} rowHeight={44} className="p-2" />}>
        {running.length === 0 ? (
          <EmptyState size="sm" icon={Zap} title="Nothing running" description="Sessions keep running on the server even when you close the browser." />
        ) : (
          <ul className="divide-y" role="list">
            {running.slice(0, 8).map((s) => {
              const Icon = protocolIcon(s.protocol)
              const st = STATE_STYLE[s.state] ?? STATE_STYLE.disconnected
              const since = s.connectedAt ?? s.createdAt
              return (
                <ActionRow key={s.id} onOpen={() => openRunningSession(s)}>
                  <span className="relative flex size-7 shrink-0 items-center justify-center rounded-md border bg-muted/50 text-muted-foreground">
                    <Icon className="size-3.5" />
                    <span className={cn('absolute -right-0.5 -bottom-0.5 size-2 rounded-full ring-2 ring-card', st.dot)} />
                  </span>
                  <span className="grid min-w-0 flex-1">
                    <span className="truncate font-medium">{s.title || s.host || protocolLabel(s.protocol)}</span>
                    <span className="truncate text-xs text-muted-foreground">
                      {st.label}
                      {s.stateMessage ? ` — ${s.stateMessage}` : ''}
                    </span>
                  </span>
                  <span className="shrink-0 text-right text-xs text-muted-foreground tabular">
                    {since ? formatDuration(now - Date.parse(since)) : ''}
                    {s.clients > 0 && <span className="block">{s.clients === 1 ? '1 viewer' : `${s.clients} viewers`}</span>}
                  </span>
                </ActionRow>
              )
            })}
          </ul>
        )}
      </LoadingState>
    </SectionCard>
  )
}

/** Saved workspaces (named layouts): one click brings a whole working set of tabs back. */
function Workspaces() {
  const saved = useSavedWorkspaces()
  const save = useCommand('workspace.save')
  return (
    <SectionCard
      title="Workspaces"
      action={
        <Button variant="ghost" size="xs" disabled={!save.enabled} onClick={() => void save.run(undefined, 'home')}>
          <Bookmark /> Save current
        </Button>
      }
    >
      {saved.length === 0 ? (
        <EmptyState
          size="sm"
          icon={LayoutDashboard}
          title="No saved workspaces"
          description="Arrange the tabs you work with — splits included — and save them. Restoring reconnects every session."
        />
      ) : (
        <ul className="divide-y" role="list">
          {saved.slice(0, 6).map((w) => (
            <WorkspaceRow key={w.id} w={w} />
          ))}
        </ul>
      )}
    </SectionCard>
  )
}

function WorkspaceRow({ w }: { w: SavedWorkspace }) {
  return (
    <ActionRow
      onOpen={() => void runCommand('workspace.restore', { id: w.id }, { source: 'home' })}
      actions={<IconButton icon={X} label={`Delete “${w.name}”`} size="xs" onClick={() => void runCommand('workspace.delete', { id: w.id }, { source: 'home' })} />}
    >
      <span className="flex size-7 shrink-0 items-center justify-center rounded-md border bg-primary/10 text-primary">
        <LayoutDashboard className="size-3.5" />
      </span>
      <span className="grid min-w-0 flex-1">
        <span className="truncate font-medium">{w.name}</span>
        <span className="truncate text-xs text-muted-foreground">
          {w.tabs.length} tab{w.tabs.length === 1 ? '' : 's'} · {w.tabs.map((t) => t.title).join(', ')}
        </span>
      </span>
      <span className="hidden shrink-0 text-xs text-muted-foreground @lg:block">{formatRelativeTime(w.savedAt)}</span>
    </ActionRow>
  )
}

/** Folders and tags at a glance: the session library as the user organized it. */
function Organize() {
  const conns = useConnections()
  const folders = useFolders()
  const newFolder = useCommand('sessions.newFolder')
  const stats = useMemo(() => {
    const list = conns.data ?? []
    const perFolder = new Map<string, number>()
    const tags = new Map<string, number>()
    for (const c of list) {
      if (c.folderId) perFolder.set(c.folderId, (perFolder.get(c.folderId) ?? 0) + 1)
      for (const t of c.tags ?? []) tags.set(t, (tags.get(t) ?? 0) + 1)
    }
    const top = (folders.data ?? []).filter((f) => !f.parentId).sort((a, b) => a.sortOrder - b.sortOrder || a.name.localeCompare(b.name))
    return {
      folders: top.slice(0, 8).map((f) => ({ folder: f, count: perFolder.get(f.id) ?? 0 })),
      tags: [...tags.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, 14),
      favorites: list.filter((c) => c.favorite).length,
      total: list.length,
    }
  }, [conns.data, folders.data])

  if (!stats.total && !stats.folders.length) return null
  return (
    <SectionCard
      title="Organize"
      action={
        newFolder.enabled ? (
          <Button variant="ghost" size="xs" onClick={() => void newFolder.run(undefined, 'home')}>
            <FolderPlus /> New folder
          </Button>
        ) : undefined
      }
    >
      <div className="grid gap-3 p-3.5">
        {stats.folders.length > 0 && (
          <div className="grid gap-2 @xl:grid-cols-2 @4xl:grid-cols-4">
            {stats.folders.map(({ folder, count }) => (
              <FolderTile key={folder.id} folder={folder} count={count} />
            ))}
          </div>
        )}
        {stats.tags.length > 0 && (
          <div className="flex flex-wrap items-center gap-1.5">
            <Tag className="size-3.5 text-muted-foreground" aria-hidden />
            {stats.tags.map(([t, n]) => (
              <button
                key={t}
                type="button"
                onClick={() => void runCommand('sessions.filterByTag', { tag: t }, { source: 'home' })}
                className="inline-flex h-6 items-center gap-1 rounded-full border bg-muted/40 px-2 text-xs outline-none transition-colors duration-150 hover:border-primary/40 hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60"
              >
                {t}
                <span className="text-muted-foreground tabular">{n}</span>
              </button>
            ))}
          </div>
        )}
        {!stats.tags.length && stats.total > 0 && (
          <p className="text-sm text-muted-foreground">Tip: add tags in a session&apos;s settings to filter the session tree by them.</p>
        )}
      </div>
    </SectionCard>
  )
}

function FolderTile({ folder, count }: { folder: Folder; count: number }) {
  return (
    <button
      type="button"
      onClick={() => void runCommand('sessions.revealFolder', { folderId: folder.id }, { source: 'home' })}
      className="flex min-w-0 items-center gap-2 rounded-md border border-transparent bg-card px-2.5 py-2 text-left outline-none transition-colors duration-150 hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring/60"
    >
      <FolderIcon className="size-4 shrink-0 text-muted-foreground" style={folder.color ? { color: folder.color } : undefined} />
      <span className="min-w-0 flex-1 truncate font-medium">{folder.name}</span>
      <span className="shrink-0 text-xs text-muted-foreground tabular">{count}</span>
    </button>
  )
}

const TILES: { icon: LucideIcon; title: string; description: string; command: string }[] = [
  { icon: Waypoints, title: 'Tunnels', description: 'Local, remote and SOCKS port forwarding', command: 'tunnels.open' },
  { icon: KeyRound, title: 'SSH keys', description: 'Generate, import and deploy keys', command: 'keys.open' },
  { icon: Wrench, title: 'Network tools', description: 'Ping, traceroute, port scan, DNS, WoL…', command: 'tools.open' },
  { icon: Server, title: 'Servers', description: 'Built-in HTTP, TFTP, SFTP and FTP servers', command: 'servers.open' },
  { icon: Clapperboard, title: 'Recordings', description: 'Replay and audit recorded sessions', command: 'recordings.open' },
  { icon: Settings, title: 'Settings', description: 'Appearance, shortcuts and security', command: 'settings.open' },
]

function FeatureTile({ icon: Icon, title, description, command }: (typeof TILES)[number]) {
  const cmd = useCommand(command)
  return (
    <button
      type="button"
      disabled={!cmd.enabled}
      onClick={() => void cmd.run(undefined, 'home')}
      className={cn(
        'group flex items-center gap-3 rounded-lg border border-transparent bg-card px-3.5 py-3 text-left outline-none transition-colors duration-150',
        'hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring/60',
        'disabled:cursor-not-allowed disabled:opacity-45 disabled:hover:bg-card',
      )}
      title={cmd.enabled ? undefined : 'Not available in this build'}
    >
      <Icon className="size-5 shrink-0 text-muted-foreground transition-colors duration-150 group-hover:text-primary" strokeWidth={1.75} />
      <span className="grid min-w-0">
        <span className="font-medium">{title}</span>
        <span className="truncate text-xs text-muted-foreground">{description}</span>
      </span>
    </button>
  )
}

function Tips() {
  const tips: { text: string; command: string }[] = [
    { text: 'Search commands, saved sessions and open tabs', command: 'palette.open' },
    { text: 'Jump to quick connect', command: 'quickConnect.focus' },
    { text: 'Split the current tab to the right', command: 'workspace.split.right' },
    { text: 'Reopen the tab you just closed', command: 'workspace.reopenClosed' },
    { text: 'Lock the screen when you step away', command: 'app.lock' },
  ]
  return (
    <section className="rounded-lg bg-muted/30 p-4">
      <h2 className="mb-3 flex items-center gap-2 text-sm font-semibold">
        <Lightbulb className="size-4 text-warning" /> Tips
      </h2>
      <ul className="grid gap-2 @2xl:grid-cols-2" role="list">
        {tips.map((t) => {
          const key = getKeybindings(t.command)[0]
          if (!key) return null
          return (
            <li key={t.command} className="flex items-center justify-between gap-3 text-sm text-muted-foreground">
              <span>{t.text}</span>
              <Kbd keys={key} />
            </li>
          )
        })}
        <li className="text-sm text-muted-foreground @2xl:col-span-2">
          Closing a session&apos;s tab can be undone for a few seconds. Middle-click a tab to close it, double-click to rename it, drag
          tabs to split or float them, and save arrangements you reuse as workspaces.
        </li>
      </ul>
    </section>
  )
}

/** Home tab / empty-workspace view (UI-7): launcher, what is running, the organized session library, workspaces. */
export default function HomeView(_props: TabProps<{ watermark?: boolean }>) {
  const user = useCurrentUser()
  const vaultLocked = useVaultLocked()
  commands.useList() // re-render when features register commands
  const name = user?.displayName?.split(' ')[0] || user?.username || ''
  // One quick-connect field on screen: the toolbar's when it shows one, else a large one here.
  const toolbarQuickConnect = useToolbarQuickConnect()

  return (
    <div className="@container h-full overflow-y-auto">
      <div className="mx-auto grid max-w-6xl gap-5 px-4 py-6 @2xl:px-6 @4xl:py-8">
        <section className="grid gap-4">
          <div className="grid gap-1">
            <h1 className="text-2xl font-semibold tracking-tight">
              {greeting()}
              {name ? `, ${name}` : ''}
            </h1>
            <p className="text-base text-muted-foreground">
              Your sessions, files and tools, organized in one workspace.
              {vaultLocked && (
                <button
                  type="button"
                  className="ml-2 text-warning underline-offset-4 hover:underline"
                  onClick={() => void runCommand('vault.unlock', undefined, { source: 'home' })}
                >
                  Vault is locked — unlock
                </button>
              )}
            </p>
          </div>
          {!toolbarQuickConnect && <QuickConnect size="lg" className="w-full max-w-2xl" />}
          <div className={cn('grid gap-3 @xl:grid-cols-2', toolbarQuickConnect ? '@3xl:grid-cols-3 @6xl:grid-cols-5' : '@5xl:grid-cols-4')}>
            {toolbarQuickConnect && (
              <ActionCard
                icon={Zap}
                title="Quick connect"
                description="A host or a saved session, in the toolbar"
                command="quickConnect.focus"
                accent="bg-primary text-primary-foreground"
              />
            )}
            <ActionCard icon={Plus} title="New session" description="SSH, RDP, VNC, SFTP, serial…" command="sessions.new" accent="bg-primary/15 text-primary" />
            <ActionCard
              icon={SquareTerminal}
              title="Local terminal"
              description="A shell on this machine"
              command="terminal.newLocal"
              accent="bg-success/15 text-success"
            />
            <ActionCard icon={FolderOpen} title="Browse files" description="Local files and transfers" command="files.openLocal" accent="bg-warning/15 text-warning" />
            <ActionCard icon={Search} title="Command palette" description="Find any action or session" command="palette.open" accent="bg-info/15 text-info" />
          </div>
        </section>

        <div className="grid gap-4 @4xl:grid-cols-[3fr_2fr]">
          <RecentSessions />
          <div className="grid content-start gap-4">
            <RunningSessions />
            <Workspaces />
          </div>
        </div>

        <Organize />

        <section className="grid gap-3">
          <h2 className="text-sm font-semibold text-muted-foreground">Tools & features</h2>
          <div className="grid gap-3 @xl:grid-cols-2 @4xl:grid-cols-3">
            {TILES.map((t) => (
              <FeatureTile key={t.command} {...t} />
            ))}
          </div>
        </section>

        <Tips />
      </div>
    </div>
  )
}
