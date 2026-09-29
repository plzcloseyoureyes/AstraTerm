/*
 * Listening ports of the host (MON-3): TCP listeners and bound UDP sockets with their processes, and shortcuts to
 * forward a port through the session's SSH connection (tunnels module) or open it in the built-in web proxy.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { ArrowRightLeft, Copy, Filter, Globe, Plug, RefreshCw, ShieldCheck, Skull } from 'lucide-react'
import { toast } from 'sonner'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { ContextMenuItem, ContextMenuLabel, ContextMenuSeparator } from '@/components/ui/context-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { LoadingPane } from '@/components/ui/spinner'
import { Toolbar, ToolbarSpacer } from '@/components/ui/toolbar'
import type { RuntimeSession } from '@/api/types'
import { useManualRefresh } from '@/lib/hooks'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { copyText, errorMessage } from '@/lib/utils'
import { usePorts } from './api'
import { signalProcess } from './actions'
import type { ListeningPort, TargetId } from './types'
import { VirtualTable, type Column, type SortState } from './VirtualTable'

const WILDCARD = new Set(['0.0.0.0', '::', '*', ''])

/** Where to connect to reach a listener from the host itself. */
function reachHost(p: ListeningPort): string {
  if (WILDCARD.has(p.address)) return '127.0.0.1'
  return p.address
}

function isLoopback(a: string): boolean {
  return a === '::1' || a.startsWith('127.')
}

const WEB_PORTS = new Set([80, 443, 3000, 3001, 4200, 5000, 5173, 8000, 8008, 8080, 8081, 8443, 8888, 9000, 9090, 9443])

