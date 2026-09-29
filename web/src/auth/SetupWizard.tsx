import { useMemo, useState } from 'react'
import { ArrowLeft, ArrowRight, Laptop, ServerCog, ShieldCheck } from 'lucide-react'
import { setup } from '@/api/auth'
import { isApiError } from '@/api/client'
import { Button } from '@/components/ui/button'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput, PasswordStrengthMeter } from '@/components/ui/password-input'
import { estimateStrength } from '@/lib/password'
import { cn, errorMessage } from '@/lib/utils'
import { getSetupToken, refreshAuth, useAuthState } from '@/stores/auth'
import { AuthCard, AuthLayout, BrandMark } from './AuthLayout'
import { useIndicatorSettled } from './useIndicatorSettled'

const USERNAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._@-]{0,63}$/
const MIN_PASSWORD = 8

/** First-run wizard (MU-1): explains the run mode, then creates the first administrator (and signs in). */
export function SetupWizard() {
  const state = useAuthState()
  const mode = state?.mode ?? 'desktop'
  const [step, setStep] = useState<0 | 1>(0)
  const [username, setUsername] = useState('admin')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPw, setConfirmPw] = useState('')
  const [touched, setTouched] = useState<Record<string, boolean>>({})
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Server mode: creating the administrator needs the one-time token from the startup banner. Opening the banner's
  // ?setup= link supplies it; otherwise (or when it was rejected) the operator pastes it.
  const tokenRequired = !!state?.setupTokenRequired
  const [setupTok, setSetupTok] = useState(() => getSetupToken() ?? '')
  const [showTokenField, setShowTokenField] = useState(() => !getSetupToken())
  const [tokenError, setTokenError] = useState<string | null>(null)

  const indicatorSettled = useIndicatorSettled(submitting)

  const strength = useMemo(() => estimateStrength(password, [username, displayName]), [password, username, displayName])
  const errors = {
    username: !username.trim()
      ? 'Username is required'
      : !USERNAME_RE.test(username.trim())
        ? "Use letters, digits, '.', '_', '@' or '-' (max 64)"
        : null,
    password: password.length < MIN_PASSWORD ? `At least ${MIN_PASSWORD} characters` : strength.score < 1 ? 'This password is too easy to guess' : null,
    confirm: confirmPw !== password ? 'Passwords do not match' : null,
    token: tokenRequired && !setupTok.trim() ? 'Paste the setup token from the server output' : tokenError,
  }
  const valid = !errors.username && !errors.password && !errors.confirm && !errors.token

  const submit = async () => {
    setTouched({ username: true, password: true, confirm: true, token: true })
    if (!valid || submitting) return
    setSubmitting(true)
    setError(null)
    try {
      await setup({
        username: username.trim(),
        password,
        displayName: displayName.trim() || undefined,
        setupToken: tokenRequired ? setupTok.trim() : undefined,
      })
      setSubmitting(false)
      // A spinner that did appear keeps its minimum time before the app replaces this screen (and the typed passwords).
      await indicatorSettled()
      await refreshAuth()
    } catch (err) {
      if (isApiError(err) && err.status === 409) {
        setError('Setup was already completed. Reloading…')
        setTimeout(() => void refreshAuth(), 1200)
      } else if (isApiError(err) && err.code === 'setup_token_required') {
        setShowTokenField(true)
        setTokenError('This setup token is not valid. Use the setup link from the server’s startup output.')
        // The server may have started requiring a token after this page loaded: refresh so the field renders.
        if (!tokenRequired) void refreshAuth()
      } else setError(errorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthLayout wide footer={state?.version ? `Termstead ${state.version}` : undefined}>
      <div className="mb-6 flex flex-col items-center gap-3 text-center">
        <BrandMark />
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Welcome to Termstead</h1>
          <p className="text-base text-muted-foreground">Let’s get your workstation ready — it only takes a minute.</p>
        </div>
        <ol className="flex items-center gap-2 text-xs text-muted-foreground" aria-label="Setup progress">
          {['How Termstead runs', 'Administrator account'].map((label, i) => (
            <li key={label} className="flex items-center gap-2">
              <span
                className={cn(
                  'flex size-5 items-center justify-center rounded-full border text-2xs font-semibold',
                  i <= step ? 'border-primary bg-primary text-primary-foreground' : 'bg-muted',
                )}
                aria-current={i === step ? 'step' : undefined}
              >
                {i + 1}
              </span>
              <span className={cn(i === step && 'text-foreground')}>{label}</span>
              {i === 0 && <span className="h-px w-6 bg-border" />}
            </li>
          ))}
        </ol>
      </div>

      <AuthCard>
        {step === 0 ? (
          <div className="grid gap-5">
            <ModeCard mode={mode} />
            <p className="flex items-start gap-2.5 text-sm text-muted-foreground">
              <ShieldCheck className="mt-0.5 size-4 shrink-0 text-success" />
              <span>Next, create the administrator account — it protects your saved sessions, keys and passwords.</span>
            </p>
            <Button size="lg" onClick={() => setStep(1)} autoFocus>
              Continue <ArrowRight />
            </Button>
          </div>
        ) : (
          <form
            className="grid gap-4"
            noValidate
            onSubmit={(e) => {
              e.preventDefault()
              void submit()
            }}
          >
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Username" required error={touched.username ? errors.username : null}>
                <Input
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  onBlur={() => setTouched((t) => ({ ...t, username: true }))}
                  autoComplete="username"
                  autoCapitalize="off"
                  spellCheck={false}
                  autoFocus
                />
              </Field>
              <Field label={<>Display name <span className="font-normal text-muted-foreground">(optional)</span></>}>
                <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} autoComplete="name" placeholder="e.g. Jane Doe" />
              </Field>
            </div>
            <Field label="Password" required error={touched.password ? errors.password : null}>
              <PasswordInput
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                onBlur={() => setTouched((t) => ({ ...t, password: true }))}
                generate={{ length: 20 }}
                onGenerate={(pw) => setConfirmPw(pw)}
                autoComplete="new-password"
              />
            </Field>
            <PasswordStrengthMeter password={password} context={[username, displayName]} className="-mt-2" />
            <Field label="Confirm password" required error={touched.confirm ? errors.confirm : null}>
              <PasswordInput
                value={confirmPw}
                onChange={(e) => setConfirmPw(e.target.value)}
                onBlur={() => setTouched((t) => ({ ...t, confirm: true }))}
                autoComplete="new-password"
              />
            </Field>
            {tokenRequired &&
              (showTokenField ? (
                <Field
                  label="Setup token"
                  required
                  hint="Printed by the server at startup (the ?setup= link) — only whoever runs the server can see it."
                  error={touched.token ? errors.token : null}
                >
                  <Input
                    value={setupTok}
                    onChange={(e) => {
                      setSetupTok(e.target.value)
                      setTokenError(null)
                    }}
                    onBlur={() => setTouched((t) => ({ ...t, token: true }))}
                    className="font-mono"
                    autoComplete="off"
                    autoCapitalize="off"
                    spellCheck={false}
                  />
                </Field>
              ) : (
                <p className="flex items-center gap-2 text-sm text-muted-foreground">
                  <ShieldCheck className="size-4 shrink-0 text-success" />
                  Opened from the server’s one-time setup link.
                </p>
              ))}
            {error && (
              <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {error}
              </p>
            )}
            <div className="flex items-center justify-between gap-2 pt-1">
              <Button variant="ghost" onClick={() => setStep(0)} disabled={submitting}>
                <ArrowLeft /> Back
              </Button>
              <Button type="submit" size="lg" loading={submitting}>
                Create account & sign in
              </Button>
            </div>
          </form>
        )}
      </AuthCard>
    </AuthLayout>
  )
}

