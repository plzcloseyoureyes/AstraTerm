/*
 * Session editor for protocol 'web' (saved web page session, PROTO-28): a saved URL opened in a Termstead tab,
 * reached directly or through an SSH gateway (sshTunnelVia) / proxy (Network tab). Optional HTTP Basic credentials use
 * the connection's user name and password.
 */
import { Globe } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { defineProtocol, type ValidationErrors } from '@/features/sessions/editors/define'
import { EditorNote, OptionSection, SwitchOption, TextOption, optString } from '@/features/sessions/editors/fields'
import { parseAddress } from './model'

function WebEditor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  const url = optString(value, 'url')
  const https = /^https:/i.test(url.trim())
  return (
    <div className="grid gap-5">
      <OptionSection title="Web page" columns={1}>
        <TextOption {...p} name="url" label="Address" placeholder="https://192.168.1.1/ or http://grafana:3000/" mono required trim />
        {https && (
          <SwitchOption
            {...p}
            name="insecureTls"
            label="Accept an untrusted certificate"
            hint="For routers, BMCs (iDRAC, iLO) and other appliances with self-signed certificates."
          />
        )}
        <SwitchOption
          {...p}
          name="basicAuth"
          defaultValue
          label="Send the user name and password (HTTP Basic)"
          hint="Only when a user name is set in Basic settings. Sent with every request to this site."
        />
      </OptionSection>
      <EditorNote>
        The page opens in a Termstead tab. To reach a site only an SSH server can see, choose that server as the SSH gateway
        (Network → SSH gateway); "localhost" then means the SSH server.
      </EditorNote>
    </div>
  )
}

function validateWeb(c: Connection): ValidationErrors {
  const url = optString(c, 'url').trim()
  if (!url) return { 'options.url': 'Enter the address of the page' }
  if (!parseAddress(url)) return { 'options.url': 'An http or https address, e.g. https://192.168.1.1/' }
  return {}
}

defineProtocol({
  protocol: 'web',
  label: 'Browser',
  icon: Globe,
  defaultPort: 443,
  group: 'other',
  order: 400,
  description: 'Web page in a tab, optionally through SSH',
  component: WebEditor,
  tabLabel: 'Browser settings',
  profile: {
    host: 'hidden',
    port: false,
    username: true,
    usernameLabel: 'HTTP user name',
    usernamePlaceholder: 'Only for HTTP Basic authentication',
    auth: 'password',
    passwordLabel: 'HTTP password',
    identity: false,
    kind: 'files',
    network: true,
    jump: 'gateway',
  },
  validate: validateWeb,
})
