/*
 * Quick-connect history hygiene. The ribbon's quick-connect field keeps a local history (localStorage) of what was
 * typed, and that text may carry inline credentials ("ssh user:secret@host", "xfreerdp /p:secret …"). The feature that
 * owns the quick-connect grammar (sessions) installs a sanitizer so only the password-free form is ever stored.
 */

/** Returns the text to keep in history, or null when the entry must not be stored. */
export type QuickConnectSanitizer = (text: string) => string | null

/** Until the owner registers: drop the password of every "user:password@" token (URL or ssh style). */
const fallback: QuickConnectSanitizer = (text) => text.replace(/(^|[\s/])([^\s:@/]+):[^\s@/]*@/g, '$1$2@')

let sanitizer: QuickConnectSanitizer = fallback

/** Install the history sanitizer; returns an unregister function. */
export function setQuickConnectSanitizer(fn: QuickConnectSanitizer): () => void {
  sanitizer = fn
  return () => {
    if (sanitizer === fn) sanitizer = fallback
  }
}

/** The form of `text` that is safe to keep in history, or null to store nothing. */
export function sanitizeQuickConnect(text: string): string | null {
  try {
    return sanitizer(text)
  } catch {
    return fallback(text)
  }
}

/** Text that is clearly a connection spec rather than a search: user@host, host:port, a URL, an IP address... */
export function looksLikeSpec(q: string): boolean {
  return /[@:/]|^\s*(ssh|sftp|telnet|rdp|vnc|ftp|serial|mosh)\s|^\s*\d{1,3}(\.\d{1,3}){3}\s*$/i.test(q)
}

/** Whether `q` is exactly a saved session's name, host or user@host[:port] (then it wins over connecting to `q`). */
export function isExactSession(q: string, c: { name: string; host?: string; username?: string; port?: number | null }): boolean {
  const t = q.trim().toLowerCase()
  if (c.name.toLowerCase() === t) return true
  if (!c.host) return false
  const host = c.host.toLowerCase()
  const target = c.username ? `${c.username.toLowerCase()}@${host}` : host
  return t === host || t === target || (!!c.port && t === `${target}:${c.port}`)
}
