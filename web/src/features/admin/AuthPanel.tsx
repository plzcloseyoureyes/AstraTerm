/*
 * Authentication settings (SEC-20, MU-5, MU-6, MU-7): login policy (passwords, brute force, account lockout,
 * sessions, required second factor, SSO-only, allowed networks), single sign-on providers and the passkey relying
 * party. Edits are drafts with a sticky save bar; nothing is sent until "Save".
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Fingerprint, Globe, Link2, Pencil, Plus, RotateCcw, Save, ShieldCheck, Trash2, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { isApiError } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { NumberInput } from '@/components/ui/number-input'
import { ErrorState, QueryState } from '@/components/ui/query-state'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { LoadingPane } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { Input } from '@/components/ui/input'
import { DELAY_PRESETS, useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, errorMessage } from '@/lib/utils'
import { refreshAuth } from '@/stores/auth'
import { deleteOidc, putPasskeyConfig, putPolicy, secQK, testOidc, updateOidc, useOidc, usePasskeyConfig, usePolicy } from '@/features/security/api'
import { ReauthCancelled } from '@/features/security/store'
import { CopyButton, Notice, PageHeader, Section } from '@/features/security/components'
import type { LoginPolicy, OidcProvider, RequireMfa } from '@/features/security/types'
import { ProviderDialog } from './ProviderDialog'

export function AuthPanel() {
  return (
    <>
      <PageHeader title="Authentication" description="How people sign in: password rules, brute-force protection, sessions, two-factor and single sign-on." />
      <div className="grid gap-10">
        <PolicyEditor />
        <SsoProviders />
        <PasskeyRelyingParty />
      </div>
    </>
  )
}

// --- login policy ----------------------------------------------------------------------------------------------------------

function Row({ label, description, children, className }: { label: ReactNode; description?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <div className={cn('flex flex-col gap-x-6 gap-y-2 px-4 py-3 @xl:flex-row @xl:items-center @xl:justify-between', className)}>
      <div className="grid min-w-0 gap-0.5">
        <span className="text-base font-medium">{label}</span>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      <div className="flex shrink-0 items-center gap-2">{children}</div>
    </div>
  )
}

function Num({ value, onChange, min, max, suffix, label }: { value: number; onChange: (n: number) => void; min: number; max: number; suffix?: string; label: string }) {
  return (
    <div className="flex items-center gap-2">
      <NumberInput value={value} onChange={(v) => onChange(v ?? min)} min={min} max={max} inputSize="sm" className="w-24" aria-label={label} />
      {suffix && <span className="w-16 text-sm text-muted-foreground">{suffix}</span>}
    </div>
  )
}

function PolicyEditor() {
  const policy = usePolicy()
  const qc = useQueryClient()
  const [draft, setDraft] = useState<LoginPolicy | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (policy.data && !draft) setDraft(strip(policy.data))
  }, [policy.data, draft])
  const saved = policy.data ? strip(policy.data) : null
  const dirty = useMemo(() => !!draft && !!saved && JSON.stringify(draft) !== JSON.stringify(saved), [draft, saved])
  const gate = useLoadingGate(!draft && !policy.isError, DELAY_PRESETS.NAVIGATION)

  if (policy.isError && !draft) return <ErrorState error={policy.error} title="Could not load the login policy" onRetry={() => void policy.refetch()} />
  if (gate.hold || !draft) return <LoadingPane active={gate.show} immediate />
  const set = <K extends keyof LoginPolicy>(k: K, v: LoginPolicy[K]) => setDraft((d) => (d ? { ...d, [k]: v } : d))
  const defaults = policy.data!.defaults

  const save = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await putPolicy(draft)
      qc.setQueryData(secQK.policy, res)
      setDraft(strip(res))
      void refreshAuth() // password policy / login options on the state endpoint
      toast.success('Login policy saved')
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      setError(isApiError(err) && err.code === 'self_lockout' ? err.message : errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="grid gap-6">
      <Section title="Passwords" description="Applied when passwords are set or changed (existing passwords keep working).">
        <div className="divide-y rounded-lg border bg-card">
          <Row label="Minimum length">
            <Num label="Minimum length" value={draft.passwordMinLength} onChange={(n) => set('passwordMinLength', n)} min={8} max={128} suffix="characters" />
          </Row>
          <Row label="Character variety" description="How many of lowercase, uppercase, digits and symbols a password must mix.">
            <SegmentedControl
              size="sm"
              value={String(draft.passwordRequireClasses)}
              onValueChange={(v) => set('passwordRequireClasses', Number(v))}
              aria-label="Character variety"
              options={['0', '2', '3', '4'].map((v) => ({ value: v, label: v === '0' ? 'Any' : `${v} kinds` }))}
            />
          </Row>
          <Row label="Reject passwords containing the user name">
            <Switch checked={draft.passwordDisallowUsername} onCheckedChange={(v) => set('passwordDisallowUsername', v)} aria-label="Reject passwords containing the user name" />
          </Row>
        </div>
      </Section>

      <Section title="Brute-force protection" description="Slows down and stops password guessing.">
        <div className="divide-y rounded-lg border bg-card">
          <Row label="Back off after" description="Failed attempts from one address for one user before each further try waits (1 s, doubling).">
            <Num label="Back off after" value={draft.lockoutThreshold} onChange={(n) => set('lockoutThreshold', n)} min={1} max={100} suffix="failures" />
          </Row>
          <Row label="Longest wait">
            <Num label="Longest wait" value={draft.lockoutMaxMinutes} onChange={(n) => set('lockoutMaxMinutes', n)} min={1} max={1440} suffix="minutes" />
          </Row>
          <Row label="Lock the account after" description="Consecutive failures from any address. 0 = never lock. Admins can unlock from Users.">
            <Num label="Lock the account after" value={draft.accountLockThreshold} onChange={(n) => set('accountLockThreshold', n)} min={0} max={100} suffix="failures" />
          </Row>
          {draft.accountLockThreshold > 0 && (
            <Row label="Lock for">
              <Num label="Lock for" value={draft.accountLockMinutes} onChange={(n) => set('accountLockMinutes', n)} min={1} max={10080} suffix="minutes" />
            </Row>
          )}
        </div>
      </Section>

      <Section title="Sessions" description="How long a browser stays signed in.">
        <div className="divide-y rounded-lg border bg-card">
          <Row label="Sign out after inactivity" description="For sign-ins without “Keep me signed in”.">
            <Num label="Sign out after inactivity" value={draft.sessionIdleHours} onChange={(n) => set('sessionIdleHours', n)} min={1} max={8760} suffix="hours" />
          </Row>
          <Row label="“Keep me signed in” lasts" description="0 hides the option on the sign-in screen.">
            <Num label="Keep me signed in lasts" value={draft.rememberDays} onChange={(n) => set('rememberDays', n)} min={0} max={365} suffix="days" />
          </Row>
          <Row label="Maximum session age" description="Signs everyone out after this long, active or not. 0 = no limit.">
            <Num label="Maximum session age" value={draft.sessionMaxDays} onChange={(n) => set('sessionMaxDays', n)} min={0} max={365} suffix="days" />
          </Row>
        </div>
      </Section>

      <Section title="Sign-in methods">
        <div className="divide-y rounded-lg border bg-card">
          <Row label="Require two-factor authentication" description="Accounts without a second factor must set up an authenticator app when they sign in with their password.">
            <SegmentedControl<RequireMfa>
              size="sm"
              value={draft.requireMfa}
              onValueChange={(v) => set('requireMfa', v)}
              aria-label="Require two-factor authentication"
              options={[
                { value: 'off', label: 'Off' },
                { value: 'admins', label: 'Admins' },
                { value: 'all', label: 'Everyone' },
              ]}
            />
          </Row>
          <Row label="Password sign-in for users" description="Off = users sign in with single sign-on or passkeys only. Administrators keep password sign-in as a fallback.">
            <Switch checked={draft.passwordLogin} onCheckedChange={(v) => set('passwordLogin', v)} aria-label="Password sign-in for users" />
          </Row>
          <div className="grid gap-2 px-4 py-3">
            <div className="grid gap-0.5">
              <span className="text-base font-medium">Allowed networks</span>
              <p className="text-sm text-muted-foreground">Only these addresses may sign in (IPs or CIDRs, e.g. 10.0.0.0/8). Empty = anywhere. The Termstead host itself is always allowed.</p>
            </div>
            <TagInput value={draft.allowedNetworks} onChange={(v) => set('allowedNetworks', v)} placeholder="Add an address or network…" aria-label="Allowed networks" />
          </div>
        </div>
      </Section>

      {error && (
        <Notice tone="destructive" icon={TriangleAlert}>
          {error}
        </Notice>
      )}
      <div
        className={cn(
          'sticky bottom-3 z-10 flex items-center gap-3 rounded-lg border bg-popover/95 px-4 py-2 shadow-popover backdrop-blur transition-all duration-200',
          dirty ? 'translate-y-0 opacity-100' : 'pointer-events-none translate-y-2 opacity-0',
        )}
        aria-hidden={!dirty}
      >
        <span className="flex-1 text-sm">You have unsaved changes to the login policy.</span>
        <Button size="sm" variant="ghost" onClick={() => setDraft(saved)} disabled={busy} tabIndex={dirty ? 0 : -1}>
          <RotateCcw /> Discard
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setDraft(strip({ ...defaults, defaults } as never))} disabled={busy} tabIndex={dirty ? 0 : -1}>
          Defaults
        </Button>
        <Button size="sm" onClick={() => void save()} loading={busy} tabIndex={dirty ? 0 : -1}>
          <Save /> Save policy
        </Button>
      </div>
    </div>
  )
}

function strip(p: LoginPolicy & { defaults?: LoginPolicy }): LoginPolicy {
  const { defaults: _d, ...rest } = p
  void _d
  return { ...rest, allowedNetworks: [...(rest.allowedNetworks ?? [])] }
}

// --- single sign-on ---------------------------------------------------------------------------------------------------------

function SsoProviders() {
  const oidc = useOidc()
  const qc = useQueryClient()
  const [editing, setEditing] = useState<OidcProvider | 'new' | null>(null)
  const [testing, setTesting] = useState<string | null>(null)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: secQK.oidc })
    void refreshAuth()
  }

  const test = async (p: OidcProvider) => {
    setTesting(p.id)
    try {
      const r = await testOidc({ id: p.id })
      if (r.ok) toast.success(`${p.name}: discovery OK (${r.latencyMs} ms)`, { description: `Issuer ${r.issuer}${r.pkceMethods?.includes('S256') ? ' · PKCE S256 supported' : ''}` })
      else toast.error(`${p.name}: cannot reach the provider`, { description: r.error })
    } catch (err) {
      toast.error(errorMessage(err))
    } finally {
      setTesting(null)
    }
  }

  const toggle = async (p: OidcProvider, enabled: boolean) => {
    qc.setQueryData(secQK.oidc, (old: typeof oidc.data) => old && { ...old, providers: old.providers.map((x) => (x.id === p.id ? { ...x, enabled } : x)) })
    try {
      await updateOidc(p.id, { enabled })
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      toast.error(errorMessage(err))
    } finally {
      refresh()
    }
  }

  const remove = async (p: OidcProvider) => {
    const ok = await confirm({
      title: `Delete ${p.name}?`,
      description: 'It disappears from the sign-in screen and every account link to it is removed. Accounts it created stay (they can still use a password or passkey).',
      confirmLabel: 'Delete',
      destructive: true,
    })
    if (!ok) return
    try {
      await deleteOidc(p.id)
      toast.success(`${p.name} deleted`)
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      toast.error(errorMessage(err))
    } finally {
      refresh()
    }
  }

  return (
    <Section
      title="Single sign-on (OpenID Connect)"
      description="Let people sign in with Okta, Microsoft Entra ID, Google Workspace, Keycloak, Authentik, GitLab…"
      actions={
        <Button size="sm" onClick={() => setEditing('new')}>
          <Plus /> Add provider
        </Button>
      }
    >
      <QueryState query={oidc} errorTitle="Could not load the single sign-on providers">
        {(data) => (
          <div className="grid gap-3">
            <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/30 px-3 py-2 text-sm">
              <span className="text-muted-foreground">Redirect URI to register at the provider:</span>
              <code className="min-w-0 truncate font-mono text-xs">{data.redirectUri}</code>
              <CopyButton text={data.redirectUri} size="xs" variant="ghost" />
            </div>
            {data.providers.length === 0 ? (
              <div className="rounded-lg border border-dashed px-4 py-6 text-center text-sm text-muted-foreground">
                No providers yet. Add one and a “Continue with …” button appears on the sign-in screen.
              </div>
            ) : (
              <ul className="divide-y rounded-lg border bg-card">
                {data.providers.map((p) => (
                  <li key={p.id} className="flex flex-wrap items-center gap-3 px-4 py-3">
                    <Link2 className="size-4 shrink-0 text-muted-foreground" />
                    <div className="grid min-w-0 flex-1 gap-0.5">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium">{p.name}</span>
                        {!p.hasClientSecret && <Badge variant="outline">Public client</Badge>}
                        {p.autoProvision && <Badge variant="secondary">Creates accounts</Badge>}
                        {p.adminGroups.length > 0 && <Badge variant="secondary">Group → admin</Badge>}
                      </div>
                      <span className="truncate text-sm text-muted-foreground">{p.issuer}</span>
                    </div>
                    <Button size="sm" variant="ghost" onClick={() => void test(p)} loading={testing === p.id}>
                      <Globe /> Test
                    </Button>
                    <Button size="icon-sm" variant="ghost" aria-label={`Edit ${p.name}`} onClick={() => setEditing(p)}>
                      <Pencil />
                    </Button>
                    <Button size="icon-sm" variant="ghost" aria-label={`Delete ${p.name}`} className="hover:text-destructive" onClick={() => void remove(p)}>
                      <Trash2 />
                    </Button>
                    <Switch checked={p.enabled} onCheckedChange={(v) => void toggle(p, v)} aria-label={`Enable ${p.name}`} />
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </QueryState>
      <ProviderDialog value={editing} redirectUri={oidc.data?.redirectUri ?? ''} onClose={() => setEditing(null)} onSaved={refresh} />
    </Section>
  )
}

// --- passkeys relying party ---------------------------------------------------------------------------------------------------

function PasskeyRelyingParty() {
  const cfg = usePasskeyConfig()
  const qc = useQueryClient()
  const [rpId, setRpId] = useState('')
  const [origins, setOrigins] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (cfg.data) {
      setRpId(cfg.data.rpId)
      setOrigins(cfg.data.origins)
    }
  }, [cfg.data])
  const gate = useLoadingGate(cfg.isPending, DELAY_PRESETS.NAVIGATION)
  if (gate.hold || cfg.isPending) return <LoadingPane active={gate.show} immediate />
  if (cfg.isError) return <ErrorState error={cfg.error} title="Could not load the passkey settings" onRetry={() => void cfg.refetch()} />
  const d = cfg.data
  const dirty = rpId !== d.rpId || JSON.stringify(origins) !== JSON.stringify(d.origins)
  const save = async () => {
    setBusy(true)
    try {
      const res = await putPasskeyConfig({ rpId: rpId.trim(), origins })
      qc.setQueryData(secQK.passkeyConfig, res)
      void qc.invalidateQueries({ queryKey: secQK.passkeyStatus })
      toast.success(res.rpId ? `Passkeys pinned to ${res.rpId}` : 'Passkeys follow the address in use')
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      toast.error('Could not save', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <Fingerprint className="size-4" /> Passkey domain
        </span>
      }
      description="Passkeys are bound to a domain (relying party). By default Termstead uses the host name of each request; pin it when Termstead is reachable under several names."
    >
      <div className="grid gap-4 rounded-lg border bg-card p-4">
        <div className="grid gap-1.5">
          <label className="text-sm font-medium" htmlFor="rp-id">
            Domain (RP ID)
          </label>
          <Input id="rp-id" value={rpId} onChange={(e) => setRpId(e.target.value)} placeholder={`Automatic (now: ${d.requestHost})`} className="max-w-sm" />
        </div>
        {rpId.trim() && (
          <div className="grid gap-1.5">
            <span className="text-sm font-medium">Allowed origins</span>
            <TagInput value={origins} onChange={setOrigins} placeholder={d.requestOrigin} aria-label="Allowed origins" />
            <span className="text-xs text-muted-foreground">Must include the address you are using ({d.requestOrigin}).</span>
          </div>
        )}
        <div className="flex items-center gap-2">
          <Button size="sm" onClick={() => void save()} disabled={!dirty} loading={busy}>
            <ShieldCheck /> Save
          </Button>
          {dirty && (
            <Button size="sm" variant="ghost" onClick={() => (setRpId(d.rpId), setOrigins(d.origins))}>
              Discard
            </Button>
          )}
        </div>
      </div>
    </Section>
  )
}
