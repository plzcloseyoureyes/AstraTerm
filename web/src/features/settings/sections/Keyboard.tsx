import { useEffect, useMemo, useState } from 'react'
import { Plus, RotateCcw, Search, TriangleAlert, X } from 'lucide-react'
import { commands, type CommandDef } from '@/app/registry'
import { getKeybindings, toBindingList } from '@/app/commands'
import { eventToKeybinding, findConflicts, formatKeybinding, normalizeKeybinding, suspendKeybindings } from '@/app/keybindings'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Input } from '@/components/ui/input'
import { Kbd } from '@/components/ui/kbd'
import { StatusDot } from '@/components/ui/status-dot'
import { Tooltip } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { keybindingSettings, type KeybindingOverrides } from '@/stores/settings'
import { SettingsPage } from '../ui'

function defaultsOf(id: string): string[] {
  return toBindingList(commands.get(id)?.keybinding)
}

function sameList(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const na = a.map(normalizeKeybinding).sort()
  const nb = b.map(normalizeKeybinding).sort()
  return na.every((x, i) => x === nb[i])
}

/** Persist a command's bindings; storing nothing when they equal the defaults. */
function setBindings(id: string, list: string[], overrides: KeybindingOverrides = keybindingSettings.get()): KeybindingOverrides {
  const next = { ...overrides }
  if (sameList(list, defaultsOf(id))) delete next[id]
  else next[id] = list
  return next
}

interface Pending {
  commandId: string
  binding: string
  conflicts: string[]
}

