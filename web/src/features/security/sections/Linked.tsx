import { Link2, Plus, Unlink } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { secQK, unlinkIdentity, useIdentities } from '../api'
import { PageHeader, When } from '../components'
import { ensureRecentAuth } from '../store'
import type { LoginMethods } from '../types'

/**
 * Navigate to the identity provider to attach an account (the server redirects back with ?sso_linked=). Linking adds
 * a way to sign in, so the server wants a recent sign-in: confirm first, before leaving the page.
 */
export async function linkProvider(id: string): Promise<void> {
  if (!(await ensureRecentAuth('Linking an account adds a way to sign in to Termstead.'))) return
  window.location.assign(`/api/auth/oidc/link?provider=${encodeURIComponent(id)}`)
}

export default function LinkedSection() {
  const ids = useIdentities()
  const qc = useQueryClient()
  const providers = useAuthStore((s) => (s.state as { loginMethods?: LoginMethods } | null)?.loginMethods?.sso ?? [])

  const unlink = async (id: string, name: string) => {
    const ok = await confirm({ title: `Unlink ${name}?`, description: 'You will no longer be able to sign in with it.', confirmLabel: 'Unlink', destructive: true })
    if (!ok) return
    try {
      await unlinkIdentity(id)
      toast.success(`Unlinked ${name}`)
    } catch (err) {
      toast.error('Could not unlink', { description: errorMessage(err) })
    } finally {
      void qc.invalidateQueries({ queryKey: secQK.identities })
    }
  }

  const connect =
    providers.length === 0 ? null : providers.length === 1 ? (
      <Button onClick={() => void linkProvider(providers[0].id)}>
        <Plus /> Link {providers[0].name}
      </Button>
    ) : (
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button>
            <Plus /> Link an account
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          {providers.map((p) => (
            <DropdownMenuItem key={p.id} onSelect={() => void linkProvider(p.id)}>
              {p.name}
            </DropdownMenuItem>
          ))}
        </DropdownMenuContent>
      </DropdownMenu>
    )

  return (
    <>
      <PageHeader title="Linked accounts" description="Single sign-on identities that can sign in to this account." actions={connect} />
      <QueryState
        query={ids}
        skeleton={<SkeletonRows rows={2} rowHeight={60} className="rounded-lg border bg-card" />}
        errorTitle="Could not load your linked accounts"
        isEmpty={(d) => d.length === 0}
        empty={
          <div className="rounded-lg border border-dashed">
            <EmptyState
              icon={Link2}
              title="No linked accounts"
              description={providers.length ? 'Link your organisation account to sign in with one click.' : 'Your administrator has not configured single sign-on.'}
              action={connect}
            />
          </div>
        }
      >
        {(items) => (
          <ul className="divide-y rounded-lg border bg-card">
            {items.map((i) => (
              <li key={i.id} className="flex items-center gap-3 px-4 py-3">
                <Link2 className="size-4 shrink-0 text-muted-foreground" />
                <div className="grid min-w-0 flex-1 gap-0.5">
                  <span className="text-base font-medium">{i.providerName}</span>
                  <div className="flex flex-wrap gap-x-3 text-sm text-muted-foreground">
                    <span className="truncate">{i.email || i.username || i.subject}</span>
                    <span>
                      Last used <When value={i.lastLoginAt} />
                    </span>
                  </div>
                </div>
                <Button size="sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={() => void unlink(i.id, i.providerName)}>
                  <Unlink /> Unlink
                </Button>
              </li>
            ))}
          </ul>
        )}
      </QueryState>
    </>
  )
}
