/*
 * "Basic settings" of the session editor: name, host, port, user and credentials (password / identity / stored key /
 * agent), shaped by the selected protocol's profile.
 */
import { useId } from 'react'
import { Controller, type UseFormReturn } from 'react-hook-form'
import { KeyRound, UserRoundCog } from 'lucide-react'
import { useCommand } from '@/app/commands'
import { defaultPort } from '@/app/protocols'
import { useIdentities } from '@/api/identities'
import type { AuthMethod, Connection, SSHKey } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { useKeysForPicker } from '../api'
import type { ProtocolProfile } from '../editors/define'
import { EditorNote, SecretInput, withOptions } from '../editors/fields'
import { autoName } from '../model'
import type { SessionFormValues } from './schema'

const NONE = '__none__'

const AUTH_CHOICES: { value: AuthMethod; label: string }[] = [
  { value: 'auto', label: 'Automatic (agent, key, password, 2FA)' },
  { value: 'password', label: 'Password' },
  { value: 'key', label: 'Public key' },
  { value: 'agent', label: 'SSH agent' },
  { value: 'keyboard-interactive', label: 'Keyboard-interactive (2FA / OTP)' },
  { value: 'none', label: 'None' },
]

function keyLabel(k: SSHKey): string {
  const bits = k.bits ? ` ${k.bits}` : ''
  return `${k.name} — ${k.type}${bits}`
}

