/*
 * Shared pickers: running sessions (dialog), saved connections (multi-select list), keyboard shortcut capture, run
 * status badge, colour select.
 */
import * as React from 'react'
import { CircleCheck, CircleDashed, CircleX, Keyboard, Search, SquareTerminal, X } from 'lucide-react'
import { useConnections } from '@/api/connections'
import { useFolders } from '@/api/folders'
import { isSessionRunning, useSessions } from '@/api/sessions'
import type { Connection, RuntimeSession } from '@/api/types'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { suspendKeybindings } from '@/app/keybindings'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { Kbd } from '@/components/ui/kbd'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { eventToKeybinding } from '@/lib/keys'
import { cn, plural } from '@/lib/utils'
import { finishSessionPick, useAutomationUI } from '../store'
import { ANSI_COLORS, cssColor } from '../highlight/rules'

export { cssColor }

// ---------------------------------------------------------------------------------------------------------------------
// Session picker dialog (pickSessions())
// ---------------------------------------------------------------------------------------------------------------------

function sessionTarget(s: RuntimeSession): string {
  return `${s.username ? `${s.username}@` : ''}${s.host ?? ''}`
}

export function SessionPickDialog() {
  const req = useAutomationUI((s) => s.sessionPick)
  const sessionsQuery = useSessions(!!req)
  const sessions = sessionsQuery.data
  const [selected, setSelected] = React.useState<Set<string>>(new Set())
  const [q, setQ] = React.useState('')
  React.useEffect(() => {
    if (req) {
      setSelected(new Set(req.selected))
      setQ('')
    }
  }, [req])
  if (!req) return null
  const live = (sessions ?? []).filter((s) => s.kind === 'terminal' && isSessionRunning(s))
  const needle = q.trim().toLowerCase()
  const shown = needle ? live.filter((s) => `${s.title} ${sessionTarget(s)} ${s.protocol}`.toLowerCase().includes(needle)) : live
  const toggle = (id: string, on: boolean) =>
    setSelected((prev) => {
      const next = new Set(prev)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })
  const allShown = shown.length > 0 && shown.every((s) => selected.has(s.id))
  return (
    <Dialog open onOpenChange={(o) => !o && finishSessionPick(null)}>
      <DialogContent size="md">
        <DialogHeader>
          <DialogTitle>{req.title}</DialogTitle>
          <DialogDescription>{req.description ?? 'Choose the sessions to send to.'}</DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2">
          <Input inputSize="sm" leading={<Search />} placeholder="Filter sessions…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter sessions" />
          <Button
            size="sm"
            variant="secondary"
            disabled={!shown.length}
            onClick={() => setSelected((prev) => {
              const next = new Set(prev)
              for (const s of shown) {
                if (allShown) next.delete(s.id)
                else next.add(s.id)
              }
              return next
            })}
          >
            {allShown ? 'None' : 'All'}
          </Button>
        </div>
        <DialogBody className="max-h-[50vh]">
          <QueryState query={sessionsQuery} skeleton={<SkeletonRows rows={4} rowHeight={32} />} errorTitle="Could not load the sessions">
            {() =>
              !shown.length ? (
                <EmptyState size="sm" icon={SquareTerminal} title="No running terminal sessions" description="Open a session first." />
              ) : (
                <ul className="flex flex-col gap-0.5" aria-label="Sessions">
                  {shown.map((s) => {
                    const Icon = protocolIcon(s.protocol)
                    const id = `pick-${s.id}`
                    return (
                      <li key={s.id}>
                        <label htmlFor={id} className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 hover:bg-accent">
                          <Checkbox id={id} checked={selected.has(s.id)} onCheckedChange={(v) => toggle(s.id, v === true)} />
                          <Icon className="size-3.5 shrink-0 text-muted-foreground" />
                          <span className="min-w-0 flex-1 truncate">{s.title}</span>
                          <span className="truncate text-xs text-muted-foreground">{sessionTarget(s)}</span>
                          {s.state !== 'connected' && (
                            <Badge variant="warning" className="shrink-0">
                              {s.state}
                            </Badge>
                          )}
                        </label>
                      </li>
                    )
                  })}
                </ul>
              )
            }
          </QueryState>
        </DialogBody>
        <DialogFooter>
          <span className="mr-auto text-sm text-muted-foreground">{plural(selected.size, 'session')} selected</span>
          <Button variant="secondary" onClick={() => finishSessionPick(null)}>
            Cancel
          </Button>
          <Button disabled={!selected.size} onClick={() => finishSessionPick(Array.from(selected))}>
            {req.confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Saved connections multi-select
// ---------------------------------------------------------------------------------------------------------------------

export function ConnectionMultiSelect({
  value,
  onChange,
  protocols,
  className,
  height = 'h-64',
}: {
  value: string[]
  onChange: (ids: string[]) => void
  /** Only list these protocols (default: every terminal protocol). */
  protocols?: string[]
  className?: string
  height?: string
}) {
  const connsQuery = useConnections()
  const conns = connsQuery.data
  const { data: folders } = useFolders()
  const [q, setQ] = React.useState('')
  const selected = new Set(value)
  const folderName = React.useMemo(() => {
    const byId = new Map((folders ?? []).map((f) => [f.id, f]))
    const path = (id?: string | null): string => {
      const parts: string[] = []
      let cur = id ? byId.get(id) : undefined
      for (let i = 0; cur && i < 20; i++) {
        parts.unshift(cur.name)
        cur = cur.parentId ? byId.get(cur.parentId) : undefined
      }
      return parts.join(' / ')
    }
    return path
  }, [folders])
  const usable = (conns ?? []).filter((c) => (protocols ? protocols.includes(c.protocol) : !['sftp', 'ftp', 's3', 'vnc', 'rdp', 'web'].includes(c.protocol)))
  const needle = q.trim().toLowerCase()
  const shown = needle
    ? usable.filter((c) => `${c.name} ${c.host} ${c.username} ${c.tags.join(' ')} ${folderName(c.folderId)}`.toLowerCase().includes(needle))
    : usable
  const groups = new Map<string, Connection[]>()
  for (const c of shown) {
    const g = folderName(c.folderId)
    const list = groups.get(g) ?? []
    list.push(c)
    groups.set(g, list)
  }
  const allShown = shown.length > 0 && shown.every((c) => selected.has(c.id))
  const setMany = (ids: string[], on: boolean) => {
    const next = new Set(selected)
    for (const id of ids) {
      if (on) next.add(id)
      else next.delete(id)
    }
    onChange(Array.from(next))
  }
  return (
    <div className={cn('flex flex-col gap-1.5', className)}>
      <div className="flex items-center gap-2">
        <Input inputSize="sm" leading={<Search />} placeholder="Filter by name, host, tag or folder…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter connections" />
        <Button size="sm" variant="secondary" disabled={!shown.length} onClick={() => setMany(shown.map((c) => c.id), !allShown)}>
          {allShown ? 'None' : 'All'}
        </Button>
      </div>
      <div className={cn('overflow-y-auto rounded-md border bg-background/40', height)} role="group" aria-label="Saved connections">
        <QueryState query={connsQuery} skeleton={<SkeletonRows rows={4} rowHeight={28} />} errorTitle="Could not load the saved sessions">
          {() =>
            !shown.length ? (
              <p className="p-3 text-sm text-muted-foreground">{usable.length ? 'Nothing matches the filter.' : 'No saved terminal sessions yet.'}</p>
            ) : (
              Array.from(groups.entries()).map(([group, list]) => (
                <div key={group || '_root'}>
                  {group && (
                    <div className="sticky top-0 z-10 flex items-center justify-between bg-muted/80 px-2 py-0.5 text-2xs font-semibold tracking-wide text-muted-foreground uppercase backdrop-blur">
                      <span className="truncate">{group}</span>
                      <button type="button" className="text-2xs font-medium normal-case hover:text-foreground" onClick={() => setMany(list.map((c) => c.id), !list.every((c) => selected.has(c.id)))}>
                        {list.every((c) => selected.has(c.id)) ? 'none' : 'all'}
                      </button>
                    </div>
                  )}
                  {list.map((c) => {
                    const Icon = protocolIcon(c.protocol)
                    const id = `conn-${c.id}`
                    return (
                      <label key={c.id} htmlFor={id} className="flex cursor-pointer items-center gap-2 px-2 py-1 hover:bg-accent">
                        <Checkbox id={id} checked={selected.has(c.id)} onCheckedChange={(v) => setMany([c.id], v === true)} />
                        <Icon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
                        <span className="min-w-0 flex-1 truncate">{c.name}</span>
                        <span className="max-w-[45%] truncate text-xs text-muted-foreground">
                          {c.username ? `${c.username}@` : ''}
                          {c.host || protocolLabel(c.protocol)}
                        </span>
                      </label>
                    )
                  })}
                </div>
              ))
            )
          }
        </QueryState>
      </div>
      <p className="text-xs text-muted-foreground">{plural(value.length, 'connection')} selected</p>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Shortcut capture
// ---------------------------------------------------------------------------------------------------------------------

export function ShortcutInput({ value, onChange, id }: { value: string; onChange: (v: string) => void; id?: string }) {
  const [recording, setRecording] = React.useState(false)
  const resume = React.useRef<(() => void) | null>(null)
  React.useEffect(() => () => resume.current?.(), [])
  const stop = () => {
    resume.current?.()
    resume.current = null
    setRecording(false)
  }
  return (
    <div className="flex items-center gap-1.5">
      <button
        id={id}
        type="button"
        className={cn(
          'flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md border border-input bg-background/60 px-2.5 text-left text-base outline-none dark:bg-input/25',
          'focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25',
          recording && 'border-primary ring-2 ring-primary/25',
        )}
        onClick={() => {
          if (recording) return stop()
          resume.current = suspendKeybindings()
          setRecording(true)
        }}
        onBlur={() => recording && stop()}
        onKeyDown={(e) => {
          if (!recording) return
          e.preventDefault()
          e.stopPropagation()
          if (e.key === 'Escape') return stop()
          if ((e.key === 'Backspace' || e.key === 'Delete') && !e.ctrlKey && !e.metaKey && !e.altKey) {
            onChange('')
            return stop()
          }
          const b = eventToKeybinding(e.nativeEvent)
          if (!b) return
          onChange(b)
          stop()
        }}
        aria-label={recording ? 'Press the new shortcut' : 'Record a keyboard shortcut'}
      >
        <Keyboard className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
        {recording ? <span className="text-muted-foreground">Press keys… (Esc cancels, Backspace clears)</span> : value ? <Kbd keys={value} /> : <span className="text-muted-foreground">None</span>}
      </button>
      {value && !recording && (
        <Button size="icon-sm" variant="ghost" aria-label="Clear shortcut" onClick={() => onChange('')}>
          <X />
        </Button>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Status badge & colours
// ---------------------------------------------------------------------------------------------------------------------

export function StatusBadge({ status, className }: { status: string; className?: string }) {
  switch (status) {
    case 'ok':
      return (
        <Badge variant="success" className={className}>
          <CircleCheck /> OK
        </Badge>
      )
    case 'error':
      return (
        <Badge variant="destructive" className={className}>
          <CircleX /> Failed
        </Badge>
      )
    case 'running':
      return (
        <Badge variant="info" className={className}>
          <Spinner immediate label="Running" className="size-3 text-current" /> Running
        </Badge>
      )
    case 'canceled':
      return (
        <Badge variant="warning" className={className}>
          Canceled
        </Badge>
      )
    case 'skipped':
      return (
        <Badge variant="outline" className={className}>
          Skipped
        </Badge>
      )
    default:
      return (
        <Badge variant="outline" className={className}>
          <CircleDashed /> {status || 'Pending'}
        </Badge>
      )
  }
}

export function ColorSelect({
  value,
  onChange,
  allowNone,
  noneLabel = 'None',
  'aria-label': ariaLabel,
}: {
  value: string | undefined
  onChange: (v: string | undefined) => void
  allowNone?: boolean
  noneLabel?: string
  'aria-label'?: string
}) {
  const isHex = !!value && value.startsWith('#')
  return (
    <div className="flex items-center gap-1.5">
      <select
        className="h-7 min-w-0 flex-1 rounded-md border border-input bg-background/60 px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25 dark:bg-input/25"
        value={isHex ? 'custom' : (value ?? '')}
        aria-label={ariaLabel}
        onChange={(e) => {
          const v = e.target.value
          if (v === '') onChange(undefined)
          else if (v === 'custom') onChange(isHex ? value : '#ff8800')
          else onChange(v)
        }}
      >
        {allowNone && <option value="">{noneLabel}</option>}
        {ANSI_COLORS.map((c) => (
          <option key={c.id} value={c.id}>
            {c.label}
          </option>
        ))}
        <option value="custom">Custom…</option>
      </select>
      {isHex ? (
        <input type="color" value={value} onChange={(e) => onChange(e.target.value)} aria-label="Custom colour" className="h-7 w-8 cursor-pointer rounded border border-input bg-transparent p-0.5" />
      ) : (
        <span className="size-4 shrink-0 rounded-sm border" style={{ background: cssColor(value) ?? 'transparent' }} aria-hidden />
      )}
    </div>
  )
}
