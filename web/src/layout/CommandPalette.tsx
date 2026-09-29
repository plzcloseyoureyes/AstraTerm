import { Fragment, useEffect, useMemo, useState } from 'react'
import { Command } from 'cmdk'
import Fuse from 'fuse.js'
import { AppWindow, ChevronRight, Command as CommandIcon, Search } from 'lucide-react'
import { commands, tabKinds, type CommandDef } from '@/app/registry'
import { getKeybindings, runCommand } from '@/app/commands'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { useConnections } from '@/api/connections'
import type { Connection } from '@/api/types'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { Kbd } from '@/components/ui/kbd'
import { cn, formatRelativeTime, storage } from '@/lib/utils'
import { focusTab, useTabs } from '@/stores/workspace'
import { closePalette, useUIStore, type PaletteMode } from '@/stores/ui'

const RECENT_KEY = 'astraterm:palette-recent'

function recentCommandIds(): string[] {
  const v = storage.get<unknown>(RECENT_KEY, [])
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}

function rememberCommand(id: string) {
  storage.set(RECENT_KEY, [id, ...recentCommandIds().filter((x) => x !== id)].slice(0, 12))
}

function available(c: CommandDef): boolean {
  if (c.hidden) return false
  try {
    return c.when ? !!c.when() : true
  } catch {
    return false
  }
}

function parseQuery(raw: string, mode: PaletteMode): { mode: PaletteMode; q: string } {
  if (raw.startsWith('>')) return { mode: 'commands', q: raw.slice(1).trim() }
  if (raw.startsWith('@')) return { mode: 'connections', q: raw.slice(1).trim() }
  if (raw.startsWith('%')) return { mode: 'tabs', q: raw.slice(1).trim() }
  return { mode, q: raw.trim() }
}

const itemClass = cn(
  'flex h-8 cursor-default items-center gap-2.5 rounded-md px-2 text-base outline-none select-none',
  'data-[selected=true]:bg-accent data-[selected=true]:text-accent-foreground data-[disabled=true]:opacity-45',
)
const groupClass =
  '[&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:pt-2 [&_[cmdk-group-heading]]:pb-1 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:font-medium [&_[cmdk-group-heading]]:text-muted-foreground'

/** With nothing typed, everyday commands come right after the recently used ones (not "Sign Out" by alphabet). */
const SUGGESTED = ['sessions.new', 'quickConnect.focus', 'terminal.newLocal', 'files.openLocal', 'workspace.split.right', 'workspace.reopenClosed', 'settings.open', 'help.shortcuts']

/** Ctrl/Cmd+K launcher: commands, saved connections (fuzzy) and open tabs. Prefixes: ">" commands, "@" hosts, "%" tabs. */
export function CommandPalette() {
  const open = useUIStore((s) => s.paletteOpen)
  const initialMode = useUIStore((s) => s.paletteMode)
  const initialQuery = useUIStore((s) => s.paletteQuery)
  const [raw, setRaw] = useState('')

  useEffect(() => {
    if (open) setRaw(initialQuery)
  }, [open, initialQuery])

  return (
    <Dialog open={open} onOpenChange={(o) => !o && closePalette()}>
      <DialogContent
        size="lg"
        hideClose
        className="top-[12vh] translate-y-0 gap-0 overflow-hidden p-0 sm:max-w-xl"
        overlayClassName="bg-overlay/40 backdrop-blur-none"
      >
        <DialogTitle className="sr-only">Command palette</DialogTitle>
        <DialogDescription className="sr-only">Search commands, saved sessions and open tabs</DialogDescription>
        {open && <PaletteBody raw={raw} setRaw={setRaw} mode={initialMode} />}
      </DialogContent>
    </Dialog>
  )
}

/** The typed words, lower-case. */
function queryWords(q: string): string[] {
  return q.toLowerCase().split(/\s+/).filter(Boolean)
}

/** Whether `text` contains every typed word (a literal hit, as opposed to a fuzzy one). */
function literalHit(text: string, q: string): boolean {
  const t = text.toLowerCase()
  return queryWords(q).every((w) => t.includes(w))
}

/**
 * Fuzzy results with the literal hits first ("settings" → Open Settings before a server's "Configure …" that only
 * lists the keyword, or a "Recordings" tab that merely resembles it).
 */
function literalFirst<T>(items: T[], q: string, text: (item: T) => string): T[] {
  const hit = items.filter((i) => literalHit(text(i), q))
  return hit.length ? [...hit, ...items.filter((i) => !hit.includes(i))] : items
}

