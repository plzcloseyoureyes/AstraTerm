/*
 * Bridge between commands/menus/dialogs and the mounted session tree. The sidebar panel registers its handlers while
 * mounted; requests made while it is not mounted are kept (reveal / focus search) and replayed on mount.
 */
import { create } from 'zustand'
import { runCommand } from '@/app/commands'
import type { NodeId } from '../types'

export interface TreeController {
  /** Select and scroll to a node, expanding its folders. */
  reveal(nodeId: NodeId): void
  /** Start inline rename (F2). Returns false when the node is not visible. */
  startRename(nodeId: NodeId): boolean
  focusSearch(text?: string): void
  focusTree(): void
  expandAll(): void
  collapseAll(): void
}

let controller: TreeController | null = null
let pendingReveal: NodeId | null = null
let pendingSearch: { text?: string } | null = null

/** Current tree focus (for commands without explicit arguments, e.g. "New session" in the focused folder). */
export const useTreeFocus = create<{ nodeId: NodeId | null }>(() => ({ nodeId: null }))

export function registerTreeController(c: TreeController): () => void {
  controller = c
  if (pendingReveal) {
    const id = pendingReveal
    pendingReveal = null
    // Let the tree render its data first.
    setTimeout(() => controller?.reveal(id), 50)
  }
  if (pendingSearch) {
    const s = pendingSearch
    pendingSearch = null
    setTimeout(() => controller?.focusSearch(s.text), 50)
  }
  return () => {
    if (controller === c) controller = null
  }
}

export function getTreeController(): TreeController | null {
  return controller
}

export function revealNode(nodeId: NodeId): void {
  if (controller) controller.reveal(nodeId)
  else pendingReveal = nodeId
}

/** Show the Sessions sidebar panel and focus its search box (optionally pre-filled). */
export function focusSessionSearch(text?: string): void {
  // Registered by the shell for every sidebar panel (expands the sidebar / opens the mobile drawer).
  void runCommand('sidebar.show.sessions', undefined, { source: 'api' })
  if (controller) controller.focusSearch(text)
  else pendingSearch = { text }
}

// ---------------------------------------------------------------------------------------------------------------------
// Context-menu focus coordination
// ---------------------------------------------------------------------------------------------------------------------

let menuOpen = false
let afterClose: (() => void) | null = null
let fallbackTimer: ReturnType<typeof setTimeout> | null = null

/** Called by the session context menu when it opens / starts closing. */
export function setSessionMenuOpen(open: boolean): void {
  menuOpen = open
  if (fallbackTimer) clearTimeout(fallbackTimer)
  fallbackTimer = null
  // Safety net: run a deferred action even if the menu never reports its focus restoration.
  if (!open && afterClose) fallbackTimer = setTimeout(() => runAfterSessionMenuClose(), 500)
}

/**
 * Run `fn` once the session context menu has closed and restored focus (immediately when no menu is open), so actions
 * that move focus themselves — inline rename — are not undone by the menu's focus restoration.
 */
export function afterSessionMenu(fn: () => void): void {
  if (menuOpen) afterClose = fn
  else fn()
}

/** The menu finished closing: run the deferred action. Returns whether one ran (the menu then skips focus restore). */
export function runAfterSessionMenuClose(): boolean {
  if (fallbackTimer) clearTimeout(fallbackTimer)
  fallbackTimer = null
  const fn = afterClose
  afterClose = null
  if (!fn) return false
  fn()
  return true
}
