/*
 * Location bar: breadcrumbs (click a segment to go there, drop files / rows on it) that turn into an editable path
 * field with directory autocomplete (Tab completes, ↑↓ choose, Enter goes), plus bookmarks and recent folders.
 */
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { Popover as PopoverPrimitive } from 'radix-ui'
import { useQuery } from '@tanstack/react-query'
import { Bookmark, ChevronRight, Clock3, Folder, Star, X } from 'lucide-react'
import { IconButton } from '@/components/ui/icon-button'
import { Spinner } from '@/components/ui/spinner'
import { cn } from '@/lib/utils'
import { fsApi, fsKeys } from '../api'
import { acceptDrag, performDrop } from '../dnd'
import { isDirLike } from '../format'
import { breadcrumbs, dirname, joinPath, resolveInput } from '../paths'
import { filesSettings, getBookmarks, getRecentPaths, removeBookmark, toggleBookmark } from '../settings'
import type { FsContext } from '../types'
import type { BrowserController } from './controller'
import { useView } from './viewStore'

interface Suggestion {
  path: string
  label: string
  kind: 'dir' | 'bookmark' | 'recent'
}

/**
 * `busy`: a navigation is taking long (already delayed by the caller) — a small spinner in a slot that is always
 * reserved, so the breadcrumbs never move when it appears.
 */
