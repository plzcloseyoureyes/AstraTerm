/*
 * File operations shared by the SFTP panel, files tabs, commander panes, context menus and commands (FILE-3, FILE-4,
 * FILE-9, FILE-12, FILE-15..17). Every function takes the view's FsContext; errors are reported with toasts.
 */
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import type { FileEntry, FsOpenRequest, OverwritePolicy } from '@/api/types'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { confirm, prompt } from '@/components/ui/dialog-host'
import { getTerminalsBySession, sendToSession } from '@/features/terminal/bus'
import { openQuick } from '@/features/terminal/open'
import { copyText, errorMessage, plural } from '@/lib/utils'
import { focusTab } from '@/stores/workspace'
import { fsApi, fsKeys, isConflict, isPermissionDenied } from './api'
import { clearFileClipboard, getFileClipboard } from './clipboard'
import { askConflict, openFilesDialog } from './dialogs/store'
import { downloadEntries, liveFsId } from './download'
import { describeSelection, fileKind, isBinaryKind, isDirLike, previewKind } from './format'
import { withFs } from './fsHandles'
import { basename, dirname, encodePathForUrl, isInside, joinPath, normalizePath, shellQuote, uniqueName, validateName } from './paths'
import { filesSettings } from './settings'
import { shellReadiness } from './terminalProbe'
import { startServerTransfer } from './transfers/store'
import type { ConflictPolicy, FsContext } from './types'
import { pickLocalFiles, planUpload } from './upload'

// ---------------------------------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------------------------------

/** Refresh every view showing `dir` of this file system. */
export function invalidateDir(ctx: FsContext, dir: string): void {
  void refreshDir(ctx, dir)
}

/** Refetch a folder's listing; resolves when the fresh listing arrived (or failed). */
export function refreshDir(ctx: FsContext, dir: string): Promise<void> {
  return queryClient.invalidateQueries({ queryKey: fsKeys.list(ctx.handle.id, normalizePath(dir)) })
}

/** How long a prefetched listing counts as fresh: opening that folder right after shows it without a request. */
const PREFETCH_FRESH_MS = 10_000

/**
 * Warm a folder's listing in the query cache (hover / cursor on a folder, the parent of the current one), so opening
 * it is instant. No-op while a fresh copy is cached; failures are ignored (the navigation itself reports them).
 */
export function prefetchDir(fsId: string, dir: string): void {
  const path = normalizePath(dir)
  void queryClient.prefetchQuery({
    queryKey: fsKeys.list(fsId, path),
    queryFn: ({ signal }) => fsApi.list(fsId, path, signal),
    staleTime: PREFETCH_FRESH_MS,
  })
}

export function invalidateAll(ctx: FsContext): void {
  void queryClient.invalidateQueries({ queryKey: fsKeys.lists(ctx.handle.id) })
}

function realEntries(entries: FileEntry[]): FileEntry[] {
  return entries.filter((e) => e.name !== '..')
}

function reportError(what: string, err: unknown): void {
  let description = errorMessage(err)
  if (isPermissionDenied(err)) description = `Permission denied. ${description === 'Permission denied' ? '' : description}`.trim()
  toast.error(what, { description })
}

async function listNames(ctx: FsContext, dir: string): Promise<{ names: Set<string>; dirs: Set<string> }> {
  const r = await withFs(ctx.key, (id) => fsApi.list(id, dir))
  return {
    names: new Set(r.entries.map((e) => e.name)),
    dirs: new Set(r.entries.filter((e) => isDirLike(e)).map((e) => e.name)),
  }
}

/** SPEC §6.0 POST /api/fs body that reopens this file system (other features keep it to survive a reload). */
export function openRequestOf(ctx: FsContext): FsOpenRequest | undefined {
  switch (ctx.source.kind) {
    case 'session':
      return { sessionId: ctx.source.sessionId }
    case 'connection':
      return { connectionId: ctx.source.connectionId }
    case 'local':
      return { local: true }
    default:
      return undefined
  }
}

