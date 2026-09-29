/*
 * "Agent" sub-tab (SSH-11/12): AstraTerm's built-in SSH agent (MobAgent) — status, start / stop / lock, the socket path
 * with shell snippets (export SSH_AUTH_SOCK=…), the keys it offers (stored keys, keys added with ssh-add) and its
 * options (confirmations, auto-lock, key lifetime). The same key selection and confirmations apply to agent
 * forwarding.
 */
import { useState, type ReactNode } from 'react'
import { KeyRound, Lock, LockOpen, Play, RefreshCw, Square, TerminalSquare, Trash2, TriangleAlert } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { NumberInput } from '@/components/ui/number-input'
import { LoadingState } from '@/components/ui/query-state'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip } from '@/components/ui/tooltip'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { errorMessage, formatCalendarTime, formatRelativeTime } from '@/lib/utils'
import { useVaultLocked } from '@/stores/auth'
import { lockAgentAction, setKeyOffered, startAgentAction, stopAgentAction } from './actions'
import { invalidateAgent, reloadAgent, removeAgentKey, useAgentKeys, useAgentStatus } from './api'
import { CopyButton, Fingerprint, SectionLabel } from './components'
import { keysSettings } from './settings'
import type { AgentStatus } from './types'
import { keyTypeLabel, toastError } from './util'

type Shell = 'sh' | 'fish' | 'powershell' | 'cmd'

