/*
 * Transient UI state: dialogs, command palette, vault-unlock requests, lock screen, mobile sidebar drawer.
 */
import { create } from 'zustand'
import { storage } from '@/lib/utils'

export type PaletteMode = 'all' | 'commands' | 'connections' | 'tabs'

interface UIStore {
  paletteOpen: boolean
  paletteMode: PaletteMode
  paletteQuery: string
  vaultUnlockOpen: boolean
  aboutOpen: boolean
  /** Mobile (< 768px) sidebar drawer. */
  drawerOpen: boolean
  /** Lock screen shown (SEC-5). */
  locked: boolean
  /** Focus request counter for the ribbon quick-connect field. */
  quickConnectFocus: number
  /** The title bar's app menu (logo button). */
  appMenuOpen: boolean
}

const LOCK_KEY = 'astraterm:locked'

export const useUIStore = create<UIStore>(() => ({
  paletteOpen: false,
  paletteMode: 'all',
  paletteQuery: '',
  vaultUnlockOpen: false,
  aboutOpen: false,
  drawerOpen: false,
  // A reload must not bypass the lock screen.
  locked: storage.get<boolean>(LOCK_KEY, false) === true,
  quickConnectFocus: 0,
  appMenuOpen: false,
}))

// --- command palette ---------------------------------------------------------------------------------------------------

export function openPalette(mode: PaletteMode = 'all', query = ''): void {
  useUIStore.setState({ paletteOpen: true, paletteMode: mode, paletteQuery: query })
}

export function closePalette(): void {
  useUIStore.setState({ paletteOpen: false })
}

// --- vault unlock --------------------------------------------------------------------------------------------------------

let unlockWaiters: ((ok: boolean) => void)[] = []

/**
 * Open the vault unlock dialog; resolves true once unlocked, false if dismissed. Concurrent callers share the dialog
 * (used by the API client on HTTP 423 to transparently retry the request).
 */
export function requestVaultUnlock(): Promise<boolean> {
  return new Promise((resolve) => {
    unlockWaiters.push(resolve)
    useUIStore.setState({ vaultUnlockOpen: true })
  })
}

/** Called by the unlock dialog when it closes. */
export function finishVaultUnlock(unlocked: boolean): void {
  const waiters = unlockWaiters
  unlockWaiters = []
  useUIStore.setState({ vaultUnlockOpen: false })
  for (const w of waiters) w(unlocked)
}

// --- lock screen ---------------------------------------------------------------------------------------------------------

export function lockApp(): void {
  storage.set(LOCK_KEY, true)
  useUIStore.setState({ locked: true, paletteOpen: false, drawerOpen: false })
}

export function unlockApp(): void {
  storage.remove(LOCK_KEY)
  useUIStore.setState({ locked: false })
}

// --- misc ------------------------------------------------------------------------------------------------------------------

export function setAboutOpen(open: boolean): void {
  useUIStore.setState({ aboutOpen: open })
}

export function setDrawerOpen(open: boolean): void {
  useUIStore.setState({ drawerOpen: open })
}

export function focusQuickConnect(): void {
  useUIStore.setState((s) => ({ quickConnectFocus: s.quickConnectFocus + 1 }))
}

export function setAppMenuOpen(open: boolean): void {
  useUIStore.setState({ appMenuOpen: open })
}
