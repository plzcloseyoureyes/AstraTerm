/*
 * "Run this command?" — the mandatory confirmation before an AI suggestion is executed. Shows the exact command, the
 * target terminal, the model's risk rating and the dangerous-command guard's verdict. Confirming while the guard is
 * still checking waits for it; newly found dangers keep the dialog open for a second look.
 */
import { useEffect, useRef, useState } from 'react'
import { AlertTriangle, Play, ShieldAlert, SquareTerminal } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Kbd } from '@/components/ui/kbd'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { RiskBadge } from './CommandBar'
import { finishRun, useRunStore } from './run'

export function RunDialog({ locked }: { locked: boolean }) {
  const req = useRunStore((s) => s.req)
  const guard = useRunStore((s) => s.guard)
  const checking = useDelayedFlag(guard.loading)
  const danger = guard.matches.some((m) => m.severity === 'danger')
  const risky = danger || req?.risk === 'high'
  const runRef = useRef<HTMLButtonElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const [waiting, setWaiting] = useState(false)

  useEffect(() => setWaiting(false), [req])
  // A confirmation given while the guard was still checking runs once it is done — unless it found a danger.
  useEffect(() => {
    if (!waiting || guard.loading) return
    setWaiting(false)
    if (!guard.matches.some((m) => m.severity === 'danger')) finishRun(true)
  }, [waiting, guard])

  const confirmRun = () => {
    if (guard.loading) setWaiting(true)
    else finishRun(true)
  }

  return (
    <Dialog open={!!req && !locked} onOpenChange={(o) => !o && finishRun(false)}>
      <DialogContent
        size="lg"
        onOpenAutoFocus={(e) => {
          e.preventDefault()
          ;(req?.risk === 'high' ? cancelRef : runRef).current?.focus()
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault()
            confirmRun()
          }
        }}
      >
        <DialogHeader>
          <DialogTitle>Run this command?</DialogTitle>
          <DialogDescription className="flex items-center gap-1.5">
            <SquareTerminal className="size-3.5" /> It will be sent with Enter to
            <span className="font-medium text-foreground">{req?.title}</span>
          </DialogDescription>
        </DialogHeader>
        <pre
          className={cn(
            'max-h-64 overflow-auto rounded-md border bg-muted/40 px-3 py-2 font-mono text-sm leading-relaxed break-all whitespace-pre-wrap transition-colors duration-150',
            risky && 'border-destructive/40',
          )}
        >
          {req?.command}
        </pre>
        {(req?.risk || guard.matches.length > 0 || checking) && (
          <div className="grid gap-2 text-sm">
            {req?.risk && (
              <div className="flex items-start gap-2">
                <RiskBadge risk={req.risk} />
                {req.riskReason && <span className="text-muted-foreground">{req.riskReason}</span>}
              </div>
            )}
            {checking && <div className="text-muted-foreground">Checking against the dangerous-command rules…</div>}
            {guard.matches.map((m, i) => (
              <div
                key={i}
                className={cn(
                  'flex items-start gap-2 rounded-md px-2.5 py-1.5 animate-in fade-in-0 duration-150',
                  m.severity === 'danger' ? 'bg-destructive/10 text-destructive' : 'bg-warning/12 text-warning',
                )}
              >
                {m.severity === 'danger' ? <ShieldAlert className="mt-0.5 size-3.5 shrink-0" /> : <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />}
                <span>
                  {m.message || m.rule}
                  {req?.command.includes('\n') && typeof m.line === 'number' ? <span className="opacity-75"> (line {m.line})</span> : null}
                </span>
              </div>
            ))}
          </div>
        )}
        <DialogFooter className="items-center">
          <span className="mr-auto hidden items-center gap-1 text-xs text-muted-foreground sm:flex">
            <Kbd keys="$mod+Enter" /> to run
          </span>
          <Button ref={cancelRef} variant="ghost" onClick={() => finishRun(false)}>
            Cancel
          </Button>
          <Button ref={runRef} variant={risky ? 'destructive' : 'default'} loading={waiting} onClick={confirmRun}>
            <Play /> {risky ? 'Run anyway' : 'Run'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
