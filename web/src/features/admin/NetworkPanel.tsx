/*
 * Network policy (SEC-7, owned by the netguard module's REST API): which destinations NexTerm may connect to on
 * behalf of users, with a dry-run tester. Rendered only when the endpoints exist.
 */
import { useEffect, useMemo, useState } from 'react'
import { CircleCheck, CircleX, Info, RotateCcw, Save, ShieldAlert } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { ErrorState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { SwitchField } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { DELAY_PRESETS, useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, errorMessage } from '@/lib/utils'
import { putNetworkPolicy, secQK, testNetworkPolicy, useAdminUsers, useNetworkPolicy } from '@/features/security/api'
import { Notice, PageHeader, Section } from '@/features/security/components'
import type { NetworkPolicy, NetworkTestResult } from '@/features/security/types'

export function NetworkPanel() {
  const view = useNetworkPolicy(true)
  const qc = useQueryClient()
  const users = useAdminUsers()
  const [draft, setDraft] = useState<NetworkPolicy | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [host, setHost] = useState('')
  const [port, setPort] = useState<number | null>(22)
  const [asUser, setAsUser] = useState('')
  const [result, setResult] = useState<NetworkTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  useEffect(() => {
    if (view.data && !draft) setDraft(view.data.policy)
  }, [view.data, draft])
  const dirty = useMemo(() => !!draft && !!view.data && JSON.stringify(draft) !== JSON.stringify(view.data.policy), [draft, view.data])
  const gate = useLoadingGate(view.isPending || (!draft && !view.isError), DELAY_PRESETS.NAVIGATION)

  if (gate.hold || view.isPending || (!draft && !view.isError)) return <LoadingPane active={gate.show} immediate />
  if (view.isError || !draft) return <ErrorState error={view.error} title="Could not load the network policy" onRetry={() => void view.refetch()} />
  const v = view.data!
  const set = <K extends keyof NetworkPolicy>(k: K, val: NetworkPolicy[K]) => setDraft((d) => (d ? { ...d, [k]: val } : d))

  const save = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await putNetworkPolicy(draft)
      qc.setQueryData(secQK.netPolicy, res)
      setDraft(res.policy)
      toast.success('Network policy saved')
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const test = async () => {
    if (!host.trim() || !port) return
    setTesting(true)
    try {
      setResult(await testNetworkPolicy({ host: host.trim(), port, userId: asUser || undefined, policy: draft }))
    } catch (err) {
      setResult({ host, port, decision: 'error', allowed: false, restricted: true, reason: errorMessage(err), addresses: [] })
    } finally {
      setTesting(false)
    }
  }

  return (
    <>
      <PageHeader title="Network policy" description="Which destinations NexTerm connects to on behalf of users (SSH, RDP, VNC, file transfers, tunnels, tools)." />
      <div className="grid gap-6">
        {!v.enforced && (
          <Notice icon={Info}>
            Desktop mode: the policy is not enforced (you are the only user). It applies once NexTerm runs in server mode.
          </Notice>
        )}
        <Section title="Destinations">
          <div className="grid gap-4 rounded-lg border bg-card p-4">
            <SwitchField
              label="Allow private networks"
              description={`RFC 1918 / ULA ranges (${v.privateRanges.slice(0, 3).join(', ')}…) — the usual bastion setup.`}
              checked={draft.allowPrivate}
              onCheckedChange={(x) => set('allowPrivate', x)}
            />
            <SwitchField
              label="Block the NexTerm host’s own addresses"
              description={v.hostAddresses.length ? `Currently ${v.hostAddresses.slice(0, 4).join(', ')}${v.hostAddresses.length > 4 ? '…' : ''}` : undefined}
              checked={draft.blockHostAddresses}
              onCheckedChange={(x) => set('blockHostAddresses', x)}
            />
            <SwitchField label="Apply to administrators too" checked={draft.applyToAdmins} onCheckedChange={(x) => set('applyToAdmins', x)} />
            <Field label="Also deny" hint="IPs or CIDRs.">
              <TagInput value={draft.deny} onChange={(x) => set('deny', x)} placeholder="e.g. 10.20.0.0/16" aria-label="Also deny" />
            </Field>
            <Field label="Exceptions (allow)" hint="The most specific match wins; an exception can re-open a built-in rule.">
              <TagInput value={draft.allow} onChange={(x) => set('allow', x)} placeholder="e.g. 127.0.0.1/32" aria-label="Exceptions" />
            </Field>
            <Field label="Allowed ports" hint="Empty = every port. Example: 22,80,443,3389,5900-5999">
              <Input value={draft.allowedPorts} onChange={(e) => set('allowedPorts', e.target.value)} placeholder="All ports" className="max-w-sm font-mono" />
            </Field>
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
            <div className="flex items-center gap-2">
              <Button size="sm" onClick={() => void save()} disabled={!dirty} loading={busy}>
                <Save /> Save
              </Button>
              {dirty && (
                <Button size="sm" variant="ghost" onClick={() => setDraft(v.policy)}>
                  <RotateCcw /> Discard
                </Button>
              )}
              <Button size="sm" variant="ghost" className="ml-auto" onClick={() => setDraft(v.default)}>
                Defaults
              </Button>
            </div>
          </div>
        </Section>
        <Section title="Always refused" description="Built-in rules (loopback, link-local, cloud metadata…). Add an exception to open one deliberately.">
          <ul className="divide-y rounded-lg border bg-card text-sm">
            {v.builtin.map((r) => (
              <li key={r.cidr} className="flex items-center gap-3 px-4 py-1.5">
                <code className="w-40 font-mono text-xs">{r.cidr}</code>
                <span className="text-muted-foreground">{r.reason}</span>
              </li>
            ))}
          </ul>
        </Section>
        <Section title="Try it" description="Evaluates the policy above (including unsaved changes) without connecting.">
          <form
            className="grid gap-3 rounded-lg border bg-card p-4"
            onSubmit={(e) => {
              e.preventDefault()
              void test()
            }}
          >
            <div className="flex flex-wrap items-end gap-2">
              <Field label="Host" className="min-w-48 flex-1">
                <Input value={host} onChange={(e) => setHost(e.target.value)} placeholder="db.internal or 10.0.0.5" spellCheck={false} />
              </Field>
              <Field label="Port">
                <NumberInput value={port} onChange={setPort} min={1} max={65535} className="w-24" />
              </Field>
              <Field label="As user">
                <SimpleSelect
                  value={asUser || 'default'}
                  onValueChange={(x) => setAsUser(x === 'default' ? '' : x)}
                  options={[{ value: 'default', label: 'An ordinary user' }, ...(users.data ?? []).map((u) => ({ value: u.id, label: u.username }))]}
                  className="w-44"
                  aria-label="As user"
                />
              </Field>
              <Button type="submit" variant="secondary" loading={testing} disabled={!host.trim() || !port}>
                Test
              </Button>
            </div>
            {result && (
              <div className={cn('grid gap-2 rounded-md border px-3 py-2 text-sm', result.allowed ? 'border-success/40 bg-success/6' : 'border-destructive/40 bg-destructive/6')}>
                <div className="flex items-center gap-2 font-medium">
                  {result.allowed ? <CircleCheck className="size-4 text-success" /> : result.decision === 'error' ? <ShieldAlert className="size-4 text-destructive" /> : <CircleX className="size-4 text-destructive" />}
                  {result.decision === 'unrestricted' ? 'Allowed (not restricted for this user)' : result.allowed ? 'Allowed' : result.decision === 'error' ? 'Could not evaluate' : 'Refused'}
                  <span className="font-normal text-muted-foreground">— {result.error || result.reason}</span>
                </div>
                {result.addresses.length > 0 && (
                  <ul className="grid gap-1">
                    {result.addresses.map((a) => (
                      <li key={a.ip} className="flex flex-wrap items-center gap-2 text-xs">
                        <code className="font-mono">{a.ip}</code>
                        <Badge variant={a.allowed ? 'success' : 'destructive'}>{a.allowed ? 'allow' : 'deny'}</Badge>
                        <span className="text-muted-foreground">
                          {a.class}
                          {a.rule ? ` · ${a.rule}` : ''} · {a.reason}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )}
          </form>
        </Section>
      </div>
    </>
  )
}
