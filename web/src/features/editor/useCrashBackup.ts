/*
 * Crash backups of an "editor" tab (IndexedDB, docstore.ts): unsaved changes are stored while typing, offered for
 * restore when the same file is opened again, and dropped once saved or discarded. Keys: tabstate.ts → backupKeys.
 */
import { useCallback, useState } from 'react'
import { useLatest } from '@/lib/hooks'
import { infoLabel, type FsHandleInfo } from './api'
import { backupId, deleteDoc, getDoc, putDoc } from './docstore'
import type { TextMeta } from './save'
import { backupFromDoc, backupKeys, type BackupRecord } from './tabstate'
import type { EditorTabParams } from './types'
import type { FsHandle } from './useFsHandle'

export interface CrashBackup {
  /** A recovered backup the user has not restored or discarded yet. */
  recovered: BackupRecord | null
  /** Forget the recovered backup (restored, or the document was reloaded). */
  dismiss: () => void
  /** Look for a backup of the file just loaded (`loadedText` = its content: an identical backup is dropped). */
  check: (loadedText: string) => Promise<void>
  write: (content: string, meta: TextMeta, extra: { baseMtime?: string; label?: string }) => void
  /** Delete every backup of this file. */
  drop: () => void
}

export function useCrashBackup(path: string, params: EditorTabParams, fs: FsHandle): CrashBackup {
  const [recovered, setRecovered] = useState<BackupRecord | null>(null)
  const latest = useLatest({ params, fs })

  const keys = useCallback(
    (info: FsHandleInfo | null): string[] => {
      const { params: p, fs: h } = latest.current
      return backupKeys(path, backupId(h.id(), path), {
        connectionId: p.source?.connectionId ?? info?.connectionId,
        label: p.label ?? (info ? infoLabel(info) : undefined),
      })
    },
    [latest, path],
  )

  const drop = useCallback(() => {
    for (const k of keys(latest.current.fs.info)) void deleteDoc(k)
  }, [keys, latest])

  const check = useCallback(
    async (loadedText: string) => {
      try {
        const info = await latest.current.fs.infoReady()
        let rec = null
        for (const key of keys(info)) {
          rec = await getDoc(key)
          if (rec) break
        }
        if (rec && rec.content === loadedText) {
          void deleteDoc(rec.id)
          return
        }
        const b = backupFromDoc(rec)
        if (b) setRecovered(b)
      } catch {
        /* no backup */
      }
    },
    [keys, latest],
  )

  const write = useCallback(
    (content: string, meta: TextMeta, extra: { baseMtime?: string; label?: string }) => {
      void putDoc({ id: keys(latest.current.fs.info)[0], content, meta: { ...meta, baseMtime: extra.baseMtime, savedAt: Date.now(), path, label: extra.label } })
    },
    [keys, latest, path],
  )

  const dismiss = useCallback(() => setRecovered(null), [])

  return { recovered, dismiss, check, write, drop }
}
