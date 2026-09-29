/*
 * Side drawer of one server: live activity log (filter, search, pause, clear, download), connected clients (with
 * disconnect) and "how to connect" examples.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import {
  CircleAlert,
  Copy,
  Download,
  Eraser,
  Info,
  Pause,
  Play,
  Search,
  TriangleAlert,
  Unplug,
  Users,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import { useQuery } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { LoadingPane } from '@/components/ui/spinner'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { events, useEventsTopic } from '@/lib/events'
import { useNow } from '@/lib/hooks'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage, formatBytes, formatRelativeTime } from '@/lib/utils'
import { copyWithToast } from './actions'
import { clearServerLogs, disconnectClient, getServerLogs, listServerClients, serverKeys, useServer } from './api'
import { StateBadge } from './components'
import { useRestoreFocus } from './hooks'
import { KINDS, clientCommands } from './model'
import type { DrawerTab } from './store'
import type { LogLevel, ServerKindEx, ServerLogEntry, ServerLogEvent } from './types'

const MAX_ENTRIES = 2000

export default function ActivityDrawer({
  kind,
  initialTab,
  openKey,
  onClose,
}: {
  kind: ServerKindEx
  initialTab: DrawerTab
  /** Changes on every open request (re-selects the requested tab). */
  openKey: number
  onClose: () => void
}) {
  const status = useServer(kind)
  const info = KINDS[kind]
  const [tab, setTab] = useState<DrawerTab>(initialTab)
  const panelRef = useRef<HTMLDivElement>(null)
  useRestoreFocus()
  useEffect(() => setTab(initialTab), [initialTab, openKey])
  useEffect(() => {
    panelRef.current?.focus()
  }, [kind])

  return (
    <div
      ref={panelRef}
      role="dialog"
      aria-modal="false"
      aria-label={`${info.label} activity`}
      tabIndex={-1}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.stopPropagation()
          onClose()
        }
      }}
      className={cn(
        'fixed inset-y-0 right-0 z-40 flex w-[min(40rem,100vw)] flex-col border-l bg-popover text-popover-foreground shadow-popover outline-none',
        'animate-in slide-in-from-right-8 fade-in-0 duration-150',
      )}
    >
      <header className="flex items-center gap-2 border-b px-3 py-2.5">
        <info.icon className="size-4 text-muted-foreground" aria-hidden />
        <h2 className="truncate font-semibold">{info.label}</h2>
        {status && <StateBadge status={status} />}
        <div className="flex-1" />
        <IconButton icon={X} label="Close" onClick={onClose} />
      </header>
      <Tabs value={tab} onValueChange={(v) => setTab(v as DrawerTab)} className="min-h-0 flex-1 gap-0">
        <TabsList className="px-2">
          <TabsTrigger value="log">Activity</TabsTrigger>
          <TabsTrigger value="clients">
            {kind === 'syslog' ? 'Senders' : 'Clients'}
            {status?.running ? ` (${status.clients})` : ''}
          </TabsTrigger>
          <TabsTrigger value="connect">How to connect</TabsTrigger>
        </TabsList>
        <TabsContent value="log" className="flex min-h-0 flex-col">
          <LogPane kind={kind} />
        </TabsContent>
        <TabsContent value="clients" className="min-h-0 overflow-auto">
          <ClientsPane kind={kind} running={!!status?.running} />
        </TabsContent>
        <TabsContent value="connect" className="min-h-0 overflow-auto">
          {status && <ConnectPane kind={kind} />}
        </TabsContent>
      </Tabs>
    </div>
  )
}

// ---- log ----------------------------------------------------------------------------------------------------------------

type LevelFilter = 'debug' | 'info' | 'warn' | 'error'

const LEVEL_RANK: Record<LogLevel, number> = { debug: 0, info: 1, warn: 2, error: 3 }

function mergeEntries(a: ServerLogEntry[], b: ServerLogEntry[]): ServerLogEntry[] {
  if (!b.length) return a
  const last = a.length ? a[a.length - 1].id : 0
  const fresh = b.filter((e) => e.id > last)
  const out = fresh.length === b.length && (!a.length || b[0].id > last) ? [...a, ...fresh] : dedupe([...a, ...b])
  return out.length > MAX_ENTRIES ? out.slice(out.length - MAX_ENTRIES) : out
}

