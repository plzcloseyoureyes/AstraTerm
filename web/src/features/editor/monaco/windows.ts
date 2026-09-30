/*
 * Editors in dockview pop-out windows. Dockview moves a tab's DOM into the pop-out's document and copies the style
 * sheets present at that moment; Monaco keeps running in the main window. What Monaco adds later lives only in the
 * main document, so for every pop-out that shows an editor:
 *
 *   - Monaco's own style sheets (<style media="screen">: the theme colours, rules added per decoration) are mirrored
 *     and kept in sync — a theme / accent change or a new colour swatch shows up in the pop-out too;
 *   - style sheets loaded after the pop-out opened (the lazy editor chunk's <link>s) are copied;
 *   - `followWindow` tells an editor when its container moved to another window, so it can move its suggestion /
 *     hover layer there and lay itself out with that window's ResizeObserver.
 */
import { onWorkspaceWindow, useWorkspaceStore } from '@/stores/workspace'

// ---------------------------------------------------------------------------------------------------------------------
// style mirroring
// ---------------------------------------------------------------------------------------------------------------------

/** Monaco creates its dynamic style sheets with media="screen" (base/browser/domStylesheets.js). */
function isMonacoStyle(el: Element): el is HTMLStyleElement {
  return el instanceof HTMLStyleElement && (el.media === 'screen' || el.classList.contains('monaco-colors'))
}

/** Vite dev server: CSS modules are <style data-vite-dev-id>; only the editor's own are mirrored. */
function isEditorDevStyle(el: Element): el is HTMLStyleElement {
  const id = el.getAttribute('data-vite-dev-id')
  return !!id && /monaco-editor|features\/editor\//.test(id)
}

function sheetText(el: HTMLStyleElement): string {
  try {
    const rules = el.sheet?.cssRules
    if (rules) return Array.from(rules, (r) => r.cssText).join('\n')
  } catch {
    /* not readable */
  }
  return el.textContent ?? ''
}

function signature(el: HTMLStyleElement): string {
  let n = -1
  try {
    n = el.sheet?.cssRules.length ?? -1
  } catch {
    /* ignore */
  }
  return `${n}:${el.textContent?.length ?? 0}`
}

class StyleMirror {
  private readonly clones = new Map<HTMLStyleElement, { el: HTMLStyleElement; sig: string }>()
  private readonly links = new Set<string>()

  constructor(readonly doc: Document) {
    for (const l of Array.from(doc.querySelectorAll<HTMLLinkElement>('link[rel="stylesheet"]'))) this.links.add(l.href)
  }

  get alive(): boolean {
    const win = this.doc.defaultView
    return !!win && !win.closed && this.doc.body?.isConnected !== false
  }

  sync(): void {
    const head = this.doc.head
    if (!head) return
    const seen = new Set<HTMLStyleElement>()
    for (const el of Array.from(document.head.children)) {
      if (el instanceof HTMLLinkElement && el.rel === 'stylesheet' && el.href && !this.links.has(el.href)) {
        this.links.add(el.href)
        const link = this.doc.createElement('link')
        link.rel = 'stylesheet'
        link.href = el.href
        head.appendChild(link)
        continue
      }
      if (!isMonacoStyle(el) && !isEditorDevStyle(el)) continue
      seen.add(el)
      const sig = signature(el)
      const clone = this.clones.get(el)
      if (clone && clone.sig === sig) continue
      const target = clone?.el ?? this.doc.createElement('style')
      target.textContent = sheetText(el)
      if (!clone) {
        target.dataset.nxMirror = ''
        head.appendChild(target)
      }
      this.clones.set(el, { el: target, sig })
    }
    for (const [src, c] of this.clones) {
      if (seen.has(src)) continue
      c.el.remove()
      this.clones.delete(src)
    }
  }

  dispose(): void {
    for (const c of this.clones.values()) c.el.remove()
    this.clones.clear()
  }
}

const mirrors = new Map<Document, StyleMirror>()
let headObserver: MutationObserver | null = null
let pollTimer: ReturnType<typeof setInterval> | null = null
let syncFrame = 0

function syncAll(): void {
  syncFrame = 0
  for (const [doc, m] of mirrors) {
    if (!m.alive) {
      mirrors.delete(doc)
      continue
    }
    m.sync()
  }
  if (!mirrors.size) stopWatching()
}

function scheduleSync(): void {
  if (!syncFrame) syncFrame = requestAnimationFrame(syncAll)
}

function stopWatching(): void {
  headObserver?.disconnect()
  headObserver = null
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
}

/** Keep Monaco's styles in `doc` (a pop-out window's document) in sync with the main document. */
function mirrorStyles(doc: Document): void {
  if (doc === document || mirrors.has(doc)) return
  const m = new StyleMirror(doc)
  mirrors.set(doc, m)
  m.sync()
  if (!headObserver) {
    headObserver = new MutationObserver(scheduleSync)
    headObserver.observe(document.head, {
      childList: true,
      subtree: true,
      characterData: true,
    })
  }
  // Rules inserted through the CSSOM (insertRule) do not show up as mutations.
  if (!pollTimer) pollTimer = setInterval(scheduleSync, 1500)
}

// ---------------------------------------------------------------------------------------------------------------------
// following an editor into another window
// ---------------------------------------------------------------------------------------------------------------------

const watchers = new Set<() => void>()
let checkTimer: ReturnType<typeof setTimeout> | null = null

function checkAll(): void {
  checkTimer = null
  for (const w of Array.from(watchers)) w()
}

function scheduleCheck(delay = 0): void {
  if (checkTimer) clearTimeout(checkTimer)
  checkTimer = setTimeout(checkAll, delay)
}

let hooked = false
function hook(): void {
  if (hooked) return
  hooked = true
  // Tabs moving between windows change the workspace's layout; a new pop-out gets its content a moment later.
  useWorkspaceStore.subscribe(() => scheduleCheck(0))
  onWorkspaceWindow(() => {
    scheduleCheck(50)
    scheduleCheck(400)
  })
}

/**
 * Call `onMove(doc)` whenever `host` ends up in another document (a pop-out window, or back in the main window), and
 * once now when it is not in the main document. Returns the disposer.
 */
export function followWindow(host: HTMLElement, onMove: (doc: Document) => void): () => void {
  hook()
  let doc: Document = document
  const check = () => {
    const d = host.ownerDocument
    if (d === doc) return
    doc = d
    if (d !== document) mirrorStyles(d)
    onMove(d)
  }
  watchers.add(check)
  check()
  return () => {
    watchers.delete(check)
  }
}

/**
 * Lay out an editor from the ResizeObserver of the window it is in now. Monaco's own observer (automaticLayout) belongs
 * to the main window: it does not see elements of other documents resize, and loses the element once it moved to a
 * pop-out and back. Returns the disposer.
 */
export function observeResize(host: HTMLElement, layout: () => void): () => void {
  const win = host.ownerDocument.defaultView as (Window & typeof globalThis) | null
  if (!win || typeof win.ResizeObserver !== 'function') return () => undefined
  // ResizeObserver callbacks already run once per rendering step: lay out right away (no extra frame, which a
  // background pop-out window may not get for a while).
  const ro = new win.ResizeObserver(() => layout())
  ro.observe(host)
  return () => ro.disconnect()
}
