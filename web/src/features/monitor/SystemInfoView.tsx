/*
 * System information of the Termstead host (MON-6: software, hardware, task list, kill task): software and
 * hardware details, live charts, processes, services, ports, disk usage and logs of the machine Termstead runs on.
 * Desktop mode or administrators only.
 */
import type { ReactNode } from 'react'
import { Cog, Copy, Cpu, HardDrive, LayoutDashboard, ListTree, Monitor, Network, Plug, ScrollText, ServerCog, ShieldAlert, Users } from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { LoadingPane } from '@/components/ui/spinner'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { copyText, errorMessage, formatCalendarTime } from '@/lib/utils'
import { updateTabParams, useIsTabVisible } from '@/stores/workspace'
import { useSystemInfo } from './api'
import { InfoRows, Section } from './components'
import { DiskUsagePanel } from './DiskUsagePanel'
import { bytes, compactBytes, pct, uptime } from './format'
import { LogsPanel } from './LogsPanel'
import { OverviewPanel } from './OverviewPanel'
import { PortsPanel } from './PortsPanel'
import { ProcessesPanel } from './ProcessesPanel'
import { ServicesPanel } from './ServicesPanel'
import { useMonitorFeed } from './store'
import type { SysInfoTabParams, SystemInfo } from './types'

type Panel = NonNullable<SysInfoTabParams['panel']>

export default function SystemInfoView({ tabId, params }: TabProps<SysInfoTabParams>) {
  const mode = useRunMode()
  const admin = useIsAdmin()
  const allowed = mode !== 'server' || admin
  const visible = useIsTabVisible(tabId)
  const panel: Panel = params?.panel ?? 'overview'
  const info = useSystemInfo(allowed && visible, visible && panel === 'overview' ? 10_000 : false)
  const feed = useMonitorFeed('local', allowed && visible)
  const platform = feed?.stats?.platform ?? info.data?.host.platform

  if (!allowed) {
    return <EmptyState icon={ShieldAlert} title="Not available" description="System information about the Termstead server is available in desktop mode or to administrators." />
  }
  const setPanel = (p: string) => updateTabParams<SysInfoTabParams>(tabId, { panel: p as Panel })
  const i = info.data
  const firstLoad = useLoadingGate(info.isPending && !i)
  return (
    <div className="@container flex h-full min-h-0 flex-col bg-background">
      <header className="flex shrink-0 items-center gap-3 border-b px-3 py-2">
        <div className="flex size-8 items-center justify-center rounded-md border bg-muted/50 text-muted-foreground">
          <Monitor className="size-4" />
        </div>
        <div className="grid min-w-0 flex-1 gap-0.5">
          <div className="flex items-center gap-2">
            <h2 className="truncate text-md font-semibold">{i?.host.hostname || feed?.stats?.hostname || 'This computer'}</h2>
            <Badge variant="outline">Termstead host</Badge>
          </div>
          <div className="truncate text-xs text-muted-foreground">
            {i ? [i.host.os, i.host.kernelVersion && `kernel ${i.host.kernelVersion}`, i.host.arch, `up ${uptime(i.host.uptimeSec)}`].filter(Boolean).join(' · ') : 'Loading…'}
          </div>
        </div>
      </header>
      <Tabs value={panel} onValueChange={setPanel} className="min-h-0 flex-1 gap-0">
        <TabsList className="shrink-0 overflow-x-auto px-2" aria-label="System information sections">
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
          {platform !== 'windows' && (
            <TabsTrigger value="logs">
              <ScrollText /> Logs
            </TabsTrigger>
          )}
        </TabsList>
        <TabsContent value="overview" className="min-h-0 overflow-auto">
          {firstLoad.hold ? (
            firstLoad.show && <LoadingPane immediate label="Reading system information…" />
          ) : info.isError && !i ? (
            <EmptyState icon={ShieldAlert} title="System information unavailable" description={errorMessage(info.error)} />
          ) : i ? (
            <div className="grid gap-3 p-3">
              <Details info={i} />
              {feed?.stats && <OverviewPanel feed={feed} onOpenDisk={() => setPanel('disk')} className="overflow-visible p-0" />}
            </div>
          ) : null}
        </TabsContent>
        <TabsContent value="processes" className="flex min-h-0 flex-col">
          <ProcessesPanel target="local" platform={platform} active={visible && panel === 'processes'} isLocal />
        </TabsContent>
        <TabsContent value="services" className="flex min-h-0 flex-col">
          <ServicesPanel target="local" platform={platform} active={visible && panel === 'services'} />
        </TabsContent>
        <TabsContent value="ports" className="flex min-h-0 flex-col">
          <PortsPanel target="local" platform={platform} active={visible && panel === 'ports'} />
        </TabsContent>
        <TabsContent value="disk" className="flex min-h-0 flex-col">
          <DiskUsagePanel target="local" platform={platform} active={visible && panel === 'disk'} stats={feed?.stats} />
        </TabsContent>
        {platform !== 'windows' && (
          <TabsContent value="logs" forceMount className="flex min-h-0 flex-col data-[state=inactive]:hidden">
            <LogsPanel target="local" platform={platform} journal={platform === 'linux'} />
          </TabsContent>
        )}
      </Tabs>
    </div>
  )
}

