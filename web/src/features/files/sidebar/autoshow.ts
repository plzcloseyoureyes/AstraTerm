/*
 * When an SSH session connects in the active tab, the left sidebar switches to the SFTP panel
 * (setting files.autoShowPanel, default on). Only for sessions this page just opened (not re-attached ones), once per
 * session, never on phones (it would cover the terminal) and never when the user hid or collapsed the sidebar.
 */
import type { Connection, RuntimeSession, SessionState } from '@/api/types'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { isFreshSession } from '@/features/terminal/open'
import type { TerminalTabParams } from '@/features/terminal/types'
import { showSidebarPanel } from '@/layout/Sidebar'
import { events } from '@/lib/events'
import { storage } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { activeTab } from '@/stores/workspace'
import { SFTP_PANEL } from '../open'
import { filesSettings } from '../settings'
import { forgetSession } from './state'

const lastState = new Map<string, SessionState>()
const shown = new Set<string>()

/** The shell's sidebar prefs (read-only; written by layout/Sidebar.tsx under this key). */
function sidebarCollapsed(): boolean {
  return storage.get<{ collapsed?: boolean }>('nexterm:sidebar', {}).collapsed === true
}

function browserType(s: RuntimeSession, params: TerminalTabParams | undefined): string {
  const conn = s.connectionId ? queryClient.getQueryData<Connection[]>(queryKeys.connections)?.find((c) => c.id === s.connectionId) : undefined
  const v = conn?.options?.sshBrowser ?? params?.quick?.options?.sshBrowser
  return typeof v === 'string' ? v : 'sftp'
}

function maybeShow(s: RuntimeSession): void {
  if (shown.has(s.id) || s.protocol !== 'ssh') return
  if (!filesSettings.get().autoShowPanel) return
  const tab = activeTab()
  const params = tab?.params as TerminalTabParams | undefined
  if (!tab || tab.kind !== 'terminal' || params?.sessionId !== s.id) return
  if (browserType(s, params) === 'none') return
  if (window.matchMedia('(max-width: 767px)').matches) return
  if (!appearanceSettings.get().showSidebar || sidebarCollapsed()) return
  shown.add(s.id)
  showSidebarPanel(SFTP_PANEL)
}

let installed = false

export function installAutoShow(): void {
  if (installed) return
  installed = true
  events.on('session.updated', (ev) => {
    const s = ev.session
    if (!s || typeof s.id !== 'string') return
    const prev = lastState.get(s.id)
    lastState.set(s.id, s.state)
    if (s.state !== 'connected') return
    // First connect of a session opened by this page (the first event may already say "connected").
    const firstConnect = prev === 'connecting' || prev === 'authenticating' || (prev === undefined && isFreshSession(s.id))
    if (firstConnect) maybeShow(s)
  })
  events.on('session.closed', (ev) => {
    if (typeof ev.id !== 'string') return
    lastState.delete(ev.id)
    shown.delete(ev.id)
    forgetSession(ev.id)
  })
}
