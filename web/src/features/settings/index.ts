/*
 * Settings feature (owned by F0): singleton "settings" tab with a searchable section list, and the core sections
 * (Appearance, General, Keyboard, Security, About). Other features add sections with registerSettingsSection and store
 * their values with defineSettings('<feature>', defaults) (src/stores/settings.ts).
 */
import { lazy } from 'react'
import { Info, Keyboard, Palette, Settings, ShieldCheck, SlidersHorizontal } from 'lucide-react'
import { registerCommand, registerSettingsSection, registerTabKind } from '@/app/registry'
import { openTab } from '@/stores/workspace'

export interface SettingsTabParams {
  section?: string
}

const SettingsView = lazy(() => import('./SettingsView'))

registerTabKind<SettingsTabParams>({
  kind: 'settings',
  title: () => 'Settings',
  icon: Settings,
  singleton: true,
  component: SettingsView,
})

/** Open the Settings tab, optionally at a section id (e.g. 'keyboard'). */
function openSettings(section?: string): void {
  openTab<SettingsTabParams>({ kind: 'settings', params: section ? { section } : undefined })
}

registerCommand<{ section?: string } | undefined>({
  id: 'settings.open',
  title: 'Open Settings',
  category: 'Preferences',
  icon: Settings,
  keybinding: '$mod+Comma',
  global: true,
  run: ({ args }) => openSettings(args && typeof args.section === 'string' ? args.section : undefined),
})

registerCommand({
  id: 'settings.keyboard',
  title: 'Keyboard Shortcuts Settings',
  category: 'Preferences',
  icon: Keyboard,
  run: () => openSettings('keyboard'),
})

registerCommand({
  id: 'settings.appearance',
  title: 'Appearance Settings',
  category: 'Preferences',
  icon: Palette,
  run: () => openSettings('appearance'),
})

registerSettingsSection({
  id: 'appearance',
  title: 'Appearance',
  icon: Palette,
  order: 10,
  group: 'appearance',
  keywords: ['theme', 'dark', 'light', 'accent', 'colour', 'color', 'zoom', 'scale', 'density', 'compact', 'opacity', 'transparency', 'translucent', 'blur', 'ribbon', 'toolbar', 'sidebar', 'status bar', 'menu'],
  component: lazy(() => import('./sections/Appearance')),
})

registerSettingsSection({
  id: 'general',
  title: 'General',
  icon: SlidersHorizontal,
  order: 20,
  group: 'general',
  keywords: ['workspace', 'restore', 'layout', 'tabs', 'confirm', 'close', 'links', 'browser', 'download', 'folder', 'save'],
  component: lazy(() => import('./sections/General')),
})

registerSettingsSection({
  id: 'keyboard',
  title: 'Keyboard shortcuts',
  icon: Keyboard,
  order: 30,
  group: 'general',
  keywords: ['keybinding', 'hotkey', 'shortcut', 'keys', 'bindings'],
  component: lazy(() => import('./sections/Keyboard')),
})

registerSettingsSection({
  id: 'security',
  title: 'Security',
  icon: ShieldCheck,
  order: 40,
  group: 'security',
  keywords: ['password', 'lock', 'auto-lock', 'idle', 'vault', 'master password', 'account'],
  component: lazy(() => import('./sections/Security')),
})

registerSettingsSection({
  id: 'about',
  title: 'About',
  icon: Info,
  order: 1000,
  group: 'about',
  keywords: ['version', 'license', 'build', 'integrations'],
  component: lazy(() => import('./sections/About')),
})
