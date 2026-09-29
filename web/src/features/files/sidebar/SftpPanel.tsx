/*
 * The "SFTP" sidebar panel — the SFTP side panel that follows the terminal (FILE-1, FILE-2, GFX-16): the files of the SSH session in the
 * active terminal tab, next to the terminal, over the terminal's own SSH connection (no second login). Switching tabs
 * switches the browser (each session keeps its folder); "Follow terminal folder" tracks the shell's cwd; drop files
 * from the OS to upload; double-click edits with automatic upload on save.
 */
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  CircleAlert,
  FolderSync,
  HardDrive,
  Info,
  Maximize2,
  Monitor,
  Plus,
  RefreshCw,
  Settings2,
  SquareTerminal,
  Unplug,
} from 'lucide-react'
import { useConnections } from '@/api/connections'
import { reconnectSession, useSessions } from '@/api/sessions'
import type { Connection, ConnectionOptions, RuntimeSession } from '@/api/types'
import { runCommand, useCommand } from '@/app/commands'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import type { TabInfo } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Spinner } from '@/components/ui/spinner'
import { statusDotClass } from '@/components/ui/status-dot'
import { Tooltip } from '@/components/ui/tooltip'
import { useMonitorBarEnabled, useMonitorBarOverride } from '@/features/monitor/settings'
import { getTerminalByTab, useTerminalInfo } from '@/features/terminal/bus'
import type { QuickSpec, TerminalTabParams } from '@/features/terminal/types'
import { useDelayedFlag, useLoadingGate } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { focusTab, useWorkspaceStore } from '@/stores/workspace'
import { fsApi, isBrowserDisabled, isDisconnected } from '../api'
import { FileBrowser } from '../browser/FileBrowser'
import { getController } from '../browser/controller'
import type { ToolAction } from '../browser/Toolbar'
import { getView, patchView } from '../browser/viewStore'
import { useFs, useFsStore, sourceKey } from '../fsHandles'
import { openFilesTab } from '../open'
import { filesSettings } from '../settings'
import type { FsContext, FsSource } from '../types'
import { followDefault, lastFollowedCwd, setCompanion, setFollow, usePanelState } from './state'

const LIVE = new Set(['connecting', 'authenticating', 'connected'])

export default function SftpPanel() {
  const tabId = usePanelState((s) => s.tabId)
  const tab = useWorkspaceStore((s) => (tabId ? s.tabs.find((t) => t.id === tabId) : undefined))
  const sessions = useSessions()
  const conns = useConnections()

  if (!tab) return <NoSession />
  const params = (tab.params ?? {}) as TerminalTabParams
  const session = sessions.data?.find((s) => s.id === params.sessionId)
  const protocol = session?.protocol ?? params.protocol
  const connection = conns.data?.find((c) => c.id === (session?.connectionId ?? params.connectionId))

  if (tab.kind === 'vnc' || tab.kind === 'rdp') return <CompanionPanel key={tab.id} tab={tab} connection={connection} connections={conns.data ?? []} />
  if (protocol !== 'ssh' || !params.sessionId) return <NotSsh tab={tab} protocol={protocol} />
  return <SessionPanel key={params.sessionId} tab={tab} sessionId={params.sessionId} session={session} connection={connection} quick={params.quick} />
}

// ---------------------------------------------------------------------------------------------------------------------
// SSH session
// ---------------------------------------------------------------------------------------------------------------------

