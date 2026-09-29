/*
 * Scheduled tasks (AUTO-12): cron schedules of commands / snippets / scripts on connections, with a cron helper
 * (presets + the next activations from the backend parser), run now, last result and history.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { CalendarClock, History, MoreHorizontal, Pencil, Play, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip } from '@/components/ui/tooltip'
import { errorMessage, formatDateTime, formatRelativeTime, plural } from '@/lib/utils'
import { autoKeys, createSchedule, dangerousMatches, deleteSchedule, previewSchedule, runScheduleNow, updateSchedule, useSchedules, useSnippets, type ScheduleInput } from '../api'
import { ConnectionMultiSelect, StatusBadge } from '../components/pickers'
import { describeCron, formatNextRun } from '../cron'
import { RunView } from '../components/RunView'
import { checkDangerous } from '../guard'
import { askDangerous } from '../store'
import type { JobAction, NotifyPolicy, Schedule, SchedulePreview } from '../types'
import { ActionFields, actionReady, actionText } from './BatchPage'

const PRESETS: { spec: string; label: string }[] = [
  { spec: '*/15 * * * *', label: 'Every 15 minutes' },
  { spec: '@hourly', label: 'Every hour' },
  { spec: '0 3 * * *', label: 'Every day at 03:00' },
  { spec: '0 8 * * mon-fri', label: 'Weekdays at 08:00' },
  { spec: '0 2 * * sun', label: 'Sundays at 02:00' },
  { spec: '0 4 1 * *', label: 'Monthly (1st, 04:00)' },
]

function usePreview(spec: string): SchedulePreview | null {
  const [p, setP] = React.useState<SchedulePreview | null>(null)
  React.useEffect(() => {
    if (!spec.trim()) {
      setP(null)
      return
    }
    let alive = true
    const t = setTimeout(() => {
      previewSchedule(spec, 3)
        .then((r) => alive && setP(r))
        .catch(() => alive && setP(null))
    }, 250)
    return () => {
      alive = false
      clearTimeout(t)
    }
  }, [spec])
  return p
}

