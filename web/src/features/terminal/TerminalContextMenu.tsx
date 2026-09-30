/*
 * Terminal context menu (registry target 'terminal' + built-in actions). Rendered into the terminal's own document so
 * it also works when the tab lives in a dockview pop-out window.
 */
import * as React from 'react'
import { ContextMenu as CM } from 'radix-ui'
import {
  Check,
  ChevronRight,
  ClipboardPaste,
  Copy,
  CopyPlus,
  Download,
  Eraser,
  FileText,
  FolderOpen,
  History,
  ListOrdered,
  PanelBottom,
  PanelRight,
  Pause,
  RefreshCw,
  RotateCcw,
  Search,
  TextSelect,
  Type,
  Video,
  X,
  Zap,
  Bell,
} from 'lucide-react'
import { commands, getContextMenuSections, joinSections, type MenuItem } from '@/app/registry'
import { getKeybindings, isCommandEnabled, runCommand } from '@/app/commands'
import { menuContentClass, menuItemClass, menuLabelClass, menuSeparatorClass, menuShortcutClass } from '@/components/ui/dropdown-menu'
import { formatKeybinding } from '@/lib/keys'
import { cn } from '@/lib/utils'
import { closeTab, getTabParams, updateTabParams } from '@/stores/workspace'
import { useClipboardHistory } from './clipboard'
import type { TerminalController } from './controller'
import { duplicateSession } from './open'
import { terminalSettings } from './settings'
import type { TerminalTabParams } from './types'

const kb = (id: string): string | undefined => getKeybindings(id)[0]

