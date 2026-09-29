/*
 * Always-mounted overlay of the automation feature (registerOverlay): dialogs (snippet / macro / logon / button bar
 * editors, variables prompt, dangerous-command confirmation, session picker) and the compose window. Each one is a
 * lazy chunk loaded the first time it opens, so the main bundle only carries this switchboard.
 */
import { lazy, Suspense } from 'react'
import { useAutomationUI } from './store'

const SnippetEditorDialog = lazy(() => import('./snippets/dialogs').then((m) => ({ default: m.SnippetEditorDialog })))
const VariablesDialogs = lazy(() => import('./snippets/dialogs').then((m) => ({ default: m.VariablesDialogs })))
const DangerDialogs = lazy(() => import('./snippets/dialogs').then((m) => ({ default: m.DangerDialogs })))
const MacroEditorDialog = lazy(() => import('./macros/MacroEditorDialog').then((m) => ({ default: m.MacroEditorDialog })))
const LogonEditorDialog = lazy(() => import('./logon/LogonEditorDialog'))
const ButtonBarEditorDialog = lazy(() => import('./buttonbar/ButtonBarEditor').then((m) => ({ default: m.ButtonBarEditorDialog })))
const SessionPickDialog = lazy(() => import('./components/pickers').then((m) => ({ default: m.SessionPickDialog })))
const ComposePanel = lazy(() => import('./compose/ComposePanel').then((m) => ({ default: m.ComposePanel })))

export function AutomationOverlay() {
  const snippet = useAutomationUI((s) => !!s.snippetEditor)
  const macro = useAutomationUI((s) => !!s.macroEditor)
  const logon = useAutomationUI((s) => !!s.logonEditor)
  const buttons = useAutomationUI((s) => !!s.buttonEditor)
  const pick = useAutomationUI((s) => !!s.sessionPick)
  const variables = useAutomationUI((s) => s.variables.length > 0)
  const danger = useAutomationUI((s) => s.danger.length > 0)
  const compose = useAutomationUI((s) => s.composeOpen)
  // One boundary per dialog: a chunk loading for one never hides another (e.g. the compose window).
  return (
    <>
      {snippet && (
        <Suspense fallback={null}>
          <SnippetEditorDialog />
        </Suspense>
      )}
      {macro && (
        <Suspense fallback={null}>
          <MacroEditorDialog />
        </Suspense>
      )}
      {logon && (
        <Suspense fallback={null}>
          <LogonEditorDialog />
        </Suspense>
      )}
      {buttons && (
        <Suspense fallback={null}>
          <ButtonBarEditorDialog />
        </Suspense>
      )}
      {pick && (
        <Suspense fallback={null}>
          <SessionPickDialog />
        </Suspense>
      )}
      {variables && (
        <Suspense fallback={null}>
          <VariablesDialogs />
        </Suspense>
      )}
      {danger && (
        <Suspense fallback={null}>
          <DangerDialogs />
        </Suspense>
      )}
      {compose && (
        <Suspense fallback={null}>
          <ComposePanel />
        </Suspense>
      )}
    </>
  )
}
