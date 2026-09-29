/*
 * Centered status overlay of a VNC tab: progress (with the backend's live state message: "Waiting for the VNC
 * password", "Negotiating TLS", …), credentials for browser-side (pass-through) authentication, server key approval,
 * the explicit confirmation of a less secure connection (close 4426: weak TLS, unencrypted fallback, clear-text
 * password — never done silently), and disconnected / error states with reconnect, automatic retry countdown and
 * "start a new session".
 */
import { useEffect, useId, useState } from 'react'
import { KeyRound, MonitorX, PlugZap, RefreshCw, ShieldAlert, ShieldQuestion, X } from 'lucide-react'
import { toast } from 'sonner'
import type { Connection } from '@/api/types'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { Spinner } from '@/components/ui/spinner'
import { cn, errorMessage } from '@/lib/utils'
import type { VncController, VncState } from './controller'
import { setConnectionOption } from './policy'

interface OverlayProps {
  c: VncController
  s: VncState
  /** Live backend message for the session (RuntimeSession.stateMessage). */
  sessionMessage?: string
  target: string
  onRestart: () => void
  onCloseTab: () => void
  /** The saved connection, when the user may change its options (offers "remember for this connection"). */
  editableConnection?: Connection
}

export function StatusOverlay({ c, s, sessionMessage, target, onRestart, onCloseTab, editableConnection }: OverlayProps) {
  switch (s.phase) {
    case 'connected':
      return null
    case 'loading':
    case 'connecting':
    case 'reconnecting':
    case 'restarting':
      return (
        <Shell>
          <Spinner className="size-6" label="Connecting" />
          <div className="text-md font-medium">
            {s.phase === 'loading'
              ? 'Loading the VNC viewer…'
              : s.phase === 'restarting'
                ? 'Starting a new session…'
                : s.phase === 'reconnecting'
                  ? `Reconnecting to ${target}…`
                  : `Connecting to ${target}…`}
          </div>
          {sessionMessage && s.phase !== 'loading' && <div className="text-sm text-muted-foreground">{sessionMessage}</div>}
          {s.phase !== 'loading' && s.phase !== 'restarting' && (
            <Button size="sm" variant="ghost" className="mt-1" onClick={() => c.disconnect()}>
              Cancel
            </Button>
          )}
        </Shell>
      )
    case 'credentials':
      return <CredentialsForm c={c} types={s.credentialTypes} />
    case 'confirm':
      return <ConfirmInsecure c={c} s={s} target={target} connection={editableConnection} onCloseTab={onCloseTab} />
    case 'verify':
      return (
        <Shell>
          <ShieldQuestion className="size-7 text-warning" />
          <div className="text-md font-medium">Verify the server&apos;s identity</div>
          <p className="max-w-md text-sm text-muted-foreground">
            The server presented this public key. Only continue if it matches the key shown on the server.
          </p>
          <code className="max-w-md rounded-md border bg-muted/60 px-2 py-1 font-mono text-xs break-all select-all">{s.serverKey}</code>
          <div className="mt-1 flex gap-2">
            <Button size="sm" variant="secondary" onClick={() => c.approveServer(false)}>
              Cancel
            </Button>
            <Button size="sm" onClick={() => c.approveServer(true)}>
              Trust and continue
            </Button>
          </div>
        </Shell>
      )
    case 'disconnected':
    case 'error':
      return <Ended c={c} s={s} onRestart={onRestart} onCloseTab={onCloseTab} />
  }
}

function Shell({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <div className="absolute inset-0 z-10 flex items-center justify-center overflow-auto bg-background/70 p-4 backdrop-blur-[1px]">
      <div
        role="status"
        className={cn('flex max-w-lg flex-col items-center gap-2 rounded-lg border bg-popover/95 px-6 py-5 text-center shadow-popover', className)}
      >
        {children}
      </div>
    </div>
  )
}

