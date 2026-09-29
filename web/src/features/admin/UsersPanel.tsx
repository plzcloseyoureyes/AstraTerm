/*
 * MU-1 / MU-2 user administration: table with search, create, edit (display name, role), disable / enable, reset
 * password, reset two-factor (TOTP + passkeys), unlock, sign out everywhere, delete (never the last administrator).
 */
import { useMemo, useState } from 'react'
import {
  Ban,
  CircleCheck,
  Fingerprint,
  KeyRound,
  Link2,
  Lock,
  LockOpen,
  LogOut,
  MoreHorizontal,
  Pencil,
  Search,
  ShieldCheck,
  ShieldOff,
  Trash2,
  UserPlus,
  Users,
} from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import type { Role } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { QueryState } from '@/components/ui/query-state'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, errorMessage, formatDateTime } from '@/lib/utils'
import { setCurrentUser, useCurrentUser } from '@/stores/auth'
import {
  adminCreateUser,
  adminDeleteUser,
  adminResetMfa,
  adminResetPassword,
  adminRevokeUserSessions,
  adminUnlockUser,
  adminUpdateUser,
  secQK,
  useAdminUsers,
} from '@/features/security/api'
import { PageHeader, When, passwordProblem, policyHint, usePasswordPolicy } from '@/features/security/components'
import type { AdminUser } from '@/features/security/types'
import { ReauthCancelled } from '@/features/security/store'

type Dlg = { kind: 'create' } | { kind: 'edit'; user: AdminUser } | { kind: 'password'; user: AdminUser } | null

