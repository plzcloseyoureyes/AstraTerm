/*
 * Docker "Containers" dialog (PROTO-30): lists the containers of the local engine, a configured engine, or an engine
 * reached through a saved SSH connection, and offers Exec (interactive shell), Logs (follow), Start, Stop and
 * Restart. Exec and Logs open as terminal sessions of protocol "docker".
 */
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Play, RefreshCw, RotateCw, ScrollText, Square, TerminalSquare } from 'lucide-react'
import type { DockerContainer } from '@/api/types'
import { useConnections } from '@/api/connections'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { LoadingState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { BusyIcon, Spinner } from '@/components/ui/spinner'
import { EmptyState } from '@/components/ui/empty-state'
import { errorMessage } from '@/lib/utils'
import { openQuick } from '@/features/terminal/open'
import { dockerAction, listDockerContainers, type DockerScope } from './api'
import { closeContainersDialog, useProtocolsUI } from './store'

const LOCAL = '__local__'
type Action = 'start' | 'stop' | 'restart'
const DONE: Record<Action, string> = { start: 'started', stop: 'stopped', restart: 'restarted' }

export function ContainersDialog() {
  const { open, scope } = useProtocolsUI((s) => s.containers)
  if (!open) return null
  return <ContainersDialogInner initialScope={scope} />
}

function ContainersDialogInner({ initialScope }: { initialScope: DockerScope }) {
  const qc = useQueryClient()
  const conns = useConnections()
  const sshConns = useMemo(() => (conns.data ?? []).filter((c) => c.protocol === 'ssh'), [conns.data])
  const [source, setSource] = useState<string>(initialScope.connectionId || LOCAL)
  const scope: DockerScope = source === LOCAL ? { host: initialScope.host } : { connectionId: source }

  const key = ['protocols', 'docker-containers', scope.host || '', scope.connectionId || '']
  const containers = useQuery({
    queryKey: key,
    queryFn: () => listDockerContainers(scope, true),
    retry: false,
  })

  const act = useMutation({
    mutationFn: ({ id, action }: { id: string; name: string; action: Action }) => dockerAction(id, action, scope),
    onSuccess: (_d, v) => {
      toast.success(`Container ${v.name} ${DONE[v.action]}`)
      void qc.invalidateQueries({ queryKey: key })
    },
    onError: (err, v) => toast.error(`Could not ${v.action} ${v.name}`, { description: errorMessage(err) }),
  })

  const options = [
    { value: LOCAL, label: initialScope.host ? `Engine ${initialScope.host}` : 'Local / configured engine' },
    ...sshConns.map((c) => ({ value: c.id, label: `SSH: ${c.name}` })),
  ]

  const openSession = (c: DockerContainer, logs: boolean) => {
    const name = c.name || c.id
    const options: Record<string, unknown> = { container: name }
    if (scope.connectionId) options.viaConnectionId = scope.connectionId
    if (scope.host) options.dockerHost = scope.host
    if (logs) options.dockerMode = 'logs'
    void openQuick({ protocol: 'docker', name: (logs ? 'logs ' : '') + name, options })
    closeContainersDialog()
  }

  const list = containers.data ?? []
  return (
    <Dialog open onOpenChange={(o) => !o && closeContainersDialog()}>
      <DialogContent size="2xl" className="max-h-[80vh]">
        <DialogHeader>
          <DialogTitle>Docker containers</DialogTitle>
          <DialogDescription>Exec a shell, follow logs, or start, stop and restart containers.</DialogDescription>
        </DialogHeader>
        <DialogBody className="min-h-0 gap-3">
          <div className="flex items-center gap-2">
            <div className="w-72 max-w-full">
              <SimpleSelect value={source} options={options} onValueChange={setSource} aria-label="Docker engine" />
            </div>
            <Button variant="outline" size="sm" onClick={() => containers.refetch()} disabled={containers.isFetching}>
              <BusyIcon icon={RefreshCw} busy={containers.isFetching} /> Refresh
            </Button>
          </div>

          <LoadingState busy={containers.isLoading} skeleton={<div className="flex justify-center py-10">
              <Spinner label="Listing containers" />
            </div>}>
            {containers.isError ? (
            <EmptyState icon={TerminalSquare} title="Cannot reach the Docker engine" description={errorMessage(containers.error)} />
          ) : list.length === 0 ? (
            <EmptyState icon={TerminalSquare} title="No containers" description="This engine has no containers." />
          ) : (
            <div className="min-h-0 overflow-auto rounded-md border">
              <table className="w-full table-fixed text-sm">
                <colgroup>
                  <col className="w-[32%]" />
                  <col />
                  <col className="w-24" />
                  <col className="w-36" />
                </colgroup>
                <thead className="sticky top-0 z-10 bg-muted text-left text-xs text-muted-foreground">
                  <tr>
                    <th className="px-3 py-1.5 font-medium">Name</th>
                    <th className="px-3 py-1.5 font-medium">Image</th>
                    <th className="px-3 py-1.5 font-medium">State</th>
                    <th className="px-3 py-1.5 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((c) => {
                    const name = c.name || c.id
                    const running = c.state === 'running'
                    const pending = act.isPending && act.variables?.id === c.id
                    return (
                      <tr key={c.id} className="border-t">
                        <td className="truncate px-3 py-1.5 font-mono" title={`${name} (${c.id})`}>
                          {name}
                        </td>
                        <td className="truncate px-3 py-1.5 text-muted-foreground" title={c.image}>
                          {c.image}
                        </td>
                        <td className="px-3 py-1.5">
                          <Badge variant={running ? 'success' : 'secondary'} title={c.status} className="max-w-full">
                            {c.state || 'unknown'}
                          </Badge>
                        </td>
                        <td className="px-3 py-1.5">
                          <div className="flex items-center justify-end gap-1">
                            <Spinner active={pending} label={`Updating ${name}`} />
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title="Exec shell"
                              aria-label={`Exec a shell in ${name}`}
                              disabled={!running}
                              onClick={() => openSession(c, false)}
                            >
                              <TerminalSquare className="size-4" />
                            </Button>
                            <Button variant="ghost" size="icon-sm" title="Follow logs" aria-label={`Follow the logs of ${name}`} onClick={() => openSession(c, true)}>
                              <ScrollText className="size-4" />
                            </Button>
                            {running ? (
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                title="Stop"
                                aria-label={`Stop ${name}`}
                                disabled={pending}
                                onClick={() => act.mutate({ id: c.id, name, action: 'stop' })}
                              >
                                <Square className="size-4" />
                              </Button>
                            ) : (
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                title="Start"
                                aria-label={`Start ${name}`}
                                disabled={pending}
                                onClick={() => act.mutate({ id: c.id, name, action: 'start' })}
                              >
                                <Play className="size-4" />
                              </Button>
                            )}
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title="Restart"
                              aria-label={`Restart ${name}`}
                              disabled={!running || pending}
                              onClick={() => act.mutate({ id: c.id, name, action: 'restart' })}
                            >
                              <RotateCw className="size-4" />
                            </Button>
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
          </LoadingState>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}
