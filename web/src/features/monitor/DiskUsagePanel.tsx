/*
 * Disk usage drill-down (MON-3): the sizes of a directory's subdirectories on the same filesystem, largest first,
 * with breadcrumbs, the mounted volumes as starting points and an optional sudo re-scan for unreadable trees.
 */
import { useEffect, useState } from 'react'
import { ArrowUp, ChevronRight, Folder, FolderOpen, HardDrive, RefreshCw, ShieldCheck, TriangleAlert } from 'lucide-react'
import { runCommand, isCommandEnabled } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingPane, Spinner } from '@/components/ui/spinner'
import { Toolbar, ToolbarSeparator, ToolbarSpacer } from '@/components/ui/toolbar'
import { useManualRefresh } from '@/lib/hooks'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, errorMessage } from '@/lib/utils'
import { useDiskUsage } from './api'
import { Meter } from './components'
import { bytes, compactBytes, diskPct, level, pct } from './format'
import { monitorSettings } from './settings'
import type { Stats, TargetId } from './types'

function crumbs(path: string): { name: string; path: string }[] {
  const parts = path.split('/').filter(Boolean)
  const out = [{ name: '/', path: '/' }]
  let acc = ''
  for (const p of parts) {
    acc += `/${p}`
    out.push({ name: p, path: acc })
  }
  return out
}

