/*
 * Status bar item: number of running tunnels (red when some failed); click opens the tunnel manager.
 */
import { Waypoints } from 'lucide-react'
import { StatusBarItem } from '@/layout/StatusBar'
import { formatRate } from '@/lib/utils'
import { openTunnelsTab } from './actions'
import { useTunnels } from './api'
import { isActive, statusText } from './model'
import { tunnelsSettings } from './settings'

export function TunnelsStatusItem() {
  const show = tunnelsSettings.useValue('showStatusItem')
  const { data } = useTunnels(show)
  if (!show || !data) return null
  const running = data.filter((t) => isActive(t.status))
  const failed = data.filter((t) => t.status.state === 'error')
  if (!running.length && !failed.length) return null
  const rateIn = running.reduce((n, t) => n + t.status.rateIn, 0)
  const rateOut = running.reduce((n, t) => n + t.status.rateOut, 0)
  const lines = [
    ...running.slice(0, 8).map((t) => `● ${t.name} — ${statusText(t.status)}`),
    ...(running.length > 8 ? [`… ${running.length - 8} more`] : []),
    ...failed.slice(0, 4).map((t) => `✕ ${t.name} — ${t.status.error ?? 'error'}`),
    ...(rateIn + rateOut > 0 ? [`↓ ${formatRate(rateIn)}  ↑ ${formatRate(rateOut)}`] : []),
  ]
  const label = running.length
    ? `${running.length} tunnel${running.length === 1 ? '' : 's'}${failed.length ? ` · ${failed.length} failed` : ''}`
    : `${failed.length} tunnel${failed.length === 1 ? '' : 's'} failed`
  return (
    <StatusBarItem
      icon={Waypoints}
      tone={failed.length ? 'danger' : 'default'}
      tooltip={<span className="whitespace-pre-line">{lines.join('\n')}</span>}
      onClick={openTunnelsTab}
      aria-label={`Tunnels: ${label}`}
    >
      {label}
    </StatusBarItem>
  )
}
