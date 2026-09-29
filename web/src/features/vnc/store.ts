/*
 * VNC feature store: live controllers by tab (commands act on the focused VNC tab), per-tab UI toggles and the
 * open-state of the feature's dialogs (rendered by the overlay registered in index.ts).
 */
import { create } from 'zustand'
import { activeTab, useWorkspaceStore } from '@/stores/workspace'
import type { VncController } from './controller'

interface TabUI {
  clipboardOpen: boolean
  toolbar: boolean
  fullscreen: boolean
  keyboardLocked: boolean
}

interface VncStore {
  controllers: Record<string, VncController>
  ui: Record<string, TabUI>
  listenersOpen: boolean
  repeaterOpen: boolean
  certsOpen: boolean
}

export const useVncStore = create<VncStore>(() => ({
  controllers: {},
  ui: {},
  listenersOpen: false,
  repeaterOpen: false,
  certsOpen: false,
}))

const DEFAULT_UI: TabUI = { clipboardOpen: false, toolbar: true, fullscreen: false, keyboardLocked: false }

export function registerController(tabId: string, c: VncController): () => void {
  useVncStore.setState((s) => ({ controllers: { ...s.controllers, [tabId]: c } }))
  return () =>
    useVncStore.setState((s) => {
      if (s.controllers[tabId] !== c) return {}
      const next = { ...s.controllers }
      delete next[tabId]
      return { controllers: next }
    })
}

export function getController(tabId: string | undefined): VncController | undefined {
  return tabId ? useVncStore.getState().controllers[tabId] : undefined
}

/** The controller of the active tab when it is a VNC tab. */
export function activeController(): VncController | undefined {
  const t = activeTab()
  return t?.kind === 'vnc' ? getController(t.id) : undefined
}

export function useActiveController(): VncController | undefined {
  const tabId = useWorkspaceStore((s) => s.activeTabId)
  const kind = useWorkspaceStore((s) => s.tabs.find((t) => t.id === s.activeTabId)?.kind)
  const controllers = useVncStore((s) => s.controllers)
  return kind === 'vnc' && tabId ? controllers[tabId] : undefined
}

export function useTabUI(tabId: string): TabUI {
  return useVncStore((s) => s.ui[tabId] ?? DEFAULT_UI)
}

export function setTabUI(tabId: string, patch: Partial<TabUI>): void {
  useVncStore.setState((s) => ({ ui: { ...s.ui, [tabId]: { ...(s.ui[tabId] ?? DEFAULT_UI), ...patch } } }))
}

export function dropTabUI(tabId: string): void {
  useVncStore.setState((s) => {
    if (!s.ui[tabId]) return {}
    const next = { ...s.ui }
    delete next[tabId]
    return { ui: next }
  })
}

export function openListeners(open = true): void {
  useVncStore.setState({ listenersOpen: open })
}

export function openRepeater(open = true): void {
  useVncStore.setState({ repeaterOpen: open })
}

export function openCerts(open = true): void {
  useVncStore.setState({ certsOpen: open })
}
