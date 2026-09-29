/*
 * Status bar item: visible while the user has active share links — "Shared · N watching" — so a shared terminal is
 * never forgotten. Click opens the share dialog of the (first) shared session.
 */
import { Share2 } from 'lucide-react'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, plural } from '@/lib/utils'
import { useMyShares } from './api'
import { openShareDialog } from './store'

export function SharesStatusItem() {
  const { data } = useMyShares()
  if (!data?.length) return null
  const viewers = data.reduce((n, s) => n + s.viewers.length, 0)
  const sessions = new Map(data.map((s) => [s.sessionId, s.sessionTitle ?? 'session']))
  const [firstId, firstTitle] = [...sessions.entries()][0]
  const interactive = data.some((s) => s.mode === 'write')
  return (
    <Tooltip
      content={`${plural(data.length, 'active share link')} on ${[...sessions.values()].join(', ')}${viewers ? ` · ${plural(viewers, 'viewer')} connected` : ''}`}
    >
      <button
        type="button"
        onClick={() => openShareDialog(firstId, firstTitle)}
        className={cn(
          'flex h-full items-center gap-1 px-2 text-xs transition-colors hover:bg-accent',
          interactive ? 'text-warning' : 'text-muted-foreground',
        )}
      >
        <Share2 className="size-3.5" />
        <span className="tabular-nums">{viewers ? `${viewers} watching` : 'Shared'}</span>
      </button>
    </Tooltip>
  )
}
