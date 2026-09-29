/*
 * Login-screen step shown when the login policy requires a second factor and the account has none yet: TOTP
 * enrollment against the pending sign-in, then the recovery codes, then into the app.
 */
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { refreshAuth } from '@/stores/auth'
import { enrollTotpEnable, enrollTotpSetup } from './api'
import { RecoveryCodeActions, RecoveryCodesList, TotpEnrollForm } from './Overlay'

export default function LoginEnroll({ mfaToken, username, onCancel }: { mfaToken: string; username?: string; onCancel: () => void }) {
  const [codes, setCodes] = useState<string[] | null>(null)
  const [saved, setSaved] = useState(false)
  if (codes) {
    return (
      <div className="grid gap-4">
        <p className="text-sm text-muted-foreground">
          Two-factor authentication is on. Save these one-time recovery codes — each signs you in once if your phone is not at hand.
        </p>
        <RecoveryCodesList codes={codes} />
        <RecoveryCodeActions codes={codes} username={username} />
        <CheckboxField checked={saved} onCheckedChange={(v) => setSaved(v === true)} label="I saved these codes somewhere safe" />
        <Button size="lg" disabled={!saved} onClick={() => void refreshAuth()}>
          Continue to Termstead
        </Button>
      </div>
    )
  }
  return (
    <TotpEnrollForm
      setup={() => enrollTotpSetup(mfaToken)}
      enable={async (code) => {
        const res = await enrollTotpEnable(mfaToken, code)
        return { recoveryCodes: res.recoveryCodes }
      }}
      onDone={setCodes}
      onCancel={onCancel}
      submitLabel="Verify and sign in"
    />
  )
}
