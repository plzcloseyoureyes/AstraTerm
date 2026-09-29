/*
 * Status bar item shown while the built-in SSH agent runs for the current user: key count, lock state; click opens
 * the Agent tab.
 */
import { KeyRound, Lock } from 'lucide-react'
import { Tooltip } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { useRunMode } from '@/stores/auth'
import { openKeysTab } from './actions'
import { useAgentStatus } from './api'

export function AgentStatusItem() {
  const desktop = useRunMode() === 'desktop'
  const { data } = useAgentStatus(desktop ? 30_000 : false)
  if (!desktop || !data?.running || !data.owner) return null
  const Icon = data.locked ? Lock : KeyRound
  return (
    <Tooltip content={`SSH agent · ${data.keyCount} key${data.keyCount === 1 ? '' : 's'}${data.locked ? ' · locked' : ''}\n${data.socketPath ?? ''}`} side="top">
      <button
        type="button"
        onClick={() => openKeysTab('agent')}
        className={cn('flex h-full items-center gap-1 px-1.5 text-xs text-muted-foreground hover:bg-accent hover:text-foreground', data.locked && 'text-warning')}
        aria-label="SSH agent status"
      >
        <Icon className="size-3.5" />
        <span className="tabular-nums">Agent · {data.keyCount}</span>
      </button>
    </Tooltip>
  )
}
