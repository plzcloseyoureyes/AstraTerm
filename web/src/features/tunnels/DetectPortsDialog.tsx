/*
 * Remote listening ports (TUN-9): lists the TCP ports listening on an SSH server (a live session's host, or a saved
 * connection) and forwards one to this machine in a click — as a temporary session forward when a session is given,
 * else as a saved local tunnel — optionally opening it in the browser.
 */
import { useEffect, useMemo, useState } from 'react'
import { ArrowRightLeft, ExternalLink, MoreHorizontal, Radar, RefreshCw, Save, Search, TerminalSquare, X } from 'lucide-react'
import { useConnections } from '@/api/connections'
import { useRuntimeSession } from '@/api/sessions'
import { isCommandEnabled } from '@/app/commands'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { BusyIcon, LoadingPane } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { Tooltip } from '@/components/ui/tooltip'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { forwardRemotePort, openRemotePortViaProxy } from './actions'
import { useRemotePorts, useSessionForwards, useTunnels } from './api'
import { ConnectionPicker } from './components'
import { hostPort, isActive, isSSHConnection, webScheme } from './model'
import { openTunnelEditor, type DetectRequest } from './store'
import type { RemotePort } from './types'

const SCOPE: Record<RemotePort['scope'], { label: string; variant: 'success' | 'secondary' | 'info'; hint: string }> = {
  all: { label: 'all interfaces', variant: 'info', hint: 'Reachable from the server’s network too' },
  loopback: { label: 'loopback', variant: 'secondary', hint: 'Only reachable from the server itself' },
  address: { label: 'one interface', variant: 'secondary', hint: 'Bound to a specific address of the server' },
}

function sameHost(a: string, b: string): boolean {
  const norm = (h: string) => (h === 'localhost' || h === '127.0.0.1' || h === '::1' ? 'loopback' : h.toLowerCase())
  return norm(a) === norm(b)
}

