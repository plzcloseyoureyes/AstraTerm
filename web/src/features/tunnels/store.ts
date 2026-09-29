/*
 * Dialog state of the tunnels feature (rendered by the overlay registered in index.ts), so commands, menus and other
 * features can open the editor / port detection / import / session-forward dialogs.
 */
import { create } from 'zustand'
import type { NewTunnelArgs } from './types'

export interface EditorRequest {
  key: number
  mode: 'create' | 'edit'
  /** Tunnel being edited. */
  id?: string
  /** Prefill for new tunnels. */
  initial?: NewTunnelArgs
}

export interface DetectRequest {
  key: number
  connectionId?: string
  sessionId?: string
}

export interface ForwardsRequest {
  key: number
  connectionId: string
}

interface DialogsState {
  editor: EditorRequest | null
  detect: DetectRequest | null
  importer: { key: number } | null
  forwards: ForwardsRequest | null
}

let seq = 0

export const useTunnelDialogs = create<DialogsState>(() => ({ editor: null, detect: null, importer: null, forwards: null }))

export function openTunnelEditor(req: Omit<EditorRequest, 'key'>): void {
  useTunnelDialogs.setState({ editor: { ...req, key: ++seq } })
}

export function closeTunnelEditor(): void {
  useTunnelDialogs.setState({ editor: null })
}

export function openDetectPorts(target: { connectionId?: string; sessionId?: string } = {}): void {
  useTunnelDialogs.setState({ detect: { ...target, key: ++seq } })
}

export function closeDetectPorts(): void {
  useTunnelDialogs.setState({ detect: null })
}

export function openTunnelImport(): void {
  useTunnelDialogs.setState({ importer: { key: ++seq } })
}

export function closeTunnelImport(): void {
  useTunnelDialogs.setState({ importer: null })
}

export function openSessionForwards(connectionId: string): void {
  useTunnelDialogs.setState({ forwards: { connectionId, key: ++seq } })
}

export function closeSessionForwards(): void {
  useTunnelDialogs.setState({ forwards: null })
}

export function closeAllTunnelDialogs(): void {
  useTunnelDialogs.setState({ editor: null, detect: null, importer: null, forwards: null })
}
