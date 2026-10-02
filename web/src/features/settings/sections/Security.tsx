import { useState } from 'react'
import { ExternalLink, Lock, LockKeyhole, LockOpen, ShieldAlert, ShieldCheck } from 'lucide-react'
import { toast } from 'sonner'
import { changePassword } from '@/api/auth'
import { isApiError } from '@/api/client'
import { lockVault, setMasterPassword } from '@/api/vault'
import { commands } from '@/app/registry'
import { runCommand } from '@/app/commands'
import { lockScreen } from '@/app/session'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Field } from '@/components/ui/field'
import { PasswordInput, PasswordStrengthMeter } from '@/components/ui/password-input'
import { SimpleSelect } from '@/components/ui/select'
import { estimateStrength } from '@/lib/password'
import { errorMessage } from '@/lib/utils'
import { setVaultHasMasterPassword, setVaultLocked, useAuthState, useCurrentUser, useIsAdmin } from '@/stores/auth'
import { securitySettings } from '@/stores/settings'
import { requestVaultUnlock } from '@/stores/ui'
import { SettingRow, SettingsGroup, SettingsPage } from '../ui'

const AUTO_LOCK_OPTIONS = [0, 1, 5, 10, 15, 30, 60, 120, 240]
const BUILTIN_SECURITY = new Set(['app.lock', 'vault.lock', 'vault.unlock'])
const MIN_PASSWORD = 8

function autoLockLabel(m: number): string {
  if (m === 0) return 'Never'
  if (m < 60) return `After ${m} minute${m === 1 ? '' : 's'}`
  return `After ${m / 60} hour${m === 60 ? '' : 's'}`
}

function ScreenLock() {
  const minutes = securitySettings.useValue('autoLockMinutes')
  const options = AUTO_LOCK_OPTIONS.includes(minutes) ? AUTO_LOCK_OPTIONS : [...AUTO_LOCK_OPTIONS, minutes].sort((a, b) => a - b)
  return (
    <SettingsGroup title="Screen lock" description="Locking hides and blurs everything; sessions keep running on the server.">
      <SettingRow label="Lock automatically" description="Lock the interface after this much time without keyboard or mouse input.">
        <SimpleSelect
          aria-label="Lock automatically"
          size="sm"
          className="w-44"
          value={String(minutes)}
          onValueChange={(v) => securitySettings.set({ autoLockMinutes: Number(v) })}
          options={options.map((m) => ({ value: String(m), label: autoLockLabel(m) }))}
        />
      </SettingRow>
      <SettingRow label="Lock now" description="You will need your account password to unlock.">
        <Button variant="secondary" size="sm" onClick={() => void lockScreen()}>
          <Lock /> Lock screen
        </Button>
      </SettingRow>
    </SettingsGroup>
  )
}

