/*
 * The singleton "automation" tab: scripts, batch runs, schedules, triggers, logon actions, run history.
 */
import { openTab } from '@/stores/workspace'

export type AutomationPage = 'scripts' | 'batch' | 'schedules' | 'triggers' | 'logon' | 'history'

export interface AutomationTabParams {
  page?: AutomationPage
  scriptId?: string
  runId?: string
  connectionId?: string
  triggerId?: string
  /** Batch page: pre-selected connections. */
  connectionIds?: string[]
  /** Changes on every open so the tab reacts to repeated requests for the same page. */
  nonce?: number
}

export const AUTOMATION_TAB = 'automation'

export const PAGE_TITLES: Record<AutomationPage, string> = {
  scripts: 'Scripts',
  batch: 'Batch runs',
  schedules: 'Scheduled tasks',
  triggers: 'Triggers',
  logon: 'Logon actions',
  history: 'Run history',
}

export function openAutomationTab(params: AutomationTabParams = {}): string {
  return openTab<AutomationTabParams>({ kind: AUTOMATION_TAB, params: { page: 'scripts', ...params, nonce: Date.now() }, title: 'Automation' })
}
