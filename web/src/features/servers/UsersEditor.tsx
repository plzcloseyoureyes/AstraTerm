/*
 * Accounts editor of the FTP / SFTP / Telnet / HTTP servers. Passwords are write-only: an empty field keeps the
 * stored password, "Remove" deletes it; SFTP users may have public keys (authorized_keys lines).
 */
import { useId, useState } from 'react'
import { ChevronDown, ChevronRight, KeyRound, Plus, Trash2, UserRound } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { Textarea } from '@/components/ui/textarea'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, uid } from '@/lib/utils'
import type { ServerUser } from './types'

const NAME_RE = /^[A-Za-z0-9._@+-]+$/

/** Client-side validation mirroring the server's (the server stays authoritative). */
export function userProblems(users: ServerUser[], opts: { keys: boolean }): Map<number, string> {
  const out = new Map<number, string>()
  const seen = new Set<string>()
  users.forEach((u, i) => {
    const name = u.username.trim()
    if (!name) out.set(i, 'Enter a user name')
    else if (!NAME_RE.test(name) || name.length > 64) out.set(i, 'Letters, digits and . _ @ + - only')
    else if (/^(anonymous|ftp)$/i.test(name)) out.set(i, `“${name}” is reserved for anonymous access`)
    else if (seen.has(name.toLowerCase())) out.set(i, 'Duplicate user name')
    else if (u.password !== undefined && u.password !== '' && [...u.password].length < 4) out.set(i, 'Passwords need at least 4 characters')
    else {
      const hasPw = u.password !== undefined ? u.password !== '' : !!u.hasPassword
      const hasKey = opts.keys && (u.publicKeys ?? []).some((k) => k.trim() && !k.trim().startsWith('#'))
      if (!hasPw && !hasKey) out.set(i, opts.keys ? 'Set a password or add a public key' : 'Set a password')
    }
    seen.add(name.toLowerCase())
  })
  return out
}

export function UsersEditor({
  users,
  onChange,
  keys = false,
  readOnlyOption = false,
  problems,
}: {
  users: ServerUser[]
  onChange: (users: ServerUser[]) => void
  /** Offer public keys (SFTP). */
  keys?: boolean
  /** Offer the per-user read-only flag (file servers). */
  readOnlyOption?: boolean
  problems: Map<number, string>
}) {
  const update = (i: number, patch: Partial<ServerUser>) => onChange(users.map((u, j) => (j === i ? { ...u, ...patch } : u)))
  const remove = (i: number) => onChange(users.filter((_, j) => j !== i))
  const add = () =>
    onChange([...users, { key: uid('user'), username: users.length ? '' : 'user', password: '', publicKeys: [], readOnly: false }])

  return (
    <div className="grid gap-2">
      {users.length === 0 ? (
        <div className="flex flex-col items-center gap-2 rounded-md border border-dashed px-4 py-6 text-center">
          <UserRound className="size-5 text-muted-foreground" aria-hidden />
          <p className="text-sm text-muted-foreground">No users yet. Clients need an account to log in.</p>
          <Button size="sm" variant="secondary" onClick={add}>
            <Plus /> Add user
          </Button>
        </div>
      ) : (
        <ul className="grid gap-2" aria-label="Users">
          {users.map((u, i) => (
            <UserRow
              key={u.id ?? u.key ?? `new-${i}`}
              user={u}
              index={i}
              keys={keys}
              readOnlyOption={readOnlyOption}
              problem={problems.get(i)}
              onChange={(p) => update(i, p)}
              onRemove={() => remove(i)}
            />
          ))}
        </ul>
      )}
      {users.length > 0 && (
        <div>
          <Button size="sm" variant="ghost" onClick={add} disabled={users.length >= 100}>
            <Plus /> Add user
          </Button>
        </div>
      )}
    </div>
  )
}

