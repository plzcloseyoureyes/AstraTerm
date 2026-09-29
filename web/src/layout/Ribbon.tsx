import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { ChevronDown, ChevronsDownUp, ChevronsUpDown, Ellipsis } from 'lucide-react'
import { commands, ribbonButtons, type MenuItem, type RibbonButtonDef } from '@/app/registry'
import { runCommand, useCommand } from '@/app/commands'
import { DynamicDropdown } from '@/components/menu-items'
import { Tooltip } from '@/components/ui/tooltip'
import { useIsMobile } from '@/lib/hooks'
import { cn } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { QuickConnect } from './QuickConnect'

/**
 * Toolbar groups, left to right (separated by a thin rule). Buttons choose one with `group`; the shell groups the
 * known ids; anything else joins 'tools'. Unknown group names form their own group after 'tools'.
 */
const GROUP_ORDER = ['connect', 'workspace', 'network', 'tools', 'app']
const KNOWN_GROUP: Record<string, string> = {
  session: 'connect',
  sessions: 'connect',
  view: 'workspace',
  split: 'workspace',
  multiexec: 'workspace',
  tunneling: 'network',
  browser: 'network',
  servers: 'network',
  tools: 'tools',
  keys: 'tools',
  automation: 'tools',
  settings: 'app',
  help: 'app',
}

function groupOf(b: RibbonButtonDef): string {
  return b.group ?? KNOWN_GROUP[b.id] ?? 'tools'
}

function groupRank(g: string): number {
  const i = GROUP_ORDER.indexOf(g)
  return i >= 0 ? i : GROUP_ORDER.indexOf('tools') + 0.5
}

/** The buttons in toolbar order (group, then their own order). */
function arrange(buttons: readonly RibbonButtonDef[]): RibbonButtonDef[] {
  return [...buttons].sort((a, b) => groupRank(groupOf(a)) - groupRank(groupOf(b)) || a.order - b.order)
}

const base = (compact: boolean) =>
  cn(
    'group relative flex shrink-0 items-center justify-center rounded-md text-foreground/80 outline-none transition-colors duration-150',
    'hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 disabled:opacity-40 disabled:hover:bg-transparent',
    'data-[state=open]:bg-accent data-[state=open]:text-foreground',
    compact ? 'size-7' : 'h-[3.25rem] min-w-[3.5rem] flex-col gap-1 px-1.5',
  )

function ButtonBody({ def, compact, menu }: { def: RibbonButtonDef; compact: boolean; menu?: boolean }) {
  const Icon = def.icon
  return (
    <>
      <Icon className={compact ? 'size-4' : 'size-5'} strokeWidth={compact ? 2 : 1.6} />
      {!compact && (
        <span className="flex max-w-20 items-center gap-0.5 text-2xs leading-tight font-medium">
          <span className="truncate">{def.label}</span>
          {menu && <ChevronDown className="size-2.5 shrink-0 text-muted-foreground" aria-hidden />}
        </span>
      )}
      {compact && menu && <ChevronDown className="absolute right-0 bottom-0 size-2.5 text-muted-foreground" aria-hidden />}
    </>
  )
}

function RibbonButton({ def, compact }: { def: RibbonButtonDef; compact: boolean }) {
  commands.useList() // re-render when commands (un)register
  const cmd = useCommand(def.command)
  const hasMenu = !!def.menu
  const hasCommand = !!def.command
  const disabled = hasCommand ? !cmd.enabled && !hasMenu : !hasMenu
  const tooltip = def.tooltip ?? cmd.command?.title ?? def.label
  const shortcut = cmd.keybindings[0]

  // Menu-only button (or its command is unavailable): the whole button opens the menu, marked by a chevron next to
  // the label.
  if (hasMenu && (!hasCommand || !cmd.enabled)) {
    return (
      <DynamicDropdown items={def.menu!} source="ribbon">
        <button type="button" className={base(compact)} aria-label={def.label} title={compact ? tooltip : undefined}>
          <ButtonBody def={def} compact={compact} menu />
        </button>
      </DynamicDropdown>
    )
  }

  const button = (
    <button
      type="button"
      className={base(compact)}
      disabled={disabled}
      aria-label={def.label}
      onClick={() => def.command && void runCommand(def.command, def.args, { source: 'ribbon' })}
    >
      <ButtonBody def={def} compact={compact} />
    </button>
  )

  if (hasMenu) {
    // Split button: the main action, and a slim arrow for the menu aligned with the label row.
    return (
      <div className="flex items-stretch">
        <Tooltip content={tooltip} shortcut={shortcut}>
          {button}
        </Tooltip>
        <DynamicDropdown items={def.menu!} source="ribbon">
          <button
            type="button"
            aria-label={`${def.label} menu`}
            className={cn(
              'flex w-3.5 justify-center rounded-sm text-muted-foreground outline-none transition-colors duration-150',
              'hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 data-[state=open]:bg-accent',
              compact ? 'items-center' : 'items-end pb-2',
            )}
          >
            <ChevronDown className="size-3" />
          </button>
        </DynamicDropdown>
      </div>
    )
  }

  return (
    <Tooltip content={disabled ? `${tooltip} (not available)` : tooltip} shortcut={disabled ? undefined : shortcut}>
      {/* span wrapper keeps the tooltip working on disabled buttons */}
      <span className="inline-flex">{button}</span>
    </Tooltip>
  )
}

