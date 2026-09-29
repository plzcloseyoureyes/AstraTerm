/*
 * Browser toolbar: icon buttons with tooltips that fold into a "More" menu when the view is narrow (the SFTP panel
 * works down to 240px): the lowest-priority actions move first. Also the filter row and the column / view menu.
 */
import { useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { Columns3, Ellipsis, Eye, EyeOff, ListFilter, RotateCcw, X } from 'lucide-react'
import type { IconType, MenuItem } from '@/app/registry'
import { DynamicDropdown } from '@/components/menu-items'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { IconButton } from '@/components/ui/icon-button'
import { Tooltip } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { filesSettings } from '../settings'
import type { ColumnId } from '../types'
import { COLUMNS, type ColumnModel } from './columns'

export interface ToolAction {
  id: string
  label: string
  icon: IconType
  run: () => void
  shortcut?: string
  disabled?: boolean
  /** Toggle state. */
  active?: boolean
  /** A slow operation the user started from this button is running: its icon spins (delayed, calm). */
  busy?: boolean
  /** Higher stays visible longer. */
  priority: number
  /** Buttons of one group are separated from the next group. */
  group: number
  /** Show a text label next to the icon when there is room (tab toolbars). */
  text?: string
  className?: string
}

const BTN = 26
const SEP = 9
const MORE = 28

/** Icon toolbar that keeps the most important actions visible and folds the rest into "More". */
export function ResponsiveToolbar({ actions, label, trailing, className }: { actions: ToolAction[]; label: string; trailing?: ReactNode; className?: string }) {
  const ref = useRef<HTMLDivElement>(null)
  const trailRef = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const measure = () => setWidth(el.clientWidth - (trailRef.current?.offsetWidth ?? 0))
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    if (trailRef.current) ro.observe(trailRef.current)
    measure()
    return () => ro.disconnect()
  }, [])

  // Choose the visible set by priority, then render it in natural order.
  let visible = new Set(actions.map((a) => a.id))
  if (width > 0) {
    const needed = (set: Set<string>) => {
      const shown = actions.filter((a) => set.has(a.id))
      const groups = new Set(shown.map((a) => a.group)).size
      return shown.length * BTN + Math.max(0, groups - 1) * SEP + (shown.length < actions.length ? MORE : 0)
    }
    const byPriority = [...actions].sort((a, b) => a.priority - b.priority)
    for (const a of byPriority) {
      if (needed(visible) <= width - 8) break
      visible = new Set(visible)
      visible.delete(a.id)
    }
  }
  const shown = actions.filter((a) => visible.has(a.id))
  const hidden = actions.filter((a) => !visible.has(a.id))

  return (
    <div ref={ref} role="toolbar" aria-label={label} className={cn('flex h-8 min-w-0 shrink-0 items-center gap-0.5 overflow-hidden', className)}>
      {shown.map((a, i) => (
        <span key={a.id} className="flex items-center">
          {i > 0 && shown[i - 1].group !== a.group && <span className="mx-1 h-4 w-px bg-border" aria-hidden />}
          <IconButton
            icon={a.icon}
            label={a.label}
            shortcut={a.shortcut}
            size="xs"
            disabled={a.disabled}
            active={a.active}
            busy={a.busy}
            onClick={a.run}
            className={a.className}
          />
        </span>
      ))}
      {hidden.length > 0 && (
        <DynamicDropdown
          align="start"
          items={() =>
            hidden.map<MenuItem>((a) => ({
              label: a.label,
              icon: a.icon,
              run: a.run,
              disabled: a.disabled,
              shortcut: a.shortcut,
              ...(a.active !== undefined ? { checked: a.active } : {}),
            }))
          }
        >
          <Button variant="ghost" size="icon-xs" aria-label="More actions" className="text-muted-foreground hover:text-foreground">
            <Ellipsis />
          </Button>
        </DynamicDropdown>
      )}
      <div className="flex-1" />
      <div ref={trailRef} className="flex shrink-0 items-center gap-0.5">
        {trailing}
      </div>
    </div>
  )
}

