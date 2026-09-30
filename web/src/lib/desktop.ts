/*
 * What the desktop app (desktop/src-tauri) tells the page about its window, through an initialization script. Absent
 * in browsers.
 */

declare global {
  interface Window {
    __ASTRATERM_DESKTOP__?: {
      /** Transparent window over the system blur (macOS, Windows): Settings → Appearance → Window opacity applies. */
      translucent?: boolean
      /** macOS: no system title bar; the window buttons sit over the top-left of the page, which drags the window. */
      titleBarOverlay?: boolean
    }
  }
}

const desktop = typeof window !== 'undefined' ? window.__ASTRATERM_DESKTOP__ : undefined

export const TRANSLUCENCY_SUPPORTED = !!desktop?.translucent
export const TITLE_BAR_OVERLAY = !!desktop?.titleBarOverlay