export default function KeyboardSection() {
  const all = commands.useList()
  const overrides = keybindingSettings.use()
  const [query, setQuery] = useState('')
  const [recordingFor, setRecordingFor] = useState<string | null>(null)
  const [pending, setPending] = useState<Pending | null>(null)

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    return [...all]
      .filter((c) => {
        if (!q) return true
        const keys = getKeybindings(c.id, overrides).map(formatKeybinding).join(' ').toLowerCase()
        return [c.title, c.category ?? '', c.id, keys].some((s) => s.toLowerCase().includes(q))
      })
      .sort((a, b) => (a.category ?? '~').localeCompare(b.category ?? '~') || a.title.localeCompare(b.title))
  }, [all, query, overrides])

  const grouped = useMemo(() => {
    const m = new Map<string, CommandDef[]>()
    for (const c of rows) {
      const k = c.category ?? 'Other'
      if (!m.has(k)) m.set(k, [])
      m.get(k)!.push(c)
    }
    return Array.from(m.entries())
  }, [rows])

  // Capture the next key combination while recording.
  useEffect(() => {
    if (!recordingFor) return
    const release = suspendKeybindings()
    const onKey = (e: KeyboardEvent) => {
      e.preventDefault()
      e.stopPropagation()
      if (e.key === 'Escape' && !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey) {
        setRecordingFor(null)
        return
      }
      const binding = eventToKeybinding(e)
      if (!binding) return
      const id = recordingFor
      setRecordingFor(null)
      if (getKeybindings(id).some((b) => normalizeKeybinding(b) === normalizeKeybinding(binding))) return
      const conflicts = findConflicts(binding, id)
      if (conflicts.length) setPending({ commandId: id, binding, conflicts })
      else keybindingSettings.replace(setBindings(id, [...getKeybindings(id), binding]))
    }
    window.addEventListener('keydown', onKey, true)
    return () => {
      window.removeEventListener('keydown', onKey, true)
      release()
    }
  }, [recordingFor])

  const assign = (p: Pending, reassign: boolean) => {
    let next = setBindings(p.commandId, [...getKeybindings(p.commandId), p.binding])
    if (reassign) {
      const target = normalizeKeybinding(p.binding)
      for (const other of p.conflicts) {
        const list = getKeybindings(other, next).filter((b) => normalizeKeybinding(b) !== target)
        next = setBindings(other, list, next)
      }
    }
    keybindingSettings.replace(next)
    setPending(null)
  }

  const modifiedCount = Object.keys(overrides).length

  return (
    <SettingsPage
      title="Keyboard shortcuts"
      description="Click + to record a new shortcut. Browsers reserve some keys (Ctrl+W, Ctrl+T, Ctrl+N) — alternatives are provided, and they work in full screen or an installed app window."
      actions={
        <Button
          variant="ghost"
          size="sm"
          disabled={!modifiedCount}
          onClick={async () => {
            if (await confirm({ title: 'Reset all shortcuts?', description: `${modifiedCount} customised command(s) return to their defaults.`, confirmLabel: 'Reset all', destructive: true }))
              keybindingSettings.replace({})
          }}
        >
          <RotateCcw /> Reset all
        </Button>
      }
    >
      <div className="grid gap-4">
        <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search by command or key (e.g. Ctrl+K)" leading={<Search />} aria-label="Search shortcuts" />

        {pending && (
          <div role="alert" className="grid gap-2 rounded-lg border border-warning/50 bg-warning/10 p-3 text-sm">
            <div className="flex items-start gap-2">
              <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" />
              <div>
                <Kbd keys={pending.binding} /> is already used by{' '}
                {pending.conflicts.map((id, i) => (
                  <span key={id}>
                    {i > 0 && ', '}
                    <strong>{commands.get(id)?.title ?? id}</strong>
                  </span>
                ))}
                .
              </div>
            </div>
            <div className="flex flex-wrap gap-2 pl-6">
              <Button size="xs" onClick={() => assign(pending, true)}>
                Reassign to {commands.get(pending.commandId)?.title ?? pending.commandId}
              </Button>
              <Button size="xs" variant="secondary" onClick={() => assign(pending, false)}>
                Keep both
              </Button>
              <Button size="xs" variant="ghost" onClick={() => setPending(null)}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {grouped.length === 0 && <p className="py-6 text-center text-sm text-muted-foreground">No commands match “{query}”.</p>}

        {grouped.map(([category, cmds]) => (
          <section key={category} className="grid gap-1.5">
            <h2 className="px-1 text-xs font-semibold tracking-wide text-muted-foreground uppercase">{category}</h2>
            <div className="divide-y rounded-lg border bg-card">
              {cmds.map((c) => (
                <ShortcutRow
                  key={c.id}
                  command={c}
                  bindings={getKeybindings(c.id, overrides)}
                  modified={Array.isArray(overrides[c.id])}
                  recording={recordingFor === c.id}
                  onRecord={() => {
                    setPending(null)
                    setRecordingFor(c.id)
                  }}
                  onCancelRecord={() => setRecordingFor(null)}
                  onRemove={(b) => keybindingSettings.replace(setBindings(c.id, getKeybindings(c.id).filter((x) => x !== b)))}
                  onReset={() => keybindingSettings.replace(setBindings(c.id, defaultsOf(c.id)))}
                />
              ))}
            </div>
          </section>
        ))}
      </div>
    </SettingsPage>
  )
}

function ShortcutRow({
  command,
  bindings,
  modified,
  recording,
  onRecord,
  onCancelRecord,
  onRemove,
  onReset,
}: {
  command: CommandDef
  bindings: string[]
  modified: boolean
  recording: boolean
  onRecord: () => void
  onCancelRecord: () => void
  onRemove: (binding: string) => void
  onReset: () => void
}) {
  const Icon = command.icon
  return (
    <div className={cn('flex flex-wrap items-center gap-x-4 gap-y-2 px-3.5 py-2', recording && 'bg-primary/5')}>
      <div className="flex min-w-0 flex-1 items-center gap-2">
        {Icon ? <Icon className="size-3.5 shrink-0 text-muted-foreground" /> : <span className="size-3.5" />}
        <div className="grid min-w-0">
          <span className="truncate">{command.title}</span>
          <span className="truncate font-mono text-2xs text-muted-foreground">{command.id}</span>
        </div>
        {modified && (
          <Badge variant="default" className="shrink-0">
            modified
          </Badge>
        )}
      </div>
      <div className="flex flex-wrap items-center justify-end gap-1.5">
        {recording ? (
          <span className="flex h-6 items-center gap-2 rounded-md border border-primary/60 bg-background px-2 text-sm text-primary" aria-live="polite">
            <StatusDot tone="primary" pending />
            Press keys… <span className="text-muted-foreground">(Esc to cancel)</span>
            <button type="button" aria-label="Cancel recording" onClick={onCancelRecord} className="rounded-sm p-0.5 hover:bg-accent">
              <X className="size-3" />
            </button>
          </span>
        ) : (
          <>
            {bindings.length === 0 && <span className="text-sm text-muted-foreground">—</span>}
            {bindings.map((b) => (
              <span key={b} className="group inline-flex items-center gap-0.5">
                <Kbd keys={b} className="h-5 px-1.5 text-xs" />
                <Tooltip content="Remove shortcut">
                  <button
                    type="button"
                    onClick={() => onRemove(b)}
                    aria-label={`Remove ${formatKeybinding(b)}`}
                    className="flex size-4 items-center justify-center rounded-sm text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 hover:bg-accent hover:text-foreground focus-visible:opacity-100"
                  >
                    <X className="size-3" />
                  </button>
                </Tooltip>
              </span>
            ))}
          </>
        )}
        {!recording && (
          <Tooltip content="Add shortcut">
            <Button variant="ghost" size="icon-xs" aria-label={`Add shortcut for ${command.title}`} onClick={onRecord}>
              <Plus />
            </Button>
          </Tooltip>
        )}
        {modified && !recording && (
          <Tooltip content="Reset to default">
            <Button variant="ghost" size="icon-xs" aria-label={`Reset ${command.title}`} onClick={onReset}>
              <RotateCcw />
            </Button>
          </Tooltip>
        )}
      </div>
    </div>
  )
}
