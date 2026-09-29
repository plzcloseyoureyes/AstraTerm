/*
 * Terminal overlays: connecting spinner, end-of-session prompt (PROTO-39: R = reconnect, D = duplicate, S = save output,
 * Q/Enter = close), transport reconnect badge, read-only / paused / paste-progress badges.
 */
import { useEffect, useState, type ReactNode } from 'react'
import { CircleAlert, CopyPlus, Download, Eye, Lock, Pause, Play, PlugZap, RefreshCw, Unplug, WifiOff, X, CircleStop, SearchX } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Kbd } from '@/components/ui/kbd'
import { Delayed, Spinner } from '@/components/ui/spinner'
import { cn, formatBytes } from '@/lib/utils'
import { closeTab } from '@/stores/workspace'
import type { TerminalController } from './controller'
import { duplicateSession } from './open'
import type { TerminalInfo } from './types'

function endedCopy(info: TerminalInfo): { title: string; icon: typeof Unplug; tone: 'muted' | 'error' } {
  const hasExit = info.exitCode !== undefined && info.exitCode !== null
  switch (info.state) {
    case 'closed':
      return { title: hasExit ? `Session closed (exit code ${info.exitCode})` : 'Session closed', icon: CircleStop, tone: 'muted' }
    case 'disconnected':
      // A process that exited carries an exit code and no error message; anything else is a lost connection.
      if (hasExit && !info.stateMessage) return { title: `Session ended (exit code ${info.exitCode})`, icon: CircleStop, tone: info.exitCode ? 'error' : 'muted' }
      if (!info.stateMessage) return { title: 'Session ended', icon: CircleStop, tone: 'muted' }
      return { title: 'Connection lost', icon: Unplug, tone: 'error' }
    case 'error':
      return { title: 'Connection failed', icon: CircleAlert, tone: 'error' }
    case 'gone':
      return { title: 'This session no longer exists', icon: SearchX, tone: 'muted' }
    default:
      return { title: 'Session ended', icon: Unplug, tone: 'muted' }
  }
}

function ConnectingOverlay({ info }: { info: TerminalInfo }) {
  const label = info.state === 'authenticating' ? 'Authenticating…' : info.transport === 'reconnecting' ? 'Connecting to the server…' : 'Connecting…'
  return (
    <div className="pointer-events-none absolute inset-0 z-20 flex items-center justify-center">
      <div className="flex max-w-sm flex-col items-center gap-2 rounded-lg border bg-popover/95 px-5 py-4 text-center shadow-popover">
        <div className="flex items-center gap-2 text-base font-medium">
          <Spinner className="size-4" label={label} />
          {label}
        </div>
        {info.stateMessage && <div className="text-sm break-words text-muted-foreground">{info.stateMessage}</div>}
        {info.state === 'authenticating' && <div className="text-xs text-muted-foreground">Answer the prompt to continue.</div>}
      </div>
    </div>
  )
}

function EndedPanel({ ctrl, info }: { ctrl: TerminalController; info: TerminalInfo }) {
  const { title, icon: Icon, tone } = endedCopy(info)
  // Closed or vanished sessions cannot be reconnected: a new session replaces them in this tab.
  const gone = info.state === 'gone' || info.state === 'closed'
  return (
    <div className="absolute inset-x-0 bottom-0 z-20 flex justify-center bg-gradient-to-t from-black/35 to-transparent px-3 pt-10 pb-4">
      <div
        role="alertdialog"
        aria-label={title}
        className="pointer-events-auto flex w-full max-w-lg flex-col gap-3 rounded-lg border bg-popover px-4 py-3 text-popover-foreground shadow-popover"
      >
        <div className="flex items-start gap-3">
          <div
            className={cn(
              'mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-md border',
              tone === 'error' ? 'bg-destructive/10 text-destructive' : 'bg-muted text-muted-foreground',
            )}
          >
            <Icon className="size-4" />
          </div>
          <div className="grid min-w-0 gap-0.5">
            <div className="text-base font-medium">{title}</div>
            {info.stateMessage && <div className="text-sm break-words text-muted-foreground">{info.stateMessage}</div>}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <Button size="sm" onClick={() => ctrl.reconnect()} autoFocus={false}>
            <RefreshCw /> {gone ? 'Start new session' : 'Reconnect'}
            <Kbd className="ml-1 border-primary-foreground/30 bg-primary-foreground/15 text-primary-foreground">R</Kbd>
          </Button>
          <Button size="sm" variant="secondary" onClick={() => void duplicateSession(ctrl.tabId)}>
            <CopyPlus /> Duplicate <Kbd className="ml-1">D</Kbd>
          </Button>
          <Button size="sm" variant="secondary" onClick={() => ctrl.saveOutput('text')}>
            <Download /> Save output <Kbd className="ml-1">S</Kbd>
          </Button>
          <Button size="sm" variant="ghost" className="ml-auto" onClick={() => void closeTab(ctrl.tabId, { force: true })}>
            <X /> Close <Kbd className="ml-1">Q</Kbd>
          </Button>
        </div>
      </div>
    </div>
  )
}

