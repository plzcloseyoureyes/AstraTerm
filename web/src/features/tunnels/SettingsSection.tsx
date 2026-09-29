/*
 * Settings → Tunnels.
 */
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { tunnelsSettings, type TunnelsSettings } from './settings'

export default function TunnelsSettingsSection() {
  const s = tunnelsSettings.use()
  return (
    <SettingsPage title="Tunnels" description="SSH port forwarding: the tunnel manager, session forwards and listening-port detection.">
      <SettingsGroup title="Tunnel manager">
        <SettingRow label="Confirm before deleting a tunnel" htmlFor="tunnels-confirm-delete">
          <Switch id="tunnels-confirm-delete" checked={s.confirmDelete} onCheckedChange={(v) => tunnelsSettings.set({ confirmDelete: v })} />
        </SettingRow>
        <SettingRow label="Notify when a tunnel fails" description="A toast when a tunnel stops with an error." htmlFor="tunnels-notify">
          <Switch id="tunnels-notify" checked={s.notifyErrors} onCheckedChange={(v) => tunnelsSettings.set({ notifyErrors: v })} />
        </SettingRow>
        <SettingRow label="Status bar indicator" description="Running tunnel count in the status bar." htmlFor="tunnels-status">
          <Switch id="tunnels-status" checked={s.showStatusItem} onCheckedChange={(v) => tunnelsSettings.set({ showStatusItem: v })} />
        </SettingRow>
        <SettingRow label="Show session forwards" description="Forwards that live with an open SSH session, below the saved tunnels." htmlFor="tunnels-session">
          <Switch id="tunnels-session" checked={s.showSessionForwards} onCheckedChange={(v) => tunnelsSettings.set({ showSessionForwards: v })} />
        </SettingRow>
        <SettingRow
          label="Open web services"
          description="“Open in browser” for tunnels to HTTP(S) services. The NexTerm web proxy also works when NexTerm runs on another machine."
        >
          <SimpleSelect<TunnelsSettings['openWith']>
            aria-label="Open web services"
            size="sm"
            className="w-64"
            value={s.openWith}
            onValueChange={(openWith) => tunnelsSettings.set({ openWith })}
            options={[
              { value: 'auto', label: 'Automatic (proxy in server mode)' },
              { value: 'proxy', label: 'Through the NexTerm web proxy' },
              { value: 'direct', label: 'Directly at the tunnel address' },
            ]}
          />
        </SettingRow>
      </SettingsGroup>
      <SettingsGroup title="Listening ports">
        <SettingRow
          label="Watch SSH sessions for new listening ports"
          description="While an SSH session is connected, NexTerm checks the server every few seconds and offers to forward ports that start listening (for example a dev server)."
          htmlFor="tunnels-watch"
        >
          <Switch id="tunnels-watch" checked={s.watchPorts} onCheckedChange={(v) => tunnelsSettings.set({ watchPorts: v })} />
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
