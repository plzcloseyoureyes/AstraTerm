/*
 * In-app file clipboard: Copy / Cut in any browser, Paste into any other (same file system: server-side copy or
 * rename; another file system: a queued server transfer).
 */
import { create } from 'zustand'
import type { FileEntry } from '@/api/types'
import type { FsContext } from './types'

export interface FileClipboard {
  op: 'copy' | 'cut'
  ctx: FsContext
  entries: FileEntry[]
  dir: string
  at: number
}

const useFileClipboard = create<{ clip: FileClipboard | null }>(() => ({ clip: null }))

export function setFileClipboard(op: 'copy' | 'cut', ctx: FsContext, entries: FileEntry[], dir: string): void {
  const list = entries.filter((e) => e.name !== '..')
  if (!list.length) return
  useFileClipboard.setState({ clip: { op, ctx, entries: list, dir, at: Date.now() } })
}

export function clearFileClipboard(): void {
  useFileClipboard.setState({ clip: null })
}

export function getFileClipboard(): FileClipboard | null {
  return useFileClipboard.getState().clip
}

/** Is `path` of the fs `key` currently cut (rendered dimmed)? */
export function useIsCut(key: string | undefined, path: string): boolean {
  return useFileClipboard((s) => !!key && s.clip?.op === 'cut' && s.clip.ctx.key === key && s.clip.entries.some((e) => e.path === path))
}
