/*
 * Settings section `vnc` (defaults for new viewers; a connection's own options — scaling, quality, compression,
 * viewOnly, shared, autoReconnect — take precedence).
 */
import { defineSettings } from '@/stores/settings'
import type { ClipboardDirection, ScalingMode } from './types'

export interface VncSettings {
  /** Scaling when the connection does not choose one. */
  scaling: ScalingMode
  /** JPEG quality 0–9 / compression level 0–9 when the connection does not set them. */
  quality: number
  compression: number
  /** Clipboard: `auto` syncs both ways when the browser allows it; `manual` only through the clipboard panel. */
  clipboard: 'auto' | 'manual'
  /**
   * The user's own clipboard direction limit (applied on top of the connection's and the administrator's policy,
   * which NexTerm enforces server-side for local → remote).
   */
  clipboardDirection: ClipboardDirection
  /**
   * "Resize remote desktop" requests the tab size in device pixels on high-DPI screens (sharp, but the remote UI
   * looks smaller unless the remote desktop scales itself).
   */
  hiDpi: boolean
  /** Reconnect automatically after network drops (unless the connection disables autoReconnect). */
  autoReconnect: boolean
  /** Show a dot when the server hides the cursor. */
  dotCursor: boolean
  /** `standard`: app shortcuts keep working (like terminals); `all`: every key goes to the remote desktop. */
  keyboardCapture: 'standard' | 'all'
  /** Server bell. */
  bell: 'visual' | 'sound' | 'off'
  /** Open a tab automatically when a VNC server connects to a listener. */
  openIncoming: boolean
  /** Delay between characters when typing text as keystrokes (ms). */
  typingDelay: number
  /** Show the toolbar (it can be toggled per tab). */
  showToolbar: boolean
}

export const vncSettings = defineSettings<VncSettings>('vnc', {
  scaling: 'fit',
  quality: 6,
  compression: 2,
  clipboard: 'auto',
  clipboardDirection: 'both',
  hiDpi: false,
  autoReconnect: true,
  dotCursor: true,
  keyboardCapture: 'standard',
  bell: 'visual',
  openIncoming: true,
  typingDelay: 12,
  showToolbar: true,
})

export function clampLevel(v: unknown, fallback: number): number {
  const n = typeof v === 'number' ? v : typeof v === 'string' && v.trim() !== '' ? Number(v) : NaN
  return Number.isInteger(n) && n >= 0 && n <= 9 ? n : fallback
}

export function isScalingMode(v: unknown): v is ScalingMode {
  return v === 'fit' || v === 'remote-resize' || v === 'none'
}

export interface ClipboardAllowed {
  toRemote: boolean
  fromRemote: boolean
}

export function isClipboardDirection(v: unknown): v is ClipboardDirection {
  return v === 'both' || v === 'to-remote' || v === 'from-remote' || v === 'none'
}

export function clipboardAllowed(d: ClipboardDirection | undefined): ClipboardAllowed {
  switch (d) {
    case 'to-remote':
      return { toRemote: true, fromRemote: false }
    case 'from-remote':
      return { toRemote: false, fromRemote: true }
    case 'none':
      return { toRemote: false, fromRemote: false }
  }
  return { toRemote: true, fromRemote: true }
}

/** The stricter of several clipboard directions. */
export function combineClipboard(...ds: Array<ClipboardDirection | undefined>): ClipboardAllowed {
  return ds.reduce<ClipboardAllowed>(
    (acc, d) => {
      const a = clipboardAllowed(d)
      return { toRemote: acc.toRemote && a.toRemote, fromRemote: acc.fromRemote && a.fromRemote }
    },
    { toRemote: true, fromRemote: true },
  )
}
