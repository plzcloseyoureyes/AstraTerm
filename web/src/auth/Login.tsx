import { lazy, useEffect, useRef, useState } from 'react'
import { ArrowLeft, Fingerprint, Info, LogIn, ShieldCheck, TriangleAlert } from 'lucide-react'
import { login } from '@/api/auth'
import { isApiError } from '@/api/client'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Kbd } from '@/components/ui/kbd'
import { PasswordInput } from '@/components/ui/password-input'
import { LazyBoundary, Spinner } from '@/components/ui/spinner'
import { mfaTotp } from '@/features/security/api'
import { IconOrSpinner } from '@/features/security/components'
import { ssoErrorMessage } from '@/features/security/ssoErrors'
import type { LoginMethods, MfaChallenge } from '@/features/security/types'
import {
  cancelConditionalPasskey,
  isPasskeyCancel,
  passkeyEnvironment,
  passkeyErrorMessage,
  passkeySecondFactor,
  signInWithPasskey,
  startConditionalPasskey,
} from '@/features/security/webauthn'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { clearAuthNotice, refreshAuth, useAuthState, useAuthStore } from '@/stores/auth'
import { AuthCard, AuthLayout, BrandMark } from './AuthLayout'
import { useIndicatorSettled } from './useIndicatorSettled'

const LoginEnroll = lazy(() => import('@/features/security/LoginEnroll'))

function loginError(err: unknown): string {
  if (isApiError(err)) {
    if (err.status === 429) return 'Too many attempts. Please wait a moment and try again.'
    if (err.code === 'account_locked') return 'This account is temporarily locked after too many failed attempts. Try again later or ask an administrator.'
    if (err.code === 'login_not_allowed') return 'Signing in is not allowed from your network.'
    if (err.code === 'password_login_disabled') return 'Password sign-in is turned off. Use single sign-on or a passkey.'
    if (err.status === 401) return err.code === 'totp_invalid' ? 'Invalid verification code.' : 'Invalid username or password.'
    if (err.status === 403) return err.message || 'This account is disabled.'
  }
  return errorMessage(err)
}

/** The 401 body of a password login that needs a second factor (older servers send only the code). */
function challengeOf(err: unknown): MfaChallenge | null {
  if (!isApiError(err)) return null
  if (err.code !== 'totp_required' && err.code !== 'mfa_required' && err.code !== 'mfa_enrollment_required') return null
  const b = (err.body ?? {}) as Partial<MfaChallenge>
  return {
    code: err.code,
    methods: Array.isArray(b.methods) && b.methods.length ? b.methods : ['totp'],
    mfaToken: typeof b.mfaToken === 'string' ? b.mfaToken : '',
    expiresIn: b.expiresIn ?? 300,
  }
}

/** Takes ?sso_error= from the address bar (set by the single-sign-on callback). */
function takeSsoError(): string | null {
  try {
    const url = new URL(window.location.href)
    const code = url.searchParams.get('sso_error')
    if (!code) return null
    url.searchParams.delete('sso_error')
    window.history.replaceState(window.history.state, '', url.pathname + url.search + url.hash)
    return ssoErrorMessage(code)
  } catch {
    return null
  }
}

type Step = 'password' | 'mfa' | 'enroll'

