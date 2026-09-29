/*
 * Settings section `termTransfer` (Settings → File transfer (terminal)).
 */
import { defineSettings } from '@/stores/settings'

export type DownloadTarget = 'ask' | 'folder' | 'downloads'
export type ConflictPolicy = 'ask' | 'overwrite' | 'rename' | 'skip'
export type SendMode = 'text' | 'binary'
export type LineEnding = 'cr' | 'lf' | 'crlf'

export interface TermTransferSettings {
  /** trzsz (`trz` / `tsz` on the server). */
  trzsz: boolean
  /** ZMODEM (`rz` / `sz` on the server). */
  zmodem: boolean
  /** Start ZMODEM downloads (`sz`) without asking when the destination is decided (below). */
  zmodemAutoReceive: boolean
  /** Where downloaded files go: ask each time, a chosen folder (remembered), or the browser's downloads. */
  downloadTarget: DownloadTarget
  /** Command typed for "upload with ZMODEM" (drops, menu). */
  rzCommand: string
  /** Drop on a terminal: what to do with items that already exist in the destination folder. */
  dropConflict: ConflictPolicy
  /** After a drop upload to an SSH session's folder, type the uploaded path(s) at the prompt. */
  dropTypePath: boolean
  /** Largest file "paste contents" accepts (bytes). */
  pasteMaxBytes: number
  /** Send file to session: last used mode and pacing. */
  sendMode: SendMode
  lineDelayMs: number
  charDelayMs: number
  waitPrompt: boolean
  /** Prompt pattern (regular expression; empty = default). */
  promptPattern: string
  promptTimeoutMs: number
  lineEnding: LineEnding
  binaryChunkBytes: number
  binaryDelayMs: number
}

export const DEFAULTS: TermTransferSettings = {
  trzsz: true,
  zmodem: true,
  zmodemAutoReceive: true,
  downloadTarget: 'ask',
  rzCommand: 'rz -E',
  dropConflict: 'ask',
  dropTypePath: false,
  pasteMaxBytes: 64 * 1024,
  sendMode: 'text',
  lineDelayMs: 0,
  charDelayMs: 0,
  waitPrompt: false,
  promptPattern: '',
  promptTimeoutMs: 15_000,
  lineEnding: 'cr',
  binaryChunkBytes: 1024,
  binaryDelayMs: 5,
}

export const LIMITS = {
  pasteMaxBytes: [1024, 1024 * 1024],
  lineDelayMs: [0, 60_000],
  charDelayMs: [0, 5_000],
  promptTimeoutMs: [1_000, 600_000],
  binaryChunkBytes: [1, 1024 * 1024],
  binaryDelayMs: [0, 10_000],
} as const

export const transferSettings = defineSettings<TermTransferSettings>('termTransfer', DEFAULTS)

function clampNum(v: unknown, [min, max]: readonly [number, number], fallback: number): number {
  const n = typeof v === 'number' ? v : Number(v)
  return Number.isFinite(n) ? Math.min(max, Math.max(min, Math.round(n))) : fallback
}

/** Current settings with every value validated (stored values may be hand-edited or from older versions). */
export function currentSettings(): TermTransferSettings {
  const s = transferSettings.get()
  return {
    trzsz: s.trzsz !== false,
    zmodem: s.zmodem !== false,
    zmodemAutoReceive: s.zmodemAutoReceive !== false,
    downloadTarget: s.downloadTarget === 'folder' || s.downloadTarget === 'downloads' ? s.downloadTarget : 'ask',
    rzCommand: typeof s.rzCommand === 'string' && s.rzCommand.trim() ? s.rzCommand.trim().slice(0, 200) : DEFAULTS.rzCommand,
    dropConflict: (['ask', 'overwrite', 'rename', 'skip'] as const).includes(s.dropConflict) ? s.dropConflict : 'ask',
    dropTypePath: s.dropTypePath === true,
    pasteMaxBytes: clampNum(s.pasteMaxBytes, LIMITS.pasteMaxBytes, DEFAULTS.pasteMaxBytes),
    sendMode: s.sendMode === 'binary' ? 'binary' : 'text',
    lineDelayMs: clampNum(s.lineDelayMs, LIMITS.lineDelayMs, DEFAULTS.lineDelayMs),
    charDelayMs: clampNum(s.charDelayMs, LIMITS.charDelayMs, DEFAULTS.charDelayMs),
    waitPrompt: s.waitPrompt === true,
    promptPattern: typeof s.promptPattern === 'string' ? s.promptPattern.slice(0, 2000) : '',
    promptTimeoutMs: clampNum(s.promptTimeoutMs, LIMITS.promptTimeoutMs, DEFAULTS.promptTimeoutMs),
    lineEnding: s.lineEnding === 'lf' || s.lineEnding === 'crlf' ? s.lineEnding : 'cr',
    binaryChunkBytes: clampNum(s.binaryChunkBytes, LIMITS.binaryChunkBytes, DEFAULTS.binaryChunkBytes),
    binaryDelayMs: clampNum(s.binaryDelayMs, LIMITS.binaryDelayMs, DEFAULTS.binaryDelayMs),
  }
}

export const EOL: Record<LineEnding, string> = { cr: '\r', lf: '\n', crlf: '\r\n' }
