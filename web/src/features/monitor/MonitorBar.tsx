/*
 * The remote monitoring bar (MON-1): the strip under an SSH tab — host, CPU, RAM, disk of /, network ↓↑,
 * users, uptime, load — rendered in the status bar for the ACTIVE tab's session (SSH, and local shells when enabled).
 * Items turn orange / red above the warning / critical thresholds; hovering shows details; clicking opens the monitor
 * tab on the matching panel. Sampling runs only while the bar is shown and a AstraTerm window is visible.
 */
import { useEffect, useLayoutEffect, useRef, useState, type Ref, type RefObject } from 'react'
import type * as React from 'react'
import { Activity, ArrowDownUp, Clock, Cpu, Ellipsis, Gauge, HardDrive, Layers, ListTree, MemoryStick, Monitor, Server, TriangleAlert, Users } from 'lucide-react'
import { TooltipContent, TooltipRoot, TooltipTrigger } from '@/components/ui/tooltip'
import { runCommand } from '@/app/commands'
import { useConnections } from '@/api/connections'
import { useRuntimeSession } from '@/api/sessions'
import { useNow } from '@/lib/hooks'
import { cn, formatCalendarTime } from '@/lib/utils'
import { useActiveTab } from '@/stores/workspace'
import { Sparkline } from './components'
import {
  bytes,
  compactBytes,
  compactRate,
  diskPct,
  isStale,
  level,
  levelText,
  loadLevel,
  memPct,
  num,
  pct,
  platformLabel,
  rate,
  rootDisk,
  swapPct,
  uptime,
  type Level,
} from './format'
import { tabTarget } from './open'
import { monitorSettings, useMonitorBarOverride, type BarItem } from './settings'
import { useMonitorFeed } from './store'
import type { MonitorPanel, Stats } from './types'

function open(sessionId: string, panel: MonitorPanel) {
  void runCommand('monitor.open', { sessionId, panel }, { source: 'api' })
}

/** Session shown by the active dock tab (`params.sessionId`, or a monitor tab's target), if it can be monitored. */
export function useActiveMonitorTarget(): { sessionId: string; protocol: string } | undefined {
  const tab = useActiveTab()
  const s = monitorSettings.use()
  const sessionId = tabTarget(tab)
  const session = useRuntimeSession(sessionId)
  const override = useMonitorBarOverride(sessionId)
  const { data: conns } = useConnections(!!session?.connectionId)
  if (!session || !sessionId || session.state === 'closed' || override === false) return undefined
  if (session.protocol !== 'ssh' && !(session.protocol === 'local' && (s.barForLocal || override))) return undefined
  if (override === true) return { sessionId, protocol: session.protocol }
  if (!s.showBar) return undefined
  const conn = session.connectionId ? conns?.find((c) => c.id === session.connectionId) : undefined
  if (conn?.options?.monitoring === false) return undefined
  return { sessionId, protocol: session.protocol }
}

/** Bar density: 0 = everything configured; each step drops detail until the bar fits the status bar. */
const MAX_DENSITY = 4
/** Items dropped first when space runs out (step 2), then these (step 3); step 4 keeps CPU, memory and disk. */
const SECONDARY: BarItem[] = ['procs', 'swap', 'users', 'uptime']
const TERTIARY: BarItem[] = ['host', 'load']

/**
 * Fit the bar into the status bar instead of letting it be clipped mid-item (the status bar hides overflow): step by
 * step drop the sparklines, the secondary items, then the host name and load, until nothing overflows. Re-evaluated
 * when the status bar or its other items change width and on every sample (numbers change width). All steps run in
 * layout effects, so only the final state is painted.
 */
