/*
 * Security feature (RESEARCH MU-1 account UI, MU-5 UI, MU-6, MU-15 UI, SEC-4, SEC-5 passkey unlock, UI-19): the
 * singleton "security" tab (account, two-factor, passkeys, API tokens, signed-in devices, linked SSO accounts), the
 * "active-sessions" tab (runtime sessions manager), security dialogs (overlay) and the lock-screen passkey unlock.
 *
 * Commands (category "Security" — linked from Settings → Security):
 *   security.open {section?}   account & security tab (section: account | twoFactor | passkeys | tokens | devices | linked)
 *   security.twoFactor · security.passkeys · security.tokens · security.devices · security.linked
 *   security.addPasskey · security.newToken · security.sessions (active sessions manager, UI-19)
 *   security.unlockWithPasskey (hidden; the lock screen overlay uses it)
 */
import { lazy } from 'react'
import { Fingerprint, KeyRound, Link2, MonitorPlay, MonitorSmartphone, Plus, ShieldCheck, UserRound } from 'lucide-react'
import { registerCommand, registerMenu, registerOverlay, registerTabKind } from '@/app/registry'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { openTab } from '@/stores/workspace'
import { LockPasskeyOverlay, SecurityOverlay, unlockWithPasskey } from './Overlay'
import type { SecuritySection, SecurityTabParams } from './SecurityView'
import { closeSecurityDialogs, openAddPasskey, openCreateToken } from './store'

const SecurityView = lazy(() => import('./SecurityView'))
const ActiveSessionsView = lazy(() => import('./ActiveSessionsView'))

registerTabKind<SecurityTabParams>({
  kind: 'security',
  title: () => 'Account & security',
  icon: ShieldCheck,
  singleton: true,
  component: SecurityView,
})

registerTabKind({
  kind: 'active-sessions',
  title: () => 'Active sessions',
  icon: MonitorPlay,
  singleton: true,
  component: ActiveSessionsView,
})

registerOverlay({ id: 'security', component: SecurityOverlay })
registerOverlay({ id: 'security.lock', component: LockPasskeyOverlay, keepMountedWhileLocked: true, order: 1000 })

export function openSecurity(section?: SecuritySection): void {
  openTab<SecurityTabParams>({ kind: 'security', params: { section: section ?? 'account' } })
}

const CATEGORY = 'Security'
const SECTIONS: SecuritySection[] = ['account', 'twoFactor', 'passkeys', 'tokens', 'devices', 'linked']

registerCommand<{ section?: SecuritySection } | undefined>({
  id: 'security.open',
  title: 'Account & Security',
  category: CATEGORY,
  icon: UserRound,
  keywords: ['account', 'profile', 'password', 'security', '2fa', 'mfa'],
  run: ({ args }) => openSecurity(args?.section && SECTIONS.includes(args.section) ? args.section : undefined),
})
registerCommand({
  id: 'security.twoFactor',
  title: 'Two-Factor Authentication',
  category: CATEGORY,
  icon: ShieldCheck,
  keywords: ['totp', 'authenticator', 'otp', 'mfa', '2fa', 'recovery codes'],
  run: () => openSecurity('twoFactor'),
})
registerCommand({
  id: 'security.passkeys',
  title: 'Passkeys',
  category: CATEGORY,
  icon: Fingerprint,
  keywords: ['webauthn', 'fido', 'security key', 'yubikey', 'touch id', 'windows hello'],
  run: () => openSecurity('passkeys'),
})
registerCommand({
  id: 'security.addPasskey',
  title: 'Add a Passkey…',
  category: CATEGORY,
  icon: Plus,
  hidden: true,
  run: () => {
    openSecurity('passkeys')
    openAddPasskey()
  },
})
registerCommand({
  id: 'security.tokens',
  title: 'API Tokens',
  category: CATEGORY,
  icon: KeyRound,
  keywords: ['personal access token', 'rest', 'bearer', 'automation'],
  run: () => openSecurity('tokens'),
})
registerCommand({
  id: 'security.newToken',
  title: 'New API Token…',
  category: CATEGORY,
  icon: Plus,
  hidden: true,
  run: () => {
    openSecurity('tokens')
    openCreateToken()
  },
})
registerCommand({
  id: 'security.devices',
  title: 'Signed-in Devices',
  category: CATEGORY,
  icon: MonitorSmartphone,
  keywords: ['login sessions', 'browsers', 'sign out', 'revoke'],
  run: () => openSecurity('devices'),
})
registerCommand({
  id: 'security.linked',
  title: 'Linked Single Sign-On Accounts',
  category: CATEGORY,
  icon: Link2,
  hidden: true,
  run: () => openSecurity('linked'),
})
registerCommand({
  id: 'security.sessions',
  title: 'Active Sessions',
  category: CATEGORY,
  icon: MonitorPlay,
  keywords: ['running', 'detached', 'terminals', 'session manager', 'close all'],
  run: () => {
    openTab({ kind: 'active-sessions' })
  },
})
registerCommand({
  id: 'security.unlockWithPasskey',
  title: 'Unlock with Passkey',
  category: CATEGORY,
  icon: Fingerprint,
  hidden: true,
  when: () => useUIStore.getState().locked,
  run: () => unlockWithPasskey(),
})

registerMenu({
  id: 'security.menu',
  menu: 'settings',
  order: 60,
  items: () => [
    { label: 'Account & security…', icon: ShieldCheck, command: 'security.open' },
    { label: 'Active sessions', icon: MonitorPlay, command: 'security.sessions' },
  ],
})

// Dialogs holding typed secrets / tokens are dropped on sign-out.
useAuthStore.subscribe((s, prev) => {
  if (prev.status === 'authenticated' && s.status !== 'authenticated') closeSecurityDialogs()
})

// ?sso_linked=<provider> after linking an identity from the Security tab.
if (typeof window !== 'undefined') {
  try {
    const url = new URL(window.location.href)
    const linked = url.searchParams.get('sso_linked')
    if (linked) {
      url.searchParams.delete('sso_linked')
      window.history.replaceState(window.history.state, '', url.pathname + url.search + url.hash)
      const stop = useAuthStore.subscribe((s) => {
        if (s.status !== 'authenticated') return
        stop()
        void import('sonner').then(({ toast }) => toast.success(`Linked your ${linked} account`, { description: 'You can now sign in with it.' }))
        openSecurity('linked')
      })
    }
    // A failed link attempt comes back as ?sso_error= while signed in (the login screen handles it otherwise).
    const failed = url.searchParams.get('sso_error')
    if (failed) {
      const stop = useAuthStore.subscribe((s) => {
        if (s.status === 'login' || s.status === 'setup') stop()
        if (s.status !== 'authenticated') return
        stop()
        const u = new URL(window.location.href)
        u.searchParams.delete('sso_error')
        window.history.replaceState(window.history.state, '', u.pathname + u.search + u.hash)
        void import('./ssoErrors').then(({ ssoErrorMessage }) =>
          import('sonner').then(({ toast }) => toast.error('Single sign-on failed', { description: ssoErrorMessage(failed) })),
        )
      })
    }
  } catch {
    /* ignore */
  }
}
