/*
 * SSH options shared by the SSH, Mosh (SSH bootstrap) and SFTP editors (SPEC §5.3 "ssh" row).
 */
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { isPlainObject } from '@/lib/utils'
import { useRunMode } from '@/stores/auth'
import { useSettingsStore } from '@/stores/settings'
import { KeyValueOption, OptionSection, SelectOption, SwitchOption, TagsOption, TextOption, optString, validateEnv } from './fields'
import type { ValidationErrors } from './define'

/** Algorithm names understood by golang.org/x/crypto/ssh (suggestions only — any name is accepted). */
export const SSH_ALGORITHMS = {
  ciphers: [
    'aes128-gcm@openssh.com',
    'aes256-gcm@openssh.com',
    'chacha20-poly1305@openssh.com',
    'aes128-ctr',
    'aes192-ctr',
    'aes256-ctr',
    'aes128-cbc',
    '3des-cbc',
    'arcfour256',
    'arcfour128',
    'arcfour',
  ],
  kex: [
    'mlkem768x25519-sha256',
    'curve25519-sha256',
    'curve25519-sha256@libssh.org',
    'ecdh-sha2-nistp256',
    'ecdh-sha2-nistp384',
    'ecdh-sha2-nistp521',
    'diffie-hellman-group-exchange-sha256',
    'diffie-hellman-group16-sha512',
    'diffie-hellman-group14-sha256',
    'diffie-hellman-group14-sha1',
    'diffie-hellman-group-exchange-sha1',
    'diffie-hellman-group1-sha1',
  ],
  macs: [
    'hmac-sha2-256-etm@openssh.com',
    'hmac-sha2-512-etm@openssh.com',
    'hmac-sha2-256',
    'hmac-sha2-512',
    'hmac-sha1',
    'hmac-sha1-96',
  ],
  hostKeyAlgorithms: [
    'ssh-ed25519',
    'ecdsa-sha2-nistp256',
    'ecdsa-sha2-nistp384',
    'ecdsa-sha2-nistp521',
    'rsa-sha2-512',
    'rsa-sha2-256',
    'ssh-rsa',
    'ssh-dss',
    'sk-ssh-ed25519@openssh.com',
    'sk-ecdsa-sha2-nistp256@openssh.com',
    'ssh-ed25519-cert-v01@openssh.com',
    'ecdsa-sha2-nistp256-cert-v01@openssh.com',
    'rsa-sha2-512-cert-v01@openssh.com',
    'rsa-sha2-256-cert-v01@openssh.com',
    'ssh-rsa-cert-v01@openssh.com',
  ],
} as const

export type SshVariant = 'ssh' | 'mosh' | 'sftp'