/** The shell command pointing SSH_AUTH_SOCK at the agent, with the path quoted for that shell. */
function snippet(shell: Shell, path: string): string {
  switch (shell) {
    case 'fish':
      return `set -gx SSH_AUTH_SOCK '${path.replaceAll('\\', '\\\\').replaceAll("'", "\\'")}'`
    case 'powershell':
      return `$env:SSH_AUTH_SOCK = '${path.replaceAll("'", "''")}'`
    case 'cmd':
      return `set "SSH_AUTH_SOCK=${path}"`
  }
  return `export SSH_AUTH_SOCK='${path.replaceAll("'", `'\\''`)}'`
}

export function AgentPanel({ visible }: { visible: boolean }) {
  const status = useAgentStatus(visible ? 5_000 : false)
  const firstLoad = useLoadingGate(status.isLoading)
  const st = status.data
  const keys = useAgentKeys(visible, visible && !!st?.running)
  const settings = keysSettings.use()
  const vaultLocked = useVaultLocked()

  if (firstLoad.hold) {
    return <div className="flex justify-center py-12">{firstLoad.show && <Spinner immediate />}</div>
  }
  if (status.isError || !st) {
    return (
      <EmptyState
        icon={KeyRound}
        title="Could not read the agent status"
        description={errorMessage(status.error)}
        action={
          <Button size="sm" variant="secondary" onClick={() => void status.refetch()}>
            Retry
          </Button>
        }
      />
    )
  }

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto grid max-w-5xl gap-4 p-4 @3xl:grid-cols-2 [&>*]:min-w-0">
        <StatusCard st={st} />
        {st.supported ? (
          <UsageCard st={st} />
        ) : (
          <Card>
            <CardHeader>
              <CardTitle>Agent forwarding</CardTitle>
              <CardDescription>
                In server mode AstraTerm does not open an agent socket on the server. Sessions with agent forwarding still offer your stored keys
                (the ones enabled below) to the remote host.
              </CardDescription>
            </CardHeader>
          </Card>
        )}
        <Card className="@3xl:col-span-2">
          <CardHeader className="flex-row items-center justify-between gap-2">
            <div className="grid gap-0.5">
              <CardTitle>Keys offered</CardTitle>
              <CardDescription>Stored keys the agent — and agent forwarding — may use. {vaultLocked && 'Unlock the vault to use them.'}</CardDescription>
            </div>
            {st.owner && (
              <Button
                size="sm"
                variant="ghost"
                onClick={async () => {
                  try {
                    await reloadAgent()
                    invalidateAgent()
                  } catch (err) {
                    toastError('Could not reload the keys', err)
                  }
                }}
              >
                <RefreshCw /> Reload keys
              </Button>
            )}
          </CardHeader>
          <CardContent className="px-0 pb-1">
            <LoadingState busy={keys.isLoading} skeleton={<div className="flex justify-center py-6">
                <Spinner />
              </div>}>
              {!keys.data?.length ? (
              <EmptyState size="sm" icon={KeyRound} title="No stored keys" description="Generate or import a key in the SSH keys tab." />
            ) : (
              <Table aria-label="Agent keys">
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-12">Offer</TableHead>
                    <TableHead>Key</TableHead>
                    <TableHead className="hidden @2xl:table-cell">Fingerprint</TableHead>
                    <TableHead>State</TableHead>
                    <TableHead className="w-0">
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {keys.data.map((k) => (
                    <TableRow key={k.id}>
                      <TableCell>
                        {k.source === 'stored' && k.keyId ? (
                          <Switch size="sm" aria-label={`Offer ${k.name}`} checked={!settings.agentExclude.includes(k.keyId)} onCheckedChange={(v) => setKeyOffered(k.keyId!, v)} />
                        ) : (
                          <Badge variant="outline">ssh-add</Badge>
                        )}
                      </TableCell>
                      <TableCell>
                        <div className="font-medium">{k.name}</div>
                        <div className="text-sm text-muted-foreground">{keyTypeLabel(k.type, k.bits)}</div>
                      </TableCell>
                      <TableCell className="hidden max-w-[18rem] @2xl:table-cell">
                        <Fingerprint value={k.fingerprint} />
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-wrap gap-1">
                          {k.unloaded && (
                            <Tooltip content="Removed with ssh-add -d / -D; “Reload keys” brings it back">
                              <Badge variant="warning">Removed</Badge>
                            </Tooltip>
                          )}
                          {k.excluded && <Badge variant="secondary">Not offered</Badge>}
                          {k.unlocked && (
                            <Tooltip content={k.expiresAt ? `Decrypted in memory until ${formatCalendarTime(k.expiresAt)}` : 'Decrypted in memory'}>
                              <Badge variant="success">
                                <LockOpen /> Unlocked
                              </Badge>
                            </Tooltip>
                          )}
                          {k.needsPassphrase && !k.unlocked && (
                            <Tooltip content="The passphrase is asked for on first use">
                              <Badge variant="info">
                                <Lock /> Asks passphrase
                              </Badge>
                            </Tooltip>
                          )}
                          {k.confirm && <Badge variant="info">Confirm each use</Badge>}
                          {k.certificate && <Badge variant="secondary">+ certificate</Badge>}
                        </div>
                      </TableCell>
                      <TableCell>
                        {k.source === 'added' && (
                          <IconButton
                            icon={Trash2}
                            size="xs"
                            label="Remove from the agent"
                            onClick={async () => {
                              try {
                                await removeAgentKey(k.id)
                                invalidateAgent()
                              } catch (err) {
                                toastError('Could not remove the key', err)
                              }
                            }}
                          />
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
            </LoadingState>
          </CardContent>
        </Card>
        <OptionsCard supported={st.supported} />
      </div>
    </div>
  )
}

function StatusCard({ st }: { st: AgentStatus }) {
  const [busy, setBusy] = useState(false)
  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    try {
      await fn()
    } finally {
      setBusy(false)
    }
  }
  const state = !st.supported ? 'Unavailable' : !st.running ? 'Stopped' : !st.owner ? 'Used by another user' : st.locked ? 'Locked' : 'Running'
  const tone = state === 'Running' ? 'success' : state === 'Locked' ? 'warning' : 'secondary'
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <KeyRound className="size-4 text-primary" /> AstraTerm SSH agent
          <Badge variant={tone} className="ml-auto">
            <span className="size-1.5 rounded-full bg-current" /> {state}
          </Badge>
        </CardTitle>
        <CardDescription>
          {st.supported
            ? 'Exposes your stored keys to local programs (ssh, git, IDEs) through a socket only your user account can open.'
            : st.reason || 'The built-in agent is only available in desktop mode.'}
        </CardDescription>
      </CardHeader>
      {st.supported && (
        <CardContent className="grid gap-3">
          {st.vaultLocked && (
            <p className="flex items-center gap-2 text-sm text-warning">
              <TriangleAlert className="size-4" /> The vault is locked: stored keys are unavailable until it is unlocked.
            </p>
          )}
          {st.owner && (
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
              <dt className="text-muted-foreground">Started</dt>
              <dd>{st.startedAt ? formatRelativeTime(st.startedAt) : '—'}</dd>
              <dt className="text-muted-foreground">Keys</dt>
              <dd>{st.keyCount}</dd>
              <dt className="text-muted-foreground">Clients</dt>
              <dd>{st.clients} connected</dd>
              <dt className="text-muted-foreground">Signatures</dt>
              <dd>
                {st.signatures}
                {st.lastUsedAt && <span className="text-muted-foreground"> · last {formatRelativeTime(st.lastUsedAt)}</span>}
              </dd>
            </dl>
          )}
          <div className="flex flex-wrap gap-1.5">
            {!st.running ? (
              <Button size="sm" loading={busy} onClick={() => void run(startAgentAction)}>
                <Play /> Start agent
              </Button>
            ) : st.owner ? (
              <>
                <Button size="sm" variant="secondary" loading={busy} onClick={() => void run(stopAgentAction)}>
                  <Square /> Stop
                </Button>
                {st.locked ? (
                  <Button size="sm" variant="secondary" onClick={() => void lockAgentAction(false)}>
                    <LockOpen /> Unlock
                  </Button>
                ) : (
                  <Button size="sm" variant="ghost" onClick={() => void lockAgentAction(true)}>
                    <Lock /> Lock
                  </Button>
                )}
              </>
            ) : null}
          </div>
          <label className="flex items-center justify-between gap-3 border-t pt-3 text-base">
            <span>
              Start automatically with AstraTerm
              <span className="block text-sm text-muted-foreground">When AstraTerm starts, the agent starts for the desktop user.</span>
            </span>
            <Switch checked={keysSettings.useValue('agentAutostart')} onCheckedChange={(v) => keysSettings.set({ agentAutostart: v })} />
          </label>
        </CardContent>
      )}
    </Card>
  )
}

function UsageCard({ st }: { st: AgentStatus }) {
  const windows = st.platform === 'windows'
  const [shell, setShell] = useState<Shell>(windows ? 'powershell' : 'sh')
  const path = st.socketPath
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <TerminalSquare className="size-4 text-primary" /> Use it from a terminal
        </CardTitle>
        <CardDescription>Point SSH_AUTH_SOCK at the agent (add it to your shell profile to make it permanent), then check with ssh-add -l.</CardDescription>
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-3">
        {!path ? (
          <p className="text-sm text-muted-foreground">{st.running ? 'The agent runs for another user.' : 'Start the agent to get its socket path.'}</p>
        ) : (
          <>
            <div className="grid min-w-0 grid-cols-1 gap-1">
              <SectionLabel>{windows ? 'Named pipe' : 'Socket'}</SectionLabel>
              <div className="flex items-center gap-2 rounded-md border bg-muted/40 px-2 py-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-sm select-all" title={path}>
                  {path}
                </span>
                <CopyButton text={path} label="Copy socket path" what="Socket path copied" />
              </div>
            </div>
            <SegmentedControl<Shell>
              size="sm"
              aria-label="Shell"
              value={shell}
              onValueChange={setShell}
              options={windows ? [{ value: 'powershell', label: 'PowerShell' }, { value: 'cmd', label: 'cmd' }] : [{ value: 'sh', label: 'bash / zsh' }, { value: 'fish', label: 'fish' }]}
            />
            <div className="flex items-start gap-2 rounded-md border bg-muted/40 px-2 py-1.5">
              <code className="min-w-0 flex-1 font-mono text-sm break-all select-all">{snippet(shell, path)}</code>
              <CopyButton text={snippet(shell, path)} label="Copy command" what="Command copied" />
            </div>
          </>
        )}
      </CardContent>
    </Card>
  )
}

function OptionsCard({ supported }: { supported: boolean }) {
  const s = keysSettings.use()
  return (
    <Card className="@3xl:col-span-2">
      <CardHeader>
        <CardTitle>Options</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 @2xl:grid-cols-2">
        {supported && (
          <OptionRow label="Confirm each use by local programs" description="Ask before a program on this computer signs with a key.">
            <Switch checked={s.agentConfirm} onCheckedChange={(v) => keysSettings.set({ agentConfirm: v })} aria-label="Confirm local use" />
          </OptionRow>
        )}
        <OptionRow label="Confirm each use by forwarded agents" description="Ask before a remote server uses your keys through agent forwarding.">
          <Switch checked={s.agentForwardConfirm} onCheckedChange={(v) => keysSettings.set({ agentForwardConfirm: v })} aria-label="Confirm forwarded use" />
        </OptionRow>
        {supported && (
          <OptionRow label="Lock after inactivity" description="Minutes without signatures (0 = never).">
            <NumberInput inputSize="sm" className="w-24" value={s.agentAutoLockMin} min={0} max={10080} onChange={(n) => keysSettings.set({ agentAutoLockMin: n ?? 0 })} aria-label="Auto-lock minutes" />
          </OptionRow>
        )}
        <OptionRow label="Forget unlocked keys after" description="Minutes; keys without a remembered passphrase ask again (0 = never).">
          <NumberInput inputSize="sm" className="w-24" value={s.agentKeyLifetimeMin} min={0} max={10080} onChange={(n) => keysSettings.set({ agentKeyLifetimeMin: n ?? 0 })} aria-label="Key lifetime minutes" />
        </OptionRow>
      </CardContent>
    </Card>
  )
}

function OptionRow({ label, description, children }: { label: string; description: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 rounded-md border px-3 py-2">
      <div className="grid gap-0.5">
        <span className="text-base font-medium">{label}</span>
        <span className="text-sm text-muted-foreground">{description}</span>
      </div>
      {children}
    </div>
  )
}
