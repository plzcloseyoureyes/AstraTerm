/*
 * What the desktop app (desktop/src-tauri) tells the page about its window, through an initialization script. Absent
 * in browsers.
 */

declare global {
  interface Window {
    __ASTRATERM_DESKTOP__?: {
      /** Transparent window over the system blur (macOS, Windows): Settings → Appearance → Window opacity applies. */
      translucent?: boolean
      /** Windows: the page switches the blur behind the window (Acrylic) itself, see setWindowBlur. */
      blurOnDemand?: boolean
      /** macOS: no system title bar; the window buttons sit over the top-left of the page, which drags the window. */
      titleBarOverlay?: boolean
    }
    __TAURI_INTERNALS__?: { invoke: (cmd: string, args?: Record<string, unknown>) => Promise<unknown> }
  }
}

const desktop = typeof window !== 'undefined' ? window.__ASTRATERM_DESKTOP__ : undefined

export const TRANSLUCENCY_SUPPORTED = !!desktop?.translucent
export const TITLE_BAR_OVERLAY = !!desktop?.titleBarOverlay

const blurState = new WeakMap<Window, boolean>()

/** Windows: Acrylic behind a see-through window, none otherwise (it slows down moving and resizing the window). */
export function setWindowBlur(win: Window | null, on: boolean): void {
  if (!desktop?.blurOnDemand || !win || blurState.get(win) === on) return
  blurState.set(win, on)
  void win.__TAURI_INTERNALS__?.invoke('plugin:window|set_effects', { value: on ? { effects: ['acrylic'] } : null }).catch(() => blurState.delete(win))
}
