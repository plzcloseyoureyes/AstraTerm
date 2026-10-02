import { toast } from 'sonner'
import { api } from '@/api/client'
import { signOut } from '@/stores/auth'
import { flushSettings } from '@/stores/settings'
import { lockApp } from '@/stores/ui'

/** Sign out, saving pending settings first (the auth gate then tears down per-user state). */
export async function logout(): Promise<void> {
  try {
    await flushSettings()
  } catch {
    /* best effort */
  }
  await signOut()
}

/**
 * Lock the screen (it unlocks with the account password). An account without one — the desktop app's local account,
 * or one from single sign-on — could not unlock it, so it is pointed to where a password is set instead.
 */
export async function lockScreen(opts: { quiet?: boolean } = {}): Promise<void> {
  const me = await api.get<{ hasPassword: boolean }>('/api/auth/me').catch(() => null)
  if (me && !me.hasPassword) {
    if (!opts.quiet) toast.info('Set a password to lock the screen', { description: 'Settings → Security → Password.' })
    return
  }
  lockApp()
}
