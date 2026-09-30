/*
 * Viewer actions shared by the toolbar and the commands: screenshots (GFX-18), fullscreen with Keyboard Lock (GFX-3),
 * clipboard helpers.
 */
import { toast } from 'sonner'
import { errorMessage } from '@/lib/utils'
import { getTabParams } from '@/stores/workspace'
import type { VncController } from './controller'
import { setTabUI } from './store'
import type { VncTabParams } from './types'

// ---- view roots (fullscreen targets) --------------------------------------------------------------------------------

const roots = new Map<string, HTMLElement>()

export function registerRoot(tabId: string, el: HTMLElement): () => void {
  roots.set(tabId, el)
  return () => {
    if (roots.get(tabId) === el) roots.delete(tabId)
  }
}

interface KeyboardLock {
  lock?: (keys?: string[]) => Promise<void>
  unlock?: () => void
}

function keyboardOf(doc: Document): KeyboardLock | undefined {
  return (doc.defaultView?.navigator as Navigator & { keyboard?: KeyboardLock } | undefined)?.keyboard
}

/**
 * Toggle fullscreen of a VNC tab. Where supported (Chromium), the keyboard is locked so system shortcuts (Alt+Tab,
 * Win, Esc) reach the remote desktop; holding Esc leaves fullscreen.
 */
export async function toggleViewerFullscreen(tabId: string): Promise<void> {
  const el = roots.get(tabId)
  if (!el) return
  const doc = el.ownerDocument
  try {
    if (doc.fullscreenElement === el) {
      keyboardOf(doc)?.unlock?.()
      await doc.exitFullscreen()
      return
    }
    await el.requestFullscreen({ navigationUI: 'hide' })
    const kb = keyboardOf(doc)
    if (kb?.lock) {
      try {
        await kb.lock()
        setTabUI(tabId, { keyboardLocked: true })
        toast.info('Full screen: keyboard captured', { description: 'Press and hold Esc to exit full screen.' })
      } catch {
        setTabUI(tabId, { keyboardLocked: false })
      }
    }
  } catch (err) {
    toast.error('Full screen is not available', { description: errorMessage(err) })
  }
}

// ---- screenshots ----------------------------------------------------------------------------------------------------

const pad = (n: number) => String(n).padStart(2, '0')

function screenshotName(tabId: string): string {
  const p = getTabParams<VncTabParams>(tabId)
  const base = (p?.title || 'vnc').replace(/[^\w.-]+/g, '_').replace(/^_+|_+$/g, '') || 'vnc'
  const d = new Date()
  return `${base}-${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}.png`
}

export async function saveScreenshot(c: VncController): Promise<void> {
  const blob = await c.screenshot()
  if (!blob) {
    toast.error('No screen to capture', { description: 'Connect to the remote desktop first.' })
    return
  }
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = screenshotName(c.tabId)
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 30_000)
  toast.success('Screenshot saved', { description: a.download })
}

export async function copyScreenshot(c: VncController): Promise<void> {
  const blob = await c.screenshot()
  if (!blob) {
    toast.error('No screen to capture', { description: 'Connect to the remote desktop first.' })
    return
  }
  try {
    if (typeof ClipboardItem === 'undefined' || !navigator.clipboard?.write) throw new Error('Copying images is not supported by this browser')
    await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })])
    toast.success('Screenshot copied to the clipboard')
  } catch (err) {
    toast.error('Could not copy the screenshot', { description: errorMessage(err) })
  }
}

// ---- clipboard --------------------------------------------------------------------------------------------------------

/** Paste the local clipboard into the remote clipboard (explicit user action, may show a permission prompt). */
export async function pasteLocalClipboard(c: VncController): Promise<void> {
  try {
    const text = await navigator.clipboard.readText()
    if (!text) {
      toast.info('The clipboard is empty')
      return
    }
    if (!c.state.clipboard.toRemote) {
      toast.error('The clipboard policy does not allow sending to the remote desktop')
      return
    }
    if (c.sendClipboard(text)) toast.success('Clipboard sent to the remote desktop')
  } catch (err) {
    toast.error('Cannot read the clipboard', { description: `${errorMessage(err)} — use the clipboard panel instead.` })
  }
}
