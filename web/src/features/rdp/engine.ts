/*
 * Contract between the tab controller (connection.ts) and the two rendering engines: IronRDP (ironrdp.ts) and the
 * Guacamole client for guacd (guac.ts).
 */
import type { RdpViewerSettings } from './settings'
import type { FileTransfer, KeyCombo, RdpEngine, RdpScaling, RdpTicketInfo, ViewerErrorCode, ViewerStatus } from './types'

export interface DesktopRequest {
  width: number
  height: number
  dpi: number
  /** Windows scale factor in percent (HiDPI). */
  scaleFactor?: number
}

export interface EndInfo {
  error?: boolean
  message?: string
  authFailed?: boolean
  code?: ViewerErrorCode
  /** The engine already reported the state to the backend (or the backend tracks it itself). */
  reported?: boolean
}

export interface EngineCallbacks {
  onStatus(status: ViewerStatus, message?: string): void
  onDesktopSize(width: number, height: number): void
  /** Remote clipboard text (guacd), or `pending` when it must be copied with a user gesture (IronRDP). */
  onRemoteClipboard(text: string | null, pending: boolean): void
  onEnded(info: EndInfo): void
  onWarning(message: string): void
  onTransfer(t: FileTransfer): void
}

export interface EngineContext {
  tabId: string
  sessionId: string
  /** Element the engine renders into (sized and centred by the controller). */
  stage: HTMLElement
  /** Focusable, scrolling container around the stage (keyboard target of the guacd engine). */
  viewport: HTMLElement
  callbacks: EngineCallbacks
  settings: () => RdpViewerSettings
  /** A read-only view (administrator shadowing a session): no input, no clipboard, no uploads. */
  readOnly: boolean
}

/** Error thrown by adapters when connecting fails. */
export class EngineError extends Error {
  readonly authFailed: boolean
  readonly code?: ViewerErrorCode
  /** The backend knows a more precise reason (session stateMessage). */
  readonly preferServerMessage: boolean
  constructor(message: string, opts: { authFailed?: boolean; code?: ViewerErrorCode; preferServerMessage?: boolean } = {}) {
    super(message)
    this.name = 'EngineError'
    this.authFailed = !!opts.authFailed
    this.code = opts.code
    this.preferServerMessage = !!opts.preferServerMessage
  }
}

/** Thrown when the adapter was destroyed while connecting. */
export class Cancelled extends Error {
  constructor() {
    super('cancelled')
    this.name = 'Cancelled'
  }
}

export interface EngineAdapter {
  readonly engine: RdpEngine
  /** Connect and resolve once the remote desktop is up. */
  connect(ticket: RdpTicketInfo, size: DesktopRequest): Promise<void>
  /** Ask the server to change the desktop size (dynamic resolution). */
  requestSize(size: DesktopRequest): void
  /** Show the desktop at `scale` (display pixels per remote pixel) in a box of cssWidth × cssHeight CSS pixels. */
  layout(scale: number, cssWidth: number, cssHeight: number): void
  focus(): void
  releaseKeys(): void
  sendCombo(combo: KeyCombo): void
  typeText(text: string, signal: AbortSignal): Promise<void>
  sendClipboardText(text: string): Promise<void>
  copyRemoteClipboard(): Promise<void>
  /** The viewer gained focus (automatic clipboard synchronisation). */
  onViewerFocus(): void
  screenshot(): Promise<Blob | null>
  canUpload(): boolean
  uploadFiles(files: File[]): void
  destroy(): void
}

/** Display box of a desktop in a viewport for a scaling mode. */
export function computeLayout(
  mode: RdpScaling,
  viewport: { width: number; height: number },
  desktop: { width: number; height: number },
  devicePixelRatio: number,
  hiDpi: boolean,
): { scale: number; width: number; height: number } {
  const px = hiDpi ? Math.max(1, devicePixelRatio) : 1
  if (mode === 'none' || viewport.width < 2 || viewport.height < 2) {
    return { scale: 1 / px, width: desktop.width / px, height: desktop.height / px }
  }
  const scale = Math.min(viewport.width / desktop.width, viewport.height / desktop.height)
  return { scale, width: Math.floor(desktop.width * scale), height: Math.floor(desktop.height * scale) }
}

/** Remote desktop size for a viewport (even width, within RDP limits). */
export function desktopRequest(viewport: { width: number; height: number }, devicePixelRatio: number, hiDpi: boolean): DesktopRequest {
  const px = hiDpi ? Math.max(1, devicePixelRatio) : 1
  let width = Math.floor(viewport.width * px)
  let height = Math.floor(viewport.height * px)
  width -= width % 2
  width = Math.min(8192, Math.max(200, width))
  height = Math.min(8192, Math.max(200, height))
  const req: DesktopRequest = { width, height, dpi: Math.round(96 * px) }
  if (hiDpi && px > 1) req.scaleFactor = Math.min(500, Math.max(100, Math.round(px * 100)))
  return req
}

export const sleep = (ms: number, signal?: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    if (signal?.aborted) return reject(new Cancelled())
    const t = setTimeout(resolve, ms)
    signal?.addEventListener(
      'abort',
      () => {
        clearTimeout(t)
        reject(new Cancelled())
      },
      { once: true },
    )
  })
