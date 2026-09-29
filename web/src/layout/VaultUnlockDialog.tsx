import { useEffect, useState } from 'react'
import { LockKeyhole } from 'lucide-react'
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { unlockVault } from '@/api/vault'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { PasswordInput } from '@/components/ui/password-input'
import { errorMessage } from '@/lib/utils'
import { setVaultLocked, useIsAdmin } from '@/stores/auth'
import { finishVaultUnlock, useUIStore } from '@/stores/ui'

/** Master-password prompt shown when the vault is locked (HTTP 423) or from the status bar. */
export function VaultUnlockDialog() {
  const open = useUIStore((s) => s.vaultUnlockOpen)
  const isAdmin = useIsAdmin()
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setPassword('')
      setError(null)
      setBusy(false)
    }
  }, [open])

  const submit = async () => {
    if (!password) return
    setBusy(true)
    setError(null)
    try {
      await unlockVault(password)
      setPassword('')
      setVaultLocked(false)
      toast.success('Vault unlocked')
      finishVaultUnlock(true)
    } catch (err) {
      if (isApiError(err) && err.code === 'wrong_password') setError('Wrong master password.')
      else if (isApiError(err) && err.status === 403) setError('Only an administrator can unlock the vault.')
      else if (isApiError(err) && err.status === 429) setError(err.message || 'Too many attempts. Try again later.')
      else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !o && finishVaultUnlock(false)}>
      <DialogContent size="sm">
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <LockKeyhole className="size-4.5 text-warning" />
              Vault is locked
            </DialogTitle>
            <DialogDescription>
              Saved passwords and keys are encrypted with the master password. Unlock the vault to use them.
              {!isAdmin && ' An administrator has to unlock it.'}
            </DialogDescription>
          </DialogHeader>
          <Field label="Master password" error={error}>
            <PasswordInput value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus />
          </Field>
          <DialogFooter>
            <Button variant="secondary" onClick={() => finishVaultUnlock(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={busy} disabled={!password}>
              Unlock
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
