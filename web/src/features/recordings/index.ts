/*
 * Recordings & sharing feature (REC-1, REC-2, REC-5, REC-7, REC-9, MU-18, MU-19, TERM-30):
 *
 *   tab kinds   recordings (singleton) · player {recordingId | sessionId+minutes} · shadow {sessionId} (admin)
 *               · liveSessions (admin, singleton) · debugLogs (admin, singleton)
 *   commands    recordings.open {tab?} · recordings.play {id, startAt?} · recordings.share {sessionId?}
 *               · recordings.replay {sessionId?, minutes?} · admin.sessions · admin.debugLogs
 *   menus       terminal / tab context menus (Share session…, Instant replay ▸), Terminal and Tools menus
 *   overlay     share dialog, admin message dialog · status item: active share links
 *   public page /share/<token> → ShareViewer (hooked in App.tsx)
 */
import { lazy } from 'react'
import { Bug, Clapperboard, Eye, History, MonitorDot, MonitorPlay, Share2 } from 'lucide-react'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerSettingsSection,
  registerStatusItem,
  registerTabKind,
  type MenuItem,
} from '@/app/registry'
import { events } from '@/lib/events'
import { useAuthStore } from '@/stores/auth'
import { activeTab } from '@/stores/workspace'
import { openRecordingsTab, playRecording, replaySession } from './actions'
import { getRecording, recKeys } from './api'
import { RecordingsOverlay } from './Overlay'
import { recordingsSettings } from './settings'
import { SharesStatusItem } from './StatusItem'
import { openShareDialog } from './store'
import type { PlayerTabParams, ShadowTabParams } from './types'
import type { RecordingsTabParams } from './RecordingsView'
import { openTab } from '@/stores/workspace'

const RecordingsView = lazy(() => import('./RecordingsView'))
const PlayerView = lazy(() => import('./PlayerView'))
const ShadowView = lazy(() => import('./ShadowView'))
const LiveSessionsView = lazy(() => import('./LiveSessionsView'))
const DebugLogsView = lazy(() => import('./DebugLogsView'))

const isAdmin = () => useAuthStore.getState().user?.role === 'admin'

registerTabKind<RecordingsTabParams>({
  kind: 'recordings',
  title: () => 'Recordings',
  icon: Clapperboard,
  singleton: true,
  component: RecordingsView,
})

registerTabKind<PlayerTabParams>({
  kind: 'player',
  title: (p) => (p?.sessionId && !p.recordingId ? `Replay · ${p.title || 'session'}` : p?.title || 'Player'),
  icon: MonitorPlay,
  component: PlayerView,
})

registerTabKind<ShadowTabParams>({
  kind: 'shadow',
  title: (p) => `${p?.title || 'Session'}${p?.owner ? ` (${p.owner})` : ''}`,
  icon: Eye,
  component: ShadowView,
  noReopen: true,
})

registerTabKind({ kind: 'liveSessions', title: () => 'Live sessions', icon: MonitorDot, singleton: true, component: LiveSessionsView })
registerTabKind({ kind: 'debugLogs', title: () => 'Debug log', icon: Bug, singleton: true, component: DebugLogsView })

registerOverlay({ id: 'recordings', component: RecordingsOverlay })
registerStatusItem({ id: 'recordings.shares', align: 'right', order: 65, component: SharesStatusItem })
registerSettingsSection({
  id: 'recordings',
  title: 'Recordings & sharing',
  icon: Clapperboard,
  order: 70,
  keywords: ['asciinema', 'cast', 'playback', 'replay', 'share link', 'retention', 'log', 'command audit'],
  component: lazy(() => import('./SettingsSection')),
})

// ---- helpers --------------------------------------------------------------------------------------------------------

function sessionById(id: string | undefined): RuntimeSession | undefined {
  if (!id) return undefined
  return queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((s) => s.id === id)
}

/** The runtime session shown by the active tab (terminal-like tabs carry params.sessionId). */
function activeSession(): RuntimeSession | undefined {
  const id = (activeTab()?.params as { sessionId?: unknown } | undefined)?.sessionId
  return typeof id === 'string' ? sessionById(id) : undefined
}

function shareSession(sessionId: string | undefined) {
  const s = sessionById(sessionId) ?? (sessionId ? undefined : activeSession())
  const id = s?.id ?? sessionId
  if (!id) {
    toast.error('Open a terminal session to share it')
    return
  }
  if (s && s.kind !== 'terminal') {
    toast.error('Only terminal sessions can be shared')
    return
  }
  openShareDialog(id, s?.title)
}

function replayItems(session: RuntimeSession | undefined): MenuItem[] {
  if (!session || session.kind !== 'terminal') return []
  const pref = recordingsSettings.get().replayMinutes
  const mins = [...new Set([pref, 1, 5, 15, 60])].sort((a, b) => a - b)
  return [
    ...mins.map<MenuItem>((m) => ({ label: `Last ${m < 60 ? `${m} min` : `${m / 60} h`}`, run: () => replaySession(session, m) })),
    { type: 'separator' },
    { label: 'Everything in the scrollback', run: () => replaySession(session, 0) },
  ]
}

