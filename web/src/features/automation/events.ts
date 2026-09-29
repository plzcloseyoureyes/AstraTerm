/*
 * Automation events on /ws/events:
 *   automation.trigger → toast / desktop notification / sound (per the trigger's actions and the user's settings),
 *                        live hit counters and the trigger log
 *   automation.run     → run history caches
 */
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { runCommand } from '@/app/commands'
import { events } from '@/lib/events'
import { findSessionTabs } from '@/features/terminal/open'
import { focusTab } from '@/stores/workspace'
import { autoKeys, installJobBuffer } from './api'
import { automationSettings } from './settings'
import { playSound } from './sound'
import type { Run, RunEvent, Trigger, TriggerEvent } from './types'

let installed = false

function focusSession(sessionId: string): void {
  const tab = findSessionTabs(sessionId)[0]
  if (tab) focusTab(tab)
  else void runCommand('terminal.attach', { sessionId })
}

function desktopNotify(ev: TriggerEvent): void {
  if (typeof Notification === 'undefined' || !ev.notify) return
  const show = () => {
    try {
      const n = new Notification(ev.notify!.title, { body: ev.notify!.message || `${ev.sessionTitle}: ${ev.line}`, tag: `nx-trigger-${ev.triggerId}` })
      n.onclick = () => {
        window.focus()
        focusSession(ev.sessionId)
        n.close()
      }
    } catch {
      /* not allowed in this context */
    }
  }
  if (Notification.permission === 'granted') show()
  else if (Notification.permission === 'default') void Notification.requestPermission().then((p) => p === 'granted' && show())
}

function onTrigger(ev: TriggerEvent): void {
  const s = automationSettings.get()
  // Live counters for the triggers page.
  queryClient.setQueryData<Trigger[]>(autoKeys.triggers, (old) => old?.map((t) => (t.id === ev.triggerId ? { ...t, stats: ev.stats } : t)))
  if (ev.logged) void queryClient.invalidateQueries({ queryKey: ['automation', 'trigger-log'] })
  if (ev.sound && s.triggerSounds) playSound(ev.sound)
  if (!ev.notify) return
  const hidden = typeof document !== 'undefined' && document.visibilityState === 'hidden'
  if (ev.notify.desktop && s.triggerDesktop && (hidden || !document.hasFocus())) desktopNotify(ev)
  if (!s.triggerToasts) return
  const opts = {
    description: ev.notify.message || `${ev.sessionTitle} · ${ev.line}`,
    action: { label: 'Show', onClick: () => focusSession(ev.sessionId) },
    id: `nx-trigger-${ev.triggerId}-${ev.sessionId}`,
  }
  switch (ev.notify.level) {
    case 'success':
      toast.success(ev.notify.title, opts)
      break
    case 'warning':
      toast.warning(ev.notify.title, opts)
      break
    case 'error':
      toast.error(ev.notify.title, { ...opts, duration: 10_000 })
      break
    default:
      toast.info(ev.notify.title, opts)
  }
}

function onRun(run: Run): void {
  void queryClient.invalidateQueries({ queryKey: autoKeys.runsAll })
  queryClient.setQueryData<Run>(autoKeys.run(run.id), (old) => (old ? { ...old, ...run, log: old.log, results: old.results } : old))
  if (run.status !== 'running') {
    void queryClient.invalidateQueries({ queryKey: autoKeys.run(run.id) })
    void queryClient.invalidateQueries({ queryKey: autoKeys.schedules })
  }
}

export function installAutomationEvents(): void {
  if (installed) return
  installed = true
  installJobBuffer()
  events.onAny((raw) => {
    const ev = raw as unknown as { type: string }
    if (ev.type === 'automation.trigger') onTrigger(ev as TriggerEvent)
    else if (ev.type === 'automation.run') onRun((ev as RunEvent).run)
  })
}
