import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource-variable/inter'
import '@fontsource-variable/jetbrains-mono'
import 'dockview-react/dist/styles/dockview.css'
import './index.css'
import { configureApiClient } from '@/api/client'
import { events } from '@/lib/events'
import { applyCachedAppearance } from '@/lib/theme'
import { markUnauthenticated, refreshAuth, useAuthStore } from '@/stores/auth'
import { requestVaultUnlock } from '@/stores/ui'
import './features'
import { App } from './App'

// Theme before first paint (cached appearance of the last session).
applyCachedAppearance()

configureApiClient({
  onUnauthorized: () => markUnauthenticated(),
  onVaultLocked: () => (useAuthStore.getState().status === 'authenticated' ? requestVaultUnlock() : Promise.resolve(false)),
})

events.configure({
  // Repeated websocket failures often mean the login session expired: re-check auth.
  onRepeatedFailure: () => void refreshAuth(),
})

const root = document.getElementById('root')
if (!root) throw new Error('#root element missing')

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
