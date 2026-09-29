/*
 * Term-transfer feature (Stage 2, SPEC §10.3; RESEARCH FILE-19, FILE-20, FILE-22, CC-8, §3.20):
 *
 *   trzsz     `trz` / `tsz` (+ -d folders, -b binary) answered in the browser: folder (File System Access API) or
 *             browser downloads (folders as ZIP), progress card, Ctrl+C / Cancel
 *   ZMODEM    `sz` / `rz` (lrzsz) via zmodem.js: detection with a retraction grace period, save target, file
 *             picker / drop for rz, windowed ZCRCQ/ZACK uploads, cancel (CAN sequence), protocol errors handled
 *   drops     OS files dropped on a terminal: SFTP upload into the shell's folder (SSH, Files API, progress toast),
 *             copy to the host + type the path (local shells), trz, rz, or paste a small text file
 *   send file `terminal.sendFile` (CC-8): text line by line (server pacer of the automation module when present,
 *             else a background-safe browser pacer) with per-line / per-char delay and wait-for-prompt, or raw binary
 *
 * Everything is a terminal plugin (registerTerminalPlugin) plus an overlay portalled into each terminal pane.
 * Settings section `termTransfer`.
 */
import { lazy } from 'react'
import { ArrowLeftRight, ClipboardPaste, FileUp, HardDriveUpload, Send, Upload } from 'lucide-react'
import { toast } from 'sonner'
import { runCommand } from '@/app/commands'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerSettingsSection,
  registerTerminalPlugin,
  type CommandContext,
  type MenuItem,
} from '@/app/registry'
import type { TransferController } from './controller'
import { activeController, getController, listControllers, localFilesAllowed } from './instances'
import { openSendDialog } from './store'
import { currentSettings } from './settings'
import { TermTransferOverlay } from './ui/Overlay'
import type { DropZoneId } from './types'

// The controller (with trzsz and zmodem.js) loads with the first terminal.
let controllerModule: Promise<typeof import('./controller')> | null = null

registerTerminalPlugin({
  id: 'termtransfer',
  // Early: the output filter should see the raw stream before other filters rewrite it.
  order: 10,
  setup: (term, ctx) => {
    let disposed = false
    let c: TransferController | null = null
    controllerModule ??= import('./controller')
    controllerModule
      .then((m) => {
        if (!disposed) c = m.createController(term, ctx)
      })
      .catch((err) => {
        controllerModule = null
        console.error('[termtransfer] cannot load the transfer module', err)
      })
    return () => {
      disposed = true
      c?.dispose()
    }
  },
})

registerOverlay({ id: 'termtransfer', component: TermTransferOverlay, order: 60 })

registerSettingsSection({
  id: 'termTransfer',
  title: 'File transfer (terminal)',
  icon: ArrowLeftRight,
  order: 26,
  keywords: ['trzsz', 'trz', 'tsz', 'zmodem', 'rz', 'sz', 'lrzsz', 'drop', 'upload', 'download', 'send file', 'ascii upload', 'paced', 'binary'],
  component: lazy(() => import('./ui/SettingsSection')),
})

// ---------------------------------------------------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------------------------------------------------

interface TargetArgs {
  tabId?: string
  sessionId?: string
}

function argsOf(args: unknown): TargetArgs & Record<string, unknown> {
  return args && typeof args === 'object' ? (args as TargetArgs & Record<string, unknown>) : {}
}

/** The terminal a command targets: explicit tab / session, else the active tab. */
function targetOf(ctx: CommandContext): TransferController | undefined {
  const a = argsOf(ctx.args)
  if (typeof a.tabId === 'string') return getController(a.tabId)
  if (typeof a.sessionId === 'string') {
    const list = listControllers().filter((c) => c.sessionId === a.sessionId)
    return list.find((c) => c.tabId === ctx.activeTab?.id) ?? list[0]
  }
  return getController(ctx.activeTab?.id) ?? activeController()
}

const usable = () => {
  const c = activeController()
  return !!c && !c.readOnly
}

registerCommand<TargetArgs & { file?: File }>({
  id: 'terminal.sendFile',
  title: 'Send File to Session…',
  category: 'Terminal',
  icon: Send,
  keywords: ['ascii upload', 'send text file', 'send binary file', 'paced', 'type file', 'upload script'],
  description: 'Type a local file into the terminal: text line by line (with delays / wait for prompt) or raw binary.',
  when: usable,
  run: (ctx) => {
    const c = targetOf(ctx)
    if (!c) {
      toast.info('Open a terminal first')
      return
    }
    if (c.readOnly) {
      toast.info('This terminal is read-only')
      return
    }
    const file = argsOf(ctx.args).file
    openSendDialog(c.tabId, typeof File !== 'undefined' && file instanceof File ? file : undefined)
  },
})

