/*
 * Install a public key on a server (TOOL-1, ssh-copy-id semantics): pick a stored key and a saved SSH connection;
 * NexTerm logs in with the connection's credentials (prompts appear as usual), creates ~/.ssh (0700) and appends the
 * key to ~/.ssh/authorized_keys (0600) unless it is already there. Optionally the connection then uses the key.
 */
import { useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { CircleAlert, CircleCheck, Search, Server } from 'lucide-react'
import { updateConnection, useConnections } from '@/api/connections'
import { queryKeys } from '@/api/queryKeys'
import type { Connection } from '@/api/types'
import { protocolIcon } from '@/app/protocols'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { LoadingState } from '@/components/ui/query-state'
import { Spinner } from '@/components/ui/spinner'
import { cn, errorMessage } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { installKey, invalidateKeys, useKeys } from '../api'
import { KeySelect } from '../components'
import type { InstallResult } from '../types'

const SSH_PROTOCOLS = new Set(['ssh', 'sftp', 'mosh'])

function target(c: Connection): string {
  const port = c.port && c.port !== 22 ? `:${c.port}` : ''
  return `${c.username ? `${c.username}@` : ''}${c.host}${port}`
}

export default function InstallDialog({ keyId: initialKey, connectionId: initialConn, onClose }: { keyId?: string; connectionId?: string; onClose: () => void }) {
  const qc = useQueryClient()
  const me = useCurrentUser()
  const keys = useKeys()
  const conns = useConnections()
  const [keyId, setKeyId] = useState(initialKey ?? '')
  const [connId, setConnId] = useState(initialConn ?? '')
  const [q, setQ] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<InstallResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [useKey, setUseKey] = useState(true)

  const candidates = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (conns.data ?? [])
      .filter((c) => SSH_PROTOCOLS.has(c.protocol))
      .filter((c) => !needle || `${c.name} ${target(c)} ${c.tags.join(' ')}`.toLowerCase().includes(needle))
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [conns.data, q])
  const conn = conns.data?.find((c) => c.id === connId)
  const key = keys.data?.find((k) => k.id === keyId)
  const canSetKey = !!conn && conn.ownerId === me?.id && conn.keyId !== keyId

  const install = async () => {
    if (!keyId || !connId) return
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      const res = await installKey(keyId, connId)
      setResult(res)
      if (useKey && canSetKey && conn) {
        try {
          await updateConnection(conn.id, { keyId, ...(conn.authMethod === 'password' || conn.authMethod === 'keyboard-interactive' ? { authMethod: 'auto' } : {}) })
          void qc.invalidateQueries({ queryKey: queryKeys.connections })
          invalidateKeys(qc)
        } catch (err) {
          toast.error('The key was installed, but the session could not be updated', { description: errorMessage(err) })
        }
      }
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <Server className="size-4.5 text-primary" /> Install public key on a server
          </DialogTitle>
          <DialogDescription>Appends the key to ~/.ssh/authorized_keys on the server, like ssh-copy-id.</DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-4">
          <Field label="Key">
            <KeySelect keys={keys.data} value={keyId} onChange={(v) => { setKeyId(v); setResult(null) }} noneLabel="" />
          </Field>
          <div className="grid gap-1.5">
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm font-medium">Server</span>
              <Input inputSize="sm" className="w-56" leading={<Search />} placeholder="Filter sessions" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter sessions" />
            </div>
            <div role="listbox" aria-label="SSH sessions" className="max-h-64 overflow-y-auto rounded-md border">
              <LoadingState busy={conns.isLoading} skeleton={<div className="flex justify-center py-6">
                  <Spinner />
                </div>}>
                {candidates.length === 0 ? (
                <EmptyState size="sm" icon={Server} title={q ? 'No matching sessions' : 'No SSH sessions'} description={q ? undefined : 'Create an SSH session first.'} />
              ) : (
                candidates.map((c) => {
                  const Icon = protocolIcon(c.protocol)
                  const selected = c.id === connId
                  return (
                    <button
                      key={c.id}
                      type="button"
                      role="option"
                      aria-selected={selected}
                      onClick={() => {
                        setConnId(c.id)
                        setResult(null)
                        setError(null)
                      }}
                      className={cn(
                        'flex w-full items-center gap-2 border-b px-3 py-1.5 text-left last:border-b-0 hover:bg-accent/50 focus-visible:bg-accent/60 focus-visible:outline-none',
                        selected && 'bg-primary/10 hover:bg-primary/15',
                      )}
                    >
                      <Icon className="size-4 shrink-0 text-muted-foreground" />
                      <span className="truncate font-medium">{c.name}</span>
                      <span className="ml-auto truncate font-mono text-sm text-muted-foreground">{target(c)}</span>
                    </button>
                  )
                })
              )}
              </LoadingState>
            </div>
          </div>
          {conn && canSetKey && (
            <CheckboxField
              checked={useKey}
              onCheckedChange={(v) => setUseKey(v === true)}
              label={`Use this key for “${conn.name}”`}
              description={conn.keyId ? 'Replaces the key currently selected for the session.' : 'The session then logs in with the key instead of a password.'}
            />
          )}
          {busy && (
            <div className="flex items-center gap-2 text-sm text-muted-foreground" role="status">
              <Spinner className="size-3.5" /> Connecting to {conn ? target(conn) : 'the server'}… answer any login prompt that appears.
            </div>
          )}
          {result && (
            <div role="status" className="flex items-start gap-2 rounded-md border border-success/40 bg-success/8 px-3 py-2 text-sm">
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-success" />
              <span>
                {result.alreadyPresent ? 'The key was already authorized' : 'Key installed'} in <span className="font-mono">{result.path}</span> on{' '}
                <span className="font-mono">{result.target}</span> (via {result.method === 'sftp' ? 'SFTP' : 'the shell'}).
                {key && ` “${key.name}” can now be used to log in.`}
              </span>
            </div>
          )}
          {error && (
            <div role="alert" className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/8 px-3 py-2 text-sm text-destructive">
              <CircleAlert className="mt-0.5 size-4 shrink-0" /> {error}
            </div>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            {result ? 'Close' : 'Cancel'}
          </Button>
          {!result && (
            <Button onClick={() => void install()} loading={busy} disabled={!keyId || !connId}>
              <Server /> Install
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
