/*
 * Security dialogs (rendered once by the shell's overlay host): confirm-it's-you, TOTP enrollment with QR code,
 * recovery codes (copy / download / print), API token creation (shown once) and adding a passkey. Plus the
 * lock-screen passkey unlock button (SEC-4), a separate overlay that stays mounted while the screen is locked.
 */
import { useEffect, useRef, useState } from 'react'
import { QRCodeSVG } from 'qrcode.react'
import { Download, Fingerprint, KeyRound, Printer, ShieldCheck, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { createToken, totpEnable, totpSetup, verifyPassword } from '@/api/auth'
import { isApiError } from '@/api/client'
import { queryKeys } from '@/api/queryKeys'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingState } from '@/components/ui/query-state'
import { Spinner } from '@/components/ui/spinner'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { refreshAuth, useCurrentUser } from '@/stores/auth'
import { unlockApp, useUIStore } from '@/stores/ui'
import { getMe, secQK, useMe, usePasskeys } from './api'
import { CopyButton, IconOrSpinner, Notice } from './components'
import { closeSecurityDialogs, finishReauth, ReauthCancelled, showRecoveryCodes, useSecurityDialogs, withReauth, withReauthOrThrow } from './store'
import { createPasskey, isPasskeyCancel, passkeyEnvironment, passkeyErrorMessage, verifyWithPasskey } from './webauthn'

export function SecurityOverlay() {
  return (
    <>
      <ReauthDialog />
      <TotpEnrollDialog />
      <RecoveryCodesDialog />
      <CreateTokenDialog />
      <AddPasskeyDialog />
    </>
  )
}

// --- confirm it's you ------------------------------------------------------------------------------------------------------

