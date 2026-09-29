import * as React from 'react'
import { X } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface TagInputProps {
  value: string[]
  onChange: (tags: string[]) => void
  placeholder?: string
  /** Suggestions shown while typing (e.g. existing tags). */
  suggestions?: string[]
  /** Normalise a tag before adding (default: trim; return '' to reject). */
  normalize?: (tag: string) => string
  maxTags?: number
  disabled?: boolean
  className?: string
  id?: string
  'aria-label'?: string
  'aria-describedby'?: string
  'aria-invalid'?: boolean
}

/** Chips input: Enter / comma / Tab adds, Backspace on empty removes the last tag. */
export function TagInput({
  value,
  onChange,
  placeholder = 'Add tag…',
  suggestions = [],
  normalize = (t) => t.trim(),
  maxTags = 64,
  disabled,
  className,
  id,
  ...aria
}: TagInputProps) {
  const [draft, setDraft] = React.useState('')
  const [active, setActive] = React.useState(-1)
  const inputRef = React.useRef<HTMLInputElement>(null)
  const listId = React.useId()

  const matches = React.useMemo(() => {
    const q = draft.trim().toLowerCase()
    if (!q) return []
    const have = new Set(value.map((v) => v.toLowerCase()))
    return suggestions.filter((s) => s.toLowerCase().includes(q) && !have.has(s.toLowerCase())).slice(0, 8)
  }, [draft, suggestions, value])

  const add = (raw: string) => {
    const parts = raw.split(',').map(normalize).filter(Boolean)
    if (!parts.length) return
    const next = [...value]
    for (const p of parts) {
      if (next.length >= maxTags) break
      if (!next.some((t) => t.toLowerCase() === p.toLowerCase())) next.push(p)
    }
    onChange(next)
    setDraft('')
    setActive(-1)
  }

  const removeAt = (i: number) => onChange(value.filter((_, idx) => idx !== i))

  return (
    <div className={cn('relative', className)}>
      <div
        className={cn(
          'flex min-h-8 w-full flex-wrap items-center gap-1 rounded-md border border-input bg-background/60 px-1.5 py-1 shadow-xs dark:bg-input/25',
          'focus-within:border-ring focus-within:ring-2 focus-within:ring-ring/25',
          aria['aria-invalid'] && 'border-destructive',
          disabled && 'pointer-events-none opacity-50',
        )}
        onMouseDown={(e) => {
          if (e.target === e.currentTarget) {
            e.preventDefault()
            inputRef.current?.focus()
          }
        }}
      >
        {value.map((tag, i) => (
          <span key={`${tag}-${i}`} className="inline-flex h-5 items-center gap-0.5 rounded-[4px] bg-secondary pr-0.5 pl-1.5 text-sm">
            {tag}
            <button
              type="button"
              onClick={() => removeAt(i)}
              className="flex size-4 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
              aria-label={`Remove ${tag}`}
            >
              <X className="size-3" />
            </button>
          </span>
        ))}
        <input
          ref={inputRef}
          id={id}
          value={draft}
          disabled={disabled}
          placeholder={value.length ? '' : placeholder}
          className="h-5 min-w-16 flex-1 bg-transparent px-1 text-base outline-none placeholder:text-muted-foreground/70"
          role="combobox"
          aria-expanded={matches.length > 0}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={active >= 0 ? `${listId}-${active}` : undefined}
          {...aria}
          onChange={(e) => {
            const v = e.target.value
            if (v.includes(',')) add(v)
            else {
              setDraft(v)
              setActive(-1)
            }
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || (e.key === 'Tab' && draft.trim())) {
              if (draft.trim() || active >= 0) {
                e.preventDefault()
                add(active >= 0 ? matches[active] : draft)
              }
            } else if (e.key === 'Backspace' && !draft && value.length) {
              removeAt(value.length - 1)
            } else if (e.key === 'ArrowDown' && matches.length) {
              e.preventDefault()
              setActive((a) => (a + 1) % matches.length)
            } else if (e.key === 'ArrowUp' && matches.length) {
              e.preventDefault()
              setActive((a) => (a <= 0 ? matches.length - 1 : a - 1))
            } else if (e.key === 'Escape') {
              setDraft('')
            }
          }}
          onBlur={() => draft.trim() && add(draft)}
        />
      </div>
      {matches.length > 0 && (
        <ul
          id={listId}
          role="listbox"
          className="absolute top-full left-0 z-50 mt-1 max-h-48 w-full overflow-auto rounded-md border bg-popover p-1 shadow-popover"
        >
          {matches.map((m, i) => (
            <li
              key={m}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              className={cn('cursor-default rounded-sm px-2 py-1 text-base', i === active && 'bg-accent')}
              onMouseDown={(e) => {
                e.preventDefault()
                add(m)
              }}
              onMouseEnter={() => setActive(i)}
            >
              {m}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
