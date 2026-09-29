/*
 * UI state of the automation feature: dialogs rendered by the always-mounted overlay (Overlay.tsx), so commands,
 * menus, terminal plugins and other features can open them.
 */
import { create } from 'zustand'
import type { Snippet } from '@/api/types'
import type { TemplateVar } from './template'
import type { DangerMatch, MacroStep } from './types'

export interface VariablesRequest {
  id: number
  title: string
  vars: TemplateVar[]
  /** Pre-filled values (e.g. the last values used for this snippet). */
  initial: Record<string, string>
  resolve: (values: Record<string, string> | null) => void
}

export interface DangerRequest {
  id: number
  matches: DangerMatch[]
  targets: string[]
  resolve: (ok: boolean) => void
}

export type SessionPickRequest = {
  id: number
  title: string
  description?: string
  confirmLabel: string
  /** Pre-selected session ids. */
  selected: string[]
  resolve: (sessionIds: string[] | null) => void
}

interface AutomationUI {
  /** Snippet editor: `null` closed, `{}` new, `{snippet}` edit. */
  snippetEditor: { snippet?: Snippet; initial?: Partial<Snippet> } | null
  /** Macro editor: macro id or 'new'. */
  macroEditor: { id?: string; steps?: MacroStep[]; name?: string } | null
  logonEditor: { connectionId: string } | null
  buttonEditor: { barId?: string } | null
  composeOpen: boolean
  variables: VariablesRequest[]
  danger: DangerRequest[]
  sessionPick: SessionPickRequest | null
}

export const useAutomationUI = create<AutomationUI>(() => ({
  snippetEditor: null,
  macroEditor: null,
  logonEditor: null,
  buttonEditor: null,
  composeOpen: false,
  variables: [],
  danger: [],
  sessionPick: null,
}))

let seq = 1

export function openSnippetEditor(snippet?: Snippet, initial?: Partial<Snippet>): void {
  useAutomationUI.setState({ snippetEditor: { snippet, initial } })
}

export function closeSnippetEditor(): void {
  useAutomationUI.setState({ snippetEditor: null })
}

export function openMacroEditor(arg: { id?: string; steps?: MacroStep[]; name?: string } = {}): void {
  useAutomationUI.setState({ macroEditor: arg })
}

export function closeMacroEditor(): void {
  useAutomationUI.setState({ macroEditor: null })
}

export function openLogonEditor(connectionId: string): void {
  useAutomationUI.setState({ logonEditor: { connectionId } })
}

export function openButtonEditor(barId?: string): void {
  useAutomationUI.setState({ buttonEditor: { barId } })
}

export function setComposeOpen(open: boolean): void {
  useAutomationUI.setState({ composeOpen: open })
}

/** Ask for snippet variables. Resolves the values, or null when cancelled. */
export function askVariables(title: string, vars: TemplateVar[], initial: Record<string, string> = {}): Promise<Record<string, string> | null> {
  return new Promise((resolve) => {
    const req: VariablesRequest = { id: seq++, title, vars, initial, resolve }
    useAutomationUI.setState((s) => ({ variables: [...s.variables, req] }))
  })
}

export function finishVariables(id: number, values: Record<string, string> | null): void {
  const req = useAutomationUI.getState().variables.find((r) => r.id === id)
  useAutomationUI.setState((s) => ({ variables: s.variables.filter((r) => r.id !== id) }))
  req?.resolve(values)
}

/** Ask before sending dangerous text (SEC-21). Cancel is the default button. */
export function askDangerous(matches: DangerMatch[], targets: string[] = []): Promise<boolean> {
  if (!matches.length) return Promise.resolve(true)
  return new Promise((resolve) => {
    const req: DangerRequest = { id: seq++, matches, targets, resolve }
    useAutomationUI.setState((s) => ({ danger: [...s.danger, req] }))
  })
}

export function finishDangerous(id: number, ok: boolean): void {
  const req = useAutomationUI.getState().danger.find((r) => r.id === id)
  useAutomationUI.setState((s) => ({ danger: s.danger.filter((r) => r.id !== id) }))
  req?.resolve(ok)
}

/** Pick target sessions (run a snippet / macro on selected sessions). */
export function pickSessions(opts: { title: string; description?: string; confirmLabel?: string; selected?: string[] }): Promise<string[] | null> {
  return new Promise((resolve) => {
    const prev = useAutomationUI.getState().sessionPick
    prev?.resolve(null)
    useAutomationUI.setState({
      sessionPick: { id: seq++, title: opts.title, description: opts.description, confirmLabel: opts.confirmLabel ?? 'Run', selected: opts.selected ?? [], resolve },
    })
  })
}

export function finishSessionPick(ids: string[] | null): void {
  const req = useAutomationUI.getState().sessionPick
  useAutomationUI.setState({ sessionPick: null })
  req?.resolve(ids)
}

/** Drop every pending dialog (sign-out). */
export function resetAutomationUI(): void {
  const s = useAutomationUI.getState()
  for (const v of s.variables) v.resolve(null)
  for (const d of s.danger) d.resolve(false)
  s.sessionPick?.resolve(null)
  useAutomationUI.setState({
    snippetEditor: null,
    macroEditor: null,
    logonEditor: null,
    buttonEditor: null,
    composeOpen: false,
    variables: [],
    danger: [],
    sessionPick: null,
  })
}
