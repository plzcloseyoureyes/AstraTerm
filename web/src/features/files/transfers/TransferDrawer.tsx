/*
 * Transfer queue UI (FILE-8): a bottom drawer above the status bar (registerOverlay) with per-transfer progress,
 * speed, ETA, cancel / retry / clear, plus the status bar item and the sidebar badge that open it.
 */
import { useEffect, useRef, useState } from 'react'
import {
  ArrowDownToLine,
  ArrowRightLeft,
  ArrowUpDown,
  ArrowUpFromLine,
  Ban,
  ChevronDown,
  ChevronRight,
  CircleCheck,
  CircleX,
  Eraser,
  FolderInput,
  RotateCw,
  Trash,
  X,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { IconButton } from '@/components/ui/icon-button'
import { StatusBarItem } from '@/layout/StatusBar'
import { refocusAfterDialog } from '../browser/controller'
import { useNow } from '@/lib/hooks'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, formatBytes, formatDuration, formatRate, formatRelativeTime, plural } from '@/lib/utils'
import {
  cancelTransfer,
  clearFinished,
  closeTransfers,
  removeTransfer,
  retryTransfer,
  summarize,
  toggleTransfers,
  useTransfersStore,
  useTransferViews,
  type TransferView,
} from './store'

const KIND_ICON = {
  upload: ArrowUpFromLine,
  download: ArrowDownToLine,
  copy: ArrowRightLeft,
  move: FolderInput,
} as const

const KIND_LABEL = { upload: 'Upload', download: 'Download', copy: 'Copy', move: 'Move' } as const

export function TransferDrawer({ locked }: { locked: boolean }) {
  const open = useTransfersStore((s) => s.drawerOpen)
  const views = useTransferViews()
  const ref = useRef<HTMLElement>(null)
  const sum = summarize(views)
  // Take the focus while open (Escape closes) and give it back to where it was (a file list, the terminal…).
  const returnTo = useRef<HTMLElement | null>(null)
  useEffect(() => {
    if (open) {
      const a = document.activeElement
      returnTo.current = a instanceof HTMLElement && a !== document.body ? a : null
      ref.current?.focus({ preventScroll: true })
      return
    }
    const back = returnTo.current
    returnTo.current = null
    if (back?.isConnected) refocusAfterDialog(() => back.focus({ preventScroll: true }))
  }, [open])
  if (!open || locked) return null
  const finished = views.filter((v) => v.state !== 'queued' && v.state !== 'running').length
  return (
    <section
      ref={ref}
      tabIndex={-1}
      role="region"
      aria-label="Transfers"
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.stopPropagation()
          closeTransfers()
        }
      }}
      className={cn(
        // Bottom-right, above the status bar: the SFTP side panel on the left stays visible.
        'fixed right-2 bottom-7 z-40 flex max-h-[min(28rem,55vh)] w-[min(44rem,calc(100vw-1rem))] flex-col',
        'rounded-lg border bg-popover text-popover-foreground shadow-popover outline-none animate-in fade-in-0 slide-in-from-bottom-2 duration-150',
      )}
    >
      <header className="flex h-9 shrink-0 items-center gap-2 border-b pr-1.5 pl-3">
        <ArrowUpDown className="size-4 text-muted-foreground" aria-hidden />
        <h2 className="text-sm font-semibold">Transfers</h2>
        <span className="truncate text-xs text-muted-foreground">
          {sum.active
            ? `${sum.active} active · ${Math.round((sum.progress ?? 0) * 100)}%${sum.bytesPerSec ? ` · ${formatRate(sum.bytesPerSec)}` : ''}`
            : views.length
              ? plural(views.length, 'transfer')
              : 'Nothing yet'}
        </span>
        <span className="flex-1" />
        <Button variant="ghost" size="xs" disabled={!finished} onClick={() => void clearFinished()}>
          <Eraser /> Clear finished
        </Button>
        <IconButton icon={X} label="Close transfers" size="xs" onClick={closeTransfers} />
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {views.length === 0 ? (
          <div className="px-4 py-8 text-center text-sm text-muted-foreground">
            Uploads, downloads and copies between file systems appear here.
            <br />
            Drop files on the SFTP browser to upload them.
          </div>
        ) : (
          <ul className="divide-y" aria-label="Transfer list">
            {views.map((v) => (
              <TransferRow key={v.id} v={v} />
            ))}
          </ul>
        )}
      </div>
    </section>
  )
}

