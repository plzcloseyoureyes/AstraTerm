/*
 * Floating MultiExec bar (AUTO-5): target selection (visible / all / selected tabs, per-tab include & exclude), a
 * compose line sent to every participant, quick keys (Enter, Ctrl+C) and a stop button. Mounted in its own React root
 * (it lives outside any dock tab) while MultiExec is on.
 */
import { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { ChevronDown, ChevronUp, CornerDownLeft, Send, Workflow, X } from 'lucide-react'
import { queryClient } from '@/api/queryClient'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { TooltipProvider } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { useUIStore } from '@/stores/ui'
import { focusTab, useWorkspaceStore } from '@/stores/workspace'
import { emitTerminalInput, useTerminalInfoStore, useTerminals } from './bus'
import { confirmIfDangerous } from './guard'
import { isParticipant, setMultiExecIncluded, setMultiExecScope, toggleMultiExec, useMultiExecStore, type MultiExecScope } from './multiexec'
import type { TerminalHandle } from './types'

function useWorkspaceRect(): DOMRect | null {
  const [rect, setRect] = useState<DOMRect | null>(null)
  useEffect(() => {
    let el: Element | null = null
    let ro: ResizeObserver | null = null
    const measure = () => {
      const target = document.querySelector('[data-workspace]')
      if (target !== el) {
        ro?.disconnect()
        el = target
        if (el) {
          ro = new ResizeObserver(measure)
          ro.observe(el)
        }
      }
      setRect(el ? el.getBoundingClientRect() : null)
    }
    measure()
    window.addEventListener('resize', measure)
    const t = setInterval(measure, 2000)
    return () => {
      window.removeEventListener('resize', measure)
      clearInterval(t)
      ro?.disconnect()
    }
  }, [])
  return rect
}

const STATE_DOT: Record<string, string> = {
  connecting: 'bg-warning',
  authenticating: 'bg-warning',
  connected: 'bg-success',
  error: 'bg-destructive',
}

function sendAll(targets: TerminalHandle[], data: string): number {
  let n = 0
  for (const t of targets) {
    if (t.send(data)) {
      n++
      emitTerminalInput({ tabId: t.tabId, sessionId: t.sessionId, data, broadcast: true })
    }
  }
  return n
}

function MultiExecBar() {
  const st = useMultiExecStore()
  const locked = useUIStore((s) => s.locked)
  const terminals = useTerminals()
  const infos = useTerminalInfoStore((s) => s.infos)
  // Re-evaluate visibility-based participation when tabs move / activate.
  useWorkspaceStore((s) => s.tabs)
  useWorkspaceStore((s) => s.activeTabId)
  const rect = useWorkspaceRect()
  const [compose, setCompose] = useState('')
  // Narrow workspaces start collapsed so the bar does not cover the terminals.
  const [expanded, setExpanded] = useState(() => (document.querySelector('[data-workspace]')?.getBoundingClientRect().width ?? window.innerWidth) >= 640)

  useEffect(() => {
    // All terminals closed: nothing left to broadcast to.
    if (!terminals.length) toggleMultiExec(false)
  }, [terminals.length])

  if (locked || !st.active) return null
  const targets = terminals.filter((t) => isParticipant(t.tabId, st))
  const send = (data: string) => {
    if (!data) return
    sendAll(targets, data)
  }
  const left = rect ? rect.left + rect.width / 2 : window.innerWidth / 2
  const bottom = rect ? Math.max(8, window.innerHeight - rect.bottom + 12) : 40
  const maxWidth = rect ? Math.max(320, rect.width - 32) : 720

  return (
    <div
      role="toolbar"
      aria-label="Broadcast input"
      className="fixed z-40 flex w-[40rem] flex-col gap-2 rounded-lg border border-amber-500/50 bg-popover/95 p-2 text-popover-foreground shadow-popover backdrop-blur"
      style={{ left, bottom, transform: 'translateX(-50%)', maxWidth }}
      onKeyDown={(e) => e.stopPropagation()}
    >
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex items-center gap-1.5 text-base font-medium">
          <Workflow className="size-4 text-amber-500" />
          Broadcast
        </div>
        <span className="rounded-sm bg-amber-500/15 px-1.5 py-px text-xs font-medium text-amber-600 dark:text-amber-400">
          {targets.length} of {terminals.length} terminal{terminals.length === 1 ? '' : 's'}
        </span>
        <SegmentedControl
          size="sm"
          aria-label="Broadcast to"
          value={st.scope}
          onValueChange={(v: MultiExecScope) => setMultiExecScope(v)}
          options={[
            { value: 'visible', label: 'Visible' },
            { value: 'all', label: 'All' },
            { value: 'selected', label: 'Selected' },
          ]}
        />
        <div className="ml-auto flex items-center gap-1">
          <Button size="icon-xs" variant="ghost" aria-label={expanded ? 'Collapse' : 'Expand'} title={expanded ? 'Collapse' : 'Expand'} onClick={() => setExpanded((e) => !e)}>
            {expanded ? <ChevronDown /> : <ChevronUp />}
          </Button>
          <Button size="xs" variant="secondary" onClick={() => toggleMultiExec(false)}>
            <X /> Stop
          </Button>
        </div>
      </div>
      {expanded && (
        <>
          <ul className="flex max-h-28 flex-wrap gap-1 overflow-y-auto" aria-label="Terminals">
            {terminals.map((t) => {
              const info = infos[t.tabId]
              const included = isParticipant(t.tabId, st)
              const id = `me-${t.tabId}`
              return (
                <li
                  key={t.tabId}
                  className={cn(
                    'flex h-6 max-w-56 items-center gap-1.5 rounded-md border px-1.5 text-sm',
                    included ? 'border-amber-500/50 bg-amber-500/10' : 'opacity-70',
                  )}
                >
                  <Checkbox id={id} checked={included} onCheckedChange={(v) => setMultiExecIncluded(t.tabId, v === true)} className="size-3.5" />
                  <span className={cn('size-1.5 shrink-0 rounded-full', STATE_DOT[info?.state ?? ''] ?? 'bg-muted-foreground/50')} />
                  <label htmlFor={id} className="truncate" title={info?.title}>
                    {info?.title ?? t.tabId}
                  </label>
                  <button type="button" className="text-xs text-muted-foreground hover:text-foreground" title="Show tab" onClick={() => focusTab(t.tabId)}>
                    ↗
                  </button>
                </li>
              )
            })}
          </ul>
          <form
            className="flex items-center gap-1.5"
            onSubmit={(e) => {
              e.preventDefault()
              const line = compose
              const n = targets.length
              // The compose line bypasses the terminals' input path: run it past the dangerous-command guard first.
              void confirmIfDangerous(line, n).then((ok) => {
                if (!ok) return
                send(`${line}\r`)
                setCompose((cur) => (cur === line ? '' : cur))
              })
            }}
          >
            <input
              value={compose}
              onChange={(e) => setCompose(e.target.value)}
              placeholder={targets.length ? `Command for ${targets.length} terminal${targets.length === 1 ? '' : 's'} — Enter sends` : 'No target terminals'}
              aria-label="Command to send to all target terminals"
              spellCheck={false}
              autoComplete="off"
              disabled={!targets.length}
              className="h-7 min-w-0 flex-1 rounded-md border border-input bg-background/60 px-2 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25 disabled:opacity-50 dark:bg-input/25"
            />
            <Button type="submit" size="sm" disabled={!targets.length} title="Send with Enter">
              <Send /> Send
            </Button>
            <Button
              type="button"
              size="sm"
              variant="secondary"
              disabled={!targets.length || !compose}
              title="Send without pressing Enter"
              onClick={() => {
                send(compose)
                setCompose('')
              }}
            >
              Type
            </Button>
            <Button type="button" size="icon-sm" variant="secondary" disabled={!targets.length} title="Press Enter in all terminals" aria-label="Press Enter in all terminals" onClick={() => send('\r')}>
              <CornerDownLeft />
            </Button>
            <Button type="button" size="sm" variant="secondary" disabled={!targets.length} title="Send Ctrl+C to all terminals" onClick={() => send('\x03')}>
              ^C
            </Button>
          </form>
          <p className="text-xs text-muted-foreground">Keystrokes and pastes in any highlighted terminal are sent to all of them.</p>
        </>
      )}
    </div>
  )
}

/** Mount the bar into its own root; returns the unmount function. */
export function mountMultiExecBar(): () => void {
  const el = document.createElement('div')
  el.dataset.nxMultiexec = ''
  document.body.appendChild(el)
  const root = createRoot(el)
  root.render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <MultiExecBar />
      </TooltipProvider>
    </QueryClientProvider>,
  )
  return () => {
    root.unmount()
    el.remove()
  }
}
