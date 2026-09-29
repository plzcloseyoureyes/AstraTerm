/*
 * One embedded server on the servers tab: status LED, address, clients, traffic, warnings and the start / stop
 * toggle, plus configuration, activity and overflow actions.
 */
import { memo, useState } from 'react'
import { Copy, ExternalLink, FolderOpen, Logs, MoreHorizontal, RotateCw, ScrollText, Settings2, Terminal, Users } from 'lucide-react'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Switch } from '@/components/ui/switch'
import { Tooltip } from '@/components/ui/tooltip'
import { useNow } from '@/lib/hooks'
import { cn, formatBytes, formatDuration, formatRelativeTime } from '@/lib/utils'
import { useRunMode } from '@/stores/auth'
import { copyWithToast, openSyslogTab, restartServerAction, startServerAction, stopServerAction } from './actions'
import { InfoRow, StateBadge, StatusDot, Warnings } from './components'
import { KINDS, bindLabel, clientCommands, isLoopbackBind, statusTone } from './model'
import { openServerConfig, openServerDrawer } from './store'
import type { ServerStatusEx } from './types'

function shortPath(p: string, home: string | undefined): string {
  if (home && (p === home || p.startsWith(home + '/') || p.startsWith(home + '\\'))) p = '~' + p.slice(home.length)
  return compactPath(p)
}

/** Keeps the informative end of a long path: "/…/projects/share", "C:\\…\\share" (the full path is in the tooltip). */
function compactPath(p: string, max = 30): string {
  if (p.length <= max) return p
  const sep = p.includes('\\') && !p.includes('/') ? '\\' : '/'
  const parts = p.split(sep)
  if (parts.length < 3) return p
  let tail = parts[parts.length - 1]
  for (let i = parts.length - 2; i > 0; i--) {
    const next = parts[i] + sep + tail
    if (parts[0].length + next.length + 3 > max) break
    tail = next
  }
  return `${parts[0]}${sep}…${sep}${tail}`
}

