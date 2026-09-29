/*
 * Calm progress (docs/UX.md "Motion & feedback"): progress never moves backwards and never resets between files. The
 * timing of the indicators themselves (300 ms delay, 400 ms minimum) is lib/useDelayedFlag's.
 */

/**
 * How often live progress is published to the UI: a few updates a second are enough for the numbers, and the shared
 * ProgressBar eases its width between them.
 */
export const PROGRESS_PUBLISH_MS = 250

export interface ProgressSample {
  done: number
  total: number | null
  fileIndex?: number
  fileCount?: number | null
  fileBytes?: number
  fileSize?: number | null
}

/**
 * Overall completion in [0, 1], or null when it is truly unknown. Without a byte total, a known file count gives
 * ((files finished) + fraction of the current file) / count — per-file progress alone would restart at 0 on every file.
 */
export function overallFraction(p: ProgressSample): number | null {
  if (p.total != null && p.total > 0) return clamp01(p.done / p.total)
  if (p.total === 0) return 1
  const count = p.fileCount
  if (count != null && count > 0 && p.fileIndex != null && p.fileIndex > 0) {
    const inFile = p.fileSize != null && p.fileSize > 0 ? clamp01((p.fileBytes ?? 0) / p.fileSize) : p.fileSize === 0 ? 1 : 0
    return clamp01((Math.min(p.fileIndex, count) - 1 + inFile) / count)
  }
  return null
}

/** Monotonic progress: once known it never goes back (retransmissions, retries) and never becomes unknown again. */
export function advance(prev: number | null | undefined, next: number | null): number | null {
  if (next == null) return prev ?? null
  return prev == null ? next : Math.max(prev, next)
}

function clamp01(x: number): number {
  return Number.isFinite(x) ? Math.min(1, Math.max(0, x)) : 0
}
