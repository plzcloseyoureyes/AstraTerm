/*
 * WinRM remote shell editor (SPEC §10.1 extension protocol `winrm`, backend internal/proto/winrm).
 * Option keys (documented in SPEC §9, F1b): `https` (bool), `insecureTls` (bool), `winrmAuth` ('ntlm'|'basic'),
 * `shell` ('powershell'|'cmd').
 */
import { MonitorCog } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { SwitchField } from '@/components/ui/switch'
import { defineProtocol } from './define'
import { OptionSection, SelectOption, optBool, withOptions } from './fields'

const HTTP_PORT = 5985
const HTTPS_PORT = 5986

function WinrmEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  const https = optBool(value, 'https')
  return (
    <div className="grid gap-5">
      <OptionSection title="Transport">
        <SwitchField
          label="HTTPS"
          description={`Encrypted WinRM listener (port ${HTTPS_PORT}).`}
          checked={https}
          onCheckedChange={(v) => {
            const next = withOptions(value, { https: v })
            // Follow the listener's conventional port unless a custom one was set.
            const atDefault = !value.port || value.port === (v ? HTTP_PORT : HTTPS_PORT)
            onChange(atDefault ? { ...next, port: v ? HTTPS_PORT : HTTP_PORT } : next)
          }}
        />
        <SwitchField
          label="Skip certificate verification"
          description="Accept self-signed listener certificates."
          checked={optBool(value, 'insecureTls')}
          disabled={!https}
          onCheckedChange={(v) => onChange(withOptions(value, { insecureTls: v }))}
        />
      </OptionSection>
      <OptionSection title="Session">
        <SelectOption
          {...p}
          name="winrmAuth"
          label="Authentication"
          defaultLabel="NTLM (default)"
          hint={
            https
              ? undefined
              : 'Over HTTP, NTLM messages are encrypted (Windows default policy). Through a proxy or SSH gateway, or with Basic, the server must allow unencrypted WinRM — prefer HTTPS.'
          }
          options={[
            { value: 'ntlm', label: 'NTLM (domain or local accounts)' },
            { value: 'basic', label: 'Basic (local accounts, HTTPS recommended)' },
          ]}
        />
        <SelectOption
          {...p}
          name="shell"
          label="Shell"
          defaultLabel="PowerShell (default)"
          options={[
            { value: 'powershell', label: 'PowerShell' },
            { value: 'cmd', label: 'Command Prompt (cmd.exe)' },
          ]}
        />
        <SwitchField
          label="Local echo"
          description="WinRM has no remote terminal: lines are edited locally (Backspace, ↑/↓ history, Ctrl+U) and sent on Enter. Turn off if the interpreter echoes commands itself."
          checked={!optBool(value, 'noLocalEcho')}
          onCheckedChange={(v) => onChange(withOptions(value, { noLocalEcho: v ? undefined : true }))}
        />
      </OptionSection>
    </div>
  )
}

defineProtocol({
  protocol: 'winrm',
  label: 'WinRM',
  icon: MonitorCog,
  defaultPort: HTTP_PORT,
  group: 'terminal',
  order: 100,
  description: 'Windows Remote Management shell',
  component: WinrmEditor,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'DOMAIN\\user or user',
    auth: 'password',
    identity: true,
    kind: 'terminal',
    network: true,
    jump: 'gateway',
  },
})
