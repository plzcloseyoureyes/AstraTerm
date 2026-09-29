/* File metadata the editor tracks for conflict detection (FILE-10): what was loaded / saved vs. what the server has. */
import type { FileEntry } from '@/api/types'

export interface FileStat {
  mtime?: string
  size: number
  mode?: number
  perm?: string
  owner?: string
  group?: string
}

/**
 * Did the file change since `base` (what we loaded or saved)? Modification times are compared as instants (the same
 * time may be formatted differently by different calls); a size change within the same second counts too.
 */
export function remoteChanged(base: FileStat, now: Partial<FileEntry>): boolean {
  const a = base.mtime ? Date.parse(base.mtime) : NaN
  const b = now.mtime ? Date.parse(now.mtime) : NaN
  const sizeChanged = typeof now.size === 'number' && now.size !== base.size
  if (Number.isNaN(a) || Number.isNaN(b)) return !base.mtime && sizeChanged
  return a !== b || sizeChanged
}

export function statOf(e: Partial<FileEntry> | undefined, fallback?: FileStat): FileStat {
  return {
    mtime: e?.mtime ?? fallback?.mtime,
    size: typeof e?.size === 'number' ? e.size : (fallback?.size ?? 0),
    mode: e?.mode ?? fallback?.mode,
    perm: e?.perm ?? fallback?.perm,
    owner: e?.owner ?? fallback?.owner,
    group: e?.group ?? fallback?.group,
  }
}
