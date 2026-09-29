/*
 * "Identities" sub-tab (SM-7): reusable credential sets (user name, password, SSH key + passphrase) referenced by
 * sessions — change once, apply everywhere. Uses the core /api/identities endpoints; "used by" counts come from the
 * session list.
 */
import { useMemo, useState } from 'react'
import { KeyRound, Pencil, Plus, Search, Trash2, UserRound } from 'lucide-react'
import { useConnections } from '@/api/connections'
import { useIdentities } from '@/api/identities'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingState } from '@/components/ui/query-state'
import { Spinner } from '@/components/ui/spinner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip } from '@/components/ui/tooltip'
import { errorMessage, formatDateTime, formatRelativeTime, plural } from '@/lib/utils'
import { deleteIdentityAction } from './actions'
import { useKeys } from './api'
import { openKeysDialog } from './store'
import { keyTypeLabel } from './util'

export function IdentitiesPanel() {
  const identities = useIdentities()
  const conns = useConnections()
  const keys = useKeys()
  const [q, setQ] = useState('')
  const usage = useMemo(() => {
    const m = new Map<string, string[]>()
    for (const c of conns.data ?? []) {
      if (c.identityId) m.set(c.identityId, [...(m.get(c.identityId) ?? []), c.name])
    }
    return m
  }, [conns.data])
  const list = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (identities.data ?? []).filter((i) => !needle || `${i.name} ${i.username}`.toLowerCase().includes(needle))
  }, [identities.data, q])

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-1.5 border-b px-3 py-2">
        <Button size="sm" onClick={() => openKeysDialog('identity', {})}>
          <Plus /> New identity…
        </Button>
        <p className="hidden text-sm text-muted-foreground @2xl:block">Reusable credentials: edit once, every session using the identity follows.</p>
        <Input inputSize="sm" className="ml-auto w-56" leading={<Search />} placeholder="Filter identities" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter identities" />
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        <LoadingState busy={identities.isLoading} skeleton={<div className="flex justify-center py-12">
            <Spinner />
          </div>}>
          {identities.isError ? (
          <EmptyState
            icon={UserRound}
            title="Could not load the identities"
            description={errorMessage(identities.error)}
            action={
              <Button size="sm" variant="secondary" onClick={() => void identities.refetch()}>
                Retry
              </Button>
            }
          />
        ) : !identities.data?.length ? (
          <EmptyState
            icon={UserRound}
            title="No identities yet"
            description="An identity bundles a user name with a password and/or an SSH key. Select it in many sessions and update the credentials in one place."
            action={
              <Button size="sm" onClick={() => openKeysDialog('identity', {})}>
                <Plus /> New identity
              </Button>
            }
          />
        ) : !list.length ? (
          <EmptyState size="sm" icon={Search} title="No matching identities" />
        ) : (
          <Table aria-label="Identities">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>User name</TableHead>
                <TableHead>Credentials</TableHead>
                <TableHead>Used by</TableHead>
                <TableHead className="hidden @3xl:table-cell">Updated</TableHead>
                <TableHead className="w-0 text-right">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((i) => {
                const key = i.keyId ? keys.data?.find((k) => k.id === i.keyId) : undefined
                const users = usage.get(i.id) ?? []
                return (
                  <TableRow key={i.id} className="group" onDoubleClick={() => openKeysDialog('identity', { id: i.id })}>
                    <TableCell className="font-medium">{i.name}</TableCell>
                    <TableCell className="font-mono text-sm">{i.username || <span className="text-muted-foreground">—</span>}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {i.secretKeys.includes('password') && <Badge variant="secondary">Password</Badge>}
                        {i.keyId && (
                          <Badge variant="info">
                            <KeyRound /> {key ? `${key.name} · ${keyTypeLabel(key.type, key.bits)}` : 'Deleted key'}
                          </Badge>
                        )}
                        {i.secretKeys.includes('passphrase') && <Badge variant="secondary">Key passphrase</Badge>}
                        {!i.secretKeys.length && !i.keyId && <span className="text-sm text-muted-foreground">User name only</span>}
                      </div>
                    </TableCell>
                    <TableCell className="text-sm">
                      {users.length ? (
                        <Tooltip content={users.slice(0, 12).join(', ') + (users.length > 12 ? '…' : '')}>
                          <span>{plural(users.length, 'session')}</span>
                        </Tooltip>
                      ) : (
                        <span className="text-muted-foreground">Unused</span>
                      )}
                    </TableCell>
                    <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell" title={formatDateTime(i.updatedAt)}>
                      {formatRelativeTime(i.updatedAt)}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-0.5 opacity-70 group-hover:opacity-100 group-focus-within:opacity-100">
                        <IconButton icon={Pencil} size="xs" label="Edit…" onClick={() => openKeysDialog('identity', { id: i.id })} />
                        <IconButton icon={Trash2} size="xs" label="Delete…" onClick={() => void deleteIdentityAction(i, users.length)} />
                      </div>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
        </LoadingState>
      </div>
    </div>
  )
}
