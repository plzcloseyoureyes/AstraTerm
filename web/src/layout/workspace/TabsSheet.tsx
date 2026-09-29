import { useState } from 'react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { Layers, X } from 'lucide-react'
import { DialogOverlay, DialogPortal } from '@/components/ui/dialog'
import { usePortalContainer } from '@/components/ui/portal'
import { useSteadyStatus } from '@/components/ui/status-dot'
import { cn } from '@/lib/utils'
import { closeTab, focusTab, useActiveTabId, useTabState, useTabs } from '@/stores/workspace'
import type { TabInfo } from '@/app/registry'
import { resolveIcon, STATUS_DOT, STATUS_LABEL } from './DockTab'

/**
 * Phones: the tab strip has room for one or two tabs, so the group header offers a "Tabs" button with the count that
 * opens a bottom sheet listing every open tab with its full title and status; tap to switch, × to close. (dockview's
 * own overflow chip is hidden on phones, index.css.)
 */
export function TabsSheetButton() {
  const tabs = useTabs()
  const [open, setOpen] = useState(false)
  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Trigger asChild>
        <button
          type="button"
          aria-label={`All tabs (${tabs.length})`}
          className="flex h-6 items-center gap-1 rounded-sm px-1.5 text-xs text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60"
        >
          <Layers className="size-3.5" />
          <span className="tabular-nums">{tabs.length}</span>
        </button>
      </DialogPrimitive.Trigger>
      <TabsSheetContent tabs={tabs} onDone={() => setOpen(false)} />
    </DialogPrimitive.Root>
  )
}

function TabsSheetContent({ tabs, onDone }: { tabs: TabInfo[]; onDone: () => void }) {
  const activeId = useActiveTabId()
  return (
    <DialogPortal container={usePortalContainer()}>
      <DialogOverlay />
      <DialogPrimitive.Content
        className={cn(
          'fixed inset-x-0 bottom-0 z-50 flex max-h-[75dvh] flex-col rounded-t-xl border-t bg-popover pb-[env(safe-area-inset-bottom)] text-popover-foreground shadow-popover outline-none',
          'data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=open]:slide-in-from-bottom data-[state=closed]:slide-out-to-bottom duration-150',
        )}
      >
        <div className="flex h-11 shrink-0 items-center justify-between border-b pr-2 pl-4">
          <DialogPrimitive.Title className="text-md font-semibold">
            Open tabs <span className="font-normal text-muted-foreground tabular-nums">{tabs.length}</span>
          </DialogPrimitive.Title>
          <DialogPrimitive.Close
            aria-label="Close"
            className="flex size-8 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60"
          >
            <X className="size-4" />
          </DialogPrimitive.Close>
        </div>
        <DialogPrimitive.Description className="sr-only">Switch to or close an open tab.</DialogPrimitive.Description>
        <ul className="min-h-0 flex-1 overflow-y-auto p-2" role="list">
          {tabs.map((t) => (
            <TabRow
              key={t.id}
              tab={t}
              active={t.id === activeId}
              onOpen={() => {
                focusTab(t.id)
                onDone()
              }}
            />
          ))}
          {tabs.length === 0 && <li className="px-3 py-6 text-center text-sm text-muted-foreground">No open tabs</li>}
        </ul>
      </DialogPrimitive.Content>
    </DialogPortal>
  )
}

function TabRow({ tab, active, onOpen }: { tab: TabInfo; active: boolean; onOpen: () => void }) {
  const state = useTabState(tab.id)
  const status = useSteadyStatus(state.status, (x) => x === 'connecting')
  const Icon = resolveIcon(tab.kind, tab.params)
  return (
    <li className={cn('flex items-center gap-1 rounded-md', active && 'bg-accent')}>
      <button
        type="button"
        onClick={onOpen}
        aria-current={active ? 'page' : undefined}
        className="flex min-h-11 min-w-0 flex-1 items-center gap-3 rounded-md px-3 text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
      >
        <span className="relative flex shrink-0">
          <Icon className="size-4 text-muted-foreground" />
          {status && (
            <span
              className={cn('absolute -right-0.5 -bottom-0.5 size-[7px] rounded-full ring-2 ring-popover', STATUS_DOT[status], status === 'connecting' && 'bg-popover')}
              aria-label={STATUS_LABEL[status]}
            />
          )}
        </span>
        <span className={cn('min-w-0 flex-1 truncate', active && 'font-medium')}>{tab.title}</span>
        {state.activity && <span className="size-1.5 shrink-0 rounded-full bg-primary" aria-label="New activity" />}
      </button>
      <button
        type="button"
        aria-label={`Close ${tab.title}`}
        onClick={() => void closeTab(tab.id)}
        className="mr-1 flex size-9 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60"
      >
        <X className="size-4" />
      </button>
    </li>
  )
}
