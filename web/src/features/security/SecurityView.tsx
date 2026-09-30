/*
 * The singleton "security" tab: Account · Two-factor · Passkeys · API tokens · Signed-in devices · Linked accounts.
 * The active section lives in the tab params (security.* commands set it).
 */
import { lazy, useEffect } from 'react'
import { Fingerprint, KeyRound, Link2, MonitorSmartphone, ShieldCheck, UserRound } from 'lucide-react'
import { useApiTokens, useAuthSessions } from '@/api/auth'
import type { TabProps } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { LazyBoundary } from '@/components/ui/spinner'
import { StatusDot } from '@/components/ui/status-dot'
import { useAuthStore } from '@/stores/auth'
import { updateTabParams } from '@/stores/workspace'
import { useIdentities, useMe, usePasskeys, usePasskeyStatus } from './api'
import { SectionLayout, type SectionNavItem } from './components'

export type SecuritySection = 'account' | 'twoFactor' | 'passkeys' | 'tokens' | 'devices' | 'linked'
export interface SecurityTabParams {
  section?: SecuritySection
}

const loaders = {
  account: () => import('./sections/Account'),
  twoFactor: () => import('./sections/TwoFactor'),
  passkeys: () => import('./sections/Passkeys'),
  tokens: () => import('./sections/Tokens'),
  devices: () => import('./sections/Devices'),
  linked: () => import('./sections/Linked'),
} satisfies Record<SecuritySection, () => Promise<unknown>>
const AccountSection = lazy(loaders.account)
const TwoFactorSection = lazy(loaders.twoFactor)
const PasskeysSection = lazy(loaders.passkeys)
const TokensSection = lazy(loaders.tokens)
const DevicesSection = lazy(loaders.devices)
const LinkedSection = lazy(loaders.linked)

/**
 * Switching sections is instant (docs/UX.md: prefer prefetch over indicators): while the tab is open, every section's
 * code and data load in the background.
 */
function usePrefetchSections(sso: boolean) {
  useEffect(() => {
    for (const load of Object.values(loaders)) void load().catch(() => undefined)
  }, [])
  usePasskeys()
  usePasskeyStatus()
  useApiTokens()
  useAuthSessions()
  useIdentities(sso)
}

const SECTIONS: SectionNavItem<SecuritySection>[] = [
  { id: 'account', label: 'Account', icon: UserRound },
  { id: 'twoFactor', label: 'Two-factor', icon: ShieldCheck },
  { id: 'passkeys', label: 'Passkeys', icon: Fingerprint },
  { id: 'tokens', label: 'API tokens', icon: KeyRound },
  { id: 'devices', label: 'Signed-in devices', icon: MonitorSmartphone },
  { id: 'linked', label: 'Linked accounts', icon: Link2 },
]

const SECURITY_SECTIONS = SECTIONS.map((s) => s.id)

export default function SecurityView({ tabId, params }: TabProps<SecurityTabParams>) {
  const section: SecuritySection = params?.section && SECURITY_SECTIONS.includes(params.section) ? params.section : 'account'
  const me = useMe()
  const sso = useAuthStore((s) => ((s.state as { loginMethods?: { sso?: unknown[] } } | null)?.loginMethods?.sso?.length ?? 0) > 0)
  usePrefetchSections(sso)
  const visible = SECTIONS.filter((s) => s.id !== 'linked' || sso || section === 'linked')
  const badge = (id: SecuritySection) => {
    const d = me.data
    if (!d) return null
    if (id === 'twoFactor') return d.user.totpEnabled ? <StatusDot tone="success" label="on" /> : d.mfaRequired ? <Badge variant="warning">Required</Badge> : null
    if (id === 'passkeys' && d.passkeys > 0)
      return (
        <Badge variant="secondary" className="px-1 tabular-nums">
          {d.passkeys}
        </Badge>
      )
    return null
  }
  return (
    <SectionLayout
      label="Account & security"
      items={visible.map((s) => ({ ...s, badge: badge(s.id) }))}
      active={section}
      onSelect={(id) => updateTabParams<SecurityTabParams>(tabId, { section: id })}
    >
      <LazyBoundary className="bg-background">
        {section === 'account' && <AccountSection />}
        {section === 'twoFactor' && <TwoFactorSection />}
        {section === 'passkeys' && <PasskeysSection />}
        {section === 'tokens' && <TokensSection />}
        {section === 'devices' && <DevicesSection />}
        {section === 'linked' && <LinkedSection />}
      </LazyBoundary>
    </SectionLayout>
  )
}
