/*
 * Drag & drop (FILE-5):
 *   - files and folders dropped from the OS → upload into the folder under the pointer (row, breadcrumb or list);
 *   - rows dragged between views (panel ↔ tab, two panes, two sessions) → copy / move, server-side;
 *   - a single file dragged out to the desktop (Chromium DownloadURL). Folders cannot leave the browser: use
 *     Download / Download as zip;
 *   - in the Linux desktop app, files dropped from other apps arrive as host paths (installFileDrops).
 */
import type React from 'react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { errorMessage, isMac } from '@/lib/utils'
import { fsApi } from './api'
import { transferEntries } from './actions'
import { absoluteUrl, zipName } from './download'
import { guessMime, isDirLike } from './format'
import { getFsHandle, openFs, sourceKey, withFs } from './fsHandles'
import { collectDrop, planUpload, withSlowToast } from './upload'
import type { FsContext } from './types'

const INTERNAL_MIME = 'application/x-astraterm-files'

interface ActiveDrag {
  viewId: string
  ctx: FsContext
  entries: FileEntry[]
  dir: string
}

let activeDrag: ActiveDrag | null = null

/** Start dragging rows out of a view. */
export function startInternalDrag(e: React.DragEvent, drag: ActiveDrag): void {
  activeDrag = drag
  const dt = e.dataTransfer
  dt.effectAllowed = 'copyMove'
  const paths = drag.entries.map((x) => x.path)
  dt.setData(INTERNAL_MIME, JSON.stringify({ fsId: drag.ctx.handle.id, paths }))
  dt.setData('text/plain', paths.join('\n'))
  // Chromium: dropping on the desktop / a native file manager downloads one file — the file itself, or a zip of a
  // folder / several items (a browser cannot hand out folders or several files).
  // Synchronous (dragstart): the latest handle id of this file system (a view's ctx may lag behind a reopen).
  const fsId = getFsHandle(drag.ctx.key)?.id ?? drag.ctx.handle.id
  const only = drag.entries.length === 1 ? drag.entries[0] : null
  const single = !!only && !isDirLike(only)
  const name = single ? only.name : zipName(drag.entries, drag.dir)
  const url = absoluteUrl(single ? fsApi.downloadUrl(fsId, [only.path]) : fsApi.downloadUrl(fsId, paths, { zip: true, name }))
  try {
    dt.setData('DownloadURL', `${single ? guessMime(name) : 'application/zip'}:${name.replace(/:/g, '_')}:${url}`)
  } catch {
    /* not supported */
  }
}

export function endInternalDrag(): void {
  activeDrag = null
}

export type DragKind = 'internal' | 'os' | null

export function dragKindOf(e: React.DragEvent): DragKind {
  const types = Array.from(e.dataTransfer?.types ?? [])
  if (types.includes(INTERNAL_MIME) && activeDrag) return 'internal'
  if (types.includes('Files')) return 'os'
  return null
}

/** Copy (Ctrl / ⌥) or move (default within one file system; other file systems always copy). */
export function dropModeFor(e: React.DragEvent | DragEvent, target: FsContext): 'copy' | 'move' {
  const drag = activeDrag
  if (!drag) return 'copy'
  const copyKey = isMac ? e.altKey : e.ctrlKey
  if (drag.ctx.key !== target.key) return e.shiftKey ? 'move' : 'copy'
  return copyKey ? 'copy' : 'move'
}

/** Can the dragged rows be dropped into `dir` of `target`? (Not onto themselves / into their own subtree.) */
function canDropInto(target: FsContext, dir: string): boolean {
  const drag = activeDrag
  if (!drag) return true
  if (drag.ctx.key !== target.key) return true
  return !drag.entries.some((x) => x.path === dir || (isDirLike(x) && (dir + '/').startsWith(`${x.path}/`)))
}

/** dragover handler body: returns true when the drop is accepted (and sets the drop effect). */
export function acceptDrag(e: React.DragEvent, target: FsContext, dir: string): boolean {
  const kind = dragKindOf(e)
  if (!kind) return false
  if (kind === 'internal' && !canDropInto(target, dir)) {
    e.dataTransfer.dropEffect = 'none'
    return false
  }
  e.preventDefault()
  e.dataTransfer.dropEffect = kind === 'os' ? 'copy' : dropModeFor(e, target)
  return true
}

