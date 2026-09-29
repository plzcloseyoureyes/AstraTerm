/*
 * Settings section "automation" ("Highlighting & triggers" in Settings). The backend reads guardEnabled / guardStrict /
 * guardCustom (dangerous-command guard on server-side sends). The admin-only key `userScripts` (scripts for non-admins in
 * server mode) lives in the global section and is edited through /api/admin/settings.
 */
import { defineSettings } from '@/stores/settings'
import type { ButtonBar, GuardCustomRule, HighlightRule } from './types'

export type TypedGuardMode = 'off' | 'production' | 'always'
export type ComposeSendKey = 'mod-enter' | 'enter'

export interface AutomationSettings {
  // --- keyword highlighting (TERM-15) ---
  highlightEnabled: boolean
  /** Built-in rule sets that are switched on (see highlight/rules.ts). */
  highlightSets: string[]
  highlightCustom: HighlightRule[]
  /** Per saved connection: true / false overrides highlightEnabled. */
  highlightConnections: Record<string, boolean>
  /** Also apply the "highlight" actions of triggers. */
  highlightTriggers: boolean
  // --- triggers (AUTO-7) ---
  triggerToasts: boolean
  triggerSounds: boolean
  triggerDesktop: boolean
  // --- password prompts (TERM-33) ---
  passwordChip: boolean
  // --- dangerous-command guard (SEC-21) ---
  guardEnabled: boolean
  guardStrict: boolean
  /** Also check commands typed directly into one terminal (Enter): never, on production-tagged hosts, always. */
  guardTyped: TypedGuardMode
  productionTags: string[]
  guardCustom: GuardCustomRule[]
  // --- compose (AUTO-6) ---
  composeSendKey: ComposeSendKey
  composeLineDelayMs: number
  composeWaitPrompt: boolean
  composeClearAfterSend: boolean
  // --- button bar (AUTO-4) ---
  buttonBarVisible: boolean
  buttonBars: ButtonBar[]
  // --- macros (AUTO-1) ---
  macroShortcuts: Record<string, string>
  macroSpeed: number
  /** Merge consecutive keystrokes typed faster than this (ms) into one step while recording (0 = every key). */
  macroMergeMs: number
}

export const DEFAULT_PRODUCTION_TAGS = ['prod', 'production', 'prd', 'live']

export const automationSettings = defineSettings<AutomationSettings>('automation', {
  highlightEnabled: true,
  highlightSets: ['errors', 'warnings', 'success', 'ipv4', 'ipv6', 'mac', 'url', 'datetime'],
  highlightCustom: [],
  highlightConnections: {},
  highlightTriggers: true,
  triggerToasts: true,
  triggerSounds: true,
  triggerDesktop: true,
  passwordChip: true,
  guardEnabled: true,
  guardStrict: false,
  guardTyped: 'production',
  productionTags: DEFAULT_PRODUCTION_TAGS,
  guardCustom: [],
  composeSendKey: 'mod-enter',
  composeLineDelayMs: 0,
  composeWaitPrompt: false,
  composeClearAfterSend: true,
  buttonBarVisible: true,
  buttonBars: [],
  macroShortcuts: {},
  macroSpeed: 1,
  macroMergeMs: 0,
})
