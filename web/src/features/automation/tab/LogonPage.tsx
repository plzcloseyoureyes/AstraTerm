/*
 * Logon actions page (AUTO-8): saved connections with their expect/send steps.
 */
import * as React from 'react'
import { LogIn, Search } from 'lucide-react'
import { useConnections } from '@/api/connections'
import type { Connection } from '@/api/types'
import { protocolIcon } from '@/app/protocols'
import { Badge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'
import { LogonEditor } from '../logon/LogonEditor'

const TERMINAL_PROTOCOLS = new Set(['ssh', 'telnet', 'rlogin', 'raw', 'serial', 'local', 'mosh', 'docker', 'kube', 'winrm', 'ipmi'])

function logonCount(c: Connection): number {
  const a = (c.options as Record<string, unknown>).logonActions
  return Array.isArray(a) ? a.length : 0
}

export default function LogonPage({ connectionId }: { connectionId?: string }) {
  const connections = useConnections()
  const { data } = connections
  const [selected, setSelected] = React.useState<string | undefined>(connectionId)
  const [q, setQ] = React.useState('')
  React.useEffect(() => {
    if (connectionId) setSelected(connectionId)
  }, [connectionId])
  const conns = (data ?? []).filter((c) => TERMINAL_PROTOCOLS.has(c.protocol))
  const needle = q.trim().toLowerCase()
  const shown = conns
    .filter((c) => !needle || `${c.name} ${c.host} ${c.username}`.toLowerCase().includes(needle))
    .sort((a, b) => logonCount(b) - logonCount(a) || a.name.localeCompare(b.name))
  const current = conns.find((c) => c.id === selected)
  return (
    <div className="flex h-full min-h-0 flex-col @3xl:flex-row">
      <aside className="flex max-h-44 w-full shrink-0 flex-col border-b @3xl:max-h-none @3xl:w-64 @3xl:border-r @3xl:border-b-0" aria-label="Connections">
        <div className="border-b p-2">
          <Input inputSize="sm" leading={<Search />} placeholder="Search connections…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search connections" />
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-1">
          <QueryState query={connections} skeleton={<SkeletonRows rows={6} />} errorTitle="Could not load the connections">
            {() =>
              !shown.length ? (
                <p className="p-2 text-sm text-muted-foreground">{conns.length ? 'Nothing matches.' : 'No terminal connections.'}</p>
              ) : (
                shown.map((c) => {
                  const Icon = protocolIcon(c.protocol)
                  const n = logonCount(c)
                  return (
                    <button
                      key={c.id}
                      type="button"
                      onClick={() => setSelected(c.id)}
                      className={cn('flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60', c.id === selected && 'bg-accent')}
                    >
                      <Icon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
                      <span className="min-w-0 flex-1 truncate">{c.name}</span>
                      {n > 0 && <Badge variant="default">{n}</Badge>}
                    </button>
                  )
                })
              )
            }
          </QueryState>
        </div>
      </aside>
      <section className="min-w-0 flex-1 overflow-y-auto p-3" aria-label="Logon actions">
        {current ? (
          <div className="flex max-w-4xl flex-col gap-3">
            <h2 className="flex items-center gap-2 text-md font-semibold">
              <LogIn className="size-4 text-primary" /> {current.name}
              <span className="text-sm font-normal text-muted-foreground">
                {current.username ? `${current.username}@` : ''}
                {current.host}
              </span>
            </h2>
            <LogonEditor key={current.id} connectionId={current.id} />
          </div>
        ) : (
          data && (
            <EmptyState
              icon={LogIn}
              title="Logon actions"
              description="Pick a connection to type its log-in sequence automatically after every connect: user names, passwords from the vault, enable secrets, “terminal length 0”…"
            />
          )
        )}
      </section>
    </div>
  )
}
