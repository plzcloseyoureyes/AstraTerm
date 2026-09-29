/*
 * Browser side of passkeys: thin wrappers over @simplewebauthn/browser (lazy-loaded) and the /api/auth/webauthn/*
 * ceremonies. Used by the Security tab, the login screen (passwordless + second factor) and the lock screen.
 */
import { isApiError } from '@/api/client'
import type { User } from '@/api/types'
import * as sec from './api'

type Lib = typeof import('@simplewebauthn/browser')
let lib: Promise<Lib> | null = null
const load = () => (lib ??= import('@simplewebauthn/browser'))

/** Passkeys need WebAuthn, a secure context (https or localhost) and a host name (not an IP address). */
export function passkeyEnvironment(): { ok: boolean; reason?: string } {
  if (typeof window === 'undefined' || !window.PublicKeyCredential) return { ok: false, reason: 'This browser does not support passkeys.' }
  if (!window.isSecureContext) return { ok: false, reason: 'Passkeys need a secure connection (HTTPS, or http://localhost).' }
  const host = location.hostname
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(host) || host.includes(':') || host.startsWith('[')) {
    const port = location.port ? `:${location.port}` : ''
    return { ok: false, reason: `Passkeys need a host name, not an IP address — open Termstead via ${location.protocol}//localhost${port}.` }
  }
  return { ok: true }
}

/** The user dismissed or timed out the browser dialog (not an error worth a toast). */
export function isPasskeyCancel(err: unknown): boolean {
  const e = err as { name?: string; code?: string; cause?: { name?: string } } | null
  const name = e?.name === 'WebAuthnError' ? e.cause?.name : e?.name
  return name === 'NotAllowedError' || name === 'AbortError' || e?.code === 'ERROR_CEREMONY_ABORTED'
}

/** Human message for a passkey failure. */
export function passkeyErrorMessage(err: unknown): string {
  if (isApiError(err)) return err.message
  const e = err as { name?: string; code?: string; message?: string; cause?: { name?: string } } | null
  const name = e?.name === 'WebAuthnError' ? e.cause?.name : e?.name
  if (e?.code === 'ERROR_AUTHENTICATOR_PREVIOUSLY_REGISTERED' || name === 'InvalidStateError') return 'This passkey is already registered.'
  if (name === 'NotAllowedError') return 'The passkey request was cancelled or timed out.'
  if (name === 'SecurityError') return 'The browser refused the request for this address. Use https or http://localhost.'
  return e?.message || 'The passkey operation failed.'
}

/** Create a passkey for the signed-in user (the caller handles reauth_required). */
export async function createPasskey(name: string, password?: string) {
  const { startRegistration } = await load()
  const begin = await sec.registerBegin(password)
  const credential = await startRegistration({ optionsJSON: begin.options })
  return sec.registerFinish(begin.ceremonyId, name, credential)
}

/** Passwordless sign-in with a discoverable passkey (account chooser of the browser / password manager). */
export async function signInWithPasskey(remember: boolean): Promise<User> {
  const { startAuthentication } = await load()
  const begin = await sec.loginBegin(false)
  const credential = await startAuthentication({ optionsJSON: begin.options })
  return (await sec.loginFinish(begin.ceremonyId, credential, remember)).user
}

let conditional: AbortController | null = null

/**
 * Conditional UI: offer passkeys in the username field's autofill (input needs autocomplete="username webauthn").
 * Resolves with the user once one is picked; never rejects for cancellation. Call cancelConditionalPasskey() when
 * the form goes away or another ceremony starts.
 */
export async function startConditionalPasskey(remember: () => boolean): Promise<User | null> {
  const { startAuthentication, browserSupportsWebAuthnAutofill } = await load()
  if (!(await browserSupportsWebAuthnAutofill())) return null
  cancelConditionalPasskey()
  conditional = new AbortController()
  const mine = conditional
  try {
    const begin = await sec.loginBegin(true)
    if (mine.signal.aborted) return null
    const credential = await startAuthentication({ optionsJSON: begin.options, useBrowserAutofill: true, verifyBrowserAutofillInput: false })
    return (await sec.loginFinish(begin.ceremonyId, credential, remember())).user
  } catch (err) {
    if (mine.signal.aborted || isPasskeyCancel(err)) return null
    throw err
  } finally {
    if (conditional === mine) conditional = null
  }
}

export function cancelConditionalPasskey(): void {
  if (!conditional) return
  conditional.abort()
  conditional = null
  void load().then(({ WebAuthnAbortService }) => WebAuthnAbortService.cancelCeremony())
}

/** Second factor of a password login. */
export async function passkeySecondFactor(mfaToken: string): Promise<User> {
  cancelConditionalPasskey()
  const { startAuthentication } = await load()
  const begin = await sec.mfaBegin(mfaToken)
  const credential = await startAuthentication({ optionsJSON: begin.options })
  return (await sec.mfaFinish(mfaToken, begin.ceremonyId, credential)).user
}

/** Re-verify the signed-in user with a passkey (lock screen, sensitive changes). */
export async function verifyWithPasskey(): Promise<void> {
  const { startAuthentication } = await load()
  const begin = await sec.verifyBegin()
  const credential = await startAuthentication({ optionsJSON: begin.options })
  await sec.verifyFinish(begin.ceremonyId, credential)
}
