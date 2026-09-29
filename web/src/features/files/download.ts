/*
 * Downloads (FILE-7, FILE-9): the browser's download manager fetches GET /api/fs/{id}/download (single files with
 * Range support; folders and multiple items as a streamed zip). Dragging a folder out of a browser is impossible
 * (RESEARCH §1.18), so the UI offers Download / Download as zip; single files can be dragged to the desktop in
 * Chromium (DownloadURL).
 */
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { uid } from '@/lib/utils'
import { fsApi } from './api'
import { isDirLike } from './format'
import { getFsHandle, withFs } from './fsHandles'
import { basename } from './paths'
import { publishLocal, removeLocal } from './transfers/store'
import type { FsContext } from './types'

/** Absolute URL (DownloadURL drag data needs one). */
export function absoluteUrl(url: string): string {
  try {
    return new URL(url, window.location.href).href
  } catch {
    return url
  }
}

function triggerBrowserDownload(url: string, filename?: string): void {
  const a = document.createElement('a')
  a.href = url
  if (filename) a.download = filename
  a.rel = 'noopener'
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  setTimeout(() => a.remove(), 1000)
}

/** Name of the zip for a selection ("logs.zip", "3 items.zip" → "<folder>.zip"). */
export function zipName(entries: FileEntry[], dir: string): string {
  if (entries.length === 1) return `${entries[0].name}.zip`
  const folder = basename(dir)
  return `${folder && folder !== '/' ? folder : 'files'}.zip`
}

/**
 * The id of a context's handle, confirmed alive on the server (it closes idle handles after a while, and a
 * session's handle is replaced on reconnect): URLs handed to the browser (downloads, previews) cannot be retried
 * with a fresh handle by the page, so check first. Falls back to the known id when the check itself fails.
 */
export async function liveFsId(ctx: FsContext): Promise<string> {
  try {
    return await withFs(ctx.key, async (id) => {
      await fsApi.info(id)
      return id
    })
  } catch {
    return getFsHandle(ctx.key)?.id ?? ctx.handle.id
  }
}

/**
 * Download entries: one file → as is; a folder, several items or `zip` → one zip. Recorded in the transfer queue
 * as handed to the browser (its download manager shows the progress).
 */
export function downloadEntries(ctx: FsContext, entries: FileEntry[], dir: string, opts: { zip?: boolean } = {}): void {
  const list = entries.filter((e) => e.name !== '..')
  if (!list.length) return
  const single = list.length === 1 && !isDirLike(list[0]) && !opts.zip
  const name = single ? list[0].name : zipName(list, dir)
  const urlFor = (fsId: string) =>
    fsApi.downloadUrl(
      fsId,
      list.map((e) => e.path),
      { zip: !single, name },
    )
  const start = async () => triggerBrowserDownload(urlFor(await liveFsId(ctx)), name)
  void start()
  const id = uid('download')
  const size = single ? list[0].size : 0
  publishLocal(
    {
      id,
      origin: 'local',
      kind: 'download',
      label: name,
      detail: `from ${single ? dir : list.length === 1 ? list[0].path : dir} on ${ctx.label} — see your browser's downloads`,
      state: 'done',
      totalBytes: size,
      doneBytes: size,
      totalFiles: single ? 1 : list.length,
      doneFiles: single ? 1 : list.length,
      failedFiles: 0,
      bytesPerSec: 0,
      createdAt: Date.now(),
      finishedAt: Date.now(),
      canCancel: false,
      canRetry: true,
    },
    {
      retry: () => void start(),
      remove: () => removeLocal(id),
    },
  )
  toast.info(`Downloading ${name}`, { description: single ? undefined : 'Folders and multiple items are packed into a zip.' })
}

/** URL for inline viewing (images, PDF, media) in the browser (`fsId`: a live handle id, see liveFsId). */
export function inlineUrl(fsId: string, path: string): string {
  return fsApi.downloadUrl(fsId, [path], { inline: true })
}
