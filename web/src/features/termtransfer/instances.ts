/*
 * Light registry of the per-terminal transfer controllers (the controller module itself — with trzsz and zmodem.js —
 * loads lazily when the first terminal mounts). Commands, menus and the overlay look controllers up here.
 */
import { useAuthStore } from '@/stores/auth'
import { useWorkspaceStore } from '@/stores/workspace'
import type { TransferController } from './controller'

const controllers = new Map<string, TransferController>()

export function registerController(c: TransferController): () => void {
  controllers.set(c.tabId, c)
  return () => {
    if (controllers.get(c.tabId) === c) controllers.delete(c.tabId)
  }
}

export function getController(tabId: string | null | undefined): TransferController | undefined {
  return tabId ? controllers.get(tabId) : undefined
}

export function listControllers(): TransferController[] {
  return Array.from(controllers.values())
}

/** The controller of the active dock tab. */
export function activeController(): TransferController | undefined {
  return getController(useWorkspaceStore.getState().activeTabId)
}

/** Local files (the NexTerm host) are reachable in desktop mode and, in server mode, for admins (SPEC principle 7). */
export function localFilesAllowed(): boolean {
  const st = useAuthStore.getState()
  return st.state?.mode !== 'server' || st.user?.role === 'admin'
}
