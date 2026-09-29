/* Strips above the editor of an "editor" tab: the banners (recovered changes, server changes, save problems, notes) and
 * the compare view's toolbar. */
import { useMemo } from 'react'
import { ArrowDown, ArrowUp, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { IconButton } from '@/components/ui/icon-button'
import { formatDateTime, formatRelativeTime } from '@/lib/utils'
import type { BackupRecord, BannerState, MergeState } from './tabstate'
import { Banner } from './ui'

export function EditorBanners(p: {
  name: string
  dirty: boolean
  banner: BannerState | null
  note: string | null
  lockedRO: string | null
  backup: BackupRecord | null
  canReconnect: boolean
  onReload: () => void
  onCompare: () => void
  onIgnore: () => void
  onOverwrite: () => void
  /** Undefined: the file system cannot write through sudo. */
  onSudo?: () => void
  onSaveAs: () => void
  onRecreate: () => void
  onReconnect: () => void
  onDismissNote: () => void
  onRestore: () => void
  onDiscardBackup: () => void
  onClose: () => void
}) {
  const b = p.banner
  const when = useMemo(() => (p.backup ? formatRelativeTime(p.backup.savedAt) : ''), [p.backup])
  return (
    <>
      {p.backup && (
        <Banner
          tone="warning"
          actions={[
            { label: 'Discard', onClick: p.onDiscardBackup },
            { label: 'Restore', onClick: p.onRestore, primary: true },
          ]}
        >
          Unsaved changes to {p.name} from {when} were recovered.
        </Banner>
      )}
      {b?.kind === 'changed' && (
        <Banner
          tone="warning"
          actions={[
            { label: 'Ignore', onClick: p.onIgnore },
            { label: 'Compare', onClick: p.onCompare, primary: p.dirty },
            { label: 'Reload', onClick: p.onReload, primary: !p.dirty },
          ]}
        >
          {p.name} was changed on the server{b.mtime ? ` (${formatDateTime(b.mtime)})` : ''}.{p.dirty ? ' You also have unsaved changes.' : ''}
        </Banner>
      )}
      {b?.kind === 'deleted' && (
        <Banner tone="warning" actions={[{ label: 'Close tab', onClick: p.onClose }, { label: 'Save to create it', onClick: p.onRecreate, primary: true }]}>
          {p.name} does not exist on the server. Saving creates it.
        </Banner>
      )}
      {b?.kind === 'gone' && (
        <Banner tone="danger" actions={p.canReconnect ? [{ label: 'Reconnect', onClick: p.onReconnect, primary: true }] : []}>
          The file system connection for {p.name} is no longer available.{p.canReconnect ? '' : ' Reopen the file from the file browser to save it (or use Save as).'}
        </Banner>
      )}
      {b?.kind === 'conflict' && (
        <Banner
          tone="danger"
          actions={[
            { label: 'Reload', onClick: p.onReload },
            { label: 'Overwrite', onClick: p.onOverwrite },
            { label: 'Compare & merge', onClick: p.onCompare, primary: true },
          ]}
        >
          Not saved: {p.name} was changed on the server since you opened it.
        </Banner>
      )}
      {b?.kind === 'denied' && (
        <Banner
          tone="danger"
          actions={p.onSudo ? [{ label: 'Save as…', onClick: p.onSaveAs }, { label: 'Save with sudo', onClick: p.onSudo, primary: true }] : [{ label: 'Save as…', onClick: p.onSaveAs, primary: true }]}
        >
          Not saved: permission denied ({b.message}).
        </Banner>
      )}
      {b?.kind === 'error' && (
        <Banner tone="danger" onDismiss={p.onIgnore}>
          {b.message}
        </Banner>
      )}
      {p.lockedRO && <Banner tone="info">{p.lockedRO} The editor is read-only.</Banner>}
      {p.note && (
        <Banner tone="info" onDismiss={p.onDismissNote}>
          {p.note} Use the encoding menu in the status bar to reopen it with another encoding.
        </Banner>
      )}
    </>
  )
}

export function MergeToolbar({
  merge,
  chunks,
  onPrev,
  onNext,
  onTakeServer,
  onApply,
  onSave,
  onCancel,
}: {
  merge: MergeState
  /** Number of changes; -1 while the first comparison is being computed. */
  chunks: number
  onPrev: () => void
  onNext: () => void
  onTakeServer: () => void
  onApply: () => void
  onSave: () => void
  onCancel: () => void
}) {
  const title =
    merge.reason === 'compare' ? 'Compare with the server version' : merge.reason === 'conflict' ? 'Resolve the conflict' : 'The file changed on the server'
  return (
    <div role="toolbar" aria-label="Compare" className="flex h-9 shrink-0 items-center gap-1.5 overflow-hidden border-b bg-info/10 px-2">
      <span className="shrink-0 text-sm font-medium">{title}</span>
      <span className="shrink-0 text-xs text-muted-foreground tabular-nums" aria-live="polite">
        {chunks < 0 ? 'Comparing…' : chunks === 0 ? 'No differences' : `${chunks} change${chunks === 1 ? '' : 's'}`}
      </span>
      <IconButton icon={ArrowUp} label="Previous change" size="sm" onClick={onPrev} disabled={chunks <= 0} />
      <IconButton icon={ArrowDown} label="Next change" size="sm" onClick={onNext} disabled={chunks <= 0} />
      <span className="hidden min-w-0 flex-1 truncate text-xs text-muted-foreground @xl:inline">Use the arrows between the two sides to take a change from the server.</span>
      <div className="min-w-2 flex-1 @xl:hidden" />
      <Button size="xs" variant="secondary" className="shrink-0" onClick={onTakeServer}>
        Use server version
      </Button>
      <Button size="xs" variant="secondary" className="shrink-0" onClick={onApply}>
        Apply to editor
      </Button>
      <Button size="xs" className="shrink-0" onClick={onSave}>
        Apply & save
      </Button>
      <IconButton icon={X} label="Close comparison" size="sm" onClick={onCancel} />
    </div>
  )
}
