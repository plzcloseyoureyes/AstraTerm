/*
 * Log follower (MON-5): tail -F of several files merged (or the systemd journal, optionally one unit) over a
 * WebSocket, with per-source colours, level colouring and filtering, pause, wrap, jump-to-time and download.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDownToLine, CircleStop, Clock, Download, Eraser, FileText, Filter, Pause, Play, ScrollText, ShieldCheck, WrapText } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Spinner } from '@/components/ui/spinner'
import { StatusDot } from '@/components/ui/status-dot'
import { TagInput } from '@/components/ui/tag-input'
import { Toolbar, ToolbarSeparator, ToolbarSpacer } from '@/components/ui/toolbar'
import { cn } from '@/lib/utils'
import { tailUrl } from './api'
import { monitorSettings } from './settings'
import type { TailFrame, TargetId } from './types'

const MAX_LINES = 20_000
const SOURCE_COLORS = ['var(--primary)', 'var(--success)', 'var(--warning)', 'var(--info)', 'var(--destructive)', 'oklch(0.65 0.2 300)', 'oklch(0.7 0.15 190)', 'oklch(0.7 0.17 20)']

type Lvl = 'error' | 'warn' | 'debug' | ''
interface Line {
  id: number
  src: number
  text: string
  lvl: Lvl
}

const ERR_RE = /\b(emerg(ency)?|alert|crit(ical)?|fatal|err(or)?|fail(ed|ure)?|panic|exception|segfault|denied|refused)\b/i
const WARN_RE = /\b(warn(ing)?|deprecated|timed? ?out|retry(ing)?|unable)\b/i
const DEBUG_RE = /\b(debug|trace|verbose)\b/i

function classify(text: string): Lvl {
  if (ERR_RE.test(text)) return 'error'
  if (WARN_RE.test(text)) return 'warn'
  if (DEBUG_RE.test(text)) return 'debug'
  return ''
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const ISO_RE = /^(\d{4})[-/](\d{2})[-/](\d{2})[T ](\d{2}):(\d{2}):(\d{2})(?:[.,]\d+)?(Z|[+-]\d{2}:?\d{2})?/
const SYSLOG_RE = /^([A-Z][a-z]{2})\s+(\d{1,2}) (\d{2}):(\d{2}):(\d{2})/

/** Timestamp at the start of a log line (ISO 8601, journal short-iso, nginx, classic syslog), ms epoch. */
function lineTime(text: string, now = new Date()): number | undefined {
  let m = ISO_RE.exec(text)
  if (m) {
    const [, y, mo, d, h, mi, s, tz] = m
    const zone = tz ? (tz === 'Z' ? 'Z' : `${tz.slice(0, 3)}:${tz.slice(-2)}`) : ''
    const t = Date.parse(`${y}-${mo}-${d}T${h}:${mi}:${s}${zone}`)
    return Number.isFinite(t) ? t : undefined
  }
  m = SYSLOG_RE.exec(text)
  if (m) {
    const month = MONTHS.indexOf(m[1])
    if (month < 0) return undefined
    const dt = new Date(now.getFullYear(), month, Number(m[2]), Number(m[3]), Number(m[4]), Number(m[5]))
    if (dt.getTime() > now.getTime() + 86_400_000) dt.setFullYear(dt.getFullYear() - 1)
    return dt.getTime()
  }
  return undefined
}

type Status = 'idle' | 'connecting' | 'live' | 'ended' | 'error'

