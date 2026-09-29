/*
 * Status bar item for the active RDP tab: state, engine and remote desktop size.
 */
import { runCommand } from '@/app/commands'
import { StatusBarItem } from '@/layout/StatusBar'
import { cn } from '@/lib/utils'
import { useActiveTab } from '@/stores/workspace'
import { useViewer } from './store'
import { ENGINE_LABEL, STATUS_LABEL, statusDot } from './Toolbar'

export function RdpStatusItem() {
  const tab = useActiveTab()
  const viewer = useViewer(tab?.kind === 'rdp' ? tab.id : undefined)
  if (!tab || tab.kind !== 'rdp' || !viewer) return null
  const engine = viewer.engine ? ENGINE_LABEL[viewer.engine] : 'RDP'
  const tooltip = [
    `${STATUS_LABEL[viewer.status]}${viewer.message ? ` — ${viewer.message}` : ''}`,
    `Engine: ${engine}`,
    viewer.destination ? `Server: ${viewer.destination}` : null,
    viewer.focused ? 'Keyboard: captured by the remote desktop' : null,
  ]
    .filter(Boolean)
    .join('\n')
  return (
    <>
      <StatusBarItem
        tooltip={<span className="whitespace-pre-line">{tooltip}</span>}
        onClick={() => void runCommand('settings.open', { section: 'rdp' }, { source: 'api' })}
        aria-label={`Remote desktop ${STATUS_LABEL[viewer.status]}`}
      >
        <span className="flex items-center gap-1.5">
          <span className={cn('size-1.5 rounded-full', statusDot(viewer.status))} aria-hidden />
          {engine}
        </span>
      </StatusBarItem>
      {viewer.status === 'connected' && viewer.desktop && (
        <StatusBarItem tone="muted" tooltip="Remote desktop size (pixels)" aria-label="Remote desktop size">
          <span className="tabular">
            {viewer.desktop.width}×{viewer.desktop.height}
          </span>
        </StatusBarItem>
      )}
    </>
  )
}
