/*
 * Run history: script runs, batch runs and scheduled tasks (last 500 per user), with the full log / per-host results.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Eraser, History, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { cn, errorMessage, formatDuration, formatRelativeTime } from '@/lib/utils'
import { autoKeys, clearRuns, deleteRun, useRuns } from '../api'
import { StatusBadge } from '../components/pickers'
import { RunView } from '../components/RunView'

type Filter = 'all' | 'script' | 'batch' | 'schedule'

export default function HistoryPage({ runId, refId }: { runId?: string; refId?: string }) {
  const qc = useQueryClient()
  const [filter, setFilter] = React.useState<Filter>('all')
  const [ref, setRef] = React.useState<string | undefined>(refId)
  const [selected, setSelected] = React.useState<string | undefined>(runId)
  React.useEffect(() => {
    if (runId) setSelected(runId)
  }, [runId])
  React.useEffect(() => setRef(refId), [refId])
  const params = filter === 'schedule' ? { origin: 'schedule', refId: ref } : filter === 'all' ? { refId: ref } : { kind: filter, refId: ref }
  const runs = useRuns(params)
  const { data } = runs
  const current = selected ?? data?.[0]?.id

  const clearAll = async () => {
    if (!(await confirm({ title: 'Clear the run history?', description: 'Running runs are kept.', destructive: true, confirmLabel: 'Clear' }))) return
    try {
      await clearRuns()
      setSelected(undefined)
      await qc.invalidateQueries({ queryKey: autoKeys.runsAll })
    } catch (err) {
      toast.error('Could not clear the history', { description: errorMessage(err) })
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col @3xl:flex-row">
      <aside className="flex max-h-56 w-full shrink-0 flex-col border-b @3xl:max-h-none @3xl:w-72 @3xl:border-r @3xl:border-b-0" aria-label="Runs">
        <div className="flex flex-col gap-2 border-b p-2">
          <SegmentedControl<Filter>
            size="sm"
            fullWidth
            value={filter}
            onValueChange={(f) => {
              setFilter(f)
              setSelected(undefined)
            }}
            options={[
              { value: 'all', label: 'All' },
              { value: 'script', label: 'Scripts' },
              { value: 'batch', label: 'Batch' },
              { value: 'schedule', label: 'Scheduled' },
            ]}
            aria-label="Filter runs"
          />
          {ref && (
            <Button size="xs" variant="secondary" onClick={() => setRef(undefined)}>
              Showing one task — show all
            </Button>
          )}
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-1">
          <QueryState
            query={runs}
            skeleton={<SkeletonRows rows={6} rowHeight={44} />}
            errorTitle="Could not load the runs"
            isEmpty={(list) => !list.length}
            empty={<p className="p-2 text-sm text-muted-foreground">No runs yet.</p>}
          >
            {(list) =>
              list.map((r) => {
                const dur = r.finishedAt ? Date.parse(r.finishedAt) - Date.parse(r.startedAt) : undefined
                return (
                  <div key={r.id} className={cn('group/run relative rounded-md', current === r.id && 'bg-accent')}>
                    <button
                      type="button"
                      onClick={() => setSelected(r.id)}
                      className="flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60"
                    >
                      <span className="flex items-center gap-2 pr-6">
                        <StatusBadge status={r.status} />
                        <span className="truncate font-medium">{r.name}</span>
                      </span>
                      <span className="truncate text-xs text-muted-foreground">
                        {r.origin !== 'manual' ? `${r.origin} · ` : ''}
                        {formatRelativeTime(r.startedAt)}
                        {dur !== undefined ? ` · ${formatDuration(dur)}` : ''}
                        {r.kind === 'batch' && r.summary.total ? ` · ${r.summary.ok}/${r.summary.total} ok` : ''}
                      </span>
                    </button>
                    {r.status !== 'running' && (
                      <IconButton
                        icon={Trash2}
                        label="Delete run"
                        size="xs"
                        className="absolute top-1.5 right-1 opacity-0 group-hover/run:opacity-100 focus-visible:opacity-100"
                        onClick={() =>
                          void deleteRun(r.id)
                            .then(() => qc.invalidateQueries({ queryKey: autoKeys.runsAll }))
                            .catch((err) => toast.error('Could not delete the run', { description: errorMessage(err) }))
                        }
                      />
                    )}
                  </div>
                )
              })
            }
          </QueryState>
        </div>
        <div className="border-t p-2">
          <Button size="xs" variant="ghost" onClick={() => void clearAll()} disabled={!data?.length}>
            <Eraser /> Clear history
          </Button>
        </div>
      </aside>
      <section className="flex min-h-0 min-w-0 flex-1 flex-col p-3" aria-label="Run">
        {current ? (
          <RunView key={current} runId={current} className="min-h-0 flex-1" />
        ) : (
          data && <EmptyState icon={History} title="Run history" description="Script runs, batch runs and scheduled tasks show up here." />
        )}
      </section>
    </div>
  )
}
