/*
 * System information: run mode, version, data directory, listener / TLS, database, users and sessions, vault and
 * optional integrations (guacd, docker, kubectl…). Refreshes in the background while visible.
 */
import type { ReactNode } from 'react'
import { Cpu, Database, HardDrive, Lock, LockOpen, Network, Puzzle, Server, ShieldAlert, ShieldCheck, Users } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { ErrorState } from '@/components/ui/query-state'
import { LoadingPane } from '@/components/ui/spinner'
import { DELAY_PRESETS, useLoadingGate } from '@/lib/useDelayedFlag'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/utils'
import { useGuacd, useSystem } from '@/features/security/api'
import { CopyButton, PageHeader } from '@/features/security/components'

function Card({ icon: Icon, title, children }: { icon: typeof Server; title: string; children: ReactNode }) {
  return (
    <section className="grid content-start gap-3 rounded-lg border bg-card p-4">
      <h2 className="flex items-center gap-2 text-base font-semibold">
        <Icon className="size-4 text-muted-foreground" /> {title}
      </h2>
      {/* One label width for every card: the values line up across the grid. */}
      <dl className="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-1.5 text-sm">{children}</dl>
    </section>
  )
}

function Item({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="truncate text-muted-foreground">{label}</dt>
      <dd className="min-w-0 truncate tabular-nums">{children}</dd>
    </>
  )
}

const FEATURE_LABELS: Record<string, string> = {
  guacd: 'guacd (RDP gateway)',
  docker: 'Docker',
  kubectl: 'kubectl',
  mosh: 'Mosh',
  wsl: 'WSL',
  caffeine: 'Keep awake (caffeine)',
  sshAgent: 'Built-in SSH agent',
}

export function SystemPanel({ visible }: { visible: boolean }) {
  const sys = useSystem(visible)
  const guacd = useGuacd(visible)
  const gate = useLoadingGate(sys.isPending, DELAY_PRESETS.NAVIGATION)
  if (gate.hold || sys.isPending) return <LoadingPane active={gate.show} immediate />
  if (sys.isError) return <ErrorState error={sys.error} title="Could not load the system information" onRetry={() => void sys.refetch()} />
  const s = sys.data
  const exposed = !s.listen.startsWith('127.') && !s.listen.startsWith('localhost') && !s.listen.startsWith('[::1]')
  return (
    <>
      <PageHeader title="System" description={`NexTerm ${s.version} · ${s.mode === 'server' ? 'server mode (multi-user)' : 'desktop mode (single user)'}`} />
      <div className="grid gap-4 @3xl:grid-cols-2">
        <Card icon={Server} title="Server">
          <Item label="Version">{s.version}</Item>
          <Item label="Mode">
            <Badge variant={s.mode === 'server' ? 'default' : 'secondary'}>{s.mode}</Badge>
          </Item>
          <Item label="Host">{s.hostname}</Item>
          <Item label="Platform">
            {s.os}/{s.arch} · {s.cpus} CPUs · {s.goVersion}
          </Item>
          <Item label="Process">PID {s.pid}</Item>
          <Item label="Uptime">
            <span title={formatDateTime(s.startedAt)}>{formatDuration(s.uptimeSec * 1000)}</span>
          </Item>
        </Card>
        <Card icon={Network} title="Network">
          <Item label="Listening on">
            <code className="font-mono">{s.listen}</code>
          </Item>
          <Item label="HTTPS">
            {s.tls ? (
              <span className="inline-flex items-center gap-1 text-success">
                <ShieldCheck className="size-3.5" /> {s.tlsSelfSigned ? 'Self-signed certificate' : 'Enabled'}
              </span>
            ) : exposed ? (
              <span className="inline-flex items-center gap-1 text-warning">
                <ShieldAlert className="size-3.5" /> Off{s.insecureHttp ? ' (--insecure-http)' : ''}
              </span>
            ) : (
              'Off (loopback only)'
            )}
          </Item>
          <Item label="Trusted proxies">{s.trustedProxies.length ? s.trustedProxies.join(', ') : 'None'}</Item>
          <Item label="Browsers connected">{s.eventClients}</Item>
        </Card>
        <Card icon={HardDrive} title="Storage">
          <Item label="Data directory">
            <span className="inline-flex max-w-full items-center gap-1">
              <code className="truncate font-mono" title={s.dataDir}>
                {s.dataDir}
              </code>
              <CopyButton text={s.dataDir} size="xs" variant="ghost" label="" />
            </span>
          </Item>
          <Item label="Database">
            <span className="inline-flex items-center gap-1">
              <Database className="size-3.5 text-muted-foreground" /> {formatBytes(s.dbSizeBytes)}
            </span>
          </Item>
          <Item label="Scrollback">{formatBytes(s.scrollbackBytes)} per session</Item>
          <Item label="Detached sessions">{s.detachedTtlSec ? `closed after ${formatDuration(s.detachedTtlSec * 1000)}` : 'kept forever'}</Item>
        </Card>
        <Card icon={Users} title="Accounts & vault">
          <Item label="Users">
            {s.users} ({s.admins} admin{s.admins === 1 ? '' : 's'})
          </Item>
          <Item label="Signed-in browsers">{s.loginSessions}</Item>
          <Item label="Vault">
            {s.vaultHasMasterPassword ? (
              s.vaultLocked ? (
                <span className="inline-flex items-center gap-1 text-warning">
                  <Lock className="size-3.5" /> Locked (master password)
                </span>
              ) : (
                <span className="inline-flex items-center gap-1 text-success">
                  <LockOpen className="size-3.5" /> Unlocked (master password)
                </span>
              )
            ) : (
              'Server key (no master password)'
            )}
          </Item>
        </Card>
        <Card icon={Puzzle} title="Integrations">
          {Object.entries(s.features)
            .sort(([a], [b]) => a.localeCompare(b))
            .map(([k, v]) => (
              <Item key={k} label={FEATURE_LABELS[k] ?? k}>
                <span className={v ? 'text-success' : 'text-muted-foreground'}>{v ? 'Available' : 'Not found'}</span>
              </Item>
            ))}
        </Card>
        <Card icon={Cpu} title="guacd (optional RDP engine)">
          {guacd.isError ? (
            <Item label="Status">Unknown (RDP module not available)</Item>
          ) : guacd.data ? (
            <>
              <Item label="Status">
                {!guacd.data.configured ? (
                  'Disabled'
                ) : guacd.data.reachable ? (
                  <span className="text-success">Reachable{guacd.data.version ? ` · protocol ${guacd.data.version}` : ''}</span>
                ) : (
                  <span className="text-warning">Unreachable</span>
                )}
              </Item>
              <Item label="Address">
                <code className="font-mono">{guacd.data.address || s.guacd || '—'}</code>
              </Item>
              {guacd.data.defaultEngine && <Item label="Default RDP engine">{guacd.data.defaultEngine}</Item>}
              {guacd.data.error && <Item label="Last error">{guacd.data.error}</Item>}
            </>
          ) : (
            <Item label="Status">…</Item>
          )}
        </Card>
      </div>
    </>
  )
}
