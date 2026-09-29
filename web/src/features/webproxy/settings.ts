import { defineSettings } from '@/stores/settings'

/**
 * User preferences of the web tabs. (The administrator configuration lives in the *global* section `webproxy`:
 * hostSuffix, pathMode, idleMinutes — read by the backend only from the global scope.)
 */
export interface WebViewSettings {
  /** Default zoom of new web tabs (1 = 100 %). */
  defaultZoom: number
  /** Recently opened addresses (no credentials), newest first. */
  recent: string[]
  /** Close the proxy when its last tab closes (else it idles out). */
  closeWithTab: boolean
}

export const webViewSettings = defineSettings<WebViewSettings>('webView', {
  defaultZoom: 1,
  recent: [],
  closeWithTab: true,
})

const MAX_RECENT = 12

/** Remember an address (user:password@ parts are never stored). */
export function rememberAddress(url: string): void {
  let clean = url.trim()
  try {
    const u = new URL(clean.includes('://') ? clean : `http://${clean}`)
    u.username = ''
    u.password = ''
    clean = u.toString()
  } catch {
    return
  }
  const prev = webViewSettings.get().recent ?? []
  const next = [clean, ...prev.filter((x) => x !== clean)].slice(0, MAX_RECENT)
  webViewSettings.set({ recent: next })
}
