/*
 * Mosh session editor (PROTO-13): bootstraps mosh-server over SSH (all SSH options apply), then talks to it over UDP
 * with AstraTerm's built-in client (no local installation needed) or an installed mosh-client.
 */
import { Radio } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { defineProtocol, type ValidationErrors } from './define'
import { EditorNote, OptionSection, SelectOption, TextOption, optString } from './fields'
import { SshCommonOptions, validateSshCommon } from './ssh-common'

function MoshEditor(props: ProtocolEditorProps) {
  const p = { value: props.value, onChange: props.onChange }
  const system = optString(props.value, 'moshClient') === 'system'
  return (
    <div className="grid gap-5">
      <EditorNote>
        SSH only starts mosh-server; the session then runs over UDP, so the server&apos;s UDP ports (60000–61000 unless set below)
        must be reachable from the AstraTerm host directly — jump hosts and proxies carry only the SSH part. mosh-server must be
        installed on the remote host.
      </EditorNote>
      <OptionSection title="Mosh">
        <TextOption {...p} name="moshPorts" label="UDP port range" placeholder="60000:61000" mono trim hint="PORT or FIRST:LAST for mosh-server." />
        <TextOption
          {...p}
          name="moshServer"
          label="mosh-server command"
          placeholder="mosh-server"
          mono
          trim
          hint="Path on the remote host when it is not in PATH (e.g. /opt/homebrew/bin/mosh-server)."
        />
        <SelectOption
          {...p}
          name="moshClient"
          label="Client"
          defaultLabel="Built-in (default)"
          options={[
            { value: 'builtin', label: 'Built-in' },
            { value: 'system', label: 'Installed mosh-client (AstraTerm host)' },
          ]}
        />
        <SelectOption
          {...p}
          name="predict"
          label="Local echo prediction"
          defaultLabel="Adaptive (default)"
          hint={system ? undefined : 'Only the installed mosh-client predicts; the built-in client shows the server echo.'}
          options={[
            { value: 'adaptive', label: 'Adaptive' },
            { value: 'always', label: 'Always' },
            { value: 'never', label: 'Never' },
          ]}
        />
      </OptionSection>
      <SshCommonOptions {...props} variant="mosh" />
    </div>
  )
}

function validateMosh(c: Connection): ValidationErrors {
  const errors = validateSshCommon(c)
  const ports = c.options?.moshPorts
  if (ports !== undefined && ports !== '') {
    const m = String(ports).trim().match(/^(\d{1,5})(?::(\d{1,5}))?$/)
    const a = m ? Number(m[1]) : NaN
    const b = m?.[2] ? Number(m[2]) : a
    if (!m || a < 1 || a > 65535 || b < 1 || b > 65535 || b < a) errors['options.moshPorts'] = 'Use PORT or FIRST:LAST (1–65535, FIRST ≤ LAST)'
  }
  const server = optString(c, 'moshServer').trim()
  if (server && !/^[A-Za-z0-9_./+~-]{1,256}$/.test(server)) errors['options.moshServer'] = 'A path without spaces or shell characters'
  return errors
}

defineProtocol({
  protocol: 'mosh',
  label: 'Mosh',
  icon: Radio,
  defaultPort: 22,
  group: 'terminal',
  order: 70,
  description: 'Mobile shell (roaming, survives sleep)',
  component: MoshEditor,
  tabLabel: 'Mosh settings',
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'Asked when connecting',
    auth: 'ssh',
    identity: true,
    kind: 'terminal',
    network: true,
    jump: 'chain',
  },
  validate: validateMosh,
})
