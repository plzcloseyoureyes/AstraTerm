/*
 * Configuration dialog of one embedded server: listening address and port, shared folder, autostart / auto-stop,
 * accounts and the server-specific options. Saving a running server restarts it (the backend does).
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Info, TriangleAlert } from 'lucide-react'
import { isApiError } from '@/api/client'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { SwitchField } from '@/components/ui/switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { cn, errorMessage, jsonEqual } from '@/lib/utils'
import { saveConfigAction } from './actions'
import { useHostInfo, useServer } from './api'
import { KINDS, STOP_AFTER_PRESETS, isLoopbackBind, isWildcard } from './model'
import type { FTPTLSMode, HostInfo, ServerKindEx, ServerUser } from './types'
import { UsersEditor, userProblems, usersPayload } from './UsersEditor'

type Draft = Record<string, unknown>

const str = (v: unknown, d = '') => (typeof v === 'string' ? v : d)
const num = (v: unknown, d = 0) => (typeof v === 'number' && Number.isFinite(v) ? v : d)
const bool = (v: unknown) => v === true

function toUsers(v: unknown): ServerUser[] {
  if (!Array.isArray(v)) return []
  return v.map((u) => {
    const o = (u ?? {}) as Record<string, unknown>
    return {
      id: str(o.id) || undefined,
      username: str(o.username),
      hasPassword: bool(o.hasPassword),
      publicKeys: Array.isArray(o.publicKeys) ? o.publicKeys.filter((k): k is string => typeof k === 'string') : [],
      readOnly: bool(o.readOnly),
    }
  })
}

function Note({ children, tone = 'info' }: { children: ReactNode; tone?: 'info' | 'warning' }) {
  const Icon = tone === 'warning' ? TriangleAlert : Info
  return (
    <p className={cn('flex items-start gap-1.5 text-sm', tone === 'warning' ? 'text-warning' : 'text-muted-foreground')}>
      <Icon className="mt-0.5 size-3.5 shrink-0" aria-hidden />
      <span>{children}</span>
    </p>
  )
}

// ---- listening address ------------------------------------------------------------------------------------------------

function BindField({ value, onChange, host }: { value: string; onChange: (v: string) => void; host?: HostInfo }) {
  const options = useMemo(() => {
    const out: { value: string; label: string }[] = [
      { value: '127.0.0.1', label: '127.0.0.1 — this machine only' },
      { value: '0.0.0.0', label: '0.0.0.0 — every IPv4 interface' },
      { value: '::', label: ':: — every interface (IPv6 and IPv4)' },
    ]
    for (const ifc of host?.interfaces ?? []) {
      if (!ifc.up) continue
      for (const a of ifc.addresses) {
        if (a.startsWith('fe80:') || out.some((o) => o.value === a)) continue
        out.push({ value: a, label: `${a} — ${ifc.name}${ifc.loopback ? ' (loopback)' : ''}` })
      }
    }
    return out
  }, [host])
  const known = options.some((o) => o.value === value)
  const [custom, setCustom] = useState(!known)
  return (
    <Field
      label="Listen on"
      hint={
        isWildcard(value)
          ? undefined
          : isLoopbackBind(value)
            ? 'Only programs on this machine can connect.'
            : 'Machines on the network of this address can connect.'
      }
    >
      <div className="grid gap-1.5">
        <SimpleSelect
          value={custom ? '__custom' : value}
          onValueChange={(v) => {
            if (v === '__custom') {
              setCustom(true)
              return
            }
            setCustom(false)
            onChange(v)
          }}
          options={[...options, { value: '__custom', label: 'Other address…' }]}
          aria-label="Listen address"
        />
        {custom && (
          <Input
            value={value}
            onChange={(e) => onChange(e.target.value.trim())}
            placeholder="IP address"
            aria-label="Custom listen address"
            className="font-mono"
            spellCheck={false}
          />
        )}
        {isWildcard(value) && <Note tone="warning">Every network interface: other machines on your networks can connect.</Note>}
      </div>
    </Field>
  )
}

/** Risks of exposing a server with this configuration beyond this machine (for the confirmation on save). */
function exposureRisks(kind: ServerKindEx, d: Draft): string[] {
  const out: string[] = []
  switch (kind) {
    case 'http':
      if (!bool(d.requireAuth)) out.push(bool(d.readOnly) ? 'Anyone who can reach it can download the shared folder.' : 'Anyone who can reach it can download and upload files.')
      else if (!bool(d.tls)) out.push('Passwords travel unencrypted (HTTPS is off).')
      break
    case 'ftp':
      if (str(d.tls, 'off') === 'off') out.push('Passwords and files travel unencrypted (FTPS is off).')
      if (bool(d.anonymous)) out.push(bool(d.anonymousWrite) && !bool(d.readOnly) ? 'Anonymous users can upload, rename and delete files.' : 'Anonymous users can download files.')
      break
    case 'sftp':
      if (bool(d.shell)) out.push('Users get a shell on this machine, outside the shared folder.')
      break
    case 'tftp':
      out.push(bool(d.readOnly) ? 'TFTP has no login: anyone who can reach it can download files.' : 'TFTP has no login: anyone who can reach it can download and upload files.')
      break
    case 'telnet':
      out.push('Telnet is unencrypted: passwords and whole shell sessions can be read on the network.')
      break
  }
  return out
}

