/*
 * Settings → Sessions: tree preferences, "Connect all" confirmation, default protocol, quick-connect history.
 */
import { Eraser, FolderTree } from 'lucide-react'
import { toast } from 'sonner'
import { protocolEditors } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { storage } from '@/lib/utils'
import { sessionsSettings } from './settings'
import type { SortMode } from './types'

const SORT_OPTIONS: { value: SortMode; label: string }[] = [
  { value: 'manual', label: 'Manual (drag to reorder)' },
  { value: 'name', label: 'Name' },
  { value: 'recent', label: 'Recently used' },
  { value: 'protocol', label: 'Protocol' },
  { value: 'host', label: 'Host' },
]

export default function SessionsSettingsSection() {
  const s = sessionsSettings.use()
  const editors = protocolEditors.useList()
  const history = Array.isArray(s.quickConnectHistory) ? s.quickConnectHistory.length : 0
  return (
    <SettingsPage title="Sessions" description="The saved-session tree, quick connect and opening behaviour.">
      <SettingsGroup title="Session tree">
        <SettingRow label="Sort sessions by" description="Folders are listed first. Drag & drop reordering needs the manual order.">
          <SimpleSelect
            aria-label="Sort sessions by"
            size="sm"
            className="w-52"
            value={s.sortMode}
            onValueChange={(sortMode) => sessionsSettings.set({ sortMode })}
            options={SORT_OPTIONS}
          />
        </SettingRow>
        <SettingRow label="Favorites section" description="Pinned sessions above the tree." htmlFor="sessions-favorites">
          <Switch id="sessions-favorites" checked={s.showFavorites} onCheckedChange={(v) => sessionsSettings.set({ showFavorites: v })} />
        </SettingRow>
        <SettingRow label="Recent section" description="The ten most recently used sessions." htmlFor="sessions-recent">
          <Switch id="sessions-recent" checked={s.showRecent} onCheckedChange={(v) => sessionsSettings.set({ showRecent: v })} />
        </SettingRow>
        <SettingRow label="Expanded folders" description={`${s.expanded.length} folder${s.expanded.length === 1 ? '' : 's'} remembered as open.`}>
          <Button variant="secondary" size="sm" disabled={!s.expanded.length} onClick={() => sessionsSettings.set({ expanded: [] })}>
            <FolderTree /> Collapse all
          </Button>
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Opening sessions">
        <SettingRow label="Default type of new sessions" description="Pre-selected in the session editor (it remembers your last choice).">
          <SimpleSelect
            aria-label="Default session type"
            size="sm"
            className="w-44"
            value={s.lastProtocol}
            onValueChange={(lastProtocol) => sessionsSettings.set({ lastProtocol })}
            options={editors.map((e) => ({ value: e.protocol as string, label: e.label }))}
          />
        </SettingRow>
        <SettingRow label="Confirm “Connect all” above" description="Ask before opening more than this many sessions at once (0 = never ask).">
          <NumberInput
            aria-label="Connect all confirmation threshold"
            className="w-28"
            inputSize="sm"
            value={s.connectAllConfirm}
            min={0}
            max={500}
            onChange={(n) => sessionsSettings.set({ connectAllConfirm: n ?? 0 })}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Quick connect">
        <SettingRow label="History" description={`${history} saved entr${history === 1 ? 'y' : 'ies'}. Inline passwords are never kept.`}>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              sessionsSettings.set({ quickConnectHistory: [] })
              // The ribbon field's own drop-down history (browser-local).
              storage.remove('termstead:quickconnect-history')
              toast.success('Quick connect history cleared')
            }}
          >
            <Eraser /> Clear history
          </Button>
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
