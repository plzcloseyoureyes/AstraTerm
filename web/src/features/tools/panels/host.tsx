/*
 * Synchronous host views of the AstraTerm server: network interfaces, and listening / established sockets with their
 * owning process (TOOL-6, MobaListPorts) including terminating the owner. Admin-only in server mode.
 */
import * as React from 'react'
import { useQuery } from '@tanstack/react-query'
import { Ban, EthernetPort, ListTree, MoreHorizontal, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { BusyIcon, LoadingPane } from '@/components/ui/spinner'
import { cn, errorMessage } from '@/lib/utils'
import { getInterfaces, getListening, killListener } from '../api'
import { type Column, ExportCsvButton, ResultsTable } from '../components'
import type { InterfaceInfo, SocketInfo } from '../types'

function SyncPanel({
  title,
  description,
  onRefresh,
  fetching,
  loading,
  error,
  controls,
  children,
}: {
  title: string
  description?: string
  onRefresh: () => void
  fetching?: boolean
  loading?: boolean
  error?: unknown
  controls?: React.ReactNode
  children: React.ReactNode
}) {
  const forbidden = isApiError(error) && error.status === 403
  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 flex-wrap items-center gap-2 border-b px-4 py-3">
        <div className="min-w-0 flex-1">
          <h2 className="text-md font-semibold">{title}</h2>
          {description && <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {controls}
          <Button variant="secondary" size="sm" onClick={onRefresh} disabled={fetching} aria-label="Refresh">
            <BusyIcon icon={RefreshCw} busy={!!fetching} className="size-3.5" /> Refresh
          </Button>
        </div>
      </header>
      {error && !forbidden ? <div role="alert" className="shrink-0 px-4 py-3 text-sm text-destructive">{errorMessage(error)}</div> : null}
      <div className="min-h-0 flex-1">
        {forbidden ? (
          <EmptyState size="sm" title="Administrators only" description="Host details of the AstraTerm server are available to administrators in server mode." className="h-full" />
        ) : loading ? (
          <LoadingPane />
        ) : (
          children
        )}
      </div>
    </div>
  )
}

export function InterfacesPanel() {
  const q = useQuery({ queryKey: ['tools', 'interfaces'], queryFn: getInterfaces, retry: false })
  return (
    <SyncPanel title="Network interfaces" description="Interfaces of the AstraTerm host." onRefresh={() => void q.refetch()} fetching={q.isFetching} loading={q.isLoading} error={q.error}>
      <div className="h-full overflow-auto">
        <div className="grid grid-cols-1 gap-2 p-4 @3xl:grid-cols-2 @6xl:grid-cols-3">
          {(q.data ?? []).map((ifc: InterfaceInfo) => (
            <div key={ifc.name} className="rounded-lg border bg-card p-3 text-sm">
              <div className="flex items-center gap-2 font-semibold">
                <span className={cn('size-2 rounded-full', ifc.up ? 'bg-success' : 'bg-muted-foreground/40')} aria-label={ifc.up ? 'up' : 'down'} />
                <span className="font-mono">{ifc.name}</span>
                <span className="ml-auto text-xs font-normal text-muted-foreground">MTU {ifc.mtu}</span>
              </div>
              {ifc.mac && <div className="mt-1 font-mono text-xs text-muted-foreground">{ifc.mac}</div>}
              <div className="mt-1 flex flex-col gap-0.5 font-mono text-xs">
                {(ifc.addrs ?? []).map((a) => (
                  <span key={a} className="break-all select-text">{a}</span>
                ))}
              </div>
              {(ifc.flags ?? []).length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1">
                  {(ifc.flags ?? []).map((fl) => (
                    <Badge key={fl} variant="outline">{fl}</Badge>
                  ))}
                </div>
              )}
            </div>
          ))}
          {q.data?.length === 0 && <EmptyState size="sm" icon={EthernetPort} title="No interfaces" />}
        </div>
      </div>
    </SyncPanel>
  )
}

/** host:port with IPv6 addresses bracketed ("" when there is no address). */
function hostPort(addr: string | undefined, port: number | undefined): string {
  if (!addr) return ''
  return `${addr.includes(':') ? `[${addr}]` : addr}:${port ?? ''}`
}

function portSort(s: SocketInfo): number {
  return s.localPort * 10 + (s.proto.startsWith('udp') ? 1 : 0)
}

