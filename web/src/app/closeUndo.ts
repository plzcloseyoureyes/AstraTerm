/*
 * Forgiving close (docs/UX.md "Forgiving"), shared by the terminal / VNC / RDP tab kinds: closing the tab of a running
 * session does not end the session at once.
 * The tab goes away immediately; the session keeps running for UNDO_MS while a single toast offers "Undo" (several
 * tabs closed together share one toast), and ends when that time is up — or right away when the page goes away.
 * Undo re-attaches the still-running session through the tab kind's `reopen` (workspace.reopenClosed); any other way
 * of showing the session in a tab again within that time (reopen, attach, workspace restore) also keeps it running.
 *
 *   scheduleSessionClose(sessionId, title, { onEnd })   from a tab kind's onClose, instead of DELETE /api/sessions/{id}
 */
import { toast } from 'sonner'
import { apiUrl, isApiError, seg } from '@/api/client'
import { closeSession } from '@/api/sessions'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { errorMessage, plural } from '@/lib/utils'
import { reopenClosed, useWorkspaceStore } from '@/stores/workspace'

/** How long a closed tab's session keeps running (and the Undo toast stays). */
const UNDO_MS = 6000
const TOAST_ID = 'terminal-close-undo'

interface Pending {
  title: string
  timer: ReturnType<typeof setTimeout>
  /** Local clean-up once the session really ends (e.g. the terminal's saved screen). */
  onEnd?: () => void
}

const pending = new Map<string, Pending>()

function end(sessionId: string): void {
  const p = pending.get(sessionId)
  pending.delete(sessionId)
  p?.onEnd?.()
  closeSession(sessionId)
    .then(() => queryClient.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => old?.filter((s) => s.id !== sessionId)))
    .catch((err) => {
      if (isApiError(err) && err.status === 404) return
      toast.error('Could not close the session', { description: errorMessage(err) })
    })
}

function showToast(): void {
  const n = pending.size
  if (!n) {
    toast.dismiss(TOAST_ID)
    return
  }
  const [first] = pending.values()
  toast(n === 1 ? `Closed “${first.title}”` : `Closed ${plural(n, 'session')}`, {
    id: TOAST_ID,
    description: n === 1 ? 'The session ends in a few seconds.' : 'They end in a few seconds.',
    duration: UNDO_MS,
    action: { label: 'Undo', onClick: () => void undoAll() },
  })
}

/** Keep the session of a just-closed tab running for UNDO_MS, then end it. */
export function scheduleSessionClose(sessionId: string, title: string, opts: { onEnd?: () => void } = {}): void {
  const prev = pending.get(sessionId)
  if (prev) clearTimeout(prev.timer)
  pending.set(sessionId, {
    title,
    onEnd: opts.onEnd,
    timer: setTimeout(() => {
      end(sessionId)
      showToast()
    }, UNDO_MS),
  })
  showToast()
}

/** The session is shown again (a restored workspace uses it): keep it running, without reopening a tab. */
export function keepSession(sessionId: string): void {
  const p = pending.get(sessionId)
  if (!p) return
  clearTimeout(p.timer)
  pending.delete(sessionId)
  showToast()
}

/** Bring every pending tab back (re-attached to its still-running session) at its old place. */
async function undoAll(): Promise<void> {
  const ids = [...pending.keys()]
  for (const id of ids) {
    clearTimeout(pending.get(id)!.timer)
    pending.delete(id)
  }
  toast.dismiss(TOAST_ID)
  // One at a time, most recently closed first (the stack order): each tab goes back next to the neighbours it had
  // when it closed, so the original order is rebuilt.
  for (const id of ids.reverse()) {
    const i = useWorkspaceStore.getState().closed.findIndex((c) => (c.params as { sessionId?: string } | undefined)?.sessionId === id)
    if (i >= 0) await reopenClosed(i)
  }
}

// A pending session shown in a tab again — reopened (Ctrl+Shift+T, "Recently closed"), attached from Home or the
// palette, or restored with a workspace — keeps running: its scheduled end is cancelled.
useWorkspaceStore.subscribe((s, prev) => {
  if (!pending.size || s.tabs === prev.tabs) return
  for (const t of s.tabs) {
    const id = (t.params as { sessionId?: unknown } | undefined)?.sessionId
    if (typeof id === 'string' && pending.has(id)) keepSession(id)
  }
})

/** The page is going away: end the pending sessions now (a keepalive request survives the unload). */
function flush(): void {
  for (const [id, p] of pending) {
    clearTimeout(p.timer)
    p.onEnd?.()
    void fetch(apiUrl(`/api/sessions/${seg(id)}`), { method: 'DELETE', keepalive: true, credentials: 'same-origin', headers: { 'X-AstraTerm': '1' } }).catch(
      () => undefined,
    )
  }
  pending.clear()
}

if (typeof window !== 'undefined') window.addEventListener('pagehide', flush)
