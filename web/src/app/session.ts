import { flushSettings } from '@/stores/settings'
import { signOut } from '@/stores/auth'

/** Sign out, saving pending settings first (the auth gate then tears down per-user state). */
export async function logout(): Promise<void> {
  try {
    await flushSettings()
  } catch {
    /* best effort */
  }
  await signOut()
}
