/*
 * FTP / FTPS file-browser editor (PROTO-23; SPEC §5.3 "ftp").
 */
import { Network } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { defineProtocol } from './define'
import { OptionSection, SelectOption, SwitchOption, TextOption, optString } from './fields'

const FTP_PORT = 21
const FTPS_IMPLICIT_PORT = 990

export function FtpEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  const tls = optString(value, 'ftpTls') || 'none'
  // Implicit FTPS listens on 990 by convention: follow it unless a custom port was chosen.
  const onTlsChange = (next: Connection) => {
    const nextTls = optString(next, 'ftpTls') || 'none'
    const port = next.port || FTP_PORT
    if (nextTls === 'implicit' && port === FTP_PORT) onChange({ ...next, port: FTPS_IMPLICIT_PORT })
    else if (nextTls !== 'implicit' && port === FTPS_IMPLICIT_PORT) onChange({ ...next, port: FTP_PORT })
    else onChange(next)
  }
  return (
    <div className="grid gap-5">
      <OptionSection title="Security">
        <SelectOption
          value={value}
          onChange={onTlsChange}
          name="ftpTls"
          label="Encryption"
          defaultLabel="None — plain FTP (default)"
          options={[
            { value: 'none', label: 'None — plain FTP' },
            { value: 'explicit', label: 'Explicit TLS (AUTH TLS)' },
            { value: 'implicit', label: 'Implicit TLS (FTPS, port 990)' },
          ]}
        />
        <div className="grid content-start gap-2 pt-1">
          <SwitchOption {...p} name="insecureTls" label="Skip certificate verification" disabled={tls === 'none'} hint="Accept self-signed certificates." />
          <SwitchOption {...p} name="passive" label="Passive mode" defaultValue hint="Recommended behind NAT and firewalls." />
        </div>
      </OptionSection>
      <OptionSection title="Browser">
        <TextOption {...p} name="initialPath" label="Initial folder" placeholder="Login folder" mono />
      </OptionSection>
    </div>
  )
}

defineProtocol({
  protocol: 'ftp',
  label: 'FTP',
  icon: Network,
  defaultPort: FTP_PORT,
  group: 'files',
  order: 310,
  description: 'FTP / FTPS file browser',
  component: FtpEditor,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'anonymous',
    auth: 'password',
    identity: true,
    kind: 'files',
    network: true,
    jump: 'gateway',
  },
})
