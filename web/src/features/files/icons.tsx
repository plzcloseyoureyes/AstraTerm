/*
 * File type icons for the file list (kind → lucide icon + tint), with a small arrow badge for symbolic links.
 */
import {
  File,
  FileArchive,
  FileBraces,
  FileCode,
  FileCog,
  FileImage,
  FileKey,
  FileMusic,
  FileQuestionMark,
  FileSpreadsheet,
  FileTerminal,
  FileText,
  FileType,
  FileVideoCamera,
  FileX,
  Folder,
  FolderSymlink,
  CornerLeftUp,
  Link,
  type LucideIcon,
} from 'lucide-react'
import type { FileEntry } from '@/api/types'
import { cn } from '@/lib/utils'
import { fileKind, type FileKind } from './format'

const KIND_ICON: Record<FileKind, { icon: LucideIcon; tint: string }> = {
  dir: { icon: Folder, tint: 'text-amber-500 dark:text-amber-400' },
  image: { icon: FileImage, tint: 'text-fuchsia-500 dark:text-fuchsia-400' },
  video: { icon: FileVideoCamera, tint: 'text-rose-500 dark:text-rose-400' },
  audio: { icon: FileMusic, tint: 'text-pink-500 dark:text-pink-400' },
  archive: { icon: FileArchive, tint: 'text-orange-600 dark:text-orange-400' },
  pdf: { icon: FileType, tint: 'text-red-500 dark:text-red-400' },
  code: { icon: FileCode, tint: 'text-sky-600 dark:text-sky-400' },
  script: { icon: FileTerminal, tint: 'text-emerald-600 dark:text-emerald-400' },
  config: { icon: FileCog, tint: 'text-slate-500 dark:text-slate-400' },
  data: { icon: FileBraces, tint: 'text-yellow-600 dark:text-yellow-400' },
  text: { icon: FileText, tint: 'text-muted-foreground' },
  spreadsheet: { icon: FileSpreadsheet, tint: 'text-green-600 dark:text-green-400' },
  document: { icon: FileText, tint: 'text-blue-600 dark:text-blue-400' },
  key: { icon: FileKey, tint: 'text-amber-600 dark:text-amber-300' },
  binary: { icon: File, tint: 'text-muted-foreground' },
  other: { icon: File, tint: 'text-muted-foreground' },
}

export function kindIcon(kind: FileKind): { icon: LucideIcon; tint: string } {
  return KIND_ICON[kind]
}

/** Icon of a list entry (the ".." row gets an "up" arrow). */
export function FileIcon({ entry, parent, className }: { entry: FileEntry; parent?: boolean; className?: string }) {
  if (parent) return <CornerLeftUp className={cn('size-4 shrink-0 text-muted-foreground', className)} aria-hidden />
  const broken = entry.type === 'symlink' && entry.linkType === 'broken'
  if (broken) return <FileX className={cn('size-4 shrink-0 text-destructive/80', className)} aria-hidden />
  const kind = fileKind(entry)
  if (entry.type === 'symlink' && kind === 'dir') return <FolderSymlink className={cn('size-4 shrink-0', KIND_ICON.dir.tint, className)} aria-hidden />
  if (entry.type === 'other') return <FileQuestionMark className={cn('size-4 shrink-0 text-muted-foreground', className)} aria-hidden />
  const { icon: Icon, tint } = KIND_ICON[kind]
  return (
    <span className={cn('relative inline-flex shrink-0', className)} aria-hidden>
      <Icon className={cn('size-4', tint, kind === 'dir' && 'fill-current/20')} />
      {entry.type === 'symlink' && (
        <span className="absolute -bottom-0.5 -left-0.5 flex size-2.5 items-center justify-center rounded-[3px] bg-panel">
          <Link className="size-2 text-foreground/80" strokeWidth={3} />
        </span>
      )}
    </span>
  )
}