function SessionPanel({
  tab,
  sessionId,
  session,
  connection,
  quick,
}: {
  tab: TabInfo
  sessionId: string
  session: RuntimeSession | undefined
  connection: Connection | undefined
  quick: QuickSpec | undefined
}) {
  const info = useTerminalInfo(tab.id)
  const opts: ConnectionOptions = connection?.options ?? quick?.options ?? {}
  const browserType = typeof opts.sshBrowser === 'string' ? opts.sshBrowser : 'sftp'
  const state = info?.state === 'gone' ? 'gone' : (session?.state ?? info?.state ?? 'unknown')
  const connected = state === 'connected'
  const source = useMemo<FsSource>(() => ({ kind: 'session', sessionId }), [sessionId])
  const key = sourceKey(source)
  const hasEntry = useFsStore((s) => !!s.entries[key])
  const fs = useFs(source, browserType !== 'none' && (connected || hasEntry))
  const host = session?.host ?? connection?.host ?? ''
  const username = session?.username ?? connection?.username ?? ''
  const label = connection?.name || (host ? `${username ? `${username}@` : ''}${host}` : session?.title || 'SSH session')

  const ctx = useMemo<FsContext | null>(() => {
    if (!fs.handle || !fs.key) return null
    return {
      key: fs.key,
      handle: fs.handle,
      source,
      sessionId,
      placeKey: connection ? `conn:${connection.id}` : `host:${username}@${host.toLowerCase()}:${quick?.port ?? 22}`,
      label,
      host: { host, port: connection?.port ?? quick?.port, username },
    }
  }, [fs.handle, fs.key, source, sessionId, connection, quick, username, host, label])

  // --- follow terminal folder (FILE-2) --------------------------------------------------------------------------------
  const followSetting = filesSettings.useValue('followTerminal')
  // Why the backend did not set up folder reporting at login (it follows the same option / setting).
  const reportingOff: CwdHintReason | undefined =
    typeof opts.remoteCommand === 'string' && opts.remoteCommand.trim()
      ? 'command'
      : opts.followCwd === false
        ? 'option'
        : opts.followCwd === undefined && !followSetting
          ? 'setting'
          : undefined
  const followChoice = usePanelState((s) => s.follow[sessionId])
  const follow = followChoice ?? followDefault(opts, followSetting)
  const liveCwd = info?.cwd || session?.cwd || ''
  const polled = useQuery({
    queryKey: ['fs', fs.handle?.id ?? '', 'cwd', sessionId],
    placeholderData: undefined, // never show another session's folder under this key
    queryFn: () => fsApi.cwd(fs.handle!.id, sessionId),
    enabled: follow && !!fs.handle && connected && !liveCwd,
    refetchInterval: (q) => (q.state.status === 'error' ? false : 4000),
    retry: false,
  })
  const cwd = liveCwd || polled.data || ''
  const viewId = `panel:${sessionId}`

  useEffect(() => {
    if (!follow || !cwd || !ctx) return
    if (lastFollowedCwd.get(sessionId) === cwd) return
    lastFollowedCwd.set(sessionId, cwd)
    const c = getController(viewId)
    // Quiet: the folder changes under the user's eyes because of the terminal — keep the rows, no loading bar.
    if (c) c.navigate(cwd, { quiet: true })
    else if (getView(viewId).path !== cwd) patchView(viewId, { pending: cwd, pendingMode: 'push', pendingSelect: null, pendingQuiet: true })
  }, [follow, cwd, ctx, sessionId, viewId])

  // The handle can race the session's "connected" event (transport not registered yet): retry a few times.
  const retries = useRef(0)
  const retryFs = fs.retry
  useEffect(() => {
    if (!connected || fs.status !== 'error' || !isDisconnected(fs.error) || retries.current >= 4) return
    const t = setTimeout(() => {
      retries.current++
      retryFs()
    }, 1200)
    return () => clearTimeout(t)
  }, [connected, fs.status, fs.error, retryFs])
  useEffect(() => {
    if (fs.status === 'ready') retries.current = 0
  }, [fs.status])

  // --- states --------------------------------------------------------------------------------------------------------
  const header = <SessionChip tab={tab} label={label} state={state} driver={fs.handle?.driver} />
  const footer = (
    <PanelFooter
      sessionId={sessionId}
      follow={follow}
      cwdKnown={!!cwd}
      reportingOff={reportingOff}
      connected={connected}
      onFollow={(v) => setFollow(sessionId, v)}
      monitoringOption={connection?.options?.monitoring !== false}
    />
  )

  if (browserType === 'none' || isBrowserDisabled(fs.error)) {
    return (
      <PanelShell header={header}>
        <EmptyState
          size="sm"
          icon={FolderSync}
          title="SSH-browser is off for this session"
          description="The session's “SSH-browser type” is set to None."
          action={
            connection ? (
              <Button size="xs" variant="secondary" onClick={() => void runCommand('sessions.edit', { id: connection.id }, { source: 'menu' })}>
                <Settings2 /> Edit session
              </Button>
            ) : undefined
          }
        />
      </PanelShell>
    )
  }

  // The terminal's transport is not usable: ended (closed / gone / the shell exited), down (link lost, error) or being
  // re-established (auto-reconnect: connecting / authenticating while a handle from the previous connection exists).
  const ended = state === 'gone' || state === 'closed'
  const exited = state === 'disconnected' && typeof session?.exitCode === 'number'
  const down = ended || state === 'disconnected' || state === 'error' || (!connected && isDisconnected(fs.error))
  // Auto-reconnect alternates error ("… reconnecting in 4s (attempt 3)") and connecting: one steady "Reconnecting…".
  const autoRetry = (state === 'error' || state === 'disconnected') && !exited && /reconnecting in/i.test(session?.stateMessage ?? '')
  const reconnecting = autoRetry || (!down && (state === 'connecting' || state === 'authenticating'))
  const offline: OfflineMode | null = ended || exited ? 'ended' : reconnecting ? 'reconnecting' : down ? 'down' : null
  // Calm (docs/UX.md): a blip that recovers within 300 ms shows nothing; an ended session is shown at once.
  const offlineShown = useDelayedFlag(!!offline && offline !== 'ended') || offline === 'ended'
  const opening = useLoadingGate(!(ctx && fs.handle) && !down && fs.status !== 'error')
  const reconnect = () => {
    const t = getTerminalByTab(tab.id)
    if (t) t.reconnect()
    else void reconnectSession(sessionId).catch(() => undefined)
  }

  // The "Connecting…" placeholder follows the loading rule even though this component returns early: once shown it
  // stays its minimum time instead of vanishing the moment the browser is ready (docs/UX.md).
  if (ctx && fs.handle && !opening.show) {
    return (
      <div className="relative flex h-full min-h-0 flex-col">
        <FileBrowser
          viewId={viewId}
          ctx={ctx}
          variant="panel"
          initialPath={(follow && cwd) || fs.handle.home}
          extraActions={[openInTabAction(ctx, viewId)]}
          header={header}
          footer={footer}
          className={cn('transition-opacity duration-200 ease-out', offline && offlineShown && 'pointer-events-none opacity-45 select-none')}
          inert={!!offline}
        />
        {offline && offlineShown && (
          <OfflineCard
            mode={offline}
            exitCode={exited ? session?.exitCode : undefined}
            message={session?.stateMessage}
            onReconnect={reconnect}
            canReconnect={state !== 'gone' && state !== 'closed'}
          />
        )}
      </div>
    )
  }

  if (down) {
    return (
      <PanelShell header={header}>
        <EmptyState
          size="sm"
          icon={Unplug}
          title={ended || exited ? 'Session ended' : 'Session disconnected'}
          description={session?.stateMessage || 'Reconnect the terminal to browse its files.'}
          action={
            !ended ? (
              <Button size="xs" onClick={reconnect}>
                <RefreshCw /> Reconnect
              </Button>
            ) : undefined
          }
        />
      </PanelShell>
    )
  }

  if (fs.status === 'error') {
    return (
      <PanelShell header={header} footer={footer}>
        <EmptyState
          size="sm"
          icon={CircleAlert}
          title="Could not open the SSH-browser"
          description={fs.error?.message || 'The file browser could not be started.'}
          action={
            <Button size="xs" variant="secondary" onClick={fs.retry}>
              <RefreshCw /> Retry
            </Button>
          }
        />
      </PanelShell>
    )
  }

  // Connecting / authenticating, or the handle is opening.
  return (
    <PanelShell header={header} footer={footer}>
      {opening.show && (
        <div className="flex flex-col items-center gap-2 px-4 py-10 text-center text-sm text-muted-foreground animate-in fade-in-0 duration-200">
          <Spinner immediate className="size-5" label="Connecting" />
          <span role="status">{LIVE.has(state) && !connected ? `Connecting to ${host || 'the server'}…` : 'Opening the SSH-browser…'}</span>
        </div>
      )}
    </PanelShell>
  )
}

