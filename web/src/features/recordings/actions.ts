/*
 * Actions shared by the recordings tab, commands and menus.
 */
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { confirm, prompt } from '@/components/ui/dialog-host'
import { errorMessage, formatBytes, plural } from '@/lib/utils'
import { openTab } from '@/stores/workspace'
import { bulkDeleteRecordings, deleteRdpRecording, deleteRecording, recKeys, terminateSession } from './api'
import type { PlayerTabParams, RecordingItem, ShadowTabParams } from './types'

export function openRecordingsTab(): void {
  openTab({ kind: 'recordings' })
}

/** Play a recording (cast → player, log → log viewer, RDP → the rdp module's player). */
export function playRecording(rec: Pick<RecordingItem, 'id' | 'title' | 'kind'> & { source?: string }, extra: Partial<PlayerTabParams> = {}): void {
  if (rec.kind === 'guac' || rec.source === 'rdp') {
    if (isCommandEnabled('rdp.recordings')) void runCommand('rdp.recordings', { id: rec.id })
    else toast.error('Remote desktop recordings cannot be played here', { description: 'The RDP module is not available.' })
    return
  }
  openTab({
    kind: 'player',
    id: `player:${rec.id}`,
    params: { recordingId: rec.id, title: rec.title, ...extra } satisfies PlayerTabParams,
    title: rec.title || 'Player',
  })
}

/** Instant replay (TERM-30): the session's active recording when it records, else its scrollback with real timing. */
export function replaySession(session: Pick<RuntimeSession, 'id' | 'title' | 'recording' | 'recordingId' | 'protocol'>, minutes: number): void {
  if (session.recording && session.recordingId) {
    playRecording({ id: session.recordingId, title: session.title, kind: 'asciicast' }, { startAt: minutes > 0 ? -minutes * 60 : 0 })
    return
  }
  openTab({
    kind: 'player',
    params: { sessionId: session.id, minutes, title: session.title, protocol: session.protocol } satisfies PlayerTabParams,
    title: `Replay · ${session.title}`,
  })
}

/** Admin: read-only view of another user's terminal session (never ends the session when closed). */
export function shadowSession(s: Pick<RuntimeSession, 'id' | 'title' | 'protocol'> & { owner?: string }): void {
  openTab({
    kind: 'shadow',
    id: `shadow:${s.id}`,
    params: { sessionId: s.id, title: s.title, owner: s.owner, protocol: s.protocol } satisfies ShadowTabParams,
  })
}

function invalidate() {
  void queryClient.invalidateQueries({ queryKey: queryKeys.recordings })
}

/** Delete recordings after a confirmation. Resolves the deleted ids. */
export async function deleteRecordingsAction(items: RecordingItem[]): Promise<string[]> {
  if (!items.length) return []
  const live = items.filter((i) => i.live)
  const bytes = items.reduce((n, i) => n + (i.size || 0), 0)
  const one = items.length === 1 ? items[0] : null
  const ok = await confirm({
    title: one ? `Delete “${one.title || 'recording'}”?` : `Delete ${plural(items.length, 'recording')}?`,
    description:
      `${formatBytes(bytes)} will be freed. This cannot be undone.` +
      (live.length ? ` ${plural(live.length, 'recording')} still being written will be skipped.` : ''),
    confirmLabel: 'Delete',
    destructive: true,
  })
  if (!ok) return []
  const rdp = items.filter((i) => i.source === 'rdp')
  const term = items.filter((i) => i.source !== 'rdp' && !i.live)
  const deleted: string[] = []
  const failures: string[] = []
  try {
    if (term.length === 1) {
      await deleteRecording(term[0].id)
      deleted.push(term[0].id)
    } else if (term.length > 1) {
      const r = await bulkDeleteRecordings(term.map((i) => i.id))
      deleted.push(...r.deleted)
      failures.push(...r.failed.map((f) => f.error))
    }
    for (const r of rdp) {
      try {
        await deleteRdpRecording(r.id)
        deleted.push(r.id)
      } catch (err) {
        failures.push(errorMessage(err))
      }
    }
  } catch (err) {
    failures.push(isApiError(err) && err.code === 'recording_active' ? 'The session is still writing it — stop the recording first.' : errorMessage(err))
  }
  invalidate()
  if (deleted.length) toast.success(deleted.length === 1 ? 'Recording deleted' : `${deleted.length} recordings deleted`)
  if (failures.length) toast.error(`${plural(failures.length, 'recording')} could not be deleted`, { description: failures[0] })
  return deleted
}

/** Admin: force-close another user's session, with an optional reason shown to the user. Resolves true when done. */
export async function terminateAction(s: { id: string; title: string; owner?: string }): Promise<boolean> {
  const reason = await prompt({
    title: `Terminate “${s.title}”${s.owner ? ` of ${s.owner}` : ''}?`,
    description: 'The session is closed immediately. The reason (optional) is shown in the terminal and to the user.',
    label: 'Reason',
    placeholder: 'e.g. maintenance window',
    confirmLabel: 'Terminate',
    selectOnOpen: false,
  })
  if (reason === null) return false
  try {
    await terminateSession(s.id, reason.trim() || undefined)
    void queryClient.invalidateQueries({ queryKey: recKeys.adminSessions })
    toast.success('Session terminated')
    return true
  } catch (err) {
    toast.error('Could not terminate the session', { description: errorMessage(err) })
    return false
  }
}

/** Open one of the caller's own sessions in its regular tab (terminal / vnc / rdp attach commands). */
export function attachOwnSession(s: Pick<RuntimeSession, 'id' | 'kind'>): void {
  const cmd = s.kind === 'terminal' ? 'terminal.attach' : `${s.kind}.attach`
  void runCommand(cmd, { sessionId: s.id }).then((ok) => {
    if (!ok) toast.error('Cannot open this session here')
  })
}
