/*
 * Tunnel editor (TUN-1…TUN-6): kind, SSH server, listener (TCP or Unix socket), destination, proxy authentication and
 * advanced behaviour, with a live flow diagram, client-side validation, a debounced "is this port free?" pre-flight
 * check, and Save / Save & start.
 */
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { toast } from 'sonner'
import {
  ArrowDownUp,
  ArrowLeftRight,
  ChevronDown,
  ChevronRight,
  CircleCheck,
  CircleX,
  Copy,
  Globe,
  Info,
  Plus,
  ShieldAlert,
  WandSparkles,
  Waypoints,
} from 'lucide-react'
import { isApiError } from '@/api/client'
import { useConnections } from '@/api/connections'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { ColorSwatchPicker } from '@/components/ui/color-swatch-picker'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Kbd } from '@/components/ui/kbd'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Switch } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { Textarea } from '@/components/ui/textarea'
import { SimpleSelect } from '@/components/ui/select'
import { useLatest } from '@/lib/hooks'
import { cn, copyText, errorMessage, isMac } from '@/lib/utils'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { checkBind, createTunnel, putTunnel, startTunnel, updateTunnel, useTunnels } from './api'
import { ConnectionPicker, IconPicker, KindBadge, PortInput, TunnelIcon } from './components'
import { FlowDiagram, type DiagramFocus } from './FlowDiagram'
import {
  DEST_PRESETS,
  KINDS,
  KIND_ORDER,
  applyDestPreset,
  connectionRoute,
  draftDockerHost,
  draftExposed,
  draftFromTunnel,
  emptyDraft,
  hostPort,
  inputFromDraft,
  isActive,
  isLoopbackHost,
  isProxyKind,
  kindOf,
  suggestName,
  validateDraft,
  type DockerHostHint,
  type TunnelDraft,
} from './model'
import type { EditorRequest } from './store'
import type { CheckBindResult, TunnelEx, TunnelKind } from './types'

function initialDraft(req: EditorRequest, existing: TunnelEx | undefined): TunnelDraft {
  if (existing) return draftFromTunnel(existing)
  const d = emptyDraft()
  const i = req.initial
  if (!i) return d
  if (i.type) d.kind = i.type === 'rdynamic' ? 'rdynamic' : i.type === 'dynamic' ? 'dynamic' : i.type === 'remote' ? 'remote' : 'local'
  if (i.connectionId) d.connectionId = i.connectionId
  if (i.name) d.name = i.name
  if (i.bindHost) d.bindHost = i.bindHost
  if (typeof i.bindPort === 'number') d.bindPort = i.bindPort
  if (i.destHost) d.destHost = i.destHost
  if (typeof i.destPort === 'number') d.destPort = i.destPort
  if (isProxyKind(d.kind) && d.bindPort == null) d.bindPort = 1080
  return d
}

const HOST_PRESETS: { value: string; label: string; server?: boolean; host?: boolean }[] = [
  { value: '127.0.0.1', label: '127.0.0.1 — this machine only (IPv4)', host: true },
  { value: '127.0.0.1', label: '127.0.0.1 — the server only', server: true },
  { value: 'localhost', label: 'localhost — loopback', host: true, server: true },
  { value: '::1', label: '::1 — loopback (IPv6)', host: true, server: true },
  { value: '*', label: '* — all interfaces (reachable from the network)', host: true, server: true },
  { value: '0.0.0.0', label: '0.0.0.0 — all IPv4 interfaces', host: true, server: true },
]

const KIND_ICONS: Record<TunnelKind, typeof Waypoints> = {
  local: ArrowLeftRight,
  remote: ArrowDownUp,
  dynamic: Globe,
  rdynamic: Globe,
}

function Section({ title, aside, children, className }: { title: ReactNode; aside?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cn('grid content-start gap-2.5', className)}>
      <div className="flex min-h-7 items-center justify-between gap-2">
        <h3 className="text-sm font-semibold tracking-wide text-muted-foreground uppercase">{title}</h3>
        {aside}
      </div>
      {children}
    </section>
  )
}

