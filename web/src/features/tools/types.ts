/*
 * Tool-specific frontend types (backend contract lives in internal/tools). Streamed job results all arrive as
 * `{rows: ToolRow[]}` job-data events; each row carries a `kind` the panels dispatch on.
 */

/** Tab parameters for the 'tools' tab kind. */
export interface ToolsTabParams {
  /** Selected tool id (see TOOL_IDS). */
  tool?: string
}

/** One streamed result row from a network tool job (heterogeneous; `kind` discriminates). */
export interface ToolRow {
  kind: string
  [key: string]: unknown
}

// ---- sync endpoints --------------------------------------------------------------------------------------------------

export interface InterfaceInfo {
  name: string
  mtu: number
  mac: string
  flags: string[]
  addrs: string[]
  up: boolean
}

export interface SocketInfo {
  proto: string
  localAddr: string
  localPort: number
  remoteAddr?: string
  remotePort?: number
  state: string
  pid: number
  process?: string
  user?: string
  service?: string
}

// ---- run history -----------------------------------------------------------------------------------------------------

export interface ToolRun {
  id: string
  tool: string
  params: Record<string, unknown>
  label: string
  at: number
}
