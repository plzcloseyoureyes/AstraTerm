import { Command, Info, Keyboard } from 'lucide-react'
import { runCommand } from '@/app/commands'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { useAuthState, useCurrentUser } from '@/stores/auth'
import { SettingRow, SettingsGroup, SettingsPage } from '../ui'

const INTEGRATION_LABELS: Record<string, string> = {
  guacd: 'Guacamole daemon (RDP via guacd)',
  docker: 'Docker Engine',
  kubectl: 'kubectl (Kubernetes exec)',
  mosh: 'mosh-client',
  wsl: 'Windows Subsystem for Linux',
}

export default function AboutSection() {
  const st = useAuthState()
  const user = useCurrentUser()
  const features = Object.entries(st?.features ?? {})
  return (
    <SettingsPage title="About Termstead" description="An organized remote-management workspace in a single, self-contained binary.">
      <SettingsGroup title="Build">
        <SettingRow label="Version">
          <span className="font-mono text-sm">{st?.version || 'dev'}</span>
        </SettingRow>
        <SettingRow label="Run mode" description={st?.mode === 'server' ? 'Multi-user server; every user signs in.' : 'Single-user desktop app bound to this computer.'}>
          <Badge variant="outline">{st?.mode ?? 'desktop'}</Badge>
        </SettingRow>
        {user && (
          <SettingRow label="Signed in as">
            <span className="text-sm">
              {user.displayName || user.username} <span className="text-muted-foreground">({user.role})</span>
            </span>
          </SettingRow>
        )}
      </SettingsGroup>

      <SettingsGroup title="Optional integrations" description="Detected on the machine running Termstead; missing ones disable the related features.">
        {features.length === 0 && <p className="px-4 py-3 text-sm text-muted-foreground">No integration information available.</p>}
        {features.map(([k, v]) => (
          <SettingRow key={k} label={INTEGRATION_LABELS[k] ?? k}>
            <Badge variant={v ? 'success' : 'outline'}>{v ? 'available' : 'not found'}</Badge>
          </SettingRow>
        ))}
      </SettingsGroup>

      <SettingsGroup title="Help">
        <div className="flex flex-wrap gap-2 p-4">
          <Button variant="secondary" size="sm" onClick={() => void runCommand('palette.open')}>
            <Command /> Command palette
          </Button>
          <Button variant="secondary" size="sm" onClick={() => void runCommand('settings.open', { section: 'keyboard' })}>
            <Keyboard /> Keyboard shortcuts
          </Button>
          <Button variant="secondary" size="sm" onClick={() => void runCommand('app.about')}>
            <Info /> About dialog
          </Button>
        </div>
      </SettingsGroup>
    </SettingsPage>
  )
}