export function LogsPanel({ target, platform, journal: hasJournal, unit: presetUnit }: { target: TargetId; platform?: string; journal?: boolean; unit?: string }) {
  const settings = monitorSettings.use()
  const [mode, setMode] = useState<'files' | 'journal'>(presetUnit ? 'journal' : 'files')
  const [paths, setPaths] = useState<string[]>([])
  const [unit, setUnit] = useState(presetUnit ?? '')
  const [history, setHistory] = useState(settings.logLines)
  const [sudo, setSudo] = useState(false)
  const [status, setStatus] = useState<Status>('idle')
  const [message, setMessage] = useState('')
  const [sources, setSources] = useState<string[]>([])
  const [version, setVersion] = useState(0)
  const [paused, setPaused] = useState(false)
  const [pending, setPending] = useState(0)
  const [filter, setFilter] = useState('')
  const [lvlFilter, setLvlFilter] = useState<'all' | 'warn' | 'error'>('all')
  const [wrap, setWrap] = useState(false)
  const [follow, setFollow] = useState(true)
  const [jump, setJump] = useState('')
  const [highlight, setHighlight] = useState<number | null>(null)
  const lines = useRef<Line[]>([])
  const held = useRef<Line[]>([])
  const nextId = useRef(1)
  const ws = useRef<WebSocket | null>(null)
  const pausedRef = useRef(false)
  pausedRef.current = paused
  const scroller = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (presetUnit) {
      setMode('journal')
      setUnit(presetUnit)
    }
  }, [presetUnit])

  const push = useCallback((batch: [number, string][]) => {
    const add = batch.map(([src, text]) => ({ id: nextId.current++, src, text, lvl: classify(text) }))
    if (pausedRef.current) {
      held.current.push(...add)
      if (held.current.length > MAX_LINES) held.current.splice(0, held.current.length - MAX_LINES)
      setPending(held.current.length)
      return
    }
    lines.current.push(...add)
    if (lines.current.length > MAX_LINES) lines.current.splice(0, lines.current.length - MAX_LINES)
    setVersion((v) => v + 1)
  }, [])

  const stop = useCallback(() => {
    const s = ws.current
    ws.current = null
    if (s) {
      try {
        if (s.readyState === WebSocket.OPEN) s.send(JSON.stringify({ type: 'stop' }))
        s.close(1000)
      } catch {
        /* closed */
      }
    }
    setStatus((st) => (st === 'live' || st === 'connecting' ? 'ended' : st))
  }, [])

  const start = useCallback(() => {
    stop()
    if (mode === 'files' && paths.length === 0) {
      toast.error('Choose at least one log file')
      return
    }
    lines.current = []
    held.current = []
    setPending(0)
    setVersion((v) => v + 1)
    setStatus('connecting')
    setMessage('')
    setFollow(true)
    const url = tailUrl(target, mode === 'journal' ? { journal: true, unit: unit.trim() || undefined, lines: history, sudo } : { paths, lines: history, sudo })
    const sock = new WebSocket(url)
    ws.current = sock
    sock.onmessage = (e) => {
      if (ws.current !== sock || typeof e.data !== 'string') return
      let f: TailFrame
      try {
        f = JSON.parse(e.data) as TailFrame
      } catch {
        return
      }
      switch (f.type) {
        case 'start':
          setSources(f.sources)
          setStatus('live')
          break
        case 'lines':
          push(f.lines)
          break
        case 'error':
          setStatus('error')
          setMessage(f.message)
          break
        case 'end':
          setStatus('ended')
          setMessage(f.message ? f.message : f.code === 0 ? 'The follower exited.' : `The follower exited with status ${f.code}.`)
          break
      }
    }
    sock.onclose = (e) => {
      if (ws.current !== sock) return
      ws.current = null
      setStatus((st) => (st === 'connecting' ? 'error' : st === 'live' ? 'ended' : st))
      if (e.code !== 1000 && e.code !== 1005) setMessage((m) => m || `Connection closed (${e.code}${e.reason ? `: ${e.reason}` : ''}).`)
    }
  }, [mode, paths, unit, history, sudo, target, push, stop])

  useEffect(() => () => stop(), [stop])

  const resume = () => {
    lines.current.push(...held.current)
    if (lines.current.length > MAX_LINES) lines.current.splice(0, lines.current.length - MAX_LINES)
    held.current = []
    setPending(0)
    setPaused(false)
    setVersion((v) => v + 1)
  }

  const matcher = useMemo(() => {
    const f = filter.trim()
    if (!f) return null
    if (f.length > 2 && f.startsWith('/') && f.endsWith('/')) {
      try {
        const re = new RegExp(f.slice(1, -1), 'i')
        return (s: string) => re.test(s)
      } catch {
        return null
      }
    }
    const lower = f.toLowerCase()
    return (s: string) => s.toLowerCase().includes(lower)
  }, [filter])

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const visible = useMemo(() => {
    return lines.current.filter((l) => (lvlFilter === 'all' || (lvlFilter === 'error' ? l.lvl === 'error' : l.lvl === 'error' || l.lvl === 'warn')) && (!matcher || matcher(l.text)))
    // version bumps whenever lines.current changes
  }, [version, matcher, lvlFilter])

  const virt = useVirtualizer({
    count: visible.length,
    getScrollElement: () => scroller.current,
    estimateSize: () => 18,
    overscan: 20,
  })
  useEffect(() => {
    if (follow && visible.length) virt.scrollToIndex(visible.length - 1, { align: 'end' })
  }, [visible.length, follow, virt])
  useEffect(() => virt.measure(), [wrap, virt])

  const onScroll = () => {
    const el = scroller.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
    if (atBottom !== follow) setFollow(atBottom)
  }

  const doJump = () => {
    const target = Date.parse(jump)
    if (!Number.isFinite(target)) return
    const now = new Date()
    const i = visible.findIndex((l) => {
      const t = lineTime(l.text, now)
      return t != null && t >= target
    })
    if (i < 0) {
      toast.info('No line at or after that time', { description: 'Load more history (lines) or pick an earlier time.' })
      return
    }
    setFollow(false)
    setHighlight(visible[i].id)
    virt.scrollToIndex(i, { align: 'center' })
  }

  const download = () => {
    const multi = sources.length > 1
    const text = lines.current.map((l) => (multi ? `[${sources[l.src] ?? l.src}] ${l.text}` : l.text)).join('\n')
    const blob = new Blob([text + '\n'], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `nexterm-logs-${new Date().toISOString().replace(/[:.]/g, '-')}.log`
    a.click()
    setTimeout(() => URL.revokeObjectURL(a.href), 5_000)
  }

  if (platform === 'windows') {
    return <EmptyState icon={ScrollText} title="Not available on Windows hosts" description="Following logs relies on tail / journalctl." />
  }

  const running = status === 'live' || status === 'connecting'
  const multi = sources.length > 1
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b bg-toolbar px-2 py-1.5">
        {hasJournal !== false && (
          <SegmentedControl
            size="sm"
            value={mode}
            onValueChange={(v) => setMode(v)}
            aria-label="Source"
            options={[
              { value: 'files', label: 'Files', icon: FileText },
              { value: 'journal', label: 'Journal', icon: ScrollText },
            ]}
          />
        )}
        {mode === 'files' ? (
          <div className="min-w-60 flex-1">
            <TagInput
              value={paths}
              onChange={(v) => setPaths(v.slice(0, 8))}
              suggestions={settings.logFiles}
              placeholder={paths.length ? 'Add another file…' : 'Log file path, e.g. /var/log/syslog'}
              normalize={(t) => {
                const v = t.trim()
                return v.startsWith('/') ? v : ''
              }}
              maxTags={8}
              aria-label="Log files"
            />
          </div>
        ) : (
          <Input inputSize="sm" className="w-64" placeholder="Unit (optional), e.g. nginx.service" value={unit} onChange={(e) => setUnit(e.target.value)} aria-label="Journal unit" />
        )}
        <label className="flex items-center gap-1 text-xs text-muted-foreground">
          History
          <NumberInput inputSize="sm" className="w-20" value={history} min={0} max={5000} step={100} integer onChange={(v) => setHistory(v ?? 200)} aria-label="Lines of history" />
        </label>
        <IconButton icon={ShieldCheck} label={sudo ? 'Reading with sudo' : 'Read with sudo (protected logs)'} size="sm" active={sudo} onClick={() => setSudo((v) => !v)} disabled={running} />
        {running ? (
          <Button size="sm" variant="outline" onClick={stop}>
            <CircleStop /> Stop
          </Button>
        ) : (
          <Button size="sm" onClick={start}>
            <Play /> {status === 'idle' ? 'Follow' : 'Restart'}
          </Button>
        )}
        <Spinner active={status === 'connecting'} className="size-3.5" />
        {status === 'live' && (
          <Badge variant="success" className="gap-1">
            <StatusDot tone="success" /> live
          </Badge>
        )}
        {mode === 'files' && paths.length === 0 && settings.logFiles.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {settings.logFiles.slice(0, 4).map((f) => (
              <Button key={f} size="xs" variant="ghost" className="font-mono" onClick={() => setPaths([f])}>
                {f}
              </Button>
            ))}
          </div>
        )}
      </div>
      <Toolbar aria-label="Log view tools" className="h-8 gap-1">
        <Input inputSize="sm" className="w-52" placeholder="Filter (text or /regex/)" value={filter} onChange={(e) => setFilter(e.target.value)} leading={<Filter />} aria-label="Filter lines" />
        <SegmentedControl
          size="sm"
          value={lvlFilter}
          onValueChange={setLvlFilter}
          aria-label="Level"
          options={[
            { value: 'all', label: 'All' },
            { value: 'warn', label: 'Warnings+' },
            { value: 'error', label: 'Errors' },
          ]}
        />
        <ToolbarSeparator />
        <IconButton icon={paused ? Play : Pause} label={paused ? 'Resume' : 'Pause (keep receiving)'} size="sm" active={paused} onClick={() => (paused ? resume() : setPaused(true))} disabled={status === 'idle'} />
        <IconButton icon={WrapText} label="Wrap long lines" size="sm" active={wrap} onClick={() => setWrap((w) => !w)} />
        <IconButton
          icon={ArrowDownToLine}
          label="Scroll to the end"
          size="sm"
          active={follow}
          onClick={() => {
            setFollow(true)
            setHighlight(null)
          }}
        />
        <IconButton
          icon={Eraser}
          label="Clear"
          size="sm"
          onClick={() => {
            lines.current = []
            held.current = []
            setPending(0)
            setVersion((v) => v + 1)
          }}
        />
        <IconButton icon={Download} label="Download" size="sm" onClick={download} disabled={!lines.current.length} />
        <ToolbarSeparator />
        <form
          className="flex items-center gap-1"
          onSubmit={(e) => {
            e.preventDefault()
            doJump()
          }}
        >
          <Clock className="size-3.5 text-muted-foreground" />
          <Input inputSize="sm" type="datetime-local" step={1} className="w-48" value={jump} onChange={(e) => setJump(e.target.value)} aria-label="Jump to time" />
          <Button size="xs" variant="ghost" type="submit" disabled={!jump}>
            Jump
          </Button>
        </form>
        <ToolbarSpacer />
        <span className="text-xs text-muted-foreground tabular">
          {visible.length === lines.current.length ? `${lines.current.length} lines` : `${visible.length} of ${lines.current.length} lines`}
        </span>
      </Toolbar>
      {multi && (
        <div className="flex flex-wrap gap-x-3 gap-y-0.5 border-b px-3 py-1 text-xs">
          {sources.map((s, i) => (
            <span key={s} className="flex items-center gap-1 font-mono">
              <span className="size-2 rounded-full" style={{ background: SOURCE_COLORS[i % SOURCE_COLORS.length] }} />
              {s}
            </span>
          ))}
        </div>
      )}
      {(status === 'error' || status === 'ended') && message && (
        <div className={cn('border-b px-3 py-1 text-xs', status === 'error' ? 'bg-destructive/10 text-destructive' : 'bg-muted/40 text-muted-foreground')}>{message}</div>
      )}
      {paused && pending > 0 && (
        <button type="button" onClick={resume} className="border-b bg-primary/10 px-3 py-1 text-left text-xs text-primary hover:bg-primary/15">
          {pending} new line{pending === 1 ? '' : 's'} while paused — click to resume
        </button>
      )}
      <div ref={scroller} onScroll={onScroll} className="relative min-h-0 flex-1 overflow-auto bg-background font-mono text-xs" role="log" aria-live="off" aria-label="Log lines">
        {status === 'idle' && lines.current.length === 0 ? (
          <EmptyState
            icon={ScrollText}
            title="Follow logs live"
            description={mode === 'files' ? 'Pick one or more log files, then Follow. Several files are merged with a colour per file.' : 'Follow the systemd journal, optionally of one unit.'}
          />
        ) : (
          <div style={{ height: virt.getTotalSize(), position: 'relative' }}>
            {virt.getVirtualItems().map((vi) => {
              const l = visible[vi.index]
              return (
                <div
                  key={l.id}
                  data-index={vi.index}
                  ref={wrap ? virt.measureElement : undefined}
                  className={cn(
                    'absolute inset-x-0 flex gap-2 px-2 leading-[18px]',
                    wrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre',
                    l.lvl === 'error' && 'text-destructive',
                    l.lvl === 'warn' && 'text-warning',
                    l.lvl === 'debug' && 'text-muted-foreground',
                    highlight === l.id && 'bg-primary/20',
                  )}
                  style={{ transform: `translateY(${vi.start}px)`, minHeight: 18 }}
                >
                  {multi && <span className="mt-[6px] size-1.5 shrink-0 rounded-full" style={{ background: SOURCE_COLORS[l.src % SOURCE_COLORS.length] }} title={sources[l.src]} />}
                  <span className="min-w-0">{l.text || ' '}</span>
                </div>
              )
            })}
          </div>
        )}
      </div>
    </div>
  )
}
