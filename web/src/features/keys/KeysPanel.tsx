/*
 * "SSH keys" sub-tab: the stored keys (name, type, fingerprint, protection, certificate, usage) with copy / export /
 * install actions, and a details pane (public key, fingerprints, randomart, passphrase and certificate state).
 */
import { useMemo, useState, type KeyboardEvent, type ReactNode } from 'react'
import {
  ArrowRightLeft,
  BadgeCheck,
  Copy,
  Download,
  Ellipsis,
  FileDown,
  KeyRound,
  LockKeyhole,
  MessageSquareText,
  PenLine,
  Pencil,
  RefreshCw,
  Search,
  Server,
  ShieldCheck,
  Sparkles,
  Trash2,
  Upload,
  X,
} from 'lucide-react'
import { useConnections } from '@/api/connections'
import { useIdentities } from '@/api/identities'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingState } from '@/components/ui/query-state'
import { Spinner } from '@/components/ui/spinner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useManualRefresh } from '@/lib/hooks'
import { errorMessage, formatDateTime, formatRelativeTime, plural } from '@/lib/utils'
import { copyPublicKey, deleteKeyAction, editCommentAction, renameKeyAction, setKeyOffered } from './actions'
import { useKeys } from './api'
import { CertBadge, CertificateDetails, CopyField, Fingerprint, KeyTypeBadge, PassphraseBadge, PublicKeyBox, Randomart, SectionLabel } from './components'
import { keysSettings } from './settings'
import { openKeysDialog } from './store'
import type { StoredKey } from './types'

export function KeysPanel() {
  const { data, isLoading, isError, error, refetch } = useKeys()
  const manual = useManualRefresh(refetch)
  const [q, setQ] = useState('')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const keys = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (data ?? []).filter((k) => !needle || `${k.name} ${k.comment} ${k.type} ${k.fingerprint}`.toLowerCase().includes(needle))
  }, [data, q])
  const selected = keys.find((k) => k.id === selectedId) ?? null

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!keys.length || (e.target as HTMLElement).closest('input,button,[role=menu]')) return
    const i = keys.findIndex((k) => k.id === selectedId)
    const move = (n: number) => {
      e.preventDefault()
      const next = keys[Math.min(Math.max(n, 0), keys.length - 1)]
      setSelectedId(next.id)
      document.getElementById(`key-row-${next.id}`)?.focus()
    }
    switch (e.key) {
      case 'ArrowDown':
        return move(i + 1)
      case 'ArrowUp':
        return move(i < 0 ? 0 : i - 1)
      case 'Home':
        return move(0)
      case 'End':
        return move(keys.length - 1)
      case 'Delete':
        if (selected) {
          e.preventDefault()
          void deleteKeyAction(selected)
        }
        return
      case 'Escape':
        setSelectedId(null)
        return
    }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'c' && selected && !window.getSelection()?.toString()) {
      e.preventDefault()
      copyPublicKey(selected)
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-1.5 border-b px-3 py-2">
        <Button size="sm" onClick={() => openKeysDialog('generate', {})}>
          <Sparkles /> Generate…
        </Button>
        <Button size="sm" variant="secondary" onClick={() => openKeysDialog('importer', { mode: 'import' })}>
          <Upload /> Import…
        </Button>
        <Button size="sm" variant="ghost" onClick={() => openKeysDialog('importer', { mode: 'convert' })}>
          <ArrowRightLeft /> Convert…
        </Button>
        <Input inputSize="sm" className="ml-auto w-56" leading={<Search />} placeholder="Filter keys" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter keys" />
        <IconButton icon={RefreshCw} label="Refresh" onClick={manual.refresh} busy={manual.refreshing} />
      </div>
      <div className="@container grid min-h-0 flex-1 grid-rows-[1fr_auto] @4xl:grid-cols-[1fr_24rem] @4xl:grid-rows-1">
        <div className="@container min-h-0 overflow-auto" onKeyDown={onKeyDown}>
          <LoadingState busy={isLoading} skeleton={<div className="flex justify-center py-12">
              <Spinner />
            </div>}>
            {isError ? (
            <EmptyState
              icon={KeyRound}
              title="Could not load the keys"
              description={errorMessage(error)}
              action={
                <Button size="sm" variant="secondary" onClick={() => void refetch()}>
                  Retry
                </Button>
              }
            />
          ) : !data?.length ? (
            <EmptyState
              icon={KeyRound}
              title="No SSH keys yet"
              description="Generate a new key pair, or import an existing private key (OpenSSH, PEM or PuTTY .ppk). Keys are encrypted in the NexTerm vault."
              action={
                <>
                  <Button size="sm" onClick={() => openKeysDialog('generate', {})}>
                    <Sparkles /> Generate key
                  </Button>
                  <Button size="sm" variant="secondary" onClick={() => openKeysDialog('importer', { mode: 'import' })}>
                    <Upload /> Import key
                  </Button>
                </>
              }
            />
          ) : !keys.length ? (
            <EmptyState size="sm" icon={Search} title="No matching keys" />
          ) : (
            <Table aria-label="SSH keys">
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead className="hidden @2xl:table-cell">Fingerprint (SHA256)</TableHead>
                  <TableHead>Protection</TableHead>
                  <TableHead className="hidden @3xl:table-cell">Used by</TableHead>
                  <TableHead className="hidden @5xl:table-cell">Added</TableHead>
                  <TableHead className="w-0 text-right">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((k) => (
                  <KeyRow key={k.id} k={k} selected={k.id === selectedId} onSelect={() => setSelectedId(k.id === selectedId ? null : k.id)} />
                ))}
              </TableBody>
            </Table>
          )}
          </LoadingState>
        </div>
        {selected && <KeyDetails k={selected} onClose={() => setSelectedId(null)} />}
      </div>
    </div>
  )
}

