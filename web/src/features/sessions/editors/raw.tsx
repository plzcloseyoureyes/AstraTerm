/*
 * Raw socket editor (PROTO-9): a byte stream to host:port over TCP, TLS or UDP, with server-side local echo and
 * line-ending translation (CR LF by default, as text protocols expect).
 */
import { Plug } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { defineProtocol } from './define'
import { LINE_ENDING_CHOICES, OptionSection, SelectOption, SwitchOption, optString } from './fields'

export function RawEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  const transport = optString(value, 'transport') || 'tcp'
  return (
    <div className="grid gap-5">
      <OptionSection title="Socket">
        <SelectOption
          {...p}
          name="transport"
          label="Transport"
          defaultLabel="TCP (default)"
          options={[
            { value: 'tcp', label: 'TCP' },
            { value: 'tls', label: 'TLS (encrypted TCP)' },
            { value: 'udp', label: 'UDP datagrams' },
          ]}
        />
        <SelectOption {...p} name="lineEnding" label="Enter sends" defaultLabel="CR LF (default)" options={LINE_ENDING_CHOICES} />
      </OptionSection>
      <OptionSection title="Options" columns={1}>
        <SwitchOption {...p} name="localEcho" label="Local echo" hint="Echo typed characters locally (most raw services do not echo)." />
        <SwitchOption {...p} name="hexView" label="Open the hex monitor" hint="Show the traffic as a hex dump (with a hex send box) when the session opens." />
        {transport === 'tls' && (
          <SwitchOption
            {...p}
            name="insecureTls"
            label="Skip certificate verification"
            hint="Accept self-signed or mismatched certificates (not recommended)."
          />
        )}
        {transport === 'udp' && (
          <p className="text-sm text-muted-foreground">UDP is a direct datagram socket and cannot be routed through a proxy or SSH gateway.</p>
        )}
      </OptionSection>
    </div>
  )
}

defineProtocol({
  protocol: 'raw',
  label: 'Raw socket',
  icon: Plug,
  defaultPort: 0,
  group: 'terminal',
  order: 40,
  description: 'Plain TCP / TLS / UDP socket',
  component: RawEditor,
  tabLabel: 'Socket settings',
  profile: {
    host: 'required',
    port: true,
    portRequired: true,
    username: false,
    auth: 'none',
    kind: 'terminal',
    network: true,
    jump: 'gateway',
  },
})