/** Callback used to select / reveal an item after an operation (set by the calling view). */
export type RevealFn = (path: string, opts?: { rename?: boolean }) => void

// ---------------------------------------------------------------------------------------------------------------------
// create / rename / delete
// ---------------------------------------------------------------------------------------------------------------------

export async function createFolder(ctx: FsContext, dir: string, reveal?: RevealFn): Promise<void> {
  const name = await prompt({
    title: 'New folder',
    description: `Create a folder in ${dir}`,
    label: 'Folder name',
    defaultValue: 'New folder',
    confirmLabel: 'Create',
    validate: (v) => validateName(v),
  })
  if (name == null) return
  const path = joinPath(dir, name.trim())
  try {
    await withFs(ctx.key, (id) => fsApi.mkdir(id, path, false))
    invalidateDir(ctx, dir)
    reveal?.(path)
  } catch (err) {
    reportError(isConflict(err) ? `“${name.trim()}” already exists` : 'Could not create the folder', err)
  }
}

export async function createFile(ctx: FsContext, dir: string, reveal?: RevealFn): Promise<void> {
  const name = await prompt({
    title: 'New file',
    description: `Create an empty file in ${dir}`,
    label: 'File name',
    defaultValue: 'new-file.txt',
    confirmLabel: 'Create',
    validate: (v) => validateName(v),
  })
  if (name == null) return
  const path = joinPath(dir, name.trim())
  try {
    const { names } = await listNames(ctx, dir)
    if (names.has(name.trim())) {
      toast.error(`“${name.trim()}” already exists`)
      reveal?.(path)
      return
    }
    await withFs(ctx.key, (id) => fsApi.touch(id, path))
    invalidateDir(ctx, dir)
    reveal?.(path)
  } catch (err) {
    reportError('Could not create the file', err)
  }
}

/** Rename an entry in place (inline rename or dialog). Resolves the new path, or null. */
export async function renameEntry(ctx: FsContext, entry: FileEntry, newName: string): Promise<string | null> {
  const name = newName.trim()
  if (!name || name === entry.name) return null
  const err = validateName(name)
  if (err) {
    toast.error(err)
    return null
  }
  const dir = dirname(entry.path)
  const to = joinPath(dir, name)
  try {
    const { names } = await listNames(ctx, dir)
    if (names.has(name) && name.toLowerCase() !== entry.name.toLowerCase()) {
      const ok = await confirm({
        title: `Replace “${name}”?`,
        description: `An item named “${name}” already exists in ${dir}. Renaming replaces it.`,
        confirmLabel: 'Replace',
        destructive: true,
      })
      if (!ok) return null
    }
    await withFs(ctx.key, (id) => fsApi.rename(id, entry.path, to))
    invalidateDir(ctx, dir)
    return to
  } catch (e) {
    reportError(`Could not rename “${entry.name}”`, e)
    return null
  }
}

export async function renameWithDialog(ctx: FsContext, entry: FileEntry, reveal?: RevealFn): Promise<void> {
  const name = await prompt({
    title: `Rename “${entry.name}”`,
    label: 'New name',
    defaultValue: entry.name,
    confirmLabel: 'Rename',
    validate: (v) => validateName(v),
  })
  if (name == null) return
  const to = await renameEntry(ctx, entry, name)
  if (to) reveal?.(to)
}

