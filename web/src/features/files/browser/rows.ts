/*
 * The rows a file view shows, in two steps so big folders stay fast (FILE-1: 100k-entry folders):
 *   sortEntries   sort once per listing / sort setting (keys computed once per entry, not per comparison)
 *   visibleRows   hidden-file, partial-upload and text filters over the sorted list (typing in the filter never
 *                 re-sorts), plus the ".." row on top
 * Pure functions (no DOM / React): unit-tested by __tests__/rows.test.mjs.
 */
import type { FileEntry } from '@/api/types'
import { entryPerm, isDirLike, isPartialUpload } from '../format'
import { dirname, isRoot } from '../paths'
import type { ColumnId, ListRow } from '../types'

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

export interface SortOptions {
  sortBy: ColumnId
  sortDesc: boolean
  foldersFirst: boolean
}

export interface FilterOptions {
  showHidden: boolean
  /** Show "<name>.nexterm-part" files (settings.files.showPartialUploads). */
  showPartial: boolean
  /** Filter box text: a substring, or globs ("*.log", "a?.txt"; several separated by spaces / commas). */
  filter: string
}

function globToRegExp(glob: string): RegExp {
  const esc = glob.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.')
  return new RegExp(`^${esc}$`, 'i')
}

/** Name predicate of the filter box (null: no filter). */
export function makeFilter(text: string): ((e: Pick<FileEntry, 'name'>) => boolean) | null {
  const t = text.trim()
  if (!t) return null
  if (/[*?]/.test(t)) {
    const parts = t.split(/[\s,;]+/).filter(Boolean).map(globToRegExp)
    return (e) => parts.some((re) => re.test(e.name))
  }
  const q = t.toLowerCase()
  return (e) => e.name.toLowerCase().includes(q)
}

function mtimeOf(e: FileEntry): number {
  const t = Date.parse(e.mtime)
  return Number.isFinite(t) ? t : 0
}

/** Comparable key of an entry for a column (computed once per entry). */
function keyOf(e: FileEntry, by: ColumnId): string | number {
  switch (by) {
    case 'size':
      return e.size || 0
    case 'mtime':
      return mtimeOf(e)
    case 'owner':
      return String(e.owner ?? e.uid ?? '')
    case 'group':
      return String(e.group ?? e.gid ?? '')
    case 'perm':
      return entryPerm(e)
    default:
      return e.name
  }
}

/** Entries in display order: folders first (optional), then the sort column, then the name. Stable for equal keys. */
export function sortEntries(entries: readonly FileEntry[], opts: SortOptions): FileEntry[] {
  const dir = opts.sortDesc ? -1 : 1
  const decorated = entries.map((e) => ({ e, dir: opts.foldersFirst && isDirLike(e) ? 0 : 1, key: keyOf(e, opts.sortBy) }))
  const byName = opts.sortBy === 'name'
  const numeric = opts.sortBy === 'size' || opts.sortBy === 'mtime'
  decorated.sort((a, b) => {
    if (a.dir !== b.dir) return a.dir - b.dir
    let r: number
    if (numeric) r = (a.key as number) - (b.key as number)
    else if (opts.sortBy === 'perm') r = (a.key as string) < (b.key as string) ? -1 : (a.key as string) > (b.key as string) ? 1 : 0
    else r = collator.compare(a.key as string, b.key as string)
    if (r !== 0) return r * dir
    return byName ? 0 : collator.compare(a.e.name, b.e.name)
  })
  return decorated.map((d) => d.e)
}

/** The ".." row of a folder (null at the root / without a folder). */
export function parentRow(dir: string | null): ListRow | null {
  if (!dir || isRoot(dir)) return null
  return { parent: true, entry: { name: '..', path: dirname(dir), type: 'dir', size: 0, mode: 0o40755, perm: '', mtime: '', hidden: false } }
}

/** Rows shown for a sorted listing: filters applied, ".." first (not while a text filter is active). */
export function visibleRows(sorted: readonly FileEntry[], dir: string | null, opts: FilterOptions): ListRow[] {
  const f = makeFilter(opts.filter)
  const rows: ListRow[] = []
  const up = f ? null : parentRow(dir)
  if (up) rows.push(up)
  for (const entry of sorted) {
    if (!opts.showHidden && entry.hidden) continue
    if (!opts.showPartial && isPartialUpload(entry)) continue
    if (f && !f(entry)) continue
    rows.push({ entry })
  }
  return rows
}

/** What the filters hide in a listing (explains an "empty" view). */
export function hiddenCounts(entries: readonly FileEntry[], opts: Pick<FilterOptions, 'showHidden' | 'showPartial'>): { hidden: number; partial: number } {
  let hidden = 0
  let partial = 0
  for (const e of entries) {
    if (!opts.showPartial && isPartialUpload(e)) partial++
    else if (!opts.showHidden && e.hidden) hidden++
  }
  return { hidden, partial }
}
