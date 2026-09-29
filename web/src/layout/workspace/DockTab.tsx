import { useEffect, useRef, useState, type HTMLAttributes } from 'react'
import type { IDockviewPanelHeaderProps } from 'dockview-react'
import { FileQuestion, X } from 'lucide-react'
import { tabKinds, type IconType } from '@/app/registry'
import { protocolIcon } from '@/app/protocols'
import { DynamicContextMenu } from '@/components/menu-items'
import { ProgressBar } from '@/components/ui/progress'
import { statusDotClass, useSteadyStatus } from '@/components/ui/status-dot'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { closeTab, renameTab, useTabState, type PanelParams, type TabStatus } from '@/stores/workspace'
import { buildTabMenu } from './tabMenu'

export const STATUS_DOT: Record<TabStatus, string> = {
  connecting: statusDotClass('warning', true),
  connected: 'bg-success',
  disconnected: 'bg-muted-foreground/60',
  error: 'bg-destructive',
}

export const STATUS_LABEL: Record<TabStatus, string> = {
  connecting: 'Connecting',
  connected: 'Connected',
  disconnected: 'Disconnected',
  error: 'Error',
}

function useTitle(api: IDockviewPanelHeaderProps['api']): string {
  const [title, setTitle] = useState(api.title ?? '')
  useEffect(() => {
    setTitle(api.title ?? '')
    const d = api.onDidTitleChange((e) => setTitle(e.title))
    return () => d.dispose()
  }, [api])
  return title
}

export function resolveIcon(kind: string | undefined, params: unknown): IconType {
  const def = kind ? tabKinds.get(kind) : undefined
  const p = (params ?? {}) as { protocol?: string }
  try {
    const custom = def?.iconFor?.(params)
    if (custom) return custom
  } catch {
    /* fall through */
  }
  if (p.protocol) return protocolIcon(p.protocol)
  return def?.icon ?? FileQuestion
}

/** Everything a tab shows besides its title: steady status, activity, and the calm progress bar. */
function useTabIndicators(tabId: string) {
  const state = useTabState(tabId)
  // Connecting blips shorter than 300 ms never show; a reconnect keeps the previous dot until it is really waiting.
  const status = useSteadyStatus(state.status, (x) => x === 'connecting')
  const progress = state.progress
  const working = progress === 'indeterminate' || (typeof progress === 'number' && progress >= 0 && progress < 1)
  // Calm like every indicator (docs/UX.md): nothing for work under 300 ms, then visible ≥ 400 ms — finishing inside
  // that time shows a full bar rather than vanishing mid-way.
  const showProgress = useDelayedFlag(working)
  const barValue = progress === 'indeterminate' ? null : typeof progress === 'number' && working ? progress : 1
  // Each operation is its own monotonic run of the bar (a later one starts from its own value, not from full).
  const run = useRef({ working: false, id: 0 })
  if (working && !run.current.working) run.current.id++
  run.current.working = working
  return { status, activity: !!state.activity, showProgress, barValue, runId: run.current.id }
}

/**
 * One tab: icon with status dot, title (inline rename), activity dot, close button and progress bar; context menu,
 * middle-click close and double-click rename. Rendered by dockview group headers (DockTab) and the title bar (TabStrip).
 */
