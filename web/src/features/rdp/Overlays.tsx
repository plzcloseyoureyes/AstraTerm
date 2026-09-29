/*
 * RDP viewport overlays: connecting card (with the backend's step: certificate check, sign-in, …), the end / error
 * panel with contextual actions (R = reconnect, Q = close), virtual drive transfers and the remote-clipboard badge.
 */
import { useEffect, useRef, type ReactNode } from 'react'
import { ArrowDownToLine, ArrowUpFromLine, CircleAlert, ClipboardCheck, KeyRound, Lock, Monitor, PlugZap, RefreshCw, SearchX, Settings2, X } from 'lucide-react'
import { toast } from 'sonner'
import type { RuntimeSession } from '@/api/types'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { Kbd } from '@/components/ui/kbd'
import { Spinner } from '@/components/ui/spinner'
import { restartSessionInTab } from '@/features/terminal/open'
import { useNow } from '@/lib/hooks'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage, formatBytes } from '@/lib/utils'
import { useIsAdmin, useServerFeatures } from '@/stores/auth'
import { requestVaultUnlock } from '@/stores/ui'
import { closeTab } from '@/stores/workspace'
import { STATUS_LABEL } from './Toolbar'
import type { RdpController, RdpTabParams, ViewerState } from './types'

interface OverlayProps {
  tabId: string
  params: RdpTabParams
  ctrl: RdpController | null
  viewer: ViewerState | undefined
  session: RuntimeSession | undefined
}

function Card({ children, className, role = 'status' }: { children: ReactNode; className?: string; role?: string }) {
  return (
    <div role={role} className={cn('pointer-events-auto rounded-lg border bg-popover px-4 py-3 text-popover-foreground shadow-popover', className)}>
      {children}
    </div>
  )
}

export function RdpOverlays({ tabId, params, ctrl, viewer, session }: OverlayProps) {
  if (!viewer) return null
  const connecting = viewer.status === 'idle' || viewer.status === 'loading' || viewer.status === 'connecting' || viewer.status === 'authenticating'
  const ended = viewer.status === 'error' || viewer.status === 'disconnected' || viewer.status === 'gone'
  const running = viewer.transfers.filter((t) => t.state === 'running')
  return (
    <>
      <ConnectingCard show={connecting} viewer={viewer} session={session} />
      {ended && <EndedPanel tabId={tabId} params={params} ctrl={ctrl} viewer={viewer} />}
      <div className="pointer-events-none absolute top-2 right-3 z-20 flex flex-col items-end gap-1.5" aria-live="polite">
        {viewer.status === 'connected' && viewer.remoteClipboardPending && (
          <Badge>
            <ClipboardCheck className="size-3.5 text-primary" /> Remote clipboard changed
            <button
              type="button"
              className="ml-1 rounded-sm px-1 font-medium underline-offset-2 hover:underline"
              onClick={() =>
                void ctrl?.copyRemoteClipboard().then(
                  () => toast.success('Copied to the local clipboard'),
                  (err) => toast.error('Could not copy', { description: errorMessage(err) }),
                )
              }
            >
              Copy
            </button>
          </Badge>
        )}
        {running.map((t) => (
          <Badge key={t.id}>
            {t.direction === 'upload' ? <ArrowUpFromLine className="size-3.5" /> : <ArrowDownToLine className="size-3.5" />}
            <span className="max-w-48 truncate">{t.name}</span>
            <span className="font-mono text-xs text-muted-foreground tabular-nums">
              {t.size > 0 ? `${Math.min(100, Math.round((t.done / t.size) * 100))}%` : formatBytes(t.done)}
            </span>
          </Badge>
        ))}
      </div>
    </>
  )
}

function Badge({ children }: { children: ReactNode }) {
  return (
    <div className="pointer-events-auto flex h-7 items-center gap-1.5 rounded-md border bg-popover/90 px-2 text-sm text-popover-foreground shadow-popover backdrop-blur">
      {children}
    </div>
  )
}

function ConnectingCard({ show, viewer, session }: { show: boolean; viewer: ViewerState; session: RuntimeSession | undefined }) {
  // Quick (re)connections never flash the card (docs/UX.md): it appears after 300 ms and then stays ≥ 400 ms.
  const visible = useDelayedFlag(show)
  if (!visible) return null
  // The backend knows the finer steps (certificate prompt, sign-in, gateway) while the viewer waits.
  const serverStep =
    session && (session.state === 'connecting' || session.state === 'authenticating') && session.stateMessage ? session.stateMessage : undefined
  const authenticating = viewer.status === 'authenticating' || session?.state === 'authenticating'
  const title = viewer.status === 'loading' ? 'Starting the remote desktop…' : authenticating ? 'Signing in…' : 'Connecting…'
  const detail = serverStep ?? viewer.message
  return (
    <div className="pointer-events-none absolute inset-0 z-20 flex items-center justify-center p-4">
      <Card className="flex max-w-sm flex-col items-center gap-2 text-center">
        <div className="flex items-center gap-2 text-base font-medium">
          <Spinner immediate className="size-4" label={title} />
          {title}
        </div>
        {detail && <div className="text-sm break-words text-muted-foreground">{detail}</div>}
        {session?.state === 'authenticating' && /confirm|credential|waiting/i.test(session.stateMessage ?? '') && (
          <div className="text-xs text-muted-foreground">Answer the prompt to continue.</div>
        )}
      </Card>
    </div>
  )
}

