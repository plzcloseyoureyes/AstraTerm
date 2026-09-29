/*
 * BMC power control (CC-2) for a saved IPMI connection: shows the chassis power state and runs power on / off / soft
 * shutdown / cycle / reset through POST /api/ipmi/:connectionId/power. Destructive actions ask for an inline
 * confirmation (the panel is also embedded in the session editor, which lives in its own React root, so it does not
 * open a second modal).
 */
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { Power, PowerOff, RefreshCw, RotateCcw, TimerReset, Zap } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { LoadingState } from '@/components/ui/query-state'
import { BusyIcon, Delayed, Spinner } from '@/components/ui/spinner'
import { errorMessage } from '@/lib/utils'
import { ipmiPower, type IpmiPowerAction } from './api'
import { closeIpmiPower, useProtocolsUI } from './store'

type Action = Exclude<IpmiPowerAction, 'status'>

interface ActionDef {
  action: Action
  label: string
  icon: typeof Power
  /** Confirmation text for destructive actions. */
  confirm?: string
  /** Offered when the chassis is on / off. */
  when: 'on' | 'off'
}

const ACTIONS: ActionDef[] = [
  { action: 'on', label: 'Power on', icon: Power, when: 'off' },
  { action: 'soft', label: 'Soft shutdown', icon: TimerReset, when: 'on', confirm: 'Ask the operating system to shut down?' },
  { action: 'off', label: 'Power off', icon: PowerOff, when: 'on', confirm: 'Cut power now? Unsaved data is lost.' },
  { action: 'cycle', label: 'Power cycle', icon: RotateCcw, when: 'on', confirm: 'Turn the server off and on again?' },
  { action: 'reset', label: 'Hard reset', icon: Zap, when: 'on', confirm: 'Reset the server now? Unsaved data is lost.' },
]

/** Power state + actions for one saved IPMI connection. */
export function IpmiPowerPanel({ connectionId }: { connectionId: string }) {
  const qc = useQueryClient()
  const key = ['protocols', 'ipmi-power', connectionId]
  const status = useQuery({ queryKey: key, queryFn: () => ipmiPower(connectionId, 'status'), retry: false, staleTime: 0 })
  const [busy, setBusy] = useState<Action | null>(null)
  const [arming, setArming] = useState<Action | null>(null)

  const run = async (def: ActionDef) => {
    if (def.confirm && arming !== def.action) {
      setArming(def.action)
      return
    }
    setArming(null)
    setBusy(def.action)
    try {
      const res = await ipmiPower(connectionId, def.action)
      qc.setQueryData(key, { action: 'status', powerOn: res.powerOn })
      toast.success(`${def.label} sent to the BMC`, { description: `Chassis power is now ${res.powerOn ? 'on' : 'off'}.` })
    } catch (err) {
      toast.error(`${def.label} failed`, { description: errorMessage(err) })
    } finally {
      setBusy(null)
    }
  }

  const powerOn = status.data?.powerOn
  const armed = ACTIONS.find((a) => a.action === arming)
  return (
    <div className="grid gap-3">
      <div className="flex items-center gap-2" aria-live="polite">
        <span className="text-muted-foreground">Power state</span>
        <LoadingState busy={status.isLoading} skeleton={<Spinner label="Reading the power state" />}>
          {status.isError ? (
          <Badge variant="destructive">Unavailable</Badge>
        ) : (
          <Badge variant={powerOn ? 'success' : 'secondary'}>{powerOn ? 'On' : 'Off'}</Badge>
        )}
        </LoadingState>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="ml-auto"
          aria-label="Refresh the power state"
          disabled={status.isFetching}
          onClick={() => void status.refetch()}
        >
          <BusyIcon icon={RefreshCw} busy={status.isFetching} />
        </Button>
      </div>
      {status.isError && <p className="text-sm text-destructive">{errorMessage(status.error)}</p>}
      <div className="grid grid-cols-2 gap-2">
        {ACTIONS.map((def) => {
          // Until the state is known every action is offered; afterwards only those that make sense.
          const irrelevant = status.data !== undefined && (def.when === 'on') !== !!powerOn
          const isArmed = arming === def.action
          return (
            <Button
              key={def.action}
              type="button"
              variant={isArmed ? 'destructive' : def.confirm ? 'outline' : 'default'}
              size="sm"
              disabled={busy !== null || irrelevant}
              onClick={() => void run(def)}
              className="justify-start"
            >
              <Delayed active={busy === def.action} fallback={<def.icon />}>
                <Spinner />
              </Delayed> {isArmed ? `Confirm: ${def.label.toLowerCase()}` : def.label}
            </Button>
          )
        })}
      </div>
      {armed && (
        <p role="alert" className="flex items-center gap-2 text-sm text-warning">
          {armed.confirm} Click “Confirm” to proceed.
          <Button type="button" variant="link" size="sm" onClick={() => setArming(null)}>
            Cancel
          </Button>
        </p>
      )}
    </div>
  )
}

export function IpmiPowerDialog() {
  const target = useProtocolsUI((s) => s.ipmiPower)
  if (!target) return null
  return (
    <Dialog open onOpenChange={(o) => !o && closeIpmiPower()}>
      <DialogContent size="sm">
        <DialogHeader>
          <DialogTitle>BMC power</DialogTitle>
          <DialogDescription>Chassis power control of {target.name} over IPMI.</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <IpmiPowerPanel key={target.connectionId} connectionId={target.connectionId} />
        </DialogBody>
        <DialogFooter>
          <Button variant="outline" onClick={closeIpmiPower}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
