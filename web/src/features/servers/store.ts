/*
 * Dialog / drawer state of the servers feature (rendered by the overlay registered in index.ts), so commands, the
 * ribbon menu and the cards can open the configuration dialog and the activity drawer.
 */
import { create } from 'zustand'
import type { ServerKindEx } from './types'

export type DrawerTab = 'log' | 'clients' | 'connect'

interface ServersUIState {
  config: { kind: ServerKindEx; key: number; tab?: string } | null
  drawer: { kind: ServerKindEx; tab: DrawerTab; key: number } | null
}

let seq = 0

export const useServersUI = create<ServersUIState>(() => ({ config: null, drawer: null }))

export function openServerConfig(kind: ServerKindEx, tab?: string): void {
  useServersUI.setState({ config: { kind, tab, key: ++seq } })
}

export function closeServerConfig(): void {
  useServersUI.setState({ config: null })
}

export function openServerDrawer(kind: ServerKindEx, tab: DrawerTab = 'log'): void {
  useServersUI.setState({ drawer: { kind, tab, key: ++seq } })
}

export function closeServerDrawer(): void {
  useServersUI.setState({ drawer: null })
}
