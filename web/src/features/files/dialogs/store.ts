/*
 * Feature dialogs (rendered by the files overlay, registerOverlay): permissions, properties, checksum, compress,
 * search, folder compare, preview and the upload / copy conflict question. Any module (commands, context menus,
 * the upload engine) opens them through this store.
 */
import { create } from 'zustand'
import type { FileEntry } from '@/api/types'
import type { ConflictPolicy, FsContext } from '../types'

export interface CompareSide {
  ctx: FsContext
  path: string
}

export type FilesDialog =
  | { kind: 'permissions'; ctx: FsContext; entries: FileEntry[] }
  | { kind: 'properties'; ctx: FsContext; entry: FileEntry }
  | { kind: 'checksum'; ctx: FsContext; entry: FileEntry }
  | { kind: 'compress'; ctx: FsContext; entries: FileEntry[]; dir: string }
  | { kind: 'search'; ctx: FsContext; path: string; viewId?: string }
  | { kind: 'compare'; left: CompareSide; right?: CompareSide }
  | { kind: 'preview'; ctx: FsContext; entry: FileEntry; siblings: FileEntry[] }
  | {
      kind: 'conflict'
      names: string[]
      destLabel: string
      verb: 'upload' | 'copy' | 'move'
      resolve: (choice: ConflictPolicy | null) => void
    }

export type OpenDialog = FilesDialog & { id: number }

interface DialogStore {
  stack: OpenDialog[]
}

export const useFilesDialogs = create<DialogStore>(() => ({ stack: [] }))

let nextId = 1

export function openFilesDialog(d: FilesDialog): number {
  const id = nextId++
  useFilesDialogs.setState((s) => ({ stack: [...s.stack, { ...d, id } as OpenDialog] }))
  return id
}

export function closeFilesDialog(id: number): void {
  const d = useFilesDialogs.getState().stack.find((x) => x.id === id)
  if (d?.kind === 'conflict') d.resolve(null)
  useFilesDialogs.setState((s) => ({ stack: s.stack.filter((x) => x.id !== id) }))
}

/** Replace a dialog's data (e.g. the preview moving to the next image). */
export function updateFilesDialog(id: number, patch: Partial<FilesDialog>): void {
  useFilesDialogs.setState((s) => ({ stack: s.stack.map((x) => (x.id === id ? ({ ...x, ...patch } as OpenDialog) : x)) }))
}

/**
 * Ask what to do with items that already exist at the destination. Resolves the chosen policy, or null (cancel).
 */
export function askConflict(names: string[], destLabel: string, verb: 'upload' | 'copy' | 'move'): Promise<ConflictPolicy | null> {
  return new Promise((resolve) => {
    let settled = false
    const id = openFilesDialog({
      kind: 'conflict',
      names,
      destLabel,
      verb,
      resolve: (choice) => {
        if (settled) return
        settled = true
        useFilesDialogs.setState((s) => ({ stack: s.stack.filter((x) => x.id !== id) }))
        resolve(choice)
      },
    })
  })
}

export function closeAllFilesDialogs(): void {
  for (const d of useFilesDialogs.getState().stack) if (d.kind === 'conflict') d.resolve(null)
  useFilesDialogs.setState({ stack: [] })
}