// ---- dialog -----------------------------------------------------------------------------------------------------------

export default function ConfigDialog({ kind, initialTab, onClose }: { kind: ServerKindEx; initialTab?: string; onClose: () => void }) {
  const status = useServer(kind)
  const { data: host } = useHostInfo()
  const info = KINDS[kind]
  const [original, setOriginal] = useState<Draft | null>(null)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [users, setUsers] = useState<ServerUser[]>([])
  const [tab, setTab] = useState(initialTab ?? 'general')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  useEffect(() => {
    if (status && !draft) {
      const c = structuredClone(status.config) as Draft
      setOriginal(c)
      setDraft(c)
      setUsers(toUsers(c.users))
    }
  }, [status, draft])

  const keys = kind === 'sftp'
  const problems = useMemo(() => userProblems(users, { keys }), [users, keys])
  const dirty = useMemo(() => {
    if (!draft || !original) return false
    const { users: _u, ...a } = draft
    const { users: _o, ...b } = original
    if (!jsonEqual(a, b)) return true
    return !jsonEqual(usersPayload(users, keys), usersPayload(toUsers(original.users), keys))
  }, [draft, original, users, keys])

  const requestClose = async () => {
    if (saving) return
    if (dirty && !(await confirm({ title: 'Discard your changes?', confirmLabel: 'Discard', destructive: true }))) return
    onClose()
  }

  if (!status || !draft) {
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent size="xl" aria-describedby={undefined}>
          <DialogTitle className="sr-only">{info.label} settings</DialogTitle>
          <LoadingPane label="Loading…" />
        </DialogContent>
      </Dialog>
    )
  }

  const set = (patch: Draft) => setDraft((d) => ({ ...d, ...patch }))
  const port = num(draft.port, info.defaultPort)
  const bind = str(draft.bindAddress, '127.0.0.1')
  const root = str(draft.root)
  const hasUsers = info.users
  const usersRequired =
    kind === 'sftp' || kind === 'telnet' || (kind === 'ftp' && !bool(draft.anonymous)) || (kind === 'http' && bool(draft.requireAuth))
  const blocking =
    problems.size > 0 ||
    port < 1 ||
    port > 65535 ||
    (info.files && !root.trim()) ||
    (kind === 'syslog' && !bool(draft.udp) && !bool(draft.tcp))

  const save = async () => {
    if (blocking) {
      if (problems.size > 0) setTab('access')
      return
    }
    // Loud confirmation when a change opens the server to the network with risky settings.
    const wasLocal = isLoopbackBind(str(original?.bindAddress, '127.0.0.1'))
    const risks = exposureRisks(kind, draft)
    if (!isLoopbackBind(bind) && risks.length > 0 && (wasLocal || !jsonEqual(exposureRisks(kind, original ?? {}), risks))) {
      const ok = await confirm({
        title: `Reachable from the network: ${info.label}`,
        description: (
          <>
            <span className="block">
              Listening on {isWildcard(bind) ? 'every interface' : bind}, other machines can connect.
            </span>
            {risks.map((r) => (
              <span key={r} className="mt-1.5 flex items-start gap-1.5 text-warning">
                <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
                {r}
              </span>
            ))}
          </>
        ),
        confirmLabel: 'Save anyway',
        destructive: true,
      })
      if (!ok) return
    }
    setSaving(true)
    setError(undefined)
    try {
      const payload: Draft = { ...draft }
      if (hasUsers) payload.users = usersPayload(users, keys)
      else delete payload.users
      await saveConfigAction(kind, payload)
      onClose()
    } catch (err) {
      setError(isApiError(err) ? err.message : errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  const privileged = !!host?.privilegedPorts && port < 1024 && !(host.privilegedWildcardOk && isWildcard(bind))
  const ownPort = host?.nexTermPort === port && kind !== 'tftp' && !(kind === 'syslog' && !bool(draft.tcp))
  const stopAfter = num(draft.stopAfterSec)
  const stopOptions = STOP_AFTER_PRESETS.some((p) => p.value === stopAfter)
    ? STOP_AFTER_PRESETS
    : [...STOP_AFTER_PRESETS, { value: stopAfter, label: `After ${Math.round(stopAfter / 60)} minutes` }]

  const tabs: { id: string; label: string }[] = [{ id: 'general', label: 'General' }]
  if (hasUsers) tabs.push({ id: 'access', label: `Users${users.length ? ` (${users.length})` : ''}` })
  if (kind !== 'http') tabs.push({ id: 'advanced', label: 'Advanced' })

  return (
    <Dialog open onOpenChange={(o) => !o && void requestClose()}>
      <DialogContent
        size="xl"
        className="h-[min(88vh,44rem)]"
        onEscapeKeyDown={(e) => {
          e.preventDefault()
          void requestClose()
        }}
        onInteractOutside={(e) => {
          e.preventDefault()
        }}
      >
        <DialogHeader>
          <DialogTitle>
            <info.icon className="size-4 text-muted-foreground" aria-hidden />
            {info.label} settings
          </DialogTitle>
          <DialogDescription>{info.description}</DialogDescription>
        </DialogHeader>

        <Tabs value={tab} onValueChange={setTab} className="min-h-0 flex-1">
          <TabsList>
            {tabs.map((t) => (
              <TabsTrigger key={t.id} value={t.id}>
                {t.label}
                {t.id === 'access' && problems.size > 0 && <span className="size-1.5 rounded-full bg-destructive" aria-label="has problems" />}
              </TabsTrigger>
            ))}
          </TabsList>
          <DialogBody className="min-h-0 flex-1 pt-2">
            <TabsContent value="general" className="grid gap-4">
              <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_9rem]">
                <BindField value={bind} onChange={(v) => set({ bindAddress: v })} host={host} />
                <Field
                  label="Port"
                  error={ownPort ? "NexTerm's own port" : undefined}
                  hint={
                    privileged
                      ? host?.privilegedWildcardOk
                        ? 'Below 1024: on macOS only when listening on every interface.'
                        : 'Below 1024: needs administrator rights.'
                      : `Default ${info.defaultPort}`
                  }
                >
                  <NumberInput value={port} min={1} max={65535} onChange={(v) => set({ port: v ?? info.defaultPort })} />
                </Field>
              </div>
              {info.files && (
                <Field
                  label="Shared folder"
                  required
                  hint="Clients see this folder as “/” and cannot leave it. NexTerm's data folder cannot be shared."
                  labelAside={
                    host?.defaultRoot && root !== host.defaultRoot ? (
                      <Button variant="link" size="xs" onClick={() => set({ root: host.defaultRoot })}>
                        Use default
                      </Button>
                    ) : undefined
                  }
                >
                  <Input
                    value={root}
                    onChange={(e) => set({ root: e.target.value })}
                    placeholder={host?.defaultRoot ?? '/path/to/folder'}
                    className="font-mono"
                    spellCheck={false}
                  />
                </Field>
              )}
              <KindGeneral kind={kind} draft={draft} set={set} host={host} />
              <div className="grid gap-3 border-t pt-4 sm:grid-cols-2">
                <SwitchField
                  label="Start with NexTerm"
                  description="Start this server automatically."
                  checked={bool(draft.autoStart)}
                  onCheckedChange={(v) => set({ autoStart: v })}
                />
                <Field label="Stop automatically">
                  <SimpleSelect
                    value={String(stopAfter)}
                    onValueChange={(v) => set({ stopAfterSec: Number(v) })}
                    options={stopOptions.map((o) => ({ value: String(o.value), label: o.label }))}
                  />
                </Field>
              </div>
            </TabsContent>

            {hasUsers && (
              <TabsContent value="access" className="grid gap-4">
                <KindAccess kind={kind} draft={draft} set={set} host={host} />
                <UsersEditor users={users} onChange={setUsers} keys={keys} readOnlyOption={info.files} problems={problems} />
                {kind === 'http' && !bool(draft.requireAuth) && users.length > 0 && (
                  <Note>These accounts are only checked while “Require login” is on.</Note>
                )}
                {usersRequired && users.length === 0 && <Note tone="warning">Add at least one user before starting the server.</Note>}
                {kind === 'sftp' && (
                  <Note>
                    Users authenticate with a password or a public key. Everyone sees the shared folder as “/”; use
                    read-only for users who should only download.
                  </Note>
                )}
              </TabsContent>
            )}

            <TabsContent value="advanced" className="grid gap-4">
              <KindAdvanced kind={kind} draft={draft} set={set} host={host} />
            </TabsContent>
          </DialogBody>
        </Tabs>

        {error && (
          <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {error}
          </p>
        )}
        <DialogFooter>
          {status.running && dirty && <span className="mr-auto text-sm text-muted-foreground">The server restarts to apply the changes.</span>}
          <Button variant="ghost" onClick={() => void requestClose()} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={() => void save()} loading={saving} disabled={!dirty || blocking}>
            {status.running && dirty ? 'Save & restart' : 'Save'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ---- kind-specific sections -------------------------------------------------------------------------------------------

interface SectionProps {
  kind: ServerKindEx
  draft: Draft
  set: (patch: Draft) => void
  host?: HostInfo
}

function KindGeneral({ kind, draft, set }: SectionProps) {
  switch (kind) {
    case 'http':
      return (
        <div className="grid gap-3 sm:grid-cols-2">
          <SwitchField
            label="Directory listings"
            description="Show folder contents (index.html is served when present)."
            checked={bool(draft.listing)}
            onCheckedChange={(v) => set({ listing: v })}
          />
          <SwitchField
            label="Read-only"
            description="Clients can only download."
            checked={bool(draft.readOnly)}
            onCheckedChange={(v) => set({ readOnly: v })}
          />
          <SwitchField
            label="Browser uploads"
            description="Upload and new-folder forms in listings (PUT uploads work whenever writing is allowed)."
            checked={bool(draft.upload)}
            disabled={bool(draft.readOnly)}
            onCheckedChange={(v) => set({ upload: v })}
          />
          <SwitchField
            label="HTTPS"
            description="Serve TLS with a self-signed certificate kept by NexTerm."
            checked={bool(draft.tls)}
            onCheckedChange={(v) => set({ tls: v })}
          />
          {!bool(draft.readOnly) && (
            <Field label="Upload size limit" hint="0 = unlimited">
              <NumberInput value={num(draft.maxUploadMB)} min={0} max={1048576} unit="MB" onChange={(v) => set({ maxUploadMB: v ?? 0 })} />
            </Field>
          )}
        </div>
      )
    case 'ftp':
      return (
        <div className="grid gap-3">
          <SwitchField
            label="Read-only for everyone"
            description="No user can upload, rename or delete."
            checked={bool(draft.readOnly)}
            onCheckedChange={(v) => set({ readOnly: v })}
          />
          <Field
            label="Encryption (FTPS)"
            hint={
              {
                off: 'Plain FTP: passwords and files travel unencrypted.',
                optional: 'Clients may upgrade with AUTH TLS (explicit FTPS). Recommended.',
                required: 'Clients must use explicit FTPS (AUTH TLS) before logging in.',
                implicit: 'Implicit FTPS: TLS from the first byte (traditionally port 990).',
              }[str(draft.tls, 'off') as FTPTLSMode]
            }
          >
            <SegmentedControl
              value={str(draft.tls, 'off') as FTPTLSMode}
              onValueChange={(v) => set({ tls: v })}
              aria-label="FTP encryption"
              options={[
                { value: 'off', label: 'Off' },
                { value: 'optional', label: 'Optional' },
                { value: 'required', label: 'Required' },
                { value: 'implicit', label: 'Implicit' },
              ]}
            />
          </Field>
        </div>
      )
    case 'sftp':
      return (
        <SwitchField
          label="Read-only for everyone"
          description="No user can upload, rename or delete."
          checked={bool(draft.readOnly)}
          onCheckedChange={(v) => set({ readOnly: v })}
        />
      )
    case 'tftp':
      return (
        <div className="grid gap-2">
          <SwitchField
            label="Allow uploads"
            description="Accept write requests (device configuration backups)."
            checked={!bool(draft.readOnly)}
            onCheckedChange={(v) => set({ readOnly: !v })}
          />
          {!bool(draft.readOnly) && <Note tone="warning">TFTP has no authentication: anyone who can reach the server can write files.</Note>}
        </div>
      )
    case 'telnet':
      return (
        <div className="grid gap-3">
          <Note tone="warning">Telnet is unencrypted. Keep it on this machine only unless you really need legacy clients.</Note>
          <Field label="Start folder" hint="Empty: your home folder.">
            <Input
              value={str(draft.workingDir)}
              onChange={(e) => set({ workingDir: e.target.value })}
              placeholder="~"
              className="font-mono"
              spellCheck={false}
            />
          </Field>
        </div>
      )
    case 'syslog':
      return (
        <div className="grid gap-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <SwitchField label="UDP" description="Classic syslog (most devices)." checked={bool(draft.udp)} onCheckedChange={(v) => set({ udp: v })} />
            <SwitchField label="TCP" description="RFC 6587 framing (octet counting or newline)." checked={bool(draft.tcp)} onCheckedChange={(v) => set({ tcp: v })} />
          </div>
          {!bool(draft.udp) && !bool(draft.tcp) && <Note tone="warning">Enable UDP, TCP or both.</Note>}
          {isLoopbackBind(str(draft.bindAddress)) && (
            <Note>Network devices can only send messages once the server listens on a network interface (or every interface).</Note>
          )}
        </div>
      )
  }
}

function KindAccess({ kind, draft, set, host }: SectionProps) {
  switch (kind) {
    case 'http':
      return (
        <div className="grid gap-2">
          <SwitchField
            label="Require login"
            description="HTTP basic authentication with the users below."
            checked={bool(draft.requireAuth)}
            onCheckedChange={(v) => set({ requireAuth: v })}
          />
          {bool(draft.requireAuth) && !bool(draft.tls) && !isLoopbackBind(str(draft.bindAddress)) && (
            <Note tone="warning">Without HTTPS the passwords travel unencrypted.</Note>
          )}
        </div>
      )
    case 'ftp':
      return (
        <div className="grid gap-3 sm:grid-cols-2">
          <SwitchField
            label="Anonymous access"
            description="Log in as “anonymous” or “ftp” with any password."
            checked={bool(draft.anonymous)}
            onCheckedChange={(v) => set({ anonymous: v, anonymousWrite: v ? draft.anonymousWrite : false })}
          />
          <SwitchField
            label="Anonymous uploads"
            description="Anonymous users may upload, rename and delete."
            checked={bool(draft.anonymousWrite)}
            disabled={!bool(draft.anonymous) || bool(draft.readOnly)}
            onCheckedChange={(v) => set({ anonymousWrite: v })}
          />
        </div>
      )
    case 'sftp':
      return (
        <div className="grid gap-2">
          <SwitchField
            label="Allow shell access"
            description="Interactive shells and remote commands (ssh user@host). Off: SFTP only."
            checked={bool(draft.shell)}
            onCheckedChange={(v) => set({ shell: v })}
          />
          {bool(draft.shell) && (
            <Note tone="warning">
              Shells run as {host?.osUser || 'the NexTerm user'} on this machine and are not limited to the shared folder.
            </Note>
          )}
        </div>
      )
    case 'telnet':
      return <Note>Sessions run a shell as {host?.osUser || 'the NexTerm user'} on this machine.</Note>
  }
  return null
}

function KindAdvanced({ kind, draft, set }: SectionProps) {
  switch (kind) {
    case 'ftp':
      return (
        <div className="grid gap-4">
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Passive ports from" hint="0 = any free port">
              <NumberInput value={num(draft.passivePortMin)} min={0} max={65535} onChange={(v) => set({ passivePortMin: v ?? 0 })} />
            </Field>
            <Field label="to">
              <NumberInput value={num(draft.passivePortMax)} min={0} max={65535} onChange={(v) => set({ passivePortMax: v ?? 0 })} />
            </Field>
            <Field label="Idle timeout">
              <NumberInput value={num(draft.idleTimeoutSec, 900)} min={30} max={86400} unit="s" onChange={(v) => set({ idleTimeoutSec: v ?? 900 })} />
            </Field>
          </div>
          <Field label="Public address for passive mode" hint="IPv4 announced in PASV replies behind NAT; empty = the address clients connect to.">
            <Input
              value={str(draft.publicHost)}
              onChange={(e) => set({ publicHost: e.target.value.trim() })}
              placeholder="e.g. 203.0.113.10"
              className="font-mono"
              spellCheck={false}
            />
          </Field>
        </div>
      )
    case 'sftp':
      return (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Shell program" hint="Empty: your default shell.">
            <Input
              value={str(draft.shellCommand)}
              onChange={(e) => set({ shellCommand: e.target.value })}
              placeholder="/bin/bash"
              className="font-mono"
              spellCheck={false}
              disabled={!bool(draft.shell)}
            />
          </Field>
          <Field label="Idle timeout">
            <NumberInput value={num(draft.idleTimeoutSec, 1800)} min={30} max={604800} unit="s" onChange={(v) => set({ idleTimeoutSec: v ?? 1800 })} />
          </Field>
        </div>
      )
    case 'tftp':
      return (
        <div className="grid gap-4">
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Max block size" hint="0 = negotiate up to the MTU">
              <NumberInput value={num(draft.blockSize)} min={0} max={65464} onChange={(v) => set({ blockSize: v ?? 0 })} />
            </Field>
            <Field label="Timeout">
              <NumberInput value={num(draft.timeoutSec, 5)} min={1} max={255} unit="s" onChange={(v) => set({ timeoutSec: v ?? 5 })} />
            </Field>
            <Field label="Retries">
              <NumberInput value={num(draft.retries, 5)} min={1} max={50} onChange={(v) => set({ retries: v ?? 5 })} />
            </Field>
          </div>
          <SwitchField
            label="Single port"
            description="Answer every transfer from the listening port (works through NAT and strict firewalls; slower)."
            checked={bool(draft.singlePort)}
            onCheckedChange={(v) => set({ singlePort: v })}
          />
        </div>
      )
    case 'telnet':
      return (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Shell program" hint="Empty: your default shell.">
            <Input
              value={str(draft.shellCommand)}
              onChange={(e) => set({ shellCommand: e.target.value })}
              placeholder="/bin/bash"
              className="font-mono"
              spellCheck={false}
            />
          </Field>
          <Field label="Idle timeout">
            <NumberInput value={num(draft.idleTimeoutSec, 3600)} min={30} max={604800} unit="s" onChange={(v) => set({ idleTimeoutSec: v ?? 3600 })} />
          </Field>
        </div>
      )
    case 'syslog':
      return (
        <div className="grid gap-4">
          <Field label="Messages kept in memory" hint="100 – 100000">
            <NumberInput value={num(draft.bufferSize, 10000)} min={100} max={100000} step={1000} onChange={(v) => set({ bufferSize: v ?? 10000 })} />
          </Field>
          <SwitchField
            label="Write messages to files"
            description="One file per day: syslog-YYYY-MM-DD.log."
            checked={bool(draft.logToFile)}
            onCheckedChange={(v) => set({ logToFile: v })}
          />
          {bool(draft.logToFile) && (
            <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_8rem_8rem]">
              <Field label="Folder" hint="Empty: NexTerm's data folder (logs/syslog).">
                <Input
                  value={str(draft.logDir)}
                  onChange={(e) => set({ logDir: e.target.value })}
                  placeholder="(default)"
                  className="font-mono"
                  spellCheck={false}
                />
              </Field>
              <Field label="Keep" hint="0 = forever">
                <NumberInput value={num(draft.retentionDays, 14)} min={0} max={3650} unit="days" onChange={(v) => set({ retentionDays: v ?? 14 })} />
              </Field>
              <Field label="Max per day" hint="0 = no limit">
                <NumberInput value={num(draft.maxFileMB, 256)} min={0} max={1048576} unit="MB" onChange={(v) => set({ maxFileMB: v ?? 256 })} />
              </Field>
            </div>
          )}
        </div>
      )
  }
  return null
}
