/*
 * Automation feature (SPEC §10.3 "automation"; RESEARCH AUTO-1, AUTO-4..12, TERM-15, TERM-17 pacing, TERM-33, SEC-21):
 *
 *   sidebar panels   "snippets" (send to the active terminal / the broadcast group, {{variables}}), "macros" (record, replay)
 *   tab kind         "automation" (singleton): scripts, batch runs, scheduled tasks, triggers, logon actions, history
 *   overlay          compose window, snippet / macro / logon / button-bar editors, variables and guard dialogs
 *   terminal plugins keyword highlighting, password-prompt chip, dangerous-command guard on Enter
 *   status items     quick-button bar, macro recording indicator
 *   settings         "Highlighting & triggers" (section `automation`)
 *
 * Commands: automation.snippets, automation.macros, automation.runSnippet {id, sessionIds?}, automation.compose,
 * automation.recordMacro, automation.scripts, automation.open {page?}, automation.batch {connectionIds?},
 * automation.schedules, automation.triggers, automation.history, automation.logonActions {connectionId?},
 * automation.sendPassword, automation.injectSecret {key, sessionId?}, automation.highlight.toggle,
 * automation.buttonBar.toggle, automation.buttonBar.edit, automation.saveSelection, plus one palette command per
 * snippet / macro (keyboard shortcuts).
 */
import { lazy } from 'react'
import {
  Bot,
  CalendarClock,
  Circle,
  Clapperboard,
  Code2,
  Highlighter,
  History,
  KeyRound,
  Layers,
  LayoutPanelTop,
  LogIn,
  Play,
  SquarePen,
  TextCursorInput,
  Zap,
} from 'lucide-react'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import type { Snippet } from '@/api/types'
import type { Macro } from './types'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerRibbonButton,
  registerSettingsSection,
  registerSidebarPanel,
  registerStatusItem,
  registerTabKind,
  registerTerminalPlugin,
  type MenuItem,
} from '@/app/registry'
import { getActiveTerminal, getTerminalByTab } from '@/features/terminal/bus'
import { showSidebarPanel } from '@/layout/Sidebar'
import { errorMessage, isPlainObject } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { autoKeys, injectSecret, listMacros, listSnippets } from './api'
import { ButtonBarStatusItem } from './buttonbar/ButtonBar'
import { installAutomationEvents } from './events'
import { isHighlightEnabled, setupHighlighter, toggleHighlight } from './highlight/plugin'
import { playMacro } from './macros/play'
import { cancelRecording, isRecording, startRecording, stopRecording, toggleRecording } from './macros/recorder'
import { RecordingStatusItem } from './macros/RecordingStatus'
import { AutomationOverlay } from './Overlay'
import { setupDropTarget } from './plugins/dropTarget'
import { setupEnterGuard } from './plugins/enterGuard'
import { promptBeforeCursor, secretForPrompt, secretLabel, sendChipSecret, sessionSecretKeys, setupPasswordChip } from './plugins/passwordChip'
import { resolveTargets, runSnippetServer, sendSnippet, sendSnippetTo, targetForSession } from './send'
import { automationSettings } from './settings'
import { openButtonEditor, openLogonEditor, openSnippetEditor, resetAutomationUI, setComposeOpen, useAutomationUI } from './store'
import { AUTOMATION_TAB, openAutomationTab, PAGE_TITLES, type AutomationPage, type AutomationTabParams } from './tab/open'

installAutomationEvents()

const CATEGORY = 'Automation'

function argObject(args: unknown): Record<string, unknown> {
  return isPlainObject(args) ? args : {}
}

function argString(args: unknown, key: string): string | undefined {
  if (typeof args === 'string' && args) return args
  const v = argObject(args)[key]
  return typeof v === 'string' && v ? v : undefined
}

function argStrings(args: unknown, key: string): string[] | undefined {
  const v = argObject(args)[key]
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string' && !!x) : undefined
}

async function snippetById(id: string): Promise<Snippet | undefined> {
  const list = await queryClient.fetchQuery({ queryKey: autoKeys.snippets, queryFn: listSnippets, staleTime: 30_000 })
  return list.find((s) => s.id === id)
}

// ---------------------------------------------------------------------------------------------------------------------
// panels, tab, overlay, settings, status items, terminal plugins
// ---------------------------------------------------------------------------------------------------------------------

registerSidebarPanel({ id: 'snippets', title: 'Snippets', icon: TextCursorInput, order: 3, component: lazy(() => import('./snippets/SnippetsPanel')) })
registerSidebarPanel({ id: 'macros', title: 'Macros', icon: Clapperboard, order: 4, component: lazy(() => import('./macros/MacrosPanel')) })