export function UsersPanel() {
  const users = useAdminUsers()
  const me = useCurrentUser()
  const qc = useQueryClient()
  const [q, setQ] = useState('')
  const [dlg, setDlg] = useState<Dlg>(null)
  const refresh = () => void qc.invalidateQueries({ queryKey: secQK.users })

  const rows = useMemo(() => {
    const n = q.trim().toLowerCase()
    const list = users.data ?? []
    return n ? list.filter((u) => [u.username, u.displayName, u.role, ...u.sso].some((v) => v.toLowerCase().includes(n))) : list
  }, [users.data, q])
  const admins = (users.data ?? []).filter((u) => u.role === 'admin' && !u.disabled).length

  const act = async (fn: () => Promise<unknown>, success: string) => {
    try {
      await fn()
      toast.success(success)
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      toast.error(errorMessage(err))
    } finally {
      refresh()
    }
  }

  const setDisabled = async (u: AdminUser, disabled: boolean) => {
    if (disabled) {
      const ok = await confirm({
        title: `Disable ${u.username}?`,
        description: 'They are signed out everywhere and cannot sign in until re-enabled. Their saved sessions and keys are kept.',
        confirmLabel: 'Disable',
        destructive: true,
      })
      if (!ok) return
    }
    await act(() => adminUpdateUser(u.id, { disabled }), disabled ? `${u.username} disabled` : `${u.username} enabled`)
  }

  const resetMfa = async (u: AdminUser) => {
    const ok = await confirm({
      title: `Reset two-factor for ${u.username}?`,
      description: 'Removes their authenticator app and all passkeys, e.g. after a lost phone. They can set them up again after signing in.',
      confirmLabel: 'Reset',
      destructive: true,
    })
    if (!ok) return
    try {
      const r = await adminResetMfa(u.id)
      toast.success('Two-factor reset', {
        description: [r.totpReset && 'authenticator app removed', r.passkeysRemoved && `${r.passkeysRemoved} passkey(s) removed`].filter(Boolean).join(', ') || 'Nothing was configured.',
      })
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      toast.error(errorMessage(err))
    } finally {
      refresh()
    }
  }

  const remove = async (u: AdminUser) => {
    const ok = await confirm({
      title: `Delete ${u.username}?`,
      description: 'Their connections, keys, snippets, tunnels, recordings metadata and settings are deleted. This cannot be undone.',
      confirmLabel: 'Delete user',
      destructive: true,
    })
    if (!ok) return
    await act(() => adminDeleteUser(u.id), `${u.username} deleted`)
  }

  return (
    <>
      <PageHeader
        title="Users"
        description="Accounts that can sign in to this Termstead server."
        actions={
          <Button onClick={() => setDlg({ kind: 'create' })}>
            <UserPlus /> New user
          </Button>
        }
      />
      <div className="mb-3 flex items-center gap-2">
        <div className="relative w-64 max-w-full">
          <Search className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search users" className="h-7 pl-7" aria-label="Search users" />
        </div>
        <span className="text-sm text-muted-foreground tabular-nums">
          {users.data ? `${users.data.length} user${users.data.length === 1 ? '' : 's'} · ${admins} admin${admins === 1 ? '' : 's'}` : ''}
        </span>
      </div>
      <QueryState
        query={users}
        skeleton={<SkeletonRows rows={5} rowHeight={48} className="rounded-lg border bg-card" />}
        errorTitle="Could not load the users"
        isEmpty={() => rows.length === 0}
        empty={<EmptyState icon={Users} title="No matching users" size="sm" />}
      >
        {() => (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full min-w-[26rem] border-collapse text-base">
              <thead className="text-left text-xs text-muted-foreground">
                <tr className="border-b">
                  <th className="px-3 py-2 font-medium">User</th>
                  <th className="px-2 py-2 font-medium">Role</th>
                  {/* On a narrow pane the details give way, the status and the actions stay. */}
                  <th className="hidden px-2 py-2 font-medium @2xl:table-cell">Sign-in</th>
                  <th className="hidden px-2 py-2 font-medium @2xl:table-cell">Last sign-in</th>
                  <th className="px-2 py-2 font-medium">Status</th>
                  <th className="w-px px-3 py-2" />
                </tr>
              </thead>
              <tbody>
                {rows.map((u) => {
                  const self = u.id === me?.id
                  const lastAdmin = u.role === 'admin' && !u.disabled && admins <= 1
                  return (
                    <tr key={u.id} className={cn('group border-b last:border-b-0 hover:bg-accent/30', u.disabled && 'text-muted-foreground')} onDoubleClick={() => setDlg({ kind: 'edit', user: u })}>
                      <td className="px-3 py-2">
                        <div className="flex items-center gap-2.5">
                          <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-primary/12 text-sm font-semibold text-primary">
                            {(u.displayName || u.username).charAt(0).toUpperCase()}
                          </div>
                          <div className="grid min-w-0">
                            <span className="truncate font-medium">
                              {u.displayName || u.username}
                              {self && <span className="ml-1.5 text-xs font-normal text-muted-foreground">(you)</span>}
                            </span>
                            <span className="truncate text-sm text-muted-foreground">@{u.username}</span>
                          </div>
                        </div>
                      </td>
                      <td className="px-2">
                        <Badge variant={u.role === 'admin' ? 'default' : 'secondary'}>{u.role === 'admin' ? 'Admin' : 'User'}</Badge>
                      </td>
                      <td className="hidden px-2 @2xl:table-cell">
                        <div className="flex flex-wrap items-center gap-1">
                          {u.hasPassword && (
                            <Tooltip content="Password">
                              <KeyRound className="size-3.5 text-muted-foreground" />
                            </Tooltip>
                          )}
                          {u.totpEnabled && (
                            <Tooltip content="Authenticator app (TOTP)">
                              <ShieldCheck className="size-3.5 text-success" />
                            </Tooltip>
                          )}
                          {u.passkeys > 0 && (
                            <Tooltip content={`${u.passkeys} passkey${u.passkeys === 1 ? '' : 's'}`}>
                              <span className="inline-flex items-center gap-0.5 text-sm text-success">
                                <Fingerprint className="size-3.5" />
                                {u.passkeys > 1 && <span className="tabular-nums">{u.passkeys}</span>}
                              </span>
                            </Tooltip>
                          )}
                          {u.sso.map((s) => (
                            <Tooltip key={s} content={`Single sign-on: ${s}`}>
                              <Badge variant="outline">
                                <Link2 /> {s}
                              </Badge>
                            </Tooltip>
                          ))}
                        </div>
                      </td>
                      <td className="hidden px-2 text-sm @2xl:table-cell">
                        <When value={u.lastLoginAt} />
                      </td>
                      <td className="px-2">
                        {u.disabled ? (
                          <Badge variant="destructive">
                            <Ban /> Disabled
                          </Badge>
                        ) : u.locked ? (
                          <Tooltip content={`Locked until ${formatDateTime(u.lockedUntil)}`}>
                            <Badge variant="warning">
                              <Lock /> Locked
                            </Badge>
                          </Tooltip>
                        ) : (
                          <span className="inline-flex items-center gap-1.5 text-sm text-muted-foreground">
                            <span className={cn('size-1.5 rounded-full', u.sessions > 0 ? 'bg-success' : 'bg-muted-foreground/40')} />
                            <span className="tabular-nums">{u.sessions > 0 ? `${u.sessions} signed in` : 'Active'}</span>
                          </span>
                        )}
                      </td>
                      <td className="px-3">
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <Button size="icon-sm" variant="ghost" aria-label={`Actions for ${u.username}`} className="opacity-70 group-hover:opacity-100 data-[state=open]:opacity-100">
                              <MoreHorizontal />
                            </Button>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end" className="min-w-52">
                            <DropdownMenuItem onSelect={() => setDlg({ kind: 'edit', user: u })}>
                              <Pencil /> Edit…
                            </DropdownMenuItem>
                            <DropdownMenuItem onSelect={() => setDlg({ kind: 'password', user: u })}>
                              <KeyRound /> {u.hasPassword ? 'Reset password…' : 'Set password…'}
                            </DropdownMenuItem>
                            <DropdownMenuItem disabled={!u.totpEnabled && u.passkeys === 0} onSelect={() => void resetMfa(u)}>
                              <ShieldOff /> Reset two-factor…
                            </DropdownMenuItem>
                            {(u.locked || u.failedLogins > 0) && (
                              <DropdownMenuItem onSelect={() => void act(() => adminUnlockUser(u.id), `${u.username} unlocked`)}>
                                <LockOpen /> Unlock account
                              </DropdownMenuItem>
                            )}
                            <DropdownMenuItem disabled={u.sessions === 0} onSelect={() => void act(() => adminRevokeUserSessions(u.id), `${u.username} signed out everywhere`)}>
                              <LogOut /> Sign out everywhere
                            </DropdownMenuItem>
                            <DropdownMenuSeparator />
                            {u.disabled ? (
                              <DropdownMenuItem onSelect={() => void setDisabled(u, false)}>
                                <CircleCheck /> Enable
                              </DropdownMenuItem>
                            ) : (
                              <DropdownMenuItem disabled={self || lastAdmin} onSelect={() => void setDisabled(u, true)}>
                                <Ban /> Disable
                              </DropdownMenuItem>
                            )}
                            <DropdownMenuItem variant="destructive" disabled={self || lastAdmin} onSelect={() => void remove(u)}>
                              <Trash2 /> Delete…
                            </DropdownMenuItem>
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </QueryState>
      <UserDialog dlg={dlg} onClose={() => setDlg(null)} onSaved={refresh} lastAdmin={admins <= 1} selfId={me?.id} />
    </>
  )
}

function UserDialog({ dlg, onClose, onSaved, lastAdmin, selfId }: { dlg: Dlg; onClose: () => void; onSaved: () => void; lastAdmin: boolean; selfId?: string }) {
  return (
    <Dialog open={!!dlg} onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="md">
        {dlg?.kind === 'create' && <CreateForm onClose={onClose} onSaved={onSaved} />}
        {dlg?.kind === 'edit' && <EditForm user={dlg.user} onClose={onClose} onSaved={onSaved} lastAdmin={lastAdmin} self={dlg.user.id === selfId} />}
        {dlg?.kind === 'password' && <PasswordForm user={dlg.user} onClose={onClose} onSaved={onSaved} />}
      </DialogContent>
    </Dialog>
  )
}

function CreateForm({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const policy = usePasswordPolicy()
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<Role>('user')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const userError = username && !/^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$/.test(username) ? 'Letters, digits, . _ @ - (max 64)' : null
  const pwError = passwordProblem(password, policy, username)
  const submit = async () => {
    if (!username || !password || userError || pwError || busy) return
    setBusy(true)
    setError(null)
    try {
      await adminCreateUser({ username: username.trim(), displayName: displayName.trim() || undefined, password, role })
      toast.success(`User ${username} created`, { description: 'Share the password through a secure channel; they can change it under Account & security.' })
      onSaved()
      onClose()
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="flex min-h-0 flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <DialogHeader>
        <DialogTitle>
          <UserPlus className="size-4" /> New user
        </DialogTitle>
        <DialogDescription>They sign in with this user name and password (and can add two-factor later).</DialogDescription>
      </DialogHeader>
      <DialogBody className="grid gap-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="User name" error={userError} required>
            <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" autoCapitalize="off" spellCheck={false} autoFocus />
          </Field>
          <Field label="Display name">
            <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder={username || 'Optional'} />
          </Field>
        </div>
        <Field label="Initial password" error={pwError} hint={!pwError ? policyHint(policy) : undefined} required>
          <PasswordInput value={password} onChange={(e) => setPassword(e.target.value)} generate autoComplete="new-password" />
        </Field>
        <Field label="Role">
          <SegmentedControl<Role>
            value={role}
            onValueChange={setRole}
            aria-label="Role"
            options={[
              { value: 'user', label: 'User' },
              { value: 'admin', label: 'Administrator' },
            ]}
          />
        </Field>
        {role === 'admin' && <p className="text-sm text-muted-foreground">Administrators manage users, global settings, the vault and see everyone’s sessions.</p>}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
      </DialogBody>
      <DialogFooter>
        <Button variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" loading={busy} disabled={!username || !password || !!userError || !!pwError}>
          Create user
        </Button>
      </DialogFooter>
    </form>
  )
}

function EditForm({ user, onClose, onSaved, lastAdmin, self }: { user: AdminUser; onClose: () => void; onSaved: () => void; lastAdmin: boolean; self: boolean }) {
  const [displayName, setDisplayName] = useState(user.displayName)
  const [role, setRole] = useState<Role>(user.role)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const demoteLocked = user.role === 'admin' && lastAdmin && !user.disabled
  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      const patch: { displayName?: string; role?: Role } = {}
      if (displayName.trim() !== user.displayName) patch.displayName = displayName.trim()
      if (role !== user.role) patch.role = role
      if (Object.keys(patch).length) {
        const u = await adminUpdateUser(user.id, patch)
        if (self) setCurrentUser(u)
        toast.success(`${user.username} updated`)
        onSaved()
      }
      onClose()
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="flex min-h-0 flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <DialogHeader>
        <DialogTitle>
          <Pencil className="size-4" /> Edit {user.username}
        </DialogTitle>
        <DialogDescription>
          Created {formatDateTime(user.createdAt)}
          {user.sso.length ? ` · signs in with ${user.sso.join(', ')}` : ''}
        </DialogDescription>
      </DialogHeader>
      <DialogBody className="grid gap-4">
        <Field label="Display name">
          <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={128} autoFocus />
        </Field>
        <Field label="Role" hint={demoteLocked ? 'The last administrator cannot be demoted.' : undefined}>
          <SegmentedControl<Role>
            value={role}
            onValueChange={setRole}
            aria-label="Role"
            options={[
              { value: 'user', label: 'User', disabled: demoteLocked },
              { value: 'admin', label: 'Administrator' },
            ]}
          />
        </Field>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
      </DialogBody>
      <DialogFooter>
        <Button variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" loading={busy}>
          Save
        </Button>
      </DialogFooter>
    </form>
  )
}

function PasswordForm({ user, onClose, onSaved }: { user: AdminUser; onClose: () => void; onSaved: () => void }) {
  const policy = usePasswordPolicy()
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const pwError = passwordProblem(password, policy, user.username)
  const submit = async () => {
    if (!password || pwError || busy) return
    setBusy(true)
    setError(null)
    try {
      await adminResetPassword(user.id, password)
      toast.success(`Password for ${user.username} ${user.hasPassword ? 'reset' : 'set'}`, { description: 'Their other sessions were signed out and any lock lifted.' })
      onSaved()
      onClose()
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="flex min-h-0 flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <DialogHeader>
        <DialogTitle>
          <KeyRound className="size-4" /> {user.hasPassword ? 'Reset' : 'Set'} password for {user.username}
        </DialogTitle>
        <DialogDescription>They are signed out everywhere and must use the new password. Two-factor stays as it is.</DialogDescription>
      </DialogHeader>
      <DialogBody className="grid gap-3">
        <Field label="New password" error={pwError ?? error} hint={!pwError && !error ? policyHint(policy) : undefined}>
          <PasswordInput value={password} onChange={(e) => setPassword(e.target.value)} generate autoComplete="new-password" autoFocus />
        </Field>
      </DialogBody>
      <DialogFooter>
        <Button variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" loading={busy} disabled={!password || !!pwError}>
          {user.hasPassword ? 'Reset password' : 'Set password'}
        </Button>
      </DialogFooter>
    </form>
  )
}
