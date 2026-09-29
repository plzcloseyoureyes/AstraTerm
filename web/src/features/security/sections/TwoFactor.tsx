import { Fingerprint, KeyRound, RefreshCw, ShieldCheck, ShieldOff, Smartphone, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { regenerateRecoveryCodes, totpDisable } from '@/api/auth'
import { isApiError } from '@/api/client'
import { runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { confirm, prompt } from '@/components/ui/dialog-host'
import { ErrorState } from '@/components/ui/query-state'
import { LoadingPane } from '@/components/ui/spinner'
import { DELAY_PRESETS, useLoadingGate } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { refreshAuth } from '@/stores/auth'
import { secQK, useMe } from '../api'
import { Notice, PageHeader, Section, StatusPill } from '../components'
import { openTotpEnroll, showRecoveryCodes, withReauth } from '../store'

/** Ask for the account password (or, for SSO accounts, rely on a recent sign-in: the server decides). */
async function askPassword(hasPassword: boolean, title: string, description: string): Promise<string | null> {
  if (!hasPassword) return ''
  return prompt({ title, description, label: 'Password', type: 'password', confirmLabel: 'Continue', selectOnOpen: false })
}

function reauthError(err: unknown): string {
  if (isApiError(err) && err.code === 'invalid_password') return 'Incorrect password.'
  if (isApiError(err) && err.code === 'mfa_required') return err.message
  if (isApiError(err) && err.status === 429) return 'Too many attempts — wait a moment.'
  return errorMessage(err)
}

export default function TwoFactorSection() {
  const me = useMe()
  const qc = useQueryClient()
  const gate = useLoadingGate(me.isPending, DELAY_PRESETS.NAVIGATION)
  if (gate.hold || me.isPending) return <LoadingPane active={gate.show} immediate />
  if (me.isError) return <ErrorState error={me.error} title="Could not load your sign-in settings" onRetry={() => void me.refetch()} />
  const d = me.data
  const on = d.user.totpEnabled

  const refresh = () => {
    void refreshAuth()
    void qc.invalidateQueries({ queryKey: secQK.me })
  }

  const regenerate = async () => {
    const pw = await askPassword(d.hasPassword, 'New recovery codes', 'Your current recovery codes stop working. Enter your password to continue.')
    if (pw === null) return
    try {
      // Accounts without a password (single sign-on) confirm with a passkey / recent sign-in instead.
      const res = await withReauth(() => regenerateRecoveryCodes(pw), 'New recovery codes replace the current ones.')
      if (!res) return
      refresh()
      showRecoveryCodes(res.recoveryCodes, 'Your new recovery codes')
    } catch (err) {
      toast.error('Could not create new codes', { description: reauthError(err) })
    }
  }

  const disable = async () => {
    const ok = await confirm({
      title: 'Turn off two-factor authentication?',
      description: d.passkeys > 0 ? 'Your passkeys stay a second factor for password sign-ins.' : 'Signing in will only need your password.',
      confirmLabel: 'Turn off',
      destructive: true,
    })
    if (!ok) return
    const pw = await askPassword(d.hasPassword, 'Confirm with your password', 'Enter your password to turn off the authenticator app.')
    if (pw === null) return
    try {
      if ((await withReauth(() => totpDisable(pw).then(() => true), 'Turning off the authenticator app weakens your sign-in.')) !== true) return
      refresh()
      toast.success('Authenticator app turned off')
    } catch (err) {
      toast.error('Could not turn it off', { description: reauthError(err) })
    }
  }

  return (
    <>
      <PageHeader title="Two-factor authentication" description="A code from your phone (or a passkey) in addition to your password." />
      <div className="grid gap-8">
        {d.mfaRequired && !on && d.passkeys === 0 && (
          <Notice tone="warning" icon={TriangleAlert}>
            Your administrator requires a second factor. You will be asked to set one up the next time you sign in with your password.
          </Notice>
        )}
        <Section title="Authenticator app" description="Time-based one-time codes (TOTP) from any authenticator app.">
          <div className="flex flex-wrap items-center gap-4 rounded-lg border bg-card p-4">
            <div className="flex size-10 items-center justify-center rounded-lg border bg-muted/40">
              <Smartphone className="size-5 text-muted-foreground" />
            </div>
            <div className="grid min-w-0 flex-1 gap-0.5">
              <StatusPill ok={on}>{on ? 'On' : 'Off'}</StatusPill>
              <span className="text-sm text-muted-foreground">
                {on ? 'Password sign-ins ask for a 6-digit code.' : 'Protect your account with a code that changes every 30 seconds.'}
              </span>
            </div>
            {on ? (
              <Button variant="ghost" className="text-destructive" onClick={() => void disable()}>
                <ShieldOff /> Turn off
              </Button>
            ) : (
              <Button onClick={openTotpEnroll}>
                <ShieldCheck /> Set up
              </Button>
            )}
          </div>
        </Section>
        {on && (
          <Section
            title="Recovery codes"
            description="One-time codes for when your phone is not at hand. Keep them offline, e.g. printed or in a password manager."
            actions={
              <Button size="sm" variant="secondary" onClick={() => void regenerate()}>
                <RefreshCw /> New codes
              </Button>
            }
          >
            <div className="flex items-center gap-3 rounded-lg border bg-card px-4 py-3">
              <KeyRound className="size-4 text-muted-foreground" />
              <span className="flex-1 text-base tabular-nums">
                {d.recoveryCodesLeft} of 10 codes left
              </span>
              {d.recoveryCodesLeft <= 3 && <span className="text-sm text-warning">Running low — create new ones</span>}
            </div>
          </Section>
        )}
        <Section title="Passkeys as a second factor" description="Any registered passkey can stand in for the code.">
          <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3">
            <Fingerprint className="size-4 text-muted-foreground" />
            <span className="flex-1 text-base">{d.passkeys > 0 ? `${d.passkeys} passkey${d.passkeys === 1 ? '' : 's'} registered` : 'No passkeys yet'}</span>
            <Button size="sm" variant="secondary" onClick={() => void runCommand('security.passkeys')}>
              {d.passkeys > 0 ? 'Manage passkeys' : 'Add a passkey'}
            </Button>
          </div>
        </Section>
      </div>
    </>
  )
}
