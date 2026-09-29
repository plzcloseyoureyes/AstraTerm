/*
 * Settings → Servers (per-user preferences; the server configurations themselves live in the servers tab).
 */
import { Server } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { NumberInput } from '@/components/ui/number-input'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { openServersTab } from './actions'
import { serversSettings } from './settings'

export default function ServersSettingsSection() {
  const s = serversSettings.use()
  return (
    <SettingsPage
      title="Servers"
      description="Embedded HTTP, FTP, SFTP, TFTP, Telnet and Syslog servers."
      actions={
        <Button size="sm" variant="secondary" onClick={openServersTab}>
          <Server /> Open servers
        </Button>
      }
    >
      <SettingsGroup title="Server manager">
        <SettingRow label="Status bar indicator" description="Running servers in the status bar." htmlFor="servers-status">
          <Switch id="servers-status" checked={s.showStatusItem} onCheckedChange={(v) => serversSettings.set({ showStatusItem: v })} />
        </SettingRow>
        <SettingRow
          label="Confirm stopping a server with clients"
          description="Ask before disconnecting connected clients."
          htmlFor="servers-confirm"
        >
          <Switch
            id="servers-confirm"
            checked={s.confirmStopWithClients}
            onCheckedChange={(v) => serversSettings.set({ confirmStopWithClients: v })}
          />
        </SettingRow>
      </SettingsGroup>
      <SettingsGroup title="Syslog viewer">
        <SettingRow label="Messages kept in the viewer" description="Older messages stay on the server and load on demand." htmlFor="servers-syslog-limit">
          <NumberInput
            id="servers-syslog-limit"
            className="w-32"
            inputSize="sm"
            value={s.syslogViewerLimit}
            min={500}
            max={100000}
            step={500}
            onChange={(v) => serversSettings.set({ syslogViewerLimit: v ?? 5000 })}
          />
        </SettingRow>
        <SettingRow label="Follow new messages" htmlFor="servers-syslog-follow">
          <Switch id="servers-syslog-follow" checked={s.syslogAutoScroll} onCheckedChange={(v) => serversSettings.set({ syslogAutoScroll: v })} />
        </SettingRow>
        <SettingRow label="Details of the selected message" htmlFor="servers-syslog-details">
          <Switch id="servers-syslog-details" checked={s.syslogShowDetails} onCheckedChange={(v) => serversSettings.set({ syslogShowDetails: v })} />
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
