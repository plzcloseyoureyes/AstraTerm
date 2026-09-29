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
