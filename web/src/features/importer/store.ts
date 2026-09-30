/*
 * Dialog state of the importer feature (a zustand store so commands, menus and other features can open the wizard /
 * export dialog). Rendered by the overlay registered in index.ts.
 */
import { create } from 'zustand'
import type { ExportFormat, ImportFormat } from './types'

export interface ImporterDialogs {
  wizard: { key: number; format?: ImportFormat } | null
  export: { key: number; format?: ExportFormat; folderId?: string; connectionIds?: string[] } | null
}

let seq = 0

export const useImporterDialogs = create<ImporterDialogs>(() => ({ wizard: null, export: null }))

export function openImportWizard(format?: ImportFormat): void {
  useImporterDialogs.setState({ wizard: { key: ++seq, format } })
}

export function openExportDialog(opts?: { format?: ExportFormat; folderId?: string; connectionIds?: string[] }): void {
  useImporterDialogs.setState({ export: { key: ++seq, ...opts } })
}

export function closeImporterDialog(kind: keyof ImporterDialogs): void {
  useImporterDialogs.setState({ [kind]: null } as Partial<ImporterDialogs>)
}
