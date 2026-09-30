/*
 * Term-transfer UI model (feature-specific types; SPEC §10: shared JSON types stay in src/api/types.ts).
 */

export type TransferProtocol = 'zmodem' | 'trzsz' | 'send'
export type TransferDirection = 'download' | 'upload'
type TransferPhase = 'running' | 'done' | 'error' | 'canceled'

/** One in-terminal transfer as shown on the terminal's transfer card. */
export interface TransferView {
  id: string
  protocol: TransferProtocol
  direction: TransferDirection
  phase: TransferPhase
  /** e.g. "Receiving with ZMODEM". */
  title: string
  /** Current file (remote name for downloads). */
  file?: string
  fileIndex?: number
  fileCount?: number | null
  fileBytes?: number
  fileSize?: number | null
  /** Progress units: bytes (default) or lines (text send). */
  unit: 'bytes' | 'lines'
  done: number
  total: number | null
  /** Overall completion 0..1 (monotonic; null while truly unknown: indeterminate bar). */
  pct?: number | null
  /** Smoothed rate in units per second. */
  rate: number
  startedAt: number
  finishedAt?: number
  /** Where downloads go ("Downloads", a folder name). */
  target?: string
  /** Summary / error text once finished. */
  message?: string
  /** Non-fatal notes (e.g. "line 12: no prompt within 15 s"). */
  warnings?: number
  canCancel: boolean
}

/** A button of a prompt card. `run` executes inside the click (or Enter key) handler: pickers need that gesture. */
export interface PromptAction {
  id: string
  label: string
  variant?: 'default' | 'secondary' | 'ghost' | 'destructive'
  run: (opts: { remember: boolean }) => void
}

export interface PromptView {
  id: string
  /** Icon / tone of the card. */
  kind: 'download' | 'upload' | 'error' | 'info'
  title: string
  message?: string
  /** Extra line (e.g. a warning about the session's character set). */
  note?: string
  actions: PromptAction[]
  /** Action run by Enter (index into actions). */
  primary: number
  /** Action run by Escape / Ctrl+C / the close button. */
  cancel: () => void
  /** Offer a "remember my choice" checkbox; its state is passed to the actions. */
  rememberLabel?: string
  remember?: boolean
  /** Files may be dropped on the card (upload prompts). */
  acceptDrop?: (dt: DataTransfer) => void
}

export type DropZoneId = 'sftp' | 'local' | 'trz' | 'rz' | 'paste'

export interface DropZone {
  id: DropZoneId
  label: string
  detail?: string
  disabled?: string
}

export interface DragView {
  zones: DropZone[]
  /** Zone under the pointer (null = the default zone). */
  hover: DropZoneId | null
  /** Why nothing can be dropped (e.g. not connected). */
  blocked?: string
}

/** Everything the overlay renders for one terminal view. */
export interface TermUi {
  tabId: string
  sessionId: string
  /** Element inside the terminal pane the overlay portals into. */
  mount: HTMLElement
  transfer?: TransferView
  prompt?: PromptView
  drag?: DragView
  /** Another view / window answers the current transfer on this session. */
  follower?: boolean
}

/** Upload of dropped / picked files through the Files API (progress toast). */
export interface UploadView {
  id: string
  label: string
  dest: string
  phase: 'preparing' | 'running' | 'done' | 'error' | 'canceled'
  files: number
  doneFiles: number
  current?: string
  bytes: number
  total: number
  rate: number
  error?: string
}
