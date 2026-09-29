/*
 * The "syslog" tab (CC-5, Tftpd64-style syslog viewer): live messages from the embedded syslog server with
 * text / regex search, severity / facility / host filters, highlight rules, pause, load older, export and clear.
 */
import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import {
  ArrowDownToLine,
  CircleAlert,
  Copy,
  Download,
  Eraser,
  Highlighter,
  History,
  Pause,
  Play,
  Plus,
  Regex,
  ScrollText,
  Search,
  Settings2,
  ShieldAlert,
  Trash2,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import type { TabProps } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { Toolbar, ToolbarSeparator } from '@/components/ui/toolbar'
import { Tooltip } from '@/components/ui/tooltip'
import { events, useEventsTopic } from '@/lib/events'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage, uid } from '@/lib/utils'
import { copyWithToast, startServerAction, stopServerAction } from './actions'
import { clearSyslogMessages, getSyslogMessages, syslogExportUrl, useServer, useServersAllowed } from './api'
import { StatusDot } from './components'
import { FACILITIES, HIGHLIGHT_COLORS, SEVERITIES, SEVERITY_SHORT, facilityName, highlightClass, severityClass } from './model'
import { serversSettings } from './settings'
import { openServerConfig } from './store'
import type { HighlightRule, SyslogEvent, SyslogMessage } from './types'

const PAGE = 1000
const ROW = 24

/** Table columns: narrow panels show time / severity / message, wider ones add host, then everything. */
const GRID =
  'grid grid-cols-[4.75rem_4.25rem_minmax(0,1fr)] @xl:grid-cols-[6.5rem_8rem_4.25rem_minmax(0,1fr)] ' +
  '@3xl:grid-cols-[6.5rem_7.5rem_8rem_4.5rem_4.25rem_8rem_minmax(0,1fr)]'
const WIDE = 'hidden @3xl:block'
const MEDIUM = 'hidden @xl:block'

type Matcher = (m: SyslogMessage) => boolean

function compileText(q: string, regex: boolean): { match?: Matcher; error?: string } {
  const text = q.trim()
  if (!text) return {}
  if (regex) {
    try {
      const re = new RegExp(text, 'i')
      return { match: (m) => re.test(m.message) || re.test(m.hostname ?? '') || re.test(m.appName ?? '') }
    } catch (err) {
      return { error: errorMessage(err, 'Invalid regular expression') }
    }
  }
  const t = text.toLowerCase()
  return {
    match: (m) =>
      m.message.toLowerCase().includes(t) ||
      (m.hostname ?? '').toLowerCase().includes(t) ||
      (m.appName ?? '').toLowerCase().includes(t) ||
      m.source.includes(t) ||
      (m.msgId ?? '').toLowerCase().includes(t),
  }
}

function compileRules(rules: HighlightRule[]): { test: (s: string) => boolean; cls: string }[] {
  const out: { test: (s: string) => boolean; cls: string }[] = []
  for (const r of rules) {
    if (!r.enabled || !r.pattern.trim()) continue
    if (r.regex) {
      try {
        const re = new RegExp(r.pattern, 'i')
        out.push({ test: (s) => re.test(s), cls: highlightClass(r.color) })
      } catch {
        /* invalid rules are skipped (the editor flags them) */
      }
    } else {
      const p = r.pattern.toLowerCase()
      out.push({ test: (s) => s.toLowerCase().includes(p), cls: highlightClass(r.color) })
    }
  }
  return out
}

function when(m: SyslogMessage): Date {
  return new Date(m.timestamp ?? m.received)
}

function fmtTime(m: SyslogMessage): string {
  const d = when(m)
  if (Number.isNaN(d.getTime())) return ''
  const today = new Date()
  const time = d.toLocaleTimeString(undefined, { hour12: false })
  return d.toDateString() === today.toDateString() ? time : `${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })} ${time}`
}

function merge(cur: SyslogMessage[], add: SyslogMessage[], limit: number): SyslogMessage[] {
  if (!add.length) return cur
  const last = cur.length ? cur[cur.length - 1].id : 0
  const fresh = add.filter((m) => m.id > last)
  if (!fresh.length) return cur
  const next = cur.concat(fresh)
  return next.length > limit ? next.slice(next.length - limit) : next
}

