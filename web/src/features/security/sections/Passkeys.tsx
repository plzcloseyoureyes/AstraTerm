import { useEffect, useRef, useState } from 'react'
import { Cloud, Fingerprint, KeyRound, Pencil, Plus, Trash2, TriangleAlert, Usb } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tooltip } from '@/components/ui/tooltip'
import { errorMessage } from '@/lib/utils'
import { deletePasskey, renamePasskey, secQK, usePasskeys, usePasskeyStatus } from '../api'
import { Notice, PageHeader, When } from '../components'
import { openAddPasskey, withReauth } from '../store'
import type { Passkey } from '../types'
import { passkeyEnvironment } from '../webauthn'

export default function PasskeysSection() {
  const list = usePasskeys()
  const status = usePasskeyStatus()
  const env = passkeyEnvironment()
  const problem = !env.ok ? env.reason : status.data && !status.data.available ? status.data.reason : null
  const here = status.data?.rpId

  return (
    <>
      <PageHeader
        title="Passkeys"
        description="Sign in with your fingerprint, face, device PIN or a security key. Passkeys cannot be phished or reused."
        actions={
          // With no passkeys the empty state carries the one "add" action.
          !!list.data?.length && (
            <Button onClick={openAddPasskey} disabled={!!problem}>
              <Plus /> Add passkey
            </Button>
          )
        }
      />
      <div className="grid gap-4">
        {problem && (
          <Notice tone="warning" icon={TriangleAlert}>
            {problem}
          </Notice>
        )}
        <QueryState
          query={list}
          skeleton={<SkeletonRows rows={3} rowHeight={60} className="rounded-lg border bg-card" />}
          errorTitle="Could not load your passkeys"
          isEmpty={(d) => d.length === 0}
          empty={
            <div className="rounded-lg border border-dashed">
              <EmptyState
                icon={Fingerprint}
                title="No passkeys yet"
                description="Add one to sign in without typing a password, to unlock Termstead with Touch ID or Windows Hello, and as a second factor."
                action={
                  <Button onClick={openAddPasskey} disabled={!!problem}>
                    <Plus /> Add your first passkey
                  </Button>
                }
              />
            </div>
          }
        >
          {(items) => (
            <ul className="divide-y rounded-lg border bg-card">
              {items.map((p) => (
                <PasskeyRow key={p.id} passkey={p} elsewhere={!!here && p.rpId !== here} />
              ))}
            </ul>
          )}
        </QueryState>
        <p className="text-sm text-muted-foreground">
          Passkeys are bound to the address you use Termstead at{here ? ` (currently ${here})` : ''}. Synced passkeys follow you to your other
          devices through your platform account or password manager.
        </p>
      </div>
    </>
  )
}

function PasskeyRow({ passkey: p, elsewhere }: { passkey: Passkey; elsewhere: boolean }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(p.name)
  const [busy, setBusy] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (editing) input.current?.select()
  }, [editing])
  const hardware = p.transports.some((t) => t === 'usb' || t === 'nfc' || t === 'ble')
  const Icon = hardware ? Usb : p.synced ? Cloud : Fingerprint

  const rename = async () => {
    const next = name.trim()
    setEditing(false)
    if (!next || next === p.name) {
      setName(p.name)
      return
    }
    // Optimistic: show the new name at once.
    qc.setQueryData<Passkey[]>(secQK.passkeys, (old) => old?.map((x) => (x.id === p.id ? { ...x, name: next } : x)))
    try {
      await renamePasskey(p.id, next)
    } catch (err) {
      setName(p.name)
      toast.error('Could not rename the passkey', { description: errorMessage(err) })
    } finally {
      void qc.invalidateQueries({ queryKey: secQK.passkeys })
    }
  }

  const remove = async () => {
    const ok = await confirm({
      title: `Remove “${p.name}”?`,
      description: 'You will no longer be able to sign in with it. The passkey itself stays on your device until you delete it there.',
      confirmLabel: 'Remove',
      destructive: true,
    })
    if (!ok) return
    setBusy(true)
    try {
      const done = await withReauth(() => deletePasskey(p.id).then(() => true), 'Removing a passkey changes how you sign in.')
      if (!done) return
      void qc.invalidateQueries({ queryKey: secQK.passkeys })
      void qc.invalidateQueries({ queryKey: secQK.me })
      toast.success(`Removed “${p.name}”`)
    } catch (err) {
      toast.error('Could not remove the passkey', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <li className="group flex items-center gap-3 px-4 py-3">
      <div className="flex size-9 shrink-0 items-center justify-center rounded-lg border bg-muted/40">
        <Icon className="size-4 text-muted-foreground" />
      </div>
      <div className="grid min-w-0 flex-1 gap-0.5">
        {editing ? (
          <Input
            ref={input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            onBlur={() => void rename()}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void rename()
              if (e.key === 'Escape') {
                e.stopPropagation()
                setName(p.name)
                setEditing(false)
              }
            }}
            maxLength={64}
            aria-label="Passkey name"
            className="h-7 max-w-72"
          />
        ) : (
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <span className="truncate text-base font-medium" onDoubleClick={() => setEditing(true)}>
              {p.name}
            </span>
            {p.authenticator && p.authenticator !== p.name && <span className="text-sm text-muted-foreground">{p.authenticator}</span>}
            {p.synced && (
              <Tooltip content="Backed up and synced by your platform or password manager">
                <Badge variant="info">
                  <Cloud /> Synced
                </Badge>
              </Tooltip>
            )}
            {hardware && (
              <Badge variant="outline">
                <KeyRound /> Security key
              </Badge>
            )}
            {elsewhere && (
              <Tooltip content={`Registered for ${p.rpId}; it only works when Termstead is opened at that address`}>
                <Badge variant="warning">{p.rpId}</Badge>
              </Tooltip>
            )}
          </div>
        )}
        <div className="flex flex-wrap gap-x-3 text-sm text-muted-foreground">
          <span>
            Added <When value={p.createdAt} />
          </span>
          <span>
            Last used <When value={p.lastUsedAt} />
          </span>
        </div>
      </div>
      <div className="flex items-center gap-0.5 opacity-60 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
        <Tooltip content="Rename">
          <Button size="icon-sm" variant="ghost" aria-label={`Rename ${p.name}`} onClick={() => setEditing(true)}>
            <Pencil />
          </Button>
        </Tooltip>
        <Tooltip content="Remove">
          <Button size="icon-sm" variant="ghost" aria-label={`Remove ${p.name}`} className="hover:text-destructive" onClick={() => void remove()} loading={busy}>
            {!busy && <Trash2 />}
          </Button>
        </Tooltip>
      </div>
    </li>
  )
}