export function BasicSection({
  form,
  profile,
  value,
  onChange,
  errors,
  onHostBlur,
}: {
  form: UseFormReturn<SessionFormValues>
  profile: ProtocolProfile
  /** Draft as a connection (credentials are edited through it). */
  value: Connection
  onChange: (next: Connection) => void
  errors: Readonly<Record<string, string | undefined>>
  /** "user@host:port" pasted into the host field is split into its parts. */
  onHostBlur: (text: string) => void
}) {
  const { register, control } = form
  const ids = { identity: useId(), auth: useId(), key: useId(), password: useId(), passphrase: useId(), port: useId() }
  const identities = useIdentities(profile.identity === true)
  const needKeys = profile.auth === 'ssh'
  const keys = useKeysForPicker(needKeys)
  const manageIdentities = useCommand('keys.identities')
  const manageKeys = useCommand('keys.open')

  const identity = value.identityId ? identities.data?.find((i) => i.id === value.identityId) : undefined
  const method = value.authMethod
  const showPassword = profile.auth === 'password' || (profile.auth === 'ssh' && (method === 'auto' || method === 'password'))
  const showKey = profile.auth === 'ssh' && (method === 'auto' || method === 'key')
  const passwordSecret = profile.passwordSecret ?? 'password'
  const namePlaceholder = autoName(value) || 'My server'
  const portPlaceholder = defaultPort(value.protocol) ? String(defaultPort(value.protocol)) : 'Port'

  const identityOptions = [
    { value: NONE, label: 'None — use the fields below' },
    ...(identities.data ?? []).map((i) => ({ value: i.id, label: i.username ? `${i.name} — ${i.username}` : i.name })),
  ]
  if (value.identityId && !identities.data?.some((i) => i.id === value.identityId)) {
    identityOptions.push({ value: value.identityId, label: identities.isLoading ? 'Loading…' : 'Unknown identity (deleted?)' })
  }

  const keysAvailable = keys.data?.available !== false
  const keyOptions = [{ value: NONE, label: identity?.keyId ? 'From the identity' : 'None' }, ...(keys.data?.keys ?? []).map((k) => ({ value: k.id, label: keyLabel(k) }))]
  if (value.keyId && !keys.data?.keys.some((k) => k.id === value.keyId)) {
    keyOptions.push({ value: value.keyId, label: keys.isLoading ? 'Loading…' : 'Unknown key (deleted?)' })
  }

  return (
    <section className="grid gap-3 rounded-lg border bg-card/60 p-3.5" aria-label="Basic settings">
      <h3 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">Basic settings</h3>
      <div className="grid gap-x-4 gap-y-3 @lg:grid-cols-2">
        <Field label="Name" error={errors.name} hint="Shown in the session tree and on tabs." className={profile.host === 'hidden' ? '@lg:col-span-2' : undefined}>
          <Input {...register('name')} placeholder={namePlaceholder} autoComplete="off" spellCheck={false} maxLength={200} />
        </Field>

        {profile.host !== 'hidden' && (
          <div className="grid grid-cols-[1fr_6.5rem] gap-2">
            <Field label={profile.hostLabel ?? 'Remote host'} required={profile.host === 'required'} error={errors.host}>
              <Input
                {...register('host', { onBlur: (e) => onHostBlur(e.target.value) })}
                placeholder={profile.hostPlaceholder ?? 'host.example.com or 10.0.0.5'}
                autoComplete="off"
                autoCapitalize="off"
                spellCheck={false}
                className="font-mono"
                data-autofocus-host=""
              />
            </Field>
            {profile.port ? (
              <Field label="Port" htmlFor={ids.port} required={profile.portRequired} error={errors.port}>
                <Controller
                  control={control}
                  name="port"
                  render={({ field }) => (
                    <NumberInput
                      id={ids.port}
                      value={field.value}
                      onChange={field.onChange}
                      onBlur={field.onBlur}
                      min={1}
                      max={65535}
                      allowEmpty
                      placeholder={portPlaceholder}
                      aria-invalid={errors.port ? true : undefined}
                    />
                  )}
                />
              </Field>
            ) : (
              <span />
            )}
          </div>
        )}

        {profile.username && (
          <Field
            label={profile.usernameLabel ?? 'Username'}
            error={errors.username}
            hint={identity ? `Leave empty to use the identity's user${identity.username ? ` (${identity.username})` : ''}.` : undefined}
          >
            <Input
              {...register('username')}
              placeholder={identity?.username ? identity.username : (profile.usernamePlaceholder ?? 'Optional')}
              autoComplete="off"
              autoCapitalize="off"
              spellCheck={false}
            />
          </Field>
        )}

        {profile.identity && (
          <Field
            label="Identity"
            htmlFor={ids.identity}
            hint="Reusable credentials; anything set on this session takes precedence."
            labelAside={
              <Button variant="link" size="xs" disabled={!manageIdentities.enabled} onClick={() => void manageIdentities.run(undefined, 'menu')}>
                <UserRoundCog className="size-3" /> Manage
              </Button>
            }
          >
            <SimpleSelect
              id={ids.identity}
              value={value.identityId || NONE}
              options={identityOptions}
              onValueChange={(v) => onChange({ ...value, identityId: v === NONE ? null : v })}
            />
          </Field>
        )}

        {profile.auth === 'ssh' && (
          <Field label="Authentication" htmlFor={ids.auth}>
            <SimpleSelect
              id={ids.auth}
              value={method}
              options={AUTH_CHOICES}
              onValueChange={(m) => {
                // "SSH agent" also turns on agent use for the automatic method order.
                const next = m === 'agent' ? withOptions(value, { useAgent: true }) : value
                onChange({ ...next, authMethod: m })
              }}
            />
          </Field>
        )}

        {showPassword && (
          <Field label={profile.passwordLabel ?? 'Password'} htmlFor={ids.password} error={errors[`secrets.${passwordSecret}`]}>
            <SecretInput
              id={ids.password}
              value={value}
              onChange={onChange}
              secret={passwordSecret}
              placeholder={identity ? 'From the identity, or asked when connecting' : 'Not stored — asked when connecting'}
            />
          </Field>
        )}

        {showKey && (
          <Field
            label="Private key"
            htmlFor={ids.key}
            error={errors.keyId}
            hint={!keysAvailable ? 'SSH key management is not available on this server yet.' : undefined}
            labelAside={
              <Button variant="link" size="xs" disabled={!manageKeys.enabled} onClick={() => void manageKeys.run(undefined, 'menu')}>
                <KeyRound className="size-3" /> Manage
              </Button>
            }
          >
            <SimpleSelect
              id={ids.key}
              value={value.keyId || NONE}
              disabled={!keysAvailable}
              options={keyOptions}
              onValueChange={(v) => onChange({ ...value, keyId: v === NONE ? null : v })}
            />
          </Field>
        )}

        {showKey && (value.keyId || identity?.keyId || (value.secretKeys ?? []).includes('passphrase')) && (
          <Field label="Key passphrase" htmlFor={ids.passphrase} hint="Only if it is not stored with the key.">
            <SecretInput id={ids.passphrase} value={value} onChange={onChange} secret="passphrase" placeholder="Stored with the key, or asked" />
          </Field>
        )}
      </div>

      {profile.auth === 'ssh' && method === 'agent' && (
        <EditorNote>Uses the keys of the SSH agent on the Termstead host (SSH_AUTH_SOCK or Pageant).</EditorNote>
      )}
      {profile.auth === 'ssh' && method === 'keyboard-interactive' && (
        <EditorNote>The server's prompts (password, one-time code…) are shown when you connect.</EditorNote>
      )}
      {profile.auth === 'ssh' && method === 'none' && <EditorNote>No authentication is attempted (restricted shells, some network devices).</EditorNote>}
    </section>
  )
}