function KeyRow({ k, selected, onSelect }: { k: StoredKey; selected: boolean; onSelect: () => void }) {
  const used = k.usedBy.connections + k.usedBy.identities
  return (
    <TableRow
      id={`key-row-${k.id}`}
      tabIndex={0}
      aria-selected={selected}
      data-state={selected ? 'selected' : undefined}
      className="group cursor-default focus-visible:bg-accent/40 focus-visible:outline-none"
      onClick={(e) => {
        if (!(e.target as HTMLElement).closest('button,[role=menuitem]')) onSelect()
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter' && e.target === e.currentTarget) onSelect()
      }}
    >
      <TableCell className="max-w-[16rem]">
        <div className="truncate font-medium">{k.name}</div>
        {k.comment && k.comment !== k.name && <div className="truncate text-sm text-muted-foreground">{k.comment}</div>}
      </TableCell>
      <TableCell>
        <KeyTypeBadge type={k.type} bits={k.bits} />
      </TableCell>
      <TableCell className="hidden max-w-[18rem] @2xl:table-cell">
        <Fingerprint value={k.fingerprint} />
      </TableCell>
      <TableCell>
        <div className="flex flex-wrap items-center gap-1">
          <PassphraseBadge k={k} />
          <CertBadge info={k.certificateInfo} />
          {!k.hasPrivateKey && <Badge variant="outline">Public only</Badge>}
        </div>
      </TableCell>
      <TableCell className="hidden text-sm text-muted-foreground @3xl:table-cell">
        {used === 0
          ? '—'
          : [k.usedBy.connections && plural(k.usedBy.connections, 'session'), k.usedBy.identities && plural(k.usedBy.identities, 'identity', 'identities')]
              .filter(Boolean)
              .join(', ')}
      </TableCell>
      <TableCell className="hidden text-sm whitespace-nowrap text-muted-foreground @5xl:table-cell" title={formatDateTime(k.createdAt)}>
        {formatRelativeTime(k.createdAt)}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex items-center justify-end gap-0.5 opacity-70 group-hover:opacity-100 group-focus-within:opacity-100">
          <IconButton icon={Copy} size="xs" label="Copy public key" onClick={() => copyPublicKey(k)} />
          <IconButton icon={FileDown} size="xs" label="Export…" onClick={() => openKeysDialog('exporter', { keyId: k.id })} />
          <IconButton icon={Server} size="xs" label="Install on a server…" onClick={() => openKeysDialog('install', { keyId: k.id })} />
          <KeyMenu k={k} />
        </div>
      </TableCell>
    </TableRow>
  )
}