export function ListeningPanel() {
  const [all, setAll] = React.useState(false)
  const [filter, setFilter] = React.useState('')
  const q = useQuery({ queryKey: ['tools', 'listening', all], queryFn: () => getListening(all), retry: false })

  const kill = React.useCallback(
    async (s: SocketInfo, signal: 'TERM' | 'KILL') => {
      const ok = await confirm({
        title: `${signal === 'KILL' ? 'Kill' : 'Terminate'} ${s.process || 'process'} (pid ${s.pid})?`,
        description:
          signal === 'KILL'
            ? `Sends SIGKILL: the process ends immediately without cleaning up. It owns ${s.proto} ${s.localAddr}:${s.localPort}.`
            : `Asks the process to exit (SIGTERM on Unix). It owns ${s.proto} ${s.localAddr}:${s.localPort}.`,
        destructive: true,
        confirmLabel: signal === 'KILL' ? 'Kill' : 'Terminate',
      })
      if (!ok) return
      try {
        await killListener(s.pid, signal)
        toast.success(`${signal === 'KILL' ? 'Killed' : 'Terminated'} ${s.process || `pid ${s.pid}`}`)
        window.setTimeout(() => void q.refetch(), 400)
      } catch (err) {
        toast.error('Could not signal the process', { description: errorMessage(err) })
      }
    },
    [q],
  )

  const data = React.useMemo(() => {
    const f = filter.trim().toLowerCase()
    return (q.data ?? []).filter((s) => !f || `${s.proto} ${s.localAddr}:${s.localPort} ${s.remoteAddr ?? ''}:${s.remotePort ?? ''} ${s.process} ${s.pid} ${s.user} ${s.service} ${s.state}`.toLowerCase().includes(f))
  }, [q.data, filter])

  const columns = React.useMemo<Column<SocketInfo>[]>(
    () => [
      { key: 'proto', header: 'Proto', className: 'w-16 font-mono', cell: (s) => s.proto, csv: (s) => s.proto, sortValue: (s) => s.proto },
      { key: 'local', header: 'Local address', className: 'font-mono', cell: (s) => hostPort(s.localAddr, s.localPort), csv: (s) => hostPort(s.localAddr, s.localPort), sortValue: portSort },
      ...(all
        ? [{ key: 'remote', header: 'Remote address', className: 'font-mono', cell: (s: SocketInfo) => hostPort(s.remoteAddr, s.remotePort), csv: (s: SocketInfo) => hostPort(s.remoteAddr, s.remotePort), sortValue: (s: SocketInfo) => s.remoteAddr ?? '' }]
        : []),
      { key: 'service', header: 'Service', className: 'text-muted-foreground', cell: (s) => s.service ?? '', csv: (s) => s.service ?? '', sortValue: (s) => s.service ?? '~' },
      { key: 'state', header: 'State', className: 'w-28', cell: (s) => s.state, csv: (s) => s.state, sortValue: (s) => s.state },
      { key: 'pid', header: 'PID', className: 'w-20 tabular', cell: (s) => (s.pid > 0 ? s.pid : ''), csv: (s) => String(s.pid || ''), sortValue: (s) => s.pid },
      { key: 'process', header: 'Process', cell: (s) => s.process ?? '', csv: (s) => s.process ?? '', sortValue: (s) => s.process ?? '~' },
      { key: 'user', header: 'User', className: 'text-muted-foreground', cell: (s) => s.user ?? '', csv: (s) => s.user ?? '', sortValue: (s) => s.user ?? '~' },
      {
        key: 'actions',
        header: <span className="sr-only">Actions</span>,
        className: 'w-10 text-right',
        cell: (s) =>
          s.pid > 0 ? (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-xs" aria-label={`Actions for pid ${s.pid}`}>
                  <MoreHorizontal />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => void kill(s, 'TERM')}>
                  <Ban /> Terminate process
                </DropdownMenuItem>
                <DropdownMenuItem variant="destructive" onSelect={() => void kill(s, 'KILL')}>
                  <Ban /> Kill process (SIGKILL)
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          ) : null,
      },
    ],
    [all, kill],
  )

  return (
    <SyncPanel
      title="Listening ports"
      description="Sockets on the AstraTerm host with the owning process (listening only, or every connection)."
      onRefresh={() => void q.refetch()}
      fetching={q.isFetching}
      loading={q.isLoading}
      error={q.error}
      controls={
        <>
          <Input inputSize="sm" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter…" className="w-40" aria-label="Filter sockets" />
          <CheckboxField label="Include connections" checked={all} onCheckedChange={(v) => setAll(!!v)} />
          <ExportCsvButton columns={columns} rows={data} filename={all ? 'sockets.csv' : 'listening-ports.csv'} />
        </>
      }
    >
      <ResultsTable
        columns={columns}
        rows={data}
        rowKey={(s, i) => `${s.proto}-${s.localAddr}-${s.localPort}-${s.remoteAddr ?? ''}-${s.remotePort ?? ''}-${i}`}
        label="Sockets"
        empty={<EmptyState size="sm" icon={ListTree} title="No sockets" description={filter ? 'Nothing matches the filter.' : 'Nothing is listening.'} className="h-full" />}
      />
    </SyncPanel>
  )
}
