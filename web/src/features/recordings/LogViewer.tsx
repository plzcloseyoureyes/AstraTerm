/*
 * Text log viewer (REC-1): virtualized lines with line numbers, the logged timestamps in their own column, instant
 * find (substring / regex, case) with next / previous and highlighting, wrap toggle, follow mode for logs still being
 * written (appends only the new bytes with Range requests), copy and download. Logs larger than 32 MiB open on their
 * last 32 MiB.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDown, ArrowUp, CaseSensitive, Clock, Download, FileText, Regex, RefreshCw, Search, WrapText } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingPane } from '@/components/ui/spinner'
import { cn, errorMessage, formatBytes, formatDateTime } from '@/lib/utils'
import { downloadUrl, recordingFileUrl } from './api'
import type { RecordingItem } from './types'

const MAX_BYTES = 32 << 20
const TS = /^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3})\] /

export default function LogViewer({ item, line }: { item: RecordingItem; line?: number }) {
  // The item is refetched every few seconds while the log is written: only a different file (url) reloads it,
  // new bytes come in through follow mode.
  const [initial] = useState(() => ({ size: item.size, live: item.live }))
  const [lines, setLines] = useState<string[]>([])
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [partial, setPartial] = useState(false)
  const [loading, setLoading] = useState(false)
  const [follow, setFollow] = useState(item.live)
  const [wrap, setWrap] = useState(false)
  const [showTs, setShowTs] = useState(true)
  const [q, setQ] = useState('')
  const [regex, setRegex] = useState(false)
  const [caseSensitive, setCaseSensitive] = useState(false)
  const [cursor, setCursor] = useState(0)
  const bytesRef = useRef(0)
  const tailRef = useRef('')
  const decoder = useRef(new TextDecoder())
  const scrollRef = useRef<HTMLDivElement>(null)
  const url = recordingFileUrl(item.id)

  const append = useCallback((chunk: string, final: boolean) => {
    const text = tailRef.current + chunk
    const parts = text.split('\n')
    tailRef.current = final ? '' : (parts.pop() ?? '')
    if (final && parts.length && parts[parts.length - 1] === '') parts.pop()
    if (parts.length) setLines((old) => old.concat(parts))
  }, [])

  const fetchRange = useCallback(
    async (from: number | null) => {
      const headers: Record<string, string> = {}
      if (from === null) headers.Range = `bytes=-${MAX_BYTES}`
      else if (from > 0) headers.Range = `bytes=${from}-`
      const res = await fetch(url, { credentials: 'same-origin', headers, cache: 'no-store' })
      if (res.status === 416) return { data: new Uint8Array(), start: from ?? 0, total: from ?? 0 }
      if (!res.ok) throw new Error(res.status === 410 ? 'The log file no longer exists.' : `HTTP ${res.status}`)
      const data = new Uint8Array(await res.arrayBuffer())
      let start = 0
      let total = data.length
      const cr = res.headers.get('Content-Range')
      const m = cr && /bytes (\d+)-(\d+)\/(\d+|\*)/.exec(cr)
      if (m) {
        start = +m[1]
        total = m[3] === '*' ? start + data.length : +m[3]
      } else if (from && from > 0) {
        // The server ignored the range: keep only the new part.
        return { data: data.subarray(from), start: from, total: data.length }
      }
      return { data, start, total }
    },
    [url],
  )

  const loadAll = useCallback(async () => {
    setLoading(true)
    try {
      setLines([])
      tailRef.current = ''
      decoder.current = new TextDecoder()
      const big = initial.size > MAX_BYTES
      const r = await fetchRange(big ? null : 0)
      let data = r.data
      if (r.start > 0) {
        // Opened in the middle of the file: drop the partial first line.
        const nl = data.indexOf(10)
        data = nl >= 0 ? data.subarray(nl + 1) : new Uint8Array()
      }
      setPartial(r.start > 0)
      bytesRef.current = r.start + r.data.length
      append(decoder.current.decode(data, { stream: true }), !initial.live)
      setLoaded(true)
      setError(null)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [append, fetchRange, initial])

  useEffect(() => {
    void loadAll()
  }, [loadAll])

  // The log was closed while open here: its last line (no newline yet) is complete now.
  useEffect(() => {
    if (!item.live && initial.live && tailRef.current) append('', true)
  }, [item.live, initial.live, append])

  // Follow: append new bytes of a log still being written.
  useEffect(() => {
    if (!follow || !loaded) return
    let stop = false
    const tick = async () => {
      try {
        const r = await fetchRange(bytesRef.current)
        if (stop || !r.data.length) return
        bytesRef.current += r.data.length
        append(decoder.current.decode(r.data, { stream: true }), false)
      } catch {
        /* transient: next tick */
      }
    }
    const t = setInterval(() => void tick(), 2000)
    return () => {
      stop = true
      clearInterval(t)
    }
  }, [follow, loaded, fetchRange, append])

  const matcher = useMemo(() => {
    const s = q.trim() ? q : ''
    if (!s) return null
    try {
      if (regex) return new RegExp(s, caseSensitive ? 'g' : 'gi')
    } catch {
      return 'invalid' as const
    }
    const esc = s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
    return new RegExp(esc, caseSensitive ? 'g' : 'gi')
  }, [q, regex, caseSensitive])

  const matches = useMemo(() => {
    if (!matcher || matcher === 'invalid') return []
    const out: number[] = []
    for (let i = 0; i < lines.length; i++) {
      matcher.lastIndex = 0
      if (matcher.test(lines[i])) out.push(i)
    }
    return out
  }, [lines, matcher])

  const virt = useVirtualizer({
    count: lines.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 20,
    overscan: 20,
    measureElement: wrap ? (el) => el.getBoundingClientRect().height : undefined,
  })

  // Keep the newest lines in view while following (unless the user scrolled up).
  const atBottom = useRef(true)
  useEffect(() => {
    if (follow && atBottom.current && lines.length) virt.scrollToIndex(lines.length - 1, { align: 'end' })
  }, [lines.length, follow, virt])

  useEffect(() => {
    if (line && loaded && line - 1 < lines.length) virt.scrollToIndex(line - 1, { align: 'center' })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [line, loaded])

  const go = (dir: 1 | -1) => {
    if (!matches.length) return
    const next = (cursor + dir + matches.length) % matches.length
    setCursor(next)
    virt.scrollToIndex(matches[next], { align: 'center' })
  }
  useEffect(() => {
    setCursor(0)
    if (matches.length) virt.scrollToIndex(matches[0], { align: 'center' })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [matcher])

  const activeLine = matches.length ? matches[Math.min(cursor, matches.length - 1)] : -1
  const gutter = String(lines.length).length

  if (error && !loaded) {
    return (
      <EmptyState
        icon={FileText}
        title="Cannot open the log"
        description={error}
        action={
          <Button size="sm" variant="secondary" onClick={() => void loadAll()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  }

  return (
    <div className="flex h-full min-h-0 flex-col bg-background">
      <div className="flex h-9 shrink-0 items-center gap-1 border-b bg-toolbar px-1.5">
        <FileText className="mx-1 size-4 text-muted-foreground" />
        <div className="min-w-0 flex-1 truncate text-sm font-medium">
          {item.title || 'Session log'}
          <span className="ml-2 font-normal text-muted-foreground tabular-nums">
            {formatDateTime(item.startedAt)} · {formatBytes(item.size)} · {lines.length.toLocaleString()} lines
          </span>
        </div>
        {item.live && (
          <Badge variant="destructive">
            <span className="size-1.5 rounded-full bg-current" /> Logging
          </Badge>
        )}
        <div className="flex w-72 items-center gap-0.5">
          <Input
            inputSize="sm"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Find…"
            leading={<Search />}
            aria-invalid={matcher === 'invalid' || undefined}
            onKeyDown={(e) => {
              if (e.key === 'Enter') go(e.shiftKey ? -1 : 1)
              if (e.key === 'Escape') setQ('')
            }}
            trailing={
              q ? (
                <span className="pr-1.5 text-xs tabular-nums">{matcher === 'invalid' ? 'bad regex' : matches.length ? `${Math.min(cursor + 1, matches.length)}/${matches.length}` : '0'}</span>
              ) : undefined
            }
          />
        </div>
        <IconButton icon={ArrowUp} label="Previous match" shortcut="Shift+Enter" onClick={() => go(-1)} disabled={!matches.length} />
        <IconButton icon={ArrowDown} label="Next match" shortcut="Enter" onClick={() => go(1)} disabled={!matches.length} />
        <IconButton icon={Regex} label="Regular expression" active={regex} onClick={() => setRegex((v) => !v)} />
        <IconButton icon={CaseSensitive} label="Match case" active={caseSensitive} onClick={() => setCaseSensitive((v) => !v)} />
        <span className="mx-1 h-4 w-px bg-border" />
        <IconButton icon={Clock} label="Show timestamps" active={showTs} onClick={() => setShowTs((v) => !v)} />
        <IconButton icon={WrapText} label="Wrap lines" active={wrap} onClick={() => setWrap((v) => !v)} />
        {item.live && <IconButton icon={ArrowDown} label={follow ? 'Stop following' : 'Follow new output'} active={follow} onClick={() => setFollow((v) => !v)} />}
        <IconButton icon={Download} label="Download log" onClick={() => downloadUrl(recordingFileUrl(item.id, undefined, true))} />
      </div>
      {partial && (
        <div className="shrink-0 border-b bg-warning/10 px-3 py-1 text-sm">Showing the last 32 MiB of this log. Download it for the complete file.</div>
      )}
      <div
        ref={scrollRef}
        className="relative min-h-0 flex-1 overflow-auto font-mono text-[12px] leading-5"
        onScroll={(e) => {
          const el = e.currentTarget
          atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
      >
        <LoadingPane active={loading && !loaded} label="Loading the log" className="pointer-events-none absolute inset-0" />
        {loaded && !lines.length && <EmptyState icon={FileText} size="sm" title="The log is empty" />}
        <div style={{ height: virt.getTotalSize(), position: 'relative', minWidth: wrap ? undefined : 'max-content' }}>
          {virt.getVirtualItems().map((v) => {
            const text = lines[v.index]
            const m = showTs ? null : TS.exec(text)
            const ts = showTs ? TS.exec(text) : null
            const body = ts ? text.slice(ts[0].length) : m ? text.slice(m[0].length) : text
            return (
              <div
                key={v.key}
                data-index={v.index}
                ref={wrap ? virt.measureElement : undefined}
                className={cn('absolute left-0 flex w-full px-2', v.index === activeLine ? 'bg-primary/15' : v.index + 1 === line && 'bg-accent')}
                style={{ transform: `translateY(${v.start}px)`, height: wrap ? undefined : 20 }}
              >
                <span className="mr-3 shrink-0 text-right text-muted-foreground/70 select-none tabular-nums" style={{ width: `${gutter}ch` }}>
                  {v.index + 1}
                </span>
                {ts && <span className="mr-2 shrink-0 text-muted-foreground tabular-nums">{ts[1].slice(11)}</span>}
                <span className={wrap ? 'break-all whitespace-pre-wrap' : 'whitespace-pre'}>
                  <Highlighted text={body} re={matcher && matcher !== 'invalid' ? matcher : null} />
                </span>
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

function Highlighted({ text, re }: { text: string; re: RegExp | null }) {
  if (!re || !text) return <>{text || ' '}</>
  const parts: React.ReactNode[] = []
  let last = 0
  re.lastIndex = 0
  let m: RegExpExecArray | null
  let guard = 0
  while ((m = re.exec(text)) && guard++ < 200) {
    if (m[0] === '') {
      re.lastIndex++
      continue
    }
    if (m.index > last) parts.push(text.slice(last, m.index))
    parts.push(
      <mark key={m.index} className="rounded-[2px] bg-warning/40 text-foreground">
        {m[0]}
      </mark>,
    )
    last = m.index + m[0].length
  }
  if (last < text.length) parts.push(text.slice(last))
  return <>{parts}</>
}
