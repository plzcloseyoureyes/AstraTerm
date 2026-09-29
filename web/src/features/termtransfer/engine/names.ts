/*
 * File names coming from the remote side (ZMODEM offers, trzsz names) are untrusted: every path component is
 * sanitised before it touches the local disk (File System Access API) or a download, and ".." / absolute paths can
 * never escape the chosen folder. No DOM, no app imports (unit-tested under Node).
 */

const WINDOWS_RESERVED = /^(con|prn|aux|nul|com[0-9¹²³]|lpt[0-9¹²³])(\..*)?$/i
const MAX_COMPONENT = 200

/** One safe file-name component (never empty, never "." / "..", no separators or control characters). */
export function sanitizeFileName(name: string, fallback = 'file'): string {
  let s = String(name ?? '')
    .normalize('NFC')
    // oxlint-disable-next-line no-control-regex -- strip C0/C1 control characters from remote names
    .replace(/[\u0000-\u001f\u007f-\u009f]/g, '')
    .replace(/[\\/:*?"<>|]/g, '_')
    .replace(/[​-‏‪-‮⁦-⁩﻿]/g, '') // zero-width / bidi overrides
    .trim()
  s = s.replace(/[. ]+$/g, '') // Windows drops trailing dots and spaces
  if (!s || s === '.' || s === '..' || /^\.+$/.test(s)) s = fallback
  if (WINDOWS_RESERVED.test(s)) s = `_${s}`
  if (s.length > MAX_COMPONENT) {
    const dot = s.lastIndexOf('.')
    const ext = dot > 0 && s.length - dot <= 16 ? s.slice(dot) : ''
    s = s.slice(0, MAX_COMPONENT - ext.length) + ext
  }
  return s
}

/**
 * A relative path from the remote side as safe components: separators "/" and "\\", empty / "." components dropped,
 * ".." components dropped (they could only climb out of the destination), each component sanitised.
 */
export function splitSafePath(path: string, fallback = 'file'): string[] {
  const parts = String(path ?? '')
    .split(/[\\/]+/)
    .filter((p) => p !== '' && p !== '.' && p !== '..')
    .map((p) => sanitizeFileName(p, fallback))
  return parts.length ? parts : [sanitizeFileName('', fallback)]
}

/** "name.ext" → "name (1).ext", "name (2).ext"… until `taken` says no. Folders (no extension split) with `dir`. */
export function uniqueName(name: string, taken: (candidate: string) => boolean, dir = false): string {
  if (!taken(name)) return name
  const dot = dir ? -1 : name.lastIndexOf('.')
  const stem = dot > 0 ? name.slice(0, dot) : name
  const ext = dot > 0 ? name.slice(dot) : ''
  for (let i = 1; i < 10_000; i++) {
    const c = `${stem} (${i})${ext}`
    if (!taken(c)) return c
  }
  return `${stem} (${Date.now()})${ext}`
}

// oxlint-disable-next-line no-control-regex -- control characters must never be typed raw into a shell
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/

/**
 * POSIX shell quoting for typing a path at a prompt: plain when safe, else single-quoted. Names with control
 * characters (a newline would submit the line) use ANSI-C quoting ($'…', bash / zsh / ksh) with every such byte
 * escaped, so nothing is ever executed by the typing itself.
 */
export function quoteShellPath(path: string): string {
  if (/^[\w@%+=:,./~-]+$/.test(path) && !path.startsWith('~')) return path
  if (CONTROL.test(path)) {
    const body = Array.from(path, (ch) => {
      const c = ch.codePointAt(0)!
      if (c < 0x20 || (c >= 0x7f && c <= 0x9f)) return `\\x${c.toString(16).padStart(2, '0')}`
      if (ch === '\\' || ch === "'") return `\\${ch}`
      return ch
    }).join('')
    return `$'${body}'`
  }
  return `'${path.replace(/'/g, `'\\''`)}'`
}

/**
 * Quoting for Windows shells (cmd / PowerShell): double quotes when needed; single quotes (PowerShell's literal
 * string) when the name contains `$` or a backtick, which PowerShell would expand inside double quotes. Control
 * characters are dropped (Windows names cannot contain them).
 */
export function quoteWindowsPath(path: string): string {
  const p = path.replace(new RegExp(CONTROL.source, 'g'), '')
  if (/[$`]/.test(p)) return `'${p.replace(/'/g, "''")}'`
  return /[\s&()^;,'{}[\]@#%!+=]/.test(p) ? `"${p.replace(/"/g, '')}"` : p
}

/** Join a remote (POSIX-style) directory and a relative path. */
export function joinRemotePath(dir: string, rel: string): string {
  const base = dir.endsWith('/') ? dir.slice(0, -1) : dir
  const tail = rel.replace(/^\/+/, '')
  if (!tail) return base || '/'
  return `${base}/${tail}`
}

/** Display form of a remote folder: "~" for the home folder, "~/x" below it. */
export function displayRemotePath(path: string, home?: string): string {
  if (home && home !== '/' && (path === home || path.startsWith(`${home}/`))) return `~${path.slice(home.length)}`
  return path
}
