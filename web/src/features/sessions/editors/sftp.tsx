/*
 * Standalone SFTP file-browser session (PROTO-24): SSH transport, opens a files tab.
 */
import { FolderOpen } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { defineProtocol } from './define'
import { SshCommonOptions, validateSshCommon } from './ssh-common'

function SftpEditor(props: ProtocolEditorProps) {
  return <SshCommonOptions {...props} variant="sftp" />
}

defineProtocol({
  protocol: 'sftp',
  label: 'SFTP',
  icon: FolderOpen,
  defaultPort: 22,
  group: 'files',
  order: 300,
  description: 'File browser over SSH',
  component: SftpEditor,
  profile: {
    host: 'required',
    port: true,
    username: true,
    usernamePlaceholder: 'Asked when connecting',
    auth: 'ssh',
    identity: true,
    kind: 'files',
    network: true,
    jump: 'chain',
  },
  validate: validateSshCommon,
})
