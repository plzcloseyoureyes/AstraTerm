/*
 * Built-in handling of /ws/events messages (SPEC §6.1):
 *   session.updated / session.closed → ['sessions'] query cache
 *   notify                           → toast
 *   vault                            → auth store (vault lock state); an unlock elsewhere resolves the unlock dialog
 *   prompt / prompt.cancel           → prompts store (rendered by <PromptHost/>)
 *   hello                            → refresh session list (state may have changed while disconnected)
 */
import { toast } from 'sonner'
import { applySessionClosed, applySessionUpdate } from '@/api/sessions'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { events } from './events'
import { setVaultLocked } from '@/stores/auth'
import { pushPrompt, removePrompt } from '@/stores/prompts'
import { finishVaultUnlock, useUIStore } from '@/stores/ui'

let installed: (() => void) | null = null

export function installEventHandlers(): () => void {
  if (installed) return installed
  const offs = [
    events.on('hello', () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.sessions })
    }),
    events.on('session.updated', (ev) => {
      if (ev.session && typeof ev.session.id === 'string') applySessionUpdate(queryClient, ev.session)
    }),
    events.on('session.closed', (ev) => {
      if (typeof ev.id === 'string') applySessionClosed(queryClient, ev.id)
    }),
    events.on('notify', (ev) => {
      const opts = ev.message ? { description: ev.message } : undefined
      switch (ev.level) {
        case 'success':
          toast.success(ev.title, opts)
          break
        case 'warning':
          toast.warning(ev.title, opts)
          break
        case 'error':
          toast.error(ev.title, { ...opts, duration: 8000 })
          break
        default:
          toast.info(ev.title, opts)
      }
    }),
    events.on('vault', (ev) => {
      setVaultLocked(!!ev.locked)
      void queryClient.invalidateQueries({ queryKey: queryKeys.vaultStatus })
      // Unlocked elsewhere (another tab or device, another admin): close a pending unlock dialog so the requests
      // waiting on it (HTTP 423 → unlock → retry) go through instead of asking for a password that is not needed.
      if (!ev.locked && useUIStore.getState().vaultUnlockOpen) finishVaultUnlock(true)
    }),
    events.on('prompt', (ev) => {
      if (ev.prompt) pushPrompt(ev.prompt)
    }),
    events.on('prompt.cancel', (ev) => {
      if (typeof ev.id === 'string') removePrompt(ev.id)
    }),
  ]
  installed = () => {
    for (const off of offs) off()
    installed = null
  }
  return installed
}
