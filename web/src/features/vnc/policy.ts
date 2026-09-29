/*
 * Per-connection VNC security policies (SPEC §9 "vnc"): options.encryption and options.clipboardDirection. The
 * session editor (sessions feature) does not show these keys yet, so the viewer offers them where they matter — the
 * security popover and the "less secure connection" confirmation — for saved connections the user may edit.
 */
import { getConnection, updateConnection } from '@/api/connections'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, User } from '@/api/types'
import type { ClipboardDirection, EncryptionPolicy } from './types'

export const ENCRYPTION_POLICIES: { value: EncryptionPolicy; label: string; description: string }[] = [
  { value: 'require', label: 'Require encryption', description: 'Only connect with TLS (VeNCrypt); never without encryption.' },
  {
    value: 'prefer',
    label: 'Prefer encryption (ask)',
    description: 'Use TLS when the server offers it; ask before a weaker or unencrypted connection.',
  },
  {
    value: 'allow-weak',
    label: 'Allow weak key exchange',
    description: 'Also accept anonymous TLS with a 1024-bit key exchange without asking.',
  },
  {
    value: 'allow-unencrypted',
    label: 'Allow unencrypted fallback',
    description: 'Fall back to an unencrypted connection without asking when TLS fails.',
  },
]

export const CLIPBOARD_DIRECTIONS: { value: ClipboardDirection; label: string }[] = [
  { value: 'both', label: 'Both directions' },
  { value: 'to-remote', label: 'Local → remote only' },
  { value: 'from-remote', label: 'Remote → local only' },
  { value: 'none', label: 'Disabled' },
]

export function encryptionPolicyLabel(p: EncryptionPolicy | undefined): string {
  return ENCRYPTION_POLICIES.find((x) => x.value === (p ?? 'prefer'))?.label ?? 'Prefer encryption (ask)'
}

export function clipboardDirectionLabel(d: ClipboardDirection | undefined): string {
  return CLIPBOARD_DIRECTIONS.find((x) => x.value === (d ?? 'both'))?.label ?? 'Both directions'
}

/** Owners and administrators may change a saved connection (the backend enforces it too). */
export function canEditConnection(conn: Connection | undefined, user: User | null): boolean {
  return !!conn && !!user && (conn.ownerId === user.id || user.role === 'admin')
}

/**
 * Set one VNC option of a saved connection. PATCH replaces the whole options object, so the latest options are
 * fetched first and only this key changes (`undefined` removes it = the default).
 */
export async function setConnectionOption(
  connectionId: string,
  key: 'encryption' | 'clipboardDirection',
  value: string | undefined,
): Promise<Connection> {
  const conn = await getConnection(connectionId)
  const options: Record<string, unknown> = { ...conn.options }
  if (value === undefined) delete options[key]
  else options[key] = value
  const updated = await updateConnection(connectionId, { options: options as Connection['options'] })
  queryClient.setQueryData(queryKeys.connection(connectionId), updated)
  queryClient.setQueryData<Connection[]>(queryKeys.connections, (old) => old?.map((c) => (c.id === connectionId ? updated : c)))
  return updated
}