/** Perform a drop into `dir` of `target`. */
export function performDrop(e: React.DragEvent, target: FsContext, dir: string): void {
  const kind = dragKindOf(e)
  if (!kind) return
  e.preventDefault()
  e.stopPropagation()
  if (kind === 'os') {
    // Read the DataTransfer now (only possible during the event); big folder trees take a moment to walk.
    const pending = withSlowToast(collectDrop(e.dataTransfer), 'Reading the dropped items…')
    void pending.then(({ files, dirs }) => planUpload(target, dir, files, dirs))
    return
  }
  const drag = activeDrag
  if (!drag || !canDropInto(target, dir)) {
    activeDrag = null
    return
  }
  const mode = dropModeFor(e, target)
  activeDrag = null
  void transferEntries(drag.ctx, drag.entries, target, dir, mode)
}

let guardInstalled = false

/**
 * OS files dropped where nothing handles them (a gap between drop zones, a disconnected SFTP panel, the tab strip…)
 * would make the browser open the file in place of AstraTerm. Drop zones handle (preventDefault) their own events
 * first; this window-level fallback only turns an unhandled file drop into a no-op.
 */
export function installDropGuard(): void {
  if (guardInstalled || typeof window === 'undefined') return
  guardInstalled = true
  const carriesFiles = (e: DragEvent) => Array.from(e.dataTransfer?.types ?? []).includes('Files')
  window.addEventListener('dragover', (e) => {
    if (e.defaultPrevented || !carriesFiles(e)) return
    e.preventDefault()
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'none'
  })
  window.addEventListener('drop', (e) => {
    if (!e.defaultPrevented && carriesFiles(e)) e.preventDefault()
  })
}

// ---- files dropped from other apps in the Linux desktop app ------------------------------------------------------------
// WebKitGTK no longer gives a page the files dropped onto it from a file manager (dataTransfer.files is empty), so the
// Linux desktop app takes those drops natively and passes their host paths and the pointer position as window events
// (desktop/src-tauri/src/main.rs, forward_file_drops). The server runs on that same machine: it copies the files from
// its local file system into the file browser under the pointer, like a drag from the local files pane.

/** Where a drop goes: the folder, and the view to mark active (the conflict question / follow-up focus belong to it). */
interface FileDropTarget {
  ctx: FsContext
  dir: string
  activate?: () => void
}

/** File browsers taking such drops: their root element → where a drop goes (null: not now). */
const fileDropZones = new WeakMap<Element, () => FileDropTarget | null>()

/** Registers a file browser root for files dropped from other apps; returns the unregister function. */
export function registerFileDropZone(el: Element, target: () => FileDropTarget | null): () => void {
  fileDropZones.set(el, target)
  return () => fileDropZones.delete(el)
}

function zoneAt(x: number, y: number): Element | null {
  for (let el = document.elementFromPoint(x, y); el; el = el.parentElement) if (fileDropZones.get(el)?.()) return el
  return null
}

let fileDropsInstalled = false

/** Listens for the desktop app's file drag / drop events (they only occur in the Linux desktop app). */
export function installFileDrops(): void {
  if (fileDropsInstalled || typeof window === 'undefined') return
  fileDropsInstalled = true
  let hovered: Element | null = null
  const highlight = (el: Element | null) => {
    if (el === hovered) return
    hovered?.removeAttribute('data-file-drop')
    el?.setAttribute('data-file-drop', '')
    hovered = el
  }
  window.addEventListener('astraterm:file-drag', (e) => {
    const p = (e as CustomEvent<{ x: number; y: number } | null>).detail
    highlight(p ? zoneAt(p.x, p.y) : null)
  })
  window.addEventListener('astraterm:file-drop', (e) => {
    const { paths, x, y } = (e as CustomEvent<{ paths: string[]; x: number; y: number }>).detail
    highlight(null)
    const zone = zoneAt(x, y)
    const target = zone && fileDropZones.get(zone)?.()
    if (!target) {
      toast.info('Drop files onto a file browser to upload them')
      return
    }
    target.activate?.()
    void uploadHostPaths(paths, target.ctx, target.dir)
  })
}

/** Copy files of this machine (host paths) into dir of target, through the transfer queue. */
async function uploadHostPaths(paths: string[], target: FsContext, dir: string): Promise<void> {
  const source = { kind: 'local' } as const
  try {
    const handle = await openFs(source)
    const local: FsContext = { key: sourceKey(source), handle, source, placeKey: 'local', label: 'Local files' }
    const entries = await Promise.all(paths.map((p) => withFs(local.key, (id) => fsApi.stat(id, p)).catch(() => null)))
    const found = entries.filter((x): x is FileEntry => !!x)
    if (found.length < paths.length) toast.error(`${paths.length - found.length} of the dropped items could not be read`)
    if (found.length) await transferEntries(local, found, target, dir, 'copy')
  } catch (err) {
    toast.error('Could not upload the dropped files', { description: errorMessage(err) })
  }
}