/** Built-in terminal menu items + contributions of other features (registerContextMenu target 'terminal'). */
function buildTerminalMenu(ctrl: TerminalController): MenuItem[] {
  const info = ctrl.info()
  const tabId = ctrl.tabId
  const selection = ctrl.getSelection()
  const ro = info.readOnly
  const running = ctrl.isRunning()
  const ended = info.state === 'closed' || info.state === 'disconnected' || info.state === 'error' || info.state === 'gone'
  const params = getTabParams<TerminalTabParams>(tabId) ?? { sessionId: ctrl.sessionId }
  const hasHistory = useClipboardHistory.getState().entries.length > 0
  const silence = terminalSettings.get().silenceSeconds

  const edit: MenuItem[] = [
    { label: 'Copy', icon: Copy, run: () => void ctrl.copySelection(), disabled: !selection, shortcut: kb('terminal.copy') },
    { label: 'Paste', icon: ClipboardPaste, run: () => void ctrl.pasteFromClipboard(), disabled: ro, shortcut: kb('terminal.paste') },
    { label: 'Paste line by line…', icon: ListOrdered, run: () => void ctrl.pasteFromClipboard({ mode: 'paced' }), disabled: ro },
    { label: 'Paste from history…', icon: History, run: () => ctrl.openClipboardHistory(), disabled: ro || !hasHistory, shortcut: kb('terminal.clipboardHistory') },
    { label: 'Select all', icon: TextSelect, run: () => ctrl.selectAll(), shortcut: kb('terminal.selectAll') },
  ]
  const view: MenuItem[] = [
    { label: 'Find…', icon: Search, run: () => ctrl.openSearch(), shortcut: kb('terminal.find') },
    { label: 'Clear scrollback', icon: Eraser, run: () => ctrl.clear(), shortcut: kb('terminal.clear') },
    { label: 'Reset terminal', icon: RotateCcw, run: () => ctrl.reset(), shortcut: kb('terminal.reset') },
    {
      type: 'submenu',
      label: 'Font size',
      icon: Type,
      items: [
        { label: 'Zoom in', run: () => ctrl.zoomBy(1), shortcut: kb('terminal.zoomIn') },
        { label: 'Zoom out', run: () => ctrl.zoomBy(-1), shortcut: kb('terminal.zoomOut') },
        { label: `Reset (${terminalSettings.get().fontSize}px)`, run: () => ctrl.zoomReset(), shortcut: kb('terminal.zoomReset') },
      ],
    },
    { label: 'Pause output', icon: Pause, checked: info.paused, run: () => ctrl.togglePause(), shortcut: kb('terminal.pauseOutput') },
    { label: 'Fixed size (80×24)', checked: ctrl.isFixedSize(), run: () => ctrl.toggleFixedSize(), disabled: ro },
    {
      type: 'submenu',
      label: 'Save output',
      icon: Download,
      items: [
        { label: 'As text (.txt)', run: () => ctrl.saveOutput('text'), shortcut: kb('terminal.saveOutput') },
        { label: 'As HTML with colours (.html)', run: () => ctrl.saveOutput('html') },
        { label: 'Full session log from the server', run: () => void ctrl.saveServerLog() },
      ],
    },
    {
      type: 'submenu',
      label: 'Copy all',
      icon: Copy,
      items: [
        { label: 'As text', run: () => void ctrl.copyAll('text') },
        { label: 'As HTML with colours', run: () => void ctrl.copyAll('html') },
      ],
    },
  ]
  if (ctrl.hasPromptMarks()) {
    view.push({
      type: 'submenu',
      label: 'Jump to prompt',
      items: [
        { label: 'Previous prompt', run: () => ctrl.jumpToPrompt(-1), shortcut: kb('terminal.prevPrompt') },
        { label: 'Next prompt', run: () => ctrl.jumpToPrompt(1), shortcut: kb('terminal.nextPrompt') },
      ],
    })
  }
  const layout: MenuItem[] = [
    { label: 'Split right', icon: PanelRight, run: () => void duplicateSession(tabId, { position: 'right' }) },
    { label: 'Split down', icon: PanelBottom, run: () => void duplicateSession(tabId, { position: 'below' }) },
    { label: 'Duplicate session', icon: CopyPlus, run: () => void duplicateSession(tabId) },
  ]
  const session: MenuItem[] = [
    { label: info.state === 'gone' || info.state === 'closed' ? 'Start new session' : 'Reconnect', icon: RefreshCw, run: () => ctrl.reconnect(), disabled: !ended },
    {
      type: 'submenu',
      label: 'Send signal',
      icon: Zap,
      disabled: !running || ro,
      items: (['INT', 'TERM', 'HUP', 'KILL'] as const).map((name) => ({ label: `SIG${name}`, run: () => ctrl.sendSignal(name) })),
    },
    { label: 'Send break', run: () => ctrl.sendBreak(), disabled: !running || ro },
    { label: info.recording ? 'Stop recording' : 'Start recording', icon: Video, run: () => void ctrl.setRecording(!info.recording), disabled: ended },
    { label: info.logging ? 'Stop logging' : 'Start logging', icon: FileText, run: () => void ctrl.setLogging(!info.logging), disabled: ended },
    {
      type: 'submenu',
      label: 'Monitor',
      icon: Bell,
      items: [
        {
          label: 'Notify on activity',
          checked: !!params.monitorActivity,
          run: () => updateTabParams<TerminalTabParams>(tabId, { monitorActivity: !params.monitorActivity || undefined }),
        },
        {
          label: `Notify after ${silence}s of silence`,
          checked: !!params.monitorSilence,
          run: () => updateTabParams<TerminalTabParams>(tabId, { monitorSilence: !params.monitorSilence || undefined }),
        },
      ],
    },
  ]
  const files: MenuItem[] = isCommandEnabled('files.openForSession')
    ? [{ label: 'Open files here', icon: FolderOpen, run: () => ctrl.openFilesHere(), disabled: info.state === 'gone' }]
    : []
  const close: MenuItem[] = [{ label: 'Close', icon: X, run: () => void closeTab(tabId), shortcut: kb('workspace.closeTab') }]
  // Contributions (registerContextMenu target 'terminal') join the built-in section named by their `group`;
  // ungrouped ones follow the built-in sections, before Close.
  const builtin: Record<TerminalMenuGroup, MenuItem[]> = { edit, selection: [], view, layout, session, files, close }
  const extra: MenuItem[][] = []
  for (const sec of getContextMenuSections('terminal', { tabId, sessionId: ctrl.sessionId, session: ctrl.session(), selection })) {
    if (sec.group && sec.group in builtin) builtin[sec.group as TerminalMenuGroup].push(...sec.items)
    else extra.push(sec.items)
  }
  const b = builtin
  return joinSections([b.edit, b.selection, b.view, b.layout, b.session, b.files, ...extra, b.close])
}

