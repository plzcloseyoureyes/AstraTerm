/*
 * Terminal feature (F1a): the "terminal" tab kind (lazy xterm chunk), terminal commands, the Terminal settings
 * section, status bar items, tab context-menu entries, MultiExec, and the public modules other features use:
 *
 *   @/features/terminal/open   openConnection / openQuick / openLocalShell / attachSession / duplicateSession
 *   @/features/terminal/bus    sendToSession / broadcast / getActiveTerminal / listTerminals / onTerminalInput|Output
 */
import { lazy } from 'react'
import { LogOut, RefreshCw, SquareTerminal } from 'lucide-react'
import { toast } from 'sonner'
import { closeSession, getSession } from '@/api/sessions'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { keepSession, scheduleSessionClose } from '@/app/closeUndo'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { registerContextMenu, registerSettingsSection, registerStatusItem, registerTabKind, type TabInfo } from '@/app/registry'
import { confirmEx } from '@/components/ui/dialog-host'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { generalSettings } from '@/stores/settings'
import { useWorkspaceStore } from '@/stores/workspace'
import { getTerminalByTab, useTerminalInfoStore } from './bus'
import { clearClipboardHistory } from './clipboard'
import { detachingTabs, detachTerminal } from './commands'
import { tintedIcon } from './icons'
import { duplicateSession, findSessionTabs, reopenTerminal, restartSessionInTab } from './open'
import { dropSavedTerminal, pruneSavedTerminals } from './persist'
import { MultiExecStatusItem, TerminalStatusItem } from './StatusItem'
import type { TerminalTabParams } from './types'

const TerminalView = lazy(() => import('./TerminalView'))

const RUNNING = new Set(['connecting', 'authenticating', 'connected'])

function isSessionLive(tab: TabInfo<TerminalTabParams>): boolean {
  const info = useTerminalInfoStore.getState().infos[tab.id]
  if (info) return RUNNING.has(info.state)
  const s = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((x) => x.id === tab.params?.sessionId)
  return !!s && RUNNING.has(s.state)
}

async function confirmClose(tab: TabInfo<TerminalTabParams>): Promise<boolean> {
  if (detachingTabs.has(tab.id) || !generalSettings.get().confirmCloseRunning || !isSessionLive(tab)) return true
  const r = await confirmEx({
    title: `Close ${tab.title}?`,
    description: 'The session is still running. Closing the tab ends it (you can undo for a few seconds; Detach keeps it running in the background).',
    confirmLabel: 'Close session',
    destructive: true,
    checkboxLabel: "Don't ask again",
  })
  if (r.ok && r.checked) generalSettings.set({ confirmCloseRunning: false })
  return r.ok
}

function onClosed(tab: TabInfo<TerminalTabParams>): void {
  const sessionId = tab.params?.sessionId
  if (!sessionId) return
  if (detachingTabs.delete(tab.id)) {
    dropSavedTerminal(sessionId)
    return
  }
  // Another tab still shows this session: keep it.
  if (findSessionTabs(sessionId).some((id) => id !== tab.id)) {
    dropSavedTerminal(sessionId)
    return
  }
  // A running session ends a few seconds later, with Undo meanwhile (app/closeUndo) — no confirmation needed.
  if (isSessionLive(tab)) {
    scheduleSessionClose(sessionId, tab.title, { onEnd: () => dropSavedTerminal(sessionId) })
    return
  }
  dropSavedTerminal(sessionId)
  closeSession(sessionId)
    .then(() => {
      queryClient.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => old?.filter((s) => s.id !== sessionId))
    })
    .catch((err) => {
      if (isApiError(err) && err.status === 404) return
      toast.error('Could not close the session', { description: errorMessage(err) })
    })
}

registerTabKind<TerminalTabParams>({
  kind: 'terminal',
  title: (p) => p?.title || protocolLabel(p?.protocol) || 'Terminal',
  icon: SquareTerminal,
  iconFor: (p) => (p?.color ? tintedIcon(protocolIcon(p.protocol ?? 'local'), p.color) : undefined),
  component: TerminalView,
  canClose: confirmClose,
  onClose: onClosed,
  duplicate: (tab, position) => void duplicateSession(tab.id, { position }),
  reopen: (closed) => reopenTerminal(closed),
  revive: (tab) => void reviveTerminal(tab),
})

/**
 * A saved workspace brought this tab back: keep its session when it still runs (even one whose tab was just closed
 * by the restore), else start a new one with the same parameters in the tab.
 */
async function reviveTerminal(tab: TabInfo<TerminalTabParams>): Promise<void> {
  const sessionId = tab.params?.sessionId
  if (sessionId) {
    keepSession(sessionId)
    try {
      const s = await getSession(sessionId)
      if (RUNNING.has(s.state)) return
    } catch {
      /* gone */
    }
  }
  await restartSessionInTab(tab.id)
}

registerSettingsSection({
  id: 'terminal',
  title: 'Terminal',
  icon: SquareTerminal,
  order: 25,
  group: 'terminal',
  keywords: [
    'font',
    'colour',
    'color',
    'scheme',
    'theme',
    'cursor',
    'scrollback',
    'bell',
    'paste',
    'clipboard',
    'copy on select',
    'right click',
    'osc 52',
    'webgl',
    'notifications',
    'multiexec',
  ],
  component: lazy(() => import('./settings/TerminalSettingsSection')),
})

registerStatusItem({ id: 'terminal', align: 'right', order: 100, component: TerminalStatusItem })
registerStatusItem({ id: 'multiexec', align: 'left', order: 20, component: MultiExecStatusItem })

// Tab strip context menu: detach / reconnect for terminal tabs.
registerContextMenu({
  target: 'tab',
  order: 50,
  items: (ctx) => {
    if (ctx.kind !== 'terminal') return []
    const t = getTerminalByTab(ctx.tabId)
    const state = t?.info().state
    return [
      { label: 'Detach (keep session running)', icon: LogOut, run: () => detachTerminal(ctx.tabId) },
      {
        label: state === 'gone' || state === 'closed' ? 'Start new session' : 'Reconnect',
        icon: RefreshCw,
        run: () => t?.reconnect(),
        disabled: !t || !(state === 'closed' || state === 'disconnected' || state === 'error' || state === 'gone'),
      },
    ]
  },
})

// Forget reload snapshots of sessions no tab shows anymore (each time a saved layout has been restored).
useWorkspaceStore.subscribe((s, prev) => {
  if (!s.ready || prev.ready) return
  setTimeout(() => {
    const keep = new Set<string>()
    for (const t of useWorkspaceStore.getState().tabs) {
      const id = (t.params as { sessionId?: unknown } | undefined)?.sessionId
      if (typeof id === 'string') keep.add(id)
    }
    pruneSavedTerminals(keep)
  }, 2000)
})

// Signing out (or an expired login) must not leave terminal content behind in this browser tab.
useAuthStore.subscribe((s, prev) => {
  if (prev.status === 'authenticated' && s.status !== 'authenticated') {
    pruneSavedTerminals(new Set())
    clearClipboardHistory()
  }
})
