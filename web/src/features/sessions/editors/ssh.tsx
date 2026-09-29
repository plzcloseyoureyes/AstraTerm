/*
 * SSH session editor (PROTO-1/2, SPEC §5.3 "ssh"). Credentials live in "Basic settings"; jump hosts, proxy and
 * port knocking in the Network tab.
 */
import { Terminal } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { defineProtocol } from './define'
import { SshCommonOptions, validateSshCommon } from './ssh-common'

export function SshEditor(props: ProtocolEditorProps) {
  return <SshCommonOptions {...props} variant="ssh" />
}

defineProtocol({
  protocol: 'ssh',
  label: 'SSH',
  icon: Terminal,
  defaultPort: 22,
  group: 'terminal',
  order: 10,
  description: 'Secure shell terminal',
  component: SshEditor,
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
  validate: validateSshCommon,
})
