/*
 * Keys feature (MobaKeyGen + MobAgent; RESEARCH TOOL-1, SSH-5, SSH-11, SSH-12, SSH-19, SSH-20, SM-7 identities UI):
 * the singleton "keys" tab (SSH keys · Identities · Known hosts · Agent), the generator / import / export / install /
 * certificate / sign / passphrase / identity / known-host dialogs (overlay), commands, the "Keys" ribbon menu, a
 * context-menu entry for SSH sessions, the agent status bar item and the "SSH keys & agent" settings section.
 *
 * Commands (category "SSH keys"):
 *   keys.open {tab?}                 open the Keys tab (tab: keys | identities | knownHosts | agent)
 *   keys.generate · keys.import · keys.convert
 *   keys.identities · keys.identity.new {name?, username?, keyId?}
 *   keys.knownHosts · keys.knownHosts.import
 *   keys.agent · keys.agent.start · keys.agent.stop · keys.agent.toggle
 *   keys.install {keyId?, connectionId?}   install a public key on a server (ssh-copy-id)
 */
import { lazy } from 'react'
import { ArrowRightLeft, FileUp, KeyRound, Play, Server, ShieldCheck, Sparkles, Square, TerminalSquare, Upload, UserRound, UserRoundPlus } from 'lucide-react'
import { queryClient } from '@/api/queryClient'
import type { IdentityInput } from '@/api/types'
import {
  registerCommand,
  registerContextMenu,
  registerOverlay,
  registerRibbonButton,
  registerSettingsSection,
  registerStatusItem,
  registerTabKind,
  ribbonButtons,
  type MenuItem,
  type RibbonButtonDef,
} from '@/app/registry'
import { useAuthStore } from '@/stores/auth'
import { openKeysTab, startAgentAction, stopAgentAction } from './actions'
import { agentStatus as fetchAgentStatus, keysQK } from './api'
import { KeysOverlay } from './Overlay'
import { AgentStatusItem } from './StatusItem'
import { closeAllKeysDialogs, openKeysDialog } from './store'
import type { AgentStatus, KeysTab, KeysTabParams } from './types'

const KeysView = lazy(() => import('./KeysView'))

registerTabKind<KeysTabParams>({
  kind: 'keys',
  title: () => 'SSH keys',
  icon: KeyRound,
  singleton: true,
  component: KeysView,
})

registerOverlay({ id: 'keys', component: KeysOverlay })
registerStatusItem({ id: 'keys.agent', align: 'right', order: 70, component: AgentStatusItem })

registerSettingsSection({
  id: 'keys',
  title: 'SSH keys & agent',
  icon: KeyRound,
  order: 45,
  group: 'security',
  keywords: ['ssh key', 'keygen', 'putty', 'ppk', 'passphrase', 'agent', 'mobagent', 'ssh-agent', 'forwarding', 'known hosts', 'certificate'],
  component: lazy(() => import('./SettingsSection')),
})

// --- commands ----------------------------------------------------------------------------------------------------------

const CATEGORY = 'SSH keys'
const TABS: KeysTab[] = ['keys', 'identities', 'knownHosts', 'agent']

function cachedAgentStatus(): AgentStatus | undefined {
  return queryClient.getQueryData<AgentStatus>(keysQK.agentStatus)
}

function isDesktop(): boolean {
  return (useAuthStore.getState().state?.mode ?? 'desktop') === 'desktop'
}

registerCommand<{ tab?: KeysTab } | undefined>({
  id: 'keys.open',
  title: 'Open SSH Keys',
  category: CATEGORY,
  icon: KeyRound,
  keywords: ['keygen', 'mobakeygen', 'ssh key', 'identities', 'known hosts', 'agent'],
  run: ({ args }) => openKeysTab(args?.tab && TABS.includes(args.tab) ? args.tab : undefined),
})

registerCommand({
  id: 'keys.generate',
  title: 'Generate SSH Key…',
  category: CATEGORY,
  icon: Sparkles,
  keywords: ['keygen', 'new key', 'ed25519', 'rsa', 'ecdsa', 'puttygen'],
  run: () => openKeysDialog('generate', {}),
})

registerCommand<{ text?: string } | undefined>({
  id: 'keys.import',
  title: 'Import SSH Key…',
  category: CATEGORY,
  icon: Upload,
  keywords: ['ppk', 'pem', 'openssh', 'private key'],
  run: ({ args }) => openKeysDialog('importer', { mode: 'import', text: typeof args?.text === 'string' ? args.text : undefined }),
})

registerCommand({
  id: 'keys.convert',
  title: 'Convert SSH Key File…',
  category: CATEGORY,
  icon: ArrowRightLeft,
  keywords: ['ppk to openssh', 'openssh to ppk', 'pem', 'putty'],
  run: () => openKeysDialog('importer', { mode: 'convert' }),
})

registerCommand({
  id: 'keys.identities',
  title: 'Manage Identities',
  category: CATEGORY,
  icon: UserRound,
  keywords: ['credentials', 'username', 'password'],
  run: () => openKeysTab('identities'),
})