function PaletteBody({ raw, setRaw, mode: initialMode }: { raw: string; setRaw: (v: string) => void; mode: PaletteMode }) {
  const allCommands = commands.useList()
  const tabs = useTabs()
  const { data: connections } = useConnections()
  const { mode, q } = parseQuery(raw, initialMode)

  const cmdList = useMemo(() => allCommands.filter(available), [allCommands])
  const cmdFuse = useMemo(
    () =>
      new Fuse(cmdList, {
        keys: [
          { name: 'title', weight: 3 },
          { name: 'category', weight: 1 },
          { name: 'keywords', weight: 1.5 },
          { name: 'id', weight: 0.5 },
        ],
        threshold: 0.38,
        ignoreLocation: true,
      }),
    [cmdList],
  )
  const connFuse = useMemo(
    () =>
      new Fuse(connections ?? [], {
        keys: [
          { name: 'name', weight: 3 },
          { name: 'host', weight: 2 },
          { name: 'username', weight: 1 },
          { name: 'tags', weight: 1.5 },
          { name: 'protocol', weight: 0.5 },
          { name: 'notes', weight: 0.3 },
        ],
        threshold: 0.35,
        ignoreLocation: true,
      }),
    [connections],
  )

  const showCommands = mode === 'all' || mode === 'commands'
  const showConnections = mode === 'all' || mode === 'connections'
  const showTabs = mode === 'all' || mode === 'tabs'
  const limit = mode === 'all' ? 7 : 60

  const cmdResults: CommandDef[] = useMemo(() => {
    if (!showCommands) return []
    if (!q) {
      const recent = recentCommandIds()
        .map((id) => cmdList.find((c) => c.id === id))
        .filter((c): c is CommandDef => !!c)
      const suggested = SUGGESTED.map((id) => cmdList.find((c) => c.id === id)).filter((c): c is CommandDef => !!c && !recent.includes(c))
      const rest = cmdList
        .filter((c) => !recent.includes(c) && !suggested.includes(c))
        .sort((a, b) => (a.category ?? '').localeCompare(b.category ?? '') || a.title.localeCompare(b.title))
      return [...recent, ...suggested, ...rest].slice(0, mode === 'all' ? 8 : 200)
    }
    return literalFirst(cmdFuse.search(q, { limit }).map((r) => r.item), q, (c) => c.title)
  }, [showCommands, q, cmdList, cmdFuse, limit, mode])

  const connResults: Connection[] = useMemo(() => {
    if (!showConnections || !connections) return []
    if (!q) {
      return [...connections]
        .sort((a, b) => {
          if (a.favorite !== b.favorite) return a.favorite ? -1 : 1
          return (Date.parse(b.lastUsedAt ?? '') || 0) - (Date.parse(a.lastUsedAt ?? '') || 0)
        })
        .slice(0, mode === 'all' ? 6 : 200)
    }
    return literalFirst(connFuse.search(q, { limit }).map((r) => r.item), q, (c) => `${c.name} ${c.username ?? ''}@${c.host ?? ''}`)
  }, [showConnections, connections, q, connFuse, limit, mode])

  const tabResults = useMemo(() => {
    if (!showTabs) return []
    if (!q) return tabs.slice(0, mode === 'all' ? 6 : 200)
    const f = new Fuse(tabs, { keys: ['title', 'kind'], threshold: 0.4, ignoreLocation: true })
    return literalFirst(f.search(q, { limit }).map((r) => r.item), q, (t) => t.title)
  }, [showTabs, tabs, q, limit, mode])

  const connectAvailable = commands.useItem('sessions.connect') !== undefined

  const exec = (fn: () => void) => {
    closePalette()
    // Let the dialog close (focus restore) before running, so commands can move focus.
    setTimeout(fn, 0)
  }

  const placeholder =
    mode === 'commands' ? 'Run a command…' : mode === 'connections' ? 'Connect to a saved session…' : mode === 'tabs' ? 'Go to tab…' : 'Search commands, sessions and tabs…'

  const empty = !cmdResults.length && !connResults.length && !tabResults.length

  // Groups in their usual order (sessions, tabs, commands), except that a group whose best result contains the typed
  // words comes before groups that only resemble them — Enter runs the first item.
  const connText = (c: Connection) => `${c.name} ${c.username ?? ''}@${c.host ?? ''}`
  const groups = [
    {
      key: 'sessions',
      literal: !!q && !!connResults[0] && literalHit(connText(connResults[0]), q),
      node: connResults.length > 0 && (
        <Command.Group heading={q ? 'Sessions' : 'Recent sessions'} className={groupClass}>
          {connResults.map((c) => {
            const Icon = protocolIcon(c.protocol)
            return (
              <Command.Item
                key={`conn:${c.id}`}
                value={`conn:${c.id}`}
                disabled={!connectAvailable}
                className={itemClass}
                onSelect={() => exec(() => void runCommand('sessions.connect', { id: c.id }, { source: 'palette' }))}
              >
                <Icon className="size-4 shrink-0 text-muted-foreground" />
                <span className="truncate">{c.name}</span>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {c.username ? `${c.username}@` : ''}
                  {c.host}
                  {c.port ? `:${c.port}` : ''}
                </span>
                <span className="ml-auto flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
                  {c.lastUsedAt && !q && <span>{formatRelativeTime(c.lastUsedAt)}</span>}
                  <span className="rounded-sm border px-1 text-2xs">{protocolLabel(c.protocol)}</span>
                </span>
              </Command.Item>
            )
          })}
        </Command.Group>
      ),
    },
    {
      key: 'tabs',
      literal: !!q && !!tabResults[0] && literalHit(tabResults[0].title, q),
      node: tabResults.length > 0 && (
        <Command.Group heading="Open tabs" className={groupClass}>
          {tabResults.map((t) => {
            const def = tabKinds.get(t.kind)
            const Icon = (t.params as { protocol?: string } | undefined)?.protocol
              ? protocolIcon((t.params as { protocol: string }).protocol)
              : def?.icon ?? AppWindow
            return (
              <Command.Item key={`tab:${t.id}`} value={`tab:${t.id}`} className={itemClass} onSelect={() => exec(() => focusTab(t.id))}>
                <Icon className="size-4 shrink-0 text-muted-foreground" />
                <span className="truncate">{t.title}</span>
                <span className="ml-auto text-xs text-muted-foreground">Go to tab</span>
              </Command.Item>
            )
          })}
        </Command.Group>
      ),
    },
    {
      key: 'commands',
      literal: !!q && !!cmdResults[0] && literalHit(cmdResults[0].title, q),
      node: cmdResults.length > 0 && (
        <Command.Group heading="Commands" className={groupClass}>
          {cmdResults.map((c) => {
            const Icon = c.icon ?? CommandIcon
            const key = getKeybindings(c.id)[0]
            return (
              <Command.Item
                key={`cmd:${c.id}`}
                value={`cmd:${c.id}`}
                className={itemClass}
                onSelect={() =>
                  exec(() => {
                    rememberCommand(c.id)
                    void runCommand(c.id, undefined, { source: 'palette' })
                  })
                }
              >
                <Icon className="size-4 shrink-0 text-muted-foreground" />
                {c.category && (
                  <span className="flex shrink-0 items-center gap-1 text-muted-foreground">
                    {c.category}
                    <ChevronRight className="size-3" />
                  </span>
                )}
                <span className="truncate">{c.title}</span>
                {key && <Kbd keys={key} className="ml-auto" />}
              </Command.Item>
            )
          })}
        </Command.Group>
      ),
    },
  ].sort((x, y) => Number(y.literal) - Number(x.literal))

  return (
    // A fixed height while open: typing filters the list inside a steady box instead of collapsing and re-growing the
    // palette on every keystroke (the page under it would otherwise appear and disappear as the results change).
    <Command shouldFilter={false} loop label="Command palette" className="flex h-[min(70vh,560px)] flex-col">
      <div className="flex h-11 items-center gap-2 border-b px-3">
        <Search className="size-4 shrink-0 text-muted-foreground" />
        <Command.Input
          value={raw}
          onValueChange={setRaw}
          placeholder={placeholder}
          className="h-full flex-1 bg-transparent text-md outline-none placeholder:text-muted-foreground/70"
          autoFocus
        />
        {mode !== 'all' && (
          <span className="rounded-sm bg-muted px-1.5 py-0.5 text-2xs font-medium text-muted-foreground uppercase">{mode}</span>
        )}
      </div>
      <Command.List className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-1.5">
        {empty && (
          <div className="px-3 py-8 text-center text-sm text-muted-foreground">
            No results. Try <span className="font-mono">&gt;</span> for commands, <span className="font-mono">@</span> for sessions,{' '}
            <span className="font-mono">%</span> for tabs.
          </div>
        )}

        {groups.map((g) => (
          <Fragment key={g.key}>{g.node}</Fragment>
        ))}
      </Command.List>
      <div className="flex h-8 shrink-0 items-center gap-3 border-t bg-muted/40 px-3 text-xs text-muted-foreground">
        <span className="flex items-center gap-1">
          <Kbd>↑</Kbd>
          <Kbd>↓</Kbd> navigate
        </span>
        <span className="flex items-center gap-1">
          <Kbd>↵</Kbd> run
        </span>
        <span className="flex items-center gap-1">
          <Kbd>Esc</Kbd> close
        </span>
        <span className="ml-auto hidden sm:inline">
          <span className="font-mono">&gt;</span> commands · <span className="font-mono">@</span> sessions · <span className="font-mono">%</span> tabs
        </span>
      </div>
    </Command>
  )
}
