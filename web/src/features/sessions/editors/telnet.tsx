/*
 * Telnet session editor (PROTO-6). Username/password (Basic settings) answer login prompts automatically; terminal
 * type, encoding and backspace are on the Terminal tab. Optional TLS (telnets) encrypts the connection.
 */
import { Cable } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { SwitchField } from '@/components/ui/switch'
import { defineProtocol } from './define'
import { OptionSection, SelectOption, SwitchOption, optBool, withOptions } from './fields'

const PLAIN_PORT = 23
const TLS_PORT = 992

function TelnetEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  const tls = optBool(value, 'tls')
  return (
    <div className="grid gap-5">
      <OptionSection title="Telnet" columns={1}>
        <SwitchOption
          {...p}
          name="negotiate"
          label="Negotiate options"
          hint="Window size (NAWS), terminal type, echo and suppress-go-ahead. Turn off for devices that mishandle option negotiation."
          defaultValue
        />
        <SelectOption
          {...p}
          name="lineEnding"
          label="Enter sends"
          defaultLabel="CR NUL (Telnet default)"
          options={[
            { value: 'crlf', label: 'CR LF (\\r\\n)' },
            { value: 'cr', label: 'CR (\\r)' },
            { value: 'lf', label: 'LF (\\n)' },
          ]}
        />
        <SwitchOption
          {...p}
          name="localEcho"
          label="Local echo"
          hint="Show typed characters while the server does not echo them; it switches off as soon as the server negotiates ECHO (password prompts stay hidden)."
        />
      </OptionSection>
      <OptionSection title="Security" columns={1}>
        <SwitchField
          label="TLS (telnets)"
          description={`Encrypt the connection with TLS (port ${TLS_PORT}).`}
          checked={tls}
          onCheckedChange={(v) => {
            const next = withOptions(value, { tls: v })
            const atDefault = !value.port || value.port === (v ? PLAIN_PORT : TLS_PORT)
            onChange(atDefault ? { ...next, port: v ? TLS_PORT : PLAIN_PORT } : next)
          }}
        />
        {tls && (
          <SwitchOption
            {...p}
            name="insecureTls"
            label="Skip certificate verification"
            hint="Accept self-signed or mismatched certificates (not recommended)."
          />
        )}
      </OptionSection>
    </div>
  )
}

defineProtocol({
  protocol: 'telnet',
  label: 'Telnet',
  icon: Cable,
  defaultPort: 23,
  group: 'terminal',
  order: 20,
  description: 'Unencrypted terminal (network gear, legacy hosts)',
  component: TelnetEditor,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'Answer the login prompt manually',
    auth: 'password',
    passwordLabel: 'Password (auto-login)',
    identity: true,
    kind: 'terminal',
    network: true,
    jump: 'gateway',
  },
})
