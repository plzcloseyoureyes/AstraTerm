/*
 * The singleton "admin" tab (administrators only): Users · Authentication · Audit log · Network policy · System.
 * The active section lives in the tab params (admin.* commands set it).
 */
import { useEffect } from 'react'
import { Globe, KeyRound, ScrollText, Server, ShieldAlert, Users } from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import type { TabProps } from '@/app/registry'
import { EmptyState } from '@/components/ui/empty-state'
import { cn } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { updateTabParams, useIsTabVisible } from '@/stores/workspace'
import { adminUsers, getGuacd, getOidc, getPasskeyConfig, getPolicy, getSystem, secQK, useNetworkPolicy } from '@/features/security/api'
import { SectionLayout, type SectionNavItem } from '@/features/security/components'
import { AuditPanel } from './AuditPanel'
import { AuthPanel } from './AuthPanel'
import { NetworkPanel } from './NetworkPanel'
import { SystemPanel } from './SystemPanel'
import { UsersPanel } from './UsersPanel'

export type AdminSection = 'users' | 'authentication' | 'audit' | 'network' | 'system'
export interface AdminTabParams {
  section?: AdminSection
}
const ADMIN_SECTIONS: AdminSection[] = ['users', 'authentication', 'audit', 'network', 'system']

const NAV: SectionNavItem<AdminSection>[] = [
  { id: 'users', label: 'Users', icon: Users },
  { id: 'authentication', label: 'Authentication', icon: KeyRound },
  { id: 'audit', label: 'Audit log', icon: ScrollText },
  { id: 'network', label: 'Network policy', icon: Globe },
  { id: 'system', label: 'System', icon: Server },
]

/**
 * Switching sections is instant (docs/UX.md: prefer prefetch over indicators): while the tab is open, every section's
 * data loads in the background.
 */
function usePrefetchSections(enabled: boolean) {
  const qc = useQueryClient()
  useEffect(() => {
    if (!enabled) return
    void qc.prefetchQuery({ queryKey: secQK.users, queryFn: adminUsers })
    void qc.prefetchQuery({ queryKey: secQK.policy, queryFn: getPolicy })
    void qc.prefetchQuery({ queryKey: secQK.oidc, queryFn: getOidc })
    void qc.prefetchQuery({ queryKey: secQK.passkeyConfig, queryFn: getPasskeyConfig })
    void qc.prefetchQuery({ queryKey: secQK.system, queryFn: getSystem })
    void qc.prefetchQuery({ queryKey: secQK.guacd, queryFn: getGuacd, retry: false })
  }, [enabled, qc])
}

export default function AdminView({ tabId, params }: TabProps<AdminTabParams>) {
  const isAdmin = useIsAdmin()
  const visible = useIsTabVisible(tabId)
  const section: AdminSection = params?.section && ADMIN_SECTIONS.includes(params.section) ? params.section : 'users'
  // The network policy API belongs to another module: its section goes away only when that module is missing (404),
  // not while the first request runs (the list would grow under the pointer).
  const net = useNetworkPolicy(isAdmin)
  usePrefetchSections(isAdmin)
  const hasNetwork = !(net.isError && (net.error as { status?: number }).status === 404)
  if (!isAdmin) {
    return <EmptyState icon={ShieldAlert} title="Administrators only" description="Ask an administrator for access." className="h-full" />
  }
  const nav = NAV.filter((n) => n.id !== 'network' || hasNetwork || section === 'network')
  // One page width for every section (the heading never moves when switching); forms keep a readable measure.
  const form = section === 'authentication' || section === 'network'
  return (
    <SectionLayout
      label="Administration"
      items={nav}
      active={section}
      onSelect={(id) => updateTabParams<AdminTabParams>(tabId, { section: id })}
      className="max-w-5xl"
    >
      <div className={cn(form && 'max-w-3xl')}>
        {section === 'users' && <UsersPanel />}
        {section === 'authentication' && <AuthPanel />}
        {section === 'audit' && <AuditPanel visible={visible} />}
        {section === 'network' && <NetworkPanel />}
        {section === 'system' && <SystemPanel visible={visible} />}
      </div>
    </SectionLayout>
  )
}
