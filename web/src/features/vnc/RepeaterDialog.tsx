/*
 * Connect through an UltraVNC repeater (GFX-17): quick-connects to the repeater with options.repeaterId; NexTerm sends
 * "ID:<id>" when the repeater greets with RFB 000.000, then authenticates with the real server behind it.
 */
import { useState } from 'react'
import { Waypoints } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { openQuick } from '@/features/terminal/open'
import { openRepeater, useVncStore } from './store'

export function RepeaterDialog({ locked }: { locked: boolean }) {
  const open = useVncStore((s) => s.repeaterOpen) && !locked
  return (
    <Dialog open={open} onOpenChange={(o) => openRepeater(o)}>
      <DialogContent size="md">{open && <RepeaterForm />}</DialogContent>
    </Dialog>
  )
}

const ID_RE = /^[A-Za-z0-9:_.-]{1,240}$/

function RepeaterForm() {
  const [host, setHost] = useState('')
  const [port, setPort] = useState<number | null>(5901)
  const [id, setId] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const hostOk = /^[^\s/\\]+$/.test(host.trim())
  const idOk = ID_RE.test(id.trim())
  const submit = async () => {
    if (!hostOk || !idOk || !port) return
    setBusy(true)
    const tab = await openQuick({
      protocol: 'vnc',
      host: host.trim(),
      port,
      name: `${id.trim()} via ${host.trim()}`,
      ...(password ? { password } : {}),
      options: { repeaterId: id.trim() },
    })
    setBusy(false)
    if (tab) openRepeater(false)
  }
  return (
    <form
      className="contents"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <DialogHeader>
        <DialogTitle>
          <Waypoints className="size-4.5 text-primary" />
          Connect through a VNC repeater
        </DialogTitle>
        <DialogDescription>
          For servers behind NAT that register with an UltraVNC repeater (mode II). NexTerm asks the repeater for the
          server with the given ID.
        </DialogDescription>
      </DialogHeader>
      <div className="grid gap-3">
        <div className="grid grid-cols-[1fr_7rem] gap-3">
          <Field label="Repeater host" error={host && !hostOk ? 'Enter a host name or IP address' : undefined}>
            <Input value={host} onChange={(e) => setHost(e.target.value)} placeholder="repeater.example.com" autoFocus />
          </Field>
          <Field label="Viewer port">
            <NumberInput value={port} onChange={setPort} min={1} max={65535} />
          </Field>
        </div>
        <Field label="Repeater ID" hint="The ID the server registered with (e.g. 1234)" error={id && !idOk ? 'Letters, digits and :_.- only' : undefined}>
          <Input value={id} onChange={(e) => setId(e.target.value)} placeholder="1234" />
        </Field>
        <Field label="VNC password" hint="Optional — asked for when needed and not stored">
          <PasswordInput value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="off" />
        </Field>
      </div>
      <DialogFooter>
        <Button type="button" variant="secondary" onClick={() => openRepeater(false)}>
          Cancel
        </Button>
        <Button type="submit" loading={busy} disabled={!hostOk || !idOk || !port}>
          Connect
        </Button>
      </DialogFooter>
    </form>
  )
}