/** Session, forwarding, environment and algorithm settings of an SSH connection. */
export function SshCommonOptions({ value, onChange, variant }: ProtocolEditorProps & { variant: SshVariant }) {
  const p = { value, onChange }
  // The backend uses the host's SSH agent by default in desktop mode (SPEC §9 B1).
  const agentDefault = useRunMode() === 'desktop'
  const hasCommand = optString(value, 'remoteCommand').trim() !== ''
  // "Follow SSH path" left unset follows the files setting `followTerminal` (default on), as the backend does.
  const followTerminalDefault = useSettingsStore((s) => {
    const files = s.values.files
    return !(isPlainObject(files) && files.followTerminal === false)
  })
  return (
    <div className="grid gap-5">
      {variant === 'ssh' && (
        <OptionSection title="Session">
          <TextOption
            {...p}
            name="remoteCommand"
            label="Remote command"
            placeholder="Login shell"
            mono
            hint="Run this instead of the login shell (like ssh host command)."
            className="@lg:col-span-2"
          />
          {hasCommand && (
            <SwitchOption
              {...p}
              name="noPty"
              label="Run without a terminal (no PTY)"
              hint="Like ssh -T: stdout and stderr stay separate (stderr in red). Interactive programs need a PTY."
              className="@lg:col-span-2"
            />
          )}
          <SelectOption
            {...p}
            name="sshBrowser"
            label="SSH-browser type"
            defaultLabel="SFTP protocol"
            options={[
              { value: 'scp', label: 'SCP (shell commands)' },
              { value: 'none', label: 'None (no side browser)' },
            ]}
            hint="How the SFTP side panel reaches the files. SCP suits servers without an SFTP subsystem."
          />
          <TextOption {...p} name="sftpRoot" label="SFTP browser folder" placeholder="Home directory" mono hint="Start folder of the side SFTP browser." />
          <div className="grid content-start gap-2 pt-1">
            <SwitchOption
              {...p}
              name="followCwd"
              label="Follow SSH path"
              hint="The SFTP browser follows the shell's current directory. Unset, “Follow terminal folder by default” (Settings → Files & SFTP) decides."
              defaultValue={followTerminalDefault}
            />
            <SwitchOption {...p} name="monitoring" label="Remote monitoring" hint="CPU, memory and disk stats in the status bar." defaultValue />
          </div>
        </OptionSection>
      )}
      {variant === 'mosh' && (
        <OptionSection title="Session">
          <TextOption
            {...p}
            name="remoteCommand"
            label="Remote command"
            placeholder="Login shell"
            mono
            hint="Command mosh-server runs instead of the login shell."
            className="@lg:col-span-2"
          />
        </OptionSection>
      )}
      {variant === 'sftp' && (
        <OptionSection title="Browser">
          <TextOption {...p} name="initialPath" label="Initial folder" placeholder="Home directory" mono hint="Folder shown when the browser opens." />
        </OptionSection>
      )}

      <OptionSection title={variant === 'ssh' ? 'Forwarding & agent' : 'Agent'}>
        <SwitchOption
          {...p}
          name="useAgent"
          label="Use local SSH agent"
          hint="Try keys from ssh-agent / Pageant on the Termstead host (desktop mode, or administrators in server mode)."
          defaultValue={agentDefault}
        />
        {variant !== 'sftp' && (
          <SwitchOption {...p} name="agentForwarding" label="Agent forwarding" hint="Let the remote host use your agent keys (ssh -A). Only for trusted hosts." />
        )}
        {variant === 'ssh' && <SwitchOption {...p} name="x11Forwarding" label="X11 forwarding" hint="Display remote X11 apps on the Termstead host's X server." />}
        {variant !== 'mosh' && <SwitchOption {...p} name="compression" label="Compression" hint="zlib compression; helps on slow links, costs CPU." />}
      </OptionSection>

      {variant !== 'sftp' && (
        <OptionSection title="Environment" columns={1}>
          <KeyValueOption {...p} name="env" label="Environment variables" hint="Sent with the session request (the server must accept them, see AcceptEnv)." />
        </OptionSection>
      )}

      <OptionSection
        title="Algorithms"
        description="Leave empty for secure defaults. Names are tried in order; +name, -name and ^name adjust the defaults (as in ssh_config)."
      >
        <SwitchOption
          {...p}
          name="legacyAlgorithms"
          label="Allow legacy algorithms"
          hint="Old devices: diffie-hellman-group1, ssh-rsa/ssh-dss, CBC ciphers. Weakens security."
          className="@lg:col-span-2"
        />
        <TagsOption {...p} name="ciphers" label="Ciphers" placeholder="Default" suggestions={[...SSH_ALGORITHMS.ciphers]} />
        <TagsOption {...p} name="kex" label="Key exchange" placeholder="Default" suggestions={[...SSH_ALGORITHMS.kex]} />
        <TagsOption {...p} name="macs" label="MACs" placeholder="Default" suggestions={[...SSH_ALGORITHMS.macs]} />
        <TagsOption {...p} name="hostKeyAlgorithms" label="Host key algorithms" placeholder="Default" suggestions={[...SSH_ALGORITHMS.hostKeyAlgorithms]} />
        {variant === 'ssh' && (
          <TextOption {...p} name="clientVersion" label="Client version string" placeholder="Default" mono trim hint="SSH-2.0-… identification sent to the server." />
        )}
      </OptionSection>
    </div>
  )
}

/** Validation shared by the SSH family. */
export function validateSshCommon(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  const envErr = validateEnv(c)
  if (envErr) errors['options.env'] = envErr
  const rc = c.options?.remoteCommand
  if (typeof rc === 'string' && rc.length > 8192) errors['options.remoteCommand'] = 'Remote command is too long'
  return errors
}
