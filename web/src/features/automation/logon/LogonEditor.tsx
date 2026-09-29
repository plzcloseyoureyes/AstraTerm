/*
 * Logon actions editor (AUTO-8): connection.options.logonActions — ordered expect/send steps the backend runs after
 * every (re)connect of the connection, with or without a browser (telnet / serial devices, enable passwords, "terminal
 * length 0", su / sudo). Patterns are validated with the backend's regex engine (RE2) before saving.
 *
 * Ordering: the backend types options.startupCommand after the last logon step (only when every step succeeded) —
 * the usual order "log in, then run the start-up commands". The editor states this next to the steps.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, CornerDownRight, KeyRound, LogIn, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { updateConnection, useConnection } from '@/api/connections'
import { queryKeys } from '@/api/queryKeys'
import type { Connection } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { EmptyState } from '@/components/ui/empty-state'
import { ErrorState } from '@/components/ui/query-state'
import { LoadingPane } from '@/components/ui/spinner'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { errorMessage, uid } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { regexTest } from '../api'
import type { LogonAction } from '../types'

interface Row extends LogonAction {
  key: string
  mode: 'text' | 'secret'
  error?: string
}

const COMMON_SECRETS = ['password', 'sudoPassword', 'enablePassword']

function toRows(conn: Connection | undefined): Row[] {
  const raw = (conn?.options as Record<string, unknown> | undefined)?.logonActions
  if (!Array.isArray(raw)) return []
  return raw
    .filter((x): x is LogonAction => !!x && typeof x === 'object')
    .map((a) => ({ ...a, key: uid('la'), mode: a.secret ? 'secret' : 'text' }))
}

function fromRows(rows: Row[]): LogonAction[] {
  return rows.map((r) => {
    const a: LogonAction = {}
    if (r.expect?.trim()) a.expect = r.expect
    if (r.mode === 'secret' && r.secret) a.secret = r.secret
    else if (r.send) a.send = r.send
    if (r.enter === false) a.enter = false
    if (r.timeoutSec && r.timeoutSec !== 20) a.timeoutSec = r.timeoutSec
    if (r.optional) a.optional = true
    if (r.delayMs) a.delayMs = r.delayMs
    return a
  })
}

function canEditConnection(conn: Connection | undefined, user: { id: string; role: string } | null): boolean {
  return !!conn && !!user && (conn.ownerId === user.id || user.role === 'admin')
}

export function LogonEditor({ connectionId, onSaved, onCancel }: { connectionId: string; onSaved?: () => void; onCancel?: () => void }) {
  const qc = useQueryClient()
  const user = useCurrentUser()
  const { data: conn, isLoading, error, refetch } = useConnection(connectionId)
  const [rows, setRows] = React.useState<Row[]>([])
  const [saving, setSaving] = React.useState(false)
  const loadedFor = React.useRef<string | null>(null)
  const gate = useLoadingGate(isLoading)
  React.useEffect(() => {
    if (conn && loadedFor.current !== conn.id) {
      loadedFor.current = conn.id
      setRows(toRows(conn))
    }
  }, [conn])
  if (gate.hold) return <LoadingPane active={gate.show} immediate />
  if (error || !conn) return <ErrorState error={error ?? new Error('The connection was not found.')} title="Could not load the connection" onRetry={() => void refetch()} />
  const editable = canEditConnection(conn, user)
  const startup = typeof (conn.options as Record<string, unknown>).startupCommand === 'string' ? ((conn.options as Record<string, unknown>).startupCommand as string).trim() : ''
  const secretOptions = Array.from(new Set([...(conn.secretKeys ?? []).filter((k) => !k.startsWith('hop:')), ...COMMON_SECRETS]))
  const set = (key: string, patch: Partial<Row>) => setRows((rs) => rs.map((r) => (r.key === key ? { ...r, ...patch, error: undefined } : r)))

  const save = async () => {
    setSaving(true)
    try {
      // Validate the patterns with the backend's engine first.
      const checked = await Promise.all(
        rows.map(async (r) => {
          if (!r.expect?.trim()) return { ...r, error: undefined }
          const res = await regexTest(r.expect, true)
          return { ...r, error: res.valid ? undefined : res.error || 'invalid pattern' }
        }),
      )
      if (checked.some((r) => r.error)) {
        setRows(checked)
        toast.error('Fix the highlighted patterns first')
        return
      }
      const options = { ...(conn.options as Record<string, unknown>) }
      const actions = fromRows(rows)
      if (actions.length) options.logonActions = actions
      else delete options.logonActions
      await updateConnection(conn.id, { options: options as Connection['options'] })
      await qc.invalidateQueries({ queryKey: queryKeys.connections })
      toast.success(actions.length ? `Logon actions saved for ${conn.name}` : `Logon actions removed from ${conn.name}`, {
        description: 'They run at the next connect or reconnect.',
      })
      onSaved?.()
    } catch (err) {
      toast.error('Could not save the logon actions', { description: errorMessage(err) })
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="flex min-h-0 flex-col gap-3">
      {!editable && (
        <p className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm">This connection is shared with you: only its owner or an administrator can change its logon actions.</p>
      )}
      <p className="text-sm text-muted-foreground">
        After each connect NexTerm waits for every step's pattern (a regular expression over the plain-text output, e.g.{' '}
        <code className="font-mono">[Pp]assword:\s*$</code>) and then types the text or a stored secret. A step without a pattern types immediately.
      </p>
      {startup && rows.length > 0 && (
        <p className="flex items-start gap-2 rounded-md border bg-muted/30 px-3 py-2 text-sm" role="note">
          <CornerDownRight className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" aria-hidden />
          <span className="min-w-0">
            The start-up command <code className="font-mono break-all">{startup.split('\n')[0]}</code>
            {startup.includes('\n') ? ' …' : ''} runs <strong>after</strong> the last step (not when a step stops the sequence).
          </span>
        </p>
      )}
      {!rows.length ? (
        <EmptyState size="sm" icon={LogIn} title="No logon actions" description="Add steps such as: wait for “login:”, type the user; wait for “Password:”, type the stored password." />
      ) : (
        <ol className="flex flex-col gap-2" aria-label="Logon actions">
          {rows.map((r, i) => (
            <li key={r.key} className="flex flex-col gap-2 rounded-md border bg-card p-2">
              <div className="flex items-center gap-2">
                <Badge variant="outline" className="tabular">
                  {i + 1}
                </Badge>
                <span className="w-12 shrink-0 text-xs text-muted-foreground">Wait for</span>
                <Input
                  inputSize="sm"
                  className="flex-1 font-mono"
                  value={r.expect ?? ''}
                  disabled={!editable}
                  onChange={(e) => set(r.key, { expect: e.target.value })}
                  placeholder="(no wait) e.g. login:\s*$"
                  aria-label={`Step ${i + 1} pattern`}
                  aria-invalid={!!r.error || undefined}
                  spellCheck={false}
                />
                <span className="flex shrink-0 items-center gap-0.5">
                  <IconButton icon={ArrowUp} label="Move up" size="xs" disabled={!editable || i === 0} onClick={() => setRows((rs) => swap(rs, i, i - 1))} />
                  <IconButton icon={ArrowDown} label="Move down" size="xs" disabled={!editable || i === rows.length - 1} onClick={() => setRows((rs) => swap(rs, i, i + 1))} />
                  <IconButton icon={Trash2} label="Delete step" size="xs" disabled={!editable} onClick={() => setRows((rs) => rs.filter((x) => x.key !== r.key))} />
                </span>
              </div>
              {r.error && (
                <p role="alert" className="pl-[4.5rem] text-xs text-destructive">
                  {r.error}
                </p>
              )}
              <div className="flex flex-wrap items-center gap-2 pl-[4.5rem]">
                <SimpleSelect<'text' | 'secret'>
                  size="sm"
                  className="w-36"
                  disabled={!editable}
                  value={r.mode}
                  onValueChange={(mode) => set(r.key, { mode })}
                  options={[
                    { value: 'text', label: 'then type' },
                    { value: 'secret', label: 'then type secret' },
                  ]}
                  aria-label={`Step ${i + 1} action`}
                />
                {r.mode === 'text' ? (
                  <Input
                    inputSize="sm"
                    className="min-w-48 flex-1 font-mono"
                    value={r.send ?? ''}
                    disabled={!editable}
                    onChange={(e) => set(r.key, { send: e.target.value })}
                    placeholder="text (\t, \x03 … escapes)"
                    aria-label={`Step ${i + 1} text`}
                    spellCheck={false}
                  />
                ) : (
                  <SimpleSelect
                    size="sm"
                    className="min-w-48 flex-1"
                    disabled={!editable}
                    value={r.secret || undefined}
                    onValueChange={(secret) => set(r.key, { secret })}
                    placeholder="Stored secret"
                    options={secretOptions.map((k) => ({
                      value: k,
                      label: (
                        <span className="flex items-center gap-1.5">
                          <KeyRound className="size-3" /> {k}
                          {!conn.secretKeys.includes(k) && <span className="text-muted-foreground">(not stored)</span>}
                        </span>
                      ),
                    }))}
                    aria-label={`Step ${i + 1} secret`}
                  />
                )}
                <label className="flex items-center gap-1.5 text-sm">
                  <Checkbox checked={r.enter !== false} disabled={!editable} onCheckedChange={(v) => set(r.key, { enter: v === true })} />
                  Enter
                </label>
                <label className="flex items-center gap-1.5 text-sm">
                  <Checkbox checked={!!r.optional} disabled={!editable} onCheckedChange={(v) => set(r.key, { optional: v === true })} />
                  Optional
                </label>
                <span className="flex items-center gap-1 text-sm">
                  Timeout
                  <span className="w-20">
                    <NumberInput inputSize="sm" value={r.timeoutSec ?? 20} min={1} max={600} unit="s" disabled={!editable} onChange={(v) => set(r.key, { timeoutSec: v ?? 20 })} aria-label={`Step ${i + 1} timeout`} />
                  </span>
                </span>
              </div>
            </li>
          ))}
        </ol>
      )}
      <div className="flex items-center gap-2">
        <Button size="sm" variant="secondary" disabled={!editable || rows.length >= 32} onClick={() => setRows((rs) => [...rs, { key: uid('la'), mode: 'text', expect: '', send: '' }])}>
          <Plus /> Add step
        </Button>
        <span className="ml-auto flex gap-2">
          {onCancel && (
            <Button size="sm" variant="secondary" onClick={onCancel}>
              Cancel
            </Button>
          )}
          <Button size="sm" onClick={() => void save()} loading={saving} disabled={!editable || (!rows.length && !toRows(conn).length)}>
            Save
          </Button>
        </span>
      </div>
    </div>
  )
}

function swap<T>(arr: T[], i: number, j: number): T[] {
  if (j < 0 || j >= arr.length) return arr
  const next = arr.slice()
  ;[next[i], next[j]] = [next[j], next[i]]
  return next
}
