/* Local files (this computer): sniff, store in the document store and open a "text" tab. Lazy-loaded by open.ts. */
import { toast } from 'sonner'
import { formatBytes, uid } from '@/lib/utils'
import { decodeBytes, sniffBytes } from './codec'
import { choose, type LocalFileHandle } from './dialogs'
import { putDoc } from './docstore'
import { MAX_LOCAL_BYTES, newScratch } from './open'
import { editorSettings } from './settings'

const MiB = 1024 * 1024

interface LocalDocMeta {
  name: string
  encoding?: string
  bom?: boolean
  lastModified?: number
  /** Unsaved changes relative to the file on disk (persisted across reloads). */
  dirty?: boolean
  /** A buffer that was never saved to a file ("Untitled"). */
  scratch?: boolean
}

export async function prepareLocalFile(file: File, handle?: LocalFileHandle): Promise<string | null> {
  if (file.size > MAX_LOCAL_BYTES) {
    toast.error(`${file.name} is too large for the editor`, { description: `${formatBytes(file.size)} (limit ${formatBytes(MAX_LOCAL_BYTES)}).` })
    return null
  }
  const limit = Math.max(1, editorSettings.get().largeFileMiB) * MiB
  if (file.size > limit) {
    const r = await choose({
      title: `Open ${file.name}?`,
      description: `This file is ${formatBytes(file.size)}. Large files can make the editor slow.`,
      tone: 'warning',
      choices: [
        { value: 'cancel', label: 'Cancel' },
        { value: 'open', label: 'Open anyway', variant: 'default' },
      ],
      cancel: 'cancel',
    })
    if (r !== 'open') return null
  }
  const bytes = new Uint8Array(await file.arrayBuffer())
  const sniff = sniffBytes(bytes)
  let mode: 'text' | 'hex' = 'text'
  if (sniff.kind === 'binary') {
    const r = await choose({
      title: `${file.name} looks like a binary file`,
      description: `${sniff.reason} Editing it as text can corrupt it.`,
      tone: 'warning',
      choices: [
        { value: 'cancel', label: 'Cancel' },
        { value: 'text', label: 'Open as text' },
        { value: 'hex', label: 'Open in hex editor', variant: 'default' },
      ],
      cancel: 'cancel',
    })
    if (r === 'cancel') return null
    mode = r as 'text' | 'hex'
  }
  const docId = uid('doc')
  const meta: LocalDocMeta = { name: file.name, lastModified: file.lastModified }
  if (mode === 'hex') {
    await putDoc({ id: docId, bytes, handle, meta: { ...meta } })
  } else {
    const encoding = sniff.kind === 'text' ? sniff.encoding : 'windows-1252'
    const { text, bom } = decodeBytes(bytes, encoding)
    await putDoc({ id: docId, content: text, handle, meta: { ...meta, encoding, bom } })
  }
  return newScratch({ title: file.name, docId, local: { name: file.name }, mode: mode === 'hex' ? 'hex' : undefined })
}