registerTabKind<AutomationTabParams>({
  kind: AUTOMATION_TAB,
  title: () => 'Automation',
  icon: Bot,
  singleton: true,
  component: lazy(() => import('./tab/AutomationTab')),
})

registerOverlay({ id: 'automation', component: AutomationOverlay, order: 60 })

registerSettingsSection({
  id: 'automation',
  title: 'Highlighting & triggers',
  icon: Highlighter,
  order: 27,
  keywords: [
    'highlight',
    'keywords',
    'syntax',
    'colors',
    'triggers',
    'notifications',
    'sound',
    'password prompt',
    'sudo',
    'dangerous',
    'guard',
    'rm -rf',
    'production',
    'compose',
    'macros',
    'button bar',
    'scripts',
    'automation',
  ],
  component: lazy(() => import('./SettingsSection')),
})

registerStatusItem({ id: 'automation-recording', align: 'left', order: 22, component: RecordingStatusItem })
registerStatusItem({ id: 'automation-buttons', align: 'left', order: 30, component: ButtonBarStatusItem })

registerTerminalPlugin({ id: 'automation.guard', order: 40, setup: setupEnterGuard })
registerTerminalPlugin({ id: 'automation.highlight', order: 50, setup: setupHighlighter })
registerTerminalPlugin({ id: 'automation.passwordChip', order: 60, setup: setupPasswordChip })
registerTerminalPlugin({ id: 'automation.dropTarget', order: 70, setup: setupDropTarget })

// ---------------------------------------------------------------------------------------------------------------------
// commands (cross-module contract: automation.snippets / macros / runSnippet / compose)
// ---------------------------------------------------------------------------------------------------------------------

registerCommand({
  id: 'automation.snippets',
  title: 'Snippets',
  category: CATEGORY,
  icon: TextCursorInput,
  keywords: ['commands', 'library', 'saved commands'],
  run: () => showSidebarPanel('snippets'),
})

registerCommand({
  id: 'automation.macros',
  title: 'Macros',
  category: CATEGORY,
  icon: Clapperboard,
  run: () => showSidebarPanel('macros'),
})

registerCommand<{ id: string; sessionIds?: string[] } | string>({
  id: 'automation.runSnippet',
  title: 'Run Snippet',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    const id = argString(args, 'id')
    if (!id) {
      showSidebarPanel('snippets')
      return
    }
    const s = await snippetById(id)
    if (!s) throw new Error('Snippet not found')
    const ids = argStrings(args, 'sessionIds')
    if (ids?.length) await runSnippetServer(s, ids)
    else await sendSnippet(s, resolveTargets('active'))
  },
})

registerCommand({
  id: 'automation.compose',
  title: 'Compose Commands…',
  category: CATEGORY,
  icon: SquarePen,
  keybinding: 'Alt+Shift+KeyC',
  keywords: ['send to all', 'broadcast', 'write commands', 'paced', 'line by line'],
  description: 'A multi-line editor that sends to the active terminal, the broadcast group or chosen sessions',
  run: () => setComposeOpen(!useAutomationUI.getState().composeOpen),
})

registerCommand({
  id: 'automation.recordMacro',
  title: 'Record Macro (Start / Stop)',
  category: CATEGORY,
  icon: Circle,
  keybinding: 'Alt+Shift+KeyR',
  run: () => toggleRecording(),
})

registerCommand({
  id: 'automation.scripts',
  title: 'Scripts',
  category: CATEGORY,
  icon: Code2,
  keywords: ['javascript', 'expect', 'automation'],
  run: ({ args }) => void openAutomationTab({ page: 'scripts', scriptId: argString(args, 'id') }),
})

registerCommand<{ page?: AutomationPage } | undefined>({
  id: 'automation.open',
  title: 'Automation',
  category: CATEGORY,
  icon: Bot,
  run: ({ args }) => {
    const page = argString(args, 'page') as AutomationPage | undefined
    openAutomationTab({ page: page && page in PAGE_TITLES ? page : 'scripts' })
  },
})

registerCommand<{ connectionIds?: string[] } | undefined>({
  id: 'automation.batch',
  title: 'Batch Run on Many Hosts…',
  category: CATEGORY,
  icon: Layers,
  keywords: ['run on all', 'cluster', 'fleet', 'parallel ssh'],
  run: ({ args }) => void openAutomationTab({ page: 'batch', connectionIds: argStrings(args, 'connectionIds') }),
})

