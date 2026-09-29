/*
 * Actions shared by the Keys tab, commands, menus and the status item.
 */
import { toast } from 'sonner'
import { deleteIdentity } from '@/api/identities'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Identity, KnownHost } from '@/api/types'
import { confirm, prompt } from '@/components/ui/dialog-host'
import { errorMessage, plural } from '@/lib/utils'
import { openTab } from '@/stores/workspace'
import {
  bulkDeleteKnownHosts,
  deleteKey,
  deleteMarker,
  exportKnownHostsText,
  invalidateAgent,
  invalidateKeys,
  invalidateKnownHosts,
  lockAgent,
  startAgent,
  stopAgent,
  unlockAgent,
  updateKey,
} from './api'
import { keysSettings } from './settings'
import type { HostKeyMarker, KeysTab, KeysTabParams, StoredKey } from './types'
import { copyWithToast, downloadText, toastError } from './util'

export function openKeysTab(tab?: KeysTab): void {
  openTab<KeysTabParams>({ kind: 'keys', params: tab ? { tab } : undefined })
}

export function copyPublicKey(k: Pick<StoredKey, 'publicKey'>): void {
  void copyWithToast(k.publicKey, 'Public key copied')
}

export async function deleteKeyAction(k: StoredKey): Promise<void> {
  const uses = [
    k.usedBy.connections ? plural(k.usedBy.connections, 'session') : '',
    k.usedBy.identities ? plural(k.usedBy.identities, 'identity', 'identities') : '',
  ].filter(Boolean)
  const ok = await confirm({
    title: `Delete the key “${k.name}”?`,
    description: uses.length
      ? `It is used by ${uses.join(' and ')}, which will no longer log in with it. The private key cannot be recovered unless you exported it.`
      : 'The private key cannot be recovered unless you exported it.',
    confirmLabel: 'Delete key',
    destructive: true,
  })
  if (!ok) return
  try {
    await deleteKey(k.id)
    invalidateKeys()
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
    void queryClient.invalidateQueries({ queryKey: queryKeys.identities })
    const excluded = keysSettings.get().agentExclude
    if (excluded.includes(k.id)) keysSettings.set({ agentExclude: excluded.filter((id) => id !== k.id) })
    toast.success(`Deleted “${k.name}”`)
  } catch (err) {
    toastError('Could not delete the key', err)
  }
}

export async function renameKeyAction(k: StoredKey): Promise<void> {
  const name = await prompt({ title: 'Rename key', label: 'Name', defaultValue: k.name, validate: (v) => (v.trim() ? null : 'The name is required') })
  if (name === null || name.trim() === k.name) return
  try {
    await updateKey(k.id, { name: name.trim() })
    invalidateKeys()
  } catch (err) {
    toastError('Could not rename the key', err)
  }
}

export async function editCommentAction(k: StoredKey): Promise<void> {
  const comment = await prompt({
    title: 'Edit comment',
    description: 'The comment is part of the public key line (authorized_keys) and of exported files.',
    label: 'Comment',
    defaultValue: k.comment,
  })
  if (comment === null || comment === k.comment) return
  try {
    await updateKey(k.id, { comment })
    invalidateKeys()
  } catch (err) {
    toastError('Could not change the comment', err)
  }
}

export async function deleteIdentityAction(i: Identity, sessions: number): Promise<void> {
  const ok = await confirm({
    title: `Delete the identity “${i.name}”?`,
    description: sessions
      ? `${plural(sessions, 'session uses', 'sessions use')} it; they keep their own settings but lose these credentials.`
      : 'No session uses it.',
    confirmLabel: 'Delete identity',
    destructive: true,
  })
  if (!ok) return
  try {
    await deleteIdentity(i.id)
    void queryClient.invalidateQueries({ queryKey: queryKeys.identities })
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
    invalidateKeys()
    toast.success(`Deleted “${i.name}”`)
  } catch (err) {
    toastError('Could not delete the identity', err)
  }
}

export async function deleteKnownHostsAction(hosts: KnownHost[]): Promise<void> {
  if (!hosts.length) return
  const one = hosts.length === 1 ? hosts[0] : null
  const ok = await confirm({
    title: one ? `Forget the ${one.keyType} key of ${hostLabel(one)}?` : `Forget ${plural(hosts.length, 'host key')}?`,
    description: 'The next connection asks again whether to trust the host.',
    confirmLabel: 'Forget',
    destructive: true,
  })
  if (!ok) return
  try {
    const res = await bulkDeleteKnownHosts(hosts.map((h) => h.id))
    invalidateKnownHosts()
    toast.success(`Forgot ${plural(res.deleted, 'host key')}`)
  } catch (err) {
    toastError('Could not delete the host keys', err)
  }
}

export async function deleteMarkerAction(m: HostKeyMarker): Promise<void> {
  const ca = m.marker === 'cert-authority'
  const ok = await confirm({
    title: ca ? 'Stop trusting this certificate authority?' : 'Remove the revocation?',
    description: ca ? `Host certificates of ${m.hosts} signed by it will need to be confirmed again.` : 'Connections presenting this key will be allowed again.',
    confirmLabel: ca ? 'Stop trusting' : 'Remove',
    destructive: ca,
  })
  if (!ok) return
  try {
    await deleteMarker(m.id)
    invalidateKnownHosts()
  } catch (err) {
    toastError('Could not delete the entry', err)
  }
}

export async function exportKnownHostsAction(hashed: boolean): Promise<void> {
  try {
    downloadText(await exportKnownHostsText(hashed), 'known_hosts')
  } catch (err) {
    toastError('Could not export the known hosts', err)
  }
}

export function hostLabel(h: Pick<KnownHost, 'host' | 'port'>): string {
  return h.port && h.port !== 22 ? `[${h.host}]:${h.port}` : h.host
}

// ---- agent ------------------------------------------------------------------------------------------------------------

export async function startAgentAction(): Promise<void> {
  try {
    const st = await startAgent()
    invalidateAgent()
    toast.success('SSH agent started', { description: st.socketPath })
  } catch (err) {
    toast.error('Could not start the SSH agent', { description: errorMessage(err) })
  }
}

export async function stopAgentAction(): Promise<void> {
  try {
    await stopAgent()
    invalidateAgent()
    toast.success('SSH agent stopped')
  } catch (err) {
    toastError('Could not stop the SSH agent', err)
  }
}

export async function lockAgentAction(locked: boolean): Promise<void> {
  try {
    await (locked ? lockAgent() : unlockAgent())
    invalidateAgent()
  } catch (err) {
    toastError(locked ? 'Could not lock the agent' : 'Could not unlock the agent', err)
  }
}

/** Offer (or stop offering) a stored key through the agent and agent forwarding (settings agentExclude). */
export function setKeyOffered(keyId: string, offered: boolean): void {
  const cur = keysSettings.get().agentExclude
  const next = offered ? cur.filter((id) => id !== keyId) : Array.from(new Set([...cur, keyId]))
  keysSettings.set({ agentExclude: next })
  setTimeout(invalidateAgent, 600) // after the debounced settings write
}
