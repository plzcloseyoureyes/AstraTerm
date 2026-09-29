/*
 * The 'monitor' tab (lazy): a host dashboard for one session — Overview (live charts), Processes, Services, Ports,
 * Disk usage, Logs and (SSH) Connection. Sampling runs while the tab is visible.
 */
import { useEffect, useState, type ReactNode } from 'react'
import {
  Activity,
  Cable,
  Cog,
  HardDrive,
  LayoutDashboard,
  ListTree,
  Monitor,
  Plug,
  ScrollText,
  Server,
  SquareTerminal,
  TriangleAlert,
  X,
} from 'lucide-react'
import { useRuntimeSession, useSessions } from '@/api/sessions'
import type { TabProps } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { LoadingPane } from '@/components/ui/spinner'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Tooltip } from '@/components/ui/tooltip'
import { attachSession } from '@/features/terminal/open'
import { useNow } from '@/lib/hooks'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { closeTab, setTabTitle, updateTabParams, useIsTabVisible } from '@/stores/workspace'
import { useHost } from './api'
import { ConnectionPanel } from './ConnectionPanel'
import { DiskUsagePanel } from './DiskUsagePanel'
import { isStale, platformLabel, uptime } from './format'
import { LogsPanel } from './LogsPanel'
import { OverviewPanel } from './OverviewPanel'
import { PortsPanel } from './PortsPanel'
import { ProcessesPanel } from './ProcessesPanel'
import { ServicesPanel } from './ServicesPanel'
import { useMonitorFeed, type FeedEntry } from './store'
import type { MonitorPanel, MonitorTabParams } from './types'

export default function MonitorView({ tabId, params }: TabProps<MonitorTabParams>) {
  const target = params.target ?? params.sessionId ?? ''
  const visible = useIsTabVisible(tabId)
  const sessions = useSessions()
  const sessionsLoad = useLoadingGate(sessions.isPending)
  const session = useRuntimeSession(target)
  const local = session?.protocol === 'local'
  const ready = !!session && (session.state === 'connected' || local)
  const feed = useMonitorFeed(target, visible && !!session)
  const host = useHost(target, ready)
  const panel: MonitorPanel = params.panel ?? 'overview'
  const [unit, setUnit] = useState<string | undefined>()
  const [diskPath, setDiskPath] = useState<string | undefined>(params.path)
  const now = useNow(2_000)
  const st = feed?.stats
  const hostname = st?.hostname || host.data?.hostname

  useEffect(() => {
    if (hostname) setTabTitle(tabId, `Monitor · ${hostname}`)
  }, [tabId, hostname])

  const setPanel = (p: string) => updateTabParams<MonitorTabParams>(tabId, { panel: p as MonitorPanel })

  if (!target) return <EmptyState icon={Activity} title="No session selected" />
  if (!session) {
    if (sessionsLoad.hold) return sessionsLoad.show ? <LoadingPane immediate /> : null
    return (
      <EmptyState
        icon={Activity}
        title="The session has ended"
        description="This monitor followed a session that no longer exists."
        action={
          <Button size="sm" variant="outline" onClick={() => void closeTab(tabId)}>
            <X /> Close tab
          </Button>
        }
      />
    )
  }

  const platform = st?.platform ?? host.data?.platform
  const tools = host.data?.tools ?? {}
  const gate = (node: ReactNode) =>
    ready ? (
      node
    ) : (
      <EmptyState
        icon={Cable}
        title={session.state === 'connecting' || session.state === 'authenticating' ? 'Connecting…' : 'The session is not connected'}
        description={session.stateMessage || 'Monitoring resumes when the SSH connection is up again.'}
        action={
          <Button size="sm" variant="outline" onClick={() => void attachSession(target)}>
            <SquareTerminal /> Go to the terminal
          </Button>
        }
      />
    )

  return (
    <div className="@container flex h-full min-h-0 flex-col bg-background">
      <Header feed={feed} now={now} title={hostname || session.title} local={local} sessionTitle={session.title} onTerminal={() => void attachSession(target)} host={host.data} />
      <Tabs value={panel} onValueChange={setPanel} className="min-h-0 flex-1 gap-0">
        <TabsList className="shrink-0 overflow-x-auto px-2" aria-label="Monitor sections">
          <TabsTrigger value="overview">
            <LayoutDashboard /> Overview
          </TabsTrigger>
          <TabsTrigger value="processes">
            <ListTree /> Processes
          </TabsTrigger>
          <TabsTrigger value="services">
            <Cog /> Services
          </TabsTrigger>
          <TabsTrigger value="ports">
            <Plug /> Ports
          </TabsTrigger>
          <TabsTrigger value="disk">
            <HardDrive /> Disk usage
          </TabsTrigger>
          <TabsTrigger value="logs">
            <ScrollText /> Logs
          </TabsTrigger>
          {!local && (
            <TabsTrigger value="connection">
              <Cable /> Connection
            </TabsTrigger>
          )}
        </TabsList>
        <TabsContent value="overview" className="flex min-h-0 flex-col">
          {st ? (
            <OverviewPanel
              feed={feed}
              onOpenDisk={(mount) => {
                setDiskPath(mount)
                setPanel('disk')
              }}
            />
          ) : (
            gate(<FeedPlaceholder feed={feed} />)
          )}
        </TabsContent>
        <TabsContent value="processes" className="flex min-h-0 flex-col">
          {gate(<ProcessesPanel target={target} platform={platform} active={visible && panel === 'processes'} isLocal={local} />)}
        </TabsContent>
        <TabsContent value="services" className="flex min-h-0 flex-col">
          {gate(
            <ServicesPanel
              target={target}
              platform={platform}
              active={visible && panel === 'services'}
              onFollowUnit={
                tools.journalctl
                  ? (u) => {
                      setUnit(u)
                      setPanel('logs')
                    }
                  : undefined
              }
            />,
          )}
        </TabsContent>
        <TabsContent value="ports" className="flex min-h-0 flex-col">
          {gate(<PortsPanel target={target} platform={platform} active={visible && panel === 'ports'} session={session} />)}
        </TabsContent>
        <TabsContent value="disk" className="flex min-h-0 flex-col">
          {gate(<DiskUsagePanel target={target} platform={platform} active={visible && panel === 'disk'} stats={st} initialPath={diskPath} sessionId={local ? undefined : target} />)}
        </TabsContent>
        {/* Kept mounted so a running follower survives switching sections. */}
        <TabsContent value="logs" forceMount className="flex min-h-0 flex-col data-[state=inactive]:hidden">
          {gate(<LogsPanel target={target} platform={platform} journal={tools.journalctl} unit={unit} />)}
        </TabsContent>
        {!local && (
          <TabsContent value="connection" className="flex min-h-0 flex-col">
            {gate(<ConnectionPanel target={target} active={visible && panel === 'connection'} />)}
          </TabsContent>
        )}
      </Tabs>
    </div>
  )
}

