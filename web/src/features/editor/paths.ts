/* Path helpers (kept out of codec.ts so the main bundle does not pull in the encoding tables). */

export function baseName(path: string | undefined): string {
  if (!path) return ''
  const trimmed = path.replace(/[\\/]+$/, '')
  const i = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'))
  return i >= 0 ? trimmed.slice(i + 1) : trimmed
}

export function dirName(path: string): string {
  const trimmed = path.replace(/\/+$/, '')
  const i = trimmed.lastIndexOf('/')
  if (i < 0) return '.'
  return i === 0 ? '/' : trimmed.slice(0, i)
}

export function joinPath(dir: string, name: string): string {
  if (!dir || dir === '.') return name
  return dir.endsWith('/') ? dir + name : `${dir}/${name}`
}

export interface Crumb {
  /** Segment label ("/" for the root). */
  name: string
  /** Path up to and including this segment. */
  path: string
}

/**
 * Breadcrumb segments of a path: "/a/b/c.txt" → "/", "a", "b", "c.txt"; "C:/x/y" → "C:", "x", "y" (a drive segment
 * lists "C:/"). Backslashes are separators too.
 */
export function crumbs(path: string): Crumb[] {
  if (!path) return []
  const norm = path.replace(/\\/g, '/')
  const abs = norm.startsWith('/')
  const parts = norm.split('/').filter(Boolean)
  const out: Crumb[] = abs ? [{ name: '/', path: '/' }] : []
  let acc = ''
  parts.forEach((p, i) => {
    acc = i === 0 ? (abs ? `/${p}` : p) : `${acc}/${p}`
    out.push({ name: p, path: /^[A-Za-z]:$/.test(acc) ? `${acc}/` : acc })
  })
  return out
}