function useFitDensity(ref: RefObject<HTMLDivElement | null>, shown: boolean, key: string): number {
  const [density, setDensity] = useState(0)
  const [geometry, setGeometry] = useState('')
  const lastKey = useRef('')
  useEffect(() => {
    const bar = ref.current
    const footer = bar?.closest('footer')
    if (!bar || !footer) return
    // Natural widths only: the flex layout squeezes the sides while the bar overflows, which must not count as a change.
    const measure = () => {
      let others = 0
      for (const side of Array.from(footer.children)) {
        if (side.contains(bar)) {
          for (const item of Array.from(side.children)) if (!item.contains(bar)) others += (item as HTMLElement).offsetWidth
        } else others += side.scrollWidth
      }
      setGeometry(`${footer.clientWidth}:${others}`)
    }
    const ro = new ResizeObserver(measure)
    ro.observe(footer)
    for (const side of Array.from(footer.children)) ro.observe(side)
    measure()
    return () => ro.disconnect()
  }, [ref, shown])
  const fullKey = `${key}|${geometry}`
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    if (lastKey.current !== fullKey) {
      lastKey.current = fullKey
      if (density !== 0) {
        setDensity(0)
        return
      }
    }
    if (el.scrollWidth > el.clientWidth + 1 && density < MAX_DENSITY) setDensity(density + 1)
  })
  return density
}