registerCommand({ id: 'automation.schedules', title: 'Scheduled Tasks', category: CATEGORY, icon: CalendarClock, keywords: ['cron'], run: () => void openAutomationTab({ page: 'schedules' }) })
registerCommand({ id: 'automation.triggers', title: 'Triggers', category: CATEGORY, icon: Zap, keywords: ['expect', 'alerts', 'notify'], run: () => void openAutomationTab({ page: 'triggers' }) })
registerCommand({ id: 'automation.history', title: 'Automation Run History', category: CATEGORY, icon: History, run: () => void openAutomationTab({ page: 'history' }) })

registerCommand<{ connectionId?: string } | string | undefined>({
  id: 'automation.logonActions',
  title: 'Logon Actions…',
  category: CATEGORY,
  icon: LogIn,
  keywords: ['expect', 'auto login', 'telnet'],
  run: ({ args }) => {
    const id = argString(args, 'connectionId')
    if (id) openLogonEditor(id)
    else openAutomationTab({ page: 'logon' })
  },
})

async function sendPassword(tabId?: string): Promise<void> {
  const h = tabId ? getTerminalByTab(tabId) : getActiveTerminal()
  if (!h) {
    toast.info('Focus a terminal first')
    return
  }
  if (sendChipSecret(h.tabId)) return
  const keys = await sessionSecretKeys(h.sessionId)
  if (!keys?.injectable) {
    toast.info('Stored secrets of this session cannot be typed', { description: keys ? 'The connection is shared with you.' : undefined })
    return
  }
  const key = secretForPrompt(promptBeforeCursor(h.term), keys.keys) ?? (keys.keys.includes('password') ? 'password' : keys.keys[0])
  if (!key) {
    toast.info('No password is stored for this connection')
    return
  }
  try {
    await injectSecret(h.sessionId, key, true)
    h.focus()
  } catch (err) {
    toast.error('Could not send the stored secret', { description: errorMessage(err) })
  }
}

registerCommand({
  id: 'automation.sendPassword',
  title: 'Send Stored Password',
  category: CATEGORY,
  icon: KeyRound,
  keybinding: 'Alt+Shift+KeyP',
  description: 'Types the password stored for the active terminal’s connection (sudo / passphrase prompts pick the matching secret)',
  when: () => !!getActiveTerminal(),
  run: () => sendPassword(),
})

registerCommand<{ key: string; sessionId?: string }>({
  id: 'automation.injectSecret',
  title: 'Send Stored Secret',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    const key = argString(args, 'key')
    const sessionId = argString(args, 'sessionId') ?? getActiveTerminal()?.sessionId
    if (!key || !sessionId) throw new Error('No secret or session given')
    await injectSecret(sessionId, key, argObject(args).enter !== false)
  },
})

registerCommand({
  id: 'automation.highlight.toggle',
  title: 'Toggle Keyword Highlighting (This Terminal)',
  category: 'Terminal',
  icon: Highlighter,
  keybinding: 'Alt+Shift+KeyH',
  when: () => !!getActiveTerminal(),
  run: () => {
    const h = getActiveTerminal()
    if (!h) return
    const on = toggleHighlight(h.tabId, h.session()?.connectionId)
    toast.info(`Keyword highlighting ${on ? 'on' : 'off'} in ${h.info().title}`)
  },
})

registerCommand({
  id: 'automation.buttonBar.toggle',
  title: 'Toggle Button Bar',
  category: 'View',
  icon: LayoutPanelTop,
  run: () => {
    const s = automationSettings.get()
    if (!s.buttonBars.length) {
      openButtonEditor()
      return
    }
    automationSettings.set({ buttonBarVisible: !s.buttonBarVisible })
  },
})

registerCommand({ id: 'automation.buttonBar.edit', title: 'Edit Button Bar…', category: CATEGORY, icon: LayoutPanelTop, run: () => openButtonEditor() })

registerCommand({
  id: 'automation.saveSelection',
  title: 'Save Terminal Selection as Snippet…',
  category: CATEGORY,
  icon: TextCursorInput,
  when: () => !!getActiveTerminal(),
  run: () => {
    const text = getActiveTerminal()?.getSelection() ?? ''
    openSnippetEditor(undefined, { content: text.trim(), sendMode: 'execute' })
  },
})

registerCommand({
  id: 'automation.newSnippet',
  title: 'New Snippet…',
  category: CATEGORY,
  icon: TextCursorInput,
  run: () => openSnippetEditor(),
})

