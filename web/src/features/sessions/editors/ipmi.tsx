/*
 * IPMI Serial-over-LAN console editor (SPEC §10.1 extension protocol `ipmi`, backend internal/proto/ipmi).
 * Option keys (documented in SPEC §9, F1b): `ipmiInterface` ('lanplus'|'lan'), `cipherSuite` (0–17),
 * `privilegeLevel` ('administrator'|'operator'|'user').
 */
import { useState } from 'react'
import { Power, ServerCog } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { IpmiPowerPanel } from '@/features/protocols/IpmiPowerDialog'
import { defineProtocol } from './define'
import { EditorNote, NumberOption, OptionSection, SelectOption, optString } from './fields'

export function IpmiEditor({ value, onChange, mode }: ProtocolEditorProps) {
  const p = { value, onChange }
  const lan = optString(value, 'ipmiInterface') === 'lan'
  const [showPower, setShowPower] = useState(false)
  return (
    <div className="grid gap-5">
      {lan && <EditorNote tone="warning">The Serial-over-LAN console needs IPMI v2.0 (lanplus); IPMI v1.5 only allows power control.</EditorNote>}
      <OptionSection title="BMC">
        <SelectOption
          {...p}
          name="ipmiInterface"
          label="Interface"
          defaultLabel="IPMI v2.0 / RMCP+ (lanplus, default)"
          options={[
            { value: 'lanplus', label: 'IPMI v2.0 / RMCP+ (lanplus)' },
            { value: 'lan', label: 'IPMI v1.5 (lan)' },
          ]}
        />
        <SelectOption
          {...p}
          name="privilegeLevel"
          label="Privilege level"
          defaultLabel="Administrator (default)"
          options={[
            { value: 'administrator', label: 'Administrator' },
            { value: 'operator', label: 'Operator' },
            { value: 'user', label: 'User' },
          ]}
        />
        <NumberOption {...p} name="cipherSuite" label="Cipher suite" min={0} max={17} placeholder="Automatic" hint="RMCP+ cipher suite ID (e.g. 3 or 17)." />
      </OptionSection>
      {mode === 'edit' && value.id && (
        <OptionSection title="Power" columns={1} description="Chassis power status and control through this BMC (uses the saved settings).">
          {showPower ? (
            <IpmiPowerPanel connectionId={value.id} />
          ) : (
            <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={() => setShowPower(true)}>
              <Power /> Show power controls
            </Button>
          )}
        </OptionSection>
      )}
    </div>
  )
}

defineProtocol({
  protocol: 'ipmi',
  label: 'IPMI',
  icon: ServerCog,
  defaultPort: 623,
  group: 'terminal',
  order: 110,
  description: 'Serial-over-LAN console of a server BMC',
  component: IpmiEditor,
  profile: {
    host: 'required',
    hostLabel: 'BMC host',
    port: true,
    username: true,
    auth: 'password',
    identity: true,
    kind: 'terminal',
    // RMCP+ is UDP: TCP proxies and SSH gateways do not apply.
    network: false,
  },
})
