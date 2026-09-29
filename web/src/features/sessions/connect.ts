/*
 * Opening sessions. The terminal feature (F1a) owns tab creation (src/features/terminal/open.ts); this module is the
 * single place the session manager calls into it, adding multi-open and "jump to running session".
 *
 * F1a's open* functions resolve the new tab id, or null after reporting the failure with a toast themselves, so
 * callers here only report unexpected exceptions.
 */
import { toast } from 'sonner'
import { commands } from '@/app/registry'
import { runCommand } from '@/app/commands'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, Protocol, RuntimeSession } from '@/api/types'
import { attachSession, openConnection, openLocalShell, openQuick } from '@/features/terminal/open'
import { errorMessage } from '@/lib/utils'
import { focusTab, listTabs } from '@/stores/workspace'
import type { ConnectPosition, ConnectionDraft } from './types'

let usedTimer: ReturnType<typeof setTimeout> | null = null

/** lastUsedAt changes server-side when a saved connection opens: refresh the list shortly after (Recent section). */
function refreshLastUsedSoon(): void {
  if (usedTimer) clearTimeout(usedTimer)
  usedTimer = setTimeout(() => {
    usedTimer = null
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
  }, 1500)
}

/** Open a saved connection. Resolves true when a tab opened (failures are already reported). */
export async function connectTo(target: Connection | string, position: ConnectPosition = 'tab'): Promise<boolean> {
  const tab = await openConnection(target, position === 'tab' ? {} : { position })
  if (tab) refreshLastUsedSoon()
  return !!tab
}

/** connectTo() that also reports unexpected exceptions (UI event handlers). */
export async function connectSafely(target: Connection | string, position: ConnectPosition = 'tab'): Promise<boolean> {
  try {
    return await connectTo(target, position)
  } catch (err) {
    const name = typeof target === 'string' ? undefined : target.name
    toast.error(name ? `Could not open "${name}"` : 'Could not open the session', { description: errorMessage(err) })
    return false
  }
}

/** Open several connections one after another (each in its own tab). Resolves how many opened. */
export async function connectMany(conns: readonly Connection[]): Promise<number> {
  let ok = 0
  for (const c of conns) if (await connectSafely(c)) ok++
  return ok
}

/** Open an unsaved (quick connect) session. Resolves true when a tab opened. */
export async function connectQuick(draft: ConnectionDraft): Promise<boolean> {
  const { password, ...rest } = draft
  const tab = await openQuick({ ...rest, protocol: rest.protocol as Protocol, ...(password ? { password } : {}) })
  return !!tab
}

/** Open a local shell (optionally a specific shell id from /api/local/shells). */
export async function connectLocalShell(shellId?: string): Promise<boolean> {
  return !!(await openLocalShell(shellId))
}

/** Focus the tab showing a runtime session, or attach a new view of it. */
export function jumpToSession(s: RuntimeSession): void {
  const tab = listTabs().find((t) => (t.params as { sessionId?: unknown } | undefined)?.sessionId === s.id)
  if (tab) {
    focusTab(tab.id)
    return
  }
  const specific = `${s.kind}.attach`
  if (s.kind !== 'terminal' && commands.get(specific)) {
    void runCommand(specific, { sessionId: s.id }, { source: 'api' })
    return
  }
  attachSession(s.id).catch((err) => toast.error('Could not attach to the session', { description: errorMessage(err) }))
}