function ReauthDialog() {
  const reauth = useSecurityDialogs((s) => s.reauth)
  const me = useMe(!!reauth)
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState<'password' | 'passkey' | null>(null)
  const open = !!reauth
  useEffect(() => {
    if (open) {
      setPassword('')
      setError(null)
      setBusy(null)
    }
  }, [open])
  const hasPassword = me.data?.hasPassword ?? true
  const canPasskey = (me.data?.passkeys ?? 0) > 0 && passkeyEnvironment().ok
  // Buttons look disabled only once the wait is visible (a quick check changes nothing on screen); `busy` guards.
  const busyShown = useDelayedFlag(busy !== null)

  const withPassword = async () => {
    if (!password || busy) return
    setBusy('password')
    setError(null)
    try {
      await verifyPassword(password)
      finishReauth(true)
    } catch (err) {
      setError(isApiError(err) && err.code === 'invalid_password' ? 'Incorrect password.' : errorMessage(err))
    } finally {
      setBusy(null)
    }
  }
  const withPasskey = async () => {
    if (busy) return
    setBusy('passkey')
    setError(null)
    try {
      await verifyWithPasskey()
      finishReauth(true)
    } catch (err) {
      if (!isPasskeyCancel(err)) setError(passkeyErrorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !o && finishReauth(false)}>
      <DialogContent size="sm">
        <DialogHeader>
          <DialogTitle>
            <ShieldCheck className="size-4 text-primary" /> Confirm it’s you
          </DialogTitle>
          <DialogDescription>{reauth?.reason ?? 'This change needs a recent sign-in.'} You won’t be asked again for 10 minutes.</DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            void withPassword()
          }}
        >
          <DialogBody className="grid gap-3">
            {hasPassword ? (
              <Field label="Password" error={error}>
                <PasswordInput value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus />
              </Field>
            ) : (
              <>
                <p className="text-sm text-muted-foreground">
                  Your account signs in through single sign-on. {canPasskey ? 'Confirm with a passkey,' : 'Sign out and in again'} to continue.
                </p>
                {error && (
                  <p role="alert" className="text-sm text-destructive">
                    {error}
                  </p>
                )}
              </>
            )}
          </DialogBody>
          <DialogFooter className="flex-wrap">
            {canPasskey && (
              <Button variant="secondary" onClick={() => void withPasskey()} disabled={busyShown && busy !== 'passkey'} className="mr-auto">
                <IconOrSpinner icon={Fingerprint} busy={busy === 'passkey'} label="Waiting for the passkey" /> Use a passkey
              </Button>
            )}
            <Button variant="ghost" onClick={() => finishReauth(false)}>
              Cancel
            </Button>
            {hasPassword && (
              <Button type="submit" disabled={!password || (busyShown && busy !== 'password')} aria-busy={busy === 'password' || undefined}>
                <Spinner active={busy === 'password'} className="size-3.5" label="Checking" />
                Confirm
              </Button>
            )}
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// --- TOTP enrollment ----------------------------------------------------------------------------------------------------------

/** Groups a base32 secret in blocks of four for reading aloud / typing. */
export function formatSecret(s: string): string {
  return s.replace(/(.{4})/g, '$1 ').trim()
}

/**
 * QR + manual secret + first-code verification. `setup` / `enable` default to the signed-in endpoints; the login
 * screen's enrollment step passes the pending-login variants.
 */
export function TotpEnrollForm({
  setup = totpSetup,
  enable = (code: string) => totpEnable(code),
  onDone,
  onCancel,
  submitLabel = 'Enable',
}: {
  setup?: () => Promise<{ secret: string; otpauthUrl: string }>
  enable?: (code: string) => Promise<{ recoveryCodes: string[] }>
  onDone: (recoveryCodes: string[]) => void
  onCancel?: () => void
  submitLabel?: string
}) {
  const [data, setData] = useState<{ secret: string; otpauthUrl: string } | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [code, setCode] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  /** Setup attempts: the secret is created once per attempt (StrictMode's second effect run must not create another). */
  const [attempt, setAttempt] = useState(0)
  const started = useRef(-1)
  useEffect(() => {
    if (started.current === attempt) return
    started.current = attempt
    setup()
      .then(setData)
      .catch((err) => setLoadError(errorMessage(err)))
  }, [setup, attempt])

  const submit = async () => {
    const c = code.replace(/\s+/g, '')
    if (c.length !== 6 || busy) return
    setBusy(true)
    setError(null)
    try {
      const res = await enable(c)
      onDone(res.recoveryCodes)
    } catch (err) {
      setError(isApiError(err) && err.code === 'totp_invalid' ? 'That code did not match. Check the app and your device clock, then try the next code.' : errorMessage(err))
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  if (loadError) {
    return (
      <Notice
        tone="destructive"
        icon={TriangleAlert}
        action={
          <Button
            size="xs"
            variant="outline"
            onClick={() => {
              setLoadError(null)
              setAttempt((a) => a + 1)
            }}
          >
            Try again
          </Button>
        }
      >
        {loadError}
      </Notice>
    )
  }
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <ol className="grid gap-4 text-sm">
        <li className="grid gap-2">
          <span className="font-medium">1. Scan this code with an authenticator app</span>
          <div className="flex flex-wrap items-center gap-4">
            <div className="flex size-[168px] items-center justify-center rounded-lg border bg-white p-2">
              <LoadingState busy={!data}>
                {data && <QRCodeSVG value={data.otpauthUrl} size={150} level="M" aria-label="Authenticator QR code" />}
              </LoadingState>
            </div>
            <div className="grid min-w-0 flex-1 gap-1.5 text-muted-foreground">
              <span>Google Authenticator, 1Password, Bitwarden, Authy, Microsoft Authenticator… all work.</span>
              <span className="text-xs">Can’t scan? Enter this key manually (time-based, 6 digits, 30 s):</span>
              <div className="flex min-w-0 items-center gap-2">
                <code className="min-w-0 truncate rounded bg-muted px-1.5 py-1 font-mono text-sm tracking-wide text-foreground select-all">
                  {data ? formatSecret(data.secret) : '…'}
                </code>
                {data && <CopyButton text={data.secret} size="xs" variant="ghost" label="Copy" />}
              </div>
            </div>
          </div>
        </li>
        <li className="grid gap-2">
          <span className="font-medium">2. Enter the 6-digit code it shows</span>
          <Field error={error}>
            <Input
              value={code}
              onChange={(e) => {
                setError(null)
                const v = e.target.value.replace(/[^\d]/g, '').slice(0, 6)
                setCode(v)
              }}
              inputMode="numeric"
              autoComplete="one-time-code"
              placeholder="000000"
              className="w-44 text-center font-mono text-lg tracking-[0.4em]"
              aria-label="Verification code"
              autoFocus
            />
          </Field>
        </li>
      </ol>
      <div className="flex justify-end gap-2">
        {onCancel && (
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        )}
        <Button type="submit" disabled={code.length !== 6 || !data} loading={busy}>
          {submitLabel}
        </Button>
      </div>
    </form>
  )
}

/** Signed-in TOTP setup: the server wants a recent sign-in, so confirm first when needed (cancel closes the dialog). */
const enrollSetup = async () => {
  try {
    return await withReauthOrThrow(totpSetup, 'Setting up an authenticator app changes how you sign in.')
  } catch (err) {
    if (err instanceof ReauthCancelled) useSecurityDialogs.setState({ totpEnroll: false })
    throw err
  }
}

function TotpEnrollDialog() {
  const open = useSecurityDialogs((s) => s.totpEnroll)
  const qc = useQueryClient()
  const close = () => useSecurityDialogs.setState({ totpEnroll: false })
  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <ShieldCheck className="size-4 text-primary" /> Set up an authenticator app
          </DialogTitle>
          <DialogDescription>After this, signing in with your password also asks for a code from your phone.</DialogDescription>
        </DialogHeader>
        <DialogBody>
          {open && (
            <TotpEnrollForm
              setup={enrollSetup}
              onCancel={close}
              onDone={(codes) => {
                close()
                void refreshAuth()
                void qc.invalidateQueries({ queryKey: secQK.me })
                toast.success('Two-factor authentication is on')
                showRecoveryCodes(codes)
              }}
            />
          )}
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

// --- recovery codes --------------------------------------------------------------------------------------------------------------

export function downloadText(name: string, text: string, type = 'text/plain') {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function recoveryText(codes: string[], username: string): string {
  return [
    `Termstead recovery codes for ${username} (${location.host})`,
    `Generated ${new Date().toLocaleString()}`,
    '',
    'Each code works once. Keep them somewhere safe.',
    '',
    ...codes,
    '',
  ].join('\n')
}

function printCodes(codes: string[], username: string) {
  const w = window.open('', '_blank', 'width=480,height=640')
  if (!w) {
    toast.error('The browser blocked the print window')
    return
  }
  const doc = w.document
  doc.title = 'Termstead recovery codes'
  const pre = doc.createElement('pre')
  pre.style.font = '14px/1.6 ui-monospace, monospace'
  pre.textContent = recoveryText(codes, username)
  doc.body.appendChild(pre)
  w.focus()
  w.print()
}

export function RecoveryCodesList({ codes }: { codes: string[] }) {
  return (
    <ul className="grid grid-cols-2 gap-x-6 gap-y-1.5 rounded-lg border bg-muted/40 px-4 py-3 font-mono text-md tracking-wide">
      {codes.map((c) => (
        <li key={c} className="select-all tabular-nums">
          {c}
        </li>
      ))}
    </ul>
  )
}

/** Copy / download / print; `username` names the file before the signed-in user is known (login enrollment). */
export function RecoveryCodeActions({ codes, username }: { codes: string[]; username?: string }) {
  const user = useCurrentUser()
  const name = username || user?.username || 'user'
  return (
    <div className="flex flex-wrap gap-2">
      <CopyButton text={codes.join('\n')} label="Copy all" />
      <Button variant="secondary" size="sm" onClick={() => downloadText(`termstead-recovery-codes-${name}.txt`, recoveryText(codes, name))}>
        <Download /> Download
      </Button>
      <Button variant="secondary" size="sm" onClick={() => printCodes(codes, name)}>
        <Printer /> Print
      </Button>
    </div>
  )
}

function RecoveryCodesDialog() {
  const rc = useSecurityDialogs((s) => s.recoveryCodes)
  const [acknowledged, setAcknowledged] = useState(false)
  useEffect(() => setAcknowledged(false), [rc])
  const close = () => useSecurityDialogs.setState({ recoveryCodes: null })
  return (
    <Dialog open={!!rc} onOpenChange={(o) => !o && acknowledged && close()}>
      <DialogContent size="md" hideClose onEscapeKeyDown={(e) => !acknowledged && e.preventDefault()} onInteractOutside={(e) => e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>
            <KeyRound className="size-4 text-primary" /> {rc?.title}
          </DialogTitle>
          <DialogDescription>
            If you lose your phone, each of these codes signs you in once instead of an authenticator code. They are shown only now.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-3">
          {rc && <RecoveryCodesList codes={rc.codes} />}
          {rc && <RecoveryCodeActions codes={rc.codes} />}
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="size-3.5 accent-[var(--primary)]" checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} />
            I saved these codes somewhere safe
          </label>
        </DialogBody>
        <DialogFooter>
          <Button onClick={close} disabled={!acknowledged}>
            Done
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// --- API tokens -------------------------------------------------------------------------------------------------------------------

const EXPIRY_OPTIONS = [
  { value: '7', label: '7 days' },
  { value: '30', label: '30 days' },
  { value: '90', label: '90 days' },
  { value: '365', label: '1 year' },
  { value: '0', label: 'No expiry' },
]

function CreateTokenDialog() {
  const open = useSecurityDialogs((s) => s.createToken)
  const created = useSecurityDialogs((s) => s.tokenCreated)
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [days, setDays] = useState('90')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (open) {
      setName('')
      setDays('90')
      setError(null)
    }
  }, [open])
  const close = () => useSecurityDialogs.setState({ createToken: false, tokenCreated: null })

  const submit = async () => {
    const n = name.trim()
    if (!n || busy) return
    setBusy(true)
    setError(null)
    try {
      const t = await withReauth(() => createToken({ name: n, expiresInDays: Number(days) }), 'An API token can act as you until it expires.')
      if (!t) return
      useSecurityDialogs.setState({ tokenCreated: t })
      void qc.invalidateQueries({ queryKey: queryKeys.authTokens })
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()}>
      <DialogContent size="md" onInteractOutside={(e) => created && e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>{created ? 'Copy your new token' : 'New API token'}</DialogTitle>
          <DialogDescription>
            {created
              ? 'This is the only time the token is shown. It acts as you: store it like a password.'
              : 'Tokens let scripts and tools call the Termstead REST API as you (Authorization: Bearer …).'}
          </DialogDescription>
        </DialogHeader>
        {created ? (
          <>
            <DialogBody className="grid gap-3">
              <div className="flex min-w-0 items-center gap-2 rounded-md border bg-muted/40 p-2">
                <code className="min-w-0 flex-1 truncate font-mono text-sm select-all" title={created.token}>
                  {created.token}
                </code>
                <CopyButton text={created.token} />
              </div>
              <p className="text-xs text-muted-foreground">
                Example: <code className="font-mono">curl -H "Authorization: Bearer {created.token.slice(0, 10)}…" {location.origin}/api/auth/state</code>
              </p>
            </DialogBody>
            <DialogFooter>
              <Button onClick={close}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form
            className="flex min-h-0 flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault()
              void submit()
            }}
          >
            <DialogBody className="grid gap-4 @container">
              <Field label="Name" hint="What will use it, e.g. “CI deploy” or “backup script”." error={error}>
                <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={100} autoFocus />
              </Field>
              <Field label="Expires">
                <SimpleSelect value={days} onValueChange={setDays} options={EXPIRY_OPTIONS} className="w-44" aria-label="Expires" />
              </Field>
            </DialogBody>
            <DialogFooter>
              <Button variant="ghost" onClick={close}>
                Cancel
              </Button>
              <Button type="submit" disabled={!name.trim()} loading={busy}>
                Create token
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

// --- add passkey --------------------------------------------------------------------------------------------------------------------

function defaultPasskeyName(): string {
  const ua = navigator.userAgent
  const os = /iPhone|iPad/.test(ua) ? 'iPhone' : /Android/.test(ua) ? 'Android' : /Mac/.test(ua) ? 'Mac' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : ''
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : ''
  return [browser, os].filter(Boolean).join(' on ') || 'Passkey'
}

function AddPasskeyDialog() {
  const open = useSecurityDialogs((s) => s.addPasskey)
  const qc = useQueryClient()
  const passkeys = usePasskeys(open)
  const [name, setName] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (open) {
      setName(defaultPasskeyName())
      setError(null)
    }
  }, [open])
  const env = passkeyEnvironment()
  const close = () => useSecurityDialogs.setState({ addPasskey: false })

  const submit = async () => {
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      const created = await withReauth(() => createPasskey(name.trim()), 'Adding a passkey adds a way to sign in to your account.')
      if (!created) return
      toast.success(`Passkey “${created.name}” added`, { description: created.synced ? 'It syncs across your devices.' : undefined })
      void qc.invalidateQueries({ queryKey: secQK.passkeys })
      void qc.invalidateQueries({ queryKey: secQK.me })
      close()
    } catch (err) {
      if (!isPasskeyCancel(err)) setError(passkeyErrorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const first = (passkeys.data?.length ?? 0) === 0
  return (
    <Dialog open={open} onOpenChange={(o) => !o && !busy && close()}>
      <DialogContent size="md">
        <DialogHeader>
          <DialogTitle>
            <Fingerprint className="size-4 text-primary" /> Add a passkey
          </DialogTitle>
          <DialogDescription>
            Sign in with Touch ID, Windows Hello, your phone or a security key — no password to type or phish.
          </DialogDescription>
        </DialogHeader>
        <form
          className="flex min-h-0 flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <DialogBody className="grid gap-4">
            {!env.ok && <Notice tone="warning" icon={TriangleAlert}>{env.reason}</Notice>}
            <Field label="Name" hint="Helps you recognise it later, e.g. “Work laptop” or “YubiKey”." error={error}>
              <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={64} autoFocus />
            </Field>
            {first && (
              <p className="text-sm text-muted-foreground">
                Once you have a passkey, password sign-ins also ask for it (or your authenticator code) as a second step.
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button variant="ghost" onClick={close} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" disabled={!env.ok} loading={busy}>
              <Fingerprint /> Continue
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// --- lock screen: unlock with a passkey (SEC-4) -------------------------------------------------------------------------------------

/** Unlock the lock screen with a passkey; resolves true when unlocked. Used by the overlay and security.unlockWithPasskey. */
export async function unlockWithPasskey(): Promise<boolean> {
  try {
    await verifyWithPasskey()
    unlockApp()
    return true
  } catch (err) {
    if (!isPasskeyCancel(err)) toast.error('Could not unlock', { description: passkeyErrorMessage(err) })
    return false
  }
}

/**
 * Rendered above the shell's LockScreen while the screen is locked and the user has a passkey for this address:
 * a "Unlock with passkey" pill under the password form.
 */
export function LockPasskeyOverlay({ locked }: { locked: boolean }) {
  const qc = useQueryClient()
  const [available, setAvailable] = useState(false)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!locked) return
    let cancelled = false
    const env = passkeyEnvironment()
    if (!env.ok) return
    qc.fetchQuery({ queryKey: secQK.me, queryFn: getMe, staleTime: 60_000 })
      .then((me) => !cancelled && setAvailable(me.passkeys > 0))
      .catch(() => !cancelled && setAvailable(false))
    return () => {
      cancelled = true
    }
  }, [locked, qc])
  const stillLocked = useUIStore((s) => s.locked)
  if (!locked || !stillLocked || !available) return null
  return (
    <div className="pointer-events-none fixed inset-x-0 top-[min(calc(50%_+_11.5rem),calc(100%_-_3rem))] z-[101] flex justify-center animate-in fade-in-0 duration-200">
      <Button
        variant="secondary"
        className="pointer-events-auto rounded-full px-4 shadow-popover"
        loading={busy}
        onClick={async () => {
          setBusy(true)
          await unlockWithPasskey()
          setBusy(false)
        }}
      >
        <Fingerprint /> Unlock with passkey
      </Button>
    </div>
  )
}

/** Dialogs are dropped (never left half-filled) on sign-out. */
export function resetSecurityDialogs(): void {
  closeSecurityDialogs()
}