function Copyable({ value }: { value: string }) {
  return (
    <span className="flex min-w-0 items-center gap-1">
      <span className="min-w-0 truncate font-mono text-xs" title={value}>
        {value}
      </span>
      <IconButton icon={Copy} label="Copy" size="xs" onClick={() => void copyText(value)} />
    </span>
  )
}

function Details({ info }: { info: SystemInfo }) {
  const h = info.host
  const c = info.cpu
  const rows = (r: [ReactNode, ReactNode][]) => <InfoRows rows={r} />
  return (
    <div className="grid gap-3 @3xl:grid-cols-2 @6xl:grid-cols-3">
      <Section title="Software" actions={<Monitor className="size-4 text-muted-foreground" />}>
        {rows([
          ['Operating system', h.os],
          ['Platform', [h.platform, h.platformFamily && h.platformFamily !== h.platform ? `(${h.platformFamily})` : '', h.platformVersion].filter(Boolean).join(' ')],
          ['Kernel', h.kernelVersion || '—'],
          ['Architecture', h.arch],
          ['Virtualization', h.virtualization || 'none detected'],
          ['Booted', formatCalendarTime(h.bootTime)],
          ['Uptime', uptime(h.uptimeSec)],
          ['Time zone', h.timezone],
          ['Host ID', h.hostId ? <Copyable value={h.hostId} /> : '—'],
        ])}
      </Section>
      <Section title="Hardware" actions={<Cpu className="size-4 text-muted-foreground" />}>
        {rows([
          ['Processor', c.model || '—'],
          ['Vendor', c.vendor || '—'],
          ['Cores', `${c.physicalCores || '?'} physical · ${c.logicalCores} logical`],
          ['Clock', c.mhz ? `${(c.mhz / 1000).toFixed(2)} GHz` : '—'],
          ['Cache', c.cacheKb ? `${c.cacheKb} KB` : '—'],
          ['CPU usage', pct(c.usage)],
          ['Load', info.load.map((l) => l.toFixed(2)).join(' · ')],
          ['Memory', `${bytes(info.mem.used)} used of ${bytes(info.mem.total)}`],
          ['Swap', info.mem.swapTotal ? `${bytes(info.mem.swapUsed)} of ${bytes(info.mem.swapTotal)}` : 'none'],
        ])}
      </Section>
      <Section title="Termstead server" actions={<ServerCog className="size-4 text-muted-foreground" />}>
        {rows([
          ['Version', info.server.version || '—'],
          ['Mode', info.server.mode],
          ['Listening on', info.server.listen || '—'],
          ['Process ID', String(info.server.pid)],
          ['Go runtime', `${info.server.goVersion} · ${info.server.goroutines} goroutines`],
          ['Heap', bytes(info.server.heapBytes)],
          ['Data directory', info.server.dataDir ? <Copyable value={info.server.dataDir} /> : '—'],
        ])}
      </Section>
      <Section title="Network interfaces" className="@3xl:col-span-2" actions={<Network className="size-4 text-muted-foreground" />}>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-xs text-muted-foreground">
              <tr className="border-b">
                <th className="px-2 py-1 text-left font-medium">Interface</th>
                <th className="px-2 py-1 text-left font-medium">Addresses</th>
                <th className="hidden px-2 py-1 text-left font-medium @xl:table-cell">MAC</th>
                <th className="hidden px-2 py-1 text-right font-medium @xl:table-cell">MTU</th>
                <th className="px-2 py-1 text-right font-medium">Received</th>
                <th className="px-2 py-1 text-right font-medium">Sent</th>
              </tr>
            </thead>
            <tbody>
              {info.net
                .filter((n) => n.addrs.length > 0 || n.rxBytes > 0)
                .map((n) => (
                  <tr key={n.name} className="border-b border-border/50 align-top">
                    <td className="px-2 py-1">
                      <span className="flex flex-wrap items-center gap-1">
                        <span className="font-medium">{n.name}</span>
                        {!n.up && <Badge variant="secondary">down</Badge>}
                        {n.virtual && <Badge variant="outline">virtual</Badge>}
                      </span>
                    </td>
                    <td className="px-2 py-1 font-mono text-xs">
                      {n.addrs.length ? n.addrs.map((a) => <div key={a}>{a}</div>) : <span className="text-muted-foreground">—</span>}
                    </td>
                    <td className="hidden px-2 py-1 font-mono text-xs @xl:table-cell">{n.mac || '—'}</td>
                    <td className="hidden px-2 py-1 text-right tabular @xl:table-cell">{n.mtu}</td>
                    <td className="px-2 py-1 text-right tabular">{compactBytes(n.rxBytes)}</td>
                    <td className="px-2 py-1 text-right tabular">{compactBytes(n.txBytes)}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      </Section>
      <Section title={`Logged-in users (${info.users.length})`} actions={<Users className="size-4 text-muted-foreground" />}>
        {info.users.length === 0 ? (
          <p className="text-sm text-muted-foreground">No interactive sessions.</p>
        ) : (
          <ul className="grid gap-1 text-sm">
            {info.users.map((u, i) => (
              <li key={i} className="flex flex-wrap gap-x-2">
                <span className="font-medium">{u.user}</span>
                {u.terminal && <span className="text-muted-foreground">{u.terminal}</span>}
                {u.host && <span className="text-muted-foreground">from {u.host}</span>}
                {u.started && <span className="text-muted-foreground">since {formatCalendarTime(u.started)}</span>}
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title="Top processes" className="@3xl:col-span-2 @6xl:col-span-3" actions={<ListTree className="size-4 text-muted-foreground" />}>
        <table className="w-full text-sm">
          <thead className="text-xs text-muted-foreground">
            <tr className="border-b">
              <th className="px-2 py-1 text-right font-medium">PID</th>
              <th className="px-2 py-1 text-left font-medium">User</th>
              <th className="px-2 py-1 text-right font-medium">CPU %</th>
              <th className="px-2 py-1 text-right font-medium">Mem %</th>
              <th className="px-2 py-1 text-left font-medium">Command</th>
            </tr>
          </thead>
          <tbody>
            {info.topProcesses.map((p) => (
              <tr key={p.pid} className="border-b border-border/50">
                <td className="px-2 py-1 text-right tabular">{p.pid}</td>
                <td className="max-w-32 truncate px-2 py-1">{p.user || '—'}</td>
                <td className="px-2 py-1 text-right tabular">{p.cpu.toFixed(1)}</td>
                <td className="px-2 py-1 text-right tabular">{p.mem.toFixed(1)}</td>
                <td className="max-w-0 truncate px-2 py-1 font-mono text-xs" title={p.command}>
                  {p.command}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Section>
    </div>
  )
}
