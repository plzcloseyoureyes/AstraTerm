/* "Open web page" dialog: an address and the route to reach it (direct, SSH session, saved SSH connection, tunnel). */
import { useId, useMemo, useState, type FormEvent } from 'react'
import { Clock, Globe } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { errorMessage } from '@/lib/utils'
import { createProxy } from './api'
import { addressToString, parseAddress } from './model'
import { openProxyTab } from './open'
import { RoutePicker, routeToSpec, specToRoute, type RouteValue } from './RoutePicker'
import { rememberAddress, webViewSettings } from './settings'
import type { OpenArgs, ProxySpec } from './types'

function initialAddress(a: OpenArgs): string {
  if (a.url) return a.url
  if (a.host || a.port) {
    const scheme = a.scheme ?? (a.port === 443 || a.port === 8443 ? 'https' : 'http')
    const host = a.host || 'localhost'
    const h = host.includes(':') ? `[${host}]` : host
    return `${scheme}://${h}${a.port ? `:${a.port}` : ''}${a.path ?? '/'}`
  }
  return a.sessionId || a.connectionId || a.tunnelId ? 'http://localhost:' : ''
}

export function OpenDialog({ init, onClose }: { init: OpenArgs; onClose: () => void }) {
  const ids = { route: useId(), address: useId() }
  const [route, setRoute] = useState<RouteValue>(specToRoute(init))
  const [address, setAddress] = useState(initialAddress(init))
  const [insecure, setInsecure] = useState(!!init.insecureTls)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const recent = webViewSettings.useValue('recent')
  const parsed = useMemo(() => parseAddress(address), [address])
  const suggestions = useMemo(() => {
    const q = address.trim().toLowerCase()
    return (recent ?? []).filter((r) => !q || r.toLowerCase().includes(q.replace(/^https?:\/\//, ''))).slice(0, 5)
  }, [recent, address])

  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!parsed) {
      setError('Enter an http or https address, such as http://localhost:8080/ or grafana.lan:3000')
      return
    }
    const spec: ProxySpec = { ...routeToSpec(route), url: addressToString(parsed) }
    if (insecure && parsed.scheme === 'https') spec.insecureTls = true
    if (init.title) spec.title = init.title
    setBusy(true)
    setError('')
    try {
      const info = await createProxy(spec)
      rememberAddress(addressToString(parsed))
      openProxyTab(info, spec, { position: init.position })
      onClose()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent size="md">
        <form onSubmit={submit} className="contents">
          <DialogHeader>
            <DialogTitle>
              <Globe className="size-4 text-primary" /> Open web page
            </DialogTitle>
            <DialogDescription>
              Termstead fetches the page itself and shows it in a tab — from this Termstead host, or from an SSH server for services only
              it can reach (Jupyter, Grafana, router and BMC pages).
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="grid gap-4">
            <Field label="Address" htmlFor={ids.address} error={error || undefined}>
              <Input
                id={ids.address}
                autoFocus
                className="font-mono"
                placeholder="http://localhost:8080/"
                spellCheck={false}
                autoComplete="off"
                value={address}
                onChange={(e) => {
                  setAddress(e.target.value)
                  setError('')
                }}
                onFocus={(e) => {
                  const el = e.currentTarget
                  requestAnimationFrame(() => el.setSelectionRange(el.value.length, el.value.length))
                }}
              />
            </Field>
            {suggestions.length > 0 && (
              <div className="-mt-2 flex flex-wrap gap-1" aria-label="Recent addresses">
                {suggestions.map((r) => (
                  <button
                    key={r}
                    type="button"
                    className="flex max-w-full items-center gap-1 truncate rounded-sm border px-1.5 py-0.5 font-mono text-xs text-muted-foreground transition-colors duration-150 hover:border-primary/50 hover:text-foreground"
                    onClick={() => setAddress(r)}
                  >
                    <Clock className="size-3 shrink-0" />
                    <span className="truncate">{r}</span>
                  </button>
                ))}
              </div>
            )}
            <Field label="Reach it through" htmlFor={ids.route} hint={route === 'direct' ? 'The Termstead host connects to the address itself.' : 'The SSH server connects to the address ("localhost" is the SSH server).'}>
              <RoutePicker id={ids.route} value={route} onChange={setRoute} tunnelLabel="The tunnel's SSH connection" />
            </Field>
            {parsed?.scheme === 'https' && (
              <CheckboxField
                checked={insecure}
                onCheckedChange={(v) => setInsecure(v === true)}
                label="Accept an untrusted certificate"
                description="For appliances with self-signed certificates. The connection stays encrypted but the site is not verified."
              />
            )}
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" loading={busy} disabled={!address.trim()}>
              Open
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