function openInTabAction(ctx: FsContext, viewId: string): ToolAction {
  return {
    id: 'open-tab',
    label: 'Open in full tab',
    icon: Maximize2,
    priority: 50,
    group: 4,
    run: () => {
      const path = getView(viewId).path ?? undefined
      openFilesTab(ctx.source, { path, protocol: ctx.source.kind === 'session' ? 'ssh' : undefined, title: `SFTP · ${ctx.label}` })
    },
  }
}

function PanelShell({ header, footer, children }: { header?: ReactNode; footer?: ReactNode; children: ReactNode }) {
  return (
    <div className="@container flex h-full min-h-0 flex-col">
      {header}
      <div className="min-h-0 flex-1 overflow-auto">{children}</div>
      {footer}
    </div>
  )
}

const STATE_DOT: Record<string, string> = {
  connected: 'bg-success',
  connecting: statusDotClass('warning', true),
  authenticating: statusDotClass('warning', true),
  disconnected: 'bg-muted-foreground/60',
  error: 'bg-destructive',
  closed: 'bg-muted-foreground/40',
  gone: 'bg-muted-foreground/40',
}

/** Which session the panel shows (it keeps the last terminal while other tabs are active). */
function SessionChip({ tab, label, state, driver }: { tab: TabInfo; label: string; state: string; driver?: string }) {
  const active = useWorkspaceStore((s) => s.activeTabId === tab.id)
  const Icon = protocolIcon('ssh')
  return (
    <div className="flex h-7 min-w-0 shrink-0 items-center gap-1.5 border-b pr-1 pl-2 text-xs">
      <span className={cn('size-1.5 shrink-0 rounded-full', STATE_DOT[state] ?? 'bg-muted-foreground/40')} aria-hidden />
      <Icon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <span className="min-w-0 truncate font-medium" title={tab.title}>
        {label}
      </span>
      {driver === 'scp' && (
        <Tooltip content="No SFTP subsystem on this server: using SCP / shell commands">
          <Badge variant="outline" className="shrink-0 px-1 text-2xs">
            SCP
          </Badge>
        </Tooltip>
      )}
      {driver === 'sudo-sftp' && (
        <Badge variant="warning" className="shrink-0 px-1 text-2xs">
          root
        </Badge>
      )}
      <span className="flex-1" />
      {!active && <IconButton icon={SquareTerminal} label={`Go to ${tab.title}`} size="xs" onClick={() => focusTab(tab.id)} />}
    </div>
  )
}

