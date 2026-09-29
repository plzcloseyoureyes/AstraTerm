/*
 * Sessions panel UI state kept outside React so it survives the sidebar being collapsed or switched.
 */
import { create } from 'zustand'
import type { Connection, RuntimeSession } from '@/api/types'
import { EMPTY_FILTERS, type SessionFilters } from '../types'

interface PanelState {
  query: string
  filters: SessionFilters
}

export const usePanelState = create<PanelState>(() => ({ query: '', filters: EMPTY_FILTERS }))

export function setPanelQuery(query: string): void {
  usePanelState.setState({ query })
}

export function setPanelFilters(patch: Partial<SessionFilters>): void {
  usePanelState.setState((s) => ({ filters: { ...s.filters, ...patch } }))
}

export function clearPanelFilters(): void {
  usePanelState.setState({ filters: EMPTY_FILTERS })
}

export function filtersActive(f: SessionFilters): boolean {
  return f.protocols.length > 0 || f.tags.length > 0 || f.favorites || f.shared || f.running
}

/** Predicate for the filter chips (protocol / tag / favorites / shared / running). */
export function filterPredicate(f: SessionFilters, running: Map<string, RuntimeSession[]>): (c: Connection) => boolean {
  const protocols = new Set(f.protocols)
  const tags = f.tags.map((t) => t.toLowerCase())
  return (c) => {
    if (protocols.size && !protocols.has(c.protocol)) return false
    if (tags.length) {
      const own = new Set((c.tags ?? []).map((t) => t.toLowerCase()))
      if (!tags.every((t) => own.has(t))) return false
    }
    if (f.favorites && !c.favorite) return false
    if (f.shared && !c.shared) return false
    if (f.running && !running.has(c.id)) return false
    return true
  }
}
