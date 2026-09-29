/*
 * The file list context menu: registry target `file` (other features — the editor — add items) with the built-in
 * items contributed by this feature (registered at order 0 in ../index.ts).
 */
import {
  Archive,
  Clipboard,
  ClipboardPaste,
  Copy,
  Download,
  ExternalLink,
  Eye,
  FileArchive,
  FilePlus,
  FolderGit2,
  FolderOpen,
  FolderPlus,
  FolderUp,
  Hash,
  Info,
  Link,
  PackageOpen,
  Pencil,
  RefreshCw,
  Scissors,
  Search,
  Shield,
  SquareTerminal,
  Star,
  StarOff,
  Trash,
  Upload,
} from 'lucide-react'
import type { FileEntry } from '@/api/types'
import { getContextMenuItems, type FileContext, type MenuItem } from '@/app/registry'
import { copyNames, copyPaths, copyScpUrls, openFile, uploadHere } from '../actions'
import { getFileClipboard } from '../clipboard'
import { downloadEntries } from '../download'
import { isDirLike, isExtractable, previewKind } from '../format'
import { openFolderInTab } from '../open'
import { basename } from '../paths'
import { isBookmarked, toggleBookmark } from '../settings'
import type { FsContext } from '../types'
import { getController, type BrowserController } from './controller'

/** Extra fields this feature puts on the registry's FileContext. */
export interface FilesMenuContext extends FileContext {
  viewId?: string
  ctx?: FsContext
}

/** Items for a right-click in a view (the selection, or the folder itself on empty space). */
export function buildFileMenu(c: BrowserController): MenuItem[] {
  const ctx = c.ctx
  const dir = c.dir
  if (!ctx || !dir) return []
  const entries = c.selectedEntries()
  const context: FilesMenuContext = { fsId: ctx.handle.id, path: dir, entries, viewId: c.viewId, ctx }
  return getContextMenuItems('file', context)
}

const sep: MenuItem = { type: 'separator' }

function folderEntry(dir: string): FileEntry {
  return { name: basename(dir) || dir, path: dir, type: 'dir', size: 0, mode: 0o40755, perm: '', mtime: '', hidden: false }
}

