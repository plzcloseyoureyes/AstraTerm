/*
 * Servers preferences (settings section `servers`, per user): syslog viewer options and highlight rules.
 */
import { defineSettings } from '@/stores/settings'
import type { HighlightRule } from './types'

export interface ServersSettings {
  /** Messages kept in the syslog viewer (older ones are loaded on demand). */
  syslogViewerLimit: number
  /** Follow new syslog messages (scroll to the bottom). */
  syslogAutoScroll: boolean
  /** Show the syslog detail pane for the selected message. */
  syslogShowDetails: boolean
  /** Colour syslog rows matching these rules (first match wins). */
  syslogHighlights: HighlightRule[]
  /** Ask before stopping a server that has connected clients. */
  confirmStopWithClients: boolean
  /** Status bar item listing running servers. */
  showStatusItem: boolean
}

export const serversSettings = defineSettings<ServersSettings>('servers', {
  syslogViewerLimit: 5000,
  syslogAutoScroll: true,
  syslogShowDetails: true,
  syslogHighlights: [
    { id: 'down', pattern: '(link|interface|line protocol).*\\bdown\\b', regex: true, color: 'red', enabled: true },
    { id: 'fail', pattern: 'fail|denied|refused', regex: true, color: 'amber', enabled: true },
    { id: 'up', pattern: 'changed state to up', regex: false, color: 'green', enabled: true },
  ],
  confirmStopWithClients: true,
  showStatusItem: true,
})