type OfflineMode = 'reconnecting' | 'down' | 'ended'

/** Over the (dimmed, inert) browser while the terminal's transport is not usable. */
function OfflineCard({
  mode,
  exitCode,
  message,
  onReconnect,
  canReconnect,
}: {
  mode: OfflineMode
  exitCode?: number
  message?: string
  onReconnect: () => void
  canReconnect: boolean
}) {
  const title = mode === 'reconnecting' ? 'Reconnecting…' : mode === 'ended' ? (exitCode !== undefined ? `Session ended (exit code ${exitCode})` : 'Session ended') : 'Session disconnected'
  return (
    <div
      className="absolute inset-x-2 top-12 z-20 flex flex-col items-center gap-2 rounded-lg border bg-popover p-3 text-center shadow-popover"
      role={mode === 'reconnecting' ? 'status' : 'alert'}
    >
      {mode === 'reconnecting' ? <Spinner immediate className="size-5" label="Reconnecting" /> : <Unplug className="size-5 text-muted-foreground" aria-hidden />}
      <div className="text-sm font-medium">{title}</div>
      {message && <div className="line-clamp-3 text-xs break-words text-muted-foreground">{message}</div>}
      {canReconnect && (
        <Button size="xs" variant={mode === 'reconnecting' ? 'secondary' : 'default'} onClick={onReconnect}>
          <RefreshCw /> {mode === 'reconnecting' ? 'Reconnect now' : 'Reconnect'}
        </Button>
      )}
    </div>
  )
}

/** "Follow terminal folder" and "Remote monitoring" (both live under the file list). */
type CwdHintReason = 'option' | 'setting' | 'command'

const CWD_HINTS: Record<CwdHintReason | 'default', string> = {
  default:
    'The shell has not reported its folder. NexTerm sets this up at login for bash, zsh, fish and ksh, but skips it when you type before the first prompt appears — reconnect to retry. Other shells need to emit OSC 7.',
  option: 'The shell was not set up to report its folder because “Follow SSH path” is off for this session. Turn it on in the session settings and reconnect.',
  setting: 'The shell was not set up to report its folder because “Follow terminal folder by default” is off in Settings → Files & SFTP. Turn it on and reconnect.',
  command: 'This session runs a remote command instead of a login shell, so its folder is only known if the program emits OSC 7.',
}

