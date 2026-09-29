/*
 * Find in terminal (TERM-14): incremental search with regex / case / whole-word toggles, previous / next, highlight
 * all matches (with overview-ruler marks) and a match counter. Enter = next, Shift+Enter = previous, Esc = close.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { CaseSensitive, ChevronDown, ChevronUp, Highlighter, Regex, WholeWord, X } from 'lucide-react'
import type { ISearchOptions } from '@xterm/addon-search'
import { cn } from '@/lib/utils'
import type { TerminalController } from './controller'

interface Opts {
  caseSensitive: boolean
  wholeWord: boolean
  regex: boolean
  highlightAll: boolean
}

let remembered: { query: string; opts: Opts } = { query: '', opts: { caseSensitive: false, wholeWord: false, regex: false, highlightAll: true } }

function Toggle({ active, onClick, label, icon: Icon }: { active: boolean; onClick: () => void; label: string; icon: typeof Regex }) {
  return (
    <button
      type="button"
      title={label}
      aria-label={label}
      aria-pressed={active}
      onMouseDown={(e) => e.preventDefault()}
      onClick={onClick}
      className={cn(
        'flex size-6 items-center justify-center rounded-sm text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50',
        active && 'bg-primary/15 text-primary hover:bg-primary/20 hover:text-primary',
      )}
    >
      <Icon className="size-3.5" />
    </button>
  )
}

export function SearchBar({ ctrl }: { ctrl: TerminalController }) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [query, setQuery] = useState(() => {
    const sel = ctrl.getSelection()
    return sel && !sel.includes('\n') && sel.length < 200 ? sel : remembered.query
  })
  const [opts, setOpts] = useState<Opts>(remembered.opts)
  const [result, setResult] = useState<{ index: number; count: number } | null>(null)
  const [invalid, setInvalid] = useState(false)
  const [found, setFound] = useState<boolean | null>(null)

  useEffect(() => {
    remembered = { query, opts }
  }, [query, opts])

  const focusInput = useCallback(() => {
    const el = inputRef.current
    if (!el) return
    const sel = ctrl.getSelection()
    if (sel && !sel.includes('\n') && sel.length < 200 && sel !== el.value) setQuery(sel)
    el.focus()
    el.select()
  }, [ctrl])

  useEffect(() => {
    focusInput()
    return ctrl.onSearchRequest(focusInput)
  }, [ctrl, focusInput])

  useEffect(() => {
    const d = ctrl.searchAddon.onDidChangeResults(({ resultIndex, resultCount }) => setResult({ index: resultIndex, count: resultCount }))
    return () => d.dispose()
  }, [ctrl])

  const search = useCallback(
    (dir: 'next' | 'prev', incremental: boolean) => {
      if (!query) {
        ctrl.searchAddon.clearDecorations()
        setResult(null)
        setFound(null)
        setInvalid(false)
        return
      }
      if (opts.regex) {
        try {
          new RegExp(query)
        } catch {
          setInvalid(true)
          ctrl.searchAddon.clearDecorations()
          setResult(null)
          return
        }
      }
      setInvalid(false)
      const so: ISearchOptions = {
        caseSensitive: opts.caseSensitive,
        wholeWord: opts.wholeWord,
        regex: opts.regex,
        incremental: incremental && dir === 'next',
        decorations: opts.highlightAll ? ctrl.searchColors() : undefined,
      }
      if (!opts.highlightAll) {
        ctrl.searchAddon.clearDecorations()
        setResult(null)
      }
      try {
        const ok = dir === 'next' ? ctrl.searchAddon.findNext(query, so) : ctrl.searchAddon.findPrevious(query, so)
        setFound(ok)
      } catch {
        setInvalid(true)
      }
    },
    [ctrl, query, opts],
  )

  // Incremental search while typing / toggling options.
  useEffect(() => {
    search('next', true)
  }, [search])

  useEffect(
    () => () => {
      try {
        ctrl.searchAddon.clearDecorations()
      } catch {
        /* terminal disposed */
      }
    },
    [ctrl],
  )

  const count = result && opts.highlightAll ? (result.count > 0 ? `${result.index >= 0 ? result.index + 1 : '?'}/${result.count}` : 'No results') : found === false ? 'No results' : ''

  return (
    <div
      role="search"
      className="absolute top-2 right-2 left-2 z-30 flex h-8 items-center gap-0.5 rounded-md border bg-popover py-0.5 pr-0.5 pl-1.5 text-popover-foreground shadow-popover @lg:left-auto @lg:right-4"
      onKeyDown={(e) => e.stopPropagation()}
    >
      <input
        ref={inputRef}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            search(e.shiftKey ? 'prev' : 'next', false)
          } else if (e.key === 'Escape') {
            e.preventDefault()
            ctrl.closeSearch()
          } else if (e.altKey && (e.code === 'KeyC' || e.code === 'KeyW' || e.code === 'KeyR')) {
            e.preventDefault()
            const key = e.code === 'KeyC' ? 'caseSensitive' : e.code === 'KeyW' ? 'wholeWord' : 'regex'
            setOpts((o) => ({ ...o, [key]: !o[key] }))
          }
        }}
        placeholder="Find"
        aria-label="Find in terminal"
        aria-invalid={invalid || undefined}
        spellCheck={false}
        autoComplete="off"
        className={cn('h-6 min-w-0 flex-1 rounded-sm bg-transparent px-1 font-mono text-sm outline-none placeholder:font-sans placeholder:text-muted-foreground/70 @lg:w-44 @lg:flex-none', invalid && 'text-destructive')}
      />
      <span className="shrink-0 px-1 text-right text-xs text-muted-foreground tabular @lg:min-w-14" aria-live="polite">
        {invalid ? 'Invalid regex' : count}
      </span>
      <Toggle label="Match case (Alt+C)" icon={CaseSensitive} active={opts.caseSensitive} onClick={() => setOpts((o) => ({ ...o, caseSensitive: !o.caseSensitive }))} />
      <Toggle label="Whole word (Alt+W)" icon={WholeWord} active={opts.wholeWord} onClick={() => setOpts((o) => ({ ...o, wholeWord: !o.wholeWord }))} />
      <Toggle label="Regular expression (Alt+R)" icon={Regex} active={opts.regex} onClick={() => setOpts((o) => ({ ...o, regex: !o.regex }))} />
      <Toggle label="Highlight all matches" icon={Highlighter} active={opts.highlightAll} onClick={() => setOpts((o) => ({ ...o, highlightAll: !o.highlightAll }))} />
      <div className="mx-0.5 h-4 w-px bg-border" />
      <Toggle label="Previous match (Shift+Enter)" icon={ChevronUp} active={false} onClick={() => search('prev', false)} />
      <Toggle label="Next match (Enter)" icon={ChevronDown} active={false} onClick={() => search('next', false)} />
      <Toggle label="Close (Esc)" icon={X} active={false} onClick={() => ctrl.closeSearch()} />
    </div>
  )
}