/** Built-in sections of the terminal context menu, in display order (see ContextMenuContribution.group). */
type TerminalMenuGroup = 'edit' | 'selection' | 'view' | 'layout' | 'session' | 'files' | 'close'


// ---------------------------------------------------------------------------------------------------------------------
// Rendering (a portal-aware variant of the shell's MenuItems)
// ---------------------------------------------------------------------------------------------------------------------

function MenuList({ items, container }: { items: MenuItem[]; container: HTMLElement | undefined }) {
  return (
    <>
      {items.map((item, i) => {
        if (item.type === 'separator') return <CM.Separator key={`sep-${i}`} className={menuSeparatorClass} />
        if (item.type === 'label')
          return (
            <CM.Label key={`lbl-${i}`} className={menuLabelClass}>
              {item.label}
            </CM.Label>
          )
        if (item.type === 'submenu') {
          let sub: MenuItem[] = []
          try {
            sub = typeof item.items === 'function' ? item.items() : item.items
          } catch (err) {
            console.error('[terminal] submenu failed', err)
          }
          const Icon = item.icon
          return (
            <CM.Sub key={`sub-${i}-${item.label}`}>
              <CM.SubTrigger disabled={item.disabled || !sub.length} className={cn(menuItemClass, 'data-[state=open]:bg-accent')}>
                {Icon ? <Icon /> : <span className="size-3.5" />}
                <span className="truncate">{item.label}</span>
                <ChevronRight className="ml-auto size-3.5" />
              </CM.SubTrigger>
              <CM.Portal container={container}>
                <CM.SubContent className={menuContentClass} sideOffset={2} alignOffset={-4}>
                  <MenuList items={sub} container={container} />
                </CM.SubContent>
              </CM.Portal>
            </CM.Sub>
          )
        }
        const cmd = item.command ? commands.get(item.command) : undefined
        const enabled = item.command ? isCommandEnabled(item.command) : !!item.run
        const binding = item.shortcut ?? (item.command ? getKeybindings(item.command)[0] : undefined)
        const Icon = item.icon ?? cmd?.icon
        return (
          <CM.Item
            key={item.id ?? `${item.command ?? item.label}-${i}`}
            disabled={!!item.disabled || !enabled}
            data-variant={item.danger ? 'destructive' : undefined}
            className={menuItemClass}
            onSelect={() => {
              if (item.command) void runCommand(item.command, item.args, { source: 'context-menu' })
              else item.run?.()
            }}
          >
            {item.checked !== undefined ? (
              <span className="flex size-3.5 items-center justify-center">{item.checked && <Check className="size-3.5 text-foreground!" />}</span>
            ) : Icon ? (
              <Icon />
            ) : (
              <span className="size-3.5" />
            )}
            <span className="truncate">{item.label || cmd?.title}</span>
            {binding && <span className={menuShortcutClass}>{formatKeybinding(binding)}</span>}
          </CM.Item>
        )
      })}
    </>
  )
}

export function TerminalContextMenu({
  ctrl,
  children,
  container,
  disabled,
}: {
  ctrl: TerminalController | null
  children: React.ReactElement
  /** Portal container inside the terminal's document (pop-out windows). */
  container: HTMLElement | undefined
  disabled?: boolean
}) {
  const [items, setItems] = React.useState<MenuItem[]>([])
  return (
    <CM.Root
      onOpenChange={(open) => {
        if (open && ctrl) {
          try {
            setItems(buildTerminalMenu(ctrl))
          } catch (err) {
            console.error('[terminal] context menu failed', err)
            setItems([])
          }
        }
      }}
    >
      <CM.Trigger asChild disabled={disabled || !ctrl}>
        {children}
      </CM.Trigger>
      <CM.Portal container={container}>
        <CM.Content
          className={cn(menuContentClass, 'max-h-(--radix-context-menu-content-available-height)')}
          onCloseAutoFocus={(e) => {
            e.preventDefault()
            ctrl?.focus()
          }}
        >
          {items.length ? <MenuList items={items} container={container} /> : <div className="px-2 py-1.5 text-sm text-muted-foreground">No actions</div>}
        </CM.Content>
      </CM.Portal>
    </CM.Root>
  )
}
