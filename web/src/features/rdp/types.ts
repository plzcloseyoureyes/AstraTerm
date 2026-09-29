/*
 * RDP feature types (SPEC §10: feature-specific types live next to the feature). The shared RdpTicket / GuacdStatus
 * types are in src/api/types.ts; the backend returns supersets of them (internal/rdp handlers.go, guacd.go).
 */
import type { GuacdStatus, Protocol, RdpTicket } from '@/api/types'
import type { QuickSpec } from '@/features/terminal/types'

export type RdpEngine = 'ironrdp' | 'guacd'

/**
 * How the remote desktop fills the tab:
 *   resize  the remote resolution follows the tab (display control / guacd display-update) — default
 *   fit     the desktop keeps its resolution and is scaled to fit the tab
 *   none    1:1 pixels, scrollbars when larger than the tab
 */
export type RdpScaling = 'resize' | 'fit' | 'none'

/** Params of a dock tab of kind "rdp" (JSON-serialisable, persisted with the layout). */
export interface RdpTabParams {
  sessionId: string
  protocol?: Protocol
  connectionId?: string
  /** Quick-connect spec without secrets (duplicate / reopen of unsaved sessions). */
  quick?: QuickSpec
  color?: string
  title?: string
  /** Per-tab scaling (falls back to the viewer settings). */
  scaling?: RdpScaling
  /** Engine override for this tab ("reconnect with guacd / IronRDP"). */
  engine?: RdpEngine
  /** Hyper-V VM id entered when the connection had none (security "vmconnect"). */
  vmId?: string
  /** An administrator's read-only view of another user's session (closing it leaves the session running). */
  shadow?: boolean
}

/** POST /api/sessions/{id}/rdp-ticket answer (superset of the shared RdpTicket). */
export interface RdpTicketInfo extends RdpTicket {
  engine: RdpEngine
  expiresIn: number
  host: string
  port: number
  dpi: number
  fixedSize: boolean
  resizeMethod: 'display-update' | 'reconnect' | 'none'
  security: 'any' | 'nla' | 'tls' | 'rdp' | 'vmconnect'
  enableCredssp: boolean
  preConnectionBlob?: string
  clipboard: boolean
  audio: boolean
  microphone: boolean
  drive: boolean
  driveName?: string
  printing: boolean
  colorDepth: number
  serverLayout?: string
  /** Route the backend dials through: "ssh" | "jump" | "proxy" | "proxy-command". */
  via?: string
  /** Reconnect automatically after an unexpected disconnection (options.autoReconnect). */
  autoReconnect: boolean
  /** A shadow ticket: the view sends no input. */
  readOnly?: boolean
  /** The connection records sessions (only guacd sessions are recorded, by NexTerm). */
  recording?: boolean
}

export interface RdpTicketRequest {
  width?: number
  height?: number
  dpi?: number
  engine?: RdpEngine
  preconnectionBlob?: string
  /** Ask for an administrator's read-only view of another user's session. */
  shadow?: boolean
}

/** GET /api/guacd/status (admins also get address / sidecar). */
export interface GuacdStatusInfo extends GuacdStatus {
  defaultEngine: RdpEngine
  address?: string
  source?: 'flag' | 'settings' | 'sidecar' | ''
  error?: string
  sidecar?: SidecarStatus
}

export interface SidecarStatus {
  dockerAvailable: boolean
  dockerVersion?: string
  exists: boolean
  running: boolean
  managed: boolean
  container: string
  image: string
  address: string
  active: boolean
  error?: string
}

/** Admin settings stored globally under settings key "rdp" (PUT /api/admin/settings). */
export interface RdpGlobalSettings {
  guacdAddress?: string
  defaultEngine?: RdpEngine
  guacdSidecar?: boolean
  guacdDataPath?: string
  guacdForwardHost?: string
}

/** POST /api/{connections|sessions}/{id}/launch-native answer. */
export interface LaunchNativeResult {
  client: string
  forwarded?: string
  passwordInjected: boolean
}

/** Viewer lifecycle as shown by the tab. */
export type ViewerStatus = 'idle' | 'loading' | 'connecting' | 'authenticating' | 'connected' | 'disconnected' | 'error' | 'gone'

/** Machine-readable reasons the UI offers specific actions for. */
export type ViewerErrorCode =
  | 'guacd_unavailable'
  | 'unsupported_security'
  | 'credentials'
  | 'locked'
  | 'gone'
  | 'network'
  | 'unsupported_browser'
  | 'shadow_unavailable'
  | 'other'

export interface FileTransfer {
  id: string
  name: string
  direction: 'upload' | 'download'
  size: number
  done: number
  state: 'running' | 'done' | 'error'
  error?: string
}

/** Live state of one RDP tab (drives the toolbar, overlays and status bar). */
export interface ViewerState {
  status: ViewerStatus
  engine?: RdpEngine
  /** Short human-readable detail (connection step, error or end reason). */
  message?: string
  errorCode?: ViewerErrorCode
  /** The last failure was a rejected logon (offer to edit the credentials). */
  authFailed?: boolean
  /** Remote desktop size in remote pixels. */
  desktop?: { width: number; height: number }
  scaling: RdpScaling
  /** The remote clipboard changed but could not be copied automatically (click to copy). */
  remoteClipboardPending?: boolean
  /** Last text received from the remote clipboard (guacd engine). */
  remoteClipboardText?: string
  clipboardEnabled: boolean
  fullscreen: boolean
  keyboardLocked: boolean
  focused: boolean
  /** Virtual drive transfers (guacd engine). */
  transfers: FileTransfer[]
  driveEnabled: boolean
  driveName?: string
  /** The engine can resize the remote display. */
  canResize: boolean
  via?: string
  destination?: string
  /** Epoch ms of the next automatic reconnection attempt (auto-reconnect). */
  reconnectAt?: number
  /** Number of the pending automatic reconnection attempt. */
  reconnectAttempt?: number
  /** A read-only view (administrator shadowing another user's session). */
  readOnly?: boolean
  /** The session is being recorded (guacd). */
  recording?: boolean
}

/** A recorded guacd session (GET /api/rdp/recordings): a Guacamole protocol stream played with SessionRecording. */
export interface RdpRecording {
  id: string
  ownerId: string
  sessionId: string
  connectionId?: string
  title: string
  kind: 'guac'
  size: number
  width: number
  height: number
  startedAt: string
  endedAt?: string
}

/** A key combination: KeyboardEvent.code values (IronRDP scancodes) and X11 keysyms (guacd), pressed in order. */
export interface KeyCombo {
  id: string
  label: string
  codes: string[]
  keysyms: number[]
}

/** The engine-independent controller of one RDP tab (commands and toolbar act through it). */
export interface RdpController {
  readonly tabId: string
  readonly sessionId: string
  ctrlAltDel(): void
  sendCombo(combo: KeyCombo): void
  typeText(text: string): Promise<void>
  sendClipboardText(text: string): Promise<void>
  copyRemoteClipboard(): Promise<void>
  screenshot(): Promise<Blob | null>
  saveScreenshot(): Promise<void>
  copyScreenshot(): Promise<void>
  setScaling(mode: RdpScaling): void
  toggleFullscreen(): Promise<void>
  reconnect(opts?: { engine?: RdpEngine }): void
  /** Cancel a pending automatic reconnection. */
  cancelAutoReconnect(): void
  disconnect(): void
  focus(): void
  uploadFiles(files: File[]): void
  canUpload(): boolean
}
