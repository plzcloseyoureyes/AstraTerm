/*
 * Passphrase of a stored key: add / change / remove it (the stored key is re-encrypted), remember it in the vault
 * (no prompt when the key is used) or forget the remembered one (AstraTerm asks each time).
 */
import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { LockKeyhole } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { PasswordInput, PasswordStrengthMeter } from '@/components/ui/password-input'
import { RadioField, RadioGroup } from '@/components/ui/radio-group'
import { errorMessage } from '@/lib/utils'
import { invalidateKeys, updateKey, useKeys } from '../api'
import type { KeyPatch } from '../types'

type Action = 'change' | 'remove' | 'remember' | 'forget' | 'add'

export default function PassphraseDialog({ keyId, onClose }: { keyId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const { data: keys } = useKeys()
  const k = keys?.find((x) => x.id === keyId)
  const [choice, setChoice] = useState<Action | null>(null)
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [next2, setNext2] = useState('')
  const [remember, setRemember] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (!k) {
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent size="sm">
          <DialogHeader>
            <DialogTitle>Passphrase</DialogTitle>
            <DialogDescription className="sr-only">Key passphrase</DialogDescription>
          </DialogHeader>
          <EmptyState size="sm" title="Key not found" />
        </DialogContent>
      </Dialog>
    )
  }

  const action: Action = choice ?? (k.hasPassphrase ? 'change' : 'add')
  const needCurrent = k.hasPassphrase && !k.passphraseSaved && (action === 'change' || action === 'remove' || action === 'remember')
  const needNext = action === 'change' || action === 'add'
  const mismatch = needNext && next !== next2
  const disabled = (needCurrent && !current) || (needNext && (!next || mismatch))

  const submit = async () => {
    const patch: KeyPatch = {}
    if (needCurrent || action === 'remember') patch.passphrase = current
    switch (action) {
      case 'change':
      case 'add':
        patch.newPassphrase = next
        patch.rememberPassphrase = remember
        break
      case 'remove':
        patch.newPassphrase = ''
        break
      case 'forget':
        patch.passphrase = ''
        break
    }
    setBusy(true)
    setError(null)
    try {
      await updateKey(k.id, patch)
      invalidateKeys(qc)
      toast.success(
        { change: 'Passphrase changed', add: 'Passphrase added', remove: 'Passphrase removed', remember: 'Passphrase remembered', forget: 'Passphrase forgotten' }[action],
      )
      onClose()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="md">
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            if (!disabled) void submit()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <LockKeyhole className="size-4.5 text-primary" /> Passphrase of “{k.name}”
            </DialogTitle>
            <DialogDescription>
              {k.hasPassphrase
                ? k.passphraseSaved
                  ? 'The key is protected by a passphrase that is remembered in the vault.'
                  : 'The key is protected by a passphrase; AstraTerm asks for it when the key is used.'
                : 'The key has no passphrase (the AstraTerm vault still encrypts it at rest).'}
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="grid gap-4">
            {k.hasPassphrase && (
              <RadioGroup value={action} onValueChange={(v) => setChoice(v as Action)} aria-label="Action">
                <RadioField value="change" label="Change the passphrase" />
                {!k.passphraseSaved && <RadioField value="remember" label="Remember the passphrase in the vault" description="Logins and the agent no longer ask for it." />}
                {k.passphraseSaved && <RadioField value="forget" label="Forget the remembered passphrase" description="AstraTerm will ask for it whenever the key is used." />}
                <RadioField value="remove" label="Remove the passphrase" />
              </RadioGroup>
            )}
            {needCurrent && (
              <Field label="Current passphrase">
                <PasswordInput value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="off" autoFocus />
              </Field>
            )}
            {needNext && (
              <>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="New passphrase">
                    <PasswordInput value={next} onChange={(e) => setNext(e.target.value)} generate onGenerate={setNext2} autoComplete="new-password" />
                  </Field>
                  <Field label="Confirm" error={mismatch && next2 ? 'The passphrases differ' : undefined}>
                    <PasswordInput value={next2} onChange={(e) => setNext2(e.target.value)} autoComplete="new-password" />
                  </Field>
                </div>
                {next && <PasswordStrengthMeter password={next} />}
                <CheckboxField checked={remember} onCheckedChange={(v) => setRemember(v === true)} label="Remember the new passphrase in the vault" />
              </>
            )}
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
            <Button type="submit" loading={busy} disabled={disabled} variant={action === 'remove' ? 'destructive' : 'default'}>
              {{ change: 'Change', add: 'Add passphrase', remove: 'Remove passphrase', remember: 'Remember', forget: 'Forget' }[action]}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
