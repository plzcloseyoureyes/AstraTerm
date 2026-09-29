/*
 * RDP editor (PROTO-16, GFX-*; SPEC §5.3 "rdp"). The default engine is IronRDP (WASM) through the Go RDCleanPath relay;
 * guacd is optional. Credentials are in Basic settings, the SSH gateway in the Network tab.
 */
import { useMemo } from 'react'
import { Monitor } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { useServerFeatures } from '@/stores/auth'
import { isValidHost } from '../quickparse'
import { defineProtocol, type ValidationErrors } from './define'
import {
  ComboOption,
  EditorNote,
  NumberOption,
  OptionSection,
  SecretOption,
  SelectOption,
  SwitchOption,
  TextOption,
  optBool,
  optNumber,
  optString,
  type ComboSuggestion,
} from './fields'

const KEYBOARD_LAYOUTS: ComboSuggestion[] = [
  ['en-us-qwerty', 'English (US)'],
  ['en-gb-qwerty', 'English (UK)'],
  ['de-de-qwertz', 'German'],
  ['de-ch-qwertz', 'Swiss German'],
  ['fr-fr-azerty', 'French'],
  ['fr-be-azerty', 'Belgian French'],
  ['fr-ch-qwertz', 'Swiss French'],
  ['it-it-qwerty', 'Italian'],
  ['es-es-qwerty', 'Spanish'],
  ['es-latam-qwerty', 'Latin American'],
  ['pt-pt-qwerty', 'Portuguese'],
  ['pt-br-qwerty', 'Portuguese (Brazil)'],
  ['sv-se-qwerty', 'Swedish'],
  ['da-dk-qwerty', 'Danish'],
  ['no-no-qwerty', 'Norwegian'],
  ['fi-fi-qwerty', 'Finnish'],
  ['hu-hu-qwertz', 'Hungarian'],
  ['tr-tr-qwerty', 'Turkish'],
  ['ja-jp-qwerty', 'Japanese'],
  ['failsafe', 'Failsafe (Unicode events)'],
].map(([value, description]) => ({ value, description }))

function timezoneSuggestions(): ComboSuggestion[] {
  try {
    return Intl.supportedValuesOf('timeZone').map((z) => ({ value: z }))
  } catch {
    return ['UTC', 'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles', 'Asia/Tokyo'].map((z) => ({ value: z }))
  }
}

