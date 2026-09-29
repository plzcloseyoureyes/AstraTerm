/*
 * "Known hosts" sub-tab (SSH-19/20): the trusted host keys shared by every user (search, multi-select delete, add,
 * import, export — optionally hashed) and the certificate authorities / revoked keys (known_hosts @cert-authority and
 * @revoked). In server mode only administrators can change them.
 */
import { useMemo, useState } from 'react'
import { ChevronDown, Download, FileUp, Plus, RefreshCw, Search, ShieldCheck, ShieldX, Trash2 } from 'lucide-react'
import type { KnownHost } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingState } from '@/components/ui/query-state'
import { Spinner } from '@/components/ui/spinner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip } from '@/components/ui/tooltip'
import { useManualRefresh } from '@/lib/hooks'
import { errorMessage, formatDateTime, formatRelativeTime } from '@/lib/utils'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { deleteKnownHostsAction, deleteMarkerAction, exportKnownHostsAction, hostLabel } from './actions'
import { useKnownHosts, useMarkers } from './api'
import { Fingerprint, SectionLabel } from './components'
import { openKeysDialog } from './store'

export function KnownHostsPanel() {
  const hosts = useKnownHosts()
  const markers = useMarkers()
  const manual = useManualRefresh(() => Promise.all([hosts.refetch(), markers.refetch()]))
  const mode = useRunMode()
  const admin = useIsAdmin()
  const canManage = mode === 'desktop' || admin
  const [q, setQ] = useState('')
  const [checked, setChecked] = useState<Set<string>>(new Set())

  const list = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (hosts.data ?? []).filter((h) => !needle || `${hostLabel(h)} ${h.keyType} ${h.fingerprint} ${h.comment}`.toLowerCase().includes(needle))
  }, [hosts.data, q])
  const selected = list.filter((h) => checked.has(h.id))
  const allChecked = list.length > 0 && selected.length === list.length
  const toggle = (id: string, on: boolean) =>
    setChecked((cur) => {
      const next = new Set(cur)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })

  const readOnly = !canManage ? 'Only administrators can change the trusted host keys in server mode' : undefined

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-1.5 border-b px-3 py-2">
        <Tooltip content={readOnly} disabled={!readOnly}>
          <span className="flex gap-1.5">
            <Button size="sm" onClick={() => openKeysDialog('knownHost', { kind: 'host' })} disabled={!canManage}>
              <Plus /> Add…
            </Button>
            <Button size="sm" variant="secondary" onClick={() => openKeysDialog('knownHostsImport', {})} disabled={!canManage}>
              <FileUp /> Import…
            </Button>
          </span>
        </Tooltip>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button size="sm" variant="ghost">
              <Download /> Export <ChevronDown className="size-3.5 opacity-70" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent>
            <DropdownMenuItem onSelect={() => void exportKnownHostsAction(false)}>known_hosts</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => void exportKnownHostsAction(true)}>known_hosts with hashed host names</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
        {selected.length > 0 && canManage && (
          <Button size="sm" variant="ghost" className="text-destructive" onClick={() => void deleteKnownHostsAction(selected).then(() => setChecked(new Set()))}>
            <Trash2 /> Forget {selected.length}
          </Button>
        )}
        <Input inputSize="sm" className="ml-auto w-56" leading={<Search />} placeholder="Filter hosts" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter hosts" />
        <IconButton
          icon={RefreshCw}
          label="Refresh"
          busy={manual.refreshing}
          onClick={manual.refresh}
        />
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        <LoadingState busy={hosts.isLoading} skeleton={<div className="flex justify-center py-12">
            <Spinner />
          </div>}>
          {hosts.isError ? (
          <EmptyState
            icon={ShieldCheck}
            title="Could not load the known hosts"
            description={errorMessage(hosts.error)}
            action={
              <Button size="sm" variant="secondary" onClick={() => void hosts.refetch()}>
                Retry
              </Button>
            }
          />
        ) : !hosts.data?.length ? (
          <EmptyState
            icon={ShieldCheck}
            title="No trusted host keys yet"
            description="Host keys you accept when connecting are listed here. You can also import your OpenSSH known_hosts or PuTTY host keys."
            action={
              canManage && (
                <Button size="sm" variant="secondary" onClick={() => openKeysDialog('knownHostsImport', {})}>
                  <FileUp /> Import known hosts
                </Button>
              )
            }
          />
        ) : !list.length ? (
          <EmptyState size="sm" icon={Search} title="No matching hosts" />
        ) : (
          <Table aria-label="Trusted host keys">
            <TableHeader>
              <TableRow>
                {canManage && (
                  <TableHead className="w-8">
                    <Checkbox aria-label="Select all" checked={allChecked ? true : selected.length ? 'indeterminate' : false} onCheckedChange={(v) => setChecked(v === true ? new Set(list.map((h) => h.id)) : new Set())} />
                  </TableHead>
                )}
                <TableHead>Host</TableHead>
                <TableHead>Key type</TableHead>
                <TableHead>Fingerprint (SHA256)</TableHead>
                <TableHead className="hidden @3xl:table-cell">Comment</TableHead>
                <TableHead className="hidden @2xl:table-cell">Added</TableHead>
                {canManage && (
                  <TableHead className="w-0">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                )}
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((h) => (
                <HostRow key={h.id} h={h} canManage={canManage} checked={checked.has(h.id)} onCheck={(v) => toggle(h.id, v)} />
              ))}
            </TableBody>
          </Table>
        )}
        </LoadingState>
        <MarkersSection canManage={canManage} />
      </div>
    </div>
  )
}

