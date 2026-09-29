/*
 * Tunnel manager preferences (settings section `tunnels`, persisted server-side per user).
 */
import { defineSettings } from '@/stores/settings'

export interface TunnelsSettings {
  /** Ask before deleting a tunnel. */
  confirmDelete: boolean
  /** "Open in browser": prefer the NexTerm web proxy (works when NexTerm runs remotely) over a direct URL. */
  openWith: 'auto' | 'proxy' | 'direct'
  /** Show live session forwards below the saved tunnels. */
  showSessionForwards: boolean
  /** Watch connected SSH sessions for new listening ports and offer to forward them (TUN-9). */
  watchPorts: boolean
  /** Toast when a running tunnel fails or loses its connection. */
  notifyErrors: boolean
  /** Status bar item: running tunnel count. */
  showStatusItem: boolean
}

export const tunnelsSettings = defineSettings<TunnelsSettings>('tunnels', {
  confirmDelete: true,
  openWith: 'auto',
  showSessionForwards: true,
  watchPorts: false,
  notifyErrors: true,
  showStatusItem: true,
})