export function RdpEditor({ value, onChange }: ProtocolEditorProps) {
  const features = useServerFeatures()
  const p = { value, onChange }
  const zones = useMemo(timezoneSuggestions, [])
  const engine = optString(value, 'rdpEngine') || 'ironrdp'
  const guacdMissing = features?.guacd === false
  return (
    <div className="grid gap-5">
      <OptionSection title="Connection">
        <SelectOption
          {...p}
          name="rdpEngine"
          label="Engine"
          defaultLabel="IronRDP (default, built in)"
          options={[
            { value: 'ironrdp', label: 'IronRDP (built in)' },
            { value: 'guacd', label: guacdMissing ? 'guacd (not reachable)' : 'guacd (Apache Guacamole)' },
          ]}
          hint={engine === 'guacd' ? 'Full fidelity (audio, drives, printing, recording) through guacd.' : 'Runs in the browser; no extra services needed.'}
        />
        <TextOption {...p} name="domain" label="Domain" placeholder="None" trim />
        <SelectOption
          {...p}
          name="security"
          label="Security"
          defaultLabel="Negotiate (default)"
          options={[
            { value: 'any', label: 'Negotiate' },
            { value: 'nla', label: 'NLA (CredSSP)' },
            { value: 'tls', label: 'TLS' },
            { value: 'rdp', label: 'Standard RDP encryption' },
            { value: 'vmconnect', label: 'Hyper-V console (vmconnect)' },
          ]}
        />
        <div className="grid content-start gap-2 pt-1">
          <SwitchOption {...p} name="ignoreCert" label="Ignore certificate errors" hint="Accept self-signed server certificates." />
          <SwitchOption {...p} name="console" label="Console (admin) session" hint="Attach to the physical console session." />
        </div>
      </OptionSection>
      {engine === 'guacd' && guacdMissing && (
        <EditorNote tone="warning">guacd is not reachable from the Termstead server; this session will fail until it is available.</EditorNote>
      )}

      <OptionSection title="Display" description="Leave width and height empty to fit the tab.">
        <NumberOption {...p} name="width" label="Width" min={200} max={8192} unit="px" placeholder="Fit" />
        <NumberOption {...p} name="height" label="Height" min={200} max={8192} unit="px" placeholder="Fit" />
        <NumberOption {...p} name="dpi" label="DPI" min={72} max={480} placeholder="Automatic" />
        <SelectOption
          {...p}
          name="colorDepth"
          label="Colour depth"
          numeric
          defaultLabel="32-bit (default)"
          options={[
            { value: '32', label: '32-bit' },
            { value: '24', label: '24-bit' },
            { value: '16', label: '16-bit' },
            { value: '8', label: '8-bit' },
          ]}
        />
        <SelectOption
          {...p}
          name="resizeMethod"
          label="When the tab is resized"
          defaultLabel="Update display (default)"
          options={[
            { value: 'display-update', label: 'Update display' },
            { value: 'reconnect', label: 'Reconnect with the new size' },
          ]}
        />
      </OptionSection>

      <OptionSection title="Devices & redirection">
        <SwitchOption {...p} name="enableAudio" label="Play remote audio" />
        <SwitchOption {...p} name="enableMic" label="Microphone" />
        <SwitchOption {...p} name="enablePrinting" label="Printing" hint="Print to a PDF delivered to the browser (guacd)." />
        <SwitchOption {...p} name="disableClipboard" label="Disable clipboard" />
        <SwitchOption {...p} name="enableDrive" label="Shared drive" hint="Expose a transfer drive to the remote session (guacd)." />
        {optBool(value, 'enableDrive') && <TextOption {...p} name="driveName" label="Drive name" placeholder="Termstead" trim />}
      </OptionSection>

      <OptionSection title="Session">
        <TextOption {...p} name="initialProgram" label="Start program" placeholder="Desktop" mono hint="Run a program instead of the desktop (RemoteApp-like)." />
        <ComboOption {...p} name="serverLayout" label="Keyboard layout" mono placeholder="Automatic" suggestions={KEYBOARD_LAYOUTS} />
        <ComboOption {...p} name="timezone" label="Time zone" placeholder="Server default" suggestions={zones} />
      </OptionSection>

      <OptionSection title="RD Gateway">
        <TextOption {...p} name="gatewayHost" label="Gateway host" placeholder="None" mono trim />
        <NumberOption {...p} name="gatewayPort" label="Gateway port" min={1} max={65535} placeholder="443" />
        <TextOption {...p} name="gatewayUsername" label="Gateway user" placeholder="Same as the session" trim />
        <TextOption {...p} name="gatewayDomain" label="Gateway domain" placeholder="None" trim />
        <SecretOption {...p} secret="gatewayPassword" label="Gateway password" placeholder="Same as the session" />
      </OptionSection>

      <OptionSection title="Performance & recording">
        <SwitchOption {...p} name="enableWallpaper" label="Desktop wallpaper" />
        <SwitchOption {...p} name="enableTheming" label="Visual themes" />
        <SwitchOption {...p} name="enableFontSmoothing" label="Font smoothing (ClearType)" />
        <SwitchOption {...p} name="recording" label="Record session" hint="Graphical recording (guacd engine)." />
      </OptionSection>
    </div>
  )
}

function validateRdp(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  for (const key of ['width', 'height'] as const) {
    const n = optNumber(c, key)
    if (n !== null && (n < 200 || n > 8192)) errors[`options.${key}`] = 'Between 200 and 8192 pixels (empty = fit)'
  }
  const dpi = optNumber(c, 'dpi')
  if (dpi !== null && (dpi < 72 || dpi > 480)) errors['options.dpi'] = 'DPI must be between 72 and 480'
  const gw = optString(c, 'gatewayHost').trim()
  if (gw && !isValidHost(gw)) errors['options.gatewayHost'] = 'Enter a host name or IP address'
  const gp = optNumber(c, 'gatewayPort')
  if (gp !== null && (gp < 1 || gp > 65535)) errors['options.gatewayPort'] = 'Port must be between 1 and 65535'
  return errors
}

defineProtocol({
  protocol: 'rdp',
  label: 'RDP',
  icon: Monitor,
  defaultPort: 3389,
  group: 'graphical',
  order: 200,
  description: 'Windows Remote Desktop',
  component: RdpEditor,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'Blank shows the Windows logon screen',
    auth: 'password',
    identity: true,
    kind: 'graphical',
    network: true,
    jump: 'gateway',
  },
  validate: validateRdp,
})
