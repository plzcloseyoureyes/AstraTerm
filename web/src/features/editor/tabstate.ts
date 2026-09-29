/*
 * State of an "editor" tab (EditorTab.tsx) that is not React: what the tab shows (Phase), the strip above the editor
 * (BannerState) and how change detection moves it, the compare view and crash backups. Pure — unit-tested in
 * __tests__/tab.test.mjs.
 */
import type { FileEntry } from '@/api/types'
import type { Eol } from './codec'
import type { DocRecord } from './docstore'
import { remoteChanged, type FileStat } from './filestat'
import type { TextMeta } from './save'

export const MiB = 1024 * 1024
/** Largest file the text editor opens. */
export const TEXT_MAX = 64 * MiB
/** Largest file the hex editor opens. */
export const HEX_MAX = 16 * MiB

export const DEFAULT_META: TextMeta = { encoding: 'utf-8', bom: false, eol: 'lf', mixedEol: false }

/** A file read into memory. */
export type LoadedDoc =
  | {
      kind: 'text'
      text: string
      meta: TextMeta
      /** Why this encoding was chosen (shown as a dismissible note). */
      note?: string
      truncated: boolean
      /** Decoding replaced bytes the encoding cannot represent: saving would change the file (opened read-only). */
      lossy?: boolean
    }
  | { kind: 'hex'; bytes: Uint8Array; truncated: boolean }

export type BinaryFile = { kind: 'binary'; bytes: Uint8Array; reason: string; size: number; stat: FileStat; truncated: boolean }
export type LoadError = { kind: 'error'; message: string; notFound?: boolean; handleGone?: boolean; isDir?: boolean; tooLarge?: boolean }

/** The outcome of reading the file (fileread.ts). */
export type ReadOutcome = { kind: 'ready'; doc: LoadedDoc; stat: FileStat } | { kind: 'large'; size: number } | BinaryFile | LoadError

/** What the tab shows. `version` changes whenever the editor must be re-created for a new document. */
export type Phase = { kind: 'loading' } | { kind: 'large'; size: number } | BinaryFile | LoadError | { kind: 'ready'; doc: LoadedDoc; version: number }

/** The strip above the editor. */
export type BannerState =
  | { kind: 'changed'; mtime?: string; size?: number }
  | { kind: 'deleted' }
  | { kind: 'conflict' }
  | { kind: 'denied'; message: string }
  | { kind: 'error'; message: string }
  | { kind: 'gone' }

/** A result of the change-detection poll. */
export type WatchEvent =
  | { kind: 'stat'; stat: Partial<FileEntry>; base: FileStat; ignoredMtime: string | null }
  | { kind: 'missing'; handleGone: boolean }

/**
 * The banner after a change-detection poll: a change (unless the user ignored that version) shows "changed" — but
 * never over a conflict or permission problem the user still has to resolve; the file being back as it was clears the
 * "changed" / "deleted" / "gone" banners; a missing file (or handle) says so, except over a conflict.
 */
export function watchBanner(prev: BannerState | null, ev: WatchEvent): BannerState | null {
  if (ev.kind === 'missing') return prev?.kind === 'conflict' ? prev : { kind: ev.handleGone ? 'gone' : 'deleted' }
  if (remoteChanged(ev.base, ev.stat)) {
    if (ev.stat.mtime === ev.ignoredMtime) return prev
    return prev && (prev.kind === 'conflict' || prev.kind === 'denied') ? prev : { kind: 'changed', mtime: ev.stat.mtime, size: ev.stat.size }
  }
  return prev && (prev.kind === 'changed' || prev.kind === 'deleted' || prev.kind === 'gone') ? null : prev
}

/** The compare view (in-tab merge of the server version into mine). */
export interface MergeState {
  /** Server version, "\n"-joined (what the comparison shows). */
  theirs: string
  /** Server version with its own line endings (kept for the lines taken from it). */
  theirsRaw: string
  theirsStat: FileStat
  reason: 'conflict' | 'external' | 'compare'
  mine: string
}

/** Unsaved changes recovered from IndexedDB (docstore.ts). */
export interface BackupRecord {
  content: string
  meta: TextMeta
  /** mtime of the file the changes were made to. */
  baseMtime?: string
  savedAt: number
}

/** Reads a stored backup document; null when it has no text. */
export function backupFromDoc(rec: Pick<DocRecord, 'content' | 'meta' | 'updatedAt'> | null | undefined): BackupRecord | null {
  if (!rec || typeof rec.content !== 'string') return null
  const m = (rec.meta ?? {}) as Partial<TextMeta> & { baseMtime?: string; savedAt?: number }
  return {
    content: rec.content,
    meta: { encoding: m.encoding ?? 'utf-8', bom: !!m.bom, eol: (m.eol as Eol) ?? 'lf', mixedEol: !!m.mixedEol },
    baseMtime: m.baseMtime,
    savedAt: m.savedAt ?? rec.updatedAt,
  }
}

/**
 * IndexedDB keys of a file's crash backup: a stable one (saved connection, else host label — survives handle changes
 * and server restarts) first, then the handle-based one (`own`).
 */
export function backupKeys(path: string, own: string, origin: { connectionId?: string; label?: string }): string[] {
  const stable = origin.connectionId ? `conn:${origin.connectionId}` : origin.label ? `label:${origin.label}` : null
  return stable ? [`backup:${stable}:${path}`, own] : [own]
}
