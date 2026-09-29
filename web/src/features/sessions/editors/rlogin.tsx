/*
 * Rlogin / Rsh session editor (PROTO-7, PROTO-8). The variant selects rlogin (interactive login, port 513) or rsh
 * (runs a single remote command, port 514). Both are plaintext BSD protocols and are flagged insecure.
 */
import { LogIn } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { defineProtocol, type ValidationErrors } from './define'
import { EditorNote, LINE_ENDING_CHOICES, OptionSection, SelectOption, SwitchOption, TextOption, optString } from './fields'

function isRsh(value: Connection): boolean {
  return optString(value, 'variant') === 'rsh'
}

export function RloginEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  const rsh = isRsh(value)
  return (
    <div className="grid gap-5">
      <EditorNote tone="warning">
        {rsh ? 'Rsh' : 'Rlogin'} sends everything, including credentials, in clear text. Use it only on trusted networks or through an
        SSH gateway. Trusted (.rhosts) logins need Termstead to run as root to use a reserved source port; otherwise the server asks
        for the password{rsh ? ' (rsh then refuses the connection)' : ''}.
      </EditorNote>
      <OptionSection title="Mode" columns={1}>
        <SelectOption
          {...p}
          name="variant"
          label="Variant"
          defaultLabel="Rlogin (interactive login, port 513)"
          options={[
            { value: 'rlogin', label: 'Rlogin — interactive login (port 513)' },
            { value: 'rsh', label: 'Rsh — run one remote command (port 514)' },
          ]}
        />
        {rsh && (
          <TextOption
            {...p}
            name="command"
            label="Remote command"
            required
            mono
            placeholder="uptime"
            hint="Command executed on the remote host; its output is shown in the terminal."
          />
        )}
      </OptionSection>
      {rsh ? (
        <OptionSection title="Input" description="Typed lines go to the command's standard input; Ctrl+D ends it.">
          <SelectOption {...p} name="lineEnding" label="Enter sends" defaultLabel="LF (default)" options={LINE_ENDING_CHOICES} />
          <div className="pt-6">
            <SwitchOption {...p} name="localEcho" label="Local echo" defaultValue hint="Rsh has no terminal on the remote side, so input is echoed locally." />
          </div>
        </OptionSection>
      ) : (
        <OptionSection title="Terminal">
          <SelectOption {...p} name="lineEnding" label="Enter sends" defaultLabel="CR (default)" options={LINE_ENDING_CHOICES} />
          <div className="pt-6">
            <SwitchOption {...p} name="localEcho" label="Local echo" hint="Echo typed characters locally (the remote terminal normally echoes)." />
          </div>
        </OptionSection>
      )}
      <OptionSection title="Identity" columns={1}>
        <TextOption
          {...p}
          name="localUser"
          label="Local user name"
          mono
          trim
          placeholder="Same as the remote user"
          hint="Sent in the handshake; .rhosts trust on the server is checked against it (password login otherwise)."
        />
      </OptionSection>
    </div>
  )
}

function validateRlogin(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  if (isRsh(c) && !optString(c, 'command').trim()) {
    errors['options.command'] = 'A remote command is required for rsh'
  }
  return errors
}

defineProtocol({
  protocol: 'rlogin',
  label: 'Rlogin / Rsh',
  icon: LogIn,
  defaultPort: 513,
  group: 'terminal',
  order: 30,
  description: 'BSD remote login / remote shell (insecure)',
  component: RloginEditor,
  tabLabel: 'Rlogin settings',
  validate: validateRlogin,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'Remote user name',
    auth: 'password',
    identity: true,
    kind: 'terminal',
    network: true,
    jump: 'gateway',
  },
})
