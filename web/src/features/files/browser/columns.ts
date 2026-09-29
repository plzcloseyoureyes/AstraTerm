/*
 * File list columns (columns: Name, Size, Last modified, Owner, Group, Access) managed by
 * @tanstack/react-table v9 (sizing, resizing, visibility). Narrow views (the sidebar at 240px) hide the least
 * important columns automatically: the effective visibility is the user's choice ∩ what fits the view's width.
 */
import { useEffect, useMemo, useState } from 'react'
import {
  columnResizingFeature,
  columnSizingFeature,
  columnVisibilityFeature,
  createColumnHelper,
  tableFeatures,
  useTable,
  type ColumnSizingState,
  type ColumnVisibilityState,
} from '@tanstack/react-table'
import type { FileEntry } from '@/api/types'
import { debounce } from '@/lib/utils'
import { filesSettings } from '../settings'
import type { ColumnId } from '../types'

export interface ColumnSpec {
  id: ColumnId
  label: string
  /** Default width (px); the name column is flexible and uses `minSize` as its minimum. */
  size: number
  minSize: number
  maxSize: number
  align?: 'right'
  /** Auto-hide order on narrow views: lower hides first. */
  priority: number
  /** Default sort direction when first clicked. */
  descFirst?: boolean
}

export const COLUMNS: ColumnSpec[] = [
  { id: 'name', label: 'Name', size: 200, minSize: 110, maxSize: 2000, priority: 100 },
  { id: 'size', label: 'Size', size: 78, minSize: 48, maxSize: 220, align: 'right', priority: 50, descFirst: true },
  { id: 'mtime', label: 'Last modified', size: 134, minSize: 80, maxSize: 280, priority: 40, descFirst: true },
  { id: 'owner', label: 'Owner', size: 70, minSize: 40, maxSize: 240, priority: 20 },
  { id: 'group', label: 'Group', size: 70, minSize: 40, maxSize: 240, priority: 10 },
  { id: 'perm', label: 'Access', size: 86, minSize: 64, maxSize: 180, priority: 30 },
]

export const COLUMN_BY_ID = Object.fromEntries(COLUMNS.map((c) => [c.id, c])) as Record<ColumnId, ColumnSpec>

const features = tableFeatures({ columnSizingFeature, columnResizingFeature, columnVisibilityFeature })
const helper = createColumnHelper<typeof features, FileEntry>()
const columnDefs = helper.columns(
  COLUMNS.map((c) =>
    helper.display({
      id: c.id,
      header: c.label,
      size: c.size,
      minSize: c.minSize,
      maxSize: c.maxSize,
      enableHiding: c.id !== 'name',
      enableResizing: c.id !== 'name',
    }),
  ),
)
const NO_DATA: FileEntry[] = []

/** Minimum room the name column needs before other columns are auto-hidden. */
const NAME_ROOM = 150

const saveSizes = debounce((sizes: ColumnSizingState) => filesSettings.set({ columnSizes: sizes as Partial<Record<ColumnId, number>> }), 600)

export interface ColumnModel {
  table: ReturnType<typeof useFileTable>
  /** Visible columns in order (after auto-hiding), with their widths. */
  visible: { id: ColumnId; size: number; spec: ColumnSpec }[]
  /** Columns hidden only because the view is too narrow. */
  autoHidden: ColumnId[]
  /** Minimum inner width of the rows (name room + fixed columns). */
  minWidth: number
  userVisibility: ColumnVisibilityState
  setUserVisibility: (id: ColumnId, visible: boolean) => void
  resetColumns: () => void
}

function useFileTable(sizing: ColumnSizingState, setSizing: (s: ColumnSizingState) => void, visibility: ColumnVisibilityState) {
  return useTable({
    features,
    columns: columnDefs,
    data: NO_DATA,
    columnResizeMode: 'onChange',
    state: { columnSizing: sizing, columnVisibility: visibility },
    onColumnSizingChange: (updater) => setSizing(typeof updater === 'function' ? updater(sizing) : updater),
  })
}

/** Column model for a view of `width` px (0 = unknown: show the user's columns). */
export function useFileColumns(width: number): ColumnModel {
  const settings = filesSettings.use()
  const [sizing, setSizingState] = useState<ColumnSizingState>(() => ({ ...(settings.columnSizes as ColumnSizingState) }))
  useEffect(() => {
    setSizingState({ ...(settings.columnSizes as ColumnSizingState) })
  }, [settings.columnSizes])
  const setSizing = (s: ColumnSizingState) => {
    setSizingState(s)
    saveSizes(s)
  }

  const userVisibility = useMemo<ColumnVisibilityState>(() => {
    const out: ColumnVisibilityState = {}
    for (const c of COLUMNS) if (c.id !== 'name' && settings.columns?.[c.id] === false) out[c.id] = false
    return out
  }, [settings.columns])

  // Auto-hide low-priority columns until the rest fits.
  const { effective, autoHidden } = useMemo(() => {
    const eff: ColumnVisibilityState = { ...userVisibility }
    const hidden: ColumnId[] = []
    if (width > 0) {
      const sizeOf = (c: ColumnSpec) => Math.max(c.minSize, Math.min(c.maxSize, sizing[c.id] ?? c.size))
      const shown = COLUMNS.filter((c) => c.id !== 'name' && eff[c.id] !== false)
      let need = NAME_ROOM + shown.reduce((n, c) => n + sizeOf(c), 0)
      for (const c of [...shown].sort((a, b) => a.priority - b.priority)) {
        if (need <= width) break
        eff[c.id] = false
        hidden.push(c.id)
        need -= sizeOf(c)
      }
    }
    return { effective: eff, autoHidden: hidden }
  }, [userVisibility, width, sizing])

  const table = useFileTable(sizing, setSizing, effective)
  const visible = table.getVisibleLeafColumns().map((col) => {
    const id = col.id as ColumnId
    return { id, size: col.getSize(), spec: COLUMN_BY_ID[id] }
  })
  const minWidth = visible.reduce((n, c) => n + (c.id === 'name' ? c.spec.minSize : c.size), 0)

  return {
    table,
    visible,
    autoHidden,
    minWidth,
    userVisibility,
    setUserVisibility: (id, v) => filesSettings.set((s) => ({ columns: { ...s.columns, [id]: v } })),
    resetColumns: () => {
      filesSettings.reset(['columns', 'columnSizes'])
      setSizingState({})
    },
  }
}