export function MonitorBar() {
  const target = useActiveMonitorTarget()
  const feed = useMonitorFeed(target?.sessionId, !!target)
  const s = monitorSettings.use()
  // Staleness only matters while the bar is shown (no 2-second re-render loop otherwise).
  const now = useNow(target ? 2_000 : 3_600_000)
  const ref = useRef<HTMLDivElement>(null)
  const st = feed?.stats
  const density = useFitDensity(ref, !!target, `${target?.sessionId}|${st?.ts}|${!!st}|${s.barItems.join(',')}|${s.sparklines}`)
  if (!target) return null
  const { sessionId } = target
  if (!st) {
    const failed = feed?.state === 'unavailable' || feed?.state === 'error'
    return (
      <BarShell ref={ref}>
        <Chip
          icon={failed ? TriangleAlert : Activity}
          lvl={failed ? 'warn' : 'ok'}
          muted={!failed}
          label={failed ? 'Remote monitoring unavailable' : 'Remote monitoring starting'}
          onClick={() => open(sessionId, 'overview')}
          tooltip={<span className="max-w-xs">{feed?.error || 'Collecting the first sample…'}</span>}
        >
          <span>{failed ? 'Monitoring unavailable' : 'Monitoring…'}</span>
        </Chip>
      </BarShell>
    )
  }
  const stale = isStale(feed?.receivedAt, now, st.intervalSec || 2) || (!!feed?.error && feed.state !== 'waiting')
  const hist = feed?.history.slice(-40) ?? []
  const configured = new Set<BarItem>(s.barItems)
  const dropped = new Set<BarItem>([
    ...(density >= 2 ? SECONDARY : []),
    ...(density >= 3 ? TERTIARY : []),
    ...(density >= 4 ? (['net'] as BarItem[]) : []),
  ])
  const items = new Set([...configured].filter((i) => !dropped.has(i)))
  const hidden = [...configured].filter((i) => dropped.has(i) && (i !== 'swap' || (st.mem?.swapTotal ?? 0) > 0))
  const spark = s.sparklines && density < 1
  const compact = density >= 3
  const cpuLvl = level(st.cpu?.usage, s.warnPct, s.critPct)
  const mp = memPct(st)
  const sp = swapPct(st)
  const disk = rootDisk(st)
  const dp = diskPct(disk)
  const cores = st.cpu?.cores || 1
  const loadLvl = loadLevel(st.load?.[0], cores, st.platform)
  const local = target.protocol === 'local'

  return (
    <BarShell ref={ref} stale={stale} warning={feed?.error && stale ? feed.error : undefined}>
      {items.has('host') && (
        <Chip icon={local ? Monitor : Server} label={`Host ${st.hostname}`} onClick={() => open(sessionId, local ? 'overview' : 'connection')} tooltip={<HostTip st={st} />}>
          <span className="max-w-32 truncate font-medium">{st.hostname || '—'}</span>
        </Chip>
      )}
      {items.has('cpu') && (
        <Chip icon={Cpu} lvl={cpuLvl} label={`CPU ${pct(st.cpu?.usage)}`} onClick={() => open(sessionId, 'overview')} tooltip={<CpuTip st={st} />}>
          <span className="w-9 text-right tabular">{pct(st.cpu?.usage)}</span>
          {spark && <Sparkline values={hist.map((h) => h.cpu)} max={100} />}
        </Chip>
      )}
      {items.has('mem') && (
        <Chip icon={MemoryStick} lvl={level(mp, s.warnPct, s.critPct)} label={`Memory ${pct(mp)}`} onClick={() => open(sessionId, 'overview')} tooltip={<MemTip st={st} />}>
          <span className="tabular">{compact ? pct(mp) : `${compactBytes(st.mem?.used)}/${compactBytes(st.mem?.total)}`}</span>
          {spark && <Sparkline values={hist.map((h) => h.memPct)} max={100} fill={false} />}
        </Chip>
      )}
      {items.has('swap') && (st.mem?.swapTotal ?? 0) > 0 && (
        <Chip icon={Layers} lvl={level(sp, s.warnPct, s.critPct)} label={`Swap ${pct(sp)}`} onClick={() => open(sessionId, 'overview')} tooltip={<MemTip st={st} />}>
          <span className="tabular">swap {pct(sp)}</span>
        </Chip>
      )}
      {items.has('disk') && disk && (
        <Chip icon={HardDrive} lvl={level(dp, s.warnPct, s.critPct)} label={`Disk ${disk.mount} ${pct(dp)}`} onClick={() => open(sessionId, 'disk')} tooltip={<DiskTip st={st} warn={s.warnPct} crit={s.critPct} />}>
          {!compact && <span className="max-w-24 truncate">{shortMount(disk.mount)}</span>}
          <span className="tabular">{pct(dp)}</span>
        </Chip>
      )}
      {items.has('net') && (
        <Chip icon={ArrowDownUp} label={`Network down ${rate(st.netTotal?.rxBps)} up ${rate(st.netTotal?.txBps)}`} onClick={() => open(sessionId, 'overview')} tooltip={<NetTip st={st} />}>
          <span className="tabular">
            ↓{st.warmup ? '…' : compactRate(st.netTotal?.rxBps)} ↑{st.warmup ? '…' : compactRate(st.netTotal?.txBps)}
          </span>
          {spark && <Sparkline values={hist.map((h) => h.rx + h.tx)} />}
        </Chip>
      )}
      {items.has('load') && (
        <Chip icon={Gauge} lvl={loadLvl} label={`Load ${num(st.load?.[0])}`} onClick={() => open(sessionId, 'overview')} tooltip={<LoadTip st={st} />}>
          <span className="tabular">{num(st.load?.[0])}</span>
        </Chip>
      )}
      {items.has('users') && (
        <Chip icon={Users} label={`${st.users} users logged in`} onClick={() => open(sessionId, 'overview')} tooltip={`${st.users} user session${st.users === 1 ? '' : 's'} logged in`}>
          <span className="tabular">{st.users}</span>
        </Chip>
      )}
      {items.has('uptime') && (
        <Chip icon={Clock} label={`Uptime ${uptime(st.uptimeSec)}`} onClick={() => open(sessionId, 'overview')} tooltip={<UptimeTip st={st} />}>
          <span className="tabular">{uptime(st.uptimeSec)}</span>
        </Chip>
      )}
      {items.has('procs') && (
        <Chip icon={ListTree} label={`${st.processes} processes`} onClick={() => open(sessionId, 'processes')} tooltip={<ProcTip st={st} />}>
          <span className="tabular">{st.processes}</span>
        </Chip>
      )}
      {hidden.length > 0 && (
        <Chip
          icon={Ellipsis}
          lvl={hidden.includes('load') ? loadLvl : 'ok'}
          label={`More: ${hidden.map((i) => `${HIDDEN_LABEL[i] ?? i} ${hiddenSummary(i, st)}`).join(', ')}`}
          onClick={() => open(sessionId, 'overview')}
          tooltip={<HiddenTip items={hidden} st={st} />}
        >
          {null}
        </Chip>
      )}
    </BarShell>
  )
}

function shortMount(m: string): string {
  if (m === '/System/Volumes/Data') return 'Data'
  return m
}

const HIDDEN_LABEL: Partial<Record<BarItem, string>> = {
  host: 'Host',
  load: 'Load',
  users: 'Users',
  uptime: 'Uptime',
  procs: 'Processes',
  swap: 'Swap',
  net: 'Network',
}

