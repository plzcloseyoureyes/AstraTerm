/*
 * Per-tool form state that outlives the panel (switching tools keeps what was typed). Memory only — never persisted —
 * and cleared when a different user signs in (secrets such as SNMPv3 passphrases are part of some forms).
 */
import { useCallback, useMemo } from 'react'
import { create } from 'zustand'
import { useAuthStore } from '@/stores/auth'
import { cancelAllTools } from './jobs'

interface FormsStore {
  forms: Record<string, Record<string, unknown>>
}

const useForms = create<FormsStore>(() => ({ forms: {} }))

useAuthStore.subscribe((s, prev) => {
  if (s.user?.id !== prev.user?.id) {
    useForms.setState({ forms: {} })
    cancelAllTools()
  }
})

/** [form, patch] for a tool; defaults must be a stable (module-level) object. */
export function useToolForm<T extends object>(tool: string, defaults: T): [T, (patch: Partial<T>) => void] {
  const stored = useForms((s) => s.forms[tool])
  const form = useMemo(() => ({ ...defaults, ...(stored as Partial<T> | undefined) }) as T, [defaults, stored])
  const set = useCallback(
    (patch: Partial<T>) => useForms.setState((s) => ({ forms: { ...s.forms, [tool]: { ...s.forms[tool], ...patch } } })),
    [tool],
  )
  return [form, set]
}