function TransferRow({ v }: { v: TransferView }) {
  const [expanded, setExpanded] = useState(false)
  const now = useNow(v.state === 'running' ? 1000 : 30_000)
  const Icon = KIND_ICON[v.kind]
  const active = v.state === 'queued' || v.state === 'running'
  const pct = v.totalBytes > 0 ? Math.min(100, (v.doneBytes / v.totalBytes) * 100) : v.state === 'done' ? 100 : 0
  const eta = v.state === 'running' && v.bytesPerSec > 0 && v.totalBytes > v.doneBytes ? ((v.totalBytes - v.doneBytes) / v.bytesPerSec) * 1000 : null
  const StateIcon = v.state === 'done' ? CircleCheck : v.state === 'error' ? CircleX : v.state === 'canceled' ? Ban : null
  return (
    <li className="grid gap-1 px-3 py-2">
      <div className="flex min-w-0 items-center gap-2">
        <Icon className={cn('size-4 shrink-0', v.state === 'error' ? 'text-destructive' : 'text-muted-foreground')} aria-hidden />
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-baseline gap-1.5">
            <span className="shrink-0 text-xs text-muted-foreground">{KIND_LABEL[v.kind]}</span>
            <span className="truncate text-sm font-medium" title={v.label}>
              {v.label}
            </span>
          </div>
          <div className="truncate text-xs text-muted-foreground" title={v.detail}>
            {v.detail}
          </div>
        </div>
        <div className="shrink-0 text-right text-xs tabular text-muted-foreground">
          {active ? (
            <>
              <div className={cn(v.note && 'text-warning')}>
                {v.note ? v.note : v.state === 'queued' ? 'Queued' : `${Math.round(pct)}%`}
                {v.bytesPerSec > 0 && ` · ${formatRate(v.bytesPerSec)}`}
              </div>
              <div>
                {formatBytes(v.doneBytes)} / {formatBytes(v.totalBytes)}
                {eta != null && ` · ${formatDuration(eta)} left`}
              </div>
            </>
          ) : (
            <div className={cn('flex items-center justify-end gap-1', v.state === 'error' && 'text-destructive', v.state === 'done' && 'text-success')}>
              {StateIcon && <StateIcon className="size-3.5" aria-hidden />}
              <span>
                {v.state === 'done'
                  ? v.kind === 'download'
                    ? 'Sent to browser'
                    : `${v.totalBytes ? formatBytes(v.totalBytes) : plural(v.totalFiles || 1, 'item')}`
                  : v.state === 'canceled'
                    ? 'Canceled'
                    : 'Failed'}
              </span>
              {v.finishedAt && <span className="text-muted-foreground">· {formatRelativeTime(v.finishedAt, now)}</span>}
            </div>
          )}
        </div>
        <div className="flex shrink-0 items-center">
          {v.canCancel && <IconButton icon={X} label="Cancel" size="xs" onClick={() => void cancelTransfer(v)} />}
          {v.canRetry && <IconButton icon={RotateCw} label={v.kind === 'download' ? 'Download again' : 'Retry'} size="xs" onClick={() => void retryTransfer(v)} />}
          {!active && <IconButton icon={Trash} label="Remove from the list" size="xs" onClick={() => void removeTransfer(v)} />}
        </div>
      </div>
      {(active || v.state === 'error') && (
        <div
          className="h-1 overflow-hidden rounded-full bg-muted"
          role="progressbar"
          aria-label={`${KIND_LABEL[v.kind]} ${v.label}`}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(pct)}
        >
          {/* Queued: an empty track (nothing moves until it runs); running: a smoothed, never-receding bar. */}
          {v.state !== 'queued' && (
            <div
              className={cn('h-full rounded-full transition-[width] duration-500 ease-out', v.state === 'error' ? 'bg-destructive' : v.note ? 'bg-warning' : 'bg-primary')}
              style={{ width: `${pct}%` }}
            />
          )}
        </div>
      )}
      {active && v.currentFile && (
        <div className="truncate font-mono text-2xs text-muted-foreground" title={v.currentFile}>
          {v.totalFiles > 1 && `${v.doneFiles}/${v.totalFiles} · `}
          {v.currentFile}
        </div>
      )}
      {v.state === 'error' && (
        <div className="text-xs text-destructive">
          {v.problems && v.problems.length > 1 ? (
            <button type="button" className="flex items-center gap-1 outline-none hover:underline focus-visible:underline" onClick={() => setExpanded((x) => !x)} aria-expanded={expanded}>
              {expanded ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
              {v.error}
            </button>
          ) : (
            <span className="break-words">{v.problems?.[0] ? `${v.problems[0].name}: ${v.problems[0].error}` : v.error}</span>
          )}
          {expanded && v.problems && (
            <ul className="mt-1 max-h-32 overflow-y-auto pl-4 text-muted-foreground">
              {v.problems.map((p) => (
                <li key={p.name} className="truncate">
                  <span className="font-mono">{p.name}</span>: {p.error}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  )
}

/** Status bar: active transfers (count, overall %, speed), failures; click opens the queue. */
export function TransfersStatusItem() {
  const views = useTransferViews()
  const open = useTransfersStore((s) => s.drawerOpen)
  const sum = summarize(views)
  // A transfer that finishes quickly never flashes its progress here (300 ms / ≥ 600 ms, docs/UX.md).
  const showActive = useDelayedFlag(sum.active > 0)
  if (!views.length) return null
  if (showActive) {
    const pct = sum.active ? Math.round((sum.progress ?? 0) * 100) : 100
    return (
      <StatusBarItem
        icon={ArrowUpDown}
        tone="accent"
        tooltip={`${plural(sum.active, 'active transfer')} — ${formatBytes(sum.doneBytes)} of ${formatBytes(sum.totalBytes)}${sum.failed ? ` · ${sum.failed} failed` : ''}. Click to show the queue.`}
        onClick={toggleTransfers}
        aria-label="Transfers"
      >
        <span className="tabular">
          {sum.active ? `${sum.active} · ` : ''}
          {pct}%{sum.active && sum.bytesPerSec ? ` · ${formatRate(sum.bytesPerSec)}` : ''}
        </span>
      </StatusBarItem>
    )
  }
  if (sum.failed) {
    return (
      <StatusBarItem icon={CircleX} tone="danger" tooltip="Some transfers failed — click for details" onClick={toggleTransfers} aria-label="Failed transfers">
        {sum.failed} failed
      </StatusBarItem>
    )
  }
  return <StatusBarItem icon={ArrowUpDown} tone={open ? 'accent' : 'muted'} tooltip="Transfers" onClick={toggleTransfers} aria-label="Transfers" />
}

/** Sidebar rail badge on the SFTP icon: number of active transfers. */
export function TransfersBadge() {
  const views = useTransferViews()
  const active = views.filter((v) => v.state === 'queued' || v.state === 'running').length
  // A quick transfer (saving an edited file) never flashes the badge: it shows after 300 ms, then for ≥ 600 ms.
  const shown = useDelayedFlag(active > 0)
  const last = useRef(active)
  if (active) last.current = active
  const n = active || last.current
  if (!shown) return null
  return (
    <span className="flex h-3.5 min-w-3.5 items-center justify-center rounded-full bg-primary px-0.5 text-[9px] leading-none font-semibold text-primary-foreground tabular" aria-label={`${n} active transfers`}>
      {n > 9 ? '9+' : n}
    </span>
  )
}