/** Built-in items of the `file` context menu. */
export function builtinFileItems(fctx: FileContext): MenuItem[] {
  const f = fctx as FilesMenuContext
  const c = getController(f.viewId)
  const ctx = f.ctx ?? c?.ctx ?? undefined
  if (!c || !ctx) return []
  const dir = f.path
  const entries = (f.entries ?? []).filter((e) => e.name !== '..')
  const caps = ctx.handle.capabilities
  const clip = getFileClipboard()
  const canTerminal = !!ctx.sessionId || ctx.source.kind === 'local' || ctx.handle.kind === 'local'
  const n = entries.length

  if (n === 0) {
    const marked = isBookmarked(ctx.placeKey, dir)
    return [
      { label: 'Refresh', icon: RefreshCw, shortcut: '$mod+r', run: () => c.refresh() },
      sep,
      { label: 'New folder…', icon: FolderPlus, shortcut: 'F7', run: () => c.newFolder() },
      { label: 'New file…', icon: FilePlus, run: () => c.newFile() },
      { label: 'Upload files here…', icon: Upload, run: () => c.upload(false) },
      { label: 'Upload folder here…', icon: FolderUp, run: () => c.upload(true) },
      { label: clip ? `Paste ${clip.entries.length === 1 ? `“${clip.entries[0].name}”` : `${clip.entries.length} items`}` : 'Paste', icon: ClipboardPaste, shortcut: '$mod+v', disabled: !clip, run: () => c.paste() },
      sep,
      { label: 'Download folder as zip', icon: FileArchive, run: () => downloadEntries(ctx, [folderEntry(dir)], dir, { zip: true }) },
      { label: 'Search in this folder…', icon: Search, disabled: caps.search === false, run: () => c.search() },
      { label: 'Compare with…', icon: FolderGit2, run: () => c.compare() },
      { label: marked ? 'Remove bookmark' : 'Bookmark this folder', icon: marked ? StarOff : Star, run: () => void toggleBookmark(ctx.placeKey, dir) },
      sep,
      ...(canTerminal ? [{ label: 'Open terminal here', icon: SquareTerminal, run: () => c.terminalHere() } as MenuItem] : []),
      { label: 'Copy path', icon: Copy, run: () => void copyPaths([], dir) },
      { label: 'Properties', icon: Info, shortcut: 'Alt+Enter', run: () => c.properties() },
    ]
  }

  const one = n === 1 ? entries[0] : undefined
  const oneDir = !!one && isDirLike(one)
  const oneFile = !!one && !oneDir
  const items: MenuItem[] = []

  if (one) {
    items.push({
      label: 'Open',
      icon: oneDir ? FolderOpen : Pencil,
      shortcut: 'Enter',
      run: () => (oneDir ? c.navigate(one.path) : openFile(ctx, one, c.files())),
    })
    if (oneFile && previewKind(one)) items.push({ label: 'Open with preview', icon: Eye, shortcut: 'F3', run: () => c.preview(one) })
    if (oneDir) items.push({ label: 'Open in new tab', icon: ExternalLink, run: () => void openFolderInTab(ctx, one.path) })
    items.push(sep)
  }

  items.push({
    label: oneFile ? 'Download' : 'Download as zip',
    icon: Download,
    run: () => downloadEntries(ctx, entries, dir),
  })
  if (oneFile) items.push({ label: 'Download as zip', icon: FileArchive, run: () => downloadEntries(ctx, entries, dir, { zip: true }) })
  items.push(
    oneDir
      ? { label: `Upload into “${one!.name}”…`, icon: Upload, run: () => void uploadHere(ctx, one!.path) }
      : { label: 'Upload here…', icon: Upload, run: () => c.upload(false) },
    { label: 'New folder…', icon: FolderPlus, shortcut: 'F7', run: () => c.newFolder() },
    { label: 'New file…', icon: FilePlus, run: () => c.newFile() },
    sep,
    { label: 'Copy', icon: Copy, shortcut: '$mod+c', run: () => c.copyToClipboard('copy') },
    { label: 'Cut', icon: Scissors, shortcut: '$mod+x', run: () => c.copyToClipboard('cut') },
    {
      label: oneDir && clip ? `Paste into “${one!.name}”` : 'Paste',
      icon: ClipboardPaste,
      shortcut: '$mod+v',
      disabled: !clip,
      run: () => c.paste(oneDir ? one!.path : undefined),
    },
    { label: 'Rename', icon: Pencil, shortcut: 'F2', disabled: n !== 1, run: () => c.startRename(one?.path) },
    { label: n > 1 ? `Delete ${n} items…` : 'Delete…', icon: Trash, shortcut: 'Delete', danger: true, run: () => void c.deleteSelection() },
    sep,
    { label: 'Permissions…', icon: Shield, disabled: !caps.chmod && !caps.chown, run: () => c.permissions() },
    { label: 'Create symbolic link…', icon: Link, disabled: !caps.symlink, run: () => c.symlink() },
  )
  if (caps.archive ?? caps.exec) items.push({ label: 'Compress…', icon: Archive, run: () => c.compress() })
  if (oneFile && (caps.extract ?? caps.exec) && isExtractable(one!.name)) items.push({ label: 'Extract here', icon: PackageOpen, run: () => c.extract(one) })
  if (oneFile) items.push({ label: 'Checksum…', icon: Hash, disabled: !caps.checksum, run: () => c.checksum() })
  items.push(
    sep,
    {
      type: 'submenu',
      label: 'Copy path',
      icon: Clipboard,
      items: [
        { label: n > 1 ? 'Full paths' : 'Full path', run: () => void copyPaths(entries) },
        { label: n > 1 ? 'Names' : 'Name', run: () => void copyNames(entries) },
        { label: ctx.source.kind === 'local' ? 'As file:// URL' : 'As scp:// URL', run: () => void copyScpUrls(ctx, entries, dir) },
      ],
    },
  )
  if (canTerminal) items.push({ label: oneDir ? 'Open terminal in this folder' : 'Open terminal here', icon: SquareTerminal, run: () => c.terminalHere(one) })
  if (oneDir) items.push({ label: 'Compare with…', icon: FolderGit2, run: () => c.compare(one) })
  items.push({ label: 'Properties', icon: Info, shortcut: 'Alt+Enter', run: () => c.properties() })
  return items
}
