import { LayoutGrid, ListRestart } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { generalSettings } from '@/stores/settings'
import { clearClosedTabs, resetSavedLayout, useClosedTabs } from '@/stores/workspace'
import { SettingRow, SettingsGroup, SettingsPage } from '../ui'

export default function GeneralSection() {
  const g = generalSettings.use()
  const closed = useClosedTabs()
  return (
    <SettingsPage title="General" description="Workspace behaviour and safety prompts.">
      <SettingsGroup title="Workspace">
        <SettingRow
          label="Restore workspace on start"
          description="Reopen your tabs, splits and floating windows when Termstead loads. Running sessions are re-attached."
          htmlFor="general-restore"
        >
          <Switch id="general-restore" checked={g.restoreWorkspace} onCheckedChange={(v) => generalSettings.set({ restoreWorkspace: v })} />
        </SettingRow>
        <SettingRow label="Saved layout" description="Forget the saved tab layout of this browser. The current tabs stay open.">
          <Button
            variant="secondary"
            size="sm"
            onClick={async () => {
              if (await confirm({ title: 'Reset saved layout?', description: 'The next reload starts with the Home tab.', confirmLabel: 'Reset' })) {
                resetSavedLayout()
                toast.success('Saved layout cleared')
              }
            }}
          >
            <LayoutGrid /> Reset layout
          </Button>
        </SettingRow>
        <SettingRow label="Recently closed tabs" description={`${closed.length} tab${closed.length === 1 ? '' : 's'} in history (reopen with the Reopen Closed Tab command).`}>
          <Button variant="secondary" size="sm" disabled={!closed.length} onClick={() => clearClosedTabs()}>
            <ListRestart /> Clear history
          </Button>
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Tabs & sessions">
        <SettingRow
          label="Confirm before closing running sessions"
          description="Ask before closing a tab whose session is still connected. Off: the tab closes at once and Undo brings the session back for a few seconds."
          htmlFor="general-confirm-close"
        >
          <Switch id="general-confirm-close" checked={g.confirmCloseRunning} onCheckedChange={(v) => generalSettings.set({ confirmCloseRunning: v })} />
        </SettingRow>
        <SettingRow label="Open links from terminals" description="What happens when you click a URL printed in a terminal.">
          <SimpleSelect
            aria-label="Open links from terminals"
            size="sm"
            className="w-44"
            value={g.openLinks}
            onValueChange={(openLinks) => generalSettings.set({ openLinks })}
            options={[
              { value: 'newTab', label: 'In a new browser tab' },
              { value: 'ask', label: 'Ask every time' },
            ]}
          />
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
