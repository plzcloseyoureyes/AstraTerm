/*
 * Requests for the session editor and folder dialog. Commands, menus and the sidebar open dialogs through these
 * functions; <SessionDialogs/> (dialogs/host.tsx) renders them.
 */
import { create } from 'zustand'
import { toast } from 'sonner'
import type { Folder } from '@/api/types'
import type { ConnectionDraft } from '../types'

export interface EditorRequest {
  key: number
  mode: 'create' | 'edit'
  /** Connection being edited (edit mode). */
  connectionId?: string
  /** Pre-filled values (create mode): quick-connect "Save as session…", templates. */
  initial?: ConnectionDraft
  /** Folder of a new session. */
  folderId?: string | null
  /** Protocol pre-selected in create mode. */
  protocol?: string
  /** Initial tab (e.g. 'bookmark'). */
  tab?: string
}

export interface FolderRequest {
  key: number
  mode: 'create' | 'edit'
  folderId?: string
  /** Parent of a new folder. */
  parentId?: string | null
  /** Field to focus first. */
  focus?: 'name' | 'color' | 'icon'
  /** Settled with the saved folder, or null when cancelled. */
  resolve?: (folder: Folder | null) => void
}

interface DialogsState {
  editor: EditorRequest | null
  /** The open editor has unsaved changes (set by the editor). */
  editorDirty: boolean
  folder: FolderRequest | null
}

export const useSessionDialogs = create<DialogsState>(() => ({ editor: null, editorDirty: false, folder: null }))

let seq = 0

/** Open the session editor. Ignored (with a hint) while another editor has unsaved changes. */
export function openSessionEditor(req: Omit<EditorRequest, 'key'>): boolean {
  const st = useSessionDialogs.getState()
  if (st.editor && st.editorDirty) {
    toast.info('Finish or cancel the open session editor first')
    return false
  }
  useSessionDialogs.setState({ editor: { ...req, key: ++seq }, editorDirty: false })
  return true
}

export function closeSessionEditor(): void {
  useSessionDialogs.setState({ editor: null, editorDirty: false })
}

export function setEditorDirty(dirty: boolean): void {
  if (useSessionDialogs.getState().editorDirty !== dirty) useSessionDialogs.setState({ editorDirty: dirty })
}

/** Open the folder dialog; resolves with the saved folder (null when cancelled or replaced). */
export function openFolderDialog(req: Omit<FolderRequest, 'key' | 'resolve'>): Promise<Folder | null> {
  return new Promise((resolve) => {
    const prev = useSessionDialogs.getState().folder
    prev?.resolve?.(null)
    useSessionDialogs.setState({ folder: { ...req, key: ++seq, resolve } })
  })
}

export function closeFolderDialog(result: Folder | null = null): void {
  const cur = useSessionDialogs.getState().folder
  if (!cur) return
  useSessionDialogs.setState({ folder: null })
  cur.resolve?.(result)
}

/** Drop every open dialog (sign-out). */
export function closeAllSessionDialogs(): void {
  closeFolderDialog(null)
  closeSessionEditor()
}
