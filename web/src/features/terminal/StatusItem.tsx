/*
 * Status bar items: the active terminal's state, size (cols × rows) and encoding; the MultiExec indicator.
 */
import { Eye, Pause, Workflow } from 'lucide-react'
import { runCommand } from '@/app/commands'
import { statusDotClass, useSteadyStatus } from '@/components/ui/status-dot'
import { StatusBarItem } from '@/layout/StatusBar'
import { cn } from '@/lib/utils'
import { useActiveTerminalInfo, useTerminals } from './bus'
import { isParticipant, toggleMultiExec, useMultiExecStore } from './multiexec'
import type { TerminalInfo } from './types'

const STATE: Record<string, { label: string; dot: string }> = {
  connecting: { label: 'Connecting', dot: statusDotClass('warning', true) },
  authenticating: { label: 'Authenticating', dot: statusDotClass('warning', true) },
  connected: { label: 'Connected', dot: 'bg-success' },
  disconnected: { label: 'Disconnected', dot: 'bg-muted-foreground/60' },
  closed: { label: 'Closed', dot: 'bg-muted-foreground/40' },
  error: { label: 'Error', dot: 'bg-destructive' },
  gone: { label: 'Session gone', dot: 'bg-muted-foreground/40' },
  unknown: { label: 'Attaching', dot: 'bg-muted-foreground/40' },
}

const RECONNECTING = { label: 'Reconnecting to server', dot: statusDotClass('warning', true) }
const PENDING = new Set([STATE.connecting, STATE.authenticating, STATE.unknown, RECONNECTING])

function stateOf(info: TerminalInfo | null | undefined): { label: string; dot: string } {
  if (!info) return STATE.unknown
  const base = STATE[info.state] ?? STATE.unknown
  if (info.state === 'connected' && info.transport !== 'open') return RECONNECTING
  return base
}

export function TerminalStatusItem() {
  const info = useActiveTerminalInfo()
  // Short connecting / reconnecting blips keep showing the previous state (no green → amber → green swap).
  const st = useSteadyStatus(stateOf(info), (x) => PENDING.has(x)) ?? STATE.unknown
  if (!info) return null
  const mismatch = info.ptyCols && info.ptyRows && (info.ptyCols !== info.cols || info.ptyRows !== info.rows)
  const details = [
    `${st.label}${info.stateMessage ? ` — ${info.stateMessage}` : ''}`,
    `Renderer: ${info.renderer === 'webgl' ? 'WebGL' : 'DOM'} · font ${info.fontSize}px`,
    info.cwd ? `Directory: ${info.cwd}` : null,
  ]
    .filter(Boolean)
    .join('\n')
  return (
    <>
      <StatusBarItem
        tooltip={<span className="whitespace-pre-line">{details}</span>}
        onClick={() => void runCommand('settings.open', { section: 'terminal' }, { source: 'api' })}
        aria-label={`Terminal ${st.label}`}
      >
        <span className="flex items-center gap-1.5">
          <span className={cn('size-1.5 rounded-full', st.dot)} aria-hidden />
          {st.label}
        </span>
      </StatusBarItem>
      <StatusBarItem
        tone={mismatch ? 'warning' : 'muted'}
        tooltip={mismatch ? `This view is ${info.cols}×${info.rows}; another client set the remote size to ${info.ptyCols}×${info.ptyRows}.` : 'Terminal size (columns × rows)'}
        aria-label="Terminal size"
      >
        <span className="tabular">
          {info.cols}×{info.rows}
        </span>
      </StatusBarItem>
      <StatusBarItem tone="muted" tooltip="Character encoding" aria-label="Encoding">
        {info.encoding}
      </StatusBarItem>
      {info.paused && (
        <StatusBarItem icon={Pause} tone="warning" tooltip="Output paused (Scroll Lock) — click to resume" onClick={() => void runCommand('terminal.pauseOutput')}>
          Paused
        </StatusBarItem>
      )}
      {info.readOnly && (
        <StatusBarItem icon={Eye} tone="muted" tooltip="Read-only view">
          Read-only
        </StatusBarItem>
      )}
    </>
  )
}

export function MultiExecStatusItem() {
  const st = useMultiExecStore()
  const terminals = useTerminals()
  if (!st.active) return null
  const n = terminals.filter((t) => isParticipant(t.tabId, st)).length
  return (
    <StatusBarItem icon={Workflow} tone="accent" className="text-amber-500" tooltip="Broadcast is on: input goes to several terminals — click to stop" onClick={() => toggleMultiExec(false)}>
      Broadcast · {n}
    </StatusBarItem>
  )
}