// ---------------------------------------------------------------------------------------------------------------------
// per-snippet / per-macro commands (palette entries + keyboard shortcuts)
// ---------------------------------------------------------------------------------------------------------------------

const dynamic = new Map<string, () => void>()

function syncDynamicCommands(): void {
  const snippets = queryClient.getQueryData<Snippet[]>(autoKeys.snippets) ?? []
  const macros = queryClient.getQueryData<Macro[]>(autoKeys.macros) ?? []
  const shortcuts = automationSettings.get().macroShortcuts
  const want = new Map<string, Parameters<typeof registerCommand>[0]>()
  for (const s of snippets) {
    want.set(`automation.snippet.${s.id}`, {
      id: `automation.snippet.${s.id}`,
      title: `Snippet: ${s.folder ? `${s.folder} / ` : ''}${s.name}`,
      category: 'Snippets',
      icon: TextCursorInput,
      keybinding: s.shortcut || undefined,
      keywords: s.tags,
      description: s.description || s.content.slice(0, 120),
      run: () => sendSnippetTo(s, 'active'),
    })
  }
  for (const m of macros) {
    want.set(`automation.macro.${m.id}`, {
      id: `automation.macro.${m.id}`,
      title: `Macro: ${m.name}`,
      category: 'Macros',
      icon: Play,
      keybinding: shortcuts[m.id] || undefined,
      run: () => playMacro(m, 'active'),
    })
  }
  // Re-register only what changed (registration order does not matter).
  for (const [id, off] of dynamic) {
    if (!want.has(id)) {
      off()
      dynamic.delete(id)
    }
  }
  for (const [id, def] of want) {
    const prev = dynamicDefs.get(id)
    const sig = JSON.stringify([def.title, def.keybinding, def.description, def.keywords])
    if (prev === sig && dynamic.has(id)) continue
    dynamic.get(id)?.()
    dynamic.set(id, registerCommand(def))
    dynamicDefs.set(id, sig)
  }
}
const dynamicDefs = new Map<string, string>()

function clearDynamicCommands(): void {
  for (const off of dynamic.values()) off()
  dynamic.clear()
  dynamicDefs.clear()
}

queryClient.getQueryCache().subscribe((e) => {
  const k = e.query.queryKey
  if (e.type === 'updated' && (k[0] === autoKeys.snippets[0] || k[0] === autoKeys.macros[0]) && k.length === 1) syncDynamicCommands()
})
automationSettings.subscribe(() => syncDynamicCommands())

function prefetchLibrary(): void {
  void queryClient.prefetchQuery({ queryKey: autoKeys.snippets, queryFn: listSnippets, staleTime: 30_000 })
  void queryClient.prefetchQuery({ queryKey: autoKeys.macros, queryFn: listMacros, staleTime: 30_000 })
  // The guard / variables dialogs must open instantly (the guard intercepts Enter): load their small chunk when idle.
  const preload = () => void import('./snippets/dialogs').catch(() => undefined)
  if (typeof requestIdleCallback === 'function') requestIdleCallback(preload, { timeout: 5000 })
  else setTimeout(preload, 2000)
}

if (useAuthStore.getState().status === 'authenticated') prefetchLibrary()
useAuthStore.subscribe((s, prev) => {
  if (s.status === 'authenticated' && prev.status !== 'authenticated') prefetchLibrary()
  if (prev.status === 'authenticated' && s.status !== 'authenticated') {
    // Leaving the app: no dialogs, recordings, commands or command history left behind in this browser tab.
    if (isRecording()) cancelRecording()
    resetAutomationUI()
    clearDynamicCommands()
    try {
      for (let i = localStorage.length - 1; i >= 0; i--) {
        const key = localStorage.key(i)
        if (key?.startsWith('astraterm:automation:compose-history:')) localStorage.removeItem(key)
      }
    } catch {
      /* storage unavailable */
    }
  }
})

// ---------------------------------------------------------------------------------------------------------------------
// menus
// ---------------------------------------------------------------------------------------------------------------------

function automationMenuItems(): MenuItem[] {
  return [
    { label: 'Compose commands…', icon: SquarePen, command: 'automation.compose' },
    { label: isRecording() ? 'Stop macro recording' : 'Record macro', icon: Circle, command: 'automation.recordMacro' },
    { type: 'separator' },
    { label: 'Scripts…', icon: Code2, command: 'automation.scripts' },
    { label: 'Batch run on many hosts…', icon: Layers, command: 'automation.batch' },
    { label: 'Scheduled tasks…', icon: CalendarClock, command: 'automation.schedules' },
    { label: 'Triggers…', icon: Zap, command: 'automation.triggers' },
    { label: 'Logon actions…', icon: LogIn, command: 'automation.logonActions' },
    { label: 'Run history…', icon: History, command: 'automation.history' },
    { type: 'separator' },
    { label: 'Edit button bar…', icon: LayoutPanelTop, command: 'automation.buttonBar.edit' },
  ]
}

