/*
 * Dialog state of the recordings feature (kept in a store so commands, menus and other features can open them).
 */
import { create } from 'zustand'

interface RecordingsUI {
  /** Share dialog for this runtime session. */
  shareSessionId: string | null
  shareTitle?: string
  /** Admin: send-message dialog. */
  messageSessionId: string | null
  messageTitle?: string
}

export const useRecordingsUI = create<RecordingsUI>(() => ({ shareSessionId: null, messageSessionId: null }))

export function openShareDialog(sessionId: string, title?: string): void {
  useRecordingsUI.setState({ shareSessionId: sessionId, shareTitle: title })
}

export function closeShareDialog(): void {
  useRecordingsUI.setState({ shareSessionId: null })
}

export function openMessageDialog(sessionId: string, title?: string): void {
  useRecordingsUI.setState({ messageSessionId: sessionId, messageTitle: title })
}

export function closeMessageDialog(): void {
  useRecordingsUI.setState({ messageSessionId: null })
}
