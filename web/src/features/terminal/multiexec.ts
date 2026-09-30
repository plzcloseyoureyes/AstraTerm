/*
 * MultiExec (AUTO-5): while active, keystrokes and pastes typed into a participating terminal are sent to every
 * participating terminal. Participants: the visible terminal tabs (default), all terminal tabs, or an explicit
 * selection — minus excluded tabs. The floating MultiExec bar (lazy chunk) lets the user pick targets and send a
 * compose line to all of them.
 */
import { create } from 'zustand'
import { toast } from 'sonner'
import { useAuthStore } from '@/stores/auth'
import { isTabVisible as isWorkspaceTabVisible } from '@/stores/workspace'
import { listTerminals } from './bus'
import type { TerminalHandle } from './types'

export type MultiExecScope = 'visible' | 'all' | 'selected'

interface MultiExecState {
  active: boolean
  scope: MultiExecScope
  /** Tab ids taking part when scope = 'selected'. */
  selected: string[]
  /** Tab ids left out when scope = 'visible' | 'all'. */
  excluded: string[]
}

export const useMultiExecStore = create<MultiExecState>(() => ({ active: false, scope: 'visible', selected: [], excluded: [] }))

/** Is the tab currently on screen (a pane of the active tab, floating or popped out)? */
const isTabVisible = isWorkspaceTabVisible

/** Participation of one terminal tab under the current MultiExec settings (false while MultiExec is off). */
export function isParticipant(tabId: string, state: MultiExecState = useMultiExecStore.getState()): boolean {
  if (!state.active) return false
  if (state.scope === 'selected') return state.selected.includes(tabId)
  if (state.excluded.includes(tabId)) return false
  return state.scope === 'all' ? true : isTabVisible(tabId)
}

/** Terminals that receive broadcast input right now. */
export function multiExecTargets(): TerminalHandle[] {
  const st = useMultiExecStore.getState()
  if (!st.active) return []
  return listTerminals().filter((h) => isParticipant(h.tabId, st))
}

export function setMultiExecScope(scope: MultiExecScope): void {
  useMultiExecStore.setState((s) => {
    // Switching to an explicit selection starts from the current participants.
    const selected = scope === 'selected' && s.scope !== 'selected' ? multiExecTargets().map((h) => h.tabId) : s.selected
    return { scope, selected }
  })
}

/** Include / exclude one tab (works for every scope). */
export function setMultiExecIncluded(tabId: string, include: boolean): void {
  useMultiExecStore.setState((s) => {
    if (s.scope === 'selected') {
      const selected = include ? Array.from(new Set([...s.selected, tabId])) : s.selected.filter((id) => id !== tabId)
      return { selected }
    }
    const excluded = include ? s.excluded.filter((id) => id !== tabId) : Array.from(new Set([...s.excluded, tabId]))
    return { excluded }
  })
}

let unmountBar: (() => void) | null = null
let mounting: Promise<void> | null = null

async function ensureBar(): Promise<void> {
  if (unmountBar || mounting) return mounting ?? undefined
  mounting = import('./MultiExecBar')
    .then((m) => {
      if (!useMultiExecStore.getState().active) return
      unmountBar = m.mountMultiExecBar()
    })
    .catch((err) => {
      console.error('[multiexec] could not load the MultiExec bar', err)
      toast.error('Could not open the broadcast bar')
    })
    .finally(() => {
      mounting = null
    })
  return mounting
}

/** Turn MultiExec on/off (toggle without argument). */
export function toggleMultiExec(on?: boolean): void {
  const next = on ?? !useMultiExecStore.getState().active
  if (next && !listTerminals().length) {
    toast.info('Broadcast needs at least one open terminal')
    return
  }
  useMultiExecStore.setState(next ? { active: true } : { active: false, selected: [], excluded: [] })
  if (next) void ensureBar()
  else {
    // Deferred: the bar may be the caller (its own Stop button / effects), and a root must not unmount itself mid-event.
    const unmount = unmountBar
    unmountBar = null
    if (unmount) setTimeout(unmount, 0)
  }
}

// Leaving the authenticated app (sign-out, expired session) ends MultiExec: its bar lives outside the app shell.
useAuthStore.subscribe((s, prev) => {
  if (prev.status === 'authenticated' && s.status !== 'authenticated' && useMultiExecStore.getState().active) toggleMultiExec(false)
})
