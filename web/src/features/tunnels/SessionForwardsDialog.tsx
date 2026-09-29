/*
 * Session port forwards (TUN-7): the forwards stored in an SSH connection's options.forwards start whenever a
 * terminal session to it connects (after every reconnect too) and stop when it closes — like `ssh -L/-R/-D` flags.
 * This dialog edits that list and shows the live forwards of the connection's open sessions.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { toast } from 'sonner'
import { Cable, Plus, ShieldAlert, Trash2 } from 'lucide-react'
import { getConnection, updateConnection, useConnection } from '@/api/connections'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection } from '@/api/types'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { cn, errorMessage } from '@/lib/utils'
import { useSessionForwards } from './api'
import { StatusDot } from './components'
import { KINDS, KIND_ORDER, forwardLabel, isLoopbackHost, isProxyKind, kindOf, typeOfKind, validBindHost, validHost } from './model'
import type { ForwardSpec, TunnelKind } from './types'

interface Row {
  key: number
  kind: TunnelKind
  bindHost: string
  bindPort: number | null
  destHost: string
  destPort: number | null
  enabled: boolean
  name: string
  /** Fields this editor does not expose (sockets) are kept as they are. */
  extra: Pick<ForwardSpec, 'bindSocket' | 'destSocket'>
}

let seq = 0

function rowsFrom(conn: Connection | undefined): Row[] {
  const list = conn?.options?.forwards
  if (!Array.isArray(list)) return []
  return list
    .filter((f): f is ForwardSpec => !!f && typeof f === 'object' && typeof (f as ForwardSpec).type === 'string')
    .map((f) => ({
      key: ++seq,
      kind: kindOf({ type: f.type, reverse: f.reverse }),
      bindHost: f.bindHost ?? '',
      bindPort: typeof f.bindPort === 'number' && f.bindPort > 0 ? f.bindPort : null,
      destHost: f.destHost ?? '',
      destPort: typeof f.destPort === 'number' ? f.destPort : null,
      enabled: !f.disabled,
      name: f.name ?? '',
      extra: { bindSocket: f.bindSocket, destSocket: f.destSocket },
    }))
}

function toSpec(r: Row): ForwardSpec {
  const { type, reverse } = typeOfKind(r.kind)
  const proxy = isProxyKind(r.kind)
  const spec: ForwardSpec = { type }
  if (reverse) spec.reverse = true
  if (r.extra.bindSocket) spec.bindSocket = r.extra.bindSocket
  else {
    if (r.bindHost.trim()) spec.bindHost = r.bindHost.trim()
    spec.bindPort = r.bindPort ?? 0
  }
  if (!proxy) {
    if (r.extra.destSocket) spec.destSocket = r.extra.destSocket
    else {
      spec.destHost = r.destHost.trim() || 'localhost'
      spec.destPort = r.destPort ?? 0
    }
  }
  if (r.name.trim()) spec.name = r.name.trim()
  if (!r.enabled) spec.disabled = true
  return spec
}

function rowError(r: Row): string | null {
  const here = KINDS[r.kind].listenOn === 'host'
  if (!r.extra.bindSocket && !validBindHost(r.bindHost, !here)) {
    return here ? 'Listen address: an IP address of this machine, localhost or *' : 'Invalid listen address'
  }
  if (r.bindPort != null && (r.bindPort < 0 || r.bindPort > 65535)) return 'Listen port 0–65535'
  if (isProxyKind(r.kind) && rowExposed(r)) {
    // Session forwards carry no proxy password, and an open proxy is refused (a saved tunnel can have one).
    return 'A session SOCKS proxy must listen on a loopback address — use a saved tunnel with a password for network access'
  }
  if (!isProxyKind(r.kind) && !r.extra.destSocket) {
    if (r.destHost.trim() && !validHost(r.destHost)) return 'Invalid destination host'
    if (!r.destPort || r.destPort < 1 || r.destPort > 65535) return 'Destination port required'
  }
  return null
}

/** The row's listener accepts connections from other machines (a TCP listener on a non-loopback address). */
function rowExposed(r: Row): boolean {
  return !r.extra.bindSocket && !isLoopbackHost(r.bindHost)
}

const exposureKey = (r: Row) => `${KINDS[r.kind].listenOn}|${r.bindHost.trim()}|${r.bindPort ?? 0}`