/** The menu entry standing for a button that did not fit (in the overflow menu). */
function overflowItem(def: RibbonButtonDef): MenuItem {
  if (def.menu) {
    const items = def.menu
    return {
      type: 'submenu',
      label: def.label,
      icon: def.icon,
      items: () => (def.command ? [{ label: def.tooltip ?? def.label, icon: def.icon, command: def.command, args: def.args }, { type: 'separator' }, ...items()] : items()),
    }
  }
  return { label: def.label, icon: def.icon, command: def.command, args: def.args }
}

const SEPARATOR_W = 9 // mx-1 + 1 px rule
const MORE_W = { compact: 28, large: 56 }

/**
 * Which buttons fit: every button keeps its natural width (measured while shown; a new set of buttons or a mode change
 * shows them all for one layout pass, before paint); the ones that do not fit move, from the end, into a "More" menu —
 * nothing is ever cut off or scrolled out of sight.
 */
function useOverflow(items: { id: string; group: string }[], compact: boolean) {
  const rowRef = useRef<HTMLDivElement>(null)
  const widths = useRef(new Map<string, number>())
  const key = `${items.map((i) => i.id).join(',')}|${compact}`
  const [state, setState] = useState({ key: '', visible: items.length })
  const [, setTick] = useState(0)
  const measuring = state.key !== key
  const visible = measuring ? items.length : state.visible

  useLayoutEffect(() => {
    const row = rowRef.current
    if (!row) return
    for (const el of row.querySelectorAll<HTMLElement>('[data-ribbon-id]')) {
      // The separator in front of a button is not part of its own width.
      const btn = el.lastElementChild as HTMLElement | null
      widths.current.set(el.dataset.ribbonId!, btn?.offsetWidth ?? el.offsetWidth)
    }
    const avail = row.clientWidth
    const more = compact ? MORE_W.compact : MORE_W.large
    let used = 0
    let n = 0
    for (const [i, it] of items.entries()) {
      const w = (widths.current.get(it.id) ?? 0) + 2 + (i > 0 && items[i - 1].group !== it.group ? SEPARATOR_W : 0)
      const last = i === items.length - 1
      if (used + w + (last ? 0 : more) > avail) break
      used += w
      n++
    }
    if (measuring || n !== state.visible) setState({ key, visible: n })
  })

  useLayoutEffect(() => {
    const row = rowRef.current
    if (!row) return
    const ro = new ResizeObserver(() => setTick((t) => t + 1))
    ro.observe(row)
    return () => ro.disconnect()
  }, [])

  return { rowRef, visible }
}

/** The toolbar buttons, grouped, with an overflow menu instead of hidden buttons. Fills the space it is given. */
function ToolbarButtons({ compact }: { compact: boolean }) {
  const registered = ribbonButtons.useList()
  const buttons = useMemo(() => arrange(registered), [registered])
  const items = useMemo(() => buttons.map((b) => ({ id: b.id, group: groupOf(b) })), [buttons])
  const { rowRef, visible } = useOverflow(items, compact)
  const shown = buttons.slice(0, visible)
  const hidden = buttons.slice(visible)

  return (
    <div ref={rowRef} role="toolbar" aria-label="Main toolbar" className="flex min-w-0 flex-1 items-center gap-0.5 overflow-hidden">
      {shown.map((b, i) => {
        const g = groupOf(b)
        const sep = i > 0 && groupOf(shown[i - 1]) !== g
        return (
          <div key={b.id} className="flex shrink-0 items-center" data-ribbon-id={b.id} data-group={g}>
            {sep && <span className={cn('mx-1 w-px bg-border', compact ? 'h-4' : 'h-9')} aria-hidden />}
            <RibbonButton def={b} compact={compact} />
          </div>
        )
      })}
      {hidden.length > 0 && (
        <DynamicDropdown items={() => hidden.map(overflowItem)} source="ribbon">
          <button type="button" className={base(compact)} aria-label={`More (${hidden.length})`} title={compact ? 'More' : undefined}>
            <Ellipsis className={compact ? 'size-4' : 'size-5'} strokeWidth={compact ? 2 : 1.6} />
            {!compact && <span className="text-2xs leading-tight font-medium">More</span>}
          </button>
        </DynamicDropdown>
      )}
    </div>
  )
}

/** Switches the toolbar row between large buttons and icons only. */
function ToolbarSizeToggle() {
  const compact = appearanceSettings.useValue('ribbonCompact')
  const label = compact ? 'Large toolbar' : 'Compact toolbar'
  return (
    <Tooltip content={label}>
      <button
        type="button"
        aria-label={label}
        onClick={() => appearanceSettings.set({ ribbonCompact: !compact })}
        className="flex size-6 shrink-0 items-center justify-center rounded-sm text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60"
      >
        {compact ? <ChevronsUpDown className="size-3.5" /> : <ChevronsDownUp className="size-3.5" />}
      </button>
    </Tooltip>
  )
}

/** Optional toolbar row (View → Toolbar): the large ribbon, or icons only. Quick connect joins it when the title bar
 *  (which normally has it) is hidden. */
export function Ribbon() {
  const compactSetting = appearanceSettings.useValue('ribbonCompact')
  const titleBar = appearanceSettings.useValue('showMenuBar')
  const mobile = useIsMobile()
  const compact = compactSetting || mobile
  return (
    <div className={cn('flex shrink-0 items-center gap-1 px-1.5', compact ? 'h-9' : 'h-[3.75rem]')}>
      <ToolbarButtons compact={compact} />
      {!mobile && !titleBar && <QuickConnect primary className="w-[min(22rem,30vw)] shrink-0" />}
      {!mobile && <ToolbarSizeToggle />}
    </div>
  )
}