function Ended({ c, s, onRestart, onCloseTab }: { c: VncController; s: VncState; onRestart: () => void; onCloseTab: () => void }) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!s.retryAt) return
    const t = setInterval(() => setNow(Date.now()), 250)
    return () => clearInterval(t)
  }, [s.retryAt])
  const secs = s.retryAt ? Math.max(0, Math.ceil((s.retryAt - now) / 1000)) : 0
  const failed = s.phase === 'error'
  const Icon = failed ? MonitorX : PlugZap
  return (
    <Shell>
      <Icon className={cn('size-7', failed ? 'text-destructive' : 'text-muted-foreground')} />
      <div className="text-md font-medium">{failed ? (s.retryAt || s.connectedAt ? 'Connection lost' : 'Could not connect') : 'Disconnected'}</div>
      {s.message && <p className="max-w-md text-sm text-balance text-muted-foreground">{s.message}</p>}
      {s.retryAt && (
        <p className="text-sm">
          Reconnecting in {secs}s{s.attempt > 1 ? ` (attempt ${s.attempt})` : ''}…
        </p>
      )}
      <div className="mt-1 flex flex-wrap justify-center gap-2">
        {s.retryAt ? (
          <>
            <Button size="sm" variant="secondary" onClick={() => c.disconnect()}>
              <X /> Stop
            </Button>
            <Button size="sm" onClick={() => c.reconnect()}>
              <RefreshCw /> Retry now
            </Button>
          </>
        ) : s.sessionGone ? (
          <>
            <Button size="sm" variant="secondary" onClick={onCloseTab}>
              Close tab
            </Button>
            <Button size="sm" onClick={onRestart}>
              <RefreshCw /> Start a new session
            </Button>
          </>
        ) : c.reverse ? (
          <Button size="sm" variant="secondary" onClick={onCloseTab}>
            Close tab
          </Button>
        ) : (
          <>
            <Button size="sm" variant="secondary" onClick={onCloseTab}>
              Close tab
            </Button>
            <Button size="sm" onClick={() => c.reconnect()} autoFocus>
              <RefreshCw /> Reconnect
            </Button>
          </>
        )}
      </div>
    </Shell>
  )
}

/**
 * The server offered more protection than NexTerm can use right now (anonymous TLS failed or only has a weak key
 * exchange, or the only way on sends the password in clear text). Nothing happens without an explicit choice:
 * Cancel is the default action, the weaker options are named for what they are.
 */