export function TabChip({
  tabId,
  title,
  kind,
  params,
  variant,
  active = false,
  className,
  ...rest
}: {
  tabId: string
  title: string
  kind: string | undefined
  params: unknown
  /** 'dock': inside a group header; 'overflow': dockview's overflow list; 'bar': the title bar. */
  variant: 'dock' | 'overflow' | 'bar'
  active?: boolean
} & Omit<HTMLAttributes<HTMLDivElement>, 'title'>) {
  tabKinds.useItem(kind) // re-render when the kind registers (icon)
  const { status, activity, showProgress, barValue, runId } = useTabIndicators(tabId)
  const [editing, setEditing] = useState(false)
  const Icon = resolveIcon(kind, params)
  const overflow = variant === 'overflow'
  const bar = variant === 'bar'
  const tooltip = [title, status ? STATUS_LABEL[status] : null].filter(Boolean).join(' — ')

  return (
    <DynamicContextMenu items={() => buildTabMenu(tabId, () => setEditing(true))} disabled={editing}>
      <div
        {...rest}
        className={cn(
          'nx-tab group/tab relative flex h-full items-center gap-1.5 pr-1 pl-2.5 text-sm select-none',
          overflow ? 'w-full min-w-40 py-1' : 'max-w-60 min-w-0',
          bar && [
            'h-7 max-w-56 min-w-24 shrink-0 rounded-md pl-2 text-muted-foreground outline-none transition-colors duration-100',
            'hover:bg-foreground/5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
            active && 'bg-foreground/10 text-foreground hover:bg-foreground/10',
          ],
          className,
        )}
        title={tooltip}
        data-tab-id={tabId}
        onMouseDown={(e) => {
          // Middle button: prevent autoscroll; close happens on auxclick.
          if (e.button === 1) e.preventDefault()
          rest.onMouseDown?.(e)
        }}
        onAuxClick={(e) => {
          if (e.button === 1) {
            e.preventDefault()
            e.stopPropagation()
            void closeTab(tabId)
          }
        }}
        onDoubleClick={(e) => {
          e.stopPropagation()
          setEditing(true)
        }}
      >
        <span className="relative flex shrink-0 items-center">
          <Icon className="size-3.5 opacity-80" />
          {status && (
            <span
              className={cn(
                'absolute -right-0.5 -bottom-0.5 size-[7px] rounded-full ring-2',
                bar ? 'ring-sidebar' : 'ring-tabbar',
                STATUS_DOT[status],
                status === 'connecting' && (bar ? 'bg-sidebar' : 'bg-tabbar'),
              )}
              aria-label={STATUS_LABEL[status]}
            />
          )}
        </span>
        {editing ? (
          <RenameInput
            initial={title}
            onDone={(v) => {
              setEditing(false)
              if (v !== null && v !== title) renameTab(tabId, v)
            }}
          />
        ) : (
          <span className="min-w-0 flex-1 truncate">{title}</span>
        )}
        {activity && <span className="size-1.5 shrink-0 rounded-full bg-primary" aria-label="New activity" />}
        {!editing && (
          <button
            type="button"
            tabIndex={bar ? -1 : undefined}
            className={cn(
              'ml-0.5 flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground outline-none',
              'hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
              'opacity-0 group-hover/tab:opacity-100 focus-visible:opacity-100 in-[.dv-active-tab]:opacity-100',
              (overflow || (bar && active)) && 'opacity-100',
            )}
            aria-label={`Close ${title}`}
            draggable={false}
            onPointerDown={(e) => e.stopPropagation()}
            onMouseDown={(e) => e.stopPropagation()}
            onClick={(e) => {
              e.stopPropagation()
              void closeTab(tabId)
            }}
          >
            <X className="size-3.5" />
          </button>
        )}
        {showProgress && (
          <span className={cn('pointer-events-none absolute inset-x-0 bottom-0', bar && 'inset-x-1.5')} aria-hidden>
            <ProgressBar value={barValue} resetKey={runId} className="h-0.5 rounded-none bg-primary/15" />
          </span>
        )}
        {variant === 'dock' && <span className="nx-tab-indicator" aria-hidden />}
        {/* Title bar: the active tab carries an accent bar (besides its tint and brighter title). */}
        {bar && active && <span className="pointer-events-none absolute inset-x-2 -bottom-px h-0.5 rounded-full bg-primary" aria-hidden />}
      </div>
    </DynamicContextMenu>
  )
}

/** Custom dockview tab (group headers of floating / pop-out groups, and every group on phones). */
export function DockTab(props: IDockviewPanelHeaderProps<PanelParams>) {
  const { api, params } = props
  const title = useTitle(api)
  return (
    <TabChip
      tabId={api.id}
      title={title}
      kind={params?.kind}
      params={params?.params}
      variant={props.tabLocation === 'headerOverflow' ? 'overflow' : 'dock'}
    />
  )
}

function RenameInput({ initial, onDone }: { initial: string; onDone: (v: string | null) => void }) {
  const ref = useRef<HTMLInputElement>(null)
  const done = useRef(false)
  const finish = (v: string | null) => {
    if (done.current) return
    done.current = true
    onDone(v)
  }
  useEffect(() => {
    ref.current?.focus()
    ref.current?.select()
  }, [])
  return (
    <input
      ref={ref}
      defaultValue={initial}
      aria-label="Tab title"
      className="h-5 min-w-24 flex-1 rounded-sm border border-ring bg-background px-1 text-sm outline-none"
      onPointerDown={(e) => e.stopPropagation()}
      onMouseDown={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        e.stopPropagation()
        if (e.key === 'Enter') finish(e.currentTarget.value)
        else if (e.key === 'Escape') finish(null)
      }}
      onBlur={(e) => finish(e.currentTarget.value)}
      draggable={false}
      onDragStart={(e) => e.preventDefault()}
    />
  )
}