registerMenu({ menu: 'tools', order: 120, items: () => [{ type: 'submenu', label: 'Automation', icon: Bot, items: automationMenuItems }] })

registerRibbonButton({ id: 'automation', label: 'Automation', icon: Bot, order: 75, command: 'automation.scripts', menu: automationMenuItems, tooltip: 'Scripts, batch runs, schedules, triggers' })

registerContextMenu({
  target: 'terminal',
  order: 70,
  items: (ctx) => {
    const sessionId = ctx.sessionId
    const items: MenuItem[] = []
    if (sessionId) {
      const keys = queryClient.getQueryData<{ keys: string[]; injectable: boolean }>(autoKeys.secretKeys(sessionId))
      if (!keys) void sessionSecretKeys(sessionId) // for the next time the menu opens
      if (keys?.injectable && keys.keys.length) {
        items.push({
          type: 'submenu',
          label: 'Send stored secret',
          icon: KeyRound,
          items: keys.keys.map((k) => ({
            label: secretLabel(k),
            run: () => void injectSecret(sessionId, k, true).catch((err) => toast.error('Could not send the secret', { description: errorMessage(err) })),
          })),
        })
      } else if (!keys) {
        items.push({ label: 'Send stored password', icon: KeyRound, run: () => void sendPassword(ctx.tabId) })
      }
    }
    const snippets = (queryClient.getQueryData<Snippet[]>(autoKeys.snippets) ?? []).slice(0, 40)
    if (snippets.length && sessionId) {
      items.push({
        type: 'submenu',
        label: 'Run snippet',
        icon: TextCursorInput,
        items: snippets.map((s) => ({ label: s.folder ? `${s.folder} / ${s.name}` : s.name, run: () => void sendSnippet(s, [targetForSession(sessionId)]) })),
      })
    }
    if (ctx.selection?.trim()) {
      items.push({ label: 'Save selection as snippet…', icon: TextCursorInput, run: () => openSnippetEditor(undefined, { content: ctx.selection!.trim(), sendMode: 'execute' }) })
    }
    const connectionId = ctx.session?.connectionId
    items.push({
      label: 'Keyword highlighting',
      icon: Highlighter,
      checked: isHighlightEnabled(ctx.tabId, connectionId),
      run: () => void toggleHighlight(ctx.tabId, connectionId),
    })
    items.push(
      isRecording()
        ? { label: 'Stop macro recording', icon: Circle, run: () => stopRecording() }
        : { label: 'Record macro here', icon: Circle, run: () => void startRecording(ctx.tabId) },
    )
    items.push({ label: 'Compose…', icon: SquarePen, command: 'automation.compose' })
    return items
  },
})

registerContextMenu({
  target: 'session-node',
  order: 70,
  items: (ctx) => {
    const selection = ctx.selection?.length ? ctx.selection : ctx.connection ? [ctx.connection.id] : []
    const items: MenuItem[] = []
    if (ctx.connection && selection.length <= 1) {
      const conn = ctx.connection
      const hlOverride = automationSettings.get().highlightConnections[conn.id]
      items.push({ label: 'Logon actions…', icon: LogIn, run: () => openLogonEditor(conn.id) })
      items.push({
        type: 'submenu',
        label: 'Keyword highlighting',
        icon: Highlighter,
        items: [
          { label: 'Default', checked: hlOverride === undefined, run: () => setConnHighlight(conn.id, undefined) },
          { label: 'On', checked: hlOverride === true, run: () => setConnHighlight(conn.id, true) },
          { label: 'Off', checked: hlOverride === false, run: () => setConnHighlight(conn.id, false) },
        ],
      })
    }
    if (selection.length) {
      items.push({
        label: selection.length > 1 ? `Run command on ${selection.length} sessions…` : 'Run command (batch)…',
        icon: Layers,
        run: () => void openAutomationTab({ page: 'batch', connectionIds: selection }),
      })
    }
    return items
  },
})

function setConnHighlight(id: string, on: boolean | undefined): void {
  const next = { ...automationSettings.get().highlightConnections }
  if (on === undefined) delete next[id]
  else next[id] = on
  automationSettings.set({ highlightConnections: next })
}
