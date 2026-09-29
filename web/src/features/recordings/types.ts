/*
 * Recording & sharing feature types (JSON contract of internal/recording; SPEC §9 "recording").
 */
import type { Protocol, Recording, RuntimeSession, SessionState } from '@/api/types'

/** A recordings row as listed by GET /api/recordings. `kind` may also be 'guac' for RDP recordings merged in. */
export interface RecordingItem extends Omit<Recording, 'kind'> {
  kind: 'asciicast' | 'log' | 'guac'
  live: boolean
  interrupted?: boolean
  durationMs?: number
  connectionName?: string
  ownerName?: string
  /** Frontend only: the row comes from the RDP module (GET /api/rdp/recordings). */
  source?: 'terminal' | 'rdp'
}

export interface RecordingList {
  items: RecordingItem[]
  total: number
  limit: number
  offset: number
}

export interface RecordingFilters {
  q?: string
  kind?: 'asciicast' | 'log' | 'guac' | ''
  connectionId?: string
  from?: string
  to?: string
  all?: boolean
  sort?: 'started' | 'size' | 'title' | 'duration'
  order?: 'asc' | 'desc'
}

export interface RecordingPolicy {
  maxAgeDays: number
  maxTotalMB: number
  commandAudit: boolean
  shareEnabled: boolean
  shareWriteEnabled: boolean
  shareMaxHours: number
  debugLog: boolean
  debugLogLevel: 'debug' | 'info' | 'warn' | 'error'
  mode: 'desktop' | 'server'
}

export interface RecordingUsage {
  count: number
  bytes: number
  byKind: Record<string, { count: number; bytes: number }>
  oldest?: string
  scope: 'own' | 'all'
  maxAgeDays: number
  maxTotalMB: number
}

export interface RetentionResult {
  dryRun: boolean
  deleted: number
  bytes: number
  byAge: number
  bySize: number
  totalBytes: number
  failed: number
}

export interface SearchMatch {
  line?: number
  time?: number
  ts?: string
  text: string
}

export interface SearchResponse {
  matches: SearchMatch[]
  total: number
  truncated: boolean
}

export interface SearchAllResponse {
  results: { recording: RecordingItem; count: number; samples: SearchMatch[] }[]
  scanned: number
  truncated: boolean
}

export interface CommandRecord {
  time: string
  command: string
  exitCode?: number
  durationMs?: number
  cwd?: string
  source: 'shell-integration' | 'input'
  running?: boolean
  /** Typed by a viewer of an interactive share link. */
  guest?: GuestRef
}

/** The share-link viewer who typed a command (command audit attribution). */
export interface GuestRef {
  shareId: string
  viewerId: string
  username?: string
  ip?: string
  label?: string
}

export interface ViewerInfo {
  id: string
  ip: string
  username?: string
  since: string
  mode: 'read' | 'write'
}

export interface ShareView {
  id: string
  sessionId: string
  sessionTitle?: string
  ownerId: string
  mode: 'read' | 'write'
  createdAt: string
  expiresAt: string
  requireLogin: boolean
  maxViewers: number
  label?: string
  uses: number
  lastUsedAt?: string
  url?: string
  token?: string
  viewers: ViewerInfo[]
  /** Interactive links: the owner paused guest input (guests watch read-only meanwhile). */
  inputPaused: boolean
}

export interface CreateShareRequest {
  mode: 'read' | 'write'
  expiresInSec: number
  requireLogin?: boolean
  maxViewers?: number
  label?: string
  /** Interactive links: start with guest input paused (guests can type once the owner allows it). */
  inputPaused?: boolean
}

/** Public description of a share link (GET /api/share/{token}). */
export interface PublicShareInfo {
  title: string
  state: SessionState
  cols: number
  rows: number
  mode: 'read' | 'write'
  expiresAt: string
  requireLogin: boolean
  label?: string
}

export interface AdminSession extends RuntimeSession {
  owner: { id: string; username: string; displayName?: string; role?: string }
  bytesOut: number
  bytesIn: number
  durationMs: number
  lastOutputAt?: string
  shares: number
  shareViewers: number
}

export interface LogEntry {
  id: number
  ts: string
  level: 'debug' | 'info' | 'warn' | 'error'
  msg: string
  module?: string
  attrs?: { k: string; v: string }[]
}

export interface LogsResponse {
  entries: LogEntry[]
  lastId: number
  stored: number
  capacity: number
  dropped: number
  enabled: boolean
  level: string
  source: 'root' | 'partial'
}

/** Params of the 'player' tab. */
export interface PlayerTabParams {
  /** A recording (cast or log). */
  recordingId?: string
  /** Instant replay of a live session (GET /api/sessions/{id}/replay). */
  sessionId?: string
  /** Replay: the last N minutes (0 = everything the scrollback holds). */
  minutes?: number
  /** Start position in seconds; negative = from the end ("last N minutes" of a recording). */
  startAt?: number
  /** Log viewer: line to reveal. */
  line?: number
  title?: string
  protocol?: Protocol
}

/** Params of the 'shadow' tab (admin read-only view of another user's session). */
export interface ShadowTabParams {
  sessionId: string
  title?: string
  owner?: string
  protocol?: Protocol
}

/** RDP recordings as served by the rdp module (GET /api/rdp/recordings). */
export interface RdpRecording {
  id: string
  ownerId: string
  sessionId: string
  connectionId?: string
  title: string
  kind: 'guac'
  size: number
  width: number
  height: number
  startedAt: string
  endedAt?: string
}
