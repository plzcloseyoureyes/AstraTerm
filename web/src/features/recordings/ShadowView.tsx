/*
 * Tab kind "shadow" {sessionId}: an administrator's read-only live view of another user's terminal session (MU-19),
 * through the terminal WebSocket's audited read-only admin attach. Closing this tab never ends the session.
 */
import { useCallback, useRef, useState } from 'react'
import { Copy, Eye, MessageSquare, Minus, Plus, Power, RefreshCw, ScanLine } from 'lucide-react'
import { toast } from 'sonner'
import { wsUrl } from '@/api/client'
import type { TabProps } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { IconButton } from '@/components/ui/icon-button'
import { Delayed, Spinner } from '@/components/ui/spinner'
import { StatusDot } from '@/components/ui/status-dot'
import { closeTab } from '@/stores/workspace'
import { terminateAction } from './actions'
import { openMessageDialog } from './store'
import type { ShadowTabParams } from './types'
import { ViewerTerminal, type ViewerStatus, type ViewerTerminalHandle } from './ViewerTerminal'

export default function ShadowView({ tabId, params }: TabProps<ShadowTabParams>) {
  const [status, setStatus] = useState<ViewerStatus | null>(null)
  const [fit, setFit] = useState<'fit' | 'actual'>('fit')
  const [font, setFont] = useState(13)
  const term = useRef<ViewerTerminalHandle>(null)
  const url = useCallback((offset: number) => wsUrl(`/ws/terminal/${encodeURIComponent(params.sessionId)}`, { offset }), [params.sessionId])
  const ended = !!status?.ended || status?.state === 'closed'
  const live = !ended && status?.transport === 'open' && status.state === 'connected'
  const attaching = !ended && (!status || status.transport !== 'open')

  return (
    <div className="flex h-full min-h-0 flex-col bg-background">
      <div className="flex h-9 shrink-0 items-center gap-1.5 border-b bg-toolbar px-2">
        <Badge variant="info">
          <Eye /> Shadowing
        </Badge>
        <StatusDot tone={live ? 'success' : ended ? 'muted' : 'warning'} pending={!live && !ended} className="size-2" />
        <div className="min-w-0 flex-1 truncate text-sm">
          <span className="font-medium">{params.title || 'Session'}</span>
          {params.owner && <span className="text-muted-foreground"> · {params.owner}</span>}
          <span className="text-muted-foreground tabular-nums">
            {' '}
            · {status?.cols ?? '–'}×{status?.rows ?? '–'} · {ended ? 'ended' : (status?.state ?? 'connecting')}
          </span>
        </div>
        <IconButton icon={ScanLine} label={fit === 'fit' ? 'Actual size' : 'Fit to pane'} active={fit === 'fit'} onClick={() => setFit((f) => (f === 'fit' ? 'actual' : 'fit'))} />
        <IconButton icon={Minus} label="Smaller text" onClick={() => setFont((f) => Math.max(8, f - 1))} />
        <IconButton icon={Plus} label="Larger text" onClick={() => setFont((f) => Math.min(28, f + 1))} />
        <IconButton
          icon={Copy}
          label="Copy selection"
          onClick={async () => {
            if (await term.current?.copySelection()) toast.success('Copied')
          }}
        />
        <IconButton icon={MessageSquare} label="Message the user" disabled={ended} onClick={() => openMessageDialog(params.sessionId, params.title)} />
        <Button
          size="sm"
          variant="ghost"
          className="text-destructive"
          disabled={ended}
          onClick={() => void terminateAction({ id: params.sessionId, title: params.title ?? 'session', owner: params.owner })}
        >
          <Power /> Terminate
        </Button>
      </div>
      <div className="relative min-h-0 flex-1">
        <ViewerTerminal ref={term} url={url} fit={fit} fontSize={font} onStatus={setStatus} className="absolute inset-0 overflow-auto" />
        <Delayed active={attaching}>
          <div className="pointer-events-none absolute top-3 left-1/2 flex -translate-x-1/2 items-center gap-2 rounded-md border bg-popover/95 px-3 py-1.5 text-sm shadow-popover">
            <Spinner className="size-3.5" /> {status?.transport === 'reconnecting' ? 'Reconnecting…' : 'Attaching…'}
          </div>
        </Delayed>
        {ended && (
          <div className="absolute inset-x-0 bottom-0 flex items-center justify-center gap-3 border-t bg-popover/95 px-3 py-2 text-sm animate-in fade-in-0 slide-in-from-bottom-1">
            The session has ended.
            <Button size="xs" variant="secondary" onClick={() => term.current?.reconnect()}>
              <RefreshCw /> Retry
            </Button>
            <Button size="xs" variant="ghost" onClick={() => void closeTab(tabId)}>
              Close view
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}
