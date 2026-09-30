/*
 * Dialog state of the security feature (rendered by the overlay, opened from anywhere) and the "confirm it's you"
 * re-authentication helper used before sensitive account changes.
 */
import { create } from 'zustand'
import { isApiError } from '@/api/client'
import type { APITokenCreated } from '@/api/types'

interface SecurityDialogs {
  reauth: { reason?: string; resolve: (ok: boolean) => void } | null
  recoveryCodes: { codes: string[]; title: string } | null
  totpEnroll: boolean
  createToken: boolean
  tokenCreated: APITokenCreated | null
  addPasskey: boolean
}

export const useSecurityDialogs = create<SecurityDialogs>(() => ({
  reauth: null,
  recoveryCodes: null,
  totpEnroll: false,
  createToken: false,
  tokenCreated: null,
  addPasskey: false,
}))

export const openTotpEnroll = () => useSecurityDialogs.setState({ totpEnroll: true })
export const openCreateToken = () => useSecurityDialogs.setState({ createToken: true })
export const openAddPasskey = () => useSecurityDialogs.setState({ addPasskey: true })
export const showRecoveryCodes = (codes: string[], title = 'Save your recovery codes') =>
  useSecurityDialogs.setState({ recoveryCodes: { codes, title } })

export function closeSecurityDialogs(): void {
  const r = useSecurityDialogs.getState().reauth
  useSecurityDialogs.setState({ reauth: null, recoveryCodes: null, totpEnroll: false, createToken: false, tokenCreated: null, addPasskey: false })
  r?.resolve(false)
}

/** Ask the user to confirm their identity (password or passkey). Resolves true once verified. */
function requestReauth(reason?: string): Promise<boolean> {
  return new Promise((resolve) => {
    const prev = useSecurityDialogs.getState().reauth
    prev?.resolve(false)
    useSecurityDialogs.setState({ reauth: { reason, resolve } })
  })
}

export function finishReauth(ok: boolean): void {
  const r = useSecurityDialogs.getState().reauth
  useSecurityDialogs.setState({ reauth: null })
  r?.resolve(ok)
}

function isReauthRequired(err: unknown): boolean {
  return isApiError(err) && err.code === 'reauth_required'
}

/**
 * Make sure the session counts as recently authenticated (server: 10-minute window) before a flow that cannot show
 * the dialog itself, e.g. a full-page redirect. Resolves false when the user cancels.
 */
export async function ensureRecentAuth(reason?: string): Promise<boolean> {
  try {
    const { getMe } = await import('./api')
    if ((await getMe()).reauthFresh) return true
  } catch {
    /* ask anyway */
  }
  return requestReauth(reason)
}

/** Thrown by flows whose re-authentication the user cancelled (callers treat it like a dismissed dialog). */
export class ReauthCancelled extends Error {
  constructor() {
    super('cancelled')
    this.name = 'ReauthCancelled'
  }
}

/** Like withReauth, but rejects with ReauthCancelled instead of resolving undefined when the user cancels. */
export async function withReauthOrThrow<T>(fn: () => Promise<T>, reason?: string): Promise<T> {
  let cancelled = true
  const out = await withReauth(async () => {
    const v = await fn()
    cancelled = false
    return v
  }, reason)
  if (cancelled) throw new ReauthCancelled()
  return out as T
}

/**
 * Run a sensitive action; when the server answers 403 reauth_required, ask the user to confirm their identity and
 * retry once. Resolves undefined when the user cancels.
 */
export async function withReauth<T>(fn: () => Promise<T>, reason?: string): Promise<T | undefined> {
  try {
    return await fn()
  } catch (err) {
    if (!isReauthRequired(err)) throw err
    if (!(await requestReauth(reason))) return undefined
    return await fn()
  }
}