function PanelFooter({
  sessionId,
  follow,
  cwdKnown,
  reportingOff,
  connected,
  onFollow,
  monitoringOption,
}: {
  sessionId: string
  follow: boolean
  cwdKnown: boolean
  reportingOff?: CwdHintReason
  connected: boolean
  onFollow: (v: boolean) => void
  /** The saved connection's `monitoring` option (default true). */
  monitoringOption: boolean
}) {
  const monitor = useCommand('monitor.toggleBar')
  // The monitor feature owns the bar's state (SPEC §9 monitor): a per-session choice wins, else the global
  // "Remote monitoring bar" setting and the connection's `monitoring` option — exactly what the bar evaluates.
  const override = useMonitorBarOverride(sessionId)
  const barEnabled = useMonitorBarEnabled()
  const monitoring = override ?? (barEnabled && monitoringOption)
  // The shell reports its folder shortly after the first prompt; only hint when it stays unknown.
  const [waited, setWaited] = useState(false)
  useEffect(() => {
    setWaited(false)
    if (!follow || cwdKnown || !connected) return
    const t = setTimeout(() => setWaited(true), 6000)
    return () => clearTimeout(t)
  }, [follow, cwdKnown, connected])
  const hint = CWD_HINTS[reportingOff ?? 'default']
  const followId = `nx-follow-${sessionId}`
  const monId = `nx-mon-${sessionId}`
  return (
    <div className="flex min-h-7 shrink-0 flex-wrap items-center gap-x-3 gap-y-0.5 border-t px-2 py-1 text-xs">
      <div className="flex items-center gap-1.5">
        <Checkbox id={followId} checked={follow} onCheckedChange={(v) => onFollow(v === true)} className="size-3.5" />
        <label htmlFor={followId} className="select-none">
          <span className="@[17rem]:hidden">Follow folder</span>
          <span className="hidden @[17rem]:inline">Follow terminal folder</span>
        </label>
        {follow && (waited || !!reportingOff) && connected && !cwdKnown && (
          <Tooltip content={hint} side="top">
            <span tabIndex={0} role="img" aria-label={`Terminal folder unknown. ${hint}`} className="rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring">
              <Info className="size-3.5 text-muted-foreground" aria-hidden />
            </span>
          </Tooltip>
        )}
      </div>
      {monitor.enabled && (
        <div className="flex items-center gap-1.5">
          <Checkbox
            id={monId}
            checked={monitoring}
            onCheckedChange={(v) => void runCommand('monitor.toggleBar', { sessionId, enabled: v === true }, { source: 'menu' })}
            className="size-3.5"
          />
          <label htmlFor={monId} className="select-none">
            <span className="@[17rem]:hidden">Monitoring</span>
            <span className="hidden @[17rem]:inline">Remote monitoring</span>
          </label>
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// empty states
// ---------------------------------------------------------------------------------------------------------------------

function NoSession() {
  const newSession = useCommand('sessions.new')
  return (
    <div className="@container flex h-full flex-col">
      <EmptyState
        size="sm"
        icon={FolderSync}
        title="No SSH session"
        description="Open an SSH session to browse its files here. The browser follows the active terminal tab."
        action={
          <>
            <Button size="xs" disabled={!newSession.enabled} onClick={() => void newSession.run({ protocol: 'ssh' }, 'menu')}>
              <Plus /> New session
            </Button>
            <Button size="xs" variant="secondary" onClick={() => void runCommand('files.openLocal', undefined, { source: 'menu' })}>
              <HardDrive /> Local files
            </Button>
          </>
        }
      />
    </div>
  )
}

function NotSsh({ tab, protocol }: { tab: TabInfo; protocol?: string }) {
  const Icon = protocolIcon(protocol ?? 'local')
  const newSession = useCommand('sessions.new')
  const local = protocol === 'local'
  return (
    <div className="@container flex h-full flex-col">
      <div className="flex h-7 min-w-0 shrink-0 items-center gap-1.5 border-b px-2 text-xs text-muted-foreground">
        <Icon className="size-3.5 shrink-0" aria-hidden />
        <span className="truncate">{tab.title}</span>
      </div>
      <EmptyState
        size="sm"
        icon={FolderSync}
        title={local ? 'Local terminal' : `No SSH-browser for ${protocolLabel(protocol) || 'this session'}`}
        description={local ? 'Browse this computer’s files in a files tab, or open an SSH session.' : 'Open an SSH session to browse its files here.'}
        action={
          <>
            <Button size="xs" variant="secondary" onClick={() => void runCommand('files.openLocal', undefined, { source: 'menu' })}>
              <HardDrive /> Local files
            </Button>
            <Button size="xs" variant="secondary" disabled={!newSession.enabled} onClick={() => void newSession.run({ protocol: 'ssh' }, 'menu')}>
              <Plus /> New SSH session
            </Button>
          </>
        }
      />
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// graphical sessions: SFTP companion (GFX-16)
// ---------------------------------------------------------------------------------------------------------------------

function CompanionPanel({ tab, connection, connections }: { tab: TabInfo; connection: Connection | undefined; connections: Connection[] }) {
  const chosen = usePanelState((s) => s.companion[tab.id])
  const host = (connection?.host ?? '').toLowerCase()
  const candidates = useMemo(
    () =>
      connections
        .filter((c) => (c.protocol === 'ssh' || c.protocol === 'sftp') && !!host && c.host.toLowerCase() === host)
        .sort((a, b) => Number(b.favorite) - Number(a.favorite) || (a.protocol === 'sftp' ? -1 : 1)),
    [connections, host],
  )
  const companion = connections.find((c) => c.id === chosen)
  const Icon = protocolIcon(tab.kind)
  if (companion) return <CompanionBrowser key={companion.id} tab={tab} companion={companion} onClose={() => setCompanion(tab.id, null)} />
  return (
    <div className="@container flex h-full flex-col">
      <div className="flex h-7 min-w-0 shrink-0 items-center gap-1.5 border-b px-2 text-xs text-muted-foreground">
        <Icon className="size-3.5 shrink-0" aria-hidden />
        <span className="truncate">{tab.title}</span>
      </div>
      <EmptyState
        size="sm"
        icon={Monitor}
        title="Graphical session"
        description={
          candidates.length
            ? `Browse ${connection?.host ?? 'the host'}’s files through a saved SSH session:`
            : `Save an SSH or SFTP session for ${connection?.host || 'this host'} to browse its files next to the ${tab.kind.toUpperCase()} view.`
        }
        action={
          candidates.length ? (
            <div className="flex flex-col items-stretch gap-1.5">
              {candidates.slice(0, 5).map((c) => (
                <Button key={c.id} size="xs" variant="secondary" onClick={() => setCompanion(tab.id, c.id)}>
                  <FolderSync /> {c.name}
                </Button>
              ))}
            </div>
          ) : undefined
        }
      />
    </div>
  )
}

function CompanionBrowser({ tab, companion, onClose }: { tab: TabInfo; companion: Connection; onClose: () => void }) {
  const source = useMemo<FsSource>(() => ({ kind: 'connection', connectionId: companion.id }), [companion.id])
  const fs = useFs(source)
  const ctx = useMemo<FsContext | null>(
    () =>
      fs.handle && fs.key
        ? {
            key: fs.key,
            handle: fs.handle,
            source,
            placeKey: `conn:${companion.id}`,
            label: companion.name,
            host: { host: companion.host, port: companion.port, username: companion.username },
          }
        : null,
    [fs.handle, fs.key, source, companion],
  )
  const viewId = `panel:companion:${tab.id}`
  const opening = useLoadingGate(!ctx && fs.status !== 'error')
  const header = (
    <div className="flex h-7 min-w-0 shrink-0 items-center gap-1.5 border-b pr-1 pl-2 text-xs">
      <FolderSync className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <span className="min-w-0 truncate font-medium">{companion.name}</span>
      <Badge variant="outline" className="shrink-0 px-1 text-2xs">
        companion
      </Badge>
      <span className="flex-1" />
      <IconButton icon={Unplug} label="Stop browsing" size="xs" onClick={onClose} />
    </div>
  )
  if (ctx && !opening.show) return <FileBrowser viewId={viewId} ctx={ctx} variant="panel" header={header} extraActions={[openInTabAction(ctx, viewId)]} />
  return (
    <PanelShell header={header}>
      {fs.status === 'error' ? (
        <EmptyState
          size="sm"
          icon={CircleAlert}
          title="Could not connect"
          description={fs.error?.message}
          action={
            <Button size="xs" variant="secondary" onClick={fs.retry}>
              <RefreshCw /> Retry
            </Button>
          }
        />
      ) : (
        opening.show && (
          <div className="flex flex-col items-center gap-2 px-4 py-10 text-sm text-muted-foreground animate-in fade-in-0 duration-200">
            <Spinner immediate className="size-5" label="Connecting" />
            <span role="status">Connecting to {companion.host}…</span>
          </div>
        )
      )}
    </PanelShell>
  )
}
