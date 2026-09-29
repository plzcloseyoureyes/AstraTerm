import { useEffect, useId, useMemo, useRef, useState } from 'react'
import Fuse from 'fuse.js'
import { ArrowRight, Clock, Star, X, Zap } from 'lucide-react'
import { commands } from '@/app/registry'
import { runCommand } from '@/app/commands'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { sanitizeQuickConnect } from '@/app/quickconnect'
import { recentConnections, useConnections } from '@/api/connections'
import type { Connection } from '@/api/types'
import { Kbd } from '@/components/ui/kbd'
import { useIsMobile } from '@/lib/hooks'
import { cn, storage } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { useUIStore } from '@/stores/ui'

const HISTORY_KEY = 'nexterm:quickconnect-history'
const MAX_HISTORY = 20

export function getQuickConnectHistory(): string[] {
  const h = storage.get<unknown>(HISTORY_KEY, [])
  return Array.isArray(h) ? h.filter((x): x is string => typeof x === 'string').slice(0, MAX_HISTORY) : []
}

function pushHistory(text: string) {
  // Never persist inline credentials: store the sanitized form the quick-connect owner computes (or nothing).
  const safe = sanitizeQuickConnect(text)?.trim()
  if (!safe) return
  const next = [safe, ...getQuickConnectHistory().filter((h) => h !== safe)].slice(0, MAX_HISTORY)
  storage.set(HISTORY_KEY, next)
}

function removeHistory(text: string) {
  storage.set(
    HISTORY_KEY,
    getQuickConnectHistory().filter((h) => h !== text),
  )
}

/** The toolbar (ribbon) shows its quick-connect field: shown with the toolbar, except on phones. Other places (Home)
 *  render their own field only when this is false, so there is never a second field on screen. */
export function useToolbarQuickConnect(): boolean {
  const showRibbon = appearanceSettings.useValue('showRibbon')
  const mobile = useIsMobile()
  return showRibbon && !mobile
}

/** Run quick connect for a spec ("ssh user@host:22", "telnet://10.0.0.1", ...) and remember it. */
export async function submitQuickConnect(text: string): Promise<boolean> {
  const spec = text.trim()
  if (!spec) return false
  const ok = await runCommand('sessions.quickConnect', spec, { source: 'ribbon' })
  if (ok) pushHistory(spec)
  return ok
}

type Suggestion = { kind: 'saved'; conn: Connection } | { kind: 'history'; text: string }

const MAX_SAVED = 6
const MAX_RECENT = 8

/** `user@host:port` of a saved session (what its row shows under the name). */
function target(c: Connection): string {
  return `${c.username ? `${c.username}@` : ''}${c.host ?? ''}${c.port ? `:${c.port}` : ''}` || protocolLabel(c.protocol)
}

/** Text that is clearly a connection spec rather than a search: the typed spec goes first. */
function looksLikeSpec(q: string): boolean {
  return /[@:/]|^\s*(ssh|sftp|telnet|rdp|vnc|ftp|serial|mosh)\s/i.test(q)
}

/**
 * Suggestions for the typed text: saved sessions (fuzzy over name, host, user, tags) and quick-connect history. With
 * nothing typed: favorites / recently used sessions, then the history.
 */
function useSuggestions(value: string, history: string[]): Suggestion[] {
  const { data: conns } = useConnections()
  const fuse = useMemo(
    () =>
      new Fuse(conns ?? [], {
        keys: [
          { name: 'name', weight: 3 },
          { name: 'host', weight: 2 },
          { name: 'username', weight: 1 },
          { name: 'tags', weight: 1.5 },
        ],
        threshold: 0.35,
        ignoreLocation: true,
      }),
    [conns],
  )
  return useMemo(() => {
    const q = value.trim()
    const hist = (q ? history.filter((h) => h.toLowerCase().includes(q.toLowerCase())) : history).slice(0, MAX_RECENT)
    let saved: Connection[]
    if (!q) {
      const favs = (conns ?? []).filter((c) => c.favorite)
      const recent = recentConnections(conns, MAX_SAVED)
      saved = [...favs, ...recent.filter((c) => !c.favorite)].slice(0, MAX_SAVED)
    } else {
      saved = fuse.search(q, { limit: MAX_SAVED }).map((r) => r.item)
    }
    const s: Suggestion[] = saved.map((conn) => ({ kind: 'saved', conn }))
    const h: Suggestion[] = hist.map((text) => ({ kind: 'history', text }))
    return q && looksLikeSpec(q) ? [...h, ...s] : [...s, ...h]
  }, [value, history, conns, fuse])
}

