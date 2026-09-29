/*
 * Always-mounted overlay (registerOverlay) rendering the servers dialogs from the UI store: the configuration dialog
 * and the activity drawer (lazy chunks).
 */
import { lazy, Suspense } from 'react'
import { closeServerConfig, closeServerDrawer, useServersUI } from './store'

const ConfigDialog = lazy(() => import('./ConfigDialog'))
const ActivityDrawer = lazy(() => import('./ActivityDrawer'))

export function ServersOverlay({ locked }: { locked: boolean }) {
  const { config, drawer } = useServersUI()
  if (locked) return null
  return (
    <Suspense fallback={null}>
      {config && <ConfigDialog key={config.key} kind={config.kind} initialTab={config.tab} onClose={closeServerConfig} />}
      {drawer && (
        <ActivityDrawer key={drawer.kind} kind={drawer.kind} initialTab={drawer.tab} openKey={drawer.key} onClose={closeServerDrawer} />
      )}
    </Suspense>
  )
}
