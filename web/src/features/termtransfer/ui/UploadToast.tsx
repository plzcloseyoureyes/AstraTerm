/*
 * Progress toast of a drop / menu upload through the Files API (FILE-22): live progress with Cancel, then the result
 * (with "Show in SFTP browser" for SSH sessions when the files feature provides it).
 */
import { useEffect } from 'react'
import { toast } from 'sonner'
import { CircleAlert, CircleCheck, FolderSync, HardDriveUpload, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ProgressBar } from '@/components/ui/progress'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, formatBytes, formatDuration, formatRate, plural } from '@/lib/utils'
import { dropUpload, useTransferStore } from '../store'
import { cancelUpload } from '../upload'

export interface UploadToastOptions {
  /** Show the destination folder in the file browser. */
  reveal?: (path: string) => void
}

export function showUploadToast(id: string, opts: UploadToastOptions = {}): void {
  toast.custom((toastId) => <UploadToast id={id} toastId={toastId} reveal={opts.reveal} />, {
    id: `ttup:${id}`,
    duration: Number.POSITIVE_INFINITY,
    className: 'border px-4',
    onDismiss: () => {
      cancelUpload(id)
      dropUpload(id)
    },
  })
}

function UploadToast({ id, toastId, reveal }: { id: string; toastId: string | number; reveal?: (path: string) => void }) {
  const u = useTransferStore((s) => s.uploads[id])
  const phase = u?.phase
  // The bar appears only for uploads longer than 300 ms and then stays its minimum time (its space is reserved meanwhile).
  const showBar = useDelayedFlag(phase === 'running' || phase === 'preparing')

  useEffect(() => {
    if (phase !== 'done' && phase !== 'canceled') return
    const t = setTimeout(() => toast.dismiss(toastId), phase === 'done' ? 7000 : 2500)
    return () => clearTimeout(t)
  }, [phase, toastId])

  if (!u) return null
  const running = u.phase === 'running' || u.phase === 'preparing'
  const fraction = u.total > 0 ? u.bytes / u.total : u.phase === 'done' ? 1 : 0
  const eta = running && u.rate > 0 && u.total > u.bytes ? ((u.total - u.bytes) / u.rate) * 1000 : null
  const Icon = u.phase === 'done' ? CircleCheck : u.phase === 'error' ? CircleAlert : HardDriveUpload
  const title =
    u.phase === 'done'
      ? `Uploaded ${u.label}`
      : u.phase === 'error'
        ? `Upload of ${u.label} failed`
        : u.phase === 'canceled'
          ? 'Upload canceled'
          : u.phase === 'preparing'
            ? `Preparing upload of ${u.label}…`
            : `Uploading ${u.label}`
  return (
    <div className="grid w-full gap-1.5 font-sans text-popover-foreground" role="status" aria-live="polite">
      <div className="flex min-w-0 items-start gap-2">
        <Icon className={cn('mt-0.5 size-4 shrink-0', u.phase === 'done' ? 'text-success' : u.phase === 'error' ? 'text-destructive' : 'text-primary')} aria-hidden />
        <div className="grid min-w-0 flex-1 gap-0.5">
          <div className="truncate text-sm font-medium" title={title}>
            {title}
          </div>
          {u.dest && (
            <div className="truncate font-mono text-xs text-muted-foreground" title={u.dest}>
              → {u.dest}
            </div>
          )}
        </div>
        <Button
          size="icon-xs"
          variant="ghost"
          aria-label={running ? 'Cancel upload' : 'Dismiss'}
          title={running ? 'Cancel upload' : 'Dismiss'}
          onClick={() => {
            if (running) cancelUpload(id)
            else toast.dismiss(toastId)
          }}
        >
          <X />
        </Button>
      </div>
      {showBar || u.phase === 'error' ? (
        // While "preparing" (sizes still unknown) the bar waits, still, at 0 — no indeterminate motion.
        <ProgressBar value={fraction} resetKey={id} tone={u.phase === 'error' ? 'destructive' : 'primary'} label={title} />
      ) : (
        running && <div aria-hidden className="h-1" /> // holds the bar's space until it is due
      )}
      {running && u.phase === 'running' && (
        <div className="flex gap-2 text-2xs tabular text-muted-foreground">
          <span className="truncate">
            {u.files > 1 ? `${Math.min(u.files, u.doneFiles + 1)}/${u.files} · ` : ''}
            {formatBytes(u.bytes)} / {formatBytes(u.total)}
          </span>
          <span className="ml-auto shrink-0">
            {u.rate > 0 ? formatRate(u.rate) : ''}
            {eta != null ? ` · ${formatDuration(eta)} left` : ''}
          </span>
        </div>
      )}
      {u.phase === 'done' && (
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <span>
            {plural(u.files, 'file')} · {formatBytes(u.total)}
          </span>
          {reveal && u.dest && (
            <Button
              size="xs"
              variant="secondary"
              className="ml-auto"
              onClick={() => {
                reveal(u.dest)
                toast.dismiss(toastId)
              }}
            >
              <FolderSync /> Show in SFTP browser
            </Button>
          )}
        </div>
      )}
      {u.phase === 'error' && u.error && <div className="text-xs break-words text-destructive">{u.error}</div>}
    </div>
  )
}
