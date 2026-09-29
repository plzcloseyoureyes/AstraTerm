/*
 * Environment shared with protocol editors rendered inside the session editor dialog. The registry props are only
 * {value, onChange, mode}; validation errors, read-only state and the saved-connection list come from this context.
 */
import { createContext, useContext } from 'react'
import type { Connection } from '@/api/types'

export interface EditorEnv {
  mode: 'create' | 'edit'
  /** Shared connection of another user: everything is shown disabled. */
  readOnly: boolean
  /** Validation errors keyed by path (`host`, `options.device`, …). */
  errors: Readonly<Record<string, string | undefined>>
  /** Saved connections (for jump-host / gateway / "Docker via SSH" pickers). */
  connections: readonly Connection[]
  /** Id of the connection being edited (excluded from pickers). */
  selfId?: string
}

const DEFAULT_ENV: EditorEnv = { mode: 'create', readOnly: false, errors: {}, connections: [] }

export const EditorEnvContext = createContext<EditorEnv>(DEFAULT_ENV)

export function useEditorEnv(): EditorEnv {
  return useContext(EditorEnvContext)
}

/** Validation message for a field path (e.g. `options.baud`). */
export function useFieldError(path: string): string | undefined {
  return useContext(EditorEnvContext).errors[path]
}