export default function SyslogView(_props: TabProps) {
  const allowed = useServersAllowed()
  if (!allowed) {
    return (
      <EmptyState
        icon={ShieldAlert}
        title="Administrators only"
        description="In server mode the syslog server runs on the NexTerm host and only administrators can read its messages."
        className="h-full"
      />
    )
  }
  return <SyslogViewer />
}

function SyslogViewer() {
  const status = useServer('syslog')
  const settings = serversSettings.use()
  const limit = Math.max(500, Math.min(100_000, settings.syslogViewerLimit))
  const [buffer, setBuffer] = useState<SyslogMessage[]>([])
  const [hasOlder, setHasOlder] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string>()
  const [olderBusy, setOlderBusy] = useState(false)
  const [paused, setPaused] = useState(false)
  const queued = useRef<SyslogMessage[]>([])
  const [queuedCount, setQueuedCount] = useState(0)
  const [q, setQ] = useState('')
  const [regex, setRegex] = useState(false)
  const [minSev, setMinSev] = useState(7)
  const [facility, setFacility] = useState(-1)
  const [host, setHost] = useState('')
  const [selected, setSelected] = useState<number | null>(null)
  const scroller = useRef<HTMLDivElement>(null)
  const searchRef = useRef<HTMLInputElement>(null)
  const stick = useRef(true)
  const pausedRef = useRef(paused)
  pausedRef.current = paused
  const limitRef = useRef(limit)
  limitRef.current = limit

  useEventsTopic('syslog')

  /** Load the newest page. `quiet` (resyncs) keeps the current view and controls as they are. */
  const load = useCallback(async (quiet = false) => {
    if (!quiet) {
      setLoading(true)
      setLoadError(undefined)
    }
    try {
      const page = await getSyslogMessages({ limit: PAGE })
      setBuffer((cur) => merge(page.messages, cur, limitRef.current))
      setHasOlder(page.hasMore)
      setLoadError(undefined)
    } catch (err) {
      if (!quiet) setLoadError(errorMessage(err))
    } finally {
      if (!quiet) setLoading(false)
    }
  }, [])

  // The live stream drops messages under a flood ({dropped}); fill the gap from the server, at most every 2 s.
  const needResync = useRef(false)
  const lastResync = useRef(0)
  const resyncTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const resync = useCallback(() => {
    if (resyncTimer.current) return // a trailing resync is already scheduled
    const wait = Math.max(0, lastResync.current + 2000 - Date.now())
    resyncTimer.current = setTimeout(() => {
      resyncTimer.current = undefined
      if (pausedRef.current) return // resumed later: resume() resyncs
      lastResync.current = Date.now()
      needResync.current = false
      void load(true)
    }, wait)
  }, [load])
  useEffect(() => () => clearTimeout(resyncTimer.current), [])

  useEffect(() => {
    void load()
    const off = events.onAny((raw) => {
      const ev = raw as unknown as SyslogEvent
      if (ev.type !== 'syslog' || !Array.isArray(ev.messages)) return
      if (ev.dropped) needResync.current = true
      if (pausedRef.current) {
        queued.current = queued.current.concat(ev.messages).slice(-limitRef.current)
        setQueuedCount(queued.current.length)
        return
      }
      setBuffer((cur) => merge(cur, ev.messages, limitRef.current))
      if (needResync.current) resync()
    })
    const offHello = events.on('hello', () => void load(true)) // messages may have been missed while disconnected
    return () => {
      off()
      offHello()
    }
  }, [load, resync])

  const resume = () => {
    setBuffer((cur) => merge(cur, queued.current, limitRef.current))
    queued.current = []
    setQueuedCount(0)
    setPaused(false)
    stick.current = true
    if (needResync.current) resync()
  }
  const showLoading = useDelayedFlag(loading && !buffer.length)

  const loadOlder = async () => {
    if (!buffer.length) return
    setOlderBusy(true)
    try {
      const page = await getSyslogMessages({ before: buffer[0].id, limit: PAGE })
      setBuffer((cur) => {
        const known = new Set(cur.map((m) => m.id))
        return page.messages.filter((m) => !known.has(m.id)).concat(cur)
      })
      setHasOlder(page.hasMore)
      stick.current = false
    } catch (err) {
      toast.error('Cannot load older messages', { description: errorMessage(err) })
    } finally {
      setOlderBusy(false)
    }
  }

  const clearAll = async () => {
    if (!(await confirm({ title: 'Clear all syslog messages?', description: 'The server buffer is emptied for everyone. Log files are kept.', confirmLabel: 'Clear', destructive: true }))) return
    try {
      await clearSyslogMessages()
      setBuffer([])
      queued.current = []
      setQueuedCount(0)
      setHasOlder(false)
      setSelected(null)
    } catch (err) {
      toast.error('Cannot clear the messages', { description: errorMessage(err) })
    }
  }

  const text = useMemo(() => compileText(q, regex), [q, regex])
  const rules = useMemo(() => compileRules(settings.syslogHighlights), [settings.syslogHighlights])
  const shown = useMemo(
    () =>
      buffer.filter(
        (m) =>
          m.severity <= minSev &&
          (facility < 0 || m.facility === facility) &&
          (!host || m.source === host || m.hostname === host) &&
          (!text.match || text.match(m)),
      ),
    [buffer, minSev, facility, host, text],
  )
  const hosts = useMemo(() => {
    const counts = new Map<string, number>()
    for (const m of buffer) {
      counts.set(m.source, (counts.get(m.source) ?? 0) + 1)
      if (m.hostname && m.hostname !== m.source) counts.set(m.hostname, (counts.get(m.hostname) ?? 0) + 1)
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 60).map(([h]) => h)
  }, [buffer])

  const virt = useVirtualizer({ count: shown.length, getScrollElement: () => scroller.current, estimateSize: () => ROW, overscan: 25 })
  useEffect(() => {
    if (settings.syslogAutoScroll && stick.current && shown.length) virt.scrollToIndex(shown.length - 1, { align: 'end' })
  }, [shown.length, settings.syslogAutoScroll, virt])

  const selIndex = selected == null ? -1 : shown.findIndex((m) => m.id === selected)
  const sel = selIndex >= 0 ? shown[selIndex] : undefined

  const onTableKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!shown.length) return
    const page = Math.max(1, Math.floor((scroller.current?.clientHeight ?? 240) / ROW) - 1)
    let i = selIndex
    switch (e.key) {
      case 'ArrowDown':
        i = Math.min(shown.length - 1, i + 1)
        break
      case 'ArrowUp':
        i = i < 0 ? shown.length - 1 : Math.max(0, i - 1)
        break
      case 'PageDown':
        i = Math.min(shown.length - 1, i + page)
        break
      case 'PageUp':
        i = Math.max(0, (i < 0 ? shown.length - 1 : i) - page)
        break
      case 'Home':
        i = 0
        break
      case 'End':
        i = shown.length - 1
        break
      case 'Escape':
        setSelected(null)
        return
      default:
        return
    }
    e.preventDefault()
    stick.current = i === shown.length - 1
    setSelected(shown[i].id)
    virt.scrollToIndex(i, { align: 'auto' })
  }

  const exportHref = syslogExportUrl({
    q: text.error ? undefined : q,
    regex,
    severity: minSev,
    facility,
    host: host || undefined,
  })
  const filtersActive = !!q.trim() || minSev < 7 || facility >= 0 || !!host
  const running = !!status?.running

  return (
    <div
      className="flex h-full min-h-0 flex-col"
      onKeyDown={(e) => {
        const t = e.target as HTMLElement
        if (e.key === '/' && !(t instanceof HTMLInputElement) && !(t instanceof HTMLTextAreaElement)) {
          e.preventDefault()
          searchRef.current?.focus()
        }
      }}
    >
      <Toolbar aria-label="Syslog server" className="h-auto min-h-9 flex-wrap gap-1.5 py-1">
        <div className="flex shrink-0 items-center gap-2 px-1">
          <ScrollText className="size-4 shrink-0 text-muted-foreground" aria-hidden />
          <span className="font-medium whitespace-nowrap">Syslog</span>
          {status && <StatusDot status={status} />}
        </div>
        {status && (
          <Button
            size="xs"
            variant={running ? 'secondary' : 'default'}
            disabled={status.state === 'starting' || status.state === 'stopping'}
            onClick={() => void (running ? stopServerAction('syslog') : startServerAction('syslog'))}
          >
            {running ? 'Stop' : 'Start'}
          </Button>
        )}
        <IconButton icon={Settings2} label="Syslog server settings" onClick={() => openServerConfig('syslog')} />
        <ToolbarSeparator />
        <Input
          ref={searchRef}
          inputSize="sm"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={regex ? 'Regular expression' : 'Search messages ( / )'}
          aria-label="Search messages"
          aria-invalid={!!text.error || undefined}
          leading={<Search />}
          trailing={
            q ? (
              <button type="button" className="rounded-sm p-0.5 hover:text-foreground" aria-label="Clear the search" onClick={() => setQ('')}>
                <X className="size-3.5" />
              </button>
            ) : undefined
          }
          className="w-44 min-w-28 flex-1 @3xl:w-52 @3xl:flex-none"
        />
        <IconButton icon={Regex} label="Regular expression" active={regex} onClick={() => setRegex((v) => !v)} />
        <SimpleSelect
          size="sm"
          className="w-34"
          aria-label="Minimum severity"
          value={String(minSev)}
          onValueChange={(v) => setMinSev(Number(v))}
          options={[
            { value: '7', label: 'All severities' },
            ...SEVERITIES.slice(0, 7).map((s, i) => ({ value: String(i), label: i === 0 ? 'Emergency only' : `${s} and worse` })),
          ]}
        />
        <SimpleSelect
          size="sm"
          className="hidden w-32 @4xl:flex"
          aria-label="Facility"
          value={String(facility)}
          onValueChange={(v) => setFacility(Number(v))}
          options={[{ value: '-1', label: 'All facilities' }, ...FACILITIES.map((f, i) => ({ value: String(i), label: f }))]}
        />
        <SimpleSelect
          size="sm"
          className="hidden w-36 @4xl:flex"
          aria-label="Host"
          value={host || '__all'}
          onValueChange={(v) => setHost(v === '__all' ? '' : v)}
          options={[{ value: '__all', label: 'All hosts' }, ...hosts.map((h) => ({ value: h, label: h }))]}
        />
        <div className="flex-1" />
        <IconButton
          icon={paused ? Play : Pause}
          label={paused ? `Resume${queuedCount ? ` (${queuedCount} new)` : ''}` : 'Pause the live view'}
          active={paused}
          onClick={() => (paused ? resume() : setPaused(true))}
        />
        <IconButton
          icon={ArrowDownToLine}
          label="Follow new messages"
          active={settings.syslogAutoScroll}
          onClick={() => serversSettings.set({ syslogAutoScroll: !settings.syslogAutoScroll })}
        />
        <HighlightRules rules={settings.syslogHighlights} />
        <Tooltip content="Export (filtered, whole server buffer)">
          <Button asChild variant="ghost" size="icon-sm" aria-label="Export messages">
            <a href={exportHref} download>
              <Download />
            </a>
          </Button>
        </Tooltip>
        <IconButton icon={Eraser} label="Clear all messages" onClick={() => void clearAll()} disabled={!buffer.length} />
      </Toolbar>

      {text.error && (
        <p role="alert" className="border-b bg-destructive/10 px-3 py-1 text-sm text-destructive">
          {text.error}
        </p>
      )}

      <div className={cn(GRID, 'shrink-0 border-b bg-muted/40 px-2 text-xs font-medium text-muted-foreground select-none')}>
        <div className="py-1">Time</div>
        <div className={cn('py-1', WIDE)}>Source</div>
        <div className={cn('py-1', MEDIUM)}>Host</div>
        <div className={cn('py-1', WIDE)}>Facility</div>
        <div className="py-1">Severity</div>
        <div className={cn('py-1', WIDE)}>App</div>
        <div className="py-1">Message</div>
      </div>

      <div
        ref={scroller}
        tabIndex={0}
        role="grid"
        aria-label="Syslog messages"
        aria-rowcount={shown.length}
        onKeyDown={onTableKey}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < ROW * 2
        }}
        className="min-h-0 flex-1 overflow-auto font-mono text-xs outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/40"
      >
        {hasOlder && !loading && (
          <div className="flex justify-center py-1.5">
            <Button size="xs" variant="ghost" onClick={() => void loadOlder()} loading={olderBusy}>
              <History /> Load older messages
            </Button>
          </div>
        )}
        {(loading && !buffer.length) || showLoading ? (
          showLoading ? <LoadingPane immediate label="Loading messages…" /> : null
        ) : loadError ? (
          <EmptyState
            size="sm"
            icon={CircleAlert}
            title="Cannot load the messages"
            description={loadError}
            action={
              <Button size="sm" variant="secondary" onClick={() => void load()}>
                Retry
              </Button>
            }
          />
        ) : !shown.length ? (
          <EmptyState
            size="sm"
            icon={ScrollText}
            title={buffer.length ? 'No message matches the filters' : running ? 'Waiting for messages…' : 'The syslog server is stopped'}
            description={
              buffer.length
                ? 'Change or clear the filters.'
                : running
                  ? `Point devices at ${status?.url?.replace('syslog://', '') ?? 'this server'} (UDP / TCP).`
                  : 'Start it to receive messages from routers, switches and servers.'
            }
            action={
              !buffer.length && !running && status ? (
                <Button size="sm" onClick={() => void startServerAction('syslog')}>
                  Start syslog server
                </Button>
              ) : filtersActive ? (
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => {
                    setQ('')
                    setMinSev(7)
                    setFacility(-1)
                    setHost('')
                  }}
                >
                  Clear filters
                </Button>
              ) : undefined
            }
          />
        ) : (
          <div style={{ height: virt.getTotalSize(), position: 'relative' }}>
            {virt.getVirtualItems().map((row) => {
              const m = shown[row.index]
              const rule = rules.find((r) => r.test(m.message) || r.test(m.appName ?? ''))
              const isSel = m.id === selected
              return (
                <div
                  key={m.id}
                  role="row"
                  aria-rowindex={row.index + 1}
                  aria-selected={isSel}
                  onClick={() => setSelected(isSel ? null : m.id)}
                  className={cn(
                    GRID,
                    'absolute inset-x-0 cursor-default items-center border-b border-border/40 px-2',
                    rule?.cls,
                    !rule && 'hover:bg-accent/40',
                    isSel && 'outline outline-1 -outline-offset-1 outline-primary bg-primary/10',
                  )}
                  style={{ height: ROW, transform: `translateY(${row.start}px)` }}
                >
                  <div role="gridcell" className="truncate tabular-nums" title={`device: ${m.timestamp ?? '—'}\nreceived: ${m.received}`}>
                    {fmtTime(m)}
                  </div>
                  <div role="gridcell" className={cn('truncate', WIDE)} title={`${m.source} (${m.transport})`}>
                    {m.source}
                  </div>
                  <div role="gridcell" className={cn('truncate', MEDIUM)} title={m.hostname}>
                    {m.hostname || '—'}
                  </div>
                  <div role="gridcell" className={cn('truncate', WIDE)}>
                    {facilityName(m.facility)}
                  </div>
                  <div role="gridcell">
                    <span className={cn('inline-block rounded-[3px] border px-1 text-[11px] leading-4', severityClass(m.severity))}>
                      {SEVERITY_SHORT[m.severity]}
                    </span>
                  </div>
                  <div role="gridcell" className={cn('truncate', WIDE)} title={m.appName}>
                    {m.appName ? `${m.appName}${m.procId ? `[${m.procId}]` : ''}` : '—'}
                  </div>
                  <div role="gridcell" className="truncate" title={m.message}>
                    {m.message}
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>

      {sel && settings.syslogShowDetails && <Details m={sel} onClose={() => setSelected(null)} />}

      <div className="flex shrink-0 items-center gap-3 border-t px-3 py-1 text-xs text-muted-foreground" aria-live="polite">
        <span>
          {shown.length === buffer.length ? `${buffer.length} messages` : `${shown.length} of ${buffer.length} messages`}
          {hasOlder && ' · older messages on the server'}
        </span>
        {paused && <span className="text-warning">Paused{queuedCount ? ` · ${queuedCount} new` : ''}</span>}
        <div className="flex-1" />
        {status?.running ? (
          <span className="truncate" title={(status.addrs ?? []).join(', ')}>
            {status.clients} sender{status.clients === 1 ? '' : 's'} in the last 5 min · listening on{' '}
            <span className="font-mono">{(status.addrs ?? []).join(', ')}</span>
          </span>
        ) : (
          <span>Syslog server stopped</span>
        )}
      </div>
    </div>
  )
}

function Details({ m, onClose }: { m: SyslogMessage; onClose: () => void }) {
  const rows: [string, string | undefined][] = [
    ['Received', new Date(m.received).toLocaleString()],
    ['Device time', m.timestamp ? new Date(m.timestamp).toLocaleString() : undefined],
    ['Source', `${m.source} (${m.transport.toUpperCase()})`],
    ['Host', m.hostname],
    ['Facility', `${facilityName(m.facility)} (${m.facility})`],
    ['Severity', `${SEVERITIES[m.severity] ?? m.severity} (${m.severity})`],
    ['Application', m.appName ? `${m.appName}${m.procId ? ` [${m.procId}]` : ''}` : undefined],
    ['Message ID', m.msgId],
    ['Format', m.format === 'rfc5424' ? 'RFC 5424' : m.format === 'rfc3164' ? 'RFC 3164 (BSD)' : 'unstructured'],
  ]
  return (
    <section aria-label="Message details" className="max-h-56 shrink-0 overflow-auto border-t bg-card px-3 py-2">
      <div className="mb-1.5 flex items-center gap-2">
        <h3 className="text-sm font-semibold">Message #{m.id}</h3>
        <div className="flex-1" />
        <IconButton icon={Copy} size="xs" label="Copy the message" onClick={() => void copyWithToast(m.message)} />
        <IconButton icon={X} size="xs" label="Close details" onClick={onClose} />
      </div>
      <dl className="grid grid-cols-[7rem_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-xs">
        {rows
          .filter(([, v]) => v)
          .map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="truncate">{v}</dd>
            </div>
          ))}
        {m.structuredData && (
          <>
            <dt className="text-muted-foreground">Structured data</dt>
            <dd className="font-mono break-all">{m.structuredData}</dd>
          </>
        )}
      </dl>
      <pre className="mt-2 rounded-md border bg-muted/40 p-2 font-mono text-xs break-words whitespace-pre-wrap">{m.message}</pre>
    </section>
  )
}

function HighlightRules({ rules }: { rules: HighlightRule[] }) {
  const update = (next: HighlightRule[]) => serversSettings.set({ syslogHighlights: next })
  const patch = (id: string, p: Partial<HighlightRule>) => update(rules.map((r) => (r.id === id ? { ...r, ...p } : r)))
  return (
    <Popover>
      <Tooltip content="Highlight rules">
        <PopoverTrigger asChild>
          <Button variant="ghost" size="icon-sm" aria-label="Highlight rules">
            <Highlighter />
          </Button>
        </PopoverTrigger>
      </Tooltip>
      <PopoverContent align="end" className="w-[26rem] p-3">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-sm font-semibold">Highlight rules</h3>
          <span className="text-xs text-muted-foreground">First match colours the row</span>
        </div>
        <ul className="grid gap-1.5">
          {rules.map((r) => {
            let invalid = false
            if (r.regex) {
              try {
                new RegExp(r.pattern)
              } catch {
                invalid = true
              }
            }
            return (
              <li key={r.id} className="flex items-center gap-1.5">
                <Checkbox checked={r.enabled} onCheckedChange={(v) => patch(r.id, { enabled: v === true })} aria-label="Enabled" />
                <Input
                  inputSize="sm"
                  value={r.pattern}
                  onChange={(e) => patch(r.id, { pattern: e.target.value })}
                  aria-label="Pattern"
                  aria-invalid={invalid || undefined}
                  className="min-w-0 flex-1 font-mono"
                  spellCheck={false}
                />
                <IconButton icon={Regex} size="xs" label="Regular expression" active={r.regex} onClick={() => patch(r.id, { regex: !r.regex })} />
                <SimpleSelect
                  size="sm"
                  className="w-24"
                  aria-label="Colour"
                  value={r.color}
                  onValueChange={(v) => patch(r.id, { color: v })}
                  options={HIGHLIGHT_COLORS.map((c) => ({
                    value: c.id,
                    label: <span className={cn('rounded-sm px-1', c.className)}>{c.label}</span>,
                  }))}
                />
                <IconButton icon={Trash2} size="xs" label="Delete rule" onClick={() => update(rules.filter((x) => x.id !== r.id))} />
              </li>
            )
          })}
        </ul>
        <div className="mt-2 flex gap-2">
          <Button
            size="xs"
            variant="secondary"
            onClick={() => update([...rules, { id: uid('hl'), pattern: '', regex: false, color: 'amber', enabled: true }])}
            disabled={rules.length >= 30}
          >
            <Plus /> Add rule
          </Button>
          <Button size="xs" variant="ghost" onClick={() => serversSettings.reset(['syslogHighlights'])}>
            Restore defaults
          </Button>
        </div>
      </PopoverContent>
    </Popover>
  )
}
