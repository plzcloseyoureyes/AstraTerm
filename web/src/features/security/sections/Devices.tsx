import { LogOut, MonitorSmartphone } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { useAuthSessions, revokeAuthSession } from '@/api/auth'
import { queryKeys } from '@/api/queryKeys'
import type { AuthSessionInfo } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tooltip } from '@/components/ui/tooltip'
import { errorMessage, formatDateTime } from '@/lib/utils'
import { revokeOtherSessions } from '../api'
import { DeviceIcon, PageHeader, When, describeUserAgent } from '../components'

export default function DevicesSection() {
  const sessions = useAuthSessions()
  const qc = useQueryClient()
  const others = sessions.data?.filter((s) => !s.current).length ?? 0

  const revoke = async (s: AuthSessionInfo) => {
    qc.setQueryData<AuthSessionInfo[]>(queryKeys.authSessions, (old) => old?.filter((x) => x.id !== s.id))
    try {
      await revokeAuthSession(s.id)
      toast.success('Signed out that device')
    } catch (err) {
      toast.error('Could not sign it out', { description: errorMessage(err) })
    } finally {
      void qc.invalidateQueries({ queryKey: queryKeys.authSessions })
    }
  }

  const revokeAll = async () => {
    const ok = await confirm({
      title: `Sign out ${others} other ${others === 1 ? 'device' : 'devices'}?`,
      description: 'They will need to sign in again. Running terminal sessions keep running.',
      confirmLabel: 'Sign out others',
      destructive: true,
    })
    if (!ok) return
    try {
      const r = await revokeOtherSessions()
      toast.success(r.revoked ? `Signed out ${r.revoked} other ${r.revoked === 1 ? 'device' : 'devices'}` : 'No other devices were signed in')
    } catch (err) {
      toast.error('Could not sign out the other devices', { description: errorMessage(err) })
    } finally {
      void qc.invalidateQueries({ queryKey: queryKeys.authSessions })
    }
  }

  return (
    <>
      <PageHeader
        title="Signed-in devices"
        description="Browsers currently signed in to your account. Sign out anything you don’t recognise and change your password."
        actions={
          others > 0 && (
            <Button variant="secondary" onClick={() => void revokeAll()}>
              <LogOut /> Sign out all others
            </Button>
          )
        }
      />
      <QueryState
        query={sessions}
        skeleton={<SkeletonRows rows={3} rowHeight={60} className="rounded-lg border bg-card" />}
        errorTitle="Could not load your signed-in devices"
        isEmpty={(d) => d.length === 0}
        empty={<EmptyState icon={MonitorSmartphone} title="No browser sessions" description="You are signed in with an API token." />}
      >
        {(items) => (
          <ul className="divide-y rounded-lg border bg-card">
            {items.map((s) => {
              const d = describeUserAgent(s.userAgent)
              return (
                <li key={s.id} className="flex items-center gap-3 px-4 py-3">
                  <div className="flex size-9 shrink-0 items-center justify-center rounded-lg border bg-muted/40">
                    <DeviceIcon kind={d.kind} className="text-muted-foreground" />
                  </div>
                  <div className="grid min-w-0 flex-1 gap-0.5">
                    <div className="flex flex-wrap items-center gap-2">
                      <Tooltip content={s.userAgent || 'Unknown client'}>
                        <span className="truncate text-base font-medium">
                          {d.browser}
                          {d.os && <span className="font-normal text-muted-foreground"> on {d.os}</span>}
                        </span>
                      </Tooltip>
                      {s.current && <Badge variant="success">This device</Badge>}
                      {s.remember && (
                        <Tooltip content={`Stays signed in until ${formatDateTime(s.expiresAt)} (renewed while used)`}>
                          <Badge variant="outline">Remembered</Badge>
                        </Tooltip>
                      )}
                    </div>
                    <div className="flex flex-wrap gap-x-3 text-sm text-muted-foreground">
                      <span className="font-mono text-xs leading-5">{s.ip || 'unknown address'}</span>
                      <span>
                        Active <When value={s.lastSeenAt} />
                      </span>
                      <span>
                        Signed in <When value={s.createdAt} />
                      </span>
                    </div>
                  </div>
                  {!s.current && (
                    <Button size="sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={() => void revoke(s)}>
                      <LogOut /> Sign out
                    </Button>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </QueryState>
    </>
  )
}
