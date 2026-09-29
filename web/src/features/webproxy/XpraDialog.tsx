/* "Run X11 app via Xpra" dialog (PROTO-20): pick an SSH host, an application and seamless / desktop mode. */
import { useId, useMemo, useState, type FormEvent } from 'react'
import { AppWindow, CheckCircle2, Info, TriangleAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Spinner } from '@/components/ui/spinner'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage } from '@/lib/utils'
import { useXpraCheck } from './api'
import { openXpra } from './open'
import { RoutePicker, routeToSpec, specToRoute, type RouteValue } from './RoutePicker'
import type { XpraDialogInit } from './store'
import type { XpraMode } from './types'

const APPS = ['xterm', 'xeyes', 'xclock', 'firefox', 'gedit', 'code', 'nautilus', 'gnome-terminal']
const DESKTOPS = ['startxfce4', 'startlxde', 'mate-session', 'openbox-session']

export function XpraDialog({ init, onClose }: { init: XpraDialogInit; onClose: () => void }) {
  const ids = { route: useId(), command: useId() }
  const [route, setRoute] = useState<RouteValue>(init.sessionId || init.connectionId ? specToRoute(init) : '')
  const [command, setCommand] = useState(init.command ?? '')
  const [mode, setMode] = useState<XpraMode>(init.mode ?? 'seamless')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const target = useMemo(() => (route ? routeToSpec(route) : null), [route])
  const check = useXpraCheck(target)
  const checking = useDelayedFlag(check.isFetching)
  const showStarting = useDelayedFlag(busy)
  const ready = !!check.data?.installed && !!check.data?.html5
  const suggestions = mode === 'desktop' ? DESKTOPS : APPS

  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!target || !command.trim()) return
    setBusy(true)
    setError('')
    try {
      await openXpra({ ...target, command: command.trim(), mode })
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
              <AppWindow className="size-4 text-primary" /> Run X11 application
            </DialogTitle>
            <DialogDescription>
              Starts a graphical Linux application on an SSH host with Xpra and shows it in a tab — no local X server needed.
              Closing the tab stops the application.
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="grid gap-4">
            <Field label="SSH host" htmlFor={ids.route}>
              <RoutePicker id={ids.route} value={route} onChange={setRoute} allowDirect={false} />
            </Field>
            <div className="-mt-2 min-h-5 text-sm" aria-live="polite">
              {!target ? null : checking ? (
                <span className="flex items-center gap-1.5 text-muted-foreground">
                  <Spinner immediate className="size-3" /> Looking for Xpra on the host…
                </span>
              ) : check.isFetching ? null : check.isError ? (
                <span className="flex items-start gap-1.5 text-destructive">
                  <TriangleAlert className="mt-0.5 size-3.5 shrink-0" /> {errorMessage(check.error)}
                </span>
              ) : ready ? (
                <span className="flex items-center gap-1.5 text-success">
                  <CheckCircle2 className="size-3.5" /> Xpra {check.data?.version ? check.data.version.replace(/^v/, '') : ''} is available
                </span>
              ) : check.data ? (
                <span className="flex items-start gap-1.5 text-warning">
                  <Info className="mt-0.5 size-3.5 shrink-0" /> {check.data.message}
                </span>
              ) : null}
            </div>
            <Field label="Mode">
              <SegmentedControl<XpraMode>
                value={mode}
                onValueChange={setMode}
                aria-label="Mode"
                options={[
                  { value: 'seamless', label: 'Application windows' },
                  { value: 'desktop', label: 'Full desktop' },
                ]}
              />
            </Field>
            <Field
              label={mode === 'desktop' ? 'Desktop session command' : 'Application'}
              htmlFor={ids.command}
              error={error || undefined}
              hint="Runs as your user on the SSH host, e.g. a program name with arguments."
            >
              <Input
                id={ids.command}
                className="font-mono"
                placeholder={mode === 'desktop' ? 'startxfce4' : 'xterm'}
                spellCheck={false}
                autoComplete="off"
                value={command}
                autoFocus={!!init.sessionId || !!init.connectionId}
                onChange={(e) => {
                  setCommand(e.target.value)
                  setError('')
                }}
              />
            </Field>
            <div className="-mt-2 flex flex-wrap gap-1" aria-label="Suggestions">
              {suggestions.map((s) => (
                <button
                  key={s}
                  type="button"
                  className={cn(
                    'rounded-sm border px-1.5 py-0.5 font-mono text-xs text-muted-foreground transition-colors duration-150 hover:border-primary/50 hover:text-foreground',
                    command === s && 'border-primary/60 text-foreground',
                  )}
                  onClick={() => setCommand(s)}
                >
                  {s}
                </button>
              ))}
            </div>
            <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
              <Info className="mt-px size-3.5 shrink-0" />
              <span>
                Xpra listens on a loopback port of the host while the application runs. On a host shared with other users, they
                could connect to that port and see or control the application — use it on hosts only you (and admins you trust) can
                log in to.
              </span>
            </p>
          </DialogBody>
          <DialogFooter>
            {showStarting && <span className="mr-auto text-sm text-muted-foreground">Starting Xpra on the host…</span>}
            <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" loading={busy} disabled={!target || !command.trim() || (check.isSuccess && !ready)}>
              Start
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
