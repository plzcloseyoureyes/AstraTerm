/*
 * Frontend types for the import/export wizard (mirrors internal/importer JSON contract). Shared model types come from
 * @/api/types; these are importer-specific.
 */
import type { ConnectionOptions, Protocol } from '@/api/types'

export type ImportFormat =
  | 'auto'
  | 'mobaxterm'
  | 'putty_reg'
  | 'ssh_config'
  | 'termius_csv'
  | 'mremoteng'
  | 'remmina'
  | 'filezilla'
  | 'winscp'
  | 'securecrt'
  | 'csv'
  | 'json'
  | 'known_hosts'

export type Dedupe = 'skip' | 'update' | 'duplicate'

export interface PreviewOptions {
  passphrase?: string
  csvMapping?: Record<string, string>
  csvDelim?: string
  /** Legacy 8-bit text encoding (WHATWG label, e.g. "windows-1255"); omitted = auto (UTF-16 BOM / UTF-8 / Windows-1252). */
  charset?: string
}

export interface PreviewFolder {
  id: string
  parentId: string
  name: string
  icon?: string
  color?: string
}

export interface PreviewConnection {
  id: string
  folderId: string
  name: string
  protocol: Protocol
  host: string
  port: number
  username?: string
  authMethod: string
  options: ConnectionOptions
  secretKeys: string[]
  icon?: string
  color?: string
  tags?: string[]
  notes?: string
  duplicate: boolean
  duplicateOf?: string
  keyName?: string
  /** SSH jump chain / gateway, first hop first (display strings). */
  via?: string[]
  /** Opening it runs a program on the NexTerm host (ProxyCommand, local shell) — highlighted in the preview. */
  runsLocalCommand?: boolean
  warnings?: string[]
}

export interface PreviewKey {
  id: string
  name: string
  type?: string
  bits?: number
  fingerprint?: string
  encrypted: boolean
  duplicate: boolean
}

export interface PreviewKnownHost {
  host: string
  port: number
  keyType: string
  fingerprint: string
  /** This exact key is already trusted. */
  duplicate: boolean
  /** A different key of this type is already trusted for the host (it is not replaced). */
  conflict: boolean
}

export interface DuplicateRef {
  id: string
  name: string
  existingId: string
  existingName: string
}

export interface PreviewCounts {
  folders: number
  connections: number
  keys: number
  knownHosts: number
  duplicates: number
  unsupported: number
  identities: number
  snippets: number
}

export interface PreviewResponse {
  format: string
  folders: PreviewFolder[]
  connections: PreviewConnection[]
  keys?: PreviewKey[]
  knownHosts?: PreviewKnownHost[]
  warnings: string[]
  duplicates: DuplicateRef[]
  counts: PreviewCounts
}

export interface CommitResponse {
  created: number
  updated: number
  skipped: number
  foldersCreated: number
  keysImported: number
  knownHostsAdded: number
  knownHostsConflicts?: number
  /** Saved SSH connections created for gateways / keyed jump hosts. */
  gatewaysCreated?: number
  identitiesCreated?: number
  snippetsCreated?: number
  warnings?: string[]
  /** Created or updated connections, in import order. */
  connectionIds: string[]
  folderIds: string[]
}

export interface DiscoverEntry {
  path: string
  format: ImportFormat
  label: string
  size: number
}

export interface SyncStatus {
  supported: boolean
  enabled: boolean
  path: string
  exists: boolean
  folderId?: string
  lastSync?: string
  lastError?: string
  synced: number
}

export type ExportFormat = 'json' | 'csv' | 'ssh_config'

/** Text encodings offered for legacy (non-UTF-8) files. */
export const CHARSET_OPTIONS: { value: string; label: string }[] = [
  { value: 'auto', label: 'Automatic (UTF-8 / Western)' },
  { value: 'windows-1250', label: 'Central European (Windows-1250)' },
  { value: 'windows-1251', label: 'Cyrillic (Windows-1251)' },
  { value: 'windows-1253', label: 'Greek (Windows-1253)' },
  { value: 'windows-1254', label: 'Turkish (Windows-1254)' },
  { value: 'windows-1255', label: 'Hebrew (Windows-1255)' },
  { value: 'windows-1256', label: 'Arabic (Windows-1256)' },
  { value: 'windows-1257', label: 'Baltic (Windows-1257)' },
  { value: 'windows-874', label: 'Thai (Windows-874)' },
  { value: 'gbk', label: 'Chinese Simplified (GBK)' },
  { value: 'big5', label: 'Chinese Traditional (Big5)' },
  { value: 'shift_jis', label: 'Japanese (Shift_JIS)' },
  { value: 'euc-kr', label: 'Korean (EUC-KR)' },
]