/**
 * Quick connect field (UI-1). Typing searches saved sessions (fuzzy: name, host, user, tags) and the quick-connect
 * history; Enter opens the highlighted suggestion, or connects to the typed spec ("ssh user@host:22") when none is
 * highlighted. ↑↓ choose, Tab completes the highlighted history entry into the field. Ctrl+Shift+Q focuses it
 * (command quickConnect.focus).
 */
export function QuickConnect({
  className,
  autoFocus,
  primary = false,
  size = 'md',
}: {
  className?: string
  autoFocus?: boolean
  /** The instance that reacts to the quickConnect.focus command (the ribbon's). */
  primary?: boolean
  /** 'lg': the Home launcher. */
  size?: 'md' | 'lg'
}) {
  const [value, setValue] = useState('')
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(-1)
  const [history, setHistory] = useState<string[]>(getQuickConnectHistory)
  const inputRef = useRef<HTMLInputElement>(null)
  const listId = useId()
  const focusReq = useUIStore((s) => s.quickConnectFocus)
  const available = commands.useItem('sessions.quickConnect') !== undefined
  const suggestions = useSuggestions(value, history)
  const lg = size === 'lg'

  useEffect(() => {
    if (primary && focusReq > 0) {
      inputRef.current?.focus()
      inputRef.current?.select()
    }
  }, [focusReq, primary])

  // A typed spec is what Enter connects to; the first saved-session match is pre-highlighted only for plain searches.
  useEffect(() => {
    const q = value.trim()
    setActive(q && !looksLikeSpec(q) && suggestions[0]?.kind === 'saved' ? 0 : -1)
  }, [value, suggestions])

  const done = () => {
    setOpen(false)
    setValue('')
    setHistory(getQuickConnectHistory())
    inputRef.current?.blur()
  }

  const choose = async (s: Suggestion | undefined) => {
    if (!s) {
      if (!value.trim()) return
      setOpen(false)
      if (await submitQuickConnect(value)) done()
      return
    }
    if (s.kind === 'saved') {
      setOpen(false)
      if (await runCommand('sessions.connect', { id: s.conn.id }, { source: 'ribbon' })) done()
      return
    }
    setOpen(false)
    if (await submitQuickConnect(s.text)) done()
  }

  const showList = open && suggestions.length > 0
  let lastKind: Suggestion['kind'] | null = null

  return (
    <div className={cn('relative', className)}>
      <div
        className={cn(
          'flex items-center gap-1.5 rounded-md border border-input bg-background/70 pr-1 pl-2 shadow-xs dark:bg-input/25',
          'focus-within:border-ring focus-within:ring-2 focus-within:ring-ring/25',
          lg ? 'h-9 pl-3' : 'h-7',
          !available && 'opacity-60',
        )}
      >
        <Zap className={cn('shrink-0 text-muted-foreground', lg ? 'size-4' : 'size-3.5')} />
        <input
          ref={inputRef}
          value={value}
          autoFocus={autoFocus}
          disabled={!available}
          onChange={(e) => {
            setValue(e.target.value)
            setOpen(true)
          }}
          onFocus={() => {
            setHistory(getQuickConnectHistory())
            setOpen(true)
          }}
          onBlur={() => setTimeout(() => setOpen(false), 120)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              void choose(active >= 0 ? suggestions[active] : undefined)
            } else if (e.key === 'ArrowDown') {
              e.preventDefault()
              setOpen(true)
              setActive((a) => Math.min(suggestions.length - 1, a + 1))
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              setActive((a) => Math.max(-1, a - 1))
            } else if (e.key === 'Tab' && !e.shiftKey && active >= 0 && suggestions[active]?.kind === 'history') {
              e.preventDefault()
              setValue((suggestions[active] as { text: string }).text)
            } else if (e.key === 'Escape') {
              if (open) setOpen(false)
              else {
                setValue('')
                inputRef.current?.blur()
              }
            }
          }}
          placeholder={available ? (lg ? 'Search saved sessions or connect: ssh user@host:22' : 'Quick connect: ssh user@host:22') : 'Quick connect unavailable'}
          aria-label="Quick connect"
          role="combobox"
          aria-expanded={showList}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={active >= 0 ? `${listId}-${active}` : undefined}
          spellCheck={false}
          autoComplete="off"
          autoCapitalize="off"
          className={cn('h-full min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground/70', lg ? 'text-base' : 'text-sm')}
        />
        {value ? (
          <button
            type="button"
            aria-label="Connect"
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => void choose(active >= 0 ? suggestions[active] : undefined)}
            className="flex size-5 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <ArrowRight className="size-3.5" />
          </button>
        ) : (
          <Kbd keys="Control+Shift+q" className="hidden lg:inline-flex" />
        )}
      </div>
      {showList && (
        <ul
          id={listId}
          role="listbox"
          aria-label="Suggestions"
          className="absolute top-full right-0 left-0 z-50 mt-1 max-h-80 overflow-auto rounded-md border bg-popover p-1 shadow-popover"
        >
          {suggestions.map((s, i) => {
            const header = s.kind !== lastKind
            lastKind = s.kind
            const key = s.kind === 'saved' ? `c:${s.conn.id}` : `h:${s.text}`
            return [
              header && (
                <li key={`hdr-${s.kind}`} className="px-2 pt-1.5 pb-1 text-xs font-medium text-muted-foreground" role="presentation">
                  {s.kind === 'saved' ? (value.trim() ? 'Saved sessions' : 'Favorites & recent') : 'Recent quick connections'}
                </li>
              ),
              <li
                key={key}
                id={`${listId}-${i}`}
                role="option"
                aria-selected={i === active}
                onMouseDown={(e) => {
                  e.preventDefault()
                  void choose(s)
                }}
                onMouseEnter={() => setActive(i)}
                className={cn('group flex h-8 cursor-default items-center gap-2 rounded-sm px-2 text-sm', i === active && 'bg-accent')}
              >
                {s.kind === 'saved' ? <SavedRow conn={s.conn} /> : <HistoryRow text={s.text} onRemove={() => {
                  removeHistory(s.text)
                  setHistory(getQuickConnectHistory())
                }} />}
              </li>,
            ]
          })}
          {value.trim() && !looksLikeSpec(value) && (
            <li className="px-2 pt-1.5 pb-1 text-xs text-muted-foreground" role="presentation">
              <Kbd keys="Enter" /> open · or type <span className="font-mono">user@host</span> to connect
            </li>
          )}
        </ul>
      )}
    </div>
  )
}

function SavedRow({ conn }: { conn: Connection }) {
  const Icon = protocolIcon(conn.protocol)
  return (
    <>
      <Icon className="size-3.5 shrink-0 text-muted-foreground" style={conn.color ? { color: conn.color } : undefined} />
      <span className="min-w-0 truncate font-medium">{conn.name}</span>
      {conn.favorite && <Star className="size-3 shrink-0 fill-warning text-warning" aria-label="Favorite" />}
      <span className="ml-auto min-w-0 truncate pl-2 font-mono text-xs text-muted-foreground">{target(conn)}</span>
    </>
  )
}

function HistoryRow({ text, onRemove }: { text: string; onRemove: () => void }) {
  return (
    <>
      <Clock className="size-3.5 shrink-0 text-muted-foreground" />
      <span className="flex-1 truncate font-mono text-xs">{text}</span>
      <button
        type="button"
        aria-label={`Remove ${text} from history`}
        onMouseDown={(e) => {
          e.preventDefault()
          e.stopPropagation()
          onRemove()
        }}
        className="invisible flex size-5 items-center justify-center rounded-sm text-muted-foreground group-hover:visible hover:bg-background hover:text-foreground"
      >
        <X className="size-3" />
      </button>
    </>
  )
}