function Badge({ children, tone = 'default', className }: { children: ReactNode; tone?: 'default' | 'warning' | 'danger'; className?: string }) {
  return (
    <div
      className={cn(
        'pointer-events-auto flex h-7 items-center gap-1.5 rounded-md border px-2 text-sm shadow-popover backdrop-blur',
        tone === 'warning' && 'border-warning/40 bg-warning/15 text-warning',
        tone === 'danger' && 'border-destructive/40 bg-destructive/15 text-destructive',
        tone === 'default' && 'bg-popover/90 text-popover-foreground',
        className,
      )}
    >
      {children}
    </div>
  )
}

/** The last non-empty value: a badge kept on screen for its minimum time (Delayed) keeps its text. */
function useLastValue<T>(value: T | null | undefined): T | null | undefined {
  const [last, setLast] = useState(value)
  useEffect(() => {
    if (value) setLast(value)
  }, [value])
  return value || last
}

export function TerminalOverlays({ ctrl, info, belowToolbar }: { ctrl: TerminalController | null; info: TerminalInfo | null; belowToolbar?: boolean }) {
  const lockReason = useLastValue(info?.inputLock)
  const pasting = useLastValue(info?.pasting)
  if (!ctrl || !info) {
    return (
      <div className="pointer-events-none absolute inset-0 z-20 flex items-center justify-center">
        <Spinner />
      </div>
    )
  }
  const ended = info.state === 'closed' || info.state === 'disconnected' || info.state === 'error' || info.state === 'gone'
  const connecting = info.state === 'connecting' || info.state === 'authenticating' || info.state === 'unknown'
  const transportDown = info.transport !== 'open' && !ended
  const endPrompt = ended && ctrl.endPromptActive()
  return (
    <>
      {/* Kept mounted: shown only when connecting takes > 300 ms, then for ≥ 400 ms (docs/UX.md). */}
      <Delayed active={connecting && !info.hasOutput}>
        <ConnectingOverlay info={info} />
      </Delayed>
      {/* Delayed like every state change that can be momentary (a restored workspace restarting a gone session). */}
      <Delayed active={endPrompt}>
        <EndedPanel ctrl={ctrl} info={info} />
      </Delayed>
      <div className={cn('pointer-events-none absolute right-4 z-20 flex flex-col items-end gap-1.5', info.searchOpen ? 'top-12' : belowToolbar ? 'top-10' : 'top-2')} aria-live="polite">
        <Delayed active={connecting && info.hasOutput}>
          <Badge>
            <Spinner className="size-3.5" />
            {info.state === 'authenticating' ? 'Authenticating…' : 'Reconnecting…'}
          </Badge>
        </Delayed>
        {/* Transport blips (the socket reconnects at once) and short transfers / pastes never flash a badge. */}
        <Delayed active={transportDown && info.hasOutput && !connecting}>
          <Badge tone="warning">
            <WifiOff className="size-3.5" />
            Connection to the server lost{info.transportAttempts > 1 ? ` (attempt ${info.transportAttempts})` : ''}
            <button type="button" className="ml-1 rounded-sm px-1 underline-offset-2 hover:underline" onClick={() => ctrl.retryTransport()}>
              Retry
            </button>
          </Badge>
        </Delayed>
        <Delayed active={ended && !endPrompt}>
          <Badge tone={info.state === 'closed' ? 'default' : 'danger'}>
            <PlugZap className="size-3.5" />
            {endedCopy(info).title}
            <button type="button" className="ml-1 rounded-sm px-1 underline-offset-2 hover:underline" onClick={() => ctrl.reconnect()}>
              {info.state === 'gone' || info.state === 'closed' ? 'New session' : 'Reconnect'}
            </button>
          </Badge>
        </Delayed>
        <Delayed active={!!info.inputLock}>
          <Badge>
            <Lock className="size-3.5" /> {lockReason} · other input is held back
          </Badge>
        </Delayed>
        {!info.readOnly && info.shadowedBy && (
          <Badge>
            <Eye className="size-3.5" /> {info.shadowedBy.length === 1 ? `${info.shadowedBy[0]} (administrator) is viewing this session` : `${info.shadowedBy.length} administrators are viewing this session`}
          </Badge>
        )}
        {info.readOnly && (
          <Badge>
            <Eye className="size-3.5" /> Read-only
          </Badge>
        )}
        {info.paused && (
          <Badge tone="warning">
            <Pause className="size-3.5" /> Output paused{info.pausedBytes ? ` · ${formatBytes(info.pausedBytes)} held` : ''}
            <button type="button" className="ml-1 flex items-center gap-1 rounded-sm px-1 underline-offset-2 hover:underline" onClick={() => ctrl.togglePause(false)}>
              <Play className="size-3" /> Resume
            </button>
          </Badge>
        )}
        <Delayed active={!!info.pasting}>
          <Badge>
            <Spinner immediate className="size-3.5" /> Pasting line <span className="tabular-nums">{pasting?.done}/{pasting?.total}</span>
            <button type="button" className="ml-1 rounded-sm px-1 underline-offset-2 hover:underline" onClick={() => ctrl.cancelPaste()}>
              Cancel
            </button>
          </Badge>
        </Delayed>
      </div>
    </>
  )
}
