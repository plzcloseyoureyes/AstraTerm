/*
 * Remote path helpers. Remote file systems use POSIX paths; the local driver of a Windows host may present drive
 * paths ("C:/Users/..."), so a leading drive letter counts as a root too. Backslashes are normalised to slashes.
 */

const DRIVE = /^[A-Za-z]:\//

/** The root of a path: "/" or "C:/". */
export function rootOf(p: string): string {
  const n = p.replace(/\\/g, '/')
  if (DRIVE.test(n)) return n.slice(0, 3)
  return '/'
}

export function isAbsolute(p: string): boolean {
  const n = p.replace(/\\/g, '/')
  return n.startsWith('/') || DRIVE.test(n)
}

/** Collapse "//", resolve "." and "..", drop a trailing slash (except for the root). */
export function normalizePath(p: string): string {
  if (!p) return '/'
  let n = p.replace(/\\/g, '/')
  if (/^[A-Za-z]:$/.test(n)) n += '/'
  const root = isAbsolute(n) ? rootOf(n) : ''
  const rest = root ? n.slice(root.length) : n
  const out: string[] = []
  for (const part of rest.split('/')) {
    if (!part || part === '.') continue
    if (part === '..') {
      if (out.length && out[out.length - 1] !== '..') out.pop()
      else if (!root) out.push('..')
      continue
    }
    out.push(part)
  }
  const joined = out.join('/')
  if (root) return root + joined
  return joined || '.'
}

export function joinPath(dir: string, name: string): string {
  if (!name) return normalizePath(dir)
  if (isAbsolute(name)) return normalizePath(name)
  const d = dir.replace(/\\/g, '/')
  return normalizePath(d.endsWith('/') ? d + name : `${d}/${name}`)
}

export function dirname(p: string): string {
  const n = normalizePath(p)
  const root = isAbsolute(n) ? rootOf(n) : ''
  if (n === root) return root
  const i = n.lastIndexOf('/')
  if (i < 0) return '.'
  if (i < root.length) return root
  return n.slice(0, i) || root || '/'
}

export function basename(p: string): string {
  const n = normalizePath(p)
  if (n === rootOf(n)) return n
  const i = n.lastIndexOf('/')
  return i < 0 ? n : n.slice(i + 1)
}

export function isRoot(p: string): boolean {
  const n = normalizePath(p)
  return isAbsolute(n) && n === rootOf(n)
}

/** Is `child` inside (or equal to) `parent`? */
export function isInside(child: string, parent: string): boolean {
  const c = normalizePath(child)
  const p = normalizePath(parent)
  if (c === p) return true
  const prefix = p.endsWith('/') ? p : `${p}/`
  return c.startsWith(prefix)
}

/**
 * Resolve what the user typed in the location bar against the current folder: absolute paths, "~" / "~/x" (home),
 * and relative paths ("../etc", "sub/dir").
 */
export function resolveInput(input: string, cwd: string, home: string): string {
  const t = input.trim()
  if (!t) return cwd
  if (t === '~') return normalizePath(home)
  if (t.startsWith('~/')) return joinPath(home, t.slice(2))
  if (isAbsolute(t)) return normalizePath(t)
  return joinPath(cwd, t)
}

export interface Crumb {
  name: string
  path: string
}

/** Breadcrumb segments of an absolute path: [{"/", "/"}, {"home", "/home"}, ...]. */
export function breadcrumbs(p: string): Crumb[] {
  const n = normalizePath(p)
  const root = isAbsolute(n) ? rootOf(n) : ''
  const out: Crumb[] = []
  if (root) out.push({ name: root === '/' ? '/' : root.slice(0, 2), path: root })
  let cur = root
  for (const part of n.slice(root.length).split('/')) {
    if (!part) continue
    cur = cur ? (cur.endsWith('/') ? cur + part : `${cur}/${part}`) : part
    out.push({ name: part, path: cur })
  }
  return out
}

/** Lower-case extension without the dot ("tar.gz" style double extensions for archives). */
export function extname(name: string): string {
  const lower = name.toLowerCase()
  for (const double of ['.tar.gz', '.tar.bz2', '.tar.xz', '.tar.zst']) if (lower.endsWith(double)) return double.slice(1)
  const i = lower.lastIndexOf('.')
  return i > 0 ? lower.slice(i + 1) : ''
}

/** Split a file name into stem and extension (".bashrc" has no extension; "a.tar.gz" → "a" + ".tar.gz"). */
export function splitName(name: string): { stem: string; ext: string } {
  const ext = extname(name)
  if (!ext) return { stem: name, ext: '' }
  return { stem: name.slice(0, name.length - ext.length - 1), ext: `.${name.slice(name.length - ext.length)}` }
}

/** A name that does not collide with `taken`: "report (1).pdf", "report (2).pdf", ... */
export function uniqueName(name: string, taken: ReadonlySet<string>): string {
  if (!taken.has(name)) return name
  const { stem, ext } = splitName(name)
  for (let i = 1; i < 10_000; i++) {
    const candidate = `${stem} (${i})${ext}`
    if (!taken.has(candidate)) return candidate
  }
  return `${stem} (${Date.now()})${ext}`
}

/** Quote for POSIX shells: 'it'\''s'. */
export function shellQuote(p: string): string {
  return `'${p.replace(/'/g, `'\\''`)}'`
}

/** A file name the user typed: non-empty, no slash, not "." or "..". Returns an error message or null. */
export function validateName(name: string): string | null {
  const n = name.trim()
  if (!n) return 'Enter a name'
  if (n === '.' || n === '..') return 'This name is reserved'
  if (n.includes('/')) return 'A name cannot contain “/”'
  if (n.includes('\0')) return 'Invalid character'
  if (n.length > 255) return 'The name is too long'
  return null
}

/** Encode a path for a file:// or scp:// URL (keeps the slashes). */
export function encodePathForUrl(p: string): string {
  return p
    .split('/')
    .map((s) => encodeURIComponent(s))
    .join('/')
}
