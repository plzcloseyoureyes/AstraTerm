/*
 * Settings → SSH keys & agent: generator defaults, PuTTY export version, passphrase remembering, and the built-in
 * agent / agent forwarding options (read by the backend).
 */
import { KeyRound, ShieldCheck, TerminalSquare } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { useRunMode } from '@/stores/auth'
import { openKeysTab } from './actions'
import { keysSettings } from './settings'
import type { GeneratedKeyType } from './types'

export default function KeysSettingsSection() {
  const s = keysSettings.use()
  const desktop = useRunMode() === 'desktop'
  return (
    <SettingsPage
      title="SSH keys & agent"
      description="Key generation defaults and NexTerm's built-in SSH agent."
      actions={
        <div className="flex gap-1.5">
          <Button size="sm" variant="secondary" onClick={() => openKeysTab('keys')}>
            <KeyRound /> Manage keys
          </Button>
          <Button size="sm" variant="secondary" onClick={() => openKeysTab('knownHosts')}>
            <ShieldCheck /> Known hosts
          </Button>
        </div>
      }
    >
      <SettingsGroup title="New keys">
        <SettingRow label="Default key type" description="Ed25519 is recommended; RSA for old servers.">
          <SimpleSelect<GeneratedKeyType>
            aria-label="Default key type"
            size="sm"
            className="w-40"
            value={s.defaultType}
            onValueChange={(defaultType) => keysSettings.set({ defaultType })}
            options={[
              { value: 'ed25519', label: 'Ed25519' },
              { value: 'rsa', label: 'RSA' },
              { value: 'ecdsa', label: 'ECDSA' },
            ]}
          />
        </SettingRow>
        <SettingRow label="RSA key size">
          <SimpleSelect
            aria-label="RSA key size"
            size="sm"
            className="w-40"
            value={String(s.defaultRsaBits)}
            onValueChange={(v) => keysSettings.set({ defaultRsaBits: Number(v) as 2048 | 3072 | 4096 })}
            options={['2048', '3072', '4096'].map((v) => ({ value: v, label: `${v} bits` }))}
          />
        </SettingRow>
        <SettingRow label="ECDSA curve">
          <SimpleSelect
            aria-label="ECDSA curve"
            size="sm"
            className="w-40"
            value={String(s.defaultEcdsaBits)}
            onValueChange={(v) => keysSettings.set({ defaultEcdsaBits: Number(v) as 256 | 384 | 521 })}
            options={['256', '384', '521'].map((v) => ({ value: v, label: `P-${v}` }))}
          />
        </SettingRow>
        <SettingRow label="Remember passphrases" description="Default of “Remember the passphrase in the vault” when generating or importing keys." htmlFor="keys-remember">
          <Switch id="keys-remember" checked={s.rememberPassphrase} onCheckedChange={(rememberPassphrase) => keysSettings.set({ rememberPassphrase })} />
        </SettingRow>
        <SettingRow label="PuTTY key format" description="Version 2 is needed for PuTTY / WinSCP releases older than 2021.">
          <SimpleSelect
            aria-label="PuTTY key format"
            size="sm"
            className="w-40"
            value={String(s.ppkVersion)}
            onValueChange={(v) => keysSettings.set({ ppkVersion: Number(v) as 2 | 3 })}
            options={[
              { value: '3', label: 'PPK version 3' },
              { value: '2', label: 'PPK version 2' },
            ]}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup
        title="Built-in SSH agent"
        description={desktop ? 'Exposes your stored keys to local programs through a socket; also used for agent forwarding.' : 'In server mode only agent forwarding uses these options.'}
      >
        {desktop && (
          <SettingRow label="Start automatically" description="Start the agent when NexTerm starts." htmlFor="keys-agent-autostart">
            <Switch id="keys-agent-autostart" checked={s.agentAutostart} onCheckedChange={(agentAutostart) => keysSettings.set({ agentAutostart })} />
          </SettingRow>
        )}
        {desktop && (
          <SettingRow label="Confirm each use by local programs" description="A prompt names the program and the host it logs in to." htmlFor="keys-agent-confirm">
            <Switch id="keys-agent-confirm" checked={s.agentConfirm} onCheckedChange={(agentConfirm) => keysSettings.set({ agentConfirm })} />
          </SettingRow>
        )}
        <SettingRow label="Confirm each use by forwarded agents" description="Ask before a remote server uses your keys through agent forwarding." htmlFor="keys-agent-fwd">
          <Switch id="keys-agent-fwd" checked={s.agentForwardConfirm} onCheckedChange={(agentForwardConfirm) => keysSettings.set({ agentForwardConfirm })} />
        </SettingRow>
        {desktop && (
          <SettingRow label="Lock after inactivity" description="Minutes without signatures before the agent locks (0 = never).">
            <NumberInput aria-label="Auto-lock minutes" inputSize="sm" className="w-28" value={s.agentAutoLockMin} min={0} max={10080} unit="min" onChange={(n) => keysSettings.set({ agentAutoLockMin: n ?? 0 })} />
          </SettingRow>
        )}
        <SettingRow label="Forget unlocked keys after" description="Keys whose passphrase is not remembered ask for it again (0 = never).">
          <NumberInput aria-label="Key lifetime minutes" inputSize="sm" className="w-28" value={s.agentKeyLifetimeMin} min={0} max={10080} unit="min" onChange={(n) => keysSettings.set({ agentKeyLifetimeMin: n ?? 0 })} />
        </SettingRow>
        <SettingRow label="Keys offered by the agent" description={`${s.agentExclude.length ? `${s.agentExclude.length} stored key(s) excluded.` : 'All stored keys are offered.'} Choose them in the Agent tab.`}>
          <Button size="sm" variant="secondary" onClick={() => openKeysTab('agent')}>
            <TerminalSquare /> Open agent
          </Button>
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
