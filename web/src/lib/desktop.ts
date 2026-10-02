/*
 * What the desktop app (desktop/src-tauri) tells the page about its window, through an initialization script. Absent
 * in browsers.
 */
declare global {
  interface Window {
    __ASTRATERM_DESKTOP__?: {
      /** The app decides where downloads go: Settings → General → Downloads (appCommand below). */
      downloads?: boolean
      /** Transparent window over the system blur (macOS, Windows): Settings → Appearance → Window opacity applies. */
      translucent?: boolean
      /** Windows: the page switches the blur behind the window (Acrylic) itself, see setWindowBlur. */
      blurOnDemand?: boolean
      /**
       * No system title bar; the page's top bar drags the window. macOS: the system window buttons sit over its left
       * end. Windows: the page draws minimize / maximize / close (layout/WindowControls.tsx).
       */
      titleBar?: 'macos' | 'windows'
    }
    __TAURI_INTERNALS__?: { invoke: (cmd: string, args?: Record<string, unknown>) => Promise<unknown> }
  }
}

const desktop = typeof window !== 'undefined' ? window.__ASTRATERM_DESKTOP__ : undefined

export const TRANSLUCENCY_SUPPORTED = !!desktop?.translucent
export const TITLE_BAR = desktop?.titleBar
export const DOWNLOAD_SETTINGS = !!desktop?.downloads
/** Lowest Window opacity. */
export const MIN_WINDOW_OPACITY = 0.1

const blurState = new WeakMap<Window, boolean>()

/** Windows: Acrylic behind a see-through window, none otherwise (it slows down moving and resizing the window). */
export function setWindowBlur(win: Window | null, on: boolean): void {
  if (!desktop?.blurOnDemand || !win || blurState.get(win) === on) return
  blurState.set(win, on)
  void win.__TAURI_INTERNALS__?.invoke('plugin:window|set_effects', { value: on ? { effects: ['acrylic'] } : null }).catch(() => blurState.delete(win))
}

/** Run a command of the desktop app (only those its capabilities allow). */
export function appCommand<T = void>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const ipc = window.__TAURI_INTERNALS__
  return ipc ? (ipc.invoke(cmd, args) as Promise<T>) : Promise.reject(new Error('not in the desktop app'))
}

/** Run a Tauri window command on this window. */
export const windowCommand = <T = void>(cmd: string): Promise<T> => appCommand<T>(`plugin:window|${cmd}`)

/** Where the desktop app saves downloads (desktop/src-tauri/src/downloads.rs). */
export interface DownloadSettings {
  /** Ask where to save each file. */
  ask: boolean
  /** The download folder. */
  dir: string
}
