/*
 * Listening mode (GFX-17): accept incoming connections from VNC servers ("reverse" / "connect to viewer": x11vnc
 * -connect, UltraVNC SC, TightVNC "Attach listening viewer"). Each connection becomes a VNC session that opens in a
 * new tab (Settings → VNC → open incoming connections).
 */
import { useState } from 'react'
import { Ear, Radio, Square } from 'lucide-react'
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { LoadingState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { SwitchField } from '@/components/ui/switch'
import { errorMessage, formatRelativeTime } from '@/lib/utils'
import { useNow } from '@/lib/hooks'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { useStartListener, useStopListener, useVncListeners } from './api'
import { openListeners, useVncStore } from './store'
import { vncSettings } from './settings'

type BindChoice = 'all' | 'loopback' | 'custom'

export function ListenersDialog({ locked }: { locked: boolean }) {
  const open = useVncStore((s) => s.listenersOpen) && !locked
  return (
    <Dialog open={open} onOpenChange={(o) => openListeners(o)}>
      <DialogContent size="lg">{open && <ListenersBody />}</DialogContent>
    </Dialog>
  )
}

function ListenersBody() {
  const mode = useRunMode()
  const admin = useIsAdmin()
  const allowed = mode === 'desktop' || admin
  const list = useVncListeners(allowed)
  const start = useStartListener()
  const stop = useStopListener()
  const settings = vncSettings.use()
  useNow(15_000)
  const [port, setPort] = useState<number | null>(5500)
  const [bind, setBind] = useState<BindChoice>('all')
  const [custom, setCustom] = useState('')
  const [password, setPassword] = useState('')
  const [viewOnly, setViewOnly] = useState(false)
  const bindHost = bind === 'all' ? '' : bind === 'loopback' ? '127.0.0.1' : custom.trim()

  const submit = async () => {
    if (!port) return
    try {
      const l = await start.mutateAsync({ port, bindHost, password: password || undefined, viewOnly })
      toast.success(`Listening on ${l.address}`)
      setPassword('')
    } catch (err) {
      toast.error('Could not start listening', { description: isApiError(err) ? err.message : errorMessage(err) })
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          <Ear className="size-4.5 text-primary" />
          Incoming VNC connections
        </DialogTitle>
        <DialogDescription>
          Let VNC servers connect to Termstead (reverse connection), e.g. <code className="font-mono">x11vnc -connect host:5500</code>,
          UltraVNC SC or TightVNC &ldquo;Attach listening viewer&rdquo;. Each connection opens as a new VNC session.
        </DialogDescription>
      </DialogHeader>
      <DialogBody className="grid gap-4">
        {!allowed ? (
          <p className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm">
            In server mode only administrators can listen for incoming connections (they open a port on the Termstead
            host).
          </p>
        ) : (
          <form
            className="grid gap-3 rounded-md border p-3"
            onSubmit={(e) => {
              e.preventDefault()
              void submit()
            }}
          >
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-[8rem_1fr]">
              <Field label="Port" hint="Default 5500">
                <NumberInput value={port} onChange={setPort} min={1} max={65535} inputSize="sm" />
              </Field>
              <Field label="Listen on">
                <SimpleSelect
                  size="sm"
                  value={bind}
                  onValueChange={setBind}
                  options={[
                    { value: 'all', label: 'All interfaces (reachable from the network)' },
                    { value: 'loopback', label: 'This computer only (127.0.0.1)' },
                    { value: 'custom', label: 'Specific address…' },
                  ]}
                />
              </Field>
            </div>
            {bind === 'custom' && (
              <Field label="Address" hint="An IP address of this host">
                <Input inputSize="sm" value={custom} onChange={(e) => setCustom(e.target.value)} placeholder="192.168.1.10" />
              </Field>
            )}
            <Field label="VNC password" hint="Used when an incoming server asks for VNC authentication (kept in memory only)">
              <PasswordInput inputSize="sm" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
            </Field>
            <SwitchField label="View only" description="Do not send keyboard or mouse input by default" checked={viewOnly} onCheckedChange={setViewOnly} />
            <SwitchField
              label="Open incoming connections automatically"
              description="Otherwise a notification offers to open them"
              checked={settings.openIncoming}
              onCheckedChange={(v) => vncSettings.set({ openIncoming: v })}
            />
            <div className="flex justify-end">
              <Button type="submit" size="sm" loading={start.isPending} disabled={!port || (bind === 'custom' && !custom.trim())}>
                <Radio /> Start listening
              </Button>
            </div>
          </form>
        )}

        {allowed && (
          <section className="grid gap-2">
            <h3 className="text-sm font-medium text-muted-foreground">Active listeners</h3>
            <LoadingState busy={list.isLoading} skeleton={<div className="flex justify-center py-4">
                <Spinner />
              </div>}>
              {list.isError ? (
              <p className="text-sm text-destructive">{errorMessage(list.error)}</p>
            ) : !list.data?.length ? (
              <EmptyState size="sm" icon={Ear} title="Not listening" description="Start a listener to accept reverse connections." />
            ) : (
              <ul className="divide-y rounded-md border">
                {list.data.map((l) => (
                  <li key={l.id} className="flex items-center gap-3 px-3 py-2">
                    <span className="size-2 shrink-0 rounded-full bg-success" aria-hidden />
                    <div className="min-w-0 flex-1">
                      <div className="font-mono text-sm">{l.address}</div>
                      <div className="text-xs text-muted-foreground">
                        {l.accepted} connection{l.accepted === 1 ? '' : 's'}
                        {l.pending > 0 && ` · ${l.pending} waiting`}
                        {l.lastFrom && l.lastAt && ` · last from ${l.lastFrom} ${formatRelativeTime(l.lastAt)}`}
                        {l.hasPassword && ' · password set'}
                        {l.viewOnly && ' · view only'}
                      </div>
                    </div>
                    <Button
                      size="xs"
                      variant="secondary"
                      loading={stop.isPending && stop.variables === l.id}
                      onClick={() =>
                        stop.mutate(l.id, {
                          onError: (err) => toast.error('Could not stop the listener', { description: errorMessage(err) }),
                        })
                      }
                    >
                      <Square /> Stop
                    </Button>
                  </li>
                ))}
              </ul>
            )}
            </LoadingState>
          </section>
        )}
      </DialogBody>
      <DialogFooter>
        <Button variant="secondary" onClick={() => openListeners(false)}>
          Close
        </Button>
      </DialogFooter>
    </>
  )
}