export function PortsPanel({ target, platform, active, session }: { target: TargetId; platform?: string; active: boolean; session?: RuntimeSession }) {
  const [sudo, setSudo] = useState(false)
  const q = usePorts(target, sudo, active)
  const firstLoad = useLoadingGate(q.isPending && !q.data)
  const manual = useManualRefresh(q.refetch)
  const [filter, setFilter] = useState('')
  const [proto, setProto] = useState<'all' | 'tcp' | 'udp'>('tcp')
  const [sort, setSort] = useState<SortState>({ id: 'port', desc: false })
  const [selected, setSelected] = useState<string | null>(null)
  const box = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  const windows = platform === 'windows'
  const remote = !!session && session.protocol === 'ssh'
  useEffect(() => {
    const el = box.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(el.clientWidth))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const rows = useMemo(() => {
    const f = filter.trim().toLowerCase()
    const list = (q.data ?? []).filter(
      (p) =>
        (proto === 'all' || p.proto === proto) &&
        (!f || String(p.port).includes(f) || p.address.toLowerCase().includes(f) || (p.process ?? '').toLowerCase().includes(f) || String(p.pid ?? '') === f),
    )
    const val = (p: ListeningPort): number | string =>
      sort.id === 'port' ? p.port : sort.id === 'pid' ? (p.pid ?? 0) : sort.id === 'process' ? (p.process ?? '').toLowerCase() : sort.id === 'proto' ? p.proto : p.address
    return list.sort((a, b) => {
      const x = val(a)
      const y = val(b)
      const r = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
      return (sort.desc ? -r : r) || a.port - b.port
    })
  }, [q.data, filter, proto, sort])

  const keyOf = (p: ListeningPort) => `${p.proto}|${p.address}|${p.port}|${p.pid ?? 0}`
  const sel = rows.find((p) => keyOf(p) === selected)
  // Tunnels module: tunnels.forwardPort works for any SSH session (quick connect too); tunnels.new needs a saved connection.
  const forwardCmd = isCommandEnabled('tunnels.forwardPort') ? 'tunnels.forwardPort' : session?.connectionId && isCommandEnabled('tunnels.new') ? 'tunnels.new' : null
  const canTunnel = remote && !!forwardCmd
  const canWeb = remote && (isCommandEnabled('webproxy.open') || isCommandEnabled('tunnels.forwardPort'))

  const forward = (p: ListeningPort) => {
    if (forwardCmd === 'tunnels.forwardPort') {
      void runCommand('tunnels.forwardPort', { sessionId: target, connectionId: session?.connectionId, port: p.port, host: reachHost(p) })
    } else if (forwardCmd === 'tunnels.new' && session?.connectionId) {
      void runCommand('tunnels.new', { connectionId: session.connectionId, type: 'local', destHost: reachHost(p), destPort: p.port })
    } else {
      toast.error('Port forwarding is not available', { description: 'It needs the Tunnels module (and a saved connection for older versions).' })
    }
  }
  const browse = (p: ListeningPort) => {
    const scheme = p.port === 443 || p.port === 8443 || p.port === 9443 ? 'https' : 'http'
    if (isCommandEnabled('webproxy.open')) void runCommand('webproxy.open', { sessionId: target, host: reachHost(p), port: p.port, scheme })
    else void runCommand('tunnels.forwardPort', { sessionId: target, connectionId: session?.connectionId, port: p.port, host: reachHost(p), open: true })
  }

  const columns: Column<ListeningPort>[] = [
    { id: 'proto', header: 'Proto', width: '4rem', sortable: true, cell: (p) => <span className="uppercase">{p.proto}</span> },
    { id: 'address', header: 'Address', width: 'minmax(7rem,12rem)', sortable: true, cell: (p) => <span className="font-mono text-xs">{p.address}</span>, title: (p) => p.address },
    { id: 'port', header: 'Port', width: '5rem', align: 'right', sortable: true, cell: (p) => <span className="font-medium">{p.port}</span> },
    {
      id: 'process',
      header: 'Process',
      width: 'minmax(8rem,1fr)',
      sortable: true,
      cell: (p) => (p.process ? p.process : <span className="text-muted-foreground">{p.pid ? '—' : 'unknown (other user)'}</span>),
    },
    { id: 'pid', header: 'PID', width: '5rem', align: 'right', sortable: true, minWidth: 520, cell: (p) => p.pid || '—' },
    {
      id: 'scope',
      header: 'Scope',
      width: '6.5rem',
      minWidth: 640,
      cell: (p) =>
        WILDCARD.has(p.address) ? (
          <span className="text-warning">all interfaces</span>
        ) : isLoopback(p.address) ? (
          <span className="text-muted-foreground">loopback</span>
        ) : (
          'interface'
        ),
    },
  ]

  const menu = (p: ListeningPort) => (
    <>
      <ContextMenuLabel>
        {p.proto.toUpperCase()} {p.address}:{p.port}
      </ContextMenuLabel>
      {remote && p.proto === 'tcp' && (
        <>
          <ContextMenuItem disabled={!canTunnel} onSelect={() => forward(p)}>
            <ArrowRightLeft /> Forward to a local port…
          </ContextMenuItem>
          <ContextMenuItem disabled={!canWeb} onSelect={() => browse(p)}>
            <Globe /> Open in browser
          </ContextMenuItem>
          <ContextMenuSeparator />
        </>
      )}
      {!!p.pid && (
        <ContextMenuItem variant="destructive" onSelect={() => void signalProcess(target, { pid: p.pid!, name: p.process, command: p.process || `PID ${p.pid}` }, 'TERM', { sudo, windows })}>
          <Skull /> End owning process
        </ContextMenuItem>
      )}
      <ContextMenuItem onSelect={() => void copyText(`${p.address.includes(':') ? `[${p.address}]` : p.address}:${p.port}`)}>
        <Copy /> Copy address
      </ContextMenuItem>
    </>
  )

  if (firstLoad.hold) return firstLoad.show ? <LoadingPane immediate label="Reading sockets…" /> : null
  if (q.isError && !q.data) {
    return (
      <EmptyState
        icon={Plug}
        title="Could not list listening ports"
        description={errorMessage(q.error)}
        action={
          <Button size="sm" variant="outline" onClick={() => void q.refetch()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  }
  const unknownOwners = (q.data ?? []).some((p) => !p.pid)

  return (
    <div ref={box} className="flex min-h-0 flex-1 flex-col">
      <Toolbar aria-label="Port tools" className="gap-1">
        <Input inputSize="sm" className="w-48" placeholder="Filter ports" value={filter} onChange={(e) => setFilter(e.target.value)} leading={<Filter />} aria-label="Filter ports" />
        <SegmentedControl
          size="sm"
          value={proto}
          onValueChange={setProto}
          aria-label="Protocol"
          options={[
            { value: 'tcp', label: 'TCP' },
            { value: 'udp', label: 'UDP' },
            { value: 'all', label: 'All' },
          ]}
        />
        <IconButton icon={RefreshCw} label="Refresh" size="sm" onClick={manual.refresh} busy={manual.refreshing} />
        {!windows && (
          <IconButton icon={ShieldCheck} label={sudo ? 'Listing with sudo (all owners)' : 'List with sudo to see every owner'} size="sm" active={sudo} onClick={() => setSudo((v) => !v)} />
        )}
        <ToolbarSpacer />
        {sel && remote && sel.proto === 'tcp' && (
          <>
            <Button size="xs" variant="outline" disabled={!canTunnel} onClick={() => forward(sel)} title={canTunnel ? undefined : 'Needs the Tunnels module'}>
              <ArrowRightLeft /> Forward…
            </Button>
            {WEB_PORTS.has(sel.port) && (
              <Button size="xs" variant="outline" disabled={!canWeb} onClick={() => browse(sel)} title={canWeb ? undefined : 'Needs the web proxy or Tunnels module'}>
                <Globe /> Open
              </Button>
            )}
          </>
        )}
        <span className="text-xs text-muted-foreground tabular">{rows.length} sockets</span>
      </Toolbar>
      {unknownOwners && !sudo && !windows && (
        <div className="border-b bg-muted/40 px-3 py-1 text-xs text-muted-foreground">
          Processes of other users are only visible with sudo.{' '}
          <button type="button" className="text-primary underline-offset-2 hover:underline" onClick={() => setSudo(true)}>
            List with sudo
          </button>
        </div>
      )}
      <VirtualTable<ListeningPort>
        rows={rows}
        columns={columns}
        rowKey={keyOf}
        sort={sort}
        onSort={(id) => setSort((s) => (s.id === id ? { id, desc: !s.desc } : { id, desc: false }))}
        selected={selected}
        onSelect={setSelected}
        onActivate={(p) => remote && p.proto === 'tcp' && canTunnel && forward(p)}
        menu={menu}
        width={width}
        ariaLabel="Listening ports"
        empty={<EmptyState size="sm" icon={Plug} title="No listening sockets" description={filter ? 'Nothing matches the filter.' : undefined} />}
      />
    </div>
  )
}
