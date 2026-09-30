/*
 * The 'tools' tab: a searchable tool list on the left and the selected tool's panel on the right (the Tools
 * layout). The selected tool is mirrored into the tab params so it persists with the layout and so `tools.open {tool}`
 * can navigate an already-open tab. Tools keep running (and keep their results) while another tool is shown; the
 * list marks running tools. Keyboard: ↑/↓ in the list, "/" or Ctrl/⌘+F-free search field, Esc stops the shown tool.
 */
import * as React from 'react'
import { Search } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from '@/components/ui/select'
import { LazyBoundary, Spinner } from '@/components/ui/spinner'
import { ErrorBoundary } from '@/components/error-boundary'
import { cn } from '@/lib/utils'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { setTabTitle, updateTabParams } from '@/stores/workspace'
import type { TabProps } from '@/app/registry'
import { DEFAULT_TOOL, getTool, TOOL_CATEGORIES, TOOLS, type ToolDef } from './catalog'
import { cancelTool, useToolRunning, useToolJobs } from './jobs'
import type { ToolsTabParams } from './types'

export default function ToolsView({ tabId, params }: TabProps<ToolsTabParams>) {
  const [selected, setSelected] = React.useState<string>(() => (getTool(params?.tool) ? params!.tool! : DEFAULT_TOOL))
  const [query, setQuery] = React.useState('')
  const listRef = React.useRef<HTMLElement>(null)

  // Follow external navigation (tools.open {tool}) which updates tab params.
  React.useEffect(() => {
    if (params?.tool && getTool(params.tool)) setSelected(params.tool)
  }, [params?.tool])

  const select = React.useCallback(
    (id: string) => {
      setSelected(id)
      updateTabParams<ToolsTabParams>(tabId, { tool: id })
    },
    [tabId],
  )

  const tool = getTool(selected) ?? getTool(DEFAULT_TOOL)!
  // The tab title follows the selection, however it changed (list click or tools.open).
  React.useEffect(() => setTabTitle(tabId, `Tools · ${tool.label}`), [tabId, tool.label])
  const Panel = tool.component

  const q = query.trim().toLowerCase()
  const visible = React.useMemo(
    () => TOOLS.filter((t) => !q || t.label.toLowerCase().includes(q) || t.id.includes(q) || (t.keywords ?? []).some((k) => k.includes(q))),
    [q],
  )

  const onListKey = (e: React.KeyboardEvent) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
    e.preventDefault()
    const idx = visible.findIndex((t) => t.id === selected)
    const next = visible[Math.max(0, Math.min(visible.length - 1, idx + (e.key === 'ArrowDown' ? 1 : -1)))]
    if (next) {
      select(next.id)
      listRef.current?.querySelector<HTMLButtonElement>(`[data-tool="${next.id}"]`)?.focus()
    }
  }

  const running = useToolJobs((s) => s.jobs[tool.id]?.status === 'running')
  const onPanelKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape' && !e.defaultPrevented && running) {
      e.preventDefault()
      void cancelTool(tool.id)
    }
  }

  return (
    <div className="flex h-full min-h-0 w-full flex-col @container @2xl:flex-row">
      {/* Narrow tabs (split panes, phones): a grouped picker replaces the side list. */}
      <div className="flex shrink-0 items-center gap-2 border-b bg-panel/50 px-2 py-1.5 @2xl:hidden">
        <Select value={tool.id} onValueChange={select}>
          <SelectTrigger size="sm" aria-label="Tool" className="flex-1">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {TOOL_CATEGORIES.map((cat) => (
              <SelectGroup key={cat}>
                <SelectLabel>{cat}</SelectLabel>
                {TOOLS.filter((t) => t.category === cat).map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.label}
                  </SelectItem>
                ))}
              </SelectGroup>
            ))}
          </SelectContent>
        </Select>
      </div>
      <aside className="hidden w-52 shrink-0 flex-col border-r bg-panel/50 @2xl:flex" aria-label="Tools">
        <div className="border-b p-2">
          <Input
            inputSize="sm"
            leading={<Search className="size-3.5" />}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && visible[0]) select(visible[0].id)
              if (e.key === 'ArrowDown' && visible[0]) {
                e.preventDefault()
                listRef.current?.querySelector<HTMLButtonElement>('[data-tool]')?.focus()
              }
            }}
            placeholder="Search tools…"
            aria-label="Search tools"
          />
        </div>
        <nav ref={listRef} className="min-h-0 flex-1 overflow-y-auto p-1.5" onKeyDown={onListKey}>
          {TOOL_CATEGORIES.map((cat) => {
            const items = visible.filter((t) => t.category === cat)
            if (items.length === 0) return null
            return (
              <div key={cat} className="mb-2" role="group" aria-label={cat}>
                <div className="px-2 py-1 text-2xs font-semibold tracking-wide text-muted-foreground uppercase">{cat}</div>
                {items.map((t) => (
                  <ToolListButton key={t.id} tool={t} active={t.id === tool.id} onSelect={() => select(t.id)} />
                ))}
              </div>
            )
          })}
          {visible.length === 0 && <div className="px-2 py-4 text-center text-sm text-muted-foreground">No tools match “{query}”.</div>}
        </nav>
      </aside>
      <main className="relative min-h-0 min-w-0 flex-1 @container" onKeyDown={onPanelKey}>
        <ErrorBoundary key={tool.id} label={tool.label}>
          <LazyBoundary>
            <Panel />
          </LazyBoundary>
        </ErrorBoundary>
      </main>
    </div>
  )
}

function ToolListButton({ tool, active, onSelect }: { tool: ToolDef; active: boolean; onSelect: () => void }) {
  const isAdmin = useIsAdmin()
  const mode = useRunMode()
  const running = useToolRunning(tool.id)
  const restricted = tool.serverAdminOnly && mode === 'server' && !isAdmin
  const Icon = tool.icon
  return (
    <button
      type="button"
      data-tool={tool.id}
      onClick={onSelect}
      className={cn(
        'flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm outline-none transition-colors',
        'hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50',
        active ? 'bg-accent font-medium text-foreground' : 'text-foreground/80',
        restricted && 'opacity-60',
      )}
      aria-current={active ? 'page' : undefined}
      title={restricted ? 'Requires an administrator in server mode' : undefined}
    >
      <Icon className="size-4 shrink-0 text-muted-foreground" />
      <span className="min-w-0 flex-1 truncate">{tool.label}</span>
      <Spinner active={!!running} className="size-3.5 text-info" label="running" />
      {restricted && (
        <Badge variant="outline" className="shrink-0">
          admin
        </Badge>
      )}
    </button>
  )
}