/** Sign-in: password (+ second factor: authenticator code, recovery code or passkey), passwordless passkeys and SSO. */
export function Login() {
  const state = useAuthState()
  const notice = useAuthStore((s) => s.notice)
  const methods = (state as { loginMethods?: LoginMethods } | null)?.loginMethods
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [remember, setRemember] = useState(true)
  const [step, setStep] = useState<Step>('password')
  const [challenge, setChallenge] = useState<MfaChallenge | null>(null)
  const [useRecovery, setUseRecovery] = useState(false)
  const [code, setCode] = useState('')
  const [error, setError] = useState<string | null>(() => takeSsoError())
  const [busy, setBusy] = useState<'password' | 'code' | 'passkey' | null>(null)
  const [conditionalGen, setConditionalGen] = useState(0)
  const rememberRef = useRef(remember)
  const passwordRef = useRef<HTMLInputElement>(null)
  rememberRef.current = remember

  const env = passkeyEnvironment()
  const passkeys = !!methods?.passkey && env.ok
  const sso = methods?.sso ?? []
  const allowRemember = methods?.remember ?? true

  // Buttons wait for the same delay as the spinners before they look disabled: a quick sign-in changes nothing on
  // screen until the app replaces it (docs/UX.md "Motion & feedback"); `busy` alone guards against double submits.
  const busyShown = useDelayedFlag(busy !== null)
  const indicatorSettled = useIndicatorSettled(busy !== null)

  const done = async () => {
    // A spinner that did appear keeps its minimum time before the app replaces this screen (which also discards the
    // typed password and code).
    setBusy(null)
    await indicatorSettled()
    clearAuthNotice()
    await refreshAuth()
  }

  // Passkeys in the username field's autofill (conditional UI) while the password step is shown.
  useEffect(() => {
    if (step !== 'password' || !passkeys) return
    let active = true
    startConditionalPasskey(() => rememberRef.current && allowRemember)
      .then((u) => {
        if (u && active) void done()
      })
      .catch((err) => {
        if (active && !isPasskeyCancel(err)) setError(passkeyErrorMessage(err))
      })
    return () => {
      active = false
      cancelConditionalPasskey()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, passkeys, conditionalGen])

  const backToPassword = (message?: string) => {
    setStep('password')
    setChallenge(null)
    setCode('')
    setUseRecovery(false)
    setError(message ?? null)
  }

  const submitPassword = async () => {
    if (!username.trim() || !password) {
      setError('Enter your username and password.')
      return
    }
    setBusy('password')
    setError(null)
    try {
      await login({ username: username.trim(), password, remember: remember && allowRemember })
      await done()
    } catch (err) {
      const ch = challengeOf(err)
      if (ch) {
        setChallenge(ch)
        setCode('')
        setUseRecovery(false)
        setStep(ch.code === 'mfa_enrollment_required' ? 'enroll' : 'mfa')
      } else {
        setError(loginError(err))
        // Wrong password: select it, so retyping replaces it (keyboard-first).
        if (isApiError(err) && err.status === 401) requestAnimationFrame(() => {
          passwordRef.current?.focus()
          passwordRef.current?.select()
        })
      }
    } finally {
      setBusy(null)
    }
  }

  const submitCode = async () => {
    if (!challenge) return
    const value = useRecovery ? code.trim() : code.replace(/\s+/g, '')
    if (!value) {
      setError(useRecovery ? 'Enter a recovery code.' : 'Enter the 6-digit code from your authenticator app.')
      return
    }
    setBusy('code')
    setError(null)
    try {
      if (challenge.mfaToken) await mfaTotp(challenge.mfaToken, value)
      else await login({ username: username.trim(), password, remember: remember && allowRemember, totp: value }) // older servers
      await done()
    } catch (err) {
      if (isApiError(err) && err.code === 'mfa_expired') backToPassword('The sign-in took too long. Please enter your password again.')
      else if (isApiError(err) && (err.code === 'totp_invalid' || err.code === 'totp_required')) {
        setError(useRecovery ? 'Invalid recovery code.' : 'Invalid verification code.')
        setCode('')
      } else setError(loginError(err))
    } finally {
      setBusy(null)
    }
  }

  const confirmWithPasskey = async () => {
    if (!challenge?.mfaToken) return
    setBusy('passkey')
    setError(null)
    try {
      await passkeySecondFactor(challenge.mfaToken)
      await done()
    } catch (err) {
      if (isApiError(err) && err.code === 'mfa_expired') backToPassword('The sign-in took too long. Please enter your password again.')
      else if (!isPasskeyCancel(err)) setError(passkeyErrorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  const passwordless = async () => {
    setBusy('passkey')
    setError(null)
    try {
      await signInWithPasskey(remember && allowRemember)
      await done()
    } catch (err) {
      if (!isPasskeyCancel(err)) setError(isApiError(err) ? loginError(err) : passkeyErrorMessage(err))
      setConditionalGen((g) => g + 1) // the modal request replaced the autofill one: offer autofill again
    } finally {
      setBusy(null)
    }
  }

  const ssoLogin = (id: string) => {
    const q = new URLSearchParams({ provider: id })
    if (remember && allowRemember) q.set('remember', '1')
    window.location.assign(`/api/auth/oidc/login?${q}`)
  }

  const hasTotp = !!challenge?.methods.includes('totp')
  const hasPasskey = !!challenge?.methods.includes('webauthn') && !!challenge.mfaToken

  const title = step === 'enroll' ? 'Set up two-factor authentication' : step === 'mfa' ? 'Two-factor authentication' : 'Sign in to Termstead'
  const subtitle =
    step === 'enroll'
      ? 'Your administrator requires a second factor for this account.'
      : step === 'mfa'
        ? hasPasskey && !hasTotp
          ? 'Confirm with your passkey.'
          : useRecovery
            ? 'Enter one of your saved recovery codes.'
            : hasPasskey
              ? 'Use your passkey or a code from your authenticator app.'
              : 'Enter the code from your authenticator app.'
        : 'Your sessions keep running while you are away.'

  const errorBox = error && (
    <p role="alert" className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
      <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
      <span>{error}</span>
    </p>
  )

  const signingInAs = (
    <div className="flex items-center gap-2 rounded-md border bg-muted/40 px-3 py-2 text-sm">
      <ShieldCheck className="size-4 text-success" />
      <span className="truncate">
        Signing in as <strong>{username}</strong>
      </span>
    </div>
  )

  return (
    <AuthLayout wide={step === 'enroll'} footer={state?.version ? `Termstead ${state.version}${state.mode === 'server' ? ' · server mode' : ''}` : undefined}>
      <div className="mb-6 flex flex-col items-center gap-3 text-center">
        <BrandMark />
        <div>
          <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          <p className="text-base text-muted-foreground">{subtitle}</p>
        </div>
      </div>
      <AuthCard>
        {step === 'enroll' && challenge ? (
          <div className="grid gap-4">
            {signingInAs}
            <div className="relative min-h-24">
              <LazyBoundary className="bg-card">
                <LoginEnroll mfaToken={challenge.mfaToken} username={username.trim()} onCancel={() => backToPassword()} />
              </LazyBoundary>
            </div>
          </div>
        ) : (
          <form
            className="grid gap-4"
            noValidate
            onKeyDown={(e) => {
              // Esc on the second-factor step goes back to the password (keyboard-first, like the Back button).
              if (e.key === 'Escape' && step === 'mfa' && !busy) {
                e.preventDefault()
                backToPassword()
              }
            }}
            onSubmit={(e) => {
              e.preventDefault()
              if (busy) return
              if (step === 'mfa' && !hasTotp) void confirmWithPasskey()
              else void (step === 'mfa' ? submitCode() : submitPassword())
            }}
          >
            {notice && step === 'password' && (
              <div className="flex items-start gap-2 rounded-md border bg-muted/50 px-3 py-2 text-sm text-muted-foreground">
                <Info className="mt-0.5 size-3.5 shrink-0" />
                <span>{notice}</span>
              </div>
            )}
            {step === 'password' ? (
              <>
                <Field label="Username">
                  <Input
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                    autoComplete={passkeys ? 'username webauthn' : 'username'}
                    autoCapitalize="off"
                    spellCheck={false}
                    autoFocus
                  />
                </Field>
                <Field label="Password">
                  <PasswordInput ref={passwordRef} value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
                </Field>
                {allowRemember && <CheckboxField checked={remember} onCheckedChange={(v) => setRemember(v === true)} label="Keep me signed in" />}
              </>
            ) : (
              <>
                {signingInAs}
                {hasPasskey && (
                  <Button size="lg" variant={hasTotp ? 'secondary' : 'default'} onClick={() => !busy && void confirmWithPasskey()} disabled={busyShown && busy !== 'passkey'} autoFocus={!hasTotp}>
                    <IconOrSpinner icon={Fingerprint} busy={busy === 'passkey'} label="Waiting for the passkey" /> Use your passkey
                  </Button>
                )}
                {hasPasskey && hasTotp && (
                  <div className="flex items-center gap-3 text-xs text-muted-foreground">
                    <span className="h-px flex-1 bg-border" /> or enter a code <span className="h-px flex-1 bg-border" />
                  </div>
                )}
                {hasTotp &&
                  (useRecovery ? (
                    <Field label="Recovery code">
                      <Input
                        value={code}
                        onChange={(e) => setCode(e.target.value)}
                        autoComplete="off"
                        autoCapitalize="off"
                        spellCheck={false}
                        className="font-mono"
                        placeholder="xxxxx-xxxxx"
                        autoFocus
                      />
                    </Field>
                  ) : (
                    <Field label="Verification code">
                      <Input
                        value={code}
                        onChange={(e) => setCode(e.target.value.replace(/[^\d\s]/g, '').slice(0, 8))}
                        inputMode="numeric"
                        autoComplete="one-time-code"
                        className="text-center font-mono text-lg tracking-[0.4em]"
                        placeholder="000000"
                        maxLength={8}
                        autoFocus
                      />
                    </Field>
                  ))}
                {hasTotp && (
                  <button
                    type="button"
                    className="justify-self-start text-sm text-primary underline-offset-4 hover:underline"
                    onClick={() => {
                      setUseRecovery((v) => !v)
                      setCode('')
                      setError(null)
                    }}
                  >
                    {useRecovery ? 'Use an authenticator code instead' : 'Use a recovery code instead'}
                  </button>
                )}
              </>
            )}
            {errorBox}
            {(step === 'password' || hasTotp) && (
              <Button type="submit" size="lg" aria-busy={busy === 'password' || busy === 'code' || undefined} disabled={busyShown && busy === 'passkey'}>
                <Spinner active={busy === 'password' || busy === 'code'} className="size-3.5" label="Signing in" />
                {step === 'mfa' ? 'Verify' : 'Sign in'}
              </Button>
            )}
            {step === 'mfa' && (
              <Button variant="ghost" size="sm" onClick={() => !busy && backToPassword()}>
                <ArrowLeft /> Back <Kbd className="ml-1">Esc</Kbd>
              </Button>
            )}
            {step === 'password' && (passkeys || sso.length > 0) && (
              <>
                <div className="flex items-center gap-3 text-xs text-muted-foreground">
                  <span className="h-px flex-1 bg-border" /> or <span className="h-px flex-1 bg-border" />
                </div>
                <div className="grid gap-2">
                  {passkeys && (
                    <Button variant="secondary" onClick={() => !busy && void passwordless()} disabled={busyShown && busy !== 'passkey'}>
                      <IconOrSpinner icon={Fingerprint} busy={busy === 'passkey'} label="Waiting for the passkey" /> Sign in with a passkey
                    </Button>
                  )}
                  {sso.map((p) => (
                    <Button key={p.id} variant="secondary" onClick={() => !busy && ssoLogin(p.id)} disabled={busyShown}>
                      <LogIn /> Continue with {p.name}
                    </Button>
                  ))}
                </div>
              </>
            )}
          </form>
        )}
      </AuthCard>
    </AuthLayout>
  )
}
