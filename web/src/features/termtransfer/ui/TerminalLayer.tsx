/*
 * What the term-transfer feature draws inside one terminal pane (portalled there by the overlay, so it follows the
 * tab into pop-out windows): the prompt card (where to save / what to upload), the transfer card (progress, cancel,
 * result) and the drop zones while OS files are dragged over the terminal.
 */
import { useEffect, useId, useRef } from 'react'
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  Ban,
  CircleAlert,
  CircleCheck,
  ClipboardPaste,
  FolderInput,
  HardDriveUpload,
  Info,
  Send,
  TriangleAlert,
  Upload,
  X,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { ProgressBar } from '@/components/ui/progress'
import { Spinner } from '@/components/ui/spinner'
import { cn, formatBytes, formatDuration, formatRate } from '@/lib/utils'
import { getController } from '../instances'
import { setPromptRemember } from '../store'
import type { DragView, DropZone, DropZoneId, PromptView, TermUi, TransferView } from '../types'

export function TerminalLayer({ ui }: { ui: TermUi }) {
  const showStack = !!(ui.prompt || ui.transfer || (ui.follower && !ui.transfer))
  return (
    <>
      {ui.drag && <DropOverlay drag={ui.drag} />}
      {showStack && (
        <div className="absolute right-3 bottom-3 flex w-[min(25rem,calc(100%-1.5rem))] flex-col items-stretch gap-2">
          {ui.prompt && <PromptCard tabId={ui.tabId} prompt={ui.prompt} />}
          {ui.transfer && <TransferCard tabId={ui.tabId} t={ui.transfer} />}
          {ui.follower && !ui.transfer && !ui.prompt && (
            <div className="pointer-events-auto ml-auto flex items-center gap-1.5 rounded-md border bg-popover/95 px-2.5 py-1.5 text-xs text-muted-foreground shadow-popover" role="status">
              <Info className="size-3.5 shrink-0" aria-hidden />
              A file transfer of this session runs in another view.
            </div>
          )}
        </div>
      )}
    </>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// prompt
// ---------------------------------------------------------------------------------------------------------------------

function PromptCard({ tabId, prompt }: { tabId: string; prompt: PromptView }) {
  const primaryRef = useRef<HTMLButtonElement>(null)
  const titleId = useId()
  const descId = useId()
  const rememberId = useId()

  // Keyboard users: when the terminal had focus, move it to the primary button (Tab / Enter / Esc work there).
  useEffect(() => {
    const btn = primaryRef.current
    const active = btn?.ownerDocument.activeElement
    if (btn && active && (active.closest('.xterm') || active === btn.ownerDocument.body)) btn.focus({ preventScroll: true })
  }, [prompt.id])

  const Icon = prompt.kind === 'download' ? ArrowDownToLine : prompt.kind === 'upload' ? ArrowUpFromLine : prompt.kind === 'error' ? CircleAlert : Info
  const primary = prompt.actions[prompt.primary]
  return (
    <div
      role="alertdialog"
      aria-labelledby={titleId}
      aria-describedby={prompt.message ? descId : undefined}
      className="pointer-events-auto grid gap-2.5 rounded-lg border bg-popover p-3 text-popover-foreground shadow-popover animate-in fade-in-0 slide-in-from-bottom-1 duration-150"
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault()
          e.stopPropagation()
          prompt.cancel()
        }
      }}
    >
      <div className="flex items-start gap-2.5">
        <div className={cn('mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-md', prompt.kind === 'error' ? 'bg-destructive/15 text-destructive' : 'bg-primary/15 text-primary')}>
          <Icon className="size-4" aria-hidden />
        </div>
        <div className="grid min-w-0 flex-1 gap-0.5">
          <div id={titleId} className="text-base font-semibold leading-tight">
            {prompt.title}
          </div>
          {prompt.message && (
            <p id={descId} className="text-sm text-muted-foreground">
              {prompt.message}
            </p>
          )}
          {prompt.note && (
            <p className="mt-1 flex gap-1.5 text-xs text-warning">
              <TriangleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
              <span>{prompt.note}</span>
            </p>
          )}
        </div>
        <Button size="icon-xs" variant="ghost" aria-label="Cancel" title="Cancel (Esc)" onClick={() => prompt.cancel()}>
          <X />
        </Button>
      </div>
      {prompt.acceptDrop && (
        <div className="flex items-center gap-2 rounded-md border border-dashed px-2.5 py-2 text-xs text-muted-foreground">
          <Upload className="size-3.5 shrink-0" aria-hidden />
          Drop files anywhere on the terminal to upload them.
        </div>
      )}
      {prompt.rememberLabel && (
        <div className="flex items-center gap-2">
          <Checkbox id={rememberId} checked={!!prompt.remember} onCheckedChange={(v) => setPromptRemember(tabId, v === true)} />
          <label htmlFor={rememberId} className="text-sm select-none">
            {prompt.rememberLabel}
          </label>
        </div>
      )}
      <div className="flex flex-wrap items-center justify-end gap-1.5">
        {prompt.actions.map((a, i) => (
          <Button
            key={a.id}
            ref={i === prompt.primary ? primaryRef : undefined}
            size="sm"
            variant={a.variant ?? (i === prompt.primary ? 'default' : 'secondary')}
            onClick={() => a.run({ remember: !!prompt.remember })}
          >
            {a.label}
          </Button>
        ))}
      </div>
      <div className="-mt-1 text-right text-2xs text-muted-foreground">{primary ? `Enter: ${primary.label.replace(/…$/, '')} · Esc: cancel` : 'Esc: cancel'}</div>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// transfer progress
// ---------------------------------------------------------------------------------------------------------------------

function TransferCard({ tabId, t }: { tabId: string; t: TransferView }) {
  const running = t.phase === 'running'
  // Monotonic overall progress from the controller (never per-file: that would restart at 0 on every file).
  const pct = t.pct != null ? t.pct * 100 : null
  const eta = running && t.rate > 0 && t.total ? ((t.total - t.done) / t.rate) * 1000 : null
  const Icon = t.phase === 'done' ? CircleCheck : t.phase === 'error' ? CircleAlert : t.phase === 'canceled' ? Ban : t.protocol === 'send' ? Send : t.direction === 'download' ? ArrowDownToLine : ArrowUpFromLine
  const amount =
    t.unit === 'lines'
      ? `${t.done.toLocaleString()}${t.total != null ? ` / ${t.total.toLocaleString()}` : ''} lines`
      : `${formatBytes(t.done)}${t.total ? ` / ${formatBytes(t.total)}` : ''}`
  const rate = t.unit === 'lines' ? `${Math.round(t.rate)} lines/s` : formatRate(t.rate)
  return (
    <div
      role="status"
      aria-live="polite"
      className="pointer-events-auto grid gap-1.5 rounded-lg border bg-popover/95 p-3 text-popover-foreground shadow-popover backdrop-blur-sm animate-in fade-in-0 slide-in-from-bottom-1 duration-150"
    >
      <div className="flex min-w-0 items-center gap-2">
        <Icon
          className={cn(
            'size-4 shrink-0',
            t.phase === 'done' ? 'text-success' : t.phase === 'error' ? 'text-destructive' : t.phase === 'canceled' ? 'text-muted-foreground' : 'text-primary',
          )}
          aria-hidden
        />
        <span className="min-w-0 truncate text-sm font-medium" title={t.title}>
          {t.title}
        </span>
        {running && (
          <span className="ml-auto flex w-9 shrink-0 justify-end text-xs tabular text-muted-foreground">
            {/* Unknown total: a calm ring instead of an indeterminate bar (the byte count below still moves). */}
            {pct != null ? `${Math.floor(pct)}%` : <Spinner immediate className="size-3.5" label="Transferring" />}
          </span>
        )}
        {running ? (
          <Button size="xs" variant="ghost" onClick={() => getController(tabId)?.cancelActive()} title="Cancel (Ctrl+C)">
            Cancel
          </Button>
        ) : (
          <Button size="icon-xs" variant="ghost" className="ml-auto" aria-label="Dismiss" title="Dismiss" onClick={() => getController(tabId)?.dismissCard()}>
            <X />
          </Button>
        )}
      </div>
      {t.file && (
        <div className="truncate font-mono text-xs text-muted-foreground" title={t.file}>
          {t.fileCount && t.fileCount > 1 ? `${t.fileIndex ?? 1}/${t.fileCount} · ` : ''}
          {t.file}
        </div>
      )}
      {((running && t.pct != null) || t.phase === 'error') && (
        // A failed transfer keeps where it got to (full if the total was unknown).
        <ProgressBar value={t.pct ?? 1} resetKey={t.id} tone={t.phase === 'error' ? 'destructive' : 'primary'} label={t.title} />
      )}
      {running ? (
        <div className="flex gap-2 text-2xs tabular text-muted-foreground">
          <span>{amount}</span>
          <span className="ml-auto">
            {t.rate > 0 ? rate : ''}
            {eta != null && eta < 86_400_000 ? ` · ${formatDuration(eta)} left` : ''}
          </span>
        </div>
      ) : (
        t.message && <div className={cn('text-xs break-words', t.phase === 'error' ? 'text-destructive' : 'text-muted-foreground')}>{t.message}</div>
      )}
      {running && (
        <div className="text-2xs text-muted-foreground">
          {t.target ? `Saving to ${t.target} · ` : ''}
          {t.warnings ? `${t.warnings} prompt warning${t.warnings === 1 ? '' : 's'} · ` : ''}Ctrl+C cancels · typing is paused
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// drop zones
// ---------------------------------------------------------------------------------------------------------------------

const ZONE_ICON: Record<DropZoneId, typeof Upload> = {
  sftp: HardDriveUpload,
  local: FolderInput,
  trz: Upload,
  rz: ArrowUpFromLine,
  paste: ClipboardPaste,
}

function DropOverlay({ drag }: { drag: DragView }) {
  const [main, ...rest] = drag.zones
  const hover = drag.hover ?? main?.id ?? null
  return (
    <div
      className="pointer-events-auto absolute inset-0 flex flex-col items-center justify-center-safe gap-[clamp(0.25rem,3cqh,0.75rem)] overflow-y-auto bg-background/75 p-[clamp(0.5rem,4cqh,1rem)] backdrop-blur-[2px] [container-type:size]"
      aria-hidden
    >
      {drag.blocked || !main ? (
        <div className="flex items-center gap-2 rounded-lg border bg-popover px-4 py-3 text-sm text-muted-foreground shadow-popover">
          <Ban className="size-4 shrink-0" />
          {drag.blocked ?? 'Files cannot be dropped here'}
        </div>
      ) : (
        <>
          <Zone zone={main} big active={hover === main.id} />
          {rest.length > 0 && (
            <div className="grid w-full max-w-xl grid-cols-[repeat(auto-fit,minmax(8.5rem,1fr))] gap-2">
              {rest.map((z) => (
                <Zone key={z.id} zone={z} active={hover === z.id} />
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}

function Zone({ zone, big, active }: { zone: DropZone; big?: boolean; active: boolean }) {
  const Icon = ZONE_ICON[zone.id]
  return (
    <div
      data-tt-zone={zone.id}
      className={cn(
        'flex min-w-0 flex-col items-center justify-center gap-1 rounded-lg border-2 border-dashed text-center transition-colors',
        big ? 'w-full max-w-xl px-4 py-[clamp(0.375rem,7cqh,1.75rem)]' : 'px-2 py-[clamp(0.25rem,3cqh,0.75rem)]',
        active ? 'border-primary bg-primary/12 text-foreground' : 'border-border bg-popover/85 text-muted-foreground',
      )}
    >
      <Icon className={cn(big ? 'size-[clamp(1rem,8cqh,1.75rem)]' : 'size-[clamp(0.875rem,5cqh,1.25rem)]', active && 'text-primary')} />
      <div className={cn('max-w-full truncate font-medium', big ? 'text-md' : 'text-sm')}>{zone.label}</div>
      {zone.detail && <div className="max-w-full truncate text-2xs">{zone.detail}</div>}
    </div>
  )
}