export function KeyMenu({ k, trigger }: { k: StoredKey; trigger?: ReactNode }) {
  const excluded = keysSettings.useValue('agentExclude')
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>{trigger ?? <IconButton icon={Ellipsis} size="xs" label="More actions" />}</DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onSelect={() => void renameKeyAction(k)}>
          <Pencil /> Rename…
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => void editCommentAction(k)}>
          <MessageSquareText /> Edit comment…
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => openKeysDialog('passphrase', { keyId: k.id })} disabled={!k.hasPrivateKey}>
          <LockKeyhole /> Passphrase…
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => openKeysDialog('certificate', { keyId: k.id })}>
          <BadgeCheck /> {k.certificate ? 'Certificate…' : 'Attach certificate…'}
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => openKeysDialog('sign', { caKeyId: k.id })} disabled={!k.hasPrivateKey || k.type === 'dsa'}>
          <PenLine /> Sign a certificate with this key…
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => openKeysDialog('knownHost', { kind: 'marker', marker: 'cert-authority', keyId: k.id })}>
          <ShieldCheck /> Trust as host CA…
        </DropdownMenuItem>
        <DropdownMenuCheckboxItem checked={!excluded.includes(k.id)} onCheckedChange={(v) => setKeyOffered(k.id, v === true)} disabled={!k.hasPrivateKey}>
          Offer through the SSH agent
        </DropdownMenuCheckboxItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem variant="destructive" onSelect={() => void deleteKeyAction(k)}>
          <Trash2 /> Delete…
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function KeyDetails({ k, onClose }: { k: StoredKey; onClose: () => void }) {
  const conns = useConnections()
  const identities = useIdentities()
  const users = [
    ...(conns.data ?? []).filter((c) => c.keyId === k.id).map((c) => c.name),
    ...(identities.data ?? []).filter((i) => i.keyId === k.id).map((i) => `${i.name} (identity)`),
  ]
  const file = `${k.name.replace(/[^\w.-]+/g, '-').toLowerCase() || 'id_' + k.type}.pub`
  return (
    <aside aria-label={`Details of ${k.name}`} className="min-h-0 overflow-y-auto border-t bg-panel/40 @4xl:border-t-0 @4xl:border-l">
      <div className="flex items-start gap-2 border-b px-4 py-3">
        <div className="min-w-0 flex-1">
          <div className="truncate text-md font-semibold">{k.name}</div>
          <div className="text-sm text-muted-foreground">
            <KeyTypeBadge type={k.type} bits={k.bits} /> · added {formatDateTime(k.createdAt)}
          </div>
        </div>
        <IconButton icon={X} size="xs" label="Close details" onClick={onClose} />
      </div>
      <div className="grid gap-4 p-4">
        <div className="grid gap-1.5">
          <SectionLabel>Public key</SectionLabel>
          <PublicKeyBox value={k.publicKey} filename={file} rows={4} />
        </div>
        <CopyField label="SHA256 fingerprint" value={k.fingerprint} />
        <CopyField label="MD5 fingerprint" value={k.fingerprintMd5} />
        <div className="grid justify-items-start gap-1.5">
          <SectionLabel>Randomart</SectionLabel>
          <Randomart fingerprint={k.fingerprint} type={k.type} bits={k.bits} />
        </div>
        <div className="grid gap-1.5">
          <SectionLabel>Protection</SectionLabel>
          <div className="flex items-center gap-2 text-sm">
            <PassphraseBadge k={k} />
            <span className="text-muted-foreground">
              {k.hasPassphrase ? (k.passphraseSaved ? 'Passphrase remembered in the vault.' : 'Asks for the passphrase when used.') : 'Encrypted by the vault only.'}
            </span>
            <Button size="xs" variant="link" className="ml-auto" onClick={() => openKeysDialog('passphrase', { keyId: k.id })} disabled={!k.hasPrivateKey}>
              Change…
            </Button>
          </div>
        </div>
        <div className="grid gap-1.5">
          <div className="flex items-center justify-between">
            <SectionLabel>Certificate</SectionLabel>
            <Button size="xs" variant="link" onClick={() => openKeysDialog('certificate', { keyId: k.id })}>
              {k.certificate ? 'Manage…' : 'Attach…'}
            </Button>
          </div>
          {k.certificateInfo ? <CertificateDetails info={k.certificateInfo} /> : <p className="text-sm text-muted-foreground">No certificate attached.</p>}
        </div>
        <div className="grid gap-1.5">
          <SectionLabel>Used by</SectionLabel>
          {users.length ? (
            <ul className="grid gap-0.5 text-sm">
              {users.slice(0, 8).map((u, i) => (
                <li key={i} className="truncate">
                  {u}
                </li>
              ))}
              {users.length > 8 && <li className="text-muted-foreground">and {users.length - 8} more</li>}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">Not used by any session or identity.</p>
          )}
        </div>
        <div className="flex flex-wrap gap-1.5 border-t pt-3">
          <Button size="sm" variant="secondary" onClick={() => openKeysDialog('exporter', { keyId: k.id })}>
            <Download /> Export…
          </Button>
          <Button size="sm" variant="secondary" onClick={() => openKeysDialog('install', { keyId: k.id })}>
            <Server /> Install on server…
          </Button>
          <Button size="sm" variant="ghost" className="ml-auto text-destructive" onClick={() => void deleteKeyAction(k)}>
            <Trash2 /> Delete
          </Button>
        </div>
      </div>
    </aside>
  )
}
