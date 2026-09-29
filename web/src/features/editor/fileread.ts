/*
 * Reading a remote file for the "editor" tab (FILE-10, FILE-23): stat (following symlinks, so conflict checks compare
 * the target), size limits, text vs. binary sniffing, decoding with the chosen / detected encoding and line-ending
 * detection. The fs calls are passed in (EditorTab routes them through its handle, re-opening an expired one), which
 * keeps this module free of React and testable (__tests__/tab.test.mjs).
 */
import type { FileEntry, FileReadResult } from '@/api/types'
import { isApiError } from '@/api/client'
import { errorMessage, formatBytes } from '@/lib/utils'
import { isHandleGone, isNotFound, isTooLarge } from './api'
import { base64ToBytes, decodeBytes, detectEol, encodingInfo, roundTrips, sniffBytes, utf8ByteLength, type Eol } from './codec'
import { statOf } from './filestat'
import { HEX_MAX, TEXT_MAX, type ReadOutcome } from './tabstate'
import type { EditorMode } from './types'

/** The file operations reading needs, bound to the tab's file system handle. */
export interface DocFs {
  stat(path: string): Promise<FileEntry>
  realpath(path: string): Promise<{ path: string }>
  read(path: string, maxBytes: number): Promise<FileReadResult>
}

export interface ReadOptions {
  mode: EditorMode
  /** Decode with this encoding instead of detecting one (binary sniffing is skipped too). */
  encoding?: string
  /** Open even when larger than `largeBytes`. */
  force?: boolean
  /** Files above this size ask before opening (settings.editor.largeFileMiB). */
  largeBytes: number
  /** Line ending of documents without any (settings.editor.defaultEol). */
  defaultEol: Eol
}

/** The outcome, plus the path to stat for change detection: the symlink's target when the file is a symlink. */
export interface ReadResult {
  outcome: ReadOutcome
  statPath: string
}

/** Stat errors that mean "stat is not supported here" (read anyway) rather than a failure. */
function statUnsupported(err: unknown): boolean {
  return isApiError(err) && err.status !== 0 && err.status < 500 && err.status !== 401 && err.status !== 403 && err.status !== 423
}

export async function readDocument(fs: DocFs, path: string, opts: ReadOptions): Promise<ReadResult> {
  let statPath = path
  const done = (outcome: ReadOutcome): ReadResult => ({ outcome, statPath })

  let st: FileEntry | undefined
  try {
    st = await fs.stat(path)
    if (st.type === 'symlink') {
      try {
        statPath = (await fs.realpath(path)).path || path
        st = { ...(await fs.stat(statPath)), path }
      } catch {
        statPath = path
      }
    }
  } catch (err) {
    if (isNotFound(err)) return done({ kind: 'error', message: errorMessage(err), notFound: !isHandleGone(err), handleGone: isHandleGone(err) })
    if (!statUnsupported(err)) return done({ kind: 'error', message: errorMessage(err) })
  }
  if (st?.type === 'dir' || (st?.type === 'symlink' && st.linkType === 'dir')) {
    return done({ kind: 'error', message: `${path} is a directory.`, isDir: true })
  }

  const size = st?.size ?? 0
  if (!opts.force && size > opts.largeBytes) return done({ kind: 'large', size })
  const hex = opts.mode === 'hex'
  const maxBytes = hex ? HEX_MAX : TEXT_MAX
  if (size > maxBytes) {
    return done({
      kind: 'error',
      tooLarge: true,
      message: `${formatBytes(size)} is more than the ${hex ? 'hex' : 'text'} editor can open (${formatBytes(maxBytes)}). Download the file instead.`,
    })
  }

  let res: FileReadResult
  try {
    res = await fs.read(path, maxBytes)
  } catch (err) {
    if (isTooLarge(err)) {
      const actual = (err.body as { size?: number } | undefined)?.size ?? size
      return done({ kind: 'error', tooLarge: true, message: `${formatBytes(actual)} is more than the editor can open (${formatBytes(maxBytes)}). Download the file instead.` })
    }
    return done({ kind: 'error', message: errorMessage(err), notFound: isNotFound(err) && !isHandleGone(err), handleGone: isHandleGone(err) })
  }

  const stat = statOf({ mtime: res.mtime, size: res.size, mode: res.mode }, statOf(st))
  const fileSize = typeof res.size === 'number' ? res.size : size

  // The server sent text (valid UTF-8).
  if (res.encoding !== 'base64') {
    let text = res.content ?? ''
    const truncated = fileSize > 0 && text.length < fileSize && utf8ByteLength(text) < fileSize
    if (hex) return done({ kind: 'ready', doc: { kind: 'hex', bytes: new TextEncoder().encode(text), truncated }, stat })
    let bom = false
    if (text.charCodeAt(0) === 0xfeff) {
      bom = true
      text = text.slice(1)
    }
    let encoding = 'utf-8'
    let lossy = false
    if (opts.encoding && opts.encoding !== 'utf-8') {
      const raw = new TextEncoder().encode((bom ? '﻿' : '') + text)
      const d = decodeBytes(raw, opts.encoding)
      text = d.text
      bom = d.bom
      encoding = opts.encoding
      lossy = !truncated && encodingInfo(encoding).canEncode && !roundTrips(raw, d.text, encoding, d.bom)
    }
    const e = detectEol(text, opts.defaultEol)
    return done({ kind: 'ready', doc: { kind: 'text', text, meta: { encoding, bom, eol: e.eol, mixedEol: e.mixed }, truncated, lossy }, stat })
  }

  // The server sent bytes.
  const bytes = base64ToBytes(res.content ?? '')
  const truncated = fileSize > bytes.length
  if (hex) return done({ kind: 'ready', doc: { kind: 'hex', bytes, truncated }, stat })
  let encoding = opts.encoding
  let note: string | undefined
  if (!encoding) {
    const sn = sniffBytes(bytes)
    if (sn.kind === 'binary') return done({ kind: 'binary', bytes, reason: sn.reason, size: fileSize, stat, truncated })
    encoding = sn.encoding
    note = sn.reason
  }
  const d = decodeBytes(bytes, encoding)
  const e = detectEol(d.text, opts.defaultEol)
  // Decode-only code pages (CJK) cannot be written back at all; saving them asks to convert to UTF-8.
  const lossy = !truncated && encodingInfo(encoding).canEncode && !roundTrips(bytes, d.text, encoding, d.bom)
  return done({ kind: 'ready', doc: { kind: 'text', text: d.text, meta: { encoding, bom: d.bom, eol: e.eol, mixedEol: e.mixed }, note, truncated, lossy }, stat })
}

/**
 * The server's current version of a text document, for the compare view: decoded like the open document (`encoding`),
 * BOM dropped; `raw` keeps its line endings, `text` has them normalised to "\n".
 */
export function decodeServerVersion(res: FileReadResult, encoding: string): { text: string; raw: string } {
  let raw: string
  if (res.encoding === 'base64') raw = decodeBytes(base64ToBytes(res.content ?? ''), encoding).text
  else {
    raw = res.content ?? ''
    if (raw.charCodeAt(0) === 0xfeff) raw = raw.slice(1)
    if (encoding !== 'utf-8') raw = decodeBytes(new TextEncoder().encode(raw), encoding).text
  }
  return { text: raw.replace(/\r\n?/g, '\n'), raw }
}
