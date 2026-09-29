/*
 * User actions on the embedded servers (start / stop / restart / save) with toasts and error handling, shared by the
 * cards, commands, the ribbon menu and the dialogs.
 */
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { confirm } from '@/components/ui/dialog-host'
import { copyText, errorMessage } from '@/lib/utils'
import { openTab } from '@/stores/workspace'
import {
  applyStatus,
  applyStatuses,
  cachedServers,
  restartServer,
  saveServerConfig,
  serverKeys,
  startServer,
  stopAllServers,
  stopServer,
} from './api'
import { KINDS } from './model'
import { serversSettings } from './settings'
import { openServerConfig } from './store'
import type { ServerKindEx, ServerStatusEx } from './types'

export function openServersTab(): void {
  openTab({ kind: 'servers', params: {} })
}

export function openSyslogTab(): void {
  openTab({ kind: 'syslog', params: {} })
}

function refresh(): void {
  void queryClient.invalidateQueries({ queryKey: serverKeys.list, exact: true })
}

function label(kind: ServerKindEx): string {
  return KINDS[kind].label
}

/** Describe a start failure (server error codes from SPEC §9 "servers"). */
function startFailure(kind: ServerKindEx, err: unknown): void {
  refresh()
  let title = `Cannot start the ${label(kind)}`
  if (isApiError(err)) {
    if (err.code === 'port_in_use') title = `${KINDS[kind].short}: port already in use`
    else if (err.code === 'port_privileged') title = `${KINDS[kind].short}: privileged port`
    else if (err.code === 'address_unavailable') title = `${KINDS[kind].short}: address not available`
    else if (err.code === 'invalid_config') title = `${KINDS[kind].short}: configuration incomplete`
  }
  toast.error(title, {
    description: errorMessage(err),
    duration: 10_000,
    action: { label: 'Configure…', onClick: () => openServerConfig(kind) },
  })
}

export async function startServerAction(kind: ServerKindEx): Promise<boolean> {
  try {
    const st = await startServer(kind)
    applyStatus(st)
    toast.success(`${label(kind)} started`, { description: st.url ?? st.addr })
    return true
  } catch (err) {
    startFailure(kind, err)
    return false
  }
}

export async function stopServerAction(kind: ServerKindEx, opts: { confirm?: boolean } = {}): Promise<boolean> {
  const st = cachedServers().find((s) => s.kind === kind)
  if (opts.confirm !== false && st && st.running && st.clients > 0 && kind !== 'syslog' && serversSettings.get().confirmStopWithClients) {
    const ok = await confirm({
      title: `Stop the ${label(kind)}?`,
      description: `${st.clients} connected client${st.clients === 1 ? '' : 's'} will be disconnected.`,
      confirmLabel: 'Stop server',
      destructive: true,
    })
    if (!ok) return false
  }
  try {
    applyStatus(await stopServer(kind))
    toast(`${label(kind)} stopped`)
    return true
  } catch (err) {
    refresh()
    toast.error(`Cannot stop the ${label(kind)}`, { description: errorMessage(err) })
    return false
  }
}

export async function toggleServerAction(kind: ServerKindEx): Promise<boolean> {
  const st = cachedServers().find((s) => s.kind === kind)
  if (st?.running || st?.state === 'starting') return stopServerAction(kind)
  return startServerAction(kind)
}

export async function restartServerAction(kind: ServerKindEx): Promise<boolean> {
  try {
    const st = await restartServer(kind)
    applyStatus(st)
    toast.success(`${label(kind)} restarted`, { description: st.url ?? st.addr })
    return true
  } catch (err) {
    startFailure(kind, err)
    return false
  }
}

export async function stopAllAction(): Promise<void> {
  const running = cachedServers().filter((s) => s.running)
  if (!running.length) {
    toast.info('No server is running')
    return
  }
  const clients = running.reduce((n, s) => n + (s.kind === 'syslog' ? 0 : s.clients), 0)
  if (clients > 0 && serversSettings.get().confirmStopWithClients) {
    const ok = await confirm({
      title: `Stop ${running.length} server${running.length === 1 ? '' : 's'}?`,
      description: `${clients} connected client${clients === 1 ? '' : 's'} will be disconnected.`,
      confirmLabel: 'Stop all',
      destructive: true,
    })
    if (!ok) return
  }
  try {
    applyStatuses(await stopAllServers())
    toast(`Stopped ${running.length} server${running.length === 1 ? '' : 's'}`)
  } catch (err) {
    refresh()
    toast.error('Cannot stop the servers', { description: errorMessage(err) })
  }
}

/**
 * Save a configuration. Resolves the new status, or throws the API error (the dialog shows it). A running server is
 * restarted by the backend; a failed restart is reported here.
 */
export async function saveConfigAction(kind: ServerKindEx, config: Record<string, unknown>): Promise<ServerStatusEx> {
  const before = cachedServers().find((s) => s.kind === kind)
  const st = await saveServerConfig(kind, config)
  applyStatus(st)
  if (st.state === 'error') {
    toast.error(`Saved, but the ${label(kind)} could not restart`, { description: st.error, duration: 10_000 })
  } else if (before?.running && st.running && before.startedAt !== st.startedAt) {
    toast.success(`${label(kind)} saved and restarted`)
  } else {
    toast.success(`${label(kind)} settings saved`)
  }
  return st
}

export async function copyWithToast(text: string, what = 'Copied to the clipboard'): Promise<void> {
  if (await copyText(text)) toast.success(what, { description: text.length > 80 ? `${text.slice(0, 80)}…` : text })
  else toast.error('Cannot access the clipboard')
}
