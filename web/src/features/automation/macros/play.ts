/*
 * Macro replay (AUTO-1): the backend replays the steps with their timing (POST /api/macros/{id}/run), so background
 * tabs keep the timing; the dangerous-command guard runs first.
 */
import { toast } from 'sonner'
import type { Macro } from '../types'
import { errorMessage } from '@/lib/utils'
import { dangerousMatches, runMacroOnSessions } from '../api'
import { checkDangerous, typedText } from '../guard'
import { resolveTargets, type TargetMode } from '../send'
import { automationSettings } from '../settings'
import { askDangerous, pickSessions } from '../store'

export function macroDuration(m: Pick<Macro, 'steps'>): number {
  return m.steps.reduce((t, s) => t + s.delayMs, 0)
}

/** Replay a macro on sessions (backend job). */
export async function playMacro(m: Macro, where: TargetMode | 'pick', speed = automationSettings.get().macroSpeed): Promise<void> {
  let ids: string[]
  let titles: string[]
  if (where === 'pick') {
    const picked = await pickSessions({ title: `Play “${m.name}”`, confirmLabel: 'Play' })
    if (!picked?.length) return
    ids = picked
    titles = picked
  } else {
    const targets = resolveTargets(where)
    if (!targets.length) {
      toast.info('No terminal to play the macro in', { description: 'Open or focus a terminal first.' })
      return
    }
    ids = targets.map((t) => t.sessionId)
    titles = targets.map((t) => t.title)
  }
  // Secret steps start a line of their own (Ctrl+U): their value is never a command the guard could see.
  await playMacroOnSessions(m, ids, titles, speed)
}

/** Replay a macro on the given sessions (dangerous-command guard first). */
export async function playMacroOnSessions(m: Macro, ids: string[], titles: string[] = ids, speed = automationSettings.get().macroSpeed): Promise<void> {
  const pre = checkDangerous(typedText(m.steps.map((s) => (s.secret ? '\x15' : '') + s.data).join('')))
  let confirmDangerous = false
  if (pre.length) {
    if (!(await askDangerous(pre, titles))) return
    confirmDangerous = true
  }
  for (;;) {
    try {
      await runMacroOnSessions(m.id, { sessionIds: ids, speed, confirmDangerous })
      if (ids.length > 1) toast.success(`Playing “${m.name}” on ${ids.length} sessions`)
      return
    } catch (err) {
      const matches = dangerousMatches(err)
      if (matches && !confirmDangerous) {
        if (!(await askDangerous(matches, titles))) return
        confirmDangerous = true
        continue
      }
      toast.error('Could not play the macro', { description: errorMessage(err) })
      return
    }
  }
}