function sessionMenu(session: RuntimeSession | undefined): MenuItem[] {
  if (!session || session.kind !== 'terminal' || session.ownerId !== useAuthStore.getState().user?.id) return []
  return [
    { label: 'Share session…', icon: Share2, command: 'recordings.share', args: { sessionId: session.id } },
    { type: 'submenu', label: 'Instant replay', icon: History, items: () => replayItems(session) },
  ]
}

// ---- commands -------------------------------------------------------------------------------------------------------

const CATEGORY = 'Recordings'

registerCommand<RecordingsTabParams | undefined>({
  id: 'recordings.open',
  title: 'Open Recordings',
  category: CATEGORY,
  icon: Clapperboard,
  keywords: ['session recordings', 'logs', 'asciinema', 'playback', 'command history', 'retention'],
  run: ({ args }) => {
    if (args && typeof args === 'object' && args.tab) openTab({ kind: 'recordings', params: { tab: args.tab } })
    else openRecordingsTab()
  },
})

registerCommand<{ id: string; startAt?: number }>({
  id: 'recordings.play',
  title: 'Play Recording',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    if (!args?.id) throw new Error('id is required')
    const rec = await getRecording(args.id)
    playRecording(rec, args.startAt != null ? { startAt: args.startAt } : {})
  },
})

registerCommand<{ sessionId?: string } | undefined>({
  id: 'recordings.share',
  title: 'Share Session…',
  category: CATEGORY,
  icon: Share2,
  keywords: ['share link', 'invite', 'watch', 'collaborate', 'read-only'],
  run: ({ args }) => shareSession(args?.sessionId),
})

registerCommand<{ sessionId?: string; minutes?: number } | undefined>({
  id: 'recordings.replay',
  title: 'Instant Replay of the Active Terminal',
  category: CATEGORY,
  icon: History,
  keywords: ['rewind', 'scrollback', 'history', 'what happened'],
  run: ({ args }) => {
    const s = args?.sessionId ? sessionById(args.sessionId) : activeSession()
    if (!s || s.kind !== 'terminal') throw new Error('open a terminal session first')
    replaySession(s, args?.minutes ?? recordingsSettings.get().replayMinutes)
  },
})

registerCommand({
  id: 'admin.sessions',
  title: 'Live Sessions (all users)',
  category: 'Administration',
  icon: MonitorDot,
  keywords: ['monitor', 'shadow', 'terminate', 'who is connected', 'active sessions'],
  when: isAdmin,
  run: () => void openTab({ kind: 'liveSessions' }),
})

registerCommand({
  id: 'admin.debugLogs',
  title: 'Debug Log',
  category: 'Administration',
  icon: Bug,
  keywords: ['application log', 'events', 'diagnostics', 'errors'],
  when: isAdmin,
  run: () => void openTab({ kind: 'debugLogs' }),
})

// ---- menus ----------------------------------------------------------------------------------------------------------

registerContextMenu({
  target: 'terminal',
  order: 60,
  items: (ctx) => sessionMenu(ctx.session ?? sessionById(ctx.sessionId)),
})

registerContextMenu({
  target: 'tab',
  order: 60,
  items: (ctx) => (ctx.kind === 'terminal' ? sessionMenu(sessionById((ctx.params as { sessionId?: string } | undefined)?.sessionId)) : []),
})

registerMenu({
  menu: 'terminal',
  order: 160,
  items: () => [
    { label: 'Share Session…', icon: Share2, command: 'recordings.share' },
    { label: 'Instant Replay', icon: History, command: 'recordings.replay' },
  ],
})

registerMenu({
  menu: 'tools',
  order: 170,
  items: () => [
    { label: 'Recordings & Logs', icon: Clapperboard, command: 'recordings.open' },
    ...(isAdmin()
      ? ([
          { label: 'Live Sessions', icon: MonitorDot, command: 'admin.sessions' },
          { label: 'Debug Log', icon: Bug, command: 'admin.debugLogs' },
        ] as MenuItem[])
      : []),
  ],
})

// Share links changed (created / revoked / viewer joined or left) in any window: refresh the lists.
events.onAny((ev) => {
  const e = ev as unknown as { type?: string; sessionId?: string }
  if (e.type !== 'share.changed') return
  void queryClient.invalidateQueries({ queryKey: recKeys.myShares })
  if (e.sessionId) void queryClient.invalidateQueries({ queryKey: recKeys.shares(e.sessionId) })
})

// A session starting / stopping a recording or log refreshes the recordings list (only on those transitions).
const recFlags = new Map<string, string>()
events.on('session.updated', (ev) => {
  const s = ev.session
  if (!s) return
  const flags = `${s.recording}:${s.recordingId ?? ''}:${s.logging}`
  const prev = recFlags.get(s.id)
  recFlags.set(s.id, flags)
  if (prev !== undefined && prev !== flags) void queryClient.invalidateQueries({ queryKey: queryKeys.recordings, refetchType: 'active' })
})
events.on('session.closed', (ev) => {
  if (recFlags.delete(ev.id)) void queryClient.invalidateQueries({ queryKey: queryKeys.recordings, refetchType: 'active' })
})
