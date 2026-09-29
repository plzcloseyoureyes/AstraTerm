/* Dialog state of the webproxy feature (rendered by Overlay.tsx), so commands and menus can open them. */
import { create } from 'zustand'
import type { OpenArgs, XpraMode } from './types'

export interface XpraDialogInit {
  connectionId?: string
  sessionId?: string
  command?: string
  mode?: XpraMode
}

interface WebproxyDialogs {
  open: OpenArgs | null
  xpra: XpraDialogInit | null
  manager: boolean
}

export const useWebproxyDialogs = create<WebproxyDialogs>(() => ({ open: null, xpra: null, manager: false }))

export function openWebDialog(init: OpenArgs = {}): void {
  useWebproxyDialogs.setState({ open: init })
}

export function closeWebDialog(): void {
  useWebproxyDialogs.setState({ open: null })
}

export function openXpraDialog(init: XpraDialogInit = {}): void {
  useWebproxyDialogs.setState({ xpra: init })
}

export function closeXpraDialog(): void {
  useWebproxyDialogs.setState({ xpra: null })
}

export function setManagerOpen(open: boolean): void {
  useWebproxyDialogs.setState({ manager: open })
}
