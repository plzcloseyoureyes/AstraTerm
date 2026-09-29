/*
 * UI state of the term-transfer feature (zustand): per terminal view the transfer card / prompt / drop overlay, the
 * uploads shown as progress toasts, and the dialogs (send file, conflicts, dangerous commands). Controllers write it,
 * the overlay (registerOverlay) renders it — portalled into each terminal pane.
 */
import { create } from 'zustand'
import type { DangerMatch } from './automation'
import type { DragView, PromptView, TermUi, TransferView, UploadView } from './types'
import type { ConflictPolicy } from './settings'

export interface ConflictRequest {
  id: string
  dest: string
  names: string[]
  resolve: (choice: Exclude<ConflictPolicy, 'ask'> | null, remember: boolean) => void
}

export interface DangerRequest {
  id: string
  title: string
  matches: DangerMatch[]
  resolve: (ok: boolean) => void
}

export interface SendDialogState {
  open: boolean
  tabId?: string
  /** File chosen before the dialog opened (drop on the dialog / command args). */
  file?: File
}

interface Store {
  terms: Record<string, TermUi>
  uploads: Record<string, UploadView>
  sendDialog: SendDialogState
  conflict: ConflictRequest | null
  danger: DangerRequest | null
}

export const useTransferStore = create<Store>(() => ({
  terms: {},
  uploads: {},
  sendDialog: { open: false },
  conflict: null,
  danger: null,
}))

function patchTerm(tabId: string, fn: (t: TermUi) => TermUi | null): void {
  useTransferStore.setState((s) => {
    const cur = s.terms[tabId]
    if (!cur) return s
    const next = fn(cur)
    if (next === cur) return s
    const terms = { ...s.terms }
    if (next) terms[tabId] = next
    else delete terms[tabId]
    return { terms }
  })
}

export function mountTerm(tabId: string, sessionId: string, mount: HTMLElement): void {
  useTransferStore.setState((s) => ({ terms: { ...s.terms, [tabId]: { tabId, sessionId, mount } } }))
}

export function unmountTerm(tabId: string): void {
  patchTerm(tabId, () => null)
}

export function setTransfer(tabId: string, transfer: TransferView | undefined): void {
  patchTerm(tabId, (t) => (t.transfer === transfer ? t : { ...t, transfer }))
}

export function setPrompt(tabId: string, prompt: PromptView | undefined): void {
  patchTerm(tabId, (t) => (t.prompt === prompt ? t : { ...t, prompt }))
}

export function setPromptRemember(tabId: string, remember: boolean): void {
  patchTerm(tabId, (t) => (t.prompt ? { ...t, prompt: { ...t.prompt, remember } } : t))
}

export function setDrag(tabId: string, drag: DragView | undefined): void {
  patchTerm(tabId, (t) => {
    if (t.drag === drag) return t
    if (t.drag && drag && t.drag.hover === drag.hover && t.drag.blocked === drag.blocked && t.drag.zones === drag.zones) return t
    return { ...t, drag }
  })
}

export function setFollower(tabId: string, follower: boolean): void {
  patchTerm(tabId, (t) => (!!t.follower === follower ? t : { ...t, follower }))
}

export function getTerm(tabId: string): TermUi | undefined {
  return useTransferStore.getState().terms[tabId]
}

// --- uploads -----------------------------------------------------------------------------------------------------------

export function putUpload(u: UploadView): void {
  useTransferStore.setState((s) => ({ uploads: { ...s.uploads, [u.id]: u } }))
}

export function dropUpload(id: string): void {
  useTransferStore.setState((s) => {
    if (!s.uploads[id]) return s
    const uploads = { ...s.uploads }
    delete uploads[id]
    return { uploads }
  })
}

// --- dialogs -----------------------------------------------------------------------------------------------------------

export function openSendDialog(tabId: string, file?: File): void {
  useTransferStore.setState({ sendDialog: { open: true, tabId, file } })
}

export function closeSendDialog(): void {
  useTransferStore.setState((s) => ({ sendDialog: { ...s.sendDialog, open: false, file: undefined } }))
}

let seq = 0

/** Ask how to handle names that already exist in the destination folder. */
export function askConflict(dest: string, names: string[]): Promise<{ choice: Exclude<ConflictPolicy, 'ask'> | null; remember: boolean }> {
  return new Promise((resolve) => {
    useTransferStore.getState().conflict?.resolve(null, false)
    let settled = false
    const req: ConflictRequest = {
      id: `c${++seq}`,
      dest,
      names,
      resolve: (choice, remember) => {
        if (settled) return
        settled = true
        if (useTransferStore.getState().conflict === req) useTransferStore.setState({ conflict: null })
        resolve({ choice, remember })
      },
    }
    useTransferStore.setState({ conflict: req })
  })
}

/** Ask whether text the automation guard flagged may be sent anyway. */
export function askDangerous(title: string, matches: DangerRequest['matches']): Promise<boolean> {
  return new Promise((resolve) => {
    useTransferStore.getState().danger?.resolve(false)
    let settled = false
    const req: DangerRequest = {
      id: `d${++seq}`,
      title,
      matches,
      resolve: (ok) => {
        if (settled) return
        settled = true
        if (useTransferStore.getState().danger === req) useTransferStore.setState({ danger: null })
        resolve(ok)
      },
    }
    useTransferStore.setState({ danger: req })
  })
}