function UserRow({
  user,
  index,
  keys,
  readOnlyOption,
  problem,
  onChange,
  onRemove,
}: {
  user: ServerUser
  index: number
  keys: boolean
  readOnlyOption: boolean
  problem?: string
  onChange: (patch: Partial<ServerUser>) => void
  onRemove: () => void
}) {
  const id = useId()
  const keyCount = (user.publicKeys ?? []).filter((k) => k.trim() && !k.trim().startsWith('#')).length
  const [showKeys, setShowKeys] = useState(keys && keyCount > 0)
  const removing = user.password === '' && user.hasPassword
  const credentialProblem = !!problem && /password|key/i.test(problem)
  const pwPlaceholder = user.hasPassword ? (removing ? 'Password will be removed' : '•••••••• (unchanged)') : 'Set a password'
  return (
    <li className={cn('grid gap-2 rounded-md border bg-muted/20 p-2.5', problem && 'border-destructive/50')}>
      <div className="grid grid-cols-1 items-start gap-2 sm:grid-cols-[minmax(0,11rem)_minmax(0,1fr)_auto]">
        <div className="grid gap-1">
          <label htmlFor={`${id}-name`} className="sr-only">
            User name {index + 1}
          </label>
          <Input
            id={`${id}-name`}
            inputSize="sm"
            value={user.username}
            placeholder="user name"
            autoComplete="off"
            spellCheck={false}
            aria-invalid={(!!problem && !credentialProblem) || undefined}
            onChange={(e) => onChange({ username: e.target.value })}
          />
        </div>
        <div className="flex min-w-0 items-center gap-1">
          <label htmlFor={`${id}-pw`} className="sr-only">
            Password of {user.username || `user ${index + 1}`}
          </label>
          <PasswordInput
            id={`${id}-pw`}
            inputSize="sm"
            generate
            value={removing ? '' : (user.password ?? '')}
            placeholder={pwPlaceholder}
            aria-invalid={credentialProblem || undefined}
            autoComplete="new-password"
            onChange={(e) => onChange({ password: e.target.value === '' ? undefined : e.target.value })}
          />
          {user.hasPassword && (
            <Tooltip content={removing ? 'Keep the stored password' : 'Remove the stored password'}>
              <Button
                variant="ghost"
                size="xs"
                className="shrink-0"
                onClick={() => onChange({ password: removing ? undefined : '' })}
              >
                {removing ? 'Keep' : 'Remove'}
              </Button>
            </Tooltip>
          )}
        </div>
        <div className="flex items-center gap-1 justify-self-end">
          {readOnlyOption && (
            <label className="flex items-center gap-1.5 px-1 text-sm whitespace-nowrap select-none">
              <Checkbox checked={!!user.readOnly} onCheckedChange={(v) => onChange({ readOnly: v === true })} />
              Read-only
            </label>
          )}
          {keys && (
            <Tooltip content="Public keys">
              <Button
                variant="ghost"
                size="xs"
                aria-expanded={showKeys}
                aria-controls={`${id}-keys`}
                onClick={() => setShowKeys((v) => !v)}
              >
                {showKeys ? <ChevronDown /> : <ChevronRight />}
                <KeyRound /> {keyCount}
              </Button>
            </Tooltip>
          )}
          <Tooltip content="Remove user">
            <Button variant="ghost" size="icon-xs" aria-label={`Remove ${user.username || 'user'}`} onClick={onRemove}>
              <Trash2 />
            </Button>
          </Tooltip>
        </div>
      </div>
      {keys && showKeys && (
        <div className="grid gap-1" id={`${id}-keys`}>
          <label htmlFor={`${id}-k`} className="text-xs text-muted-foreground">
            Authorized keys (one OpenSSH public key per line)
          </label>
          <Textarea
            id={`${id}-k`}
            mono
            rows={3}
            spellCheck={false}
            placeholder="ssh-ed25519 AAAAC3… user@laptop"
            value={(user.publicKeys ?? []).join('\n')}
            onChange={(e) => onChange({ publicKeys: e.target.value.split('\n') })}
          />
        </div>
      )}
      {problem && (
        <p role="alert" className="text-xs text-destructive">
          {problem}
        </p>
      )}
    </li>
  )
}

/** Payload for PUT: write-only passwords only when changed, keys cleaned. */
export function usersPayload(users: ServerUser[], keys: boolean): Record<string, unknown>[] {
  return users.map((u) => {
    const out: Record<string, unknown> = { username: u.username.trim(), readOnly: !!u.readOnly }
    if (u.id) out.id = u.id
    if (u.password !== undefined) out.password = u.password
    if (keys) out.publicKeys = (u.publicKeys ?? []).map((k) => k.trim()).filter((k) => k && !k.startsWith('#'))
    return out
  })
}
