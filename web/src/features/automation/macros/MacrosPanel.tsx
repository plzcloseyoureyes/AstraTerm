/*
 * Macros sidebar panel (AUTO-1): record from the active terminal, replay with the recorded timing (backend job, so
 * background tabs keep the timing) on the active terminal, the broadcast group or chosen sessions; edit, delete.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Circle, Clapperboard, FastForward, Hourglass, KeyRound, Pencil, Play, Plus, Search, SendToBack, Square, Trash2, Users, X } from 'lucide-react'
import { toast } from 'sonner'
import type { Macro } from '../types'
import { Button } from '@/components/ui/button'
import { ContextMenuItem, ContextMenuSeparator } from '@/components/ui/context-menu'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenuItem, DropdownMenuSeparator } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { Kbd } from '@/components/ui/kbd'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { errorMessage, formatDuration, plural } from '@/lib/utils'
import { autoKeys, deleteMacro, useMacros } from '../api'
import { LibraryRow, type MenuKind } from '../components/LibraryRow'
import { startLibraryDrag } from '../plugins/dropTarget'
import { automationSettings } from '../settings'
import { openMacroEditor } from '../store'
import { macroDuration, playMacro } from './play'
import { startRecording, stopRecording, useRecorder } from './recorder'

function useRecordingClock(): string {
  const startedAt = useRecorder((s) => s.startedAt)
  const [, tick] = React.useState(0)
  React.useEffect(() => {
    if (!startedAt) return
    const t = setInterval(() => tick((n) => n + 1), 1000)
    return () => clearInterval(t)
  }, [startedAt])
  return startedAt ? formatDuration(Date.now() - startedAt) : ''
}

export function RecordingBanner() {
  const rec = useRecorder()
  const clock = useRecordingClock()
  if (!rec.recording) return null
  return (
    <div className="flex items-center gap-2 border-b bg-destructive/10 px-2 py-1.5 text-sm" role="status">
      <Circle className="size-2.5 fill-destructive text-destructive" aria-hidden />
      <span className="min-w-0 flex-1 truncate">
        Recording <span className="font-medium">{rec.title}</span> · {clock} · {rec.steps.length} steps
      </span>
      <Button size="xs" variant="destructive" onClick={stopRecording}>
        <Square className="fill-current" /> Stop
      </Button>
    </div>
  )
}

function MacroItems({ m, kind, onDelete }: { m: Macro; kind: MenuKind; onDelete: () => void }) {
  const Item = kind === 'dropdown' ? DropdownMenuItem : ContextMenuItem
  const Sep = kind === 'dropdown' ? DropdownMenuSeparator : ContextMenuSeparator
  return (
    <>
      <Item onSelect={() => void playMacro(m, 'active')}>
        <Play /> Play in active terminal
      </Item>
      <Item onSelect={() => void playMacro(m, 'multiexec')}>
        <SendToBack /> Play in broadcast group
      </Item>
      <Item onSelect={() => void playMacro(m, 'pick')}>
        <Users /> Play in sessions…
      </Item>
      <Item onSelect={() => void playMacro(m, 'active', 0)}>
        <FastForward /> Play without delays
      </Item>
      <Sep />
      <Item onSelect={() => openMacroEditor({ id: m.id })}>
        <Pencil /> Edit…
      </Item>
      <Item variant="destructive" onSelect={onDelete}>
        <Trash2 /> Delete
      </Item>
    </>
  )
}

/** "3 steps · 2.4 s" (the duration only when the macro waits between steps). */
function macroSummary(m: Macro): string {
  const ms = macroDuration(m)
  return `${plural(m.steps.length, 'step')}${ms > 0 ? ` · ${formatDuration(ms)}` : ''}`
}

function MacroRow({ m }: { m: Macro }) {
  const qc = useQueryClient()
  const shortcut = automationSettings.useValue('macroShortcuts')[m.id]
  const remove = async () => {
    if (!(await confirm({ title: `Delete “${m.name}”?`, destructive: true, confirmLabel: 'Delete' }))) return
    try {
      await deleteMacro(m.id)
      const sc = { ...automationSettings.get().macroShortcuts }
      if (sc[m.id]) {
        delete sc[m.id]
        automationSettings.set({ macroShortcuts: sc })
      }
      await qc.invalidateQueries({ queryKey: autoKeys.macros })
    } catch (err) {
      toast.error('Could not delete the macro', { description: errorMessage(err) })
    }
  }
  const waits = m.steps.some((st) => st.waitFor)
  const secret = m.steps.some((st) => st.secret)
  return (
    <LibraryRow
      name={m.name}
      icon={<Play className="text-primary" />}
      badge={
        <>
          {waits && <Hourglass className="size-3 shrink-0 text-muted-foreground" aria-label="Waits for output" />}
          {secret && <KeyRound className="size-3 shrink-0 text-muted-foreground" aria-label="Types a stored secret" />}
        </>
      }
      meta={shortcut ? <Kbd keys={shortcut} /> : macroSummary(m)}
      hint={`${macroSummary(m)}${waits ? ' · waits for output' : ''}${secret ? ' · types a stored secret' : ''}\n\nClick: play in the active terminal · Shift+click: broadcast group · drag onto any terminal`}
      onActivate={(e) => void playMacro(m, e.shiftKey ? 'multiexec' : 'active')}
      onDragStart={(e) => startLibraryDrag(e, 'macro', m.id, m.name)}
      menu={(kind) => <MacroItems m={m} kind={kind} onDelete={remove} />}
    />
  )
}

export default function MacrosPanel() {
  const macros = useMacros()
  const { data } = macros
  const recording = useRecorder((s) => s.recording)
  const [q, setQ] = React.useState('')
  const needle = q.trim().toLowerCase()
  const list = (data ?? []).filter((m) => !needle || m.name.toLowerCase().includes(needle))
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex h-9 shrink-0 items-center gap-1 px-1.5">
        <Input
          inputSize="sm"
          variant="filled"
          leading={<Search />}
          trailing={q ? <IconButton icon={X} label="Clear search" size="xs" onClick={() => setQ('')} /> : undefined}
          placeholder="Search macros…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          aria-label="Search macros"
        />
        {recording ? (
          <IconButton icon={Square} label="Stop recording" className="text-destructive" onClick={stopRecording} />
        ) : (
          <IconButton icon={Circle} label="Record a macro in the active terminal" iconClassName="fill-destructive text-destructive" onClick={() => startRecording()} />
        )}
        <IconButton icon={Plus} label="New macro" onClick={() => openMacroEditor({})} />
      </div>
      <RecordingBanner />
      <div className="min-h-0 flex-1 overflow-y-auto px-1 py-1">
        <QueryState
          query={macros}
          skeleton={<SkeletonRows rows={6} />}
          errorTitle="Could not load macros"
          isEmpty={(all) => !all.length}
          empty={
            <EmptyState
              size="sm"
              icon={Clapperboard}
              title="No macros yet"
              description="Record what you type in a terminal, then replay it with the same timing in one or many sessions."
              action={
                <Button size="sm" onClick={() => startRecording()} disabled={recording}>
                  <Circle className="fill-current" /> Record a macro
                </Button>
              }
            />
          }
        >
          {() =>
            !list.length ? (
              <EmptyState size="sm" title="Nothing matches" description="Try another search." />
            ) : (
              <ul>
                {list.map((m) => (
                  <MacroRow key={m.id} m={m} />
                ))}
              </ul>
            )
          }
        </QueryState>
      </div>
    </div>
  )
}