function Header({
  feed,
  now,
  title,
  local,
  sessionTitle,
  host,
  onTerminal,
}: {
  feed?: FeedEntry
  now: number
  title: string
  local: boolean
  sessionTitle: string
  host?: { os?: string; kernel?: string; arch?: string; cpuModel?: string; cores?: number; virt?: string; platform?: string }
  onTerminal: () => void
}) {
  const st = feed?.stats
  const stale = isStale(feed?.receivedAt, now, st?.intervalSec || 2)
  let badge: ReactNode
  if (feed?.state === 'closed') badge = <Badge variant="secondary">ended</Badge>
  else if (!st && feed?.error) badge = <Badge variant={feed.state === 'unavailable' ? 'destructive' : 'warning'}>{feed.state === 'waiting' ? 'waiting' : 'unavailable'}</Badge>
  else if (st && (stale || feed?.error))
    badge = (
      <Tooltip content={feed?.error || 'No fresh sample'}>
        <Badge variant="warning" className="gap-1">
          <TriangleAlert /> stale
        </Badge>
      </Tooltip>
    )
  else if (st)
    badge = (
      <Badge variant="success" className="gap-1">
        {/* A steady dot: live data is conveyed by the numbers changing, never by motion. */}
        <span className="size-1.5 rounded-full bg-success" /> live ·{' '}
        {st.intervalSec ? `${Math.round(st.intervalSec)}s` : '2s'}
      </Badge>
    )
  else badge = <Badge variant="secondary">starting…</Badge>
  const os = st?.os || host?.os
  const kernel = st?.kernel || host?.kernel
  const arch = st?.arch || host?.arch
  const cpuModel = st?.cpuModel || host?.cpuModel
  const details = [os || platformLabel(st?.platform ?? host?.platform), kernel && `kernel ${kernel}`, arch, st?.virt || host?.virt].filter(Boolean).join(' · ')
  const Icon = local ? Monitor : Server
  return (
    <header className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1 border-b px-3 py-2">
      <div className="flex size-8 items-center justify-center rounded-md border bg-muted/50 text-muted-foreground">
        <Icon className="size-4" />
      </div>
      <div className="grid min-w-0 flex-1 gap-0.5">
        <div className="flex min-w-0 items-center gap-2">
          <h2 className="truncate text-md font-semibold">{title}</h2>
          {badge}
          {sessionTitle && sessionTitle !== title && <span className="truncate text-xs text-muted-foreground">session “{sessionTitle}”</span>}
        </div>
        <div className="truncate text-xs text-muted-foreground" title={details}>
          {details || 'Identifying the host…'}
          {cpuModel ? ` · ${cpuModel}` : ''}
          {st?.cpu?.cores ? ` × ${st.cpu.cores}` : ''}
          {st ? ` · up ${uptime(st.uptimeSec)}` : ''}
        </div>
      </div>
      <Button size="xs" variant="outline" onClick={onTerminal}>
        <SquareTerminal /> Terminal
      </Button>
      {feed?.error && st && <p className={cn('basis-full text-xs text-warning')}>{feed.error}</p>}
    </header>
  )
}

function FeedPlaceholder({ feed }: { feed?: FeedEntry }) {
  if (feed?.error && feed.state !== 'waiting') {
    return <EmptyState icon={TriangleAlert} title={feed.state === 'unavailable' ? 'Monitoring is unavailable on this host' : 'Monitoring is retrying'} description={feed.error} />
  }
  return <LoadingPane label={feed?.error || 'Collecting the first samples…'} />
}
