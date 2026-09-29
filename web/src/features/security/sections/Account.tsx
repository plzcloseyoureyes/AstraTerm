import { useEffect, useRef, useState } from 'react'
import { Check, Fingerprint, KeyRound, Link2, Pencil, ShieldCheck, X } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { isApiError } from '@/api/client'
import { runCommand } from '@/app/commands'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput, PasswordStrengthMeter } from '@/components/ui/password-input'
import { ErrorState } from '@/components/ui/query-state'
import { LoadingPane } from '@/components/ui/spinner'
import { DELAY_PRESETS, useLoadingGate } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { setCurrentUser, useRunMode } from '@/stores/auth'
import { changePassword, secQK, updateMe, useMe } from '../api'
import { PageHeader, Section, StatusPill, When, passwordProblem, policyHint, usePasswordPolicy } from '../components'
import { withReauth } from '../store'

export default function AccountSection() {
  const me = useMe()
  const gate = useLoadingGate(me.isPending, DELAY_PRESETS.NAVIGATION)
  if (gate.hold || me.isPending) return <LoadingPane active={gate.show} immediate />
  if (me.isError) return <ErrorState error={me.error} title="Could not load your account" onRetry={() => void me.refetch()} />
  const d = me.data
  return (
    <>
      <PageHeader title="Account" description="Your profile, password and how you sign in." />
      <div className="grid gap-8">
        <Profile />
        <PasswordCard hasPassword={d.hasPassword} changedAt={d.passwordChangedAt} />
        <Section title="Ways to sign in" description="Add a second factor so a leaked password alone is not enough.">
          <div className="divide-y rounded-lg border bg-card">
            <MethodRow icon={KeyRound} title="Password" ok={d.hasPassword} status={d.hasPassword ? 'Set' : 'Not set (single sign-on account)'} />
            <MethodRow
              icon={ShieldCheck}
              title="Authenticator app"
              ok={d.user.totpEnabled}
              status={d.user.totpEnabled ? `On · ${d.recoveryCodesLeft} recovery codes left` : d.mfaRequired ? 'Required by your administrator' : 'Off'}
              action={() => void runCommand('security.twoFactor')}
              actionLabel={d.user.totpEnabled ? 'Manage' : 'Set up'}
            />
            <MethodRow
              icon={Fingerprint}
              title="Passkeys"
              ok={d.passkeys > 0}
              status={d.passkeys > 0 ? `${d.passkeys} registered` : 'None yet'}
              action={() => void runCommand('security.passkeys')}
              actionLabel={d.passkeys > 0 ? 'Manage' : 'Add'}
            />
          </div>
        </Section>
      </div>
    </>
  )
}

function MethodRow({ icon: Icon, title, status, ok, action, actionLabel }: { icon: typeof KeyRound; title: string; status: string; ok: boolean; action?: () => void; actionLabel?: string }) {
  return (
    <div className="flex items-center gap-3 px-4 py-3">
      <div className="flex size-8 items-center justify-center rounded-md border bg-muted/40">
        <Icon className="size-4 text-muted-foreground" />
      </div>
      <div className="grid min-w-0 flex-1 gap-0.5">
        <span className="text-base font-medium">{title}</span>
        <StatusPill ok={ok}>{status}</StatusPill>
      </div>
      {action && (
        <Button size="sm" variant="secondary" onClick={action}>
          {actionLabel}
        </Button>
      )}
    </div>
  )
}