export function PathBar({ controller, ctx, busy = false, className }: { controller: BrowserController; ctx: FsContext; busy?: boolean; className?: string }) {
  const viewId = controller.viewId
  const path = useView(viewId, (v) => v.pending ?? v.path)
  const editTick = useView(viewId, (v) => v.editTick)
  const editSeed = useView(viewId, (v) => v.editSeed)
  const bookmarks = filesSettings.useValue('bookmarks')
  const [editing, setEditing] = useState(false)
  const [text, setText] = useState('')
  const [typed, setTyped] = useState(false)
  const [active, setActive] = useState(-1)
  /** Suggestions hidden with Escape (shown again when typing). */
  const [dismissed, setDismissed] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  const crumbsRef = useRef<HTMLDivElement>(null)
  const [dropOn, setDropOn] = useState<string | null>(null)

  const marked = !!path && (bookmarks?.[ctx.placeKey] ?? []).some((b) => b.path === path)

  const startEdit = (seed?: string | null) => {
    setText(seed ?? path ?? '')
    setTyped(!!seed)
    setActive(-1)
    setDismissed(false)
    setEditing(true)
  }

  useEffect(() => {
    if (editTick > 0) startEdit(editSeed)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editTick])

  useLayoutEffect(() => {
    if (!editing) return
    const el = inputRef.current
    if (!el) return
    el.focus()
    if (typed) el.setSelectionRange(el.value.length, el.value.length)
    else el.select()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editing])

  // Keep the last breadcrumb visible.
  useLayoutEffect(() => {
    const el = crumbsRef.current
    if (el) el.scrollLeft = el.scrollWidth
  }, [path, editing])

  // --- autocomplete --------------------------------------------------------------------------------------------------
  const home = ctx.handle.home || '/'
  const cwd = path ?? home
  const resolved = text.trim() ? resolveInput(text, cwd, home) : cwd
  const endsWithSlash = /[/\\]$/.test(text.trim())
  const parent = endsWithSlash ? resolved : dirname(resolved)
  const prefix = endsWithSlash ? '' : (text.trim().split(/[/\\]/).pop() ?? '')
  const listing = useQuery({
    queryKey: fsKeys.list(ctx.handle.id, parent),
    queryFn: ({ signal }) => fsApi.list(ctx.handle.id, parent, signal),
    enabled: editing && typed && !!parent,
    staleTime: 15_000,
    retry: false,
    placeholderData: undefined, // another folder's entries would suggest paths that do not exist
  })

  const computed = useMemo<Suggestion[]>(() => {
    if (!editing || dismissed) return []
    if (!typed) {
      const out: Suggestion[] = []
      for (const b of getBookmarks(ctx.placeKey)) out.push({ path: b.path, label: b.label || b.path, kind: 'bookmark' })
      for (const r of getRecentPaths(ctx.placeKey)) if (r !== path && !out.some((o) => o.path === r)) out.push({ path: r, label: r, kind: 'recent' })
      return out.slice(0, 30)
    }
    const entries = listing.data?.entries ?? []
    const p = prefix.toLowerCase()
    const showHidden = filesSettings.get().showHidden || p.startsWith('.')
    return entries
      .filter((e) => isDirLike(e) && (showHidden || !e.hidden) && e.name.toLowerCase().startsWith(p))
      .sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' }))
      .slice(0, 60)
      .map((e) => ({ path: joinPath(parent, e.name), label: e.name, kind: 'dir' as const }))
    // bookmarks: re-read when the settings change
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editing, dismissed, typed, listing.data, prefix, parent, ctx.placeKey, path, bookmarks])

  // While the folder being typed into still lists, the previous suggestions stay (the popup does not close and
  // reopen on every keystroke of a slow connection); they are replaced as soon as the listing is there.
  const loadingSuggestions = editing && typed && !dismissed && listing.isFetching && !listing.data
  const [suggestions, setSuggestions] = useState<Suggestion[]>([])
  useEffect(() => {
    if (!(loadingSuggestions && computed.length === 0)) setSuggestions(computed)
  }, [computed, loadingSuggestions])
  useEffect(() => setActive(-1), [suggestions])

  const go = (target: string) => {
    setEditing(false)
    controller.go(target)
    controller.focusList()
  }

  const complete = (s: Suggestion) => {
    const next = s.path.endsWith('/') ? s.path : `${s.path}/`
    setText(next)
    setTyped(true)
    requestAnimationFrame(() => inputRef.current?.setSelectionRange(next.length, next.length))
  }

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    e.stopPropagation()
    const open = suggestions.length > 0
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (open) setActive((a) => Math.min(suggestions.length - 1, a + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      if (open) setActive((a) => Math.max(-1, a - 1))
    } else if (e.key === 'Tab' && !e.shiftKey && open && typed) {
      e.preventDefault()
      complete(suggestions[Math.max(0, active)])
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (active >= 0 && suggestions[active]) go(suggestions[active].path)
      else go(text)
    } else if (e.key === 'Escape') {
      e.preventDefault()
      // First Escape closes the suggestions, the second one leaves the location bar.
      if (open && (typed || active >= 0)) {
        setDismissed(true)
        setActive(-1)
        return
      }
      setEditing(false)
      controller.focusList()
    }
  }

  const crumbs = path ? breadcrumbs(path) : []
  const listId = `${viewId}-pathlist`

  if (editing) {
    return (
      <PopoverPrimitive.Root open={suggestions.length > 0} onOpenChange={(o) => !o && setActive(-1)}>
        <PopoverPrimitive.Anchor asChild>
          <div className={cn('flex h-7 min-w-0 items-center rounded-md border border-ring bg-background ring-2 ring-ring/20', className)}>
            <input
              ref={inputRef}
              role="combobox"
              aria-label="Location"
              aria-expanded={suggestions.length > 0}
              aria-controls={listId}
              aria-autocomplete="list"
              aria-activedescendant={active >= 0 ? `${listId}-${active}` : undefined}
              value={text}
              spellCheck={false}
              autoComplete="off"
              autoCapitalize="off"
              className="h-full min-w-0 flex-1 bg-transparent px-2 font-mono text-sm outline-none"
              onChange={(e) => {
                setText(e.target.value)
                setTyped(true)
                setDismissed(false)
              }}
              onKeyDown={onKeyDown}
              onBlur={() => setEditing(false)}
            />
            <IconButton icon={X} label="Cancel" size="xs" onMouseDown={(e) => e.preventDefault()} onClick={() => setEditing(false)} className="mr-0.5" />
          </div>
        </PopoverPrimitive.Anchor>
        <PopoverPrimitive.Portal>
          <PopoverPrimitive.Content
            align="start"
            sideOffset={3}
            onOpenAutoFocus={(e) => e.preventDefault()}
            onCloseAutoFocus={(e) => e.preventDefault()}
            className="z-50 max-h-72 w-(--radix-popover-trigger-width) min-w-56 overflow-y-auto rounded-md border bg-popover p-1 text-popover-foreground shadow-popover"
          >
            <div role="listbox" id={listId} aria-label="Folders">
              {suggestions.map((s, i) => {
                const Icon = s.kind === 'bookmark' ? Star : s.kind === 'recent' ? Clock3 : Folder
                const showHeader = !typed && (i === 0 || suggestions[i - 1].kind !== s.kind)
                return (
                  <div key={`${s.kind}:${s.path}`}>
                    {showHeader && <div className="px-2 pt-1.5 pb-0.5 text-2xs font-semibold tracking-wider text-muted-foreground uppercase">{s.kind === 'bookmark' ? 'Bookmarks' : 'Recent'}</div>}
                    <div
                      id={`${listId}-${i}`}
                      role="option"
                      aria-selected={i === active}
                      onMouseDown={(e) => e.preventDefault()}
                      onClick={() => go(s.path)}
                      onMouseEnter={() => setActive(i)}
                      className={cn(
                        'group flex h-7 cursor-default items-center gap-2 rounded-sm px-2 text-sm',
                        i === active && 'bg-accent text-accent-foreground',
                      )}
                    >
                      <Icon className={cn('size-3.5 shrink-0', s.kind === 'dir' ? 'text-amber-500' : s.kind === 'bookmark' ? 'text-amber-500' : 'text-muted-foreground')} />
                      <span className={cn('min-w-0 flex-1 truncate', s.kind !== 'dir' && 'font-mono text-xs')}>{s.label}</span>
                      {s.kind === 'bookmark' && (
                        <button
                          type="button"
                          aria-label={`Remove bookmark ${s.path}`}
                          className="rounded-sm p-0.5 text-muted-foreground opacity-0 group-hover:opacity-100 hover:bg-background hover:text-foreground"
                          onMouseDown={(e) => e.preventDefault()}
                          onClick={(e) => {
                            e.stopPropagation()
                            removeBookmark(ctx.placeKey, s.path)
                          }}
                        >
                          <X className="size-3" />
                        </button>
                      )}
                    </div>
                  </div>
                )
              })}
            </div>
          </PopoverPrimitive.Content>
        </PopoverPrimitive.Portal>
      </PopoverPrimitive.Root>
    )
  }

  return (
    <div
      className={cn(
        'group/path flex h-7 min-w-0 items-center rounded-md border border-input bg-background/60 shadow-xs dark:bg-input/25',
        'focus-within:border-ring hover:border-ring/50',
        className,
      )}
      onClick={(e) => {
        if (e.target === e.currentTarget || (e.target as Element).hasAttribute('data-crumbs')) startEdit()
      }}
      title="Click to type a path"
    >
      <div ref={crumbsRef} data-crumbs="" className="flex h-full min-w-0 flex-1 items-center overflow-x-auto pl-1 scrollbar-none" role="navigation" aria-label="Breadcrumbs">
        {crumbs.map((c, i) => {
          const last = i === crumbs.length - 1
          return (
            <span key={c.path} className="flex shrink-0 items-center">
              {i > 0 && <ChevronRight className="size-3 shrink-0 text-muted-foreground/60" aria-hidden />}
              <button
                type="button"
                aria-current={last ? 'location' : undefined}
                onClick={() => (last ? startEdit() : controller.navigate(c.path))}
                onDragOver={(e) => {
                  if (acceptDrag(e, ctx, c.path)) setDropOn(c.path)
                }}
                onDragLeave={() => setDropOn(null)}
                onDrop={(e) => {
                  setDropOn(null)
                  performDrop(e, ctx, c.path)
                }}
                className={cn(
                  'max-w-40 truncate rounded-sm px-1 py-0.5 text-sm outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50',
                  last ? 'font-medium text-foreground' : 'text-muted-foreground hover:text-foreground',
                  i === 0 && 'font-mono',
                  dropOn === c.path && 'bg-primary/20 ring-1 ring-primary',
                )}
                title={c.path}
              >
                {c.name}
              </button>
            </span>
          )
        })}
        {!crumbs.length && <span className="px-1 text-sm text-muted-foreground">…</span>}
      </div>
      <span className="flex size-5 shrink-0 items-center justify-center" aria-hidden={!busy}>
        <Spinner immediate active={busy} className="size-3.5" label="Opening folder" />
      </span>
      {path && (
        <IconButton
          icon={marked ? Star : Bookmark}
          label={marked ? 'Remove bookmark' : 'Bookmark this folder'}
          size="xs"
          active={marked}
          iconClassName={marked ? 'fill-amber-400 text-amber-500' : undefined}
          className={cn('mr-0.5', !marked && 'opacity-0 group-hover/path:opacity-100 focus-visible:opacity-100')}
          onClick={(e) => {
            e.stopPropagation()
            toggleBookmark(ctx.placeKey, path)
          }}
        />
      )}
    </div>
  )
}