export default function DetectPortsDialog({ request, onClose }: { request: DetectRequest; onClose: () => void }) {
  const conns = useConnections()
  const [connectionId, setConnectionId] = useState(request.connectionId ?? '')
  // With a single SSH connection there is nothing to choose.
  useEffect(() => {
    if (connectionId || request.sessionId || !conns.data) return
    const ssh = conns.data.filter(isSSHConnection)
    if (ssh.length === 1) setConnectionId(ssh[0].id)
  }, [conns.data, connectionId, request.sessionId])
  const [sessionId, setSessionId] = useState(request.sessionId ?? '')
  const session = useRuntimeSession(sessionId || null)
  const effectiveConnection = sessionId ? session?.connectionId || connectionId : connectionId
  const target = sessionId ? { sessionId } : connectionId ? { connectionId } : null
  const q = useRemotePorts(target)
  const firstLoad = useLoadingGate(q.isLoading)
  const tunnels = useTunnels()
  const sf = useSessionForwards()
  const [filter, setFilter] = useState('')
  const [hideSystem, setHideSystem] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const proxyAvailable = isCommandEnabled('webproxy.open')

  const ports = useMemo(() => {
    const list = q.data?.ports ?? []
    const f = filter.trim().toLowerCase()
    return list.filter((p) => {
      if (hideSystem && p.port < 1024) return false
      if (!f) return true
      return `${p.port} ${p.address} ${p.process ?? ''}`.toLowerCase().includes(f)
    })
  }, [q.data, filter, hideSystem])

  /** The tunnel or session forward already forwarding a port (if any). */
  const forwardedBy = (p: RemotePort): string | null => {
    for (const t of tunnels.data ?? []) {
      if (t.type === 'local' && t.connectionId === effectiveConnection && t.destPort === p.port && sameHost(t.destHost, p.connectHost)) {
        return isActive(t.status) && t.status.localAddr ? `${t.name} (${t.status.localAddr})` : t.name
      }
    }
    for (const f of sf.data ?? []) {
      if (f.sessionId === sessionId && f.spec.type === 'local' && f.spec.destPort === p.port && sameHost(f.spec.destHost ?? 'localhost', p.connectHost)) {
        return f.status.localAddr ? `this session (${f.status.localAddr})` : 'this session'
      }
    }
    return null
  }

  const forward = async (p: RemotePort, open: boolean) => {
    const key = `${p.connectHost}:${p.port}`
    setBusy(key)
    try {
      await forwardRemotePort(p, sessionId ? { sessionId } : { connectionId }, { open })
    } finally {
      setBusy(null)
    }
  }

  const openViaProxy = (p: RemotePort) => openRemotePortViaProxy(p, sessionId ? { sessionId } : { connectionId })

  const title = session ? session.title : 'SSH server'
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="xl" className="max-h-[calc(100dvh-2rem)]">
        <DialogHeader>
          <DialogTitle>
            <Radar className="size-4 text-primary" /> Listening ports
          </DialogTitle>
          <DialogDescription>
            TCP ports listening on {sessionId ? <strong className="font-medium text-foreground">{title}</strong> : 'the SSH server'}. Forward one
            to this machine in a click{sessionId ? ' — it stays open while the session is connected' : ''}.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-wrap items-center gap-2">
          {sessionId ? (
            <div className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md border bg-muted/40 px-2.5 text-base">
              <TerminalSquare className="size-3.5 shrink-0 text-muted-foreground" />
              <span className="truncate">{session ? `${session.title}${session.host ? ` — ${session.username ? `${session.username}@` : ''}${session.host}` : ''}` : 'Session'}</span>
              {effectiveConnection && (
                <Tooltip content="Query the saved connection instead">
                  <button
                    type="button"
                    aria-label="Use the saved connection"
                    className="ml-auto flex size-5 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
                    onClick={() => {
                      setConnectionId(effectiveConnection)
                      setSessionId('')
                    }}
                  >
                    <X className="size-3" />
                  </button>
                </Tooltip>
              )}
            </div>
          ) : (
            <div className="min-w-0 flex-1">
              <ConnectionPicker value={connectionId} onChange={(id) => setConnectionId(id)} />
            </div>
          )}
          <Input
            inputSize="sm"
            className="w-36"
            placeholder="Filter…"
            aria-label="Filter ports"
            leading={<Search />}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
          <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
            <Switch size="sm" checked={hideSystem} onCheckedChange={setHideSystem} aria-label="Hide ports below 1024" />
            Hide &lt; 1024
          </label>
          <Button variant="ghost" size="sm" disabled={!target || q.isFetching} onClick={() => void q.refetch()}>
            <BusyIcon icon={RefreshCw} busy={q.isFetching} /> Refresh
          </Button>
        </div>

        <DialogBody className="min-h-48">
          {!target ? (
            <EmptyState size="sm" icon={Radar} title="Choose an SSH server" description="NexTerm connects and lists the ports listening on it." />
          ) : firstLoad.hold ? (
            firstLoad.show && <LoadingPane immediate label="Asking the server…" />
          ) : q.isError ? (
            <EmptyState
              size="sm"
              icon={Radar}
              title="Could not list the ports"
              description={errorMessage(q.error)}
              action={
                <Button size="sm" variant="secondary" onClick={() => void q.refetch()}>
                  <RefreshCw /> Retry
                </Button>
              }
            />
          ) : !ports.length ? (
            <EmptyState size="sm" icon={Radar} title={q.data?.ports.length ? 'No port matches' : 'No listening TCP port found'} />
          ) : (
            <table className="w-full border-collapse text-base">
              <thead className="sticky top-0 z-10 bg-popover">
                <tr className="border-b text-left text-xs text-muted-foreground">
                  <th className="h-7 w-16 px-2 font-medium">Port</th>
                  <th className="px-2 font-medium">Listening on</th>
                  <th className="px-2 font-medium">Process</th>
                  <th className="w-px px-2" aria-label="Actions" />
                </tr>
              </thead>
              <tbody>
                {ports.map((p) => {
                  const key = `${p.connectHost}:${p.port}`
                  const web = webScheme(p.port)
                  const by = forwardedBy(p)
                  const scope = SCOPE[p.scope]
                  return (
                    <tr key={`${p.address}-${p.port}`} className="border-b border-border/60 hover:bg-accent/30">
                      <td className="px-2 py-1 font-mono font-medium tabular-nums">{p.port}</td>
                      <td className="px-2">
                        <span className="mr-1.5 font-mono text-sm">{p.address === '*' ? '*' : hostPort(p.address, p.port).replace(/:\d+$/, '')}</span>
                        <Badge variant={scope.variant} title={scope.hint}>
                          {scope.label}
                        </Badge>
                      </td>
                      <td className="max-w-0 px-2">
                        <span className="block truncate text-sm text-muted-foreground" title={p.process}>
                          {p.process ? `${p.process}${p.pid ? ` (${p.pid})` : ''}` : '—'}
                        </span>
                        {by && <span className="block truncate text-xs text-success">Forwarded by {by}</span>}
                      </td>
                      <td className="px-2 whitespace-nowrap">
                        <div className="flex items-center justify-end gap-1">
                          {web && (
                            <Button
                              size="xs"
                              variant="ghost"
                              disabled={busy === key}
                              onClick={() => void forward(p, true)}
                            >
                              <ExternalLink /> Open
                            </Button>
                          )}
                          <Button size="xs" variant="secondary" loading={busy === key} disabled={!!busy} onClick={() => void forward(p, false)}>
                            <ArrowRightLeft /> Forward
                          </Button>
                          <DropdownMenu>
                            <DropdownMenuTrigger asChild>
                              <Button size="icon-xs" variant="ghost" aria-label={`More actions for port ${p.port}`}>
                                <MoreHorizontal />
                              </Button>
                            </DropdownMenuTrigger>
                            <DropdownMenuContent align="end">
                              <DropdownMenuItem
                                disabled={!effectiveConnection}
                                onSelect={() => {
                                  openTunnelEditor({
                                    mode: 'create',
                                    initial: {
                                      connectionId: effectiveConnection,
                                      type: 'local',
                                      destHost: p.connectHost,
                                      destPort: p.port,
                                      bindPort: p.port >= 1024 ? p.port : undefined,
                                      name: `${p.process ? `${p.process} ` : ''}:${p.port}`,
                                    },
                                  })
                                }}
                              >
                                <Save /> Save as tunnel…
                              </DropdownMenuItem>
                              {web && proxyAvailable && (
                                <DropdownMenuItem onSelect={() => openViaProxy(p)}>
                                  <ExternalLink /> Open through the NexTerm web proxy
                                </DropdownMenuItem>
                              )}
                            </DropdownMenuContent>
                          </DropdownMenu>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </DialogBody>

        <DialogFooter className="items-center">
          <span className="mr-auto text-xs text-muted-foreground">
            {q.data ? `${q.data.ports.length} port${q.data.ports.length === 1 ? '' : 's'} · via ${q.data.method}${q.data.note ? ` · ${q.data.note}` : ''}` : ''}
          </span>
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