const MODES = {
  desktop: {
    icon: Laptop,
    title: 'Desktop mode',
    text: 'Termstead runs on this computer for you alone. It listens on 127.0.0.1 only, and local shells, serial ports and the built-in servers are available. The app opens with a one-time launch link, so you normally won’t need to type your password.',
    other: <>To share Termstead with several people, start it with <Flag>--mode server</Flag>.</>,
  },
  server: {
    icon: ServerCog,
    title: 'Server mode',
    text: 'Termstead is shared by several users through the browser. Everyone signs in with their own account; administrators manage users, shared sessions and security policies. Use TLS when exposing it on a network.',
    other: <>For a single-user workstation on this computer, start it with <Flag>--mode desktop</Flag>.</>,
  },
} as const

function Flag({ children }: { children: string }) {
  return <code className="rounded-sm bg-muted px-1 font-mono text-foreground/80">{children}</code>
}

/** How this server runs (a start-up flag, so informative only) and how to get the other mode. */
function ModeCard({ mode }: { mode: 'desktop' | 'server' }) {
  const m = MODES[mode]
  return (
    <div className="grid gap-3 rounded-lg border border-primary/40 bg-primary/6 p-4">
      <div className="flex items-center gap-3">
        <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/15 text-primary">
          <m.icon className="size-4.5" />
        </div>
        <div className="grid gap-0.5">
          <div className="font-medium">{m.title}</div>
          <div className="text-xs text-muted-foreground">How this server is running</div>
        </div>
      </div>
      <p className="text-sm leading-relaxed text-muted-foreground">{m.text}</p>
      <p className="border-t pt-3 text-xs text-muted-foreground">{m.other}</p>
    </div>
  )
}
