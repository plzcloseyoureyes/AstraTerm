/*
 * Create / edit an identity (SM-7): a reusable credential set — user name, password (write-only), SSH key and its
 * passphrase — that sessions reference; changing it once applies everywhere.
 */
import { useState } from 'react'
import { toast } from 'sonner'
import { Eraser, UserRound } from 'lucide-react'
import { useCreateIdentity, useIdentities, useUpdateIdentity } from '@/api/identities'
import type { IdentityInput } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { Spinner } from '@/components/ui/spinner'
import { errorMessage } from '@/lib/utils'
import { useKeys } from '../api'
import { KeySelect } from '../components'
import { openKeysDialog } from '../store'

export default function IdentityDialog({ id, initial, onClose }: { id?: string; initial?: Partial<IdentityInput>; onClose: () => void }) {
  const identities = useIdentities()
  if (id && identities.isLoading) {
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent size="sm">
          <DialogHeader>
            <DialogTitle>Identity</DialogTitle>
            <DialogDescription className="sr-only">Loading</DialogDescription>
          </DialogHeader>
          <div className="flex justify-center py-6">
            <Spinner />
          </div>
        </DialogContent>
      </Dialog>
    )
  }
  return <IdentityForm id={id} initial={initial} onClose={onClose} />
}

function IdentityForm({ id, initial, onClose }: { id?: string; initial?: Partial<IdentityInput>; onClose: () => void }) {
  const identities = useIdentities()
  const keys = useKeys()
  const existing = id ? identities.data?.find((i) => i.id === id) : undefined
  const create = useCreateIdentity()
  const update = useUpdateIdentity()
  const [name, setName] = useState(existing?.name ?? initial?.name ?? '')
  const [username, setUsername] = useState(existing?.username ?? initial?.username ?? '')
  const [keyId, setKeyId] = useState(existing?.keyId ?? initial?.keyId ?? '')
  const [password, setPassword] = useState('')
  const [passphrase, setPassphrase] = useState('')
  // Secrets already stored for the identity, and the ones cleared in this edit.
  const [cleared, setCleared] = useState<Record<string, boolean>>({})
  const [error, setError] = useState<string | null>(null)
  const busy = create.isPending || update.isPending
  const has = (k: string) => !!existing?.secretKeys.includes(k) && !cleared[k]
  const selectedKey = keys.data?.find((k) => k.id === keyId)

  if (id && !existing && identities.isSuccess) {
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent size="sm">
          <DialogHeader>
            <DialogTitle>Identity not found</DialogTitle>
            <DialogDescription>It may have been deleted.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button onClick={onClose}>Close</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    )
  }

  const save = async () => {
    setError(null)
    if (!name.trim()) {
      setError('The name is required')
      return
    }
    const secrets: Record<string, string> = {}
    if (password) secrets.password = password
    else if (cleared.password) secrets.password = ''
    if (passphrase) secrets.passphrase = passphrase
    else if (cleared.passphrase) secrets.passphrase = ''
    const body: IdentityInput = { name: name.trim(), username: username.trim(), keyId: keyId || null, ...(Object.keys(secrets).length ? { secrets } : {}) }
    try {
      if (existing) await update.mutateAsync({ id: existing.id, patch: body })
      else await create.mutateAsync(body)
      toast.success(existing ? 'Identity saved' : `Identity “${body.name}” created`)
      onClose()
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  const secretField = (key: 'password' | 'passphrase', label: string, value: string, set: (v: string) => void, hint?: string) => (
    <Field
      label={label}
      hint={hint}
      labelAside={
        has(key) ? (
          <span className="flex items-center gap-1.5">
            <Badge variant="success">Stored</Badge>
            <Button type="button" variant="link" size="xs" onClick={() => setCleared((c) => ({ ...c, [key]: true }))}>
              <Eraser /> Clear
            </Button>
          </span>
        ) : cleared[key] ? (
          <Badge variant="warning">Will be removed</Badge>
        ) : undefined
      }
    >
      <PasswordInput
        value={value}
        onChange={(e) => {
          set(e.target.value)
          if (e.target.value) setCleared((c) => ({ ...c, [key]: false }))
        }}
        placeholder={has(key) ? '•••••••• (unchanged)' : 'Not set'}
        autoComplete="new-password"
      />
    </Field>
  )

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="md">
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <UserRound className="size-4.5 text-primary" /> {existing ? `Edit identity “${existing.name}”` : 'New identity'}
            </DialogTitle>
            <DialogDescription>Sessions using this identity log in with these credentials; edit them once to update every session.</DialogDescription>
          </DialogHeader>
          <DialogBody className="grid gap-4">
            <Field label="Name" required>
              <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Production admin" maxLength={200} autoFocus />
            </Field>
            <Field label="User name" hint="Used by sessions that leave their user name empty.">
              <Input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="root" autoComplete="off" spellCheck={false} maxLength={255} />
            </Field>
            {secretField('password', 'Password', password, setPassword, 'Stored encrypted; never shown again.')}
            <Field
              label="SSH key"
              labelAside={
                <Button type="button" variant="link" size="xs" onClick={() => openKeysDialog('generate', {})}>
                  Generate a key…
                </Button>
              }
            >
              <KeySelect keys={keys.data} value={keyId} onChange={setKeyId} noneLabel="None" />
            </Field>
            {selectedKey?.hasPassphrase && !selectedKey.passphraseSaved &&
              secretField('passphrase', 'Key passphrase', passphrase, setPassphrase, 'The passphrase of this key is not remembered with the key; store it here to avoid prompts.')}
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={busy}>
              {existing ? 'Save' : 'Create'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
