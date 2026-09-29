/*
 * Storage & retention (REC-7) plus the administrator's recording policy: retention (max age / total size) with a
 * cleanup preview, command audit, session sharing and debug-log capture. Non-admins see their usage and the policy.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Bug, Eraser, HardDrive, History, Share2, Timer } from 'lucide-react'
import { toast } from 'sonner'
import { runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { NumberInput } from '@/components/ui/number-input'
import { ProgressBar } from '@/components/ui/progress'
import { ErrorState, LoadingState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { Skeleton, SkeletonText } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { errorMessage, formatBytes, formatDateTime, plural } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { putPolicy, recKeys, runCleanup, usePolicy, useUsage } from './api'
import type { RecordingPolicy, RetentionResult } from './types'

function Section({ icon: Icon, title, description, children }: { icon: typeof HardDrive; title: string; description?: string; children: React.ReactNode }) {
  return (
    <section className="grid gap-3 rounded-lg border bg-card p-4">
      <header className="flex items-start gap-2.5">
        <Icon className="mt-0.5 size-4 text-muted-foreground" />
        <div>
          <h3 className="text-md font-semibold">{title}</h3>
          {description && <p className="text-sm text-muted-foreground">{description}</p>}
        </div>
      </header>
      <div className="grid gap-3 pl-6.5">{children}</div>
    </section>
  )
}

function Row({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="min-w-0">
        <div className="text-base">{label}</div>
        {hint && <div className="text-sm text-muted-foreground">{hint}</div>}
      </div>
      <div className="flex items-center gap-2">{children}</div>
    </div>
  )
}

export default function StoragePanel() {
  const isAdmin = useIsAdmin()
  const qc = useQueryClient()
  const policyQ = usePolicy()
  const usage = useUsage(isAdmin)
  const [draft, setDraft] = useState<RecordingPolicy | null>(null)
  const [preview, setPreview] = useState<RetentionResult | null>(null)
  const [cleaning, setCleaning] = useState(false)
  const timers = useRef<Record<string, ReturnType<typeof setTimeout>>>({})
  const policy = draft ?? policyQ.data ?? null

  useEffect(() => {
    if (policyQ.data && !draft) setDraft(policyQ.data)
  }, [policyQ.data, draft])
  useEffect(() => {
    const t = timers.current
    return () => Object.values(t).forEach(clearTimeout)
  }, [])

  /** Optimistic update; numbers are committed after typing pauses. */
  const change = (patch: Partial<RecordingPolicy>, debounceMs = 0) => {
    setDraft((d) => (d ? { ...d, ...patch } : d))
    setPreview(null)
    const key = Object.keys(patch).join(',')
    clearTimeout(timers.current[key])
    timers.current[key] = setTimeout(async () => {
      try {
        const p = await putPolicy(patch)
        qc.setQueryData(recKeys.policy, p)
        void qc.invalidateQueries({ queryKey: recKeys.usage(true) })
      } catch (err) {
        toast.error('Could not save the policy', { description: errorMessage(err) })
        setDraft(null)
        void policyQ.refetch()
      }
    }, debounceMs)
  }

  const byKind = useMemo(() => Object.entries(usage.data?.byKind ?? {}), [usage.data])
  const limitBytes = (policy?.maxTotalMB ?? 0) * 1024 * 1024
  const pct = limitBytes > 0 && usage.data ? Math.min(100, (usage.data.bytes / limitBytes) * 100) : 0

  const doPreview = async () => {
    try {
      setPreview(await runCleanup(true))
    } catch (err) {
      toast.error('Preview failed', { description: errorMessage(err) })
    }
  }
  const doClean = async () => {
    const p = preview ?? (await runCleanup(true).catch(() => null))
    if (!p) return
    if (!p.deleted) {
      toast.message('Nothing to clean up: every recording is within the policy.')
      return
    }
    if (!(await confirm({ title: `Delete ${plural(p.deleted, 'recording')}?`, description: `${formatBytes(p.bytes)} will be freed. This cannot be undone.`, confirmLabel: 'Clean up', destructive: true }))) return
    setCleaning(true)
    try {
      const r = await runCleanup(false)
      toast.success(`${plural(r.deleted, 'recording')} deleted`, { description: `${formatBytes(r.bytes)} freed` })
      setPreview(null)
      void qc.invalidateQueries({ queryKey: recKeys.all })
    } catch (err) {
      toast.error('Cleanup failed', { description: errorMessage(err) })
    } finally {
      setCleaning(false)
    }
  }

  const ro = !isAdmin
  return (
    <div className="mx-auto grid max-w-3xl gap-4 p-4">
      <Section icon={HardDrive} title={isAdmin ? 'Storage (all users)' : 'Your storage'} description="Session recordings (asciicast) and text logs kept on the NexTerm host.">
        <LoadingState
          busy={!usage.data && usage.isPending}
          skeleton={
            <div className="grid gap-2">
              <Skeleton className="h-7 w-28" />
              <Skeleton className="h-3.5 w-44" />
            </div>
          }
        >
          <div className="flex flex-wrap items-end gap-6">
            <div>
              <div className="text-2xl font-semibold tabular-nums">{formatBytes(usage.data?.bytes ?? 0)}</div>
              <div className="text-sm text-muted-foreground tabular-nums">
                {plural(usage.data?.count ?? 0, 'file')}
                {usage.data?.oldest ? ` · oldest ${formatDateTime(usage.data.oldest)}` : ''}
              </div>
            </div>
            {byKind.map(([k, v]) => (
              <div key={k} className="text-sm">
                <div className="text-muted-foreground">{k === 'asciicast' ? 'Recordings' : k === 'log' ? 'Text logs' : k}</div>
                <div className="tabular-nums">
                  {v.count} · {formatBytes(v.bytes)}
                </div>
              </div>
            ))}
          </div>
          {limitBytes > 0 && (
            <div className="grid gap-1">
              {/* A meter, not progress: keyed by the usage so it may shrink after a cleanup (still eased). */}
              <ProgressBar value={pct / 100} resetKey={usage.data?.bytes} tone={pct > 90 ? 'warning' : 'primary'} label="Storage used" />
              <div className="text-xs text-muted-foreground tabular-nums">
                {pct.toFixed(0)} % of the {formatBytes(limitBytes)} limit{usage.data?.scope === 'own' ? ' (the limit applies to all users together)' : ''}
              </div>
            </div>
          )}
        </LoadingState>
      </Section>

      {!policy && policyQ.error ? (
        <ErrorState error={policyQ.error} title="Could not load the recording policy" onRetry={() => void policyQ.refetch()} />
      ) : (
        <LoadingState busy={!policy} skeleton={<SkeletonText lines={6} className="rounded-lg border p-4" />}>
          {policy && (
            <>
              <Section icon={Timer} title="Retention" description="Finished recordings outside the policy are deleted every 10 minutes. Recordings still being written are never touched.">
                <Row label="Delete recordings older than" hint="0 keeps them forever.">
                  <NumberInput inputSize="sm" className="w-28" value={policy.maxAgeDays} min={0} max={36500} unit="days" disabled={ro} onChange={(v) => change({ maxAgeDays: v ?? 0 }, 700)} />
                </Row>
                <Row label="Keep the total size under" hint="The oldest recordings go first. 0 = unlimited.">
                  <NumberInput inputSize="sm" className="w-32" value={policy.maxTotalMB} min={0} max={16777216} unit="MB" disabled={ro} onChange={(v) => change({ maxTotalMB: v ?? 0 }, 700)} />
                </Row>
                {isAdmin && (
                  <div className="flex flex-wrap items-center gap-2">
                    <Button size="sm" variant="secondary" onClick={() => void doPreview()}>
                      Preview cleanup
                    </Button>
                    <Button size="sm" variant="secondary" loading={cleaning} onClick={() => void doClean()}>
                      <Eraser /> Clean up now
                    </Button>
                    {preview && (
                      <span className="text-sm text-muted-foreground animate-in fade-in-0">
                        {preview.deleted
                          ? `${plural(preview.deleted, 'recording')} (${formatBytes(preview.bytes)}) would be deleted — ${preview.byAge} by age, ${preview.bySize} by size.`
                          : 'Nothing would be deleted.'}
                      </span>
                    )}
                  </div>
                )}
              </Section>

              <Section icon={History} title="Command audit" description="Record every command run in terminal sessions in the audit log (from shell integration marks, else from the echoed input line). Typed passwords are never recorded.">
                <Row label="Audit executed commands" hint={policy.mode === 'desktop' ? 'Off by default in desktop mode.' : 'On by default in server mode.'}>
                  <Switch checked={policy.commandAudit} disabled={ro} onCheckedChange={(v) => change({ commandAudit: v })} aria-label="Audit executed commands" />
                </Row>
              </Section>

              <Section icon={Share2} title="Session sharing" description="Share links let people watch (or type into) a live terminal session without an account.">
                <Row label="Allow share links">
                  <Switch checked={policy.shareEnabled} disabled={ro} onCheckedChange={(v) => change({ shareEnabled: v })} aria-label="Allow share links" />
                </Row>
                <Row label="Allow interactive links" hint="Viewers can type into the session. Turning this off revokes existing interactive links.">
                  <Switch checked={policy.shareWriteEnabled} disabled={ro || !policy.shareEnabled} onCheckedChange={(v) => change({ shareWriteEnabled: v })} aria-label="Allow interactive links" />
                </Row>
                <Row label="Longest link lifetime">
                  <NumberInput inputSize="sm" className="w-28" value={policy.shareMaxHours} min={1} max={720} unit="hours" disabled={ro || !policy.shareEnabled} onChange={(v) => change({ shareMaxHours: v ?? 168 }, 700)} />
                </Row>
              </Section>

              {isAdmin && (
                <Section icon={Bug} title="Debug log" description="Keep the most recent application log records in memory for the debug log viewer (secrets are redacted).">
                  <Row label="Capture log records">
                    <Switch checked={policy.debugLog} onCheckedChange={(v) => change({ debugLog: v })} aria-label="Capture log records" />
                  </Row>
                  <Row label="Capture level">
                    <SimpleSelect
                      size="sm"
                      className="w-28"
                      value={policy.debugLogLevel}
                      disabled={!policy.debugLog}
                      onValueChange={(v) => change({ debugLogLevel: v })}
                      options={[
                        { value: 'debug', label: 'Debug' },
                        { value: 'info', label: 'Info' },
                        { value: 'warn', label: 'Warning' },
                        { value: 'error', label: 'Error' },
                      ]}
                    />
                    <Button size="sm" variant="ghost" onClick={() => void runCommand('admin.debugLogs')}>
                      Open viewer
                    </Button>
                  </Row>
                </Section>
              )}
            </>
          )}
        </LoadingState>
      )}
    </div>
  )
}