function EndedPanel({ tabId, params, ctrl, viewer }: { tabId: string; params: RdpTabParams; ctrl: RdpController | null; viewer: ViewerState }) {
  const ref = useRef<HTMLDivElement>(null)
  const features = useServerFeatures()
  const isAdmin = useIsAdmin()
  const gone = viewer.status === 'gone'
  const error = viewer.status === 'error'
  const title = gone ? 'This session no longer exists' : error ? STATUS_LABEL.error : 'Remote desktop disconnected'
  const Icon = gone ? SearchX : error ? CircleAlert : PlugZap

  const reconnect = () => {
    if (gone) void restartSessionInTab(tabId)
    else ctrl?.reconnect()
  }
  const close = () => void closeTab(tabId, { force: true })

  // R / Q shortcuts while the (now empty) desktop has focus.
  useEffect(() => {
    const vp = ref.current?.closest<HTMLElement>('[data-rdp-viewport]')
    if (!vp) return
    const onKey = (e: KeyboardEvent) => {
      if (e.ctrlKey || e.altKey || e.metaKey || e.target !== vp) return
      if (e.key === 'r' || e.key === 'R') {
        e.preventDefault()
        reconnect()
      } else if (e.key === 'q' || e.key === 'Q') {
        e.preventDefault()
        close()
      }
    }
    vp.addEventListener('keydown', onKey)
    return () => vp.removeEventListener('keydown', onKey)
  })

  const actions: ReactNode[] = []
  if (viewer.reconnectAt) {
    actions.push(
      <Button key="cancel-auto" size="sm" variant="secondary" onClick={() => ctrl?.cancelAutoReconnect()}>
        Stop reconnecting
      </Button>,
    )
  }
  if (viewer.errorCode === 'guacd_unavailable') {
    actions.push(
      <Button key="iron" size="sm" variant="secondary" onClick={() => ctrl?.reconnect({ engine: 'ironrdp' })}>
        <Monitor /> Use IronRDP
      </Button>,
    )
    if (isAdmin)
      actions.push(
        <Button key="cfg" size="sm" variant="ghost" onClick={() => void runCommand('settings.open', { section: 'rdp' })}>
          <Settings2 /> guacd settings
        </Button>,
      )
  }
  if (viewer.errorCode === 'unsupported_security' && features?.guacd) {
    actions.push(
      <Button key="guacd" size="sm" variant="secondary" onClick={() => ctrl?.reconnect({ engine: 'guacd' })}>
        <Monitor /> Use guacd
      </Button>,
    )
  }
  if (viewer.errorCode === 'locked') {
    actions.push(
      <Button key="unlock" size="sm" variant="secondary" onClick={() => void requestVaultUnlock().then((ok) => ok && ctrl?.reconnect())}>
        <Lock /> Unlock the vault
      </Button>,
    )
  }
  if ((viewer.authFailed || viewer.errorCode === 'credentials') && params.connectionId && !params.shadow && isCommandEnabled('sessions.edit')) {
    actions.push(
      <Button key="edit" size="sm" variant="secondary" onClick={() => void runCommand('sessions.edit', { id: params.connectionId })}>
        <KeyRound /> Edit credentials…
      </Button>,
    )
  }

  return (
    <div ref={ref} className="absolute inset-x-0 bottom-0 z-20 flex justify-center bg-gradient-to-t from-black/40 to-transparent px-3 pt-10 pb-4">
      <Card role="alertdialog" className="flex w-full max-w-lg flex-col gap-3">
        <div className="flex items-start gap-3">
          <div
            className={cn(
              'mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-md border',
              error ? 'bg-destructive/10 text-destructive' : 'bg-muted text-muted-foreground',
            )}
          >
            <Icon className="size-4" />
          </div>
          <div className="grid min-w-0 gap-0.5">
            <div className="text-base font-medium">{title}</div>
            {viewer.message && <div className="text-sm break-words text-muted-foreground">{viewer.message}</div>}
            {viewer.reconnectAt && <AutoReconnect at={viewer.reconnectAt} attempt={viewer.reconnectAttempt ?? 1} />}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <Button size="sm" onClick={reconnect} disabled={!ctrl && !gone}>
            <RefreshCw /> {gone ? 'Start new session' : 'Reconnect'}
            <Kbd className="ml-1 border-primary-foreground/30 bg-primary-foreground/15 text-primary-foreground">R</Kbd>
          </Button>
          {actions}
          <Button size="sm" variant="ghost" className="ml-auto" onClick={close}>
            <X /> Close <Kbd className="ml-1">Q</Kbd>
          </Button>
        </div>
      </Card>
    </div>
  )
}

function AutoReconnect({ at, attempt }: { at: number; attempt: number }) {
  const now = useNow(500)
  const secs = Math.max(0, Math.ceil((at - now) / 1000))
  return (
    <div className="flex items-center gap-1.5 text-sm text-warning" aria-live="polite">
      <Spinner className="size-3.5 text-warning" /> Reconnecting {secs > 0 ? `in ${secs}s` : 'now'} (attempt {attempt})
    </div>
  )
}
