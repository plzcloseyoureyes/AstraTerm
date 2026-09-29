/*
 * VNC editor (PROTO-17; SPEC §5.3 "vnc"). The password is stored as `vncPassword` and never reaches the browser: the
 * Go side terminates RFB security (SPEC §6.3).
 */
import { MonitorSmartphone } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { defineProtocol, type ValidationErrors } from './define'
import { NumberOption, OptionSection, SelectOption, SwitchOption, optNumber } from './fields'

export function VncEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  return (
    <div className="grid gap-5">
      <OptionSection title="Display">
        <SelectOption
          {...p}
          name="scaling"
          label="Scaling"
          defaultLabel="Fit to tab (default)"
          options={[
            { value: 'fit', label: 'Fit to tab' },
            { value: 'remote-resize', label: 'Resize the remote desktop' },
            { value: 'none', label: 'None (scroll)' },
          ]}
        />
        <div className="grid content-start gap-2 pt-1">
          <SwitchOption {...p} name="viewOnly" label="View only" hint="Do not send keyboard or mouse input." />
          <SwitchOption {...p} name="shared" label="Shared session" hint="Keep other viewers connected." defaultValue />
        </div>
      </OptionSection>
      <OptionSection title="Encoding" description="0 = fastest / lowest quality, 9 = best quality / most compression.">
        <NumberOption {...p} name="quality" label="JPEG quality" min={0} max={9} placeholder="6" />
        <NumberOption {...p} name="compression" label="Compression level" min={0} max={9} placeholder="2" />
      </OptionSection>
    </div>
  )
}

function validateVnc(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  for (const key of ['quality', 'compression'] as const) {
    const n = optNumber(c, key)
    if (n !== null && (n < 0 || n > 9 || !Number.isInteger(n))) errors[`options.${key}`] = 'A whole number from 0 to 9'
  }
  return errors
}

defineProtocol({
  protocol: 'vnc',
  label: 'VNC',
  icon: MonitorSmartphone,
  defaultPort: 5900,
  group: 'graphical',
  order: 210,
  description: 'Remote framebuffer desktop',
  component: VncEditor,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'Only if the server asks for one',
    auth: 'password',
    passwordSecret: 'vncPassword',
    passwordLabel: 'VNC password',
    identity: false,
    kind: 'graphical',
    network: true,
    jump: 'gateway',
  },
  validate: validateVnc,
})
