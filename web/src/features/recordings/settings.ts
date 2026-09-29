/*
 * User preferences of the recordings feature (settings section `recordings`; only changed keys are stored).
 * The administrator's policy (retention, command audit, sharing, debug log) is global: GET /api/recordings/policy,
 * PUT /api/admin/recordings/policy.
 */
import { defineSettings } from '@/stores/settings'

export interface RecordingsSettings {
  /** Default playback speed of the player. */
  speed: number
  /** Idle time limit in seconds (0 = keep pauses as recorded). */
  idleTimeLimit: number
  /** Start playback as soon as a recording opens. */
  autoPlay: boolean
  /** Pause at markers (commands) while playing. */
  pauseOnMarkers: boolean
  /** Share dialog defaults. */
  shareMode: 'read' | 'write'
  shareExpiresInSec: number
  shareRequireLogin: boolean
  /** Interactive links start with guest input paused (the owner allows it from the Share dialog). */
  shareInputPaused: boolean
  /** Minutes offered first by "Replay last N minutes". */
  replayMinutes: number
  /** Recordings list: page size. */
  pageSize: number
}

export const recordingsSettings = defineSettings<RecordingsSettings>('recordings', {
  speed: 1,
  idleTimeLimit: 2,
  autoPlay: true,
  pauseOnMarkers: false,
  shareMode: 'read',
  shareExpiresInSec: 3600,
  shareRequireLogin: false,
  shareInputPaused: true,
  replayMinutes: 5,
  pageSize: 100,
})