export function DiskUsagePanel({
  target,
  platform,
  active,
  stats,
  initialPath,
  sessionId,
}: {
  target: TargetId
  platform?: string
  active: boolean
  stats?: Stats
  initialPath?: string
  /** Session for "open in the file browser" (files module). */
  sessionId?: string
}) {
  const s = monitorSettings.use()
  const [path, setPath] = useState(initialPath || stats?.disks?.[0]?.mount || '/')
  const [sudo, setSudo] = useState(false)
  const [edit, setEdit] = useState<string | null>(null)
  useEffect(() => {
    if (initialPath) setPath(initialPath)
  }, [initialPath])
  const q = useDiskUsage(target, path, sudo, active && platform !== 'windows')
  const manual = useManualRefresh(q.refetch)

  if (platform === 'windows') {
    return <EmptyState icon={HardDrive} title="Not available on Windows hosts" description="The drill-down relies on du; the Overview lists the volumes." />
  }
  const du = q.data
  const firstLoad = useLoadingGate(q.isPending && !du)
  const max = du?.entries.reduce((m, e) => Math.max(m, e.size), 0) ?? 0
  const canBrowse = !!sessionId && isCommandEnabled('files.openForSession')

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Toolbar aria-label="Disk usage tools" className="gap-1">
        <IconButton icon={ArrowUp} label="Parent directory" size="sm" disabled={path === '/'} onClick={() => setPath(du?.parent || path.replace(/\/[^/]+\/?$/, '') || '/')} />
        <IconButton icon={RefreshCw} label="Scan again" size="sm" onClick={manual.refresh} busy={manual.refreshing} />
        <IconButton icon={ShieldCheck} label={sudo ? 'Scanning with sudo' : 'Scan with sudo (read every directory)'} size="sm" active={sudo} onClick={() => setSudo((v) => !v)} />
        <ToolbarSeparator />
        {edit != null ? (
          <form
            className="flex-1"
            onSubmit={(e) => {
              e.preventDefault()
              const v = edit.trim()
              if (v.startsWith('/')) setPath(v.replace(/\/+$/, '') || '/')
              setEdit(null)
            }}
          >
            <Input inputSize="sm" autoFocus value={edit} onChange={(e) => setEdit(e.target.value)} onBlur={() => setEdit(null)} aria-label="Path" className="font-mono" />
          </form>
        ) : (
          <nav aria-label="Path" className="flex min-w-0 flex-1 items-center overflow-hidden text-sm" onDoubleClick={() => setEdit(path)}>
            {crumbs(path).map((c, i, all) => (
              <span key={c.path} className="flex min-w-0 items-center">
                {i > 1 && <ChevronRight className="size-3 shrink-0 text-muted-foreground" />}
                <button
                  type="button"
                  onClick={() => setPath(c.path)}
                  className={cn('truncate rounded px-1 font-mono text-xs hover:bg-accent', i === all.length - 1 && 'font-semibold')}
                  aria-current={i === all.length - 1 ? 'page' : undefined}
                >
                  {c.name}
                </button>
              </span>
            ))}
          </nav>
        )}
        <ToolbarSpacer />
        <Spinner active={q.isFetching} reserve className="size-3.5" />
        {du && <span className="text-xs text-muted-foreground tabular">{bytes(du.total)}</span>}
        {canBrowse && (
          <Button size="xs" variant="ghost" onClick={() => void runCommand('files.openForSession', { sessionId, path })}>
            <FolderOpen /> Browse
          </Button>
        )}
      </Toolbar>
      {stats && stats.disks.length > 0 && (
        <div className="flex flex-wrap gap-1.5 border-b px-2 py-1.5">
          {stats.disks.slice(0, 12).map((d) => {
            const p = diskPct(d)
            return (
              <button
                key={d.mount}
                type="button"
                onClick={() => setPath(d.mount)}
                className={cn(
                  'flex min-w-36 flex-col gap-0.5 rounded-md border px-2 py-1 text-left text-xs hover:bg-accent/60',
                  path === d.mount && 'border-primary/60 bg-primary/5',
                )}
              >
                <span className="flex items-center justify-between gap-2">
                  <span className="flex min-w-0 items-center gap-1 font-medium">
                    <HardDrive className="size-3 shrink-0" />
                    <span className="truncate">{d.mount}</span>
                  </span>
                  <span className="tabular">{pct(p)}</span>
                </span>
                <Meter value={p} lvl={level(p, s.warnPct, s.critPct)} label={`${d.mount} used`} />
                <span className="text-muted-foreground tabular">
                  {compactBytes(d.used)} of {compactBytes(d.total)} · {d.fs}
                </span>
              </button>
            )
          })}
        </div>
      )}
      {du?.partial && (
        <div className="flex items-center gap-2 border-b bg-warning/10 px-3 py-1 text-xs text-warning">
          <TriangleAlert className="size-3.5" />
          <span className="min-w-0 flex-1 truncate">Some directories could not be read, sizes are lower bounds{du.warning ? `: ${du.warning}` : '.'}</span>
          {!sudo && (
            <button type="button" className="shrink-0 underline-offset-2 hover:underline" onClick={() => setSudo(true)}>
              Scan with sudo
            </button>
          )}
        </div>
      )}
      <div className="min-h-0 flex-1 overflow-auto">
        {firstLoad.hold ? (
          firstLoad.show && <LoadingPane immediate label={`Measuring ${path}… (large trees can take a while)`} />
        ) : q.isError && !du ? (
          <EmptyState
            icon={TriangleAlert}
            title="Could not measure this directory"
            description={errorMessage(q.error)}
            action={
              <Button size="sm" variant="outline" onClick={() => setSudo(true)} disabled={sudo}>
                <ShieldCheck /> Retry with sudo
              </Button>
            }
          />
        ) : du && du.entries.length === 0 ? (
          <EmptyState size="sm" icon={Folder} title="Empty directory" />
        ) : (
          <ul className="divide-y divide-border/50" aria-label={`Contents of ${path}`}>
            {du?.entries.map((e) => {
              const share = du.total > 0 ? (e.size / du.total) * 100 : 0
              return (
                <li key={e.path + e.name}>
                  <button
                    type="button"
                    disabled={!e.dir}
                    onClick={() => e.dir && setPath(e.path)}
                    className="grid w-full grid-cols-[minmax(8rem,1fr)_minmax(6rem,2fr)_5rem_4rem] items-center gap-3 px-3 py-1 text-left text-sm hover:bg-accent/50 disabled:cursor-default disabled:hover:bg-transparent"
                  >
                    <span className="flex min-w-0 items-center gap-1.5">
                      {e.dir ? <Folder className="size-3.5 shrink-0 text-primary" /> : <span className="size-3.5 shrink-0" />}
                      <span className={cn('truncate', !e.dir && 'text-muted-foreground italic')}>{e.name}</span>
                    </span>
                    <span className="h-2 overflow-hidden rounded-full bg-muted">
                      <span className="block h-full rounded-full bg-primary/70" style={{ width: `${max > 0 ? (e.size / max) * 100 : 0}%` }} />
                    </span>
                    <span className="text-right tabular">{compactBytes(e.size)}</span>
                    <span className="text-right text-xs text-muted-foreground tabular">{pct(share)}</span>
                  </button>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </div>
  )
}