function Profile() {
  const me = useMe()
  const qc = useQueryClient()
  const mode = useRunMode()
  const user = me.data!.user
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(user.displayName)
  const [busy, setBusy] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (editing) input.current?.select()
  }, [editing])
  const save = async () => {
    const next = name.trim()
    if (next === user.displayName) {
      setEditing(false)
      return
    }
    setBusy(true)
    try {
      const u = await updateMe({ displayName: next })
      setCurrentUser(u)
      void qc.invalidateQueries({ queryKey: secQK.me })
      setEditing(false)
      toast.success('Display name updated')
    } catch (err) {
      toast.error('Could not update the display name', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }
  const initial = (user.displayName || user.username).trim().charAt(0).toUpperCase()
  return (
    <div className="flex flex-wrap items-center gap-4 rounded-lg border bg-card p-4">
      <div className="flex size-12 items-center justify-center rounded-full bg-primary/15 text-lg font-semibold text-primary">{initial}</div>
      <div className="grid min-w-0 flex-1 gap-1">
        {editing ? (
          <form
            className="flex items-center gap-1.5"
            onSubmit={(e) => {
              e.preventDefault()
              void save()
            }}
          >
            <Input
              ref={input}
              value={name}
              onChange={(e) => setName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.stopPropagation()
                  setName(user.displayName)
                  setEditing(false)
                }
              }}
              maxLength={128}
              aria-label="Display name"
              className="h-7 max-w-64"
            />
            <Button type="submit" size="icon-sm" variant="ghost" aria-label="Save" loading={busy}>
              {!busy && <Check />}
            </Button>
            <Button size="icon-sm" variant="ghost" aria-label="Cancel" onClick={() => (setName(user.displayName), setEditing(false))}>
              <X />
            </Button>
          </form>
        ) : (
          <button
            type="button"
            className="group flex w-fit items-center gap-1.5 rounded text-md font-semibold outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
            onClick={() => setEditing(true)}
            title="Rename"
          >
            {user.displayName || user.username}
            <Pencil className="size-3.5 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 group-focus-visible:opacity-100" />
          </button>
        )}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">
          <span>@{user.username}</span>
          <Badge variant={user.role === 'admin' ? 'default' : 'secondary'}>{user.role === 'admin' ? 'Administrator' : 'User'}</Badge>
          {mode === 'desktop' && <span>Desktop mode</span>}
          <span>
            Member since <When value={user.createdAt} day />
          </span>
          {user.lastLoginAt && (
            <span>
              Last sign-in <When value={user.lastLoginAt} />
            </span>
          )}
        </div>
      </div>
    </div>
  )
}

function PasswordCard({ hasPassword, changedAt }: { hasPassword: boolean; changedAt?: string }) {
  const me = useMe()
  const qc = useQueryClient()
  const policy = usePasswordPolicy()
  const username = me.data?.user.username ?? ''
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const nextError = passwordProblem(next, policy, username)
  const againError = again && again !== next ? 'Passwords do not match' : null
  const canSubmit = (!hasPassword || !!current) && !!next && !!again && !nextError && !againError && !busy

  const submit = async () => {
    if (!canSubmit) return
    setBusy(true)
    setError(null)
    try {
      const ok = hasPassword
        ? (await changePassword(current, next), true)
        : await withReauth(() => changePassword('', next).then(() => true), 'Setting a password adds a way to sign in to your account.')
      if (!ok) return
      setCurrent('')
      setNext('')
      setAgain('')
      void qc.invalidateQueries({ queryKey: secQK.me })
      toast.success(hasPassword ? 'Password changed' : 'Password set', { description: 'Your other signed-in devices were signed out.' })
    } catch (err) {
      if (isApiError(err) && err.code === 'invalid_password') setError('The current password is incorrect.')
      else if (isApiError(err) && err.status === 429) setError('Too many attempts — wait a moment and try again.')
      else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      title={hasPassword ? 'Password' : 'Set a password'}
      description={
        hasPassword ? (
          <>
            {changedAt ? (
              <>
                Last changed <When value={changedAt} />.{' '}
              </>
            ) : null}
            Changing it signs out your other devices.
          </>
        ) : (
          'Your account was created by single sign-on. A password lets you sign in when the identity provider is unavailable.'
        )
      }
    >
      <form
        className="grid gap-4 rounded-lg border bg-card p-4"
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <input type="text" autoComplete="username" value={username} readOnly hidden />
        {hasPassword && (
          <Field label="Current password">
            <PasswordInput value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" />
          </Field>
        )}
        <div className="grid gap-4 @xl:grid-cols-2">
          <Field label="New password" error={nextError} hint={!nextError ? policyHint(policy) : undefined}>
            <PasswordInput value={next} onChange={(e) => setNext(e.target.value)} generate onGenerate={setAgain} autoComplete="new-password" />
          </Field>
          <Field label="Confirm new password" error={againError}>
            <PasswordInput value={again} onChange={(e) => setAgain(e.target.value)} autoComplete="new-password" />
          </Field>
        </div>
        {next && <PasswordStrengthMeter password={next} context={[username]} />}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        <div>
          <Button type="submit" size="sm" disabled={!canSubmit} loading={busy}>
            {hasPassword ? 'Change password' : 'Set password'}
          </Button>
        </div>
      </form>
      {!hasPassword && (
        <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
          <Link2 className="size-3.5" /> Single sign-on keeps working after you set a password.
        </p>
      )}
    </Section>
  )
}
