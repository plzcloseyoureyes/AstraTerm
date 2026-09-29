/*
 * Settings → Files & SFTP.
 */
import { Eraser } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { clearRecentPaths, filesSettings, MAX_UPLOAD_PARALLELISM } from './settings'
import type { ConflictPolicy, DoubleClickAction } from './types'

const DOUBLE_CLICK: { value: DoubleClickAction; label: string }[] = [
  { value: 'edit', label: 'Edit (text files), view media' },
  { value: 'preview', label: 'Preview' },
  { value: 'download', label: 'Download' },
]

const CONFLICT: { value: ConflictPolicy; label: string }[] = [
  { value: 'ask', label: 'Ask' },
  { value: 'overwrite', label: 'Overwrite' },
  { value: 'skip', label: 'Skip existing' },
  { value: 'rename', label: 'Keep both (rename)' },
]

export default function FilesSettingsSection() {
  const s = filesSettings.use()
  const bookmarkCount = Object.values(s.bookmarks ?? {}).reduce((n, l) => n + (Array.isArray(l) ? l.length : 0), 0)
  return (
    <SettingsPage title="Files & SFTP" description="The SSH-browser side panel, file tabs, uploads and transfers.">
      <SettingsGroup title="SSH-browser (SFTP panel)">
        <SettingRow label="Show the SFTP panel when an SSH session connects" description="Switches the left sidebar to the session's files." htmlFor="files-autoshow">
          <Switch id="files-autoshow" checked={s.autoShowPanel} onCheckedChange={(v) => filesSettings.set({ autoShowPanel: v })} />
        </SettingRow>
        <SettingRow
          label="Follow terminal folder by default"
          description="The browser goes where the shell goes (cd). A session's “Follow SSH path” option can turn it off."
          htmlFor="files-follow"
        >
          <Switch id="files-follow" checked={s.followTerminal} onCheckedChange={(v) => filesSettings.set({ followTerminal: v })} />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Browsing">
        <SettingRow label="Show hidden files" description="Names starting with a dot." htmlFor="files-hidden">
          <Switch id="files-hidden" checked={s.showHidden} onCheckedChange={(v) => filesSettings.set({ showHidden: v })} />
        </SettingRow>
        <SettingRow
          label="Show partial uploads"
          description="“.termstead-part” files: uploads and transfers in progress, or left behind when one was interrupted."
          htmlFor="files-parts"
        >
          <Switch id="files-parts" checked={s.showPartialUploads} onCheckedChange={(v) => filesSettings.set({ showPartialUploads: v })} />
        </SettingRow>
        <SettingRow label="Folders first" htmlFor="files-folders-first">
          <Switch id="files-folders-first" checked={s.foldersFirst} onCheckedChange={(v) => filesSettings.set({ foldersFirst: v })} />
        </SettingRow>
        <SettingRow label="Double-click on a file" description="Folders always open. Media files open in the viewer when “Edit” is chosen.">
          <SimpleSelect
            aria-label="Double-click on a file"
            size="sm"
            className="w-60"
            value={s.doubleClickAction}
            onValueChange={(doubleClickAction) => filesSettings.set({ doubleClickAction })}
            options={DOUBLE_CLICK}
          />
        </SettingRow>
        <SettingRow label="Confirm before deleting" htmlFor="files-confirm-delete">
          <Switch id="files-confirm-delete" checked={s.confirmDelete} onCheckedChange={(v) => filesSettings.set({ confirmDelete: v })} />
        </SettingRow>
        <SettingRow label="Bookmarks and recent folders" description={`${bookmarkCount} bookmark${bookmarkCount === 1 ? '' : 's'}. Recent folders are kept in this browser.`}>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              clearRecentPaths()
              toast.success('Recent folders cleared')
            }}
          >
            <Eraser /> Clear recent folders
          </Button>
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Transfers">
        <SettingRow label="Parallel uploads" description="Files uploaded at the same time (8 MiB chunks, resumable).">
          <NumberInput
            aria-label="Parallel uploads"
            className="w-24"
            inputSize="sm"
            value={s.uploadParallelism}
            min={1}
            max={MAX_UPLOAD_PARALLELISM}
            onChange={(n) => filesSettings.set({ uploadParallelism: n ?? 3 })}
          />
        </SettingRow>
        <SettingRow label="When items already exist" description="For uploads, copies and moves.">
          <SimpleSelect
            aria-label="When items already exist"
            size="sm"
            className="w-48"
            value={s.conflictPolicy}
            onValueChange={(conflictPolicy) => filesSettings.set({ conflictPolicy })}
            options={CONFLICT}
          />
        </SettingRow>
        <SettingRow label="Open the transfer queue when a transfer starts" htmlFor="files-open-queue">
          <Switch id="files-open-queue" checked={s.openQueueOnTransfer} onCheckedChange={(v) => filesSettings.set({ openQueueOnTransfer: v })} />
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}

