/*
 * Serial line-status toolbar (PROTO-10, CC-18): a compact floating bar for the active serial session showing the
 * input modem lines (CTS/DSR/DCD/RI), toggling the output lines (DTR/RTS — disabled while hardware flow control drives
 * them), sending a BREAK and opening the hex monitor. Line status is polled from GET /api/sessions/:id/serial/status.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Binary, Zap } from 'lucide-react'
import { cn, errorMessage } from '@/lib/utils'
import { getTerminalsBySession } from '@/features/terminal/bus'
import { getSerialStatus, setSerialLines } from './api'
import { toggleHexMonitor, useProtocolsUI } from './store'
import type { SerialStatus } from './types'

const FLOW_LABEL: Record<string, string> = { rtscts: 'RTS/CTS', xonxoff: 'XON/XOFF', dsrdtr: 'DSR/DTR' }

function Led({ label, on, title }: { label: string; on: boolean; title: string }) {
  return (
    <span className="flex items-center gap-1" title={`${title}: ${on ? 'on' : 'off'}`}>
      <span aria-hidden className={cn('size-1.5 rounded-full', on ? 'bg-success shadow-[0_0_4px] shadow-success/70' : 'bg-muted-foreground/30')} />
      <span className={cn('text-[10px] font-medium tracking-wide', on ? 'text-foreground' : 'text-muted-foreground/70')}>
        {label}
        <span className="sr-only">{on ? ' on' : ' off'}</span>
      </span>
    </span>
  )
}

function LineToggle({ label, on, onClick, title, disabled }: { label: string; on: boolean; onClick: () => void; title: string; disabled?: boolean }) {
  return (
    <button
      type="button"
      title={title}
      aria-pressed={on}
      disabled={disabled}
      onClick={onClick}
      onMouseDown={(e) => e.preventDefault()}
      className={cn(
        'rounded px-1.5 py-0.5 text-[10px] font-semibold tracking-wide transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50',
        on ? 'bg-primary/20 text-primary hover:bg-primary/30' : 'bg-muted text-muted-foreground hover:bg-accent hover:text-foreground',
      )}
    >
      {label}
    </button>
  )
}

export function SerialToolbar({ sessionId }: { sessionId: string }) {
  const qc = useQueryClient()
  const hexSession = useProtocolsUI((s) => s.hexSession)
  const key = ['protocols', 'serial-status', sessionId]
  const status = useQuery<SerialStatus>({
    queryKey: key,
    queryFn: () => getSerialStatus(sessionId),
    refetchInterval: (q) => (q.state.error ? 5000 : 1000),
    retry: false,
  })

  const setLine = async (patch: { dtr?: boolean; rts?: boolean }) => {
    try {
      qc.setQueryData(key, await setSerialLines(sessionId, patch))
    } catch (err) {
      toast.error('Could not change the line', { description: errorMessage(err) })
    }
  }

  const st = status.data
  const flow = st?.flowControl ?? 'none'
  const sendBreak = () => getTerminalsBySession(sessionId)[0]?.sendBreak()

  return (
    <div
      role="toolbar"
      aria-label="Serial line status"
      className="pointer-events-auto flex items-center gap-2.5 rounded-full border bg-popover/95 px-3 py-1 text-xs text-popover-foreground shadow-popover backdrop-blur"
    >
      <span className="max-w-48 truncate font-medium text-muted-foreground" title={st?.device}>
        {st?.device ?? 'Serial'}
      </span>
      {status.isError ? (
        <span className="text-destructive">Line status unavailable</span>
      ) : (
        <>
          <div className="h-3.5 w-px bg-border" />
          <Led label="CTS" on={!!st?.cts} title="Clear To Send" />
          <Led label="DSR" on={!!st?.dsr} title="Data Set Ready" />
          <Led label="DCD" on={!!st?.dcd} title="Data Carrier Detect" />
          <Led label="RI" on={!!st?.ri} title="Ring Indicator" />
          <div className="h-3.5 w-px bg-border" />
          <LineToggle
            label="DTR"
            on={!!st?.dtr}
            disabled={!st || flow === 'dsrdtr'}
            title={flow === 'dsrdtr' ? 'DTR is driven by DSR/DTR flow control' : 'Toggle Data Terminal Ready'}
            onClick={() => setLine({ dtr: !st?.dtr })}
          />
          <LineToggle
            label="RTS"
            on={!!st?.rts}
            disabled={!st || flow === 'rtscts'}
            title={flow === 'rtscts' ? 'RTS is driven by RTS/CTS flow control' : 'Toggle Request To Send'}
            onClick={() => setLine({ rts: !st?.rts })}
          />
          {flow !== 'none' && (
            <span className="text-[10px] text-muted-foreground" title="Flow control">
              {FLOW_LABEL[flow] ?? flow}
            </span>
          )}
        </>
      )}
      <div className="h-3.5 w-px bg-border" />
      <button
        type="button"
        title="Send BREAK (250 ms)"
        onClick={sendBreak}
        onMouseDown={(e) => e.preventDefault()}
        className="flex items-center gap-1 rounded px-1.5 py-0.5 text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        <Zap className="size-3" aria-hidden /> Break
      </button>
      <button
        type="button"
        title="Toggle hex monitor"
        aria-pressed={hexSession === sessionId}
        onClick={() => toggleHexMonitor(sessionId)}
        onMouseDown={(e) => e.preventDefault()}
        className={cn(
          'flex items-center gap-1 rounded px-1.5 py-0.5 outline-none focus-visible:ring-2 focus-visible:ring-ring/50',
          hexSession === sessionId ? 'bg-primary/20 text-primary' : 'text-muted-foreground hover:bg-accent hover:text-foreground',
        )}
      >
        <Binary className="size-3" aria-hidden /> Hex
      </button>
    </div>
  )
}
