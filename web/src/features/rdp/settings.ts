/*
 * Viewer preferences (settings section "rdpViewer", per user). Server-side engine configuration (guacd address,
 * default engine) is the admin's global settings section "rdp" (see SettingsSection.tsx).
 */
import { defineSettings } from '@/stores/settings'
import type { RdpScaling } from './types'

export interface RdpViewerSettings {
  /** Default scaling of new remote desktop tabs. */
  scaling: RdpScaling
  /** Request the remote resolution in device pixels on high-DPI screens (sharper, more bandwidth). */
  hiDpi: boolean
  /** Synchronise the clipboard automatically when the viewer has focus (else through the clipboard panel). */
  autoClipboard: boolean
  /** In fullscreen, capture system keys (Esc, Alt+Tab, Win…) with the Keyboard Lock API (Chromium). */
  keyboardLock: boolean
  /** Show the remote cursor through the browser cursor (smoother) instead of drawing it (guacd engine). */
  localCursor: boolean
}

export const rdpSettings = defineSettings<RdpViewerSettings>('rdpViewer', {
  scaling: 'resize',
  hiDpi: false,
  autoClipboard: true,
  keyboardLock: true,
  localCursor: true,
})