function AccountPassword() {
  const user = useCurrentUser()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const strength = estimateStrength(next, [user?.username ?? '', user?.displayName ?? ''])
  const nextError = next && next.length < MIN_PASSWORD ? `At least ${MIN_PASSWORD} characters` : next && strength.score < 1 ? 'Too easy to guess' : null
  const againError = again && again !== next ? 'Passwords do not match' : null
  const canSubmit = !!current && !!next && !!again && !nextError && !againError && !busy

  const submit = async () => {
    if (!canSubmit) return
    setBusy(true)
    setError(null)
    try {
      await changePassword({ currentPassword: current, newPassword: next })
      setCurrent('')
      setNext('')
      setAgain('')
      toast.success('Password changed')
    } catch (err) {
      if (isApiError(err) && (err.code === 'invalid_password' || err.status === 401)) setError('The current password is incorrect.')
      else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <SettingsGroup title="Account password" description={user ? `Signed in as ${user.username}.` : undefined}>
      <form
        className="grid gap-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <input type="text" autoComplete="username" value={user?.username ?? ''} readOnly hidden />
        <Field label="Current password">
          <PasswordInput value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" />
        </Field>
        <div className="grid gap-4 @xl:grid-cols-2">
          <Field label="New password" error={nextError}>
            <PasswordInput value={next} onChange={(e) => setNext(e.target.value)} generate onGenerate={setAgain} autoComplete="new-password" />
          </Field>
          <Field label="Confirm new password" error={againError}>
            <PasswordInput value={again} onChange={(e) => setAgain(e.target.value)} autoComplete="new-password" />
          </Field>
        </div>
        {next && <PasswordStrengthMeter password={next} context={[user?.username ?? '']} />}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        <div>
          <Button type="submit" size="sm" disabled={!canSubmit} loading={busy}>
            Change password
          </Button>
        </div>
      </form>
    </SettingsGroup>
  )
}

function MasterPassword() {
  const state = useAuthState()
  const isAdmin = useIsAdmin()
  const has = !!state?.vaultHasMasterPassword
  const locked = !!state?.vaultLocked
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState<'save' | 'remove' | 'lock' | null>(null)

  const nextError = next && next.length < MIN_PASSWORD ? `At least ${MIN_PASSWORD} characters` : null
  const againError = again && again !== next ? 'Passwords do not match' : null
  const canSave = !!next && !!again && !nextError && !againError && (!has || !!current) && !busy

  const reset = () => {
    setCurrent('')
    setNext('')
    setAgain('')
    setError(null)
  }

  const handleError = (err: unknown) => {
    if (isApiError(err) && err.code === 'wrong_password') setError('The current master password is incorrect.')
    else if (isApiError(err) && err.status === 403) setError('Only administrators can change the master password.')
    else if (isApiError(err) && err.status === 423) setError('Unlock the vault first.')
    else setError(errorMessage(err))
  }

  const save = async () => {
    if (!canSave) return
    setBusy('save')
    setError(null)
    try {
      await setMasterPassword({ currentPassword: has ? current : undefined, newPassword: next })
      setVaultHasMasterPassword(true)
      reset()
      toast.success(has ? 'Master password changed' : 'Master password set', {
        description: 'The vault will start locked after each server restart.',
      })
    } catch (err) {
      handleError(err)
    } finally {
      setBusy(null)
    }
  }

  const remove = async () => {
    if (!current) {
      setError('Enter the current master password to remove it.')
      return
    }
    const ok = await confirm({
      title: 'Remove the master password?',
      description: 'Secrets stay encrypted with the server key, but anyone with access to the data directory can decrypt them.',
      confirmLabel: 'Remove',
      destructive: true,
    })
    if (!ok) return
    setBusy('remove')
    setError(null)
    try {
      await setMasterPassword({ currentPassword: current, newPassword: '' })
      setVaultHasMasterPassword(false)
      setVaultLocked(false)
      reset()
      toast.success('Master password removed')
    } catch (err) {
      handleError(err)
    } finally {
      setBusy(null)
    }
  }

  const lockNow = async () => {
    setBusy('lock')
    try {
      await lockVault()
      setVaultLocked(true)
      toast.success('Vault locked')
    } catch (err) {
      toast.error('Could not lock the vault', { description: errorMessage(err) })
    } finally {
      setBusy(null)
    }
  }

  return (
    <SettingsGroup
      title="Vault master password"
      description="Protects saved passwords, passphrases and private keys with a password only you know."
    >
      <SettingRow
        label={
          <span className="flex items-center gap-2">
            Status
            {has ? (
              locked ? (
                <Badge variant="warning">
                  <LockKeyhole /> Locked
                </Badge>
              ) : (
                <Badge variant="success">
                  <LockOpen /> Unlocked
                </Badge>
              )
            ) : (
              <Badge variant="outline">Not set</Badge>
            )}
          </span>
        }
        description={
          has
            ? 'The vault starts locked after every server restart until an administrator unlocks it.'
            : 'Secrets are encrypted with a key stored on the server. Set a master password for stronger protection.'
        }
      >
        {has && locked && (
          <Button size="sm" onClick={() => void requestVaultUnlock()}>
            <LockOpen /> Unlock
          </Button>
        )}
        {has && !locked && isAdmin && (
          <Button size="sm" variant="secondary" onClick={() => void lockNow()} loading={busy === 'lock'}>
            <LockKeyhole /> Lock vault
          </Button>
        )}
      </SettingRow>
      {!isAdmin ? (
        <p className="px-4 py-3 text-sm text-muted-foreground">Only administrators can set or change the master password.</p>
      ) : locked ? (
        <p className="px-4 py-3 text-sm text-muted-foreground">Unlock the vault to change the master password.</p>
      ) : (
        <form
          className="grid gap-4 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <div className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/8 px-3 py-2 text-sm">
            <ShieldAlert className="mt-0.5 size-4 shrink-0 text-warning" />
            <span>If you forget the master password, saved secrets cannot be recovered.</span>
          </div>
          {has && (
            <Field label="Current master password">
              <PasswordInput value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="off" />
            </Field>
          )}
          <div className="grid gap-4 @xl:grid-cols-2">
            <Field label={has ? 'New master password' : 'Master password'} error={nextError}>
              <PasswordInput value={next} onChange={(e) => setNext(e.target.value)} generate onGenerate={setAgain} autoComplete="off" />
            </Field>
            <Field label="Confirm" error={againError}>
              <PasswordInput value={again} onChange={(e) => setAgain(e.target.value)} autoComplete="off" />
            </Field>
          </div>
          {next && <PasswordStrengthMeter password={next} />}
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button type="submit" size="sm" disabled={!canSave} loading={busy === 'save'}>
              <ShieldCheck /> {has ? 'Change master password' : 'Set master password'}
            </Button>
            {has && (
              <Button variant="ghost" size="sm" className="text-destructive" onClick={() => void remove()} loading={busy === 'remove'}>
                Remove master password
              </Button>
            )}
          </div>
        </form>
      )}
    </SettingsGroup>
  )
}

function MoreSecurity() {
  const all = commands.useList()
  const extra = all.filter((c) => c.category === 'Security' && !BUILTIN_SECURITY.has(c.id) && !c.hidden)
  if (!extra.length) return null
  return (
    <SettingsGroup title="More security settings" description="Two-factor authentication, passkeys, API tokens and sessions.">
      <div className="flex flex-wrap gap-2 p-4">
        {extra.map((c) => {
          const Icon = c.icon ?? ExternalLink
          return (
            <Button key={c.id} variant="secondary" size="sm" onClick={() => void runCommand(c.id, undefined, { source: 'menu' })}>
              <Icon /> {c.title}
            </Button>
          )
        })}
      </div>
    </SettingsGroup>
  )
}

export default function SecuritySection() {
  return (
    <SettingsPage title="Security" description="Screen lock, your account password and the secrets vault.">
      <ScreenLock />
      <AccountPassword />
      <MasterPassword />
      <MoreSecurity />
    </SettingsPage>
  )
}
