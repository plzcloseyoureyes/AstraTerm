/*
 * React access to a tool's job (see jobs.ts): the state lives in a store keyed by tool id, so a panel can unmount
 * (switching tools) and later show the same results — or a still-running job — again.
 */
import { useCallback, useMemo } from 'react'
import { cancelTool, resetTool, runTool, useToolJobs, type ToolJobState } from './jobs'
import type { ToolRow } from './types'

export type { ToolJobState }

const EMPTY: ToolJobState = { status: 'idle', rows: [], latest: {}, keyed: {} }

export function useToolJob(tool: string) {
  const state = useToolJobs((st) => st.jobs[tool] ?? EMPTY)
  const run = useCallback((params: Record<string, unknown>) => runTool(tool, params), [tool])
  const cancel = useCallback(() => cancelTool(tool), [tool])
  const reset = useCallback(() => resetTool(tool), [tool])
  return { state, run, cancel, reset, running: state.status === 'running' || state.status === 'starting' }
}

/** Rows of one kind (memoized on the rows array identity). */
export function useRowsOfKind(state: ToolJobState, kind: string): ToolRow[] {
  return useMemo(() => state.rows.filter((r) => r.kind === kind), [state.rows, kind])
}
