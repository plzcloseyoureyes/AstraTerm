/*
 * Admin feature (RESEARCH MU-1 / MU-2 admin UI, MU-7 + SEC-20 settings, REC-4 audit viewer, system info): the
 * singleton "admin" tab — Users · Authentication (login policy, single sign-on, passkey domain) · Audit log ·
 * Network policy (netguard module's API, when present) · System.
 *
 * Commands (category "Administration", administrators only):
 *   admin.open {section?}   (section: users | authentication | audit | network | system)
 *   admin.users · admin.authentication · admin.audit · admin.networkPolicy · admin.system · admin.newUser
 * (admin.sessions — live session monitoring — belongs to the recording module.)
 */
import { lazy } from 'react'
import { Globe, KeyRound, ScrollText, Server, ShieldEllipsis, UserPlus, Users } from 'lucide-react'
import { registerCommand, registerMenu, registerTabKind } from '@/app/registry'
import { useAuthStore } from '@/stores/auth'
import { openTab } from '@/stores/workspace'
import type { AdminSection, AdminTabParams } from './AdminView'

const AdminView = lazy(() => import('./AdminView'))

registerTabKind<AdminTabParams>({
  kind: 'admin',
  title: () => 'Administration',
  icon: ShieldEllipsis,
  singleton: true,
  component: AdminView,
})

const isAdmin = () => useAuthStore.getState().user?.role === 'admin'
const SECTIONS: AdminSection[] = ['users', 'authentication', 'audit', 'network', 'system']

export function openAdmin(section?: AdminSection): void {
  openTab<AdminTabParams>({ kind: 'admin', params: { section: section ?? 'users' } })
}

const CATEGORY = 'Administration'

registerCommand<{ section?: AdminSection } | undefined>({
  id: 'admin.open',
  title: 'Administration',
  category: CATEGORY,
  icon: ShieldEllipsis,
  keywords: ['admin', 'users', 'audit', 'sso', 'policy'],
  when: isAdmin,
  run: ({ args }) => openAdmin(args?.section && SECTIONS.includes(args.section) ? args.section : undefined),
})
registerCommand({ id: 'admin.users', title: 'Manage Users', category: CATEGORY, icon: Users, keywords: ['accounts', 'roles'], when: isAdmin, run: () => openAdmin('users') })
registerCommand({
  id: 'admin.newUser',
  title: 'New User…',
  category: CATEGORY,
  icon: UserPlus,
  hidden: true,
  when: isAdmin,
  run: () => openAdmin('users'),
})
registerCommand({
  id: 'admin.authentication',
  title: 'Authentication Settings',
  category: CATEGORY,
  icon: KeyRound,
  keywords: ['login policy', 'password policy', 'lockout', 'oidc', 'sso', 'single sign-on', 'mfa', 'passkeys'],
  when: isAdmin,
  run: () => openAdmin('authentication'),
})
registerCommand({ id: 'admin.audit', title: 'Audit Log', category: CATEGORY, icon: ScrollText, keywords: ['activity', 'log', 'history', 'csv'], when: isAdmin, run: () => openAdmin('audit') })
registerCommand({
  id: 'admin.networkPolicy',
  title: 'Network Policy',
  category: CATEGORY,
  icon: Globe,
  keywords: ['ssrf', 'destinations', 'allow', 'deny', 'firewall'],
  when: isAdmin,
  run: () => openAdmin('network'),
})
registerCommand({ id: 'admin.system', title: 'NexTerm Server Status', category: CATEGORY, icon: Server, keywords: ['system', 'version', 'data dir', 'tls', 'guacd', 'uptime'], when: isAdmin, run: () => openAdmin('system') })

registerMenu({
  id: 'admin.menu',
  menu: 'settings',
  order: 70,
  items: () =>
    isAdmin()
      ? [
          { label: 'Administration…', icon: ShieldEllipsis, command: 'admin.open' },
          { label: 'Audit log', icon: ScrollText, command: 'admin.audit' },
        ]
      : [],
})