function dedupe(list: ServerLogEntry[]): ServerLogEntry[] {
  const m = new Map<number, ServerLogEntry>()
  for (const e of list) m.set(e.id, e)
  return [...m.values()].sort((x, y) => x.id - y.id)
}

function useServerLog(kind: ServerKindEx) {
  const [entries, setEntries] = useState<ServerLogEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string>()
  useEventsTopic('servers.log', { kind })
  const reload = useCallback(async () => {
    setLoading(true)
    setError(undefined)
    try {
      const r = await getServerLogs(kind, 0, MAX_ENTRIES)
      setEntries((cur) => mergeEntries([], dedupe([...r.entries, ...cur.filter((e) => e.id > r.lastId)])))
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [kind])
  useEffect(() => {
    setEntries([])
    void reload()
    return events.onAny((raw) => {
      const ev = raw as unknown as ServerLogEvent
      if (ev.type !== 'server.log' || ev.kind !== kind || !Array.isArray(ev.entries)) return
      setEntries((cur) => mergeEntries(cur, ev.entries))
    })
  }, [kind, reload])
  return { entries, setEntries, loading, error, reload }
}

function LevelIcon({ level }: { level: LogLevel }) {
  if (level === 'error') return <CircleAlert className="size-3.5 text-destructive" aria-label="error" />
  if (level === 'warn') return <TriangleAlert className="size-3.5 text-warning" aria-label="warning" />
  return <Info className="size-3.5 text-muted-foreground/60" aria-label={level} />
}

function timeOf(ts: string): string {
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString(undefined, { hour12: false })
}

function LogPane({ kind }: { kind: ServerKindEx }) {
  const { entries, setEntries, loading, error, reload } = useServerLog(kind)
  const showLoading = useDelayedFlag(loading && !entries.length)
  const [level, setLevel] = useState<LevelFilter>('info')
  const [query, setQuery] = useState('')
  const [paused, setPaused] = useState<ServerLogEntry[] | null>(null)
  const scroller = useRef<HTMLDivElement>(null)
  const stick = useRef(true)

  const source = paused ?? entries
  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    const min = LEVEL_RANK[level]
    return source.filter(
      (e) =>
        LEVEL_RANK[e.level] >= min &&
        (!q || e.message.toLowerCase().includes(q) || (e.client ?? '').includes(q) || (e.user ?? '').toLowerCase().includes(q)),
    )
  }, [source, level, query])

  const virt = useVirtualizer({
    count: shown.length,
    getScrollElement: () => scroller.current,
    estimateSize: () => 22,
    overscan: 20,
  })
  useEffect(() => {
    if (stick.current && shown.length) virt.scrollToIndex(shown.length - 1, { align: 'end' })
  }, [shown.length, virt])

  const pausedNew = paused ? entries.filter((e) => e.id > (paused[paused.length - 1]?.id ?? 0)).length : 0

  const clear = async () => {
    if (!(await confirm({ title: 'Clear the activity log?', confirmLabel: 'Clear', destructive: true }))) return
    try {
      await clearServerLogs(kind)
      setEntries([])
      setPaused(null)
    } catch (err) {
      toast.error('Cannot clear the log', { description: errorMessage(err) })
    }
  }

  const download = () => {
    const text = shown
      .map((e) => [e.ts, e.level.toUpperCase().padEnd(5), e.client ?? '-', e.user || '-', e.message].join(' '))
      .join('\n')
    const url = URL.createObjectURL(new Blob([text + '\n'], { type: 'text/plain;charset=utf-8' }))
    const a = document.createElement('a')
    a.href = url
    a.download = `astraterm-${kind}-server-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.log`
    a.click()
    setTimeout(() => URL.revokeObjectURL(url), 5000)
  }

  return (
    <>
      <div className="flex flex-wrap items-center gap-1.5 border-b px-2 py-1.5">
        <SegmentedControl
          size="sm"
          value={level}
          onValueChange={setLevel}
          aria-label="Minimum level"
          options={[
            { value: 'debug', label: 'Debug' },
            { value: 'info', label: 'Info' },
            { value: 'warn', label: 'Warnings' },
            { value: 'error', label: 'Errors' },
          ]}
        />
        <Input
          inputSize="sm"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Filter…"
          aria-label="Filter the log"
          leading={<Search />}
          className="w-40 flex-1"
        />
        <IconButton
          icon={paused ? Play : Pause}
          label={paused ? `Resume${pausedNew ? ` (${pausedNew} new)` : ''}` : 'Pause'}
          active={!!paused}
          onClick={() => setPaused((p) => (p ? null : entries))}
        />
        <IconButton icon={Download} label="Download the shown lines" onClick={download} disabled={!shown.length} />
        <IconButton icon={Eraser} label="Clear the log" onClick={() => void clear()} disabled={!entries.length} />
      </div>
      <div
        ref={scroller}
        className="min-h-0 flex-1 overflow-auto font-mono text-xs"
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
        }}
        role="log"
        aria-live={paused ? 'off' : 'polite'}
        aria-relevant="additions"
      >
        {(loading && !entries.length) || showLoading ? (
          showLoading ? <LoadingPane immediate /> : null
        ) : error ? (
          <EmptyState
            size="sm"
            icon={CircleAlert}
            title="Cannot load the log"
            description={error}
            action={
              <Button size="sm" variant="secondary" onClick={() => void reload()}>
                Retry
              </Button>
            }
          />
        ) : !shown.length ? (
          <EmptyState
            size="sm"
            title={entries.length ? 'No matching lines' : 'No activity yet'}
            description={entries.length ? 'Change the filter to see more.' : 'Connections, logins and transfers appear here live.'}
          />
        ) : (
          <div style={{ height: virt.getTotalSize(), position: 'relative' }}>
            {virt.getVirtualItems().map((row) => {
              const e = shown[row.index]
              return (
                <div
                  key={e.id}
                  data-index={row.index}
                  ref={virt.measureElement}
                  className={cn(
                    'absolute inset-x-0 flex items-start gap-2 px-2 py-0.5 hover:bg-accent/40',
                    e.level === 'error' && 'bg-destructive/5',
                    e.level === 'warn' && 'bg-warning/5',
                  )}
                  style={{ transform: `translateY(${row.start}px)` }}
                >
                  <span className="shrink-0 text-muted-foreground tabular-nums" title={e.ts}>
                    {timeOf(e.ts)}
                  </span>
                  <span className="mt-px shrink-0">
                    <LevelIcon level={e.level} />
                  </span>
                  {e.client && <span className="shrink-0 text-muted-foreground">{e.client}</span>}
                  {e.user && <span className="shrink-0 text-primary">{e.user}</span>}
                  <span className="min-w-0 break-words whitespace-pre-wrap">{e.message}</span>
                </div>
              )
            })}
          </div>
        )}
      </div>
      <div className="flex items-center gap-2 border-t px-2 py-1 text-xs text-muted-foreground">
        <span>
          {shown.length === source.length ? `${source.length} lines` : `${shown.length} of ${source.length} lines`}
        </span>
        {paused && <span className="text-warning">Paused{pausedNew ? ` · ${pausedNew} new` : ''}</span>}
      </div>
    </>
  )
}