function ScheduleEditor({ schedule, onClose }: { schedule?: Schedule; onClose: () => void }) {
  const qc = useQueryClient()
  const { data: snippets } = useSnippets()
  const [name, setName] = React.useState(schedule?.name ?? '')
  const [spec, setSpec] = React.useState(schedule?.spec ?? '0 3 * * *')
  const [action, setAction] = React.useState<JobAction>(schedule?.action ?? { kind: 'command', command: '', mode: 'auto', parallel: 4, timeoutSec: 120 })
  const [conns, setConns] = React.useState<string[]>(schedule?.connectionIds ?? [])
  const [notify, setNotify] = React.useState<NotifyPolicy>(schedule?.notify ?? 'failure')
  const [saving, setSaving] = React.useState(false)
  const preview = usePreview(spec)
  const needsConns = action.kind !== 'script'
  const ready = !!name.trim() && preview?.valid !== false && actionReady(action) && (!needsConns || conns.length > 0)

  const save = async () => {
    setSaving(true)
    try {
      let confirm = false
      const pre = checkDangerous(actionText(action, snippets))
      if (pre.length) {
        if (!(await askDangerous(pre, [`scheduled task “${name}”`]))) return
        confirm = true
      }
      const body: ScheduleInput = { name: name.trim(), spec, action, connectionIds: conns, notify, confirmDangerous: confirm }
      for (;;) {
        try {
          if (schedule) await updateSchedule(schedule.id, body)
          else await createSchedule({ ...body, enabled: true })
          break
        } catch (err) {
          const m = dangerousMatches(err)
          if (m && !body.confirmDangerous) {
            if (!(await askDangerous(m, [`scheduled task “${name}”`]))) return
            body.confirmDangerous = true
            continue
          }
          throw err
        }
      }
      await qc.invalidateQueries({ queryKey: autoKeys.schedules })
      toast.success(schedule ? 'Task saved' : 'Task scheduled')
      onClose()
    } catch (err) {
      toast.error('Could not save the task', { description: errorMessage(err) })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="2xl" className="max-h-[92vh]">
        <DialogHeader>
          <DialogTitle>
            <CalendarClock className="size-4 text-primary" /> {schedule ? 'Edit scheduled task' : 'New scheduled task'}
          </DialogTitle>
          <DialogDescription>Runs while NexTerm is running. Stored passwords need an unlocked vault; prompts (host keys, 2FA) make a host fail.</DialogDescription>
        </DialogHeader>
        <DialogBody className="flex flex-col gap-3">
          <Field label="Name" required>
            <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} maxLength={200} />
          </Field>
          <Field
            label="When"
            hint="Cron: minute hour day-of-month month day-of-week — or @hourly, @daily, @every 30m; prefix CRON_TZ=Europe/Paris for a time zone"
            error={preview && !preview.valid ? preview.error : undefined}
          >
            <Input className="font-mono" value={spec} onChange={(e) => setSpec(e.target.value)} spellCheck={false} />
          </Field>
          <div className="flex flex-wrap gap-1">
            {PRESETS.map((p) => (
              <Button key={p.spec} size="xs" variant={spec === p.spec ? 'default' : 'secondary'} onClick={() => setSpec(p.spec)}>
                {p.label}
              </Button>
            ))}
          </div>
          {/* Space reserved: the preview arrives a moment after typing and must not push the form down. */}
          <p className="-mt-1 min-h-5 text-sm text-muted-foreground" aria-live="polite">
            {preview?.valid && (
              <>
                {describeCron(spec) && <span className="font-medium text-foreground">{describeCron(spec)} · </span>}
                Next: {preview.next.map((n) => formatDateTime(n)).join(' · ')}
              </>
            )}
          </p>
          <ActionFields value={action} onChange={setAction} />
          <Field label={needsConns ? 'Connections' : 'Connections (optional: without any, the script runs once)'}>
            <ConnectionMultiSelect value={conns} onChange={setConns} height="h-48" />
          </Field>
          <Field label="Notify me" className="w-60">
            <SimpleSelect<NotifyPolicy>
              value={notify}
              onValueChange={setNotify}
              options={[
                { value: 'failure', label: 'When it fails' },
                { value: 'always', label: 'After every run' },
                { value: 'never', label: 'Never' },
              ]}
            />
          </Field>
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => void save()} loading={saving} disabled={!ready}>
            {schedule ? 'Save' : 'Schedule'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export default function SchedulesPage({ onShowRuns }: { onShowRuns: (refId: string) => void }) {
  const qc = useQueryClient()
  const schedules = useSchedules()
  const [editing, setEditing] = React.useState<Schedule | 'new' | null>(null)
  const [view, setView] = React.useState<{ runId?: string; jobId: string } | null>(null)

  const toggle = async (s: Schedule, enabled: boolean) => {
    try {
      await updateSchedule(s.id, { enabled })
      await qc.invalidateQueries({ queryKey: autoKeys.schedules })
    } catch (err) {
      toast.error('Could not update the task', { description: errorMessage(err) })
    }
  }
  const runNow = async (s: Schedule) => {
    try {
      const r = await runScheduleNow(s.id)
      setView(r)
      toast.success(`“${s.name}” started`)
    } catch (err) {
      toast.error('Could not start the task', { description: errorMessage(err) })
    }
  }
  const remove = async (s: Schedule) => {
    if (!(await confirm({ title: `Delete “${s.name}”?`, destructive: true, confirmLabel: 'Delete' }))) return
    try {
      await deleteSchedule(s.id)
      await qc.invalidateQueries({ queryKey: autoKeys.schedules })
    } catch (err) {
      toast.error('Could not delete the task', { description: errorMessage(err) })
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col gap-3 overflow-y-auto p-3">
      <div className="flex items-center gap-2">
        <h2 className="flex items-center gap-2 text-md font-semibold">
          <CalendarClock className="size-4 text-primary" /> Scheduled tasks
        </h2>
        <Button size="sm" className="ml-auto" onClick={() => setEditing('new')}>
          <Plus /> New task
        </Button>
      </div>
      <QueryState
        query={schedules}
        skeleton={<SkeletonRows rows={4} rowHeight={44} className="rounded-md border" />}
        errorTitle="Could not load the tasks"
        isEmpty={(list) => !list.length}
        empty={
          <EmptyState
            icon={CalendarClock}
            title="No scheduled tasks"
            description="Run a command, snippet or script on your hosts on a schedule — backups, health checks, cleanups."
            action={
              <Button onClick={() => setEditing('new')}>
                <Plus /> New task
              </Button>
            }
          />
        }
      >
        {(list) => (
          <Table containerClassName="rounded-md border">
            <TableHeader>
              <TableRow>
                <TableHead className="w-14">On</TableHead>
                <TableHead>Task</TableHead>
                <TableHead>Schedule</TableHead>
                <TableHead>Last run</TableHead>
                <TableHead>Next run</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((s) => (
                <TableRow key={s.id}>
                  <TableCell>
                    <Switch size="sm" checked={s.enabled} onCheckedChange={(v) => void toggle(s, v)} aria-label={`Enable ${s.name}`} />
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col">
                      <span className="font-medium">{s.name}</span>
                      <span className="text-xs text-muted-foreground">
                        {s.action.kind}
                        {s.connectionIds.length ? ` on ${plural(s.connectionIds.length, 'connection')}` : ''}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col">
                      <span className="whitespace-nowrap">{describeCron(s.spec) ?? <code className="font-mono text-xs">{s.spec}</code>}</span>
                      {describeCron(s.spec) && <code className="font-mono text-2xs text-muted-foreground">{s.spec}</code>}
                    </div>
                  </TableCell>
                  <TableCell>
                    {s.lastRunAt ? (
                      <span className="flex items-center gap-1.5">
                        <StatusBadge status={s.lastStatus ?? ''} />
                        <Tooltip content={formatDateTime(s.lastRunAt)}>
                          <span className="text-xs whitespace-nowrap text-muted-foreground">{formatRelativeTime(s.lastRunAt)}</span>
                        </Tooltip>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">Not yet</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs whitespace-nowrap text-muted-foreground">
                    {s.enabled && s.nextRunAt ? (
                      <Tooltip content={`${formatDateTime(s.nextRunAt)} (${formatRelativeTime(s.nextRunAt)})`}>
                        <span>{formatNextRun(s.nextRunAt)}</span>
                      </Tooltip>
                    ) : (
                      <span title={s.enabled ? undefined : 'Paused'}>—</span>
                    )}
                  </TableCell>
                  <TableCell>
                    <span className="flex justify-end gap-0.5">
                      <Button size="icon-xs" variant="ghost" aria-label={`Run ${s.name} now`} onClick={() => void runNow(s)}>
                        <Play />
                      </Button>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button size="icon-xs" variant="ghost" aria-label={`Actions for ${s.name}`}>
                            <MoreHorizontal />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onSelect={() => setEditing(s)}>
                            <Pencil /> Edit…
                          </DropdownMenuItem>
                          <DropdownMenuItem onSelect={() => void runNow(s)}>
                            <Play /> Run now
                          </DropdownMenuItem>
                          <DropdownMenuItem onSelect={() => onShowRuns(s.id)}>
                            <History /> Show runs
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem variant="destructive" onSelect={() => void remove(s)}>
                            <Trash2 /> Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
        </Table>
        )}
      </QueryState>
      {view && (
        <section className="flex min-h-72 flex-col" aria-label="Current run">
          <RunView runId={view.runId} jobId={view.jobId} className="min-h-0 flex-1" />
        </section>
      )}
      {editing && <ScheduleEditor schedule={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)} />}
    </div>
  )
}
