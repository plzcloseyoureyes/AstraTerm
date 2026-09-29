/*
 * Authentication state machine: loading → setup | login → authenticated (| error when the server is unreachable).
 * Handles the desktop-mode one-time launch token (?launch=TOKEN): it is removed from the URL immediately and exchanged
 * with POST /api/auth/launch. The server-mode setup token (?setup=TOKEN) is likewise removed and kept for the setup
 * wizard (getSetupToken).
 */
import { create } from 'zustand'
import * as authApi from '@/api/auth'
import { isApiError } from '@/api/client'
import type { AuthState, User } from '@/api/types'
import { errorMessage } from '@/lib/utils'

export type AuthStatus = 'loading' | 'setup' | 'login' | 'authenticated' | 'error'

interface AuthStore {
  status: AuthStatus
  state: AuthState | null
  user: User | null
  error?: string
  /** Message shown on the login screen (expired launch link, session expired...). */
  notice?: string
}

export const useAuthStore = create<AuthStore>(() => ({ status: 'loading', state: null, user: null }))

function statusFor(st: AuthState): AuthStatus {
  if (st.setupRequired) return 'setup'
  return st.authenticated && st.user ? 'authenticated' : 'login'
}

/** Remove ?<param>=… from the address bar (without navigation, so it stays out of history) and return it. */
function takeUrlToken(param: 'launch' | 'setup'): string | null {
  try {
    const url = new URL(window.location.href)
    const token = url.searchParams.get(param)
    if (!token) return null
    url.searchParams.delete(param)
    window.history.replaceState(window.history.state, '', url.pathname + url.search + url.hash)
    return token
  } catch {
    return null
  }
}

let bootstrapped = false
let setupToken: string | null = null

/** One-time setup token from the startup banner's ?setup= link (server mode); kept in memory for the setup wizard. */
export function getSetupToken(): string | null {
  return setupToken
}

/** First load: consume a launch token if present, then resolve the auth state. */
export async function bootstrapAuth(): Promise<void> {
  if (bootstrapped) return
  bootstrapped = true
  setupToken = takeUrlToken('setup')
  const token = takeUrlToken('launch')
  try {
    let st = await authApi.getAuthState()
    let notice: string | undefined
    if (token && !st.setupRequired && !st.authenticated) {
      try {
        await authApi.launch(token)
        st = await authApi.getAuthState()
      } catch (err) {
        notice = isApiError(err) && err.status < 500 ? 'This launch link has expired or was already used. Please sign in.' : errorMessage(err)
      }
    }
    useAuthStore.setState({ state: st, user: st.user ?? null, status: statusFor(st), error: undefined, notice })
  } catch (err) {
    useAuthStore.setState({ status: 'error', error: errorMessage(err) })
  }
}

/** Re-read /api/auth/state (after login/setup/logout, or when a request came back 401). */
export async function refreshAuth(): Promise<AuthState | null> {
  try {
    const st = await authApi.getAuthState()
    useAuthStore.setState({ state: st, user: st.user ?? null, status: statusFor(st), error: undefined })
    return st
  } catch (err) {
    const status = useAuthStore.getState().status
    if (status === 'loading' || status === 'error') useAuthStore.setState({ status: 'error', error: errorMessage(err) })
    return null
  }
}

/** Retry after the server was unreachable. */
export async function retryAuth(): Promise<void> {
  useAuthStore.setState({ status: 'loading', error: undefined })
  await refreshAuth()
}

/** A request returned 401: the session is gone — show the login screen. */
export function markUnauthenticated(notice = 'Your session has ended. Please sign in again.'): void {
  const s = useAuthStore.getState()
  if (s.status !== 'authenticated') return
  useAuthStore.setState({
    status: 'login',
    user: null,
    state: s.state ? { ...s.state, authenticated: false, user: undefined } : s.state,
    notice,
  })
}

export function setVaultLocked(locked: boolean): void {
  useAuthStore.setState((s) => (s.state ? { state: { ...s.state, vaultLocked: locked } } : {}))
}

export function setVaultHasMasterPassword(has: boolean): void {
  useAuthStore.setState((s) => (s.state ? { state: { ...s.state, vaultHasMasterPassword: has } } : {}))
}

export function setCurrentUser(user: User): void {
  useAuthStore.setState((s) => ({ user, state: s.state ? { ...s.state, user } : s.state }))
}

export function clearAuthNotice(): void {
  useAuthStore.setState({ notice: undefined })
}

/** Sign out on the server and return to the login screen. */
export async function signOut(): Promise<void> {
  try {
    await authApi.logout()
  } catch {
    /* already signed out */
  }
  const s = useAuthStore.getState()
  useAuthStore.setState({
    status: s.state?.setupRequired ? 'setup' : 'login',
    user: null,
    state: s.state ? { ...s.state, authenticated: false, user: undefined } : s.state,
    notice: undefined,
  })
}

// --- selectors -------------------------------------------------------------------------------------------------------

export const useCurrentUser = () => useAuthStore((s) => s.user)
export const useIsAdmin = () => useAuthStore((s) => s.user?.role === 'admin')
export const useAuthState = () => useAuthStore((s) => s.state)
export const useRunMode = () => useAuthStore((s) => s.state?.mode ?? 'desktop')
export const useVaultLocked = () => useAuthStore((s) => !!s.state?.vaultLocked)
export const useServerFeatures = () => useAuthStore((s) => s.state?.features)
export const useAppVersion = () => useAuthStore((s) => s.state?.version ?? '')
