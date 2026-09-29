/*
 * Public share viewer: /share/<token> renders this page instead of the application (no login; see App.tsx). It shows
 * a live terminal session shared by its owner — read-only, or interactive for "write" links — with minimal chrome:
 * title, mode, connection state, the link's remaining time, fit / zoom, copy and fullscreen.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Clock, Copy, Eye, Keyboard, LogIn, Maximize, Minimize, Minus, Pause, Plus, RefreshCw, ScanLine, Unplug } from 'lucide-react'
import { toast } from 'sonner'
import { isApiError, wsUrl } from '@/api/client'
import { BrandMark } from '@/auth/AuthLayout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { IconButton } from '@/components/ui/icon-button'
import { Delayed, Spinner } from '@/components/ui/spinner'
import { StatusDot } from '@/components/ui/status-dot'
import { useFullscreen, toggleFullscreen, useNow } from '@/lib/hooks'
import { errorMessage } from '@/lib/utils'
import { getPublicShare } from './api'
import type { PublicShareInfo } from './types'
import { ViewerTerminal, type ViewerStatus, type ViewerTerminalHandle } from './ViewerTerminal'

type Problem = { kind: 'invalid' | 'login' | 'limited' | 'error'; message: string }

function tokenFromPath(): string {
  const m = /^\/share\/([A-Za-z0-9_-]{16,128})\/?$/.exec(location.pathname)
  return m ? m[1] : ''
}

function remaining(expiresAt: string | undefined, now: number): string {
  if (!expiresAt) return ''
  const ms = Date.parse(expiresAt) - now
  if (ms <= 0) return 'expired'
  const m = Math.round(ms / 60000)
  if (m < 1) return 'less than a minute left'
  if (m < 60) return `${m} min left`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h ${m % 60 ? `${m % 60} min ` : ''}left`
  return `${Math.round(h / 24)} days left`
}

export default function ShareViewer() {
  const token = useMemo(tokenFromPath, [])
  const [info, setInfo] = useState<PublicShareInfo | null>(null)
  const [problem, setProblem] = useState<Problem | null>(token ? null : { kind: 'invalid', message: 'This share link is not valid.' })
  const [status, setStatus] = useState<ViewerStatus | null>(null)
  const [revoked, setRevoked] = useState(false)
  const hadInfo = useRef(false)
  const [fit, setFit] = useState<'fit' | 'actual'>('fit')
  const [font, setFont] = useState(14)
  const term = useRef<ViewerTerminalHandle>(null)
  const fullscreen = useFullscreen()
  const now = useNow(15_000)

  const load = useCallback(async () => {
    if (!token) return
    try {
      const i = await getPublicShare(token)
      setInfo(i)
      setProblem(null)
      setRevoked(false)
      hadInfo.current = true
      document.title = `${i.title} — shared session · AstraTerm`
    } catch (err) {
      if (isApiError(err) && err.status === 401) setProblem({ kind: 'login', message: 'The owner requires viewers to be signed in to AstraTerm.' })
      else if (isApiError(err) && err.status === 404 && hadInfo.current) setRevoked(true) // keep what was seen on screen
      else if (isApiError(err) && err.status === 404) setProblem({ kind: 'invalid', message: 'This share link is invalid, has expired or was revoked.' })
      else if (isApiError(err) && err.status === 429) setProblem({ kind: 'limited', message: 'Too many attempts from this address. Try again in a minute.' })
      else setProblem({ kind: 'error', message: errorMessage(err) })
    }
  }, [token])

  useEffect(() => {
    void load()
  }, [load])

  // Referrers must never carry the token.
  useEffect(() => {
    const meta = document.createElement('meta')
    meta.name = 'referrer'
    meta.content = 'no-referrer'
    document.head.appendChild(meta)
    return () => meta.remove()
  }, [])

  const expired = !!info && Date.parse(info.expiresAt) <= now
  useEffect(() => {
    if (revoked || expired) term.current?.stop()
  }, [revoked, expired])
  // 4410: the server ended the stream because the link was revoked or expired.
  const linkEnded = status?.closeCode === 4410
  const ended = status?.ended || status?.state === 'closed' || revoked || linkEnded
  const interactive = info?.mode === 'write'
  // Interactive links: the owner may pause guest input (the server then reports the view as read-only).
  const inputPaused = interactive && !!status && status.transport === 'open' && status.readOnly
  const connecting = !status || status.transport === 'connecting' || status.transport === 'reconnecting'

  const onConnectFailed = useCallback(
    (attempt: number) => {
      // The socket was refused before it opened: the link may be gone. Re-check (quietly) every few attempts.
      if (attempt === 1 || attempt % 3 === 0) void load()
    },
    [load],
  )

  const url = useCallback((offset: number) => wsUrl(`/ws/share/${encodeURIComponent(token)}`, { offset }), [token])

  if (problem) return <ProblemPage problem={problem} onRetry={() => void load()} />
  if (!info) return <div className="flex h-full items-center justify-center bg-background" />

  const stateLabel = ended
    ? 'Ended'
    : status?.transport === 'open'
      ? status.state === 'connected'
        ? 'Live'
        : (status.state ?? 'Live')
      : status?.transport === 'reconnecting'
        ? 'Reconnecting'
        : 'Connecting'
  const live = !ended && status?.transport === 'open' && status.state === 'connected'

  return (
    <div className="flex h-full flex-col bg-background text-foreground">
      <header className="flex h-10 shrink-0 items-center gap-2 border-b bg-toolbar px-3">
        <BrandMark className="size-5" />
        <div className="flex min-w-0 items-baseline gap-2">
          <h1 className="truncate text-base font-semibold">{info.title}</h1>
          {info.label && <span className="truncate text-sm text-muted-foreground">{info.label}</span>}
        </div>
        <Badge variant={interactive && !inputPaused ? 'warning' : 'secondary'} className="ml-1">
          {inputPaused ? <Pause /> : interactive ? <Keyboard /> : <Eye />}
          {inputPaused ? 'Input paused by the owner' : interactive ? 'Interactive' : 'View only'}
        </Badge>
        <span className="flex items-center gap-1.5 text-sm text-muted-foreground" aria-live="polite">
          <StatusDot tone={live ? 'success' : ended ? 'muted' : 'warning'} pending={!live && !ended} className="size-2" />
          {stateLabel}
        </span>
        <div className="flex-1" />
        {!expired && !ended && (
          <span className="hidden items-center gap-1 text-sm text-muted-foreground tabular-nums sm:flex">
            <Clock className="size-3.5" />
            {remaining(info.expiresAt, now)}
          </span>
        )}
        <div className="ml-2 flex items-center gap-0.5">
          <IconButton
            icon={ScanLine}
            label={fit === 'fit' ? 'Actual size' : 'Fit to window'}
            active={fit === 'fit'}
            onClick={() => setFit((f) => (f === 'fit' ? 'actual' : 'fit'))}
          />
          <IconButton icon={Minus} label="Smaller text" disabled={font <= 8} onClick={() => setFont((f) => Math.max(8, f - 1))} />
          <IconButton icon={Plus} label="Larger text" disabled={font >= 28} onClick={() => setFont((f) => Math.min(28, f + 1))} />
          <IconButton
            icon={Copy}
            label="Copy selection"
            onClick={async () => {
              if (await term.current?.copySelection()) toast.success('Copied')
              else toast.message('Select text in the terminal first')
            }}
          />
          <IconButton icon={fullscreen ? Minimize : Maximize} label={fullscreen ? 'Exit fullscreen' : 'Fullscreen'} onClick={() => void toggleFullscreen()} />
        </div>
      </header>
      <main className="relative min-h-0 flex-1">
        {!expired && (
          <ViewerTerminal
            ref={term}
            url={url}
            interactive={interactive}
            fit={fit}
            fontSize={font}
            onStatus={setStatus}
            onConnectFailed={onConnectFailed}
            className="absolute inset-0 overflow-auto"
          />
        )}
        <Delayed active={connecting && !ended && !expired}>
          <div className="pointer-events-none absolute top-3 left-1/2 flex -translate-x-1/2 items-center gap-2 rounded-md border bg-popover/95 px-3 py-1.5 text-sm shadow-popover animate-in fade-in-0">
            <Spinner className="size-3.5" />
            {status?.transport === 'reconnecting' ? 'Reconnecting…' : 'Connecting…'}
          </div>
        </Delayed>
        {(ended || expired) && (
          <div className="absolute inset-0 flex items-center justify-center bg-background/70 backdrop-blur-[2px] animate-in fade-in-0 duration-200">
            <div className="flex max-w-sm flex-col items-center gap-2 rounded-lg border bg-popover px-6 py-5 text-center shadow-popover">
              <Unplug className="size-5 text-muted-foreground" />
              <div className="text-md font-medium">
                {expired || (linkEnded && /expired/i.test(status?.closeReason ?? ''))
                  ? 'This share link has expired'
                  : revoked || linkEnded
                    ? 'The owner stopped sharing'
                    : 'The shared session has ended'}
              </div>
              <p className="text-sm text-muted-foreground">
                {expired || revoked || linkEnded
                  ? 'Ask the owner for a new link if you still need access. What you saw stays on screen until you leave.'
                  : 'The owner closed the session or stopped sharing it. What you saw stays on screen until you leave.'}
              </p>
              {!expired && !revoked && !linkEnded && (
                <Button size="sm" variant="secondary" onClick={() => void load().then(() => term.current?.reconnect())}>
                  <RefreshCw /> Try again
                </Button>
              )}
            </div>
          </div>
        )}
      </main>
    </div>
  )
}

function ProblemPage({ problem, onRetry }: { problem: Problem; onRetry: () => void }) {
  return (
    <div className="flex h-full items-center justify-center bg-background p-6 text-foreground">
      <div className="flex max-w-sm flex-col items-center gap-3 text-center">
        <BrandMark className="size-9" />
        <h1 className="text-lg font-semibold">
          {problem.kind === 'login' ? 'Sign-in required' : problem.kind === 'invalid' ? 'Link unavailable' : 'Cannot open the shared session'}
        </h1>
        <p className="text-sm text-muted-foreground">{problem.message}</p>
        <div className="mt-1 flex gap-2">
          {problem.kind === 'login' && (
            <Button asChild>
              <a href="/" target="_blank" rel="noopener noreferrer">
                <LogIn /> Sign in to AstraTerm
              </a>
            </Button>
          )}
          {problem.kind !== 'invalid' && (
            <Button variant="secondary" onClick={onRetry}>
              <RefreshCw /> Try again
            </Button>
          )}
        </div>
        {problem.kind === 'login' && <p className="text-xs text-muted-foreground">After signing in, come back to this tab and try again.</p>}
      </div>
    </div>
  )
}
