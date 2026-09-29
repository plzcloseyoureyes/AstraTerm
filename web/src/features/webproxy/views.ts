/* Mounted web tabs expose a small handle so palette commands / shortcuts can drive the active one. */

export interface WebViewHandle {
  back: () => void
  forward: () => void
  reload: () => void
  home: () => void
  zoomIn: () => void
  zoomOut: () => void
  zoomReset: () => void
  openExternal: () => void
  focusAddress: () => void
}

const views = new Map<string, WebViewHandle>()

export function registerWebView(tabId: string, h: WebViewHandle): () => void {
  views.set(tabId, h)
  return () => {
    if (views.get(tabId) === h) views.delete(tabId)
  }
}

export function getWebView(tabId: string | undefined): WebViewHandle | undefined {
  return tabId ? views.get(tabId) : undefined
}
