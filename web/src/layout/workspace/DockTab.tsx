import { useEffect, useRef, useState } from 'react'
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

/** Custom dockview tab: icon, title, status/activity dots, progress bar, close; rename on double-click. */
export function DockTab(props: IDockviewPanelHeaderProps<PanelParams>) {
  const { api, params } = props
  const tabId = api.id
  const title = useTitle(api)
  tabKinds.useItem(params?.kind) // re-render when the kind registers (icon)
  const state = useTabState(tabId)
  // Connecting blips shorter than 300 ms never show; a reconnect keeps the previous dot until it is really waiting.
  const status = useSteadyStatus(state.status, (x) => x === 'connecting')
  const [editing, setEditing] = useState(false)
  const Icon = resolveIcon(params?.kind, params?.params)
  const overflow = props.tabLocation === 'headerOverflow'

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
  const tooltip = [title, status ? STATUS_LABEL[status] : null].filter(Boolean).join(' — ')

  return (
    <DynamicContextMenu items={() => buildTabMenu(tabId, () => setEditing(true))} disabled={editing}>
      <div
        className={cn(
          'nx-tab group/tab relative flex h-full items-center gap-1.5 pr-1 pl-2.5 text-sm select-none',
          overflow ? 'w-full min-w-40 py-1' : 'max-w-60 min-w-0',
        )}
        title={tooltip}
        data-tab-id={tabId}
        onMouseDown={(e) => {
          // Middle button: prevent autoscroll; close happens on auxclick.
          if (e.button === 1) e.preventDefault()
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
              className={cn('absolute -right-0.5 -bottom-0.5 size-[7px] rounded-full ring-2 ring-tabbar', STATUS_DOT[status], status === 'connecting' && 'bg-tabbar')}
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
        {state.activity && <span className="size-1.5 shrink-0 rounded-full bg-primary" aria-label="New activity" />}
        {!editing && (
          <button
            type="button"
            className={cn(
              'ml-0.5 flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground outline-none',
              'hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
              'opacity-0 group-hover/tab:opacity-100 focus-visible:opacity-100 in-[.dv-active-tab]:opacity-100',
              overflow && 'opacity-100',
            )}
            aria-label={`Close ${title}`}
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
          <span className="pointer-events-none absolute inset-x-0 bottom-0" aria-hidden>
            <ProgressBar value={barValue} resetKey={run.current.id} className="h-0.5 rounded-none bg-primary/15" />
          </span>
        )}
        {!overflow && <span className="nx-tab-indicator" aria-hidden />}
      </div>
    </DynamicContextMenu>
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
