import { lazy, Suspense, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { RotateCcw, ServerOff } from 'lucide-react'
import { queryClient } from '@/api/queryClient'
import { Button } from '@/components/ui/button'
import { Spinner } from '@/components/ui/spinner'
import { installEventHandlers } from '@/lib/eventHandlers'
import { events } from '@/lib/events'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { bootstrapAuth, retryAuth, useAuthStore, type AuthStatus } from '@/stores/auth'
import { usePromptsStore } from '@/stores/prompts'
import { loadSettings, resetSettingsStore, useSettingsStore } from '@/stores/settings'
import { closePalette, unlockApp } from '@/stores/ui'
import { AuthLayout } from './AuthLayout'
import { Login } from './Login'

const SetupWizard = lazy(() => import('./SetupWizard').then((m) => ({ default: m.SetupWizard })))
// Fetched in parallel with the auth-state request (see AuthGate), so a signed-in reload does not wait for it after.
const loadAppShell = () => import('@/layout/AppShell')
const AppShell = lazy(() => loadAppShell().then((m) => ({ default: m.AppShell })))

/**
 * Start-up splash (docs/UX.md "Loading states"): the same markup as the static splash in index.html (.nx-splash), so
 * React's first render changes nothing on screen. The mark is still; the spinner + label appear only when starting
 * takes > 300 ms, and then the splash stays until the spinner has been readable for its minimum time (so it never
 * blinks). It stays mounted — one instance for every boot phase (auth state, settings, the AppShell chunk) — and fades
 * out once (150 ms) over the first real screen, the spinner (if any) fading with it.
 */
function Splash({ visible, label }: { visible: boolean; label: string }) {
  const spin = useDelayedFlag(visible)
  const shown = visible || spin
  const [mounted, setMounted] = useState(shown)
  const [spinning, setSpinning] = useState(false)
  useEffect(() => {
    if (spin) setSpinning(true)
  }, [spin])
  useEffect(() => {
    if (shown) {
      setMounted(true)
      return
    }
    const t = setTimeout(() => {
      setMounted(false)
      setSpinning(false)
    }, 160)
    return () => clearTimeout(t)
  }, [shown])
  if (!mounted && !shown) return null
  return (
    <div className="nx-splash" data-leaving={shown ? undefined : ''} aria-busy={visible || undefined}>
      <svg viewBox="0 0 64 64" className="nx-splash-mark" aria-hidden>
        <rect width="64" height="64" rx="14" fill="var(--primary)" />
        <path d="M16 22l12 10-12 10" stroke="var(--primary-foreground)" strokeWidth="5" fill="none" strokeLinecap="round" strokeLinejoin="round" />
        <path d="M32 44h16" stroke="var(--primary-foreground)" strokeWidth="5" strokeLinecap="round" />
      </svg>
      <div className="nx-splash-status" aria-live="polite">
        {spinning && (
          <>
            <Spinner immediate className="size-3.5" />
            {label}
          </>
        )}
      </div>
    </div>
  )
}

/** Suspense fallback that only reports "still loading" (the Splash above shows it). */
function Pending({ onChange }: { onChange: (pending: boolean) => void }) {
  useLayoutEffect(() => {
    onChange(true)
    return () => onChange(false)
  }, [onChange])
  return null
}

function ServerUnreachable() {
  const error = useAuthStore((s) => s.error)
  return (
    <AuthLayout>
      <div className="flex flex-col items-center gap-4 text-center">
        <div className="flex size-12 items-center justify-center rounded-xl border bg-destructive/10 text-destructive">
          <ServerOff className="size-5" />
        </div>
        <div className="grid gap-1">
          <h1 className="text-lg font-semibold">Cannot reach the NexTerm server</h1>
          <p className="text-sm text-muted-foreground">{error ?? 'The server did not respond.'}</p>
        </div>
        <Button onClick={() => void retryAuth()}>
          <RotateCcw /> Try again
        </Button>
      </div>
    </AuthLayout>
  )
}

/** Session lifecycle: start/stop the events socket, load settings, and wipe per-user state on sign-out. */
function useSessionLifecycle(status: AuthStatus) {
  const prev = useRef<AuthStatus>(status)
  useEffect(() => {
    const was = prev.current
    prev.current = status
    if (status === 'authenticated' && was !== 'authenticated') {
      installEventHandlers()
      events.start()
      void loadSettings()
    } else if (status !== 'authenticated' && was === 'authenticated') {
      events.stop()
      closePalette()
      unlockApp()
      usePromptsStore.setState({ queue: [] })
      resetSettingsStore()
      queryClient.clear()
    }
    // A lock screen only makes sense for a signed-in session.
    if (status === 'login' || status === 'setup') unlockApp()
  }, [status])
}

/** Chooses between splash, setup wizard, login and the application shell. */
export function AuthGate() {
  const status = useAuthStore((s) => s.status)
  const settingsLoaded = useSettingsStore((s) => s.loaded)

  const [chunkPending, setChunkPending] = useState(false)

  useEffect(() => {
    void bootstrapAuth()
    void loadAppShell().catch(() => undefined) // preload; a failure surfaces when AppShell renders
  }, [])
  useSessionLifecycle(status)

  let content: ReactNode = null
  switch (status) {
    case 'error':
      content = <ServerUnreachable />
      break
    case 'setup':
      content = (
        <Suspense fallback={<Pending onChange={setChunkPending} />}>
          <SetupWizard />
        </Suspense>
      )
      break
    case 'login':
      content = <Login />
      break
    case 'authenticated':
      if (settingsLoaded) {
        content = (
          <Suspense fallback={<Pending onChange={setChunkPending} />}>
            <AppShell />
          </Suspense>
        )
      }
      break
  }
  const booting = status === 'loading' || (status === 'authenticated' && !settingsLoaded) || chunkPending
  return (
    <>
      {content}
      <Splash visible={booting} label={status === 'authenticated' ? 'Loading your workspace…' : 'Starting NexTerm…'} />
    </>
  )
}
