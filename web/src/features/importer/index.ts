/*
 * Importer feature (IMP-1..4, SSH-36): the import wizard + export dialog (overlay), the cross-module commands
 * `importer.open {format?}` and `importer.export` that the sessions tree / menus call, Sessions- and Tools-menu
 * entries, and the Settings → Import & Export section (SSH-config live sync, admin backup/restore).
 *
 * Formats: MobaXterm, PuTTY/KiTTY, OpenSSH ssh_config & known_hosts, Termius, mRemoteNG, Remmina, FileZilla, WinSCP,
 * SecureCRT, generic CSV and NexTerm JSON. No passwords are ever read from third-party files.
 */
import { lazy } from 'react'
import { Download, FolderInput } from 'lucide-react'
import { registerCommand, registerMenu, registerOverlay, registerSettingsSection, type MenuItem } from '@/app/registry'
import { ImporterOverlay } from './Overlay'
import { openExportDialog, openImportWizard } from './store'
import type { ExportFormat, ImportFormat } from './types'

registerOverlay({ id: 'importer', component: ImporterOverlay })

registerSettingsSection({
  id: 'importer',
  title: 'Import & Export',
  icon: FolderInput,
  order: 80,
  group: 'general',
  keywords: ['import', 'export', 'mobaxterm', 'putty', 'ssh config', 'termius', 'backup', 'restore', 'csv', 'migrate'],
  component: lazy(() => import('./SettingsSection')),
})

registerCommand<{ format?: ImportFormat } | undefined>({
  id: 'importer.open',
  title: 'Import sessions…',
  category: 'Sessions',
  icon: FolderInput,
  keywords: ['import', 'migrate', 'mobaxterm', 'putty', 'ssh config', 'termius', 'mremoteng', 'remmina', 'filezilla', 'winscp'],
  run: ({ args }) => openImportWizard(args && typeof args === 'object' ? args.format : undefined),
})

registerCommand<{ format?: ExportFormat; folderId?: string; connectionIds?: string[] } | undefined>({
  id: 'importer.export',
  title: 'Export sessions…',
  category: 'Sessions',
  icon: Download,
  keywords: ['export', 'backup', 'save sessions', 'ssh config', 'csv', 'json'],
  run: ({ args }) => openExportDialog(args && typeof args === 'object' ? args : undefined),
})

const menuItems = (): MenuItem[] => [
  { label: 'Import sessions…', icon: FolderInput, command: 'importer.open' },
  { label: 'Export sessions…', icon: Download, command: 'importer.export' },
]

registerMenu({ menu: 'sessions', order: 170, items: menuItems })
registerMenu({ menu: 'tools', order: 160, items: menuItems })
