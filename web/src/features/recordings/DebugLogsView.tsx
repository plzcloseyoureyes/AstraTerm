/*
 * Tab kind "debugLogs" (admin, command admin.debugLogs): the application's recent log records (REC-9) from the
 * server's in-memory ring — level filter, module filter, text search, live follow, details per record, export
 * (secrets redacted server-side) and clear.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDownToLine, Bug, ChevronRight, Download, Pause, Play, Search, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import type { TabProps } from '@/app/registry'
import { runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { cn, errorMessage } from '@/lib/utils'
import { useIsTabVisible } from '@/stores/workspace'
import { clearLogs, downloadUrl, getLogs, logsExportUrl } from './api'
import type { LogEntry, LogsResponse } from './types'

const MAX = 5000
type Level = 'debug' | 'info' | 'warn' | 'error'
const RANK: Record<Level, number> = { debug: 0, info: 1, warn: 2, error: 3 }
const LEVEL_CLASS: Record<Level, string> = {
  debug: 'text-muted-foreground',
  info: 'text-info',
  warn: 'text-warning',
  error: 'text-destructive',
}

export default function DebugLogsView({ tabId }: TabProps) {
  const visible = useIsTabVisible(tabId)
  const [entries, setEntries] = useState<LogEntry[]>([])
  const [meta, setMeta] = useState<Omit<LogsResponse, 'entries'> | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [level, setLevel] = useState<Level>('debug')
  const [module, setModule] = useState('')
  const [q, setQ] = useState('')
  const [paused, setPaused] = useState(false)
  const [follow, setFollow] = useState(true)
  const [open, setOpen] = useState<Set<number>>(new Set())
  const lastId = useRef(0)
  const scrollRef = useRef<HTMLDivElement>(null)

  // Poll new records while visible and not paused (only the delta after the last id).
  useEffect(() => {
    if (!visible || paused) return
    let stop = false
    const tick = async () => {
      try {
        const r = await getLogs({ after: lastId.current, limit: MAX })
        if (stop) return
        const { entries: fresh, ...m } = r
        setMeta(m)
        setError(null)
        if (r.lastId < lastId.current) {
          // Cleared on the server (or the server restarted).
          lastId.current = 0
          setEntries([])
          return
        }
        if (fresh.length) {
          lastId.current = fresh[fresh.length - 1].id
          setEntries((old) => {
            const next = old.concat(fresh)
            return next.length > MAX ? next.slice(next.length - MAX) : next
          })
        }
      } catch (err) {
        if (!stop) setError(errorMessage(err))
      }
    }
    void tick()
    const t = setInterval(() => void tick(), 2000)
    return () => {
      stop = true
      clearInterval(t)
    }
  }, [visible, paused])

  const modules = useMemo(() => {
    const s = new Set<string>()
    for (const e of entries) if (e.module) s.add(e.module)
    return [...s].sort()
  }, [entries])

  const shown = useMemo(() => {
    const s = q.trim().toLowerCase()
    return entries.filter((e) => {
      if (RANK[e.level] < RANK[level]) return false
      if (module && e.module !== module) return false
      if (!s) return true
      return (
        e.msg.toLowerCase().includes(s) ||
        (e.module ?? '').toLowerCase().includes(s) ||
        (e.attrs ?? []).some((a) => `${a.k}=${a.v}`.toLowerCase().includes(s))
      )
    })
  }, [entries, level, module, q])

  const virt = useVirtualizer({
    count: shown.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 22,
    overscan: 20,
    measureElement: (el) => el.getBoundingClientRect().height,
  })

  useEffect(() => {
    if (follow && shown.length) virt.scrollToIndex(shown.length - 1, { align: 'end' })
  }, [shown.length, follow, virt])

  const clear = async () => {
    if (!(await confirm({ title: 'Clear the debug log?', description: 'Every captured record is removed from the server’s memory.', confirmLabel: 'Clear', destructive: true }))) return
    try {
      await clearLogs()
      setEntries([])
      toast.success('Debug log cleared')
    } catch (err) {
      toast.error('Could not clear the log', { description: errorMessage(err) })
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col bg-background">
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b bg-toolbar px-2 py-1.5">
        <Bug className="mx-1 size-4 text-muted-foreground" />
        <SegmentedControl<Level>
          size="sm"
          aria-label="Minimum level"
          value={level}
          onValueChange={setLevel}
          options={[
            { value: 'debug', label: 'All' },
            { value: 'info', label: 'Info+' },
            { value: 'warn', label: 'Warnings+' },
            { value: 'error', label: 'Errors' },
          ]}
        />
        <SimpleSelect
          size="sm"
          value={module || '__all'}
          onValueChange={(v) => setModule(v === '__all' ? '' : v)}
          aria-label="Module"
          className="w-36"
          options={[{ value: '__all', label: 'All modules' }, ...modules.map((m) => ({ value: m, label: m }))]}
        />
        <Input inputSize="sm" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search messages and fields…" leading={<Search />} className="w-64" />
        <div className="flex-1" />
        <span className="text-xs text-muted-foreground tabular-nums">
          {shown.length.toLocaleString()} shown{meta ? ` · ${meta.stored.toLocaleString()} / ${meta.capacity.toLocaleString()} kept` : ''}
        </span>
        <IconButton icon={paused ? Play : Pause} label={paused ? 'Resume' : 'Pause'} active={paused} onClick={() => setPaused((v) => !v)} />
        <IconButton icon={ArrowDownToLine} label="Follow newest" active={follow} onClick={() => setFollow((v) => !v)} />
        <IconButton icon={Download} label="Export (redacted)" onClick={() => downloadUrl(logsExportUrl({ level: level === 'debug' ? undefined : level, q: q.trim() || undefined }))} />
        <IconButton icon={Trash2} label="Clear" onClick={() => void clear()} />
      </div>
      {meta && (!meta.enabled || meta.source === 'partial') && (
        <div className="flex shrink-0 items-center gap-2 border-b bg-muted/40 px-3 py-1 text-sm text-muted-foreground">
          {!meta.enabled
            ? 'Log capture is switched off.'
            : 'Only part of AstraTerm’s log is captured (components that start after the recording module); the process log has everything.'}
          {!meta.enabled && (
            <Button size="xs" variant="ghost" onClick={() => void runCommand('recordings.open', { tab: 'storage' })}>
              Settings
            </Button>
          )}
          {meta.enabled && <span className="ml-auto tabular-nums">level ≥ {meta.level}</span>}
        </div>
      )}
      <div
        ref={scrollRef}
        className="relative min-h-0 flex-1 overflow-auto font-mono text-[12px] leading-[22px]"
        onWheel={(e) => {
          if (e.deltaY < 0 && follow) setFollow(false)
        }}
      >
        <LoadingPane active={!meta && !error} label="Loading the debug log" className="pointer-events-none absolute inset-0" />
        {error && !entries.length ? (
          <EmptyState icon={Bug} title="Cannot load the debug log" description={error} />
        ) : !shown.length && meta ? (
          <EmptyState icon={Bug} size="sm" title={entries.length ? 'No matching records' : 'No records yet'} />
        ) : null}
        <div style={{ height: virt.getTotalSize(), position: 'relative' }}>
          {virt.getVirtualItems().map((v) => {
            const e = shown[v.index]
            const expanded = open.has(e.id)
            return (
              <div key={e.id} data-index={v.index} ref={virt.measureElement} className="absolute left-0 w-full" style={{ transform: `translateY(${v.start}px)` }}>
                <button
                  type="button"
                  className={cn('flex w-full items-baseline gap-2 px-2 text-left hover:bg-accent/40', expanded && 'bg-accent/30')}
                  onClick={() =>
                    setOpen((old) => {
                      const n = new Set(old)
                      if (n.has(e.id)) n.delete(e.id)
                      else n.add(e.id)
                      return n
                    })
                  }
                >
                  <ChevronRight className={cn('size-3 shrink-0 self-center text-muted-foreground transition-transform duration-150', expanded && 'rotate-90', !e.attrs?.length && 'invisible')} />
                  <span className="shrink-0 text-muted-foreground tabular-nums">{new Date(e.ts).toLocaleTimeString(undefined, { hour12: false })}</span>
                  <span className={cn('w-10 shrink-0 font-semibold uppercase', LEVEL_CLASS[e.level])}>{e.level}</span>
                  {e.module && <span className="shrink-0 text-muted-foreground">[{e.module}]</span>}
                  <span className="min-w-0 truncate">{e.msg}</span>
                  {!expanded && !!e.attrs?.length && (
                    <span className="min-w-0 flex-1 truncate text-muted-foreground">{e.attrs.map((a) => `${a.k}=${a.v}`).join(' ')}</span>
                  )}
                </button>
                {expanded && !!e.attrs?.length && (
                  <dl className="grid grid-cols-[max-content_1fr] gap-x-3 border-b px-9 pb-1.5 text-[12px]">
                    {e.attrs.map((a, i) => (
                      <div key={i} className="contents">
                        <dt className="text-muted-foreground">{a.k}</dt>
                        <dd className="break-all whitespace-pre-wrap">{a.v}</dd>
                      </div>
                    ))}
                  </dl>
                )}
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}