/** Filter box + hidden files toggle + columns menu. */
export function FilterRow({
  value,
  onChange,
  columns,
  className,
  inputRef,
  onEnterList,
}: {
  value: string
  onChange: (v: string) => void
  columns: ColumnModel
  className?: string
  inputRef?: React.Ref<HTMLInputElement>
  onEnterList?: () => void
}) {
  const showHidden = filesSettings.useValue('showHidden')
  return (
    <div className={cn('flex h-7 min-w-0 shrink-0 items-center gap-0.5', className)}>
      <div className="relative flex h-6 min-w-0 flex-1 items-center">
        <ListFilter className="pointer-events-none absolute left-1.5 size-3.5 text-muted-foreground" aria-hidden />
        <input
          ref={inputRef}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape' && value) {
              e.preventDefault()
              e.stopPropagation()
              onChange('')
            } else if (e.key === 'ArrowDown' || e.key === 'Enter') {
              e.preventDefault()
              onEnterList?.()
            }
          }}
          placeholder="Filter (e.g. *.log)"
          aria-label="Filter files"
          spellCheck={false}
          className={cn(
            'h-full w-full min-w-0 rounded-md border border-transparent bg-muted/50 pr-6 pl-6 text-sm outline-none placeholder:text-muted-foreground/70',
            'focus-visible:border-ring focus-visible:bg-background focus-visible:ring-2 focus-visible:ring-ring/20',
            value && 'border-primary/40 bg-primary/8',
          )}
        />
        {value && (
          <button
            type="button"
            aria-label="Clear filter"
            onClick={() => onChange('')}
            className="absolute right-1 rounded-sm p-0.5 text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <X className="size-3" />
          </button>
        )}
      </div>
      <IconButton
        icon={showHidden ? Eye : EyeOff}
        label={showHidden ? 'Hide hidden files' : 'Show hidden files'}
        size="xs"
        active={showHidden}
        onClick={() => filesSettings.set({ showHidden: !showHidden })}
      />
      <ColumnsMenu columns={columns} />
    </div>
  )
}

function ColumnsMenu({ columns }: { columns: ColumnModel }) {
  const foldersFirst = filesSettings.useValue('foldersFirst')
  const sortBy = filesSettings.useValue('sortBy')
  const sortDesc = filesSettings.useValue('sortDesc')
  return (
    <DropdownMenu>
      <Tooltip content="Columns & sorting">
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon-xs" aria-label="Columns and sorting" className="text-muted-foreground hover:text-foreground">
            <Columns3 />
          </Button>
        </DropdownMenuTrigger>
      </Tooltip>
      <DropdownMenuContent align="end" className="min-w-48">
        <DropdownMenuLabel>Columns</DropdownMenuLabel>
        {COLUMNS.filter((c) => c.id !== 'name').map((c) => {
          const on = columns.userVisibility[c.id] !== false
          const auto = columns.autoHidden.includes(c.id as ColumnId)
          return (
            <DropdownMenuCheckboxItem
              key={c.id}
              checked={on}
              onSelect={(e) => e.preventDefault()}
              onCheckedChange={(v) => columns.setUserVisibility(c.id, v === true)}
            >
              <span className="flex-1">{c.label}</span>
              {on && auto && <span className="ml-3 text-2xs text-muted-foreground">too narrow</span>}
            </DropdownMenuCheckboxItem>
          )
        })}
        <DropdownMenuSeparator />
        {/* Sorting from the keyboard (the column headers are mouse targets). */}
        <DropdownMenuLabel>Sort by</DropdownMenuLabel>
        <DropdownMenuRadioGroup
          value={sortBy}
          onValueChange={(v) => {
            const spec = COLUMNS.find((c) => c.id === v)
            if (spec) filesSettings.set({ sortBy: spec.id, sortDesc: !!spec.descFirst })
          }}
        >
          {COLUMNS.map((c) => (
            <DropdownMenuRadioItem key={c.id} value={c.id} onSelect={(e) => e.preventDefault()}>
              {c.label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
        <DropdownMenuCheckboxItem checked={sortDesc} onSelect={(e) => e.preventDefault()} onCheckedChange={(v) => filesSettings.set({ sortDesc: v === true })}>
          Descending
        </DropdownMenuCheckboxItem>
        <DropdownMenuCheckboxItem checked={foldersFirst} onSelect={(e) => e.preventDefault()} onCheckedChange={(v) => filesSettings.set({ foldersFirst: v === true })}>
          Folders first
        </DropdownMenuCheckboxItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => columns.resetColumns()}>
          <RotateCcw /> Reset columns
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