function hiddenSummary(i: BarItem, st: Stats): string {
  switch (i) {
    case 'host':
      return st.hostname || '—'
    case 'load':
      return num(st.load?.[0])
    case 'users':
      return String(st.users)
    case 'uptime':
      return uptime(st.uptimeSec)
    case 'procs':
      return String(st.processes)
    case 'swap':
      return pct(swapPct(st))
    case 'net':
      return `↓ ${rate(st.netTotal?.rxBps)} ↑ ${rate(st.netTotal?.txBps)}`
  }
  return ''
}

/** Items hidden to fit the status bar, with their values. */
function HiddenTip({ items, st }: { items: BarItem[]; st: Stats }) {
  return (
    <>
      <Rows rows={items.map((i) => [HIDDEN_LABEL[i] ?? i, hiddenSummary(i, st)] as [string, string])} />
      <div className="text-muted-foreground">Hidden to fit the status bar — click for the monitor tab</div>
    </>
  )
}

function BarShell({ children, stale, warning, ref }: { children: React.ReactNode; stale?: boolean; warning?: string; ref?: Ref<HTMLDivElement> }) {
  return (
    <div
      ref={ref}
      role="group"
      aria-label="Remote monitoring"
      className={cn('ml-1 flex h-full min-w-0 items-center gap-px border-l pl-1 transition-opacity', stale && 'opacity-55')}
      title={warning}
    >
      {children}
    </div>
  )
}

function Chip({
  icon: Icon,
  children,
  tooltip,
  lvl = 'ok',
  muted,
  label,
  onClick,
}: {
  icon: React.ComponentType<{ className?: string }>
  children: React.ReactNode
  tooltip: React.ReactNode
  lvl?: Level
  muted?: boolean
  label: string
  onClick: () => void
}) {
  return (
    <TooltipRoot delayDuration={250}>
      <TooltipTrigger asChild>
        <button
          type="button"
          onClick={onClick}
          aria-label={label}
          className={cn(
            'flex h-full shrink-0 items-center gap-1 rounded-sm px-1.5 whitespace-nowrap outline-none',
            'hover:bg-foreground/8 focus-visible:ring-1 focus-visible:ring-ring',
            muted && 'text-muted-foreground',
            levelText[lvl],
          )}
        >
          <Icon className="size-3.5 shrink-0 opacity-80" />
          {children}
        </button>
      </TooltipTrigger>
      <TooltipContent side="top" className="max-w-sm flex-col items-stretch gap-1 py-1.5 text-xs">
        {tooltip}
      </TooltipContent>
    </TooltipRoot>
  )
}

function Rows({ rows }: { rows: [string, React.ReactNode][] }) {
  return (
    <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
      {rows.map(([k, v]) => (
        <div key={k} className="contents">
          <span className="text-muted-foreground">{k}</span>
          <span className="tabular">{v}</span>
        </div>
      ))}
    </div>
  )
}

function HostTip({ st }: { st: Stats }) {
  return (
    <>
      <div className="font-semibold">{st.hostname}</div>
      <Rows
        rows={[
          ['System', st.os || platformLabel(st.platform)],
          ['Kernel', st.kernel || '—'],
          ['Architecture', st.arch || '—'],
          ['CPU', st.cpuModel ? `${st.cpuModel} · ${st.cpu?.cores} cores` : `${st.cpu?.cores} cores`],
          ...(st.virt ? ([['Virtualization', st.virt]] as [string, string][]) : []),
        ]}
      />
      <div className="text-muted-foreground">Click for the monitor tab</div>
    </>
  )
}

function CpuTip({ st }: { st: Stats }) {
  const per = st.cpu?.perCore ?? []
  return (
    <>
      <div className="font-semibold">CPU {pct(st.cpu?.usage)}</div>
      <Rows
        rows={[
          ['User', pct(st.cpu?.user)],
          ['System', pct(st.cpu?.system)],
          ...(st.platform === 'linux' ? ([['I/O wait', pct(st.cpu?.iowait)], ['Steal', pct(st.cpu?.steal)]] as [string, string][]) : []),
          ['Cores', String(st.cpu?.cores ?? '—')],
        ]}
      />
      {per.length > 1 && per.length <= 64 && (
        <div className="mt-1 flex h-8 items-end gap-0.5" aria-hidden>
          {per.map((v, i) => (
            <div key={i} className={cn('flex h-full items-end rounded-[1px] bg-muted', per.length > 16 ? 'w-1' : 'w-2.5')}>
              <div className={cn('w-full rounded-[1px]', v >= 90 ? 'bg-destructive' : v >= 80 ? 'bg-warning' : 'bg-primary')} style={{ height: `${Math.max(v, 2)}%` }} />
            </div>
          ))}
        </div>
      )}
    </>
  )
}

