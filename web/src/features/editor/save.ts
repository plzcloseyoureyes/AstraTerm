/* Encoding documents for writing (remote write body, local file bytes) and local File System Access writes. */
import { bytesToBase64, encodeText, EOL_SEQ, type Eol } from './codec'
import type { LocalFileHandle } from './dialogs'

export interface TextMeta {
  encoding: string
  bom: boolean
  /** Main line ending (new line breaks; every break when `mixedEol` is false). */
  eol: Eol
  /**
   * The file has mixed line endings and the user did not pick one: every existing line break is written back with its
   * own sequence (see cm/eol.ts). Choosing a line ending in the status bar clears it (= convert all).
   */
  mixedEol: boolean
}

export function sameMeta(a: TextMeta | null | undefined, b: TextMeta | null | undefined): boolean {
  if (!a || !b) return a === b
  return a.encoding === b.encoding && a.bom === b.bom && a.eol === b.eol && a.mixedEol === b.mixedEol
}

/** "\n"-joined text with another line ending (for texts that are not in an editor session). */
export function withEol(textWithLf: string, eol: Eol): string {
  return eol === 'lf' ? textWithLf : textWithLf.replace(/\n/g, EOL_SEQ[eol])
}

/**
 * Body for PUT /api/fs/{id}/write from the final text (line endings already applied): UTF-8 travels as text, every
 * other encoding as base64 bytes. Throws EncodeError for characters the encoding cannot represent.
 */
export function writeBody(text: string, meta: Pick<TextMeta, 'encoding' | 'bom'>): { content: string; encoding: 'utf-8' | 'base64' } {
  if (meta.encoding === 'utf-8') return { content: (meta.bom ? '﻿' : '') + text, encoding: 'utf-8' }
  return { content: bytesToBase64(encodeText(text, meta.encoding, meta.bom)), encoding: 'base64' }
}

/** Bytes of the final text (line endings already applied) as they will be written to disk. */
export function textBytes(text: string, meta: Pick<TextMeta, 'encoding' | 'bom'>): Uint8Array {
  return encodeText(text, meta.encoding, meta.bom)
}

/** Ask for write access to a local file handle (must run in a user gesture the first time). */
export async function ensureWritable(handle: LocalFileHandle): Promise<boolean> {
  if (!handle.createWritable) return false
  try {
    if (handle.queryPermission && (await handle.queryPermission({ mode: 'readwrite' })) === 'granted') return true
    if (handle.requestPermission) return (await handle.requestPermission({ mode: 'readwrite' })) === 'granted'
    return true
  } catch {
    return false
  }
}

export async function writeLocalHandle(handle: LocalFileHandle, data: Uint8Array): Promise<void> {
  if (!handle.createWritable) throw new Error('This browser cannot write files directly.')
  const w = await handle.createWritable()
  await w.write(data as unknown as BlobPart)
  await w.close()
}

type SavePickerWindow = Window & {
  showSaveFilePicker?: (opts?: { suggestedName?: string }) => Promise<LocalFileHandle>
}

export function canPickSaveFile(): boolean {
  return typeof (window as SavePickerWindow).showSaveFilePicker === 'function'
}

/** File System Access "Save as" picker (call from a user gesture). Null when cancelled or unsupported. */
export async function pickSaveFile(suggestedName: string): Promise<LocalFileHandle | null> {
  const w = window as SavePickerWindow
  if (!w.showSaveFilePicker) return null
  try {
    return await w.showSaveFilePicker({ suggestedName })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') return null
    throw err
  }
}
