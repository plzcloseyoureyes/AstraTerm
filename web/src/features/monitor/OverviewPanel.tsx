/*
 * Monitor overview: live uPlot charts (CPU, memory, network, load, disk I/O) over the last minutes, per-core usage,
 * mounted filesystems and network interfaces.
 */
import { useMemo, useState } from 'react'
import { Clock, Cpu, FileStack, HardDrive, ListTree, Network, Users } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { cn } from '@/lib/utils'
import { Meter, Section, StatTile } from './components'
import { bytes, compactBytes, compactRate, diskPct, level, levelText, loadLevel, memPct, num, pct, rate, swapPct, uptime } from './format'
import { monitorSettings } from './settings'
import type { FeedEntry } from './store'
import { TimeChart } from './TimeChart'

const WINDOWS = [
  { value: '1', label: '1 min' },
  { value: '5', label: '5 min' },
  { value: '10', label: '10 min' },
] as const

export function OverviewPanel({ feed, onOpenDisk, className }: { feed?: FeedEntry; onOpenDisk?: (mount: string) => void; className?: string }) {
  const s = monitorSettings.use()
  const [win, setWin] = useState<'1' | '5' | '10'>('5')
  const st = feed?.stats
  const windowMs = Number(win) * 60_000
  const hist = useMemo(() => {
    const all = feed?.history ?? []
    const last = all[all.length - 1]?.t ?? 0
    return all.filter((h) => h.t >= last - windowMs)
  }, [feed?.history, windowMs])
  const xs = useMemo(() => hist.map((h) => h.t), [hist])

  if (!st) return null
  const linux = st.platform === 'linux'
  const windows = st.platform === 'windows'
  const cores = st.cpu?.cores || 1
  const mp = memPct(st)
  const cpuSeries = [
    { label: 'Total', color: '--primary', values: hist.map((h) => h.cpu), fill: true },
    { label: 'User', color: '--info', values: hist.map((h) => h.user) },
    { label: 'System', color: '--warning', values: hist.map((h) => h.system) },
    ...(linux ? [{ label: 'I/O wait', color: '--destructive', values: hist.map((h) => h.iowait) }] : []),
  ]
  const memSeries = [
    { label: 'Used', color: '--success', values: hist.map((h) => h.memPct), fill: true },
    ...(st.mem?.cached ? [{ label: 'Cache', color: '--info', values: hist.map((h) => h.cachePct), dash: [4, 3] }] : []),
    ...(st.mem?.swapTotal ? [{ label: 'Swap', color: '--warning', values: hist.map((h) => h.swapPct) }] : []),
  ]
  return (
    <div className={cn('grid min-h-0 flex-1 auto-rows-min gap-3 overflow-auto p-3', className)}>
      <div className="grid grid-cols-2 gap-2 @2xl:grid-cols-4 @5xl:grid-cols-6">
        <StatTile icon={Cpu} label="CPU" value={pct(st.cpu?.usage)} lvl={level(st.cpu?.usage, s.warnPct, s.critPct)} sub={`${cores} core${cores === 1 ? '' : 's'}`}>
          <Meter value={st.cpu?.usage ?? 0} lvl={level(st.cpu?.usage, s.warnPct, s.critPct)} label="CPU usage" />
        </StatTile>
        <StatTile icon={FileStack} label="Memory" value={pct(mp)} lvl={level(mp, s.warnPct, s.critPct)} sub={`${compactBytes(st.mem?.used)} of ${compactBytes(st.mem?.total)}`}>
          <Meter value={mp} lvl={level(mp, s.warnPct, s.critPct)} label="Memory usage" />
        </StatTile>
        <StatTile
          icon={Network}
          label="Network"
          value={
            <span className="text-md">
              ↓ {st.warmup ? '…' : compactRate(st.netTotal?.rxBps)} ↑ {st.warmup ? '…' : compactRate(st.netTotal?.txBps)}
            </span>
          }
          sub={st.diskIo ? `Disk R ${compactRate(st.diskIo.readBps)} · W ${compactRate(st.diskIo.writeBps)}` : undefined}
        />
        <StatTile
          icon={Clock}
          label={windows ? 'Queue / uptime' : 'Load / uptime'}
          value={windows ? num(st.load?.[0], 0) : `${num(st.load?.[0])} ${num(st.load?.[1])} ${num(st.load?.[2])}`}
          lvl={loadLevel(st.load?.[0], cores, st.platform)}
          sub={`up ${uptime(st.uptimeSec)}`}
        />
        <StatTile icon={ListTree} label="Processes" value={st.processes} sub={st.threads ? `${st.threads} threads` : undefined} />
        <StatTile
          icon={Users}
          label="Users · files"
          value={st.users}
          sub={st.fds ? `${st.fds.used}${st.fds.max ? ` / ${st.fds.max}` : ''} open files` : `${st.users} session${st.users === 1 ? '' : 's'}`}
        />
      </div>
      <div className="flex items-center justify-end gap-2">
        <span className="text-xs text-muted-foreground">History</span>
        <SegmentedControl size="sm" value={win} onValueChange={setWin} aria-label="Chart window" options={WINDOWS.map((w) => ({ value: w.value, label: w.label }))} />
      </div>
      <div className="grid gap-3 @3xl:grid-cols-2">
        <TimeChart title="CPU" headline={pct(st.cpu?.usage)} xs={xs} series={cpuSeries} yMax={100} format={(v) => `${Math.round(v)}%`} windowMs={windowMs} />
        <TimeChart title="Memory" headline={`${bytes(st.mem?.used)} / ${bytes(st.mem?.total)}`} xs={xs} series={memSeries} yMax={100} format={(v) => `${Math.round(v)}%`} windowMs={windowMs} />
        <TimeChart
          title="Network"
          headline={`↓ ${rate(st.netTotal?.rxBps)} ↑ ${rate(st.netTotal?.txBps)}`}
          xs={xs}
          series={[
            { label: 'Received', color: '--success', values: hist.map((h) => h.rx), fill: true },
            { label: 'Sent', color: '--primary', values: hist.map((h) => h.tx) },
          ]}
          format={(v) => compactRate(v)}
          windowMs={windowMs}
        />
        {windows ? (
          <TimeChart
            title="Processor queue"
            headline={num(st.load?.[0], 0)}
            xs={xs}
            series={[{ label: 'Queue length', color: '--warning', values: hist.map((h) => h.load1), fill: true }]}
            format={(v) => v.toFixed(0)}
            windowMs={windowMs}
          />
        ) : (
          <TimeChart
            title="Load average"
            headline={`${num(st.load?.[0])} · ${cores} cores`}
            xs={xs}
            series={[
              { label: '1 min', color: '--warning', values: hist.map((h) => h.load1), fill: true },
              { label: '5 min', color: '--info', values: hist.map((h) => h.load5) },
              { label: '15 min', color: '--success', values: hist.map((h) => h.load15), dash: [4, 3] },
            ]}
            format={(v) => v.toFixed(v < 10 ? 2 : 1)}
            windowMs={windowMs}
          />
        )}
        {st.diskIo && (
          <TimeChart
            title="Disk I/O"
            headline={`R ${rate(st.diskIo.readBps)} · W ${rate(st.diskIo.writeBps)}`}
            xs={xs}
            series={[
              { label: 'Read', color: '--info', values: hist.map((h) => h.read), fill: true },
              { label: 'Write', color: '--destructive', values: hist.map((h) => h.write) },
            ]}
            format={(v) => compactRate(v)}
            windowMs={windowMs}
          />
        )}
        {(st.cpu?.perCore?.length ?? 0) > 1 && (
          <Section title={`Per-core usage (${st.cpu.perCore.length})`}>
            <div className="grid grid-cols-[repeat(auto-fill,minmax(7rem,1fr))] gap-x-3 gap-y-1.5">
              {st.cpu.perCore.map((v, i) => (
                <div key={i} className="grid grid-cols-[2.5rem_1fr_2.5rem] items-center gap-1.5 text-xs">
                  <span className="text-muted-foreground tabular">#{i}</span>
                  <Meter value={v} lvl={level(v, s.warnPct, s.critPct)} label={`Core ${i}`} />
                  <span className="text-right tabular">{Math.round(v)}%</span>
                </div>
              ))}
            </div>
          </Section>
        )}
      </div>
      <Section title="Filesystems" actions={<HardDrive className="size-4 text-muted-foreground" />}>
        {st.disks.length === 0 ? (
          <p className="text-sm text-muted-foreground">No local filesystems reported.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-xs text-muted-foreground">
                <tr className="border-b">
                  <th className="px-2 py-1 text-left font-medium">Mount</th>
                  <th className="hidden px-2 py-1 text-left font-medium @xl:table-cell">Type</th>
                  <th className="hidden px-2 py-1 text-left font-medium @3xl:table-cell">Device</th>
                  <th className="w-1/3 px-2 py-1 text-left font-medium">Usage</th>
                  <th className="px-2 py-1 text-right font-medium">Used</th>
                  <th className="px-2 py-1 text-right font-medium">Free</th>
                  <th className="px-2 py-1 text-right font-medium">Size</th>
                </tr>
              </thead>
              <tbody>
                {st.disks.map((d) => {
                  const p = diskPct(d)
                  const lv = level(p, s.warnPct, s.critPct)
                  return (
                    <tr
                      key={d.mount}
                      className={cn('border-b border-border/50', onOpenDisk && 'cursor-pointer hover:bg-accent/40')}
                      onClick={() => onOpenDisk?.(d.mount)}
                      title={onOpenDisk ? 'Explore disk usage' : undefined}
                    >
                      <td className="max-w-48 truncate px-2 py-1 font-medium">{d.mount}</td>
                      <td className="hidden px-2 py-1 text-muted-foreground @xl:table-cell">{d.fs || '—'}</td>
                      <td className="hidden max-w-48 truncate px-2 py-1 font-mono text-xs text-muted-foreground @3xl:table-cell">{d.device || '—'}</td>
                      <td className="px-2 py-1">
                        <div className="flex items-center gap-2">
                          <Meter value={p} lvl={lv} label={`${d.mount} used`} />
                          <span className={cn('w-12 shrink-0 text-right tabular', levelText[lv])}>{pct(p)}</span>
                        </div>
                      </td>
                      <td className="px-2 py-1 text-right tabular">{compactBytes(d.used)}</td>
                      <td className="px-2 py-1 text-right tabular">{compactBytes(d.avail ?? d.total - d.used)}</td>
                      <td className="px-2 py-1 text-right tabular">{compactBytes(d.total)}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </Section>
      <Section title="Network interfaces" actions={<Network className="size-4 text-muted-foreground" />}>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-xs text-muted-foreground">
              <tr className="border-b">
                <th className="px-2 py-1 text-left font-medium">Interface</th>
                <th className="px-2 py-1 text-right font-medium">↓ Receive</th>
                <th className="px-2 py-1 text-right font-medium">↑ Send</th>
                <th className="hidden px-2 py-1 text-right font-medium @xl:table-cell">Received</th>
                <th className="hidden px-2 py-1 text-right font-medium @xl:table-cell">Sent</th>
              </tr>
            </thead>
            <tbody>
              {st.net.map((n) => (
                <tr key={n.iface} className="border-b border-border/50">
                  <td className="px-2 py-1">
                    <span className="flex items-center gap-1.5">
                      <span className="max-w-60 truncate font-medium">{n.iface}</span>
                      {n.virtual && (
                        <Badge variant="outline" className="font-normal">
                          virtual
                        </Badge>
                      )}
                    </span>
                  </td>
                  <td className="px-2 py-1 text-right tabular">{st.warmup ? '…' : rate(n.rxBps)}</td>
                  <td className="px-2 py-1 text-right tabular">{st.warmup ? '…' : rate(n.txBps)}</td>
                  <td className="hidden px-2 py-1 text-right text-muted-foreground tabular @xl:table-cell">{bytes(n.rxTotal)}</td>
                  <td className="hidden px-2 py-1 text-right text-muted-foreground tabular @xl:table-cell">{bytes(n.txTotal)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {(st.mem?.swapTotal ?? 0) > 0 && (
          <p className="text-xs text-muted-foreground tabular">
            Swap {bytes(st.mem.swapUsed)} of {bytes(st.mem.swapTotal)} ({pct(swapPct(st))})
          </p>
        )}
      </Section>
    </div>
  )
}