function MemTip({ st }: { st: Stats }) {
  const m = st.mem
  return (
    <>
      <div className="font-semibold">Memory {pct(memPct(st))}</div>
      <Rows
        rows={[
          ['Used', `${bytes(m?.used)} of ${bytes(m?.total)}`],
          ['Available', bytes(m?.available)],
          ...(m?.cached ? ([['Cache / buffers', bytes(m.cached)]] as [string, string][]) : []),
          ['Swap', m?.swapTotal ? `${bytes(m.swapUsed)} of ${bytes(m.swapTotal)} (${pct(swapPct(st))})` : 'none'],
        ]}
      />
    </>
  )
}

function DiskTip({ st, warn, crit }: { st: Stats; warn: number; crit: number }) {
  return (
    <>
      <div className="font-semibold">Disks</div>
      <div className="grid grid-cols-[auto_auto_auto] gap-x-3 gap-y-0.5">
        {st.disks.slice(0, 12).map((d) => {
          const p = diskPct(d)
          return (
            <div key={d.mount} className="contents">
              <span className="max-w-40 truncate">{d.mount}</span>
              <span className={cn('text-right tabular', levelText[level(p, warn, crit)])}>{pct(p)}</span>
              <span className="text-muted-foreground tabular">
                {compactBytes(d.used)} / {compactBytes(d.total)}
              </span>
            </div>
          )
        })}
      </div>
      <div className="text-muted-foreground">Click for the disk usage explorer</div>
    </>
  )
}

function NetTip({ st }: { st: Stats }) {
  const ifs = st.net.filter((n) => !n.virtual || st.net.every((x) => x.virtual))
  return (
    <>
      <div className="font-semibold">
        Network ↓ {rate(st.netTotal?.rxBps)} ↑ {rate(st.netTotal?.txBps)}
      </div>
      <div className="grid grid-cols-[auto_auto_auto] gap-x-3 gap-y-0.5">
        {ifs.slice(0, 10).map((n) => (
          <div key={n.iface} className="contents">
            <span className="max-w-40 truncate">{n.iface}</span>
            <span className="tabular">↓ {compactRate(n.rxBps)}</span>
            <span className="tabular">↑ {compactRate(n.txBps)}</span>
          </div>
        ))}
      </div>
      {st.diskIo && (
        <div className="text-muted-foreground tabular">
          Disk I/O: read {compactRate(st.diskIo.readBps)}, write {compactRate(st.diskIo.writeBps)}
        </div>
      )}
    </>
  )
}

function LoadTip({ st }: { st: Stats }) {
  const win = st.platform === 'windows'
  return (
    <>
      <div className="font-semibold">{win ? 'Processor queue length' : 'Load average'}</div>
      <Rows
        rows={
          win
            ? [['Queue', num(st.load?.[0], 0)]]
            : [
                ['1 min', num(st.load?.[0])],
                ['5 min', num(st.load?.[1])],
                ['15 min', num(st.load?.[2])],
                ['Cores', String(st.cpu?.cores ?? '—')],
              ]
        }
      />
    </>
  )
}

function UptimeTip({ st }: { st: Stats }) {
  const boot = st.uptimeSec ? new Date(Date.parse(st.ts) - st.uptimeSec * 1000) : undefined
  return (
    <Rows
      rows={[
        ['Up', uptime(st.uptimeSec)],
        ['Booted', formatCalendarTime(boot)],
      ]}
    />
  )
}

function ProcTip({ st }: { st: Stats }) {
  return (
    <Rows
      rows={[
        ['Processes', String(st.processes)],
        ...(st.threads ? ([['Threads', String(st.threads)]] as [string, string][]) : []),
        ...(st.fds ? ([['Open files', st.fds.max ? `${st.fds.used} of ${st.fds.max}` : String(st.fds.used)]] as [string, string][]) : []),
      ]}
    />
  )
}
