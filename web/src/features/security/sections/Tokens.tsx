import { KeyRound, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { deleteToken, useApiTokens } from '@/api/auth'
import { queryKeys } from '@/api/queryKeys'
import type { APIToken } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tooltip } from '@/components/ui/tooltip'
import { errorMessage, formatDateTime } from '@/lib/utils'
import { PageHeader, When } from '../components'
import { openCreateToken } from '../store'

function expiry(t: APIToken): { label: string; variant: 'secondary' | 'warning' | 'destructive' | 'outline' } {
  if (!t.expiresAt) return { label: 'No expiry', variant: 'outline' }
  const left = Date.parse(t.expiresAt) - Date.now()
  if (left <= 0) return { label: 'Expired', variant: 'destructive' }
  const days = Math.ceil(left / 86_400_000)
  return { label: days <= 1 ? 'Expires today' : `Expires in ${days} days`, variant: days <= 7 ? 'warning' : 'secondary' }
}

export default function TokensSection() {
  const tokens = useApiTokens()
  const qc = useQueryClient()

  const revoke = async (t: APIToken) => {
    const ok = await confirm({
      title: `Revoke “${t.name}”?`,
      description: 'Scripts using it stop working immediately. This cannot be undone.',
      confirmLabel: 'Revoke',
      destructive: true,
    })
    if (!ok) return
    qc.setQueryData<APIToken[]>(queryKeys.authTokens, (old) => old?.filter((x) => x.id !== t.id))
    try {
      await deleteToken(t.id)
      toast.success(`Revoked “${t.name}”`)
    } catch (err) {
      toast.error('Could not revoke the token', { description: errorMessage(err) })
    } finally {
      void qc.invalidateQueries({ queryKey: queryKeys.authTokens })
    }
  }

  return (
    <>
      <PageHeader
        title="API tokens"
        description="Personal access tokens for the REST API (Authorization: Bearer nxt_…). They act with your permissions."
        actions={
          // With no tokens the empty state carries the one "create" action.
          !!tokens.data?.length && (
            <Button onClick={openCreateToken}>
              <Plus /> New token
            </Button>
          )
        }
      />
      <QueryState
        query={tokens}
        skeleton={<SkeletonRows rows={3} rowHeight={60} className="rounded-lg border bg-card" />}
        errorTitle="Could not load your API tokens"
        isEmpty={(d) => d.length === 0}
        empty={
          <div className="rounded-lg border border-dashed">
            <EmptyState
              icon={KeyRound}
              title="No API tokens"
              description="Create one to automate AstraTerm from scripts, CI jobs or other tools."
              action={
                <Button onClick={openCreateToken}>
                  <Plus /> Create a token
                </Button>
              }
            />
          </div>
        }
      >
        {(items) => (
          <ul className="divide-y rounded-lg border bg-card">
            {items.map((t) => {
              const exp = expiry(t)
              return (
                <li key={t.id} className="group flex items-center gap-3 px-4 py-3">
                  <KeyRound className="size-4 shrink-0 text-muted-foreground" />
                  <div className="grid min-w-0 flex-1 gap-0.5">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="truncate text-base font-medium">{t.name}</span>
                      <Tooltip content={t.expiresAt ? formatDateTime(t.expiresAt) : 'Never expires'}>
                        <Badge variant={exp.variant}>{exp.label}</Badge>
                      </Tooltip>
                    </div>
                    <div className="flex flex-wrap gap-x-3 text-sm text-muted-foreground">
                      <span>
                        Created <When value={t.createdAt} />
                      </span>
                      <span>
                        Last used <When value={t.lastUsedAt} />
                      </span>
                    </div>
                  </div>
                  <Button size="sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={() => void revoke(t)}>
                    <Trash2 /> Revoke
                  </Button>
                </li>
              )
            })}
          </ul>
        )}
      </QueryState>
    </>
  )
}