export default function SessionForwardsDialog({ connectionId, onClose }: { connectionId: string; onClose: () => void }) {
  const conn = useConnection(connectionId)
  const live = useSessionForwards()
  const [rows, setRows] = useState<Row[] | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [showErrors, setShowErrors] = useState(false)
  // Listeners that were already reachable from other machines when the dialog opened (no new opt-in needed).
  const exposedBefore = useRef<Set<string>>(new Set())

  useEffect(() => {
    if (rows === null && conn.data) {
      const initial = rowsFrom(conn.data)
      exposedBefore.current = new Set(initial.filter((r) => r.enabled && rowExposed(r)).map(exposureKey))
      setRows(initial)
    }
  }, [conn.data, rows])

  const liveForConn = useMemo(() => (live.data ?? []).filter((f) => f.connectionId === connectionId && f.source === 'connection'), [live.data, connectionId])
  const errors = (rows ?? []).map(rowError)
  const hasErrors = errors.some(Boolean)
  const update = (key: number, patch: Partial<Row>) => setRows((rs) => rs?.map((r) => (r.key === key ? { ...r, ...patch } : r)) ?? rs)

  const exposed = (rows ?? []).filter((r) => r.enabled && rowExposed(r))

  const save = async () => {
    setShowErrors(true)
    if (!rows || hasErrors) return
    // Network exposure is an explicit opt-in (these forwards open automatically with every session).
    const added = exposed.filter((r) => !exposedBefore.current.has(exposureKey(r)))
    if (
      added.length &&
      !(await confirm({
        title: 'Make session forwards reachable from other machines?',
        description: `${added.map((r) => forwardLabel(toSpec(r))).join(', ')} will accept connections from other machines (no password is asked) whenever a session to ${conn.data?.name ?? 'this server'} is open. Keep a loopback address (127.0.0.1) unless other machines must use them.`,
        confirmLabel: 'Allow network access',
        destructive: true,
      }))
    ) {
      return
    }
    setSaving(true)
    setError(null)
    try {
      // Re-read the connection so concurrent edits of other options are not overwritten (PATCH replaces options).
      const fresh = await getConnection(connectionId)
      const options = { ...fresh.options }
      const specs = rows.map(toSpec)
      if (specs.length) options.forwards = specs
      else delete options.forwards
      const saved = await updateConnection(connectionId, { options })
      queryClient.setQueryData(queryKeys.connection(connectionId), saved)
      void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
      toast.success('Session port forwards saved', { description: 'They apply the next time a session to this server connects.' })
      onClose()
    } catch (err) {
      setError(isApiError(err) && err.status === 403 ? 'This shared connection is read-only for you.' : errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  const name = conn.data?.name ?? 'SSH connection'
  return (
    <Dialog open onOpenChange={(o) => !o && !saving && onClose()}>
      <DialogContent size="2xl" className="max-h-[calc(100dvh-2rem)]">
        <DialogHeader>
          <DialogTitle>
            <Cable className="size-4 text-primary" /> Session port forwards — {name}
          </DialogTitle>
          <DialogDescription>
            Started with every terminal session to this server (and after reconnects), stopped when the session closes — like the -L / -R / -D
            options of ssh. For tunnels that run on their own, use the tunnel manager.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="grid content-start gap-3">
          {conn.isLoading || rows === null ? (
            conn.isError ? (
              <EmptyState size="sm" icon={Cable} title="Could not load the connection" description={errorMessage(conn.error)} />
            ) : (
              <LoadingPane />
            )
          ) : (
            <>
              {rows.length === 0 ? (
                <EmptyState size="sm" icon={Cable} title="No session forwards" description="Add one to have it open with every session to this server." />
              ) : (
                <table className="w-full border-collapse text-base">
                  <thead>
                    <tr className="text-left text-xs text-muted-foreground">
                      <th className="w-10 pb-1 font-medium">On</th>
                      <th className="pb-1 font-medium">Type</th>
                      <th className="pb-1 font-medium">Listen address</th>
                      <th className="w-24 pb-1 font-medium">Port</th>
                      <th className="pb-1 font-medium">Destination</th>
                      <th className="w-24 pb-1 font-medium">Port</th>
                      <th className="w-8" aria-label="Remove" />
                    </tr>
                  </thead>
                  <tbody className="[&_td]:py-1 [&_td]:pr-1.5">
                    {rows.map((r, i) => {
                      const proxy = isProxyKind(r.kind)
                      const err = showErrors ? errors[i] : null
                      return (
                        <tr key={r.key} className={cn('align-top', err && '[&_input]:aria-invalid:border-destructive')}>
                          <td className="pt-2">
                            <Switch size="sm" checked={r.enabled} onCheckedChange={(v) => update(r.key, { enabled: v })} aria-label="Enabled" />
                          </td>
                          <td>
                            <SimpleSelect<TunnelKind>
                              size="sm"
                              aria-label="Forward type"
                              className="w-40"
                              value={r.kind}
                              onValueChange={(kind) => update(r.key, { kind })}
                              options={KIND_ORDER.map((k) => ({ value: k, label: `${KINDS[k].flag} ${KINDS[k].label}` }))}
                            />
                          </td>
                          <td>
                            {r.extra.bindSocket ? (
                              <Input inputSize="sm" className="font-mono" value={r.extra.bindSocket} readOnly title="Unix socket (edit in the tunnel manager)" />
                            ) : (
                              <Input
                                inputSize="sm"
                                className="font-mono"
                                placeholder="127.0.0.1"
                                value={r.bindHost}
                                aria-invalid={!!err?.includes('listen address') || undefined}
                                onChange={(e) => update(r.key, { bindHost: e.target.value })}
                              />
                            )}
                          </td>
                          <td>
                            {!r.extra.bindSocket && (
                              <NumberInput inputSize="sm" value={r.bindPort} allowEmpty min={0} max={65535} placeholder="auto" onChange={(v) => update(r.key, { bindPort: v })} />
                            )}
                          </td>
                          <td>
                            {proxy ? (
                              <span className="flex h-7 items-center text-sm text-muted-foreground">SOCKS / HTTP proxy</span>
                            ) : r.extra.destSocket ? (
                              <Input inputSize="sm" className="font-mono" value={r.extra.destSocket} readOnly title="Unix socket (edit in the tunnel manager)" />
                            ) : (
                              <Input
                                inputSize="sm"
                                className="font-mono"
                                placeholder="localhost"
                                value={r.destHost}
                                aria-invalid={!!err?.includes('destination host') || undefined}
                                onChange={(e) => update(r.key, { destHost: e.target.value })}
                              />
                            )}
                          </td>
                          <td>
                            {!proxy && !r.extra.destSocket && (
                              <NumberInput
                                inputSize="sm"
                                value={r.destPort}
                                allowEmpty
                                min={1}
                                max={65535}
                                placeholder="80"
                                aria-invalid={!!err?.includes('Destination port') || undefined}
                                onChange={(v) => update(r.key, { destPort: v })}
                              />
                            )}
                          </td>
                          <td className="pt-0.5">
                            <IconButton icon={Trash2} size="xs" label="Remove forward" onClick={() => setRows((rs) => rs?.filter((x) => x.key !== r.key) ?? rs)} />
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              )}
              {showErrors && hasErrors && <p className="text-sm text-destructive">{errors.find(Boolean)}</p>}
              {exposed.length > 0 && (
                <div className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/8 px-2.5 py-2 text-sm">
                  <ShieldAlert className="mt-0.5 size-3.5 shrink-0 text-warning" />
                  <span className="min-w-0">
                    {exposed.length === 1 ? 'A forward listens' : `${exposed.length} forwards listen`} on a non-loopback address: other machines can
                    connect while a session is open ({exposed.map((r) => forwardLabel(toSpec(r))).join(', ')}).
                  </span>
                </div>
              )}
              <div>
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() =>
                    setRows((rs) => [
                      ...(rs ?? []),
                      { key: ++seq, kind: 'local', bindHost: '', bindPort: null, destHost: 'localhost', destPort: null, enabled: true, name: '', extra: {} },
                    ])
                  }
                >
                  <Plus /> Add forward
                </Button>
              </div>
              {liveForConn.length > 0 && (
                <section className="grid gap-1.5 rounded-md border p-2.5">
                  <h3 className="text-sm font-semibold text-muted-foreground">Active in open sessions</h3>
                  {liveForConn.map((f) => (
                    <div key={f.id} className="flex min-w-0 items-center gap-2 text-sm">
                      <StatusDot status={f.status} />
                      <span className="truncate font-mono">{forwardLabel(f.spec)}</span>
                      <span className="ml-auto shrink-0 text-muted-foreground">
                        {f.status.state === 'error' ? <span className="text-destructive">{f.status.error}</span> : (f.status.localAddr || f.status.remoteAddr) ?? ''}
                      </span>
                    </div>
                  ))}
                </section>
              )}
              {error && (
                <p role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              )}
            </>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={() => void save()} loading={saving} disabled={rows === null}>
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
