/*
 * Home feature (owned by F0): the singleton "home" tab (also rendered as the empty-workspace watermark) and the
 * `app.home` command.
 */
import { lazy } from 'react'
import { House } from 'lucide-react'
import { registerCommand, registerTabKind } from '@/app/registry'
import { openTab } from '@/stores/workspace'

const HomeView = lazy(() => import('./HomeView'))

registerTabKind({
  kind: 'home',
  title: () => 'Home',
  icon: House,
  singleton: true,
  noReopen: true,
  component: HomeView,
})

registerCommand({
  id: 'app.home',
  title: 'Open Home',
  category: 'View',
  icon: House,
  keybinding: 'Alt+Home',
  run: () => {
    openTab({ kind: 'home' })
  },
})