function HostRow({ h, canManage, checked, onCheck }: { h: KnownHost; canManage: boolean; checked: boolean; onCheck: (v: boolean) => void }) {
  return (
    <TableRow className="group" data-state={checked ? 'selected' : undefined}>
      {canManage && (
        <TableCell>
          <Checkbox aria-label={`Select ${hostLabel(h)}`} checked={checked} onCheckedChange={(v) => onCheck(v === true)} />
        </TableCell>
      )}
      <TableCell className="font-mono text-sm">{hostLabel(h)}</TableCell>
      <TableCell className="font-mono text-sm text-muted-foreground">{h.keyType}</TableCell>
      <TableCell className="max-w-[20rem]">
        <Fingerprint value={h.fingerprint} />
      </TableCell>
      <TableCell className="hidden max-w-[16rem] truncate text-sm text-muted-foreground @3xl:table-cell" title={h.comment}>
        {h.comment || '—'}
      </TableCell>
      <TableCell className="hidden text-sm whitespace-nowrap text-muted-foreground @2xl:table-cell" title={formatDateTime(h.createdAt)}>
        {formatRelativeTime(h.createdAt)}
      </TableCell>
      {canManage && (
        <TableCell>
          <IconButton icon={Trash2} size="xs" label="Forget this key" className="opacity-60 group-hover:opacity-100 focus-visible:opacity-100" onClick={() => void deleteKnownHostsAction([h])} />
        </TableCell>
      )}
    </TableRow>
  )
}

function MarkersSection({ canManage }: { canManage: boolean }) {
  const markers = useMarkers()
  const list = markers.data ?? []
  return (
    <section aria-labelledby="markers-title" className="mt-4 border-t">
      <div className="flex flex-wrap items-center gap-2 px-3 pt-3 pb-2">
        <SectionLabel>
          <span id="markers-title">Certificate authorities &amp; revoked keys</span>
        </SectionLabel>
        {canManage && (
          <div className="ml-auto flex gap-1.5">
            <Button size="xs" variant="secondary" onClick={() => openKeysDialog('knownHost', { kind: 'marker', marker: 'cert-authority' })}>
              <ShieldCheck /> Trust a CA…
            </Button>
            <Button size="xs" variant="secondary" onClick={() => openKeysDialog('knownHost', { kind: 'marker', marker: 'revoked' })}>
              <ShieldX /> Revoke a key…
            </Button>
          </div>
        )}
      </div>
      <LoadingState busy={markers.isLoading} skeleton={<div className="flex justify-center py-6">
          <Spinner />
        </div>}>
        {markers.isError ? (
        <p role="alert" className="flex flex-wrap items-center gap-2 px-3 pb-4 text-sm text-destructive">
          Could not load the certificate authorities and revoked keys: {errorMessage(markers.error)}
          <Button size="xs" variant="secondary" onClick={() => void markers.refetch()}>
            Retry
          </Button>
        </p>
      ) : !list.length ? (
        <p className="px-3 pb-4 text-sm text-muted-foreground">
          None. Trust a CA to accept host certificates it signed without prompts (known_hosts @cert-authority).
        </p>
      ) : (
        <Table aria-label="Certificate authorities and revoked keys">
          <TableHeader>
            <TableRow>
              <TableHead>Kind</TableHead>
              <TableHead>Hosts</TableHead>
              <TableHead>Key</TableHead>
              <TableHead className="hidden @3xl:table-cell">Comment</TableHead>
              <TableHead className="hidden @2xl:table-cell">Added</TableHead>
              {canManage && (
                <TableHead className="w-0">
                  <span className="sr-only">Actions</span>
                </TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((m) => (
              <TableRow key={m.id} className="group">
                <TableCell>
                  {m.marker === 'cert-authority' ? (
                    <Badge variant="success">
                      <ShieldCheck /> CA
                    </Badge>
                  ) : (
                    <Badge variant="destructive">
                      <ShieldX /> Revoked
                    </Badge>
                  )}
                </TableCell>
                <TableCell className="max-w-[16rem] truncate font-mono text-sm" title={m.hosts}>
                  {m.hosts}
                </TableCell>
                <TableCell className="max-w-[20rem]">
                  <div className="flex items-center gap-1.5">
                    <span className="font-mono text-sm text-muted-foreground">{m.keyType}</span>
                    <Fingerprint value={m.fingerprint} />
                  </div>
                </TableCell>
                <TableCell className="hidden max-w-[14rem] truncate text-sm text-muted-foreground @3xl:table-cell" title={m.comment}>
                  {m.comment || '—'}
                </TableCell>
                <TableCell className="hidden text-sm whitespace-nowrap text-muted-foreground @2xl:table-cell">{formatRelativeTime(m.createdAt)}</TableCell>
                {canManage && (
                  <TableCell>
                    <IconButton icon={Trash2} size="xs" label="Remove" className="opacity-60 group-hover:opacity-100 focus-visible:opacity-100" onClick={() => void deleteMarkerAction(m)} />
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      </LoadingState>
    </section>
  )
}