export const ServerCard = memo(function ServerCard({ status, home }: { status: ServerStatusEx; home?: string }) {
  const info = KINDS[status.kind]
  const Icon = info.icon
  const now = useNow(10_000)
  const mode = useRunMode()
  const tone = statusTone(status)
  const busy = status.state === 'starting' || status.state === 'stopping'
  // Instant feedback: the switch moves on click (start) or holds still disabled (stop) until the server answers.
  const [pending, setPending] = useState<'start' | 'stop' | null>(null)
  const on = status.running || status.state === 'starting'
  const cfg = status.config
  const bind = String(cfg.bindAddress ?? '127.0.0.1')
  const port = Number(cfg.port ?? info.defaultPort)
  const root = typeof cfg.root === 'string' ? cfg.root : ''
  const users = Array.isArray(cfg.users) ? (cfg.users as unknown[]).length : 0
  const canBrowse = status.kind === 'http' && status.running && !!status.url && (mode === 'desktop' || !isLoopbackBind(bind))
  const filesCmd = info.files && root && isCommandEnabled('files.openLocal')
  const toggle = () => {
    if (pending) return
    const action = on ? 'stop' : 'start'
    const origin = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setPending(action)
    void (action === 'stop' ? stopServerAction(status.kind) : startServerAction(status.kind)).finally(() => {
      setPending(null)
      // Keyboard users keep their place after the confirmation dialog / toast.
      if (origin?.isConnected && (document.activeElement === document.body || document.activeElement === null)) {
        origin.focus({ preventScroll: true })
      }
    })
  }
  const hasFingerprint =
    status.kind === 'sftp' || (status.kind === 'http' && cfg.tls === true) || (status.kind === 'ftp' && !!cfg.tls && cfg.tls !== 'off')
  const upMs = status.startedAt ? now - Date.parse(status.startedAt) : 0
  const uptime = upMs < 1000 ? 'just started' : `up ${formatDuration(upMs)}`
  const stopsIn = status.stopAt ? formatRelativeTime(status.stopAt, now) : ''
  const commands = clientCommands(status)

  return (
    <article
      aria-labelledby={`server-${status.kind}-title`}
      className={cn(
        'flex min-w-0 flex-col rounded-lg border bg-card text-card-foreground shadow-xs transition-colors',
        tone === 'running' && 'border-success/35',
        tone === 'error' && 'border-destructive/40',
      )}
    >
      <header className="flex items-start gap-3 px-4 pt-3.5 pb-3">
        <div
          className={cn(
            'relative flex size-10 shrink-0 items-center justify-center rounded-lg border bg-muted/50 text-muted-foreground',
            tone === 'running' && 'border-success/30 bg-success/10 text-success',
            tone === 'error' && 'border-destructive/30 bg-destructive/10 text-destructive',
          )}
        >
          <Icon className="size-5" aria-hidden />
          <StatusDot status={status} className="absolute -right-0.5 -bottom-0.5 ring-2 ring-card" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h3 id={`server-${status.kind}-title`} className="truncate text-md font-semibold">
              {info.label}
            </h3>
          </div>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
            <StateBadge status={status} />
            <span className="tabular-nums">
              {info.transport} {port}
            </span>
            {cfg.autoStart === true && <span title="Starts with Termstead">· autostart</span>}
          </div>
        </div>
        {/* The tooltip wraps a span: Radix tooltip triggers set data-state, which would clobber the switch's. */}
        <Tooltip content={status.running ? 'Stop server' : 'Start server'}>
          <span className="mt-1 inline-flex">
            {/* Never `disabled` while busy: a disabled control drops the keyboard focus (e.g. when the stop
                confirmation closes and returns the focus here); busy clicks are ignored instead. */}
            <Switch
              checked={pending === 'start' ? true : on}
              aria-disabled={busy || pending !== null || undefined}
              aria-busy={busy || pending !== null || undefined}
              className={cn((busy || pending !== null) && 'cursor-progress opacity-60')}
              onCheckedChange={() => {
                if (!busy) toggle()
              }}
              aria-label={`${on ? 'Stop' : 'Start'} the ${info.label}`}
            />
          </span>
        </Tooltip>
      </header>

      <dl className="grid gap-1.5 border-t px-4 py-3">
        {status.running && status.url ? (
          <InfoRow label="Address">
            <div className="flex min-w-0 items-center gap-1">
              <span className="truncate font-mono text-sm" title={(status.addrs ?? [status.addr]).join('\n')}>
                {status.url}
              </span>
              <Button variant="ghost" size="icon-xs" aria-label="Copy address" onClick={() => void copyWithToast(status.url ?? '')}>
                <Copy />
              </Button>
              {canBrowse && (
                <Button variant="ghost" size="icon-xs" aria-label="Open in browser" onClick={() => window.open(status.url, '_blank', 'noopener,noreferrer')}>
                  <ExternalLink />
                </Button>
              )}
            </div>
          </InfoRow>
        ) : (
          <InfoRow label="Listens on">
            <span className="truncate">
              <span className="font-mono">{bind}</span>
              <span className="text-muted-foreground"> · {bindLabel(bind)}</span>
            </span>
          </InfoRow>
        )}
        {info.files && (
          <InfoRow label="Folder">
            <div className="flex min-w-0 items-center gap-1">
              <span className="truncate font-mono text-sm" title={root}>
                {root ? shortPath(root, home) : '—'}
              </span>
              {filesCmd && (
                <Button variant="ghost" size="icon-xs" aria-label="Browse the folder" onClick={() => void runCommand('files.openLocal', { path: root })}>
                  <FolderOpen />
                </Button>
              )}
              {cfg.readOnly === true && <span className="shrink-0 text-xs text-muted-foreground">read-only</span>}
            </div>
          </InfoRow>
        )}
        {info.users && (
          <InfoRow label="Access">
            <span className="text-sm">
              {users ? `${users} user${users === 1 ? '' : 's'}` : 'no users'}
              {status.kind === 'ftp' && cfg.anonymous === true && <span className="text-muted-foreground"> · anonymous</span>}
              {status.kind === 'http' && cfg.requireAuth !== true && <span className="text-muted-foreground"> · no login</span>}
              {status.kind === 'sftp' && cfg.shell === true && <span className="text-warning"> · shell</span>}
            </span>
          </InfoRow>
        )}
        {/* Rows keep their place whether the server runs or not: starting / stopping never reflows the grid. */}
        <InfoRow label={status.kind === 'syslog' ? 'Senders' : 'Clients'}>
          {status.running ? (
            <button
              type="button"
              className="inline-flex max-w-full min-w-0 items-center gap-1.5 rounded-sm text-sm outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring/50"
              onClick={() => openServerDrawer(status.kind, 'clients')}
            >
              <Users className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
              <span className="tabular-nums">{status.clients}</span>
              <span className="truncate text-muted-foreground">
                · {uptime}
                {stopsIn && ` · stops ${stopsIn}`}
              </span>
            </button>
          ) : (
            <span className="text-sm text-muted-foreground">Not running</span>
          )}
        </InfoRow>
        <InfoRow label="Traffic">
          {status.running || status.stats.connections > 0 ? (
            <span className="block truncate text-sm tabular-nums">
              <span className="text-muted-foreground">↓</span>
              {formatBytes(status.stats.bytesIn)} <span className="text-muted-foreground">↑</span>
              {formatBytes(status.stats.bytesOut)}
              <span className="text-muted-foreground">
                {' · '}
                {status.kind === 'syslog'
                  ? `${status.stats.messages ?? 0} messages`
                  : `${status.stats.transfers} transfer${status.stats.transfers === 1 ? '' : 's'}`}
                {status.stats.authFailures > 0 && ` · ${status.stats.authFailures} failed logins`}
              </span>
            </span>
          ) : (
            <span className="text-sm text-muted-foreground">—</span>
          )}
        </InfoRow>
        {hasFingerprint && (
          <InfoRow label={status.kind === 'sftp' ? 'Host key' : 'Certificate'}>
            {status.fingerprint && status.running ? (
              <button
                type="button"
                className="block max-w-full truncate rounded-sm text-left font-mono text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
                title={`${status.fingerprint}\n(click to copy)`}
                onClick={() => void copyWithToast(status.fingerprint ?? '', 'Fingerprint copied')}
              >
                {status.fingerprint}
              </button>
            ) : (
              <span className="text-sm text-muted-foreground">Shown while running</span>
            )}
          </InfoRow>
        )}
      </dl>

      {(status.error || status.warnings.length > 0) && (
        <div className="grid gap-2 border-t px-4 py-2.5">
          {status.error && (
            <p role="alert" className="text-sm text-destructive">
              {status.error}
            </p>
          )}
          <Warnings items={status.warnings} />
        </div>
      )}

      <footer className="mt-auto flex items-center gap-1 border-t px-2.5 py-2">
        <Button variant="ghost" size="sm" onClick={() => openServerConfig(status.kind)}>
          <Settings2 /> Configure
        </Button>
        <Button variant="ghost" size="sm" onClick={() => openServerDrawer(status.kind, 'log')}>
          <Logs /> Activity
        </Button>
        {status.kind === 'syslog' && (
          <Button variant="ghost" size="sm" onClick={openSyslogTab}>
            <ScrollText /> Messages
          </Button>
        )}
        <div className="flex-1" />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={`More actions for the ${info.label}`}>
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem disabled={!status.running} onSelect={() => void restartServerAction(status.kind)}>
              <RotateCw /> Restart
            </DropdownMenuItem>
            <DropdownMenuItem disabled={!status.url} onSelect={() => void copyWithToast(status.url ?? '')}>
              <Copy /> Copy address
            </DropdownMenuItem>
            {commands[0] && (
              <DropdownMenuItem onSelect={() => void copyWithToast(commands[0].command, 'Command copied')}>
                <Terminal /> Copy “{commands[0].label}” command
              </DropdownMenuItem>
            )}
            <DropdownMenuItem onSelect={() => openServerDrawer(status.kind, 'connect')}>
              <ExternalLink /> How to connect…
            </DropdownMenuItem>
            {info.files && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuItem disabled={!filesCmd} onSelect={() => void runCommand('files.openLocal', { path: root })}>
                  <FolderOpen /> Browse the shared folder
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </footer>
    </article>
  )
})