// ---- clients ------------------------------------------------------------------------------------------------------------

function ClientsPane({ kind, running }: { kind: ServerKindEx; running: boolean }) {
  const now = useNow(5_000)
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: serverKeys.clients(kind),
    placeholderData: undefined, // never show another server's clients under this key
    queryFn: () => listServerClients(kind),
    refetchInterval: running ? 2_000 : false,
    enabled: running || kind === 'syslog',
  })
  const showLoading = useDelayedFlag(isLoading)
  const kick = async (id: string, addr: string) => {
    if (!(await confirm({ title: `Disconnect ${addr}?`, confirmLabel: 'Disconnect', destructive: true }))) return
    try {
      await disconnectClient(kind, id)
      toast.success(`Disconnected ${addr}`)
      void refetch()
    } catch (err) {
      toast.error('Cannot disconnect the client', { description: errorMessage(err) })
    }
  }
  if (!running && kind !== 'syslog') {
    return <EmptyState size="sm" icon={Users} title="The server is stopped" description="Start it to see connected clients." />
  }
  if (isLoading || showLoading) return showLoading ? <LoadingPane immediate /> : null
  if (error) {
    return (
      <EmptyState
        size="sm"
        icon={CircleAlert}
        title="Cannot list the clients"
        description={errorMessage(error)}
        action={
          <Button size="sm" variant="secondary" onClick={() => void refetch()}>
            Retry
          </Button>
        }
      />
    )
  }
  if (!data?.length) {
    return (
      <EmptyState
        size="sm"
        icon={Users}
        title={kind === 'syslog' ? 'No senders in the last 5 minutes' : 'No clients connected'}
        description={kind === 'syslog' ? 'Devices that send messages appear here.' : 'Connected clients appear here live.'}
      />
    )
  }
  const syslog = kind === 'syslog'
  return (
    <table className="w-full text-sm">
      <thead className="sticky top-0 bg-popover text-xs text-muted-foreground">
        <tr className="border-b">
          <th className="px-3 py-1.5 text-left font-medium">Address</th>
          {!syslog && <th className="px-2 py-1.5 text-left font-medium">User</th>}
          <th className="px-2 py-1.5 text-left font-medium">{syslog ? 'Last message' : 'Connected'}</th>
          <th className="px-2 py-1.5 text-left font-medium">{syslog ? 'Messages' : 'Activity'}</th>
          <th className="px-2 py-1.5 text-right font-medium">Traffic</th>
          {!syslog && <th className="w-8" />}
        </tr>
      </thead>
      <tbody>
        {data.map((c) => (
          <tr key={c.id} className="border-b border-border/60 hover:bg-accent/30">
            <td className="px-3 py-1.5 font-mono text-xs">{c.addr}</td>
            {!syslog && <td className="px-2 py-1.5">{c.user || <span className="text-muted-foreground">—</span>}</td>}
            <td className="px-2 py-1.5 whitespace-nowrap text-muted-foreground">{formatRelativeTime(c.since, now)}</td>
            <td className="max-w-40 truncate px-2 py-1.5 text-muted-foreground" title={c.activity}>
              {c.activity || '—'}
            </td>
            <td className="px-2 py-1.5 text-right text-xs whitespace-nowrap text-muted-foreground tabular-nums">
              ↓{formatBytes(c.bytesIn)}
              {!syslog && ` ↑${formatBytes(c.bytesOut)}`}
            </td>
            {!syslog && (
              <td className="px-1 py-1">
                <IconButton icon={Unplug} size="xs" label={`Disconnect ${c.addr}`} onClick={() => void kick(c.id, c.addr)} />
              </td>
            )}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// ---- connect --------------------------------------------------------------------------------------------------------

function ConnectPane({ kind }: { kind: ServerKindEx }) {
  const status = useServer(kind)
  if (!status) return null
  const cmds = clientCommands(status)
  return (
    <div className="grid gap-4 p-3">
      {!status.running && (
        <p className="rounded-md border bg-muted/40 px-3 py-2 text-sm text-muted-foreground">
          The server is stopped: start it before connecting. Addresses below use the configured listen address.
        </p>
      )}
      {status.url && (
        <div className="grid gap-1">
          <div className="text-xs font-medium text-muted-foreground">Address</div>
          <CommandLine text={status.url} />
          {status.addrs && status.addrs.length > 1 && (
            <div className="text-xs text-muted-foreground">Listening: {status.addrs.join(', ')}</div>
          )}
        </div>
      )}
      <div className="grid gap-2">
        {cmds.map((c) => (
          <div key={c.label} className="grid gap-1">
            <div className="text-xs font-medium text-muted-foreground">{c.label}</div>
            <CommandLine text={c.command} />
          </div>
        ))}
      </div>
      {status.fingerprint && (
        <div className="grid gap-1">
          <div className="text-xs font-medium text-muted-foreground">
            {kind === 'sftp' ? 'Host key fingerprint (verify on first connect)' : 'TLS certificate SHA-256 (self-signed)'}
          </div>
          <CommandLine text={status.fingerprint} />
        </div>
      )}
      {kind === 'ftp' && (
        <p className="text-sm text-muted-foreground">
          Behind a firewall, open the control port and the passive port range (Advanced settings) and set the public
          address for passive mode when clients come through NAT.
        </p>
      )}
      {kind === 'tftp' && (
        <p className="text-sm text-muted-foreground">
          TFTP answers from a new port for every transfer. Through NAT or strict firewalls enable “Single port” in the
          advanced settings.
        </p>
      )}
    </div>
  )
}

function CommandLine({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-1 rounded-md border bg-muted/40 py-1 pr-1 pl-2.5">
      <code className="min-w-0 flex-1 truncate font-mono text-xs" title={text}>
        {text}
      </code>
      <IconButton icon={Copy} size="xs" label="Copy" onClick={() => void copyWithToast(text)} />
    </div>
  )
}
