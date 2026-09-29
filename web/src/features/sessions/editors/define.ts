/*
 * defineProtocol(): one call per editors/<protocol>.tsx. It registers the protocol editor with the shared registry
 * (registerProtocolEditor — other modules can override or extend) and records the protocol's *profile*: which basic
 * fields apply (host/port/user/credentials), which editor tabs are shown, and extra validation.
 */
import { registerProtocolEditor, type ProtocolEditorDef, type ProtocolEditorProps } from '@/app/registry'
import type { Connection, Protocol } from '@/api/types'
import type { ComponentType } from 'react'
import type { EditorProtocol } from '../types'

export interface ProtocolProfile {
  /** Host field in "Basic settings". */
  host: 'required' | 'optional' | 'hidden'
  hostLabel?: string
  hostPlaceholder?: string
  /** Port field (placeholder shows the protocol default). */
  port: boolean
  /** Port must be given (no meaningful default, e.g. raw TCP). */
  portRequired?: boolean
  username: boolean
  usernameLabel?: string
  usernamePlaceholder?: string
  /**
   * Credentials UI: `ssh` = method (auto/password/key/agent/keyboard-interactive/none) + password + stored key +
   * identity; `password` = a password field (+ identity); `none`.
   */
  auth: 'ssh' | 'password' | 'none'
  /** Secret written by the password field (default `password`; VNC uses `vncPassword`). */
  passwordSecret?: string
  passwordLabel?: string
  /** Offer reusable identities (username + key + password from the vault). */
  identity?: boolean
  /** Tabs: terminal → Terminal + Automation, graphical → Automation (reconnect only), files → neither. */
  kind: 'terminal' | 'graphical' | 'files'
  /** Network tab (proxy, timeouts, keepalive, port knocking). */
  network: boolean
  /** Hop configuration: SSH jump chain (SSH family) or a single SSH gateway (`sshTunnelVia`) for other protocols. */
  jump?: 'chain' | 'gateway'
}

/** Errors keyed by field path (e.g. `options.device`). */
export type ValidationErrors = Record<string, string>

export interface ProtocolSpec {
  protocol: EditorProtocol
  label: string
  icon: ProtocolEditorDef['icon']
  defaultPort: number
  group: ProtocolEditorDef['group']
  order: number
  description?: string
  component: ComponentType<ProtocolEditorProps>
  profile: ProtocolProfile
  /** Protocol-specific checks on the draft (basic host/port checks come from the profile). */
  validate?: (c: Connection) => ValidationErrors | undefined
  /** Title of the protocol tab (default "<Label> settings"). */
  tabLabel?: string
}

const specs = new Map<string, ProtocolSpec>()

export function defineProtocol(spec: ProtocolSpec): () => void {
  specs.set(spec.protocol, spec)
  const unregister = registerProtocolEditor({
    // api/types' Protocol predates the §10.1 extension protocols (winrm, ipmi); the registry keys by string.
    protocol: spec.protocol as Protocol,
    label: spec.label,
    icon: spec.icon,
    defaultPort: spec.defaultPort,
    group: spec.group,
    component: spec.component,
    description: spec.description,
    order: spec.order,
  })
  return () => {
    if (specs.get(spec.protocol) === spec) specs.delete(spec.protocol)
    unregister()
  }
}

export function getProtocolSpec(protocol: string | undefined | null): ProtocolSpec | undefined {
  return protocol ? specs.get(protocol) : undefined
}

/** Profile used for protocols registered by other modules without a spec here. */
export const DEFAULT_PROFILE: ProtocolProfile = {
  host: 'required',
  port: true,
  username: true,
  auth: 'password',
  identity: true,
  kind: 'terminal',
  network: true,
  jump: 'gateway',
}

export function getProtocolProfile(protocol: string | undefined | null): ProtocolProfile {
  return getProtocolSpec(protocol)?.profile ?? DEFAULT_PROFILE
}

/** SSH-based protocols (share SSH options and the jump chain). */
export function isSshFamily(protocol: string | undefined | null): boolean {
  return protocol === 'ssh' || protocol === 'sftp' || protocol === 'mosh'
}