function Note({ tone = 'info', children }: { tone?: 'info' | 'warning'; children: ReactNode }) {
  const Icon = tone === 'warning' ? ShieldAlert : Info
  return (
    <div
      className={cn(
        'flex items-start gap-2 rounded-md border px-2.5 py-2 text-sm',
        tone === 'warning' ? 'border-warning/40 bg-warning/8 text-foreground' : 'border-border bg-muted/40 text-muted-foreground',
      )}
    >
      <Icon className={cn('mt-0.5 size-3.5 shrink-0', tone === 'warning' ? 'text-warning' : 'text-muted-foreground')} />
      <div className="min-w-0">{children}</div>
    </div>
  )
}

export default function TunnelEditorDialog({ request, onClose }: { request: EditorRequest; onClose: () => void }) {
  const tunnels = useTunnels()
  const existing = request.id ? tunnels.data?.find((t) => t.id === request.id) : undefined
  const { data: conns } = useConnections()
  const runMode = useRunMode()
  const isAdmin = useIsAdmin()
  const restricted = runMode === 'server' && !isAdmin

  const [draft, setDraft] = useState<TunnelDraft>(() => initialDraft(request, existing))
  const initialJSON = useRef(JSON.stringify(draft))
  const [loadedId, setLoadedId] = useState<string | undefined>(existing?.id)
  const [createdId, setCreatedId] = useState<string | undefined>()
  const [focus, setFocus] = useState<DiagramFocus>(null)
  const [showErrors, setShowErrors] = useState(false)
  const [serverError, setServerError] = useState<string | null>(null)
  const [saving, setSaving] = useState<null | 'save' | 'start'>(null)
  const [advanced, setAdvanced] = useState(false)
  const [bindCheck, setBindCheck] = useState<(CheckBindResult & { key: string }) | null>(null)

  // Editing a tunnel whose list was not loaded yet: adopt it once it arrives.
  useEffect(() => {
    if (existing && loadedId !== existing.id) {
      const d = draftFromTunnel(existing)
      setDraft(d)
      initialJSON.current = JSON.stringify(d)
      setLoadedId(existing.id)
    }
  }, [existing, loadedId])

  const set = (patch: Partial<TunnelDraft>) => setDraft((d) => ({ ...d, ...patch }))
  const conn = conns?.find((c) => c.id === draft.connectionId)
  const kind = draft.kind
  const proxy = isProxyKind(kind)
  const hostListener = KINDS[kind].listenOn === 'host'
  const storedPw = !!existing?.secretKeys?.includes('socksPassword')
  const suggested = suggestName(draft, conn)
  const finalDraft = useMemo(() => ({ ...draft, name: draft.name.trim() || suggested }), [draft, suggested])
  const errors = validateDraft(finalDraft, storedPw)
  const err = (k: keyof typeof errors) => (showErrors ? errors[k] : undefined)
  const running = !!existing && isActive(existing.status)
  const exposed = !draft.useBindSocket && !isLoopbackHost(draft.bindHost)
  const route = connectionRoute(conn, conns).join(' → ')
  const docker = draftDockerHost(draft)

  // Pre-flight: is the listen address free on this machine?
  const bindKey = `${draft.bindHost}|${draft.bindPort}`
  useEffect(() => {
    if (!hostListener || draft.useBindSocket || !draft.bindPort) {
      setBindCheck(null)
      return
    }
    const ctrl = new AbortController()
    const timer = setTimeout(() => {
      checkBind({ bindHost: draft.bindHost, bindPort: draft.bindPort ?? 0, id: existing?.id ?? createdId }, ctrl.signal)
        .then((r) => setBindCheck({ ...r, key: bindKey }))
        .catch(() => setBindCheck(null))
    }, 350)
    return () => {
      clearTimeout(timer)
      ctrl.abort()
    }
  }, [hostListener, draft.useBindSocket, draft.bindHost, draft.bindPort, bindKey, existing?.id, createdId])

  const changeKind = (k: TunnelKind) => {
    setDraft((d) => {
      const next = { ...d, kind: k }
      if (isProxyKind(k) && d.bindPort == null) next.bindPort = 1080
      if (KINDS[k].listenOn !== KINDS[d.kind].listenOn) {
        next.onDemand = false
        if (d.useBindSocket) next.bindSocket = ''
      }
      return next
    })
  }

  const dirty = JSON.stringify(draft) !== initialJSON.current
  const requestClose = async () => {
    if (saving) return
    if (dirty && !(await confirm({ title: 'Discard changes?', description: 'The tunnel was not saved.', confirmLabel: 'Discard', destructive: true }))) return
    onClose()
  }

  const save = async (start: boolean) => {
    setShowErrors(true)
    if (Object.keys(errors).length) return
    // Network exposure is an explicit opt-in: confirm whenever a save makes the listener reachable from other
    // machines (a new exposed tunnel, or a changed listen address).
    const alreadyExposed =
      !!existing &&
      !existing.options.bindSocket &&
      !isLoopbackHost(existing.bindHost) &&
      existing.bindHost === draft.bindHost.trim() &&
      KINDS[kindOf(existing)].listenOn === KINDS[kind].listenOn
    if (draftExposed(finalDraft) && !alreadyExposed && !(await confirmExposure())) return
    setSaving(start ? 'start' : 'save')
    setServerError(null)
    const id = existing?.id ?? createdId
    try {
      const input = inputFromDraft(finalDraft, storedPw)
      let t = id ? await updateTunnel(id, input) : await createTunnel(input)
      putTunnel(t)
      if (!id) setCreatedId(t.id)
      if (start && !isActive(t.status)) {
        t = await startTunnel(t.id)
        putTunnel(t)
      }
      toast.success(id ? `Saved “${t.name}”` : `Created “${t.name}”`, {
        description: start || isActive(t.status) ? 'The tunnel is starting.' : undefined,
      })
      onClose()
    } catch (e) {
      if (isApiError(e) && e.status === 423) return
      setServerError(errorMessage(e))
    } finally {
      setSaving(null)
    }
  }

  const confirmExposure = () => {
    const via = conn ? ` through ${serverText}` : ''
    const listenLabel = draft.useBindSocket ? draft.bindSocket : draft.bindPort ? hostPort(draft.bindHost, draft.bindPort) : `${draft.bindHost} (port picked when it starts)`
    let what: string
    switch (kind) {
      case 'local':
        what = `Machines that can reach this computer on ${listenLabel} can connect to ${destText}${via} — no password is asked.`
        break
      case 'dynamic':
        what = draft.socksAuth
          ? `Machines that can reach this computer on ${listenLabel} can use the proxy (with its username and password) to reach any host${via}.`
          : `The allowed clients that can reach this computer on ${listenLabel} can use the proxy to reach any host${via}.`
        break
      case 'remote':
        what = `Machines on the SSH server’s network can connect to ${listenLabel} there and reach ${destText} from this computer (when the server’s GatewayPorts setting allows it).`
        break
      default:
        what = `Machines on the SSH server’s network can use the proxy on ${listenLabel} (with its username and password) to reach any host from this computer.`
    }
    return confirm({
      title: 'Make the tunnel reachable from other machines?',
      description: `${what} Keep a loopback address (127.0.0.1) unless other machines must use the tunnel.`,
      confirmLabel: 'Allow network access',
      destructive: true,
    })
  }

  // Submit one tick later so value commits made by the same key press (Enter in a field) are rendered first.
  const saveRef = useLatest(save)
  const submit = (start: boolean) => setTimeout(() => void saveRef.current(start), 0)

  const listenLabel = draft.useBindSocket ? draft.bindSocket || 'socket path' : hostPort(draft.bindHost || '127.0.0.1', draft.bindPort ?? 0)
  const destText = proxy
    ? 'any host:port'
    : draft.useDestSocket
      ? draft.destSocket || 'socket path'
      : hostPort(draft.destHost || 'localhost', draft.destPort ?? '?')
  const serverText = conn ? `${conn.username ? `${conn.username}@` : ''}${conn.host}` : 'choose a server'
  const listenWhere = hostListener ? 'this machine' : 'the SSH server'

  const presets = HOST_PRESETS.filter((p) => (hostListener ? p.host : p.server))
  const editingLabel = existing ? existing.name : 'New tunnel'

  return (
    <Dialog open onOpenChange={(o) => !o && void requestClose()}>
      <DialogContent
        size="2xl"
        className="max-h-[calc(100dvh-2rem)]"
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault()
            submit(true)
          }
        }}
      >
        <DialogHeader>
          <DialogTitle>
            <Waypoints className="size-4 text-primary" />
            {existing ? `Edit tunnel — ${editingLabel}` : 'New tunnel'}
          </DialogTitle>
          <DialogDescription>{KINDS[kind].description}</DialogDescription>
        </DialogHeader>

        <DialogBody className="@container">
          <form
            id="tunnel-editor-form"
            className="grid gap-4 pb-1"
            onSubmit={(e) => {
              e.preventDefault()
              submit(!existing && !!request.initial?.start)
            }}
          >
            <SegmentedControl<TunnelKind>
              aria-label="Tunnel type"
              fullWidth
              value={kind}
              onValueChange={changeKind}
              options={KIND_ORDER.map((k) => ({
                value: k,
                icon: KIND_ICONS[k],
                label: `${KINDS[k].label} (${KINDS[k].flag})`,
                disabled: restricted && KINDS[k].listenOn === 'server',
                title: restricted && KINDS[k].listenOn === 'server' ? 'Reserved for administrators in server mode' : KINDS[k].description,
              }))}
            />

            <div className="rounded-lg border bg-muted/25 px-2 py-1.5">
              <FlowDiagram kind={kind} listen={listenLabel} dest={destText} server={serverText} via={route || undefined} focus={focus} exposed={exposed} />
            </div>

            <div className="grid gap-x-5 gap-y-3 @xl:grid-cols-2">
              <Field label="Name" error={err('name')} hint={!draft.name.trim() ? `Default: ${suggested}` : undefined}>
                <Input
                  value={draft.name}
                  placeholder={suggested}
                  maxLength={200}
                  autoFocus={!existing}
                  onChange={(e) => set({ name: e.target.value })}
                />
              </Field>
              <div onFocusCapture={() => setFocus('server')} onBlurCapture={() => setFocus(null)}>
                <Field
                  label="SSH server"
                  required
                  error={err('connectionId')}
                  hint={route ? `Reached through ${route} (from the session’s settings).` : undefined}
                  labelAside={
                    isCommandEnabled('sessions.new') ? (
                      <button
                        type="button"
                        className="flex items-center gap-1 text-xs text-primary hover:underline"
                        onClick={() => void runCommand('sessions.new', { protocol: 'ssh' })}
                      >
                        <Plus className="size-3" /> New SSH session
                      </button>
                    ) : undefined
                  }
                >
                  <ConnectionPicker value={draft.connectionId} invalid={!!err('connectionId')} onChange={(id) => set({ connectionId: id })} />
                </Field>
              </div>
            </div>

            <div className="grid gap-x-5 gap-y-4 @xl:grid-cols-2">
              <Section
                title={proxy ? `Proxy on ${listenWhere}` : `Listen on ${listenWhere}`}
                aside={
                  <SegmentedControl<'tcp' | 'unix'>
                    size="sm"
                    aria-label="Listener type"
                    value={draft.useBindSocket ? 'unix' : 'tcp'}
                    onValueChange={(v) => set({ useBindSocket: v === 'unix' })}
                    options={[
                      { value: 'tcp', label: 'TCP' },
                      { value: 'unix', label: <span className="whitespace-nowrap">Unix socket</span>, title: 'Unix socket', disabled: restricted && hostListener },
                    ]}
                  />
                }
              >
                <div className="grid gap-2.5" onFocusCapture={() => setFocus('listen')} onBlurCapture={() => setFocus(null)}>
                  {draft.useBindSocket ? (
                    <Field
                      label="Socket path"
                      error={err('bindSocket')}
                      hint={hostListener ? 'Created on this machine, readable by the AstraTerm user only.' : 'Created on the SSH server (streamlocal forwarding).'}
                    >
                      <Input
                        className="font-mono"
                        value={draft.bindSocket}
                        placeholder={hostListener ? '/tmp/app.sock' : '/tmp/remote.sock'}
                        onChange={(e) => set({ bindSocket: e.target.value })}
                      />
                    </Field>
                  ) : (
                    <div className="grid grid-cols-[1fr_7.5rem] gap-2">
                      <Field label="Address" error={err('bindHost')}>
                        <Input
                          className="font-mono"
                          value={draft.bindHost}
                          placeholder="127.0.0.1"
                          onChange={(e) => set({ bindHost: e.target.value })}
                          trailing={
                            <DropdownMenu>
                              <DropdownMenuTrigger asChild>
                                <button
                                  type="button"
                                  aria-label="Address presets"
                                  className="flex size-6 items-center justify-center rounded-sm hover:bg-accent hover:text-foreground"
                                >
                                  <ChevronDown className="size-3.5" />
                                </button>
                              </DropdownMenuTrigger>
                              <DropdownMenuContent align="end">
                                {presets.map((p) => (
                                  <DropdownMenuItem
                                    key={`${p.value}-${p.label}`}
                                    disabled={restricted && !isLoopbackHost(p.value)}
                                    onSelect={() => set({ bindHost: p.value })}
                                  >
                                    <span className="font-mono">{p.value}</span>
                                    <span className="ml-2 text-xs text-muted-foreground">{p.label.split(' — ')[1]}</span>
                                  </DropdownMenuItem>
                                ))}
                              </DropdownMenuContent>
                            </DropdownMenu>
                          }
                        />
                      </Field>
                      <Field label="Port" error={err('bindPort')}>
                        <PortInput value={draft.bindPort} placeholder="auto" onChange={(v) => set({ bindPort: v })} />
                      </Field>
                    </div>
                  )}
                  {hostListener && !draft.useBindSocket && draft.bindPort ? <BindStatus check={bindCheck} current={bindKey} onUse={(p) => set({ bindPort: p })} /> : null}
                  {!draft.useBindSocket && !draft.bindPort && (
                    <p className="text-sm text-muted-foreground">
                      {hostListener ? 'A free port is picked when the tunnel starts.' : 'The SSH server allocates a free port when the tunnel starts.'}
                    </p>
                  )}
                  {exposed && (
                    <Note tone="warning">
                      {hostListener
                        ? `Anyone who can reach this machine on ${listenLabel} can use the tunnel${proxy ? '' : ` to reach ${destText}`}.`
                        : 'Clients on the server’s network can connect (the server needs GatewayPorts yes/clientspecified).'}
                    </Note>
                  )}
                </div>
              </Section>

              {proxy ? (
                <Section title="Proxy authentication">
                  <div className="grid gap-2.5" onFocusCapture={() => setFocus('dest')} onBlurCapture={() => setFocus(null)}>
                    <label className="flex items-center justify-between gap-3 text-base">
                      <span>Require a username and password</span>
                      <Switch checked={draft.socksAuth} onCheckedChange={(v) => set({ socksAuth: v })} aria-label="Require a username and password" />
                    </label>
                    {draft.socksAuth ? (
                      <div className="grid grid-cols-2 gap-2">
                        <Field label="Username" error={err('socksUsername')}>
                          <Input value={draft.socksUsername} autoComplete="off" onChange={(e) => set({ socksUsername: e.target.value })} />
                        </Field>
                        <Field label="Password" error={err('socksPassword')} hint={storedPw && !draft.socksPassword ? 'Stored — type to replace' : undefined}>
                          <PasswordInput
                            value={draft.socksPassword}
                            autoComplete="new-password"
                            placeholder={storedPw ? '••••••••' : ''}
                            generate
                            onChange={(e) => set({ socksPassword: e.target.value })}
                          />
                        </Field>
                      </div>
                    ) : (
                      err('socksUsername') && <p className="text-sm text-destructive">{err('socksUsername')}</p>
                    )}
                    <p className="text-sm text-muted-foreground">
                      SOCKS5 (also SOCKS4/4a without authentication)
                      {draft.httpProxy ? ', HTTP CONNECT and plain HTTP proxying on the same port, plus a PAC file at /proxy.pac.' : '.'} Host
                      names are resolved {kind === 'dynamic' ? 'by the SSH server' : 'on this machine'}.
                    </p>
                  </div>
                </Section>
              ) : (
                <Section
                  title="Destination"
                  aside={
                    <div className="flex items-center gap-1">
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button type="button" variant="ghost" size="icon-xs" aria-label="Destination presets" title="Presets: Docker socket, databases, web…">
                            <WandSparkles />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end" className="min-w-64">
                          {DEST_PRESETS.map((g, gi) => (
                            <div key={g.group}>
                              {gi > 0 && <DropdownMenuSeparator />}
                              <DropdownMenuLabel>{g.group}</DropdownMenuLabel>
                              {g.items.map((p) => (
                                <DropdownMenuItem key={p.id} onSelect={() => setDraft((d) => applyDestPreset(d, p))}>
                                  <TunnelIcon name={p.icon} />
                                  <span>{p.label}</span>
                                  <span className="ml-auto pl-3 font-mono text-xs text-muted-foreground">{p.socket ?? `${p.host}:${p.port}`}</span>
                                </DropdownMenuItem>
                              ))}
                            </div>
                          ))}
                        </DropdownMenuContent>
                      </DropdownMenu>
                      <SegmentedControl<'tcp' | 'unix'>
                        size="sm"
                        aria-label="Destination type"
                        value={draft.useDestSocket ? 'unix' : 'tcp'}
                        onValueChange={(v) => set({ useDestSocket: v === 'unix' })}
                        options={[
                          { value: 'tcp', label: 'TCP' },
                          { value: 'unix', label: <span className="whitespace-nowrap">Unix socket</span> },
                        ]}
                      />
                    </div>
                  }
                >
                  <div className="grid gap-2.5" onFocusCapture={() => setFocus('dest')} onBlurCapture={() => setFocus(null)}>
                    {draft.useDestSocket ? (
                      <Field
                        label="Socket path"
                        error={err('destSocket')}
                        hint={kind === 'local' ? 'e.g. /var/run/docker.sock or /run/postgresql/.s.PGSQL.5432 on the server' : 'A socket on this machine'}
                      >
                        <Input
                          className="font-mono"
                          value={draft.destSocket}
                          placeholder="/var/run/docker.sock"
                          onChange={(e) => set({ destSocket: e.target.value })}
                        />
                      </Field>
                    ) : (
                      <div className="grid grid-cols-[1fr_7.5rem] gap-2">
                        <Field label="Host" error={err('destHost')}>
                          <Input className="font-mono" value={draft.destHost} placeholder="localhost" onChange={(e) => set({ destHost: e.target.value })} />
                        </Field>
                        <Field label="Port" error={err('destPort')}>
                          <PortInput value={draft.destPort} min={1} placeholder="80" onChange={(v) => set({ destPort: v })} />
                        </Field>
                      </div>
                    )}
                    {docker ? (
                      <DockerHostNote hint={docker} />
                    ) : (
                      <p className="text-sm text-muted-foreground">
                        {kind === 'local'
                          ? '“localhost” is the SSH server itself; other hosts must be reachable from it.'
                          : '“localhost” is this machine (the one running AstraTerm).'}
                      </p>
                    )}
                  </div>
                </Section>
              )}
            </div>

            <div className="grid gap-2 rounded-lg border px-3 py-2.5">
              <div className="flex flex-wrap items-center gap-x-6 gap-y-2">
                <label className="flex items-center gap-2 text-base">
                  <Switch checked={draft.autoStart} onCheckedChange={(v) => set({ autoStart: v })} aria-label="Start with AstraTerm" />
                  Start with AstraTerm
                </label>
                <label className="flex items-center gap-2 text-base">
                  <Switch checked={draft.autoReconnect} onCheckedChange={(v) => set({ autoReconnect: v })} aria-label="Reconnect automatically" />
                  Reconnect automatically
                </label>
                {hostListener && (
                  <label className="flex items-center gap-2 text-base" title="The listener opens right away; SSH connects when the first client arrives.">
                    <Switch checked={draft.onDemand} onCheckedChange={(v) => set({ onDemand: v })} aria-label="Connect on demand" />
                    Connect on demand
                  </label>
                )}
                <button
                  type="button"
                  className="ml-auto flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
                  aria-expanded={advanced}
                  onClick={() => setAdvanced((a) => !a)}
                >
                  {advanced ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
                  Advanced
                </button>
              </div>

              {advanced && (
                <div className="grid gap-x-5 gap-y-3 border-t pt-3 @xl:grid-cols-2">
                  {hostListener && draft.onDemand && (
                    <Field label="Disconnect when idle for" hint="Seconds without any connection (minimum 10).">
                      <NumberInput value={draft.idleTimeoutSec} min={10} max={86400} unit="s" onChange={(v) => set({ idleTimeoutSec: v })} />
                    </Field>
                  )}
                  <Field label="Maximum concurrent connections" hint="Empty = 1024.">
                    <NumberInput value={draft.maxConns} allowEmpty min={1} max={65535} placeholder="1024" onChange={(v) => set({ maxConns: v })} />
                  </Field>
                  {hostListener && (
                    <Field label="Allowed clients" error={err('allowFrom')} hint="IP addresses or CIDR ranges; empty = anyone who can reach the listener.">
                      <TagInput value={draft.allowFrom} onChange={(allowFrom) => set({ allowFrom })} placeholder="192.168.1.0/24" />
                    </Field>
                  )}
                  {proxy && (
                    <label className="flex items-center justify-between gap-3 text-base @xl:col-span-2">
                      <span>
                        HTTP proxy and PAC file on the same port
                        <span className="block text-sm text-muted-foreground">Browsers can use it directly as an HTTP proxy.</span>
                      </span>
                      <Switch checked={draft.httpProxy} onCheckedChange={(v) => set({ httpProxy: v })} aria-label="HTTP proxy on the same port" />
                    </label>
                  )}
                  {kind === 'local' && !draft.useDestSocket && (
                    <Field label="Open in browser as" hint="Auto-detected from the destination port.">
                      <SimpleSelect<'auto' | 'http' | 'https'>
                        aria-label="Open in browser as"
                        value={draft.scheme || 'auto'}
                        onValueChange={(v) => set({ scheme: v === 'auto' ? '' : v })}
                        options={[
                          { value: 'auto', label: 'Automatic' },
                          { value: 'http', label: 'http://' },
                          { value: 'https', label: 'https://' },
                        ]}
                      />
                    </Field>
                  )}
                  <Field label="Colour">
                    <ColorSwatchPicker size="sm" allowNone value={draft.color || null} onChange={(c) => set({ color: c ?? '' })} />
                  </Field>
                  <Field label="Icon" className="@xl:col-span-2">
                    <IconPicker value={draft.icon} onChange={(icon) => set({ icon })} />
                  </Field>
                  <Field label="Notes" className="@xl:col-span-2">
                    <Textarea rows={2} value={draft.notes} maxLength={4096} onChange={(e) => set({ notes: e.target.value })} />
                  </Field>
                </div>
              )}
            </div>

            {runMode === 'server' && (
              <Note>
                AstraTerm runs in server mode: listeners open on the AstraTerm server, not on your computer.
                {restricted ? ' Remote forwarding, non-loopback addresses and ports below 1024 are reserved for administrators.' : ''} Use
                “Open in browser” to reach web services through AstraTerm.
              </Note>
            )}
            {serverError && (
              <div role="alert" className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/8 px-2.5 py-2 text-sm">
                <CircleX className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                <span className="min-w-0 break-words">{serverError}</span>
              </div>
            )}
          </form>
        </DialogBody>

        <DialogFooter className="items-center">
          <div className="mr-auto hidden items-center gap-2 text-xs text-muted-foreground sm:flex">
            <KindBadge kind={kind} />
            <span>
              <Kbd keys={isMac ? 'Meta+Enter' : 'Control+Enter'} /> save &amp; start
            </span>
          </div>
          <Button variant="secondary" onClick={() => void requestClose()} disabled={!!saving}>
            Cancel
          </Button>
          <Button variant="secondary" type="submit" form="tunnel-editor-form" loading={saving === 'save'} disabled={!!saving}>
            {running ? 'Save & restart' : 'Save'}
          </Button>
          {!running && (
            <Button onClick={() => submit(true)} loading={saving === 'start'} disabled={!!saving}>
              Save &amp; start
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function BindStatus({ check, current, onUse }: { check: (CheckBindResult & { key: string }) | null; current: string; onUse: (port: number) => void }) {
  if (!check || check.key !== current) return <p className="h-5 text-sm text-muted-foreground">Checking the port…</p>
  if (check.available) {
    return (
      <p className="flex h-5 items-center gap-1.5 text-sm text-success">
        <CircleCheck className="size-3.5" /> Available on this machine
      </p>
    )
  }
  return (
    <p className="flex min-h-5 flex-wrap items-center gap-1.5 text-sm text-destructive">
      <CircleX className="size-3.5 shrink-0" /> {check.error || 'Not available'}
      {check.suggestion ? (
        <Button type="button" size="xs" variant="outline" className="ml-1" onClick={() => onUse(check.suggestion!)}>
          Use {check.suggestion}
        </Button>
      ) : null}
    </p>
  )
}

/** "Point Docker at the tunnel" hint for forwards to a Docker socket (TUN-6). */
function DockerHostNote({ hint }: { hint: DockerHostHint }) {
  const text = `DOCKER_HOST=${hint.value ?? 'tcp://127.0.0.1:<port>'}`
  return (
    <div className="grid gap-1 rounded-md border bg-muted/40 px-2.5 py-2 text-sm text-muted-foreground">
      <span>Point Docker {hint.where === 'here' ? 'on this machine' : 'on the SSH server'} at the tunnel:</span>
      <div className="flex min-w-0 items-center gap-1">
        <code className="min-w-0 flex-1 truncate font-mono text-foreground" title={text}>
          {text}
        </code>
        {hint.value && (
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            aria-label="Copy DOCKER_HOST"
            title="Copy"
            onClick={() => void copyText(text).then((ok) => (ok ? toast.success('Copied', { description: text }) : toast.error('Could not copy to the clipboard')))}
          >
            <Copy />
          </Button>
        )}
      </div>
      {!hint.value && <span>The port is assigned when the tunnel starts.</span>}
    </div>
  )
}
