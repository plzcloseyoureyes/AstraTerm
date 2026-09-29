/*
 * Per-terminal mini toolbar (CC-19), shown while hovering the terminal: find, split right / down, MultiExec, output
 * pause and session logging.
 */
import { FileText, PanelBottom, PanelRight, Pause, Search, Workflow } from 'lucide-react'
import { getKeybindings } from '@/app/commands'
import { formatKeybinding } from '@/lib/keys'
import { cn } from '@/lib/utils'
import type { TerminalController } from './controller'
import { isParticipant, toggleMultiExec, useMultiExecStore } from './multiexec'
import { duplicateSession } from './open'
import type { TerminalInfo } from './types'

function ToolButton({ label, icon: Icon, onClick, active, disabled }: { label: string; icon: typeof Search; onClick: () => void; active?: boolean; disabled?: boolean }) {
  return (
    <button
      type="button"
      title={label}
      aria-label={label}
      aria-pressed={active}
      disabled={disabled}
      onMouseDown={(e) => e.preventDefault()}
      onClick={onClick}
      className={cn(
        'flex size-6 items-center justify-center rounded-sm text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 disabled:opacity-40',
        active && 'bg-primary/15 text-primary hover:bg-primary/20 hover:text-primary',
      )}
    >
      <Icon className="size-3.5" />
    </button>
  )
}

function withKey(label: string, command: string): string {
  const b = getKeybindings(command)[0]
  return b ? `${label} (${formatKeybinding(b)})` : label
}

export function TerminalToolbar({ ctrl, info }: { ctrl: TerminalController; info: TerminalInfo }) {
  const multi = useMultiExecStore()
  const ended = info.state === 'closed' || info.state === 'disconnected' || info.state === 'error' || info.state === 'gone'
  return (
    <div
      role="toolbar"
      aria-label="Terminal actions"
      className="absolute top-1.5 right-4 z-20 flex items-center gap-0.5 rounded-md border bg-popover/90 p-0.5 opacity-0 shadow-popover backdrop-blur transition-opacity duration-150 group-hover/term:opacity-100 focus-within:opacity-100"
    >
      <ToolButton label={withKey('Find', 'terminal.find')} icon={Search} onClick={() => ctrl.openSearch()} />
      <ToolButton label="Split right" icon={PanelRight} onClick={() => void duplicateSession(ctrl.tabId, { position: 'right' })} />
      <ToolButton label="Split down" icon={PanelBottom} onClick={() => void duplicateSession(ctrl.tabId, { position: 'below' })} />
      <ToolButton label={multi.active ? 'Stop broadcast' : 'Broadcast input'} icon={Workflow} active={multi.active && isParticipant(ctrl.tabId, multi)} onClick={() => toggleMultiExec()} />
      <ToolButton label={info.paused ? 'Resume output' : withKey('Pause output', 'terminal.pauseOutput')} icon={Pause} active={info.paused} onClick={() => ctrl.togglePause()} />
      <ToolButton
        label={info.logging ? 'Stop session log' : 'Start session log'}
        icon={FileText}
        active={info.logging}
        disabled={ended}
        onClick={() => void ctrl.setLogging(!info.logging)}
      />
    </div>
  )
}