export async function deleteEntries(ctx: FsContext, entries: FileEntry[]): Promise<boolean> {
  const list = realEntries(entries)
  if (!list.length) return false
  const dirs = list.filter((e) => e.type === 'dir')
  if (filesSettings.get().confirmDelete) {
    const what = describeSelection(list)
    const ok = await confirm({
      title: `Delete ${what}?`,
      description: dirs.length
        ? `${dirs.length === list.length && list.length === 1 ? 'The folder and everything in it' : 'Folders are deleted with their contents and'} cannot be recovered.`
        : `This permanently deletes ${list.length === 1 ? 'the file' : 'the files'} from ${ctx.label}.`,
      confirmLabel: 'Delete',
      destructive: true,
    })
    if (!ok) return false
  }
  const pending = toast.loading(`Deleting ${describeSelection(list)}…`)
  try {
    await withFs(ctx.key, (id) =>
      fsApi.remove(
        id,
        list.map((e) => e.path),
        dirs.length > 0,
      ),
    )
    toast.success(`Deleted ${describeSelection(list)}`, { id: pending })
    return true
  } catch (err) {
    toast.dismiss(pending)
    reportError(`Could not delete ${describeSelection(list)}`, err)
    return false
  } finally {
    for (const d of new Set(list.map((e) => dirname(e.path)))) invalidateDir(ctx, d)
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// open / edit / preview / download / upload
// ---------------------------------------------------------------------------------------------------------------------

export function previewEntry(ctx: FsContext, entry: FileEntry, siblings: FileEntry[] = []): void {
  openFilesDialog({ kind: 'preview', ctx, entry, siblings })
}

/** Open a file in the built-in editor (FILE-10; editor.open is provided by the editor feature). */
export async function editEntry(ctx: FsContext, entry: FileEntry, siblings: FileEntry[] = []): Promise<void> {
  if (isCommandEnabled('editor.open')) {
    await runCommand('editor.open', { fsId: ctx.handle.id, path: entry.path, label: ctx.label, source: openRequestOf(ctx) })
    return
  }
  if (previewKind(entry)) {
    toast.info('The editor is not available yet — showing a preview')
    previewEntry(ctx, entry, siblings)
    return
  }
  toast.info('The editor is not available yet — downloading instead')
  downloadEntries(ctx, [entry], dirname(entry.path))
}

/** Double-click / Enter on a file (settings: edit | preview | download). */
export function openFile(ctx: FsContext, entry: FileEntry, siblings: FileEntry[] = []): void {
  if (entry.type === 'symlink' && entry.linkType === 'broken') {
    toast.error(`“${entry.name}” is a broken link`, { description: entry.linkTarget ? `It points to ${entry.linkTarget}, which does not exist.` : undefined })
    return
  }
  const kind = fileKind(entry)
  const pk = previewKind(entry)
  switch (filesSettings.get().doubleClickAction) {
    case 'download':
      downloadEntries(ctx, [entry], dirname(entry.path))
      return
    case 'preview':
      if (pk) previewEntry(ctx, entry, siblings)
      else if (isBinaryKind(kind)) downloadEntries(ctx, [entry], dirname(entry.path))
      else void editEntry(ctx, entry, siblings)
      return
    default:
      // Double-click edits text files; media opens in the viewer, other binaries download.
      if (!isBinaryKind(kind)) void editEntry(ctx, entry, siblings)
      else if (pk) previewEntry(ctx, entry, siblings)
      else downloadEntries(ctx, [entry], dirname(entry.path))
  }
}

export async function uploadHere(ctx: FsContext, dir: string, folder = false): Promise<void> {
  const { files, dirs } = await pickLocalFiles(folder)
  if (!files.length && !dirs.length) return
  await planUpload(ctx, dir, files, dirs)
}

// ---------------------------------------------------------------------------------------------------------------------
// copy / move between folders and file systems
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Copy or move entries into `dstDir` (FILE-5, FILE-13, FILE-14). Same file system: server-side copy / rename.
 * Another file system: a queued server transfer (data never passes through the browser). Existing names are
 * resolved with the conflict policy.
 */
export async function transferEntries(src: FsContext, entries: FileEntry[], dst: FsContext, dstDir: string, mode: 'copy' | 'move'): Promise<boolean> {
  const list = realEntries(entries)
  const dir = normalizePath(dstDir)
  if (!list.length) return false
  const sameFs = src.key === dst.key
  if (sameFs) {
    if (list.every((e) => dirname(e.path) === dir)) {
      if (mode === 'move') {
        toast.info(list.length === 1 ? `“${list[0].name}” is already in this folder` : 'The items are already in this folder')
        return false
      }
      // Copy into the same folder = duplicate ("name (copy)", named by the server).
      return duplicateEntries(src, list, dir)
    }
    const into = list.find((e) => isDirLike(e) && isInside(dir, e.path))
    if (into) {
      toast.error(`Cannot ${mode} “${into.name}” into itself`)
      return false
    }
  }
  let existing: { names: Set<string>; dirs: Set<string> }
  try {
    existing = await listNames(dst, dir)
  } catch (err) {
    reportError(`Cannot ${mode} to ${dir}`, err)
    return false
  }
  const conflicts = list.filter((e) => existing.names.has(e.name)).map((e) => e.name)
  let policy: ConflictPolicy = 'overwrite'
  if (conflicts.length) {
    const pref = filesSettings.get().conflictPolicy
    const chosen = pref === 'ask' ? await askConflict(conflicts, `${dir} on ${dst.label}`, mode) : pref
    if (!chosen) return false
    policy = chosen
  }
  let todo = list
  if (policy === 'skip') todo = list.filter((e) => !existing.names.has(e.name))
  if (!todo.length) {
    toast.info('Nothing to do', { description: 'Every item already exists and was skipped.' })
    return false
  }
  const label = todo.length === 1 ? todo[0].name : plural(todo.length, 'item')

  // Same file system, move: rename into the folder.
  if (sameFs && mode === 'move') {
    const taken = new Set(existing.names)
    let failed = 0
    for (const e of todo) {
      let name = e.name
      if (policy === 'rename' && taken.has(name)) name = uniqueName(name, taken)
      taken.add(name)
      try {
        await withFs(src.key, (id) => fsApi.rename(id, e.path, joinPath(dir, name)))
      } catch (err) {
        failed++
        reportError(`Could not move “${e.name}”`, err)
      }
    }
    for (const d of new Set(todo.map((e) => dirname(e.path)))) invalidateDir(src, d)
    invalidateDir(dst, dir)
    if (failed < todo.length) toast.success(`Moved ${label}`, { description: `to ${dir}` })
    return failed === 0
  }

  // Same file system: fast server-side copy (cp -a when available). "Keep both" needs the transfer queue (renames).
  if (sameFs && mode === 'copy' && policy !== 'rename') {
    const pending = toast.loading(`Copying ${label}…`)
    try {
      await withFs(src.key, (id) =>
        fsApi.copy(
          id,
          todo.map((e) => e.path),
          dir,
          policy === 'overwrite',
        ),
      )
      toast.success(`Copied ${label}`, { id: pending, description: `to ${dir}` })
      invalidateDir(dst, dir)
      return true
    } catch (err) {
      toast.dismiss(pending)
      reportError(`Could not copy ${label}`, err)
      return false
    }
  }

  // Everything else goes through the transfer queue (progress, cancel, retry). The server needs live handle ids
  // (a view's handle may have idled out or been replaced by a reconnect).
  try {
    const [srcFs, dstFs] = await Promise.all([liveFsId(src), liveFsId(dst)])
    await startServerTransfer(
      {
        srcFs,
        srcPaths: todo.map((e) => e.path),
        dstFs,
        dstDir: dir,
        overwrite: policy as OverwritePolicy,
      },
      { move: mode === 'move' ? { srcKey: src.key } : undefined, label, dstLabel: dst.label },
    )
    return true
  } catch (err) {
    reportError(`Could not start the ${mode}`, err)
    return false
  }
}

/** Copies next to the originals ("name (copy)"). */
async function duplicateEntries(ctx: FsContext, entries: FileEntry[], dir: string): Promise<boolean> {
  const label = entries.length === 1 ? `“${entries[0].name}”` : plural(entries.length, 'item')
  try {
    await withFs(ctx.key, (id) =>
      fsApi.copy(
        id,
        entries.map((e) => e.path),
        dir,
      ),
    )
    toast.success(`Duplicated ${label}`)
    invalidateDir(ctx, dir)
    return true
  } catch (err) {
    reportError(`Could not duplicate ${label}`, err)
    return false
  }
}

/** Paste the in-app clipboard into a folder. */
export async function pasteInto(ctx: FsContext, dir: string): Promise<void> {
  const clip = getFileClipboard()
  if (!clip) {
    toast.info('The file clipboard is empty', { description: 'Copy or cut files first (Ctrl+C / Ctrl+X).' })
    return
  }
  const ok = await transferEntries(clip.ctx, clip.entries, ctx, dir, clip.op === 'cut' ? 'move' : 'copy')
  if (ok && clip.op === 'cut') clearFileClipboard()
}

// ---------------------------------------------------------------------------------------------------------------------
// links, archives, misc
// ---------------------------------------------------------------------------------------------------------------------

export async function createSymlink(ctx: FsContext, dir: string, target?: FileEntry): Promise<void> {
  const targetPath = await prompt({
    title: 'Create symbolic link',
    description: 'The link points to this path (absolute, or relative to the link’s folder).',
    label: 'Target',
    defaultValue: target ? target.path : '',
    placeholder: '/path/to/target',
    confirmLabel: 'Next',
    validate: (v) => (v.trim() ? null : 'Enter the target path'),
  })
  if (targetPath == null) return
  const suggested = target ? `${target.name}-link` : `${basename(targetPath.trim()) || 'link'}-link`
  const name = await prompt({
    title: 'Create symbolic link',
    description: `A link in ${dir} pointing to ${targetPath.trim()}`,
    label: 'Link name',
    defaultValue: suggested,
    confirmLabel: 'Create link',
    validate: (v) => validateName(v),
  })
  if (name == null) return
  try {
    await withFs(ctx.key, (id) => fsApi.symlink(id, targetPath.trim(), joinPath(dir, name.trim())))
    invalidateDir(ctx, dir)
  } catch (err) {
    reportError('Could not create the link', err)
  }
}

export async function extractHere(ctx: FsContext, entry: FileEntry, destDir?: string): Promise<void> {
  const dir = destDir ?? dirname(entry.path)
  const pending = toast.loading(`Extracting ${entry.name}…`)
  try {
    await withFs(ctx.key, (id) => fsApi.extract(id, entry.path, dir))
    toast.success(`Extracted ${entry.name}`, { id: pending, description: `into ${dir}` })
    invalidateDir(ctx, dir)
  } catch (err) {
    toast.dismiss(pending)
    reportError(`Could not extract ${entry.name}`, err)
  }
}

export async function copyPaths(entries: FileEntry[], fallbackDir?: string): Promise<void> {
  const list = realEntries(entries)
  const text = list.length ? list.map((e) => e.path).join('\n') : (fallbackDir ?? '')
  if (!text) return
  if (await copyText(text)) toast.success(list.length > 1 ? `Copied ${list.length} paths` : 'Path copied', { description: list.length > 1 ? undefined : text })
  else toast.error('Could not copy to the clipboard')
}

export async function copyNames(entries: FileEntry[]): Promise<void> {
  const list = realEntries(entries)
  if (!list.length) return
  if (await copyText(list.map((e) => e.name).join('\n'))) toast.success(list.length > 1 ? `Copied ${list.length} names` : 'Name copied')
}

/** scp://user@host:port/path (or file:// for the local host). */
export function scpUrl(ctx: FsContext, path: string): string {
  if (ctx.source.kind === 'local' || ctx.handle.kind === 'local') return `file://${encodePathForUrl(path)}`
  const h = ctx.host
  if (!h?.host) return path
  const scheme = ctx.handle.kind === 'ftp' ? 'ftp' : ctx.handle.kind === 's3' ? 's3' : 'scp'
  const user = h.username ? `${encodeURIComponent(h.username)}@` : ''
  const port = h.port && h.port !== 22 && scheme === 'scp' ? `:${h.port}` : h.port && scheme === 'ftp' && h.port !== 21 ? `:${h.port}` : ''
  const host = h.host.includes(':') && !h.host.startsWith('[') ? `[${h.host}]` : h.host
  return `${scheme}://${user}${host}${port}${encodePathForUrl(path.startsWith('/') ? path : `/${path}`)}`
}

export async function copyScpUrls(ctx: FsContext, entries: FileEntry[], fallbackDir: string): Promise<void> {
  const list = realEntries(entries)
  const text = (list.length ? list.map((e) => e.path) : [fallbackDir]).map((p) => scpUrl(ctx, p)).join('\n')
  if (await copyText(text)) toast.success('URL copied', { description: text.split('\n')[0] })
  else toast.error('Could not copy to the clipboard')
}

/**
 * "Open terminal here" (FILE-2): SSH-browser → type a quoted `cd` into the session's terminal; local host → a new
 * local shell started in the folder.
 */
export async function openTerminalHere(ctx: FsContext, dir: string, opts: { force?: boolean } = {}): Promise<void> {
  const sessionId = ctx.sessionId
  if (sessionId) {
    const tabs = getTerminalsBySession(sessionId)
    // Only type into a shell waiting at its prompt (never into vim, a running command or a half-typed line).
    const readiness = shellReadiness(sessionId)
    if (!opts.force && readiness !== 'prompt') {
      toast.warning(readiness === 'busy' ? 'The terminal is busy' : 'The terminal is not open here', {
        description:
          readiness === 'busy'
            ? 'A program is running or a command is being typed, so “cd” was not sent.'
            : 'The shell cannot be checked for a prompt, so “cd” was not sent.',
        action: { label: 'Send anyway', onClick: () => void openTerminalHere(ctx, dir, { force: true }) },
      })
      return
    }
    const ok = await sendToSession(sessionId, `cd ${shellQuote(dir)}\r`)
    if (!ok) {
      toast.error('Could not send the command to the terminal')
      return
    }
    if (tabs[0]) {
      focusTab(tabs[0].tabId)
      tabs[0].focus()
    }
    return
  }
  if (ctx.source.kind === 'local' || ctx.handle.kind === 'local') {
    await openQuick({ protocol: 'local', name: basename(dir) || dir, options: { cwd: dir } })
    return
  }
  toast.info('No terminal for this location', { description: 'Open an SSH session to the host to get a terminal.' })
}

export function checksumOf(ctx: FsContext, entry: FileEntry): void {
  openFilesDialog({ kind: 'checksum', ctx, entry })
}

export function permissionsOf(ctx: FsContext, entries: FileEntry[]): void {
  const list = realEntries(entries)
  if (list.length) openFilesDialog({ kind: 'permissions', ctx, entries: list })
}

export function propertiesOf(ctx: FsContext, entry: FileEntry): void {
  openFilesDialog({ kind: 'properties', ctx, entry })
}

export function compressEntries(ctx: FsContext, entries: FileEntry[], dir: string): void {
  const list = realEntries(entries)
  if (list.length) openFilesDialog({ kind: 'compress', ctx, entries: list, dir })
}

export function searchIn(ctx: FsContext, dir: string, viewId?: string): void {
  openFilesDialog({ kind: 'search', ctx, path: dir, viewId })
}

export function compareFolders(left: { ctx: FsContext; path: string }, right?: { ctx: FsContext; path: string }): void {
  openFilesDialog({ kind: 'compare', left, right })
}

/** Human description of an error for inline states. */
export function describeError(err: unknown): { title: string; description: string; permission: boolean; notFound: boolean } {
  const message = errorMessage(err)
  if (isPermissionDenied(err)) return { title: 'Permission denied', description: message, permission: true, notFound: false }
  if (isApiError(err) && err.status === 404) return { title: 'Folder not found', description: message, permission: false, notFound: true }
  return { title: 'Could not load the folder', description: message, permission: false, notFound: false }
}