function ConfirmInsecure({
  c,
  s,
  target,
  connection,
  onCloseTab,
}: {
  c: VncController
  s: VncState
  target: string
  connection?: Connection
  onCloseTab: () => void
}) {
  const id = useId()
  const [remember, setRemember] = useState(false)
  const [busy, setBusy] = useState(false)
  const info = s.confirm
  const choose = async (level: 'weak' | 'unencrypted') => {
    if (busy) return
    setBusy(true)
    if (remember && connection) {
      const policy = level === 'weak' ? 'allow-weak' : 'allow-unencrypted'
      try {
        await setConnectionOption(connection.id, 'encryption', policy)
        toast.success(`Saved for ${connection.name}`, { description: 'Change it in the connection security menu of the VNC tab.' })
      } catch (err) {
        toast.error('Could not save the choice for this connection', { description: errorMessage(err) })
      }
    }
    setBusy(false)
    c.confirmInsecure(level)
  }
  return (
    <div className="absolute inset-0 z-10 flex items-center justify-center overflow-auto bg-background/80 p-4 backdrop-blur-[1px]">
      <div
        role="alertdialog"
        aria-labelledby={`${id}-title`}
        aria-describedby={`${id}-desc`}
        className="grid w-full max-w-lg gap-3 rounded-lg border border-warning/50 bg-popover px-5 py-4 text-sm shadow-popover"
      >
        <div id={`${id}-title`} className="flex items-center gap-2 text-md font-medium">
          <ShieldAlert className="size-5 shrink-0 text-warning" />
          Less secure connection to {target}
        </div>
        <div id={`${id}-desc`} className="grid gap-2">
          <p>{s.message}</p>
          <p className="text-muted-foreground">
            This may be how the server is configured — or someone on the network interfering to weaken the connection.
            Only continue on a network you trust.
          </p>
        </div>
        {!info ? (
          <div className="flex items-center gap-2 text-muted-foreground">
            <Spinner className="size-4" label="Loading details" /> Loading the options…
          </div>
        ) : (
          <ul className="grid gap-1.5 rounded-md border bg-muted/40 px-3 py-2">
            {info.weakTls && (
              <li>
                <span className="font-medium">Weak encryption:</span> the server&apos;s anonymous TLS uses a {info.dhBits ?? 1024}-bit
                key exchange, which well-resourced attackers can break. Still better than no encryption.
              </li>
            )}
            {info.unencrypted && (
              <li>
                <span className="font-medium">{info.cleartext ? 'Clear-text login:' : 'No encryption:'}</span>{' '}
                {info.cleartext
                  ? 'your user name and password would cross the network readable by anyone on the path, as would the desktop.'
                  : 'the desktop and everything you type would cross the network unencrypted (the VNC password itself is not sent).'}
              </li>
            )}
            {!info.weakTls && !info.unencrypted && <li>The server offers no alternative NexTerm could use.</li>}
          </ul>
        )}
        {connection && (info?.weakTls || info?.unencrypted) && (
          <CheckboxField
            checked={remember}
            onCheckedChange={(v) => setRemember(v === true)}
            label={`Remember for “${connection.name}”`}
            description="Stores the choice in the connection (encryption policy); otherwise it applies to this session only."
          />
        )}
        <div className="flex flex-wrap justify-end gap-2">
          <Button size="sm" variant="secondary" autoFocus onClick={() => c.declineInsecure()}>
            Cancel
          </Button>
          {info?.weakTls && (
            <Button size="sm" variant="outline" loading={busy} onClick={() => void choose('weak')}>
              Use weak encryption
            </Button>
          )}
          {info?.unencrypted && (
            <Button size="sm" variant="destructive" loading={busy} onClick={() => void choose('unencrypted')}>
              {info.cleartext ? 'Send the password in clear text' : 'Connect without encryption'}
            </Button>
          )}
          {info && !info.weakTls && !info.unencrypted && (
            <Button size="sm" variant="ghost" onClick={onCloseTab}>
              Close tab
            </Button>
          )}
        </div>
      </div>
    </div>
  )
}

function CredentialsForm({ c, types }: { c: VncController; types: VncState['credentialTypes'] }) {
  const id = useId()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [target, setTarget] = useState('')
  const needUser = types.includes('username')
  const needTarget = types.includes('target')
  return (
    <div className="absolute inset-0 z-10 flex items-center justify-center overflow-auto bg-background/70 p-4">
      <form
        className="grid w-full max-w-sm gap-3 rounded-lg border bg-popover p-5 shadow-popover"
        onSubmit={(e) => {
          e.preventDefault()
          c.sendCredentials({
            ...(needUser ? { username } : {}),
            password,
            ...(needTarget ? { target } : {}),
          })
        }}
      >
        <div className="flex items-center gap-2 font-medium">
          <KeyRound className="size-4.5 text-primary" />
          Sign in to the remote desktop
        </div>
        <p className="text-sm text-muted-foreground">
          This server uses an authentication method that runs in the browser. The credentials are sent to the server
          once and are not stored.
        </p>
        {needUser && (
          <div className="grid gap-1.5">
            <label htmlFor={`${id}-u`} className="text-sm font-medium">
              Username
            </label>
            <Input id={`${id}-u`} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus />
          </div>
        )}
        <div className="grid gap-1.5">
          <label htmlFor={`${id}-p`} className="text-sm font-medium">
            Password
          </label>
          <PasswordInput
            id={`${id}-p`}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            autoFocus={!needUser}
          />
        </div>
        {needTarget && (
          <div className="grid gap-1.5">
            <label htmlFor={`${id}-t`} className="text-sm font-medium">
              Target
            </label>
            <Input id={`${id}-t`} value={target} onChange={(e) => setTarget(e.target.value)} />
          </div>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={() => c.disconnect()}>
            Cancel
          </Button>
          <Button type="submit" disabled={!password && !needUser}>
            Continue
          </Button>
        </div>
      </form>
    </div>
  )
}
