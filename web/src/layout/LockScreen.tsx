import { useEffect, useRef, useState } from 'react'
import { Lock, LogOut } from 'lucide-react'
import { isApiError } from '@/api/client'
import { verifyPassword } from '@/api/auth'
import { Button } from '@/components/ui/button'
import { PasswordInput } from '@/components/ui/password-input'
import { errorMessage } from '@/lib/utils'
import { logout } from '@/app/session'
import { useCurrentUser } from '@/stores/auth'
import { securitySettings } from '@/stores/settings'
import { lockApp, unlockApp, useUIStore } from '@/stores/ui'

const ACTIVITY_EVENTS = ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart'] as const

/** Idle auto-lock (SEC-5): locks the UI after settings.security.autoLockMinutes without input (0 = off). */
export function useAutoLock(): void {
  const minutes = securitySettings.useValue('autoLockMinutes')
  const locked = useUIStore((s) => s.locked)
  const last = useRef(0)

  useEffect(() => {
    if (!minutes || minutes <= 0 || locked) return
    last.current = Date.now()
    let throttled = 0
    const onActivity = () => {
      const now = Date.now()
      if (now - throttled < 1000) return
      throttled = now
      last.current = now
    }
    for (const ev of ACTIVITY_EVENTS) window.addEventListener(ev, onActivity, { passive: true, capture: true })
    const timer = setInterval(() => {
      if (Date.now() - last.current >= minutes * 60_000) lockApp()
    }, 10_000)
    return () => {
      for (const ev of ACTIVITY_EVENTS) window.removeEventListener(ev, onActivity, { capture: true })
      clearInterval(timer)
    }
  }, [minutes, locked])
}

/** Full-screen lock overlay; sessions keep running underneath. Unlock re-verifies the account password. */
export function LockScreen() {
  const locked = useUIStore((s) => s.locked)
  const user = useCurrentUser()
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [cooldown, setCooldown] = useState(false)
  const failures = useRef(0)

  useEffect(() => {
    if (!locked) {
      setPassword('')
      setError(null)
      failures.current = 0
    }
  }, [locked])

  if (!locked) return null

  const submit = async () => {
    if (!password || busy || cooldown) return
    setBusy(true)
    setError(null)
    try {
      await verifyPassword(password)
      setPassword('')
      unlockApp()
    } catch (err) {
      failures.current++
      if (isApiError(err) && err.status === 429) setError(err.message || 'Too many attempts. Try again later.')
      else if (isApiError(err) && (err.code === 'invalid_password' || err.status === 401 || err.status === 403)) setError('Incorrect password.')
      else if (isApiError(err) && err.status === 404) setError('This server cannot verify passwords. Sign out to continue.')
      else setError(errorMessage(err))
      // Slow down guessing: growing delay after repeated failures.
      if (failures.current >= 3) {
        setCooldown(true)
        setTimeout(() => setCooldown(false), Math.min(30, 2 ** (failures.current - 2)) * 1000)
      }
    } finally {
      setBusy(false)
    }
  }

  const name = user?.displayName || user?.username || 'NexTerm'
  const initial = name.trim().charAt(0).toUpperCase()

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Screen locked"
      className="fixed inset-0 z-[100] flex items-center justify-center bg-background/55 backdrop-blur-2xl animate-in fade-in-0 duration-200"
    >
      <form
        className="flex w-[min(92vw,340px)] flex-col items-center gap-4 rounded-xl border bg-popover/90 p-6 text-center shadow-popover"
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <div className="relative">
          <div className="flex size-14 items-center justify-center rounded-full bg-primary/15 text-xl font-semibold text-primary">{initial}</div>
          <span className="absolute -right-1 -bottom-1 flex size-6 items-center justify-center rounded-full border bg-popover">
            <Lock className="size-3.5 text-muted-foreground" />
          </span>
        </div>
        <div className="grid gap-0.5">
          <div className="text-md font-semibold">{name}</div>
          <div className="text-sm text-muted-foreground">NexTerm is locked. Sessions keep running.</div>
        </div>
        <div className="grid w-full gap-1.5 text-left">
          <PasswordInput
            value={password}
            onChange={(e) => {
              setPassword(e.target.value)
              setError(null)
            }}
            placeholder="Password"
            aria-label="Password"
            aria-invalid={!!error || undefined}
            autoComplete="current-password"
            autoFocus
          />
          <p role="alert" className="min-h-4 text-sm text-destructive">
            {error}
          </p>
        </div>
        <Button type="submit" className="w-full" loading={busy} disabled={!password || cooldown}>
          {cooldown ? 'Please wait…' : 'Unlock'}
        </Button>
        <Button
          variant="link"
          size="sm"
          className="text-muted-foreground"
          onClick={async () => {
            unlockApp()
            await logout()
          }}
        >
          <LogOut className="size-3.5" /> Sign out
        </Button>
      </form>
    </div>
  )
}
