/*
 * Status bar item: the number of running embedded servers (red when one failed); click opens the servers tab.
 */
import { Server } from 'lucide-react'
import { StatusBarItem } from '@/layout/StatusBar'
import { openServersTab } from './actions'
import { useServers } from './api'
import { KINDS } from './model'
import { serversSettings } from './settings'

export function ServersStatusItem() {
  const show = serversSettings.useValue('showStatusItem')
  const { data } = useServers(show)
  if (!show || !data) return null
  const running = data.filter((s) => s.running)
  const failed = data.filter((s) => s.state === 'error')
  if (!running.length && !failed.length) return null
  const lines = [
    ...running.map((s) => `● ${KINDS[s.kind]?.short ?? s.kind} — ${s.url ?? s.addr ?? ''}${s.kind !== 'syslog' && s.clients ? ` · ${s.clients} client${s.clients === 1 ? '' : 's'}` : ''}`),
    ...failed.map((s) => `✕ ${KINDS[s.kind]?.short ?? s.kind} — ${s.error ?? 'error'}`),
  ]
  const label = running.length
    ? `${running.length} server${running.length === 1 ? '' : 's'}${failed.length ? ` · ${failed.length} failed` : ''}`
    : `${failed.length} server${failed.length === 1 ? '' : 's'} failed`
  return (
    <StatusBarItem
      icon={Server}
      tone={failed.length ? 'danger' : 'success'}
      tooltip={<span className="whitespace-pre-line">{lines.join('\n')}</span>}
      onClick={openServersTab}
      aria-label={`Embedded servers: ${label}`}
    >
      {label}
    </StatusBarItem>
  )
}