registerCommand<Partial<IdentityInput> | undefined>({
  id: 'keys.identity.new',
  title: 'New Identity…',
  category: CATEGORY,
  icon: UserRoundPlus,
  run: ({ args }) => openKeysDialog('identity', { initial: args ?? undefined }),
})

registerCommand({
  id: 'keys.knownHosts',
  title: 'Manage Known Hosts',
  category: CATEGORY,
  icon: ShieldCheck,
  keywords: ['host keys', 'fingerprint', 'known_hosts', 'certificate authority'],
  run: () => openKeysTab('knownHosts'),
})

registerCommand({
  id: 'keys.knownHosts.import',
  title: 'Import Known Hosts…',
  category: CATEGORY,
  icon: FileUp,
  run: () => {
    openKeysTab('knownHosts')
    openKeysDialog('knownHostsImport', {})
  },
})

registerCommand({
  id: 'keys.agent',
  title: 'Open SSH Agent',
  category: CATEGORY,
  icon: TerminalSquare,
  keywords: ['mobagent', 'ssh-agent', 'SSH_AUTH_SOCK'],
  run: () => openKeysTab('agent'),
})

registerCommand({
  id: 'keys.agent.start',
  title: 'Start SSH Agent',
  category: CATEGORY,
  icon: Play,
  when: isDesktop,
  run: () => startAgentAction(),
})

registerCommand({
  id: 'keys.agent.stop',
  title: 'Stop SSH Agent',
  category: CATEGORY,
  icon: Square,
  when: isDesktop,
  run: () => stopAgentAction(),
})

registerCommand({
  id: 'keys.agent.toggle',
  title: 'Start / Stop SSH Agent',
  category: CATEGORY,
  icon: TerminalSquare,
  hidden: true,
  when: isDesktop,
  run: async () => {
    const st = await queryClient.fetchQuery({ queryKey: keysQK.agentStatus, queryFn: fetchAgentStatus, staleTime: 2_000 })
    return st.running && st.owner ? stopAgentAction() : startAgentAction()
  },
})

registerCommand<{ keyId?: string; connectionId?: string } | undefined>({
  id: 'keys.install',
  title: 'Install SSH Key on a Server…',
  category: CATEGORY,
  icon: Server,
  keywords: ['ssh-copy-id', 'authorized_keys', 'deploy key'],
  run: ({ args }) => openKeysDialog('install', { keyId: args?.keyId, connectionId: args?.connectionId }),
})

// --- ribbon, menus -----------------------------------------------------------------------------------------------------

function ribbonMenu(): MenuItem[] {
  const st = cachedAgentStatus()
  const items: MenuItem[] = [
    { label: 'Generate key…', icon: Sparkles, command: 'keys.generate' },
    { label: 'Import key…', icon: Upload, command: 'keys.import' },
    { label: 'Convert key file…', icon: ArrowRightLeft, command: 'keys.convert' },
    { label: 'Install key on a server…', icon: Server, command: 'keys.install' },
    { type: 'separator' },
    { label: 'SSH keys', icon: KeyRound, command: 'keys.open' },
    { label: 'Identities', icon: UserRound, command: 'keys.identities' },
    { label: 'Known hosts', icon: ShieldCheck, command: 'keys.knownHosts' },
    { label: 'SSH agent', icon: TerminalSquare, command: 'keys.agent' },
  ]
  if (isDesktop()) {
    items.push(
      { type: 'separator' },
      st?.running && st.owner
        ? { label: 'Stop SSH agent', icon: Square, command: 'keys.agent.stop' }
        : { label: 'Start SSH agent', icon: Play, command: 'keys.agent.start', disabled: !!st?.running && !st.owner },
    )
  }
  return items
}

// The shell registers a plain "Keys" ribbon button (command keys.open) under the same id when it loads after this
// module; keep ours, which adds the drop-down menu.
const keysRibbon: RibbonButtonDef = { id: 'keys', label: 'Keys', icon: KeyRound, order: 90, command: 'keys.open', tooltip: 'SSH keys, identities, known hosts, agent', menu: ribbonMenu }
function ensureKeysRibbon(): void {
  if (ribbonButtons.get('keys') !== keysRibbon) registerRibbonButton(keysRibbon)
}
ribbonButtons.subscribe(ensureKeysRibbon)
ensureKeysRibbon()

const SSH_PROTOCOLS = new Set(['ssh', 'sftp', 'mosh'])

registerContextMenu({
  id: 'keys.session',
  target: 'session-node',
  order: 70,
  items: (ctx) => {
    const c = ctx.connection
    if (!c || !SSH_PROTOCOLS.has(c.protocol) || (ctx.selection && ctx.selection.length > 1)) return []
    return [{ label: 'Install SSH key…', icon: Server, command: 'keys.install', args: { connectionId: c.id } }]
  },
})

// Dialogs holding typed secrets are dropped on sign-out.
useAuthStore.subscribe((s, prev) => {
  if (prev.status === 'authenticated' && s.status !== 'authenticated') closeAllKeysDialogs()
})
