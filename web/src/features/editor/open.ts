/*
 * Opening editor tabs (light module, part of the main bundle — the editor itself is lazy):
 *
 *   openRemoteFile({fsId, path, readOnly?, label?, source?, mode?, line?})  focuses an existing tab for the same file
 *   newScratch({title?, content?, language?})                                 "Untitled-N" text tab
 *   openDiff(left?, right?)                                                   TOOL-3 diff tab
 *   openLocalFile()                                                           file from this computer (FS Access API)
 */
import { toast } from 'sonner'
import type { TabInfo, TabPosition } from '@/app/registry'
import { errorMessage, uid } from '@/lib/utils'
import { focusTab, listTabs, openTab, updateTabParams } from '@/stores/workspace'
import { rememberHandle } from './api'
import { getController } from './controllers'
import type { LocalFileHandle } from './dialogs'
import { putDoc } from './docstore'
import { addRecent } from './recent'
import type { DiffSource, DiffSourceInput, DiffTabParams, EditorTabParams, TextTabParams } from './types'

// ---------------------------------------------------------------------------------------------------------------------
// remote files
// ---------------------------------------------------------------------------------------------------------------------

export function findEditorTab(fsId: string, path: string): TabInfo<EditorTabParams> | undefined {
  return listTabs().find((t) => t.kind === 'editor' && t.params?.fsId === fsId && t.params?.path === path) as TabInfo<EditorTabParams> | undefined
}

export interface OpenRemoteOptions extends EditorTabParams {
  position?: TabPosition
  activate?: boolean
}

/** Open (or focus) the editor for a remote file. Returns the tab id. */
export function openRemoteFile(opts: OpenRemoteOptions): string {
  const { position, activate, ...params } = opts
  if (!params.fsId || !params.path) throw new Error('editor.open needs {fsId, path}')
  if (params.label) rememberHandle({ id: params.fsId, label: params.label })
  addRecent({ fsId: params.fsId, path: params.path, label: params.label, source: params.source })
  const existing = findEditorTab(params.fsId, params.path)
  if (existing) {
    if (activate !== false) focusTab(existing.id)
    const patch: Partial<EditorTabParams> = {}
    if (params.mode && params.mode !== existing.params.mode) patch.mode = params.mode
    if (params.label && !existing.params.label) patch.label = params.label
    if (params.source && !existing.params.source) patch.source = params.source
    if (Object.keys(patch).length) updateTabParams<EditorTabParams>(existing.id, patch)
    if (params.line) getController(existing.id)?.revealLine?.(params.line)
    if (params.find) getController(existing.id)?.revealText?.(params.find)
    return existing.id
  }
  return openTab<EditorTabParams>({ kind: 'editor', params, position, activate })
}

// ---------------------------------------------------------------------------------------------------------------------
// scratch documents
// ---------------------------------------------------------------------------------------------------------------------

function nextUntitled(): string {
  const used = new Set(
    listTabs()
      .filter((t) => t.kind === 'text')
      .map((t) => /Untitled-(\d+)$/.exec(t.title.replace(/^● /, ''))?.[1])
      .filter(Boolean)
      .map(Number),
  )
  let n = 1
  while (used.has(n)) n++
  return `Untitled-${n}`
}

export interface NewTextOptions {
  title?: string
  content?: string
  language?: string
  position?: TabPosition
  local?: TextTabParams['local']
  mode?: TextTabParams['mode']
  /** Pre-created document id (content already stored). */
  docId?: string
}

/** New "text" tab. The content goes to the document store, not into the (persisted) tab params. */
export function newScratch(opts: NewTextOptions = {}): string {
  const docId = opts.docId ?? uid('doc')
  if (!opts.docId) void putDoc({ id: docId, content: opts.content ?? '' })
  const params: TextTabParams = { title: opts.title?.trim() || nextUntitled(), docId }
  if (opts.language !== undefined) params.language = opts.language
  if (opts.local) params.local = opts.local
  if (opts.mode) params.mode = opts.mode
  return openTab<TextTabParams>({ kind: 'text', params, position: opts.position })
}

// ---------------------------------------------------------------------------------------------------------------------
// diff
// ---------------------------------------------------------------------------------------------------------------------

/** Normalise `editor.diff` side arguments (content is moved into the document store). */
export function toDiffSource(input: DiffSourceInput | undefined | null): DiffSource | undefined {
  if (!input || typeof input !== 'object') return undefined
  if ('kind' in input && (input.kind === 'remote' || input.kind === 'text')) return input
  if ('fsId' in input && typeof input.fsId === 'string' && typeof input.path === 'string') {
    if (input.label) rememberHandle({ id: input.fsId, label: input.label })
    const out: DiffSource = { kind: 'remote', fsId: input.fsId, path: input.path, label: input.label }
    if (input.source && typeof input.source === 'object') out.source = input.source
    return out
  }
  const content = 'content' in input ? input.content : 'text' in input ? input.text : undefined
  if (typeof content === 'string') {
    const docId = uid('doc')
    void putDoc({ id: docId, content })
    return { kind: 'text', title: ('title' in input && input.title) || 'Text', docId }
  }
  return undefined
}

export function openDiff(left?: DiffSourceInput | null, right?: DiffSourceInput | null, opts: { position?: TabPosition } = {}): string {
  const params: DiffTabParams = { left: toDiffSource(left), right: toDiffSource(right) }
  return openTab<DiffTabParams>({ kind: 'diff', params, position: opts.position })
}

// ---------------------------------------------------------------------------------------------------------------------
// local files (this computer)
// ---------------------------------------------------------------------------------------------------------------------

type OpenPickerWindow = Window & {
  showOpenFilePicker?: (opts?: { multiple?: boolean }) => Promise<LocalFileHandle[]>
}

export const MAX_LOCAL_BYTES = 64 * 1024 * 1024

/**
 * Ask for a local file. MUST be called synchronously from a user gesture (it opens the picker before any await).
 * Resolves null when the user cancels.
 */
export function pickLocalFile(): Promise<{ file: File; handle?: LocalFileHandle } | null> {
  const w = window as OpenPickerWindow
  if (typeof w.showOpenFilePicker === 'function') {
    return w
      .showOpenFilePicker({ multiple: false })
      .then(async ([handle]) => (handle ? { file: await handle.getFile(), handle } : null))
      .catch((err: unknown) => {
        if (err instanceof DOMException && err.name === 'AbortError') return null
        throw err
      })
  }
  return new Promise((resolve) => {
    const input = document.createElement('input')
    input.type = 'file'
    input.style.display = 'none'
    let settled = false
    const done = (v: { file: File } | null) => {
      if (settled) return
      settled = true
      input.remove()
      resolve(v)
    }
    input.addEventListener('change', () => done(input.files?.[0] ? { file: input.files[0] } : null))
    input.addEventListener('cancel', () => done(null))
    document.body.appendChild(input)
    input.click()
  })
}

/** Editor → "Open local file…": pick, sniff (text / binary), store and open a "text" tab. */
export async function openLocalFile(): Promise<string | null> {
  const picked = await pickLocalFile()
  if (!picked) return null
  try {
    const { prepareLocalFile } = await import('./local')
    return await prepareLocalFile(picked.file, picked.handle)
  } catch (err) {
    toast.error('Could not open the file', { description: errorMessage(err) })
    return null
  }
}
