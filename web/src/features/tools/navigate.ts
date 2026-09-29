/*
 * Opening tools programmatically: `openTool('portscan', {targets: '10.0.0.5'})` focuses the singleton Tools tab on a
 * tool and hands its panel a one-shot form prefill (also used by `tools.open {tool, params}`).
 */
import { useEffect } from 'react'
import { create } from 'zustand'
import { focusTab, openTab } from '@/stores/workspace'
import type { ToolsTabParams } from './types'

interface PrefillStore {
  prefill: Record<string, Record<string, unknown> | undefined>
}

const usePrefillStore = create<PrefillStore>(() => ({ prefill: {} }))

export function openTool(tool?: string, prefill?: Record<string, unknown>): void {
  if (tool && prefill && Object.keys(prefill).length > 0) {
    usePrefillStore.setState((s) => ({ prefill: { ...s.prefill, [tool]: prefill } }))
  }
  openTab<ToolsTabParams>({ kind: 'tools', params: tool ? { tool } : {} })
  focusTab('tools')
}

/** Consume a pending prefill for tool (once) and apply it to the panel's form. */
export function usePrefill(tool: string, apply: (p: Record<string, unknown>) => void): void {
  const p = usePrefillStore((s) => s.prefill[tool])
  useEffect(() => {
    if (!p) return
    usePrefillStore.setState((s) => ({ prefill: { ...s.prefill, [tool]: undefined } }))
    apply(p)
    // apply is a fresh closure every render; the prefill object identity is the trigger.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [p, tool])
}

export const asString = (v: unknown, fallback = ''): string => (typeof v === 'string' ? v : typeof v === 'number' ? String(v) : fallback)
export const asNumber = (v: unknown, fallback: number): number => (typeof v === 'number' && Number.isFinite(v) ? v : fallback)
export const asBool = (v: unknown, fallback: boolean): boolean => (typeof v === 'boolean' ? v : fallback)
