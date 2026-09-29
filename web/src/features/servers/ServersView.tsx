/*
 * The "servers" tab (SRV-1): a grid of server cards with start / stop toggles, live
 * status, clients and traffic, plus a toolbar (syslog viewer, stop all, refresh).
 */
import { CircleStop, RefreshCw, ScrollText, Server, ShieldAlert } from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { BusyIcon, LoadingPane } from '@/components/ui/spinner'
import { Toolbar, ToolbarSeparator, ToolbarSpacer } from '@/components/ui/toolbar'
import { Tooltip } from '@/components/ui/tooltip'
import { useManualRefresh } from '@/lib/hooks'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { useRunMode } from '@/stores/auth'
import { openSyslogTab, stopAllAction } from './actions'
import { useHostInfo, useServers, useServersAllowed } from './api'
import { KIND_ORDER } from './model'
import { ServerCard } from './ServerCard'

export default function ServersView(_props: TabProps) {
  const allowed = useServersAllowed()
  const mode = useRunMode()
  const { data, isLoading, error, refetch } = useServers()
  const { data: host } = useHostInfo()
  // Only a refresh the user asked for shows motion; background refetches (events) stay silent.
  const { refreshing, refresh } = useManualRefresh(refetch)
  const showLoading = useDelayedFlag(isLoading)

  if (!allowed) {
    return (
      <EmptyState
        icon={ShieldAlert}
        title="Administrators only"
        description="In server mode the embedded servers run on the AstraTerm host and can only be managed by administrators."
        className="h-full"
      />
    )
  }
  if (isLoading || showLoading) return showLoading ? <LoadingPane immediate label="Loading servers…" /> : null
  if (error || !data) {
    return (
      <EmptyState
        icon={Server}
        title="Cannot load the servers"
        description={errorMessage(error)}
        action={
          <Button variant="secondary" size="sm" onClick={refresh} loading={refreshing}>
            <RefreshCw /> Retry
          </Button>
        }
        className="h-full"
      />
    )
  }
  const list = KIND_ORDER.map((k) => data.find((s) => s.kind === k)).filter((s) => s !== undefined)
  const running = list.filter((s) => s.running).length
  const clients = list.reduce((n, s) => n + (s.running && s.kind !== 'syslog' ? s.clients : 0), 0)

  return (
    <div className="flex h-full min-h-0 flex-col">
      <Toolbar aria-label="Servers">
        <div className="flex min-w-0 items-center gap-2 px-1.5">
          <Server className="size-4 shrink-0 text-muted-foreground" aria-hidden />
          <span className="font-medium">Servers</span>
          <span className="truncate text-sm text-muted-foreground" aria-live="polite">
            {running ? `${running} of ${list.length} running` : 'All stopped'}
            {clients > 0 && ` · ${clients} client${clients === 1 ? '' : 's'}`}
          </span>
        </div>
        <ToolbarSpacer />
        <Button variant="ghost" size="sm" onClick={openSyslogTab}>
          <ScrollText /> Syslog viewer
        </Button>
        <Button variant="ghost" size="sm" disabled={!running} onClick={() => void stopAllAction()}>
          <CircleStop /> Stop all
        </Button>
        <ToolbarSeparator />
        <Tooltip content="Refresh">
          <Button variant="ghost" size="icon-sm" aria-label="Refresh" onClick={refresh} disabled={refreshing}>
            <BusyIcon icon={RefreshCw} busy={refreshing} />
          </Button>
        </Tooltip>
      </Toolbar>
      <div className="min-h-0 flex-1 overflow-auto">
        <div className="mx-auto grid max-w-[1400px] gap-4 p-4 @container">
          <p className="max-w-3xl text-sm text-muted-foreground">
            Servers run on {mode === 'server' ? `the AstraTerm host${host?.hostname ? ` (${host.hostname})` : ''}` : 'this computer'}
            {host?.osUser ? ` as ${host.osUser}` : ''}. By default they only accept connections from this machine; choose
            another interface in a server's settings to reach it from the network. Settings are kept when you stop a server.
          </p>
          <div className="grid grid-cols-1 gap-4 @[44rem]:grid-cols-2 @[70rem]:grid-cols-3">
            {list.map((st) => (
              <ServerCard key={st.kind} status={st} home={host?.home} />
            ))}
          </div>
        </div>
      </div>
    </div>
  )
}