function registerUpload(id: string, title: string, via: (c: TransferController) => DropZoneId | null, keywords: string[], icon: typeof Upload) {
  registerCommand<TargetArgs>({
    id,
    title,
    category: 'Terminal',
    icon,
    keywords,
    when: () => {
      const c = activeController()
      return !!c && !c.readOnly && !!via(c)
    },
    run: (ctx) => {
      const c = targetOf(ctx)
      const v = c ? via(c) : null
      if (!c || !v) {
        toast.info('Not available for this terminal')
        return
      }
      c.pickAndUpload(v)
    },
  })
}

registerUpload(
  'termtransfer.upload',
  'Upload Files to the Terminal’s Folder…',
  (c) => (c.protocol === 'ssh' ? 'sftp' : c.protocol === 'local' && localFilesAllowed() ? 'local' : null),
  ['sftp', 'upload', 'current directory', 'cwd', 'drop'],
  HardDriveUpload,
)
registerUpload('termtransfer.uploadTrz', 'Upload Files with trz (trzsz)…', () => (currentSettings().trzsz ? 'trz' : null), ['trzsz', 'trz', 'upload'], Upload)
registerUpload('termtransfer.uploadRz', 'Upload Files with rz (ZMODEM)…', () => (currentSettings().zmodem ? 'rz' : null), ['zmodem', 'rz', 'lrzsz', 'upload'], FileUp)
registerUpload('termtransfer.pasteFile', 'Paste a Text File’s Contents…', () => 'paste', ['paste file', 'insert file'], ClipboardPaste)

registerCommand({
  id: 'termtransfer.cancel',
  title: 'Cancel Terminal File Transfer',
  category: 'Terminal',
  hidden: true,
  when: () => !!activeController()?.busy,
  run: (ctx) => targetOf(ctx)?.cancelActive(),
})

registerCommand({
  id: 'termtransfer.settings',
  title: 'File Transfer (Terminal) Settings',
  category: 'Terminal',
  icon: ArrowLeftRight,
  keywords: ['trzsz', 'zmodem', 'drop'],
  run: () => void runCommand('settings.open', { section: 'termTransfer' }),
})

// ---------------------------------------------------------------------------------------------------------------------
// menus
// ---------------------------------------------------------------------------------------------------------------------

function uploadItems(c: TransferController | undefined, tabId: string | undefined): MenuItem[] {
  const args = tabId ? { tabId } : undefined
  const s = currentSettings()
  const items: MenuItem[] = []
  const folder = c?.protocol === 'ssh' ? 'sftp' : c?.protocol === 'local' && localFilesAllowed() ? 'local' : null
  if (folder) items.push({ label: folder === 'sftp' ? 'To the current folder (SFTP)…' : 'Copy to the current folder…', icon: HardDriveUpload, command: 'termtransfer.upload', args })
  if (s.trzsz) items.push({ label: 'With trz (trzsz)…', icon: Upload, command: 'termtransfer.uploadTrz', args })
  if (s.zmodem) items.push({ label: 'With rz (ZMODEM)…', icon: FileUp, command: 'termtransfer.uploadRz', args })
  items.push({ label: 'Paste a text file’s contents…', icon: ClipboardPaste, command: 'termtransfer.pasteFile', args })
  return items
}

registerContextMenu({
  id: 'termtransfer.terminal',
  target: 'terminal',
  group: 'files',
  order: 45,
  items: (ctx) => {
    const c = getController(ctx.tabId)
    if (!c || c.readOnly) return []
    const disabled = !c.isConnected() || c.busy
    return [
      { label: 'Send file…', icon: Send, command: 'terminal.sendFile', args: { tabId: ctx.tabId }, disabled },
      { type: 'submenu', label: 'Upload files', icon: Upload, disabled, items: () => uploadItems(c, ctx.tabId) },
    ]
  },
})

registerMenu({
  id: 'termtransfer.terminal-menu',
  menu: 'terminal',
  order: 170,
  items: () => [
    { label: 'Send File to Session…', icon: Send, command: 'terminal.sendFile' },
    { type: 'submenu', label: 'Upload Files', icon: Upload, items: () => uploadItems(activeController(), undefined) },
    { label: 'File Transfer Settings…', icon: ArrowLeftRight, command: 'termtransfer.settings' },
  ],
})
