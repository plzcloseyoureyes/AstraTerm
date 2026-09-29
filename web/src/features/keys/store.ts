/*
 * Dialog state of the keys feature, rendered by the overlay registered in index.ts, so commands, menus and other
 * features can open the generator, import / export / install / certificate / sign / passphrase dialogs and the
 * identity and known-host editors.
 */
import { create } from 'zustand'
import type { IdentityInput } from '@/api/types'
import type { ExportFormat, MarkerKind } from './types'

export interface KeysDialogs {
  generate: { key: number } | null
  importer: { key: number; mode: 'import' | 'convert'; text?: string } | null
  exporter: { key: number; keyId: string; format?: ExportFormat } | null
  install: { key: number; keyId?: string; connectionId?: string } | null
  certificate: { key: number; keyId: string } | null
  sign: { key: number; caKeyId?: string; subjectKeyId?: string } | null
  passphrase: { key: number; keyId: string } | null
  identity: { key: number; id?: string; initial?: Partial<IdentityInput> } | null
  knownHost: { key: number; kind: 'host' | 'marker'; marker?: MarkerKind; keyId?: string } | null
  knownHostsImport: { key: number } | null
}

let seq = 0

const empty: KeysDialogs = {
  generate: null,
  importer: null,
  exporter: null,
  install: null,
  certificate: null,
  sign: null,
  passphrase: null,
  identity: null,
  knownHost: null,
  knownHostsImport: null,
}

export const useKeysDialogs = create<KeysDialogs>(() => ({ ...empty }))

type Req<K extends keyof KeysDialogs> = Omit<NonNullable<KeysDialogs[K]>, 'key'>

/** Open one of the dialogs (replacing an open one of the same kind). */
export function openKeysDialog<K extends keyof KeysDialogs>(kind: K, req: Req<K>): void {
  useKeysDialogs.setState({ [kind]: { ...req, key: ++seq } } as Partial<KeysDialogs>)
}

export function closeKeysDialog(kind: keyof KeysDialogs): void {
  useKeysDialogs.setState({ [kind]: null } as Partial<KeysDialogs>)
}

export function closeAllKeysDialogs(): void {
  useKeysDialogs.setState({ ...empty })
}
