/*
 * Row of large protocol buttons, grouped (terminal / remote desktop / files). Radio-group semantics
 * with arrow-key navigation.
 */
import { useRef, type KeyboardEvent } from 'react'
import { protocolEditors, type ProtocolEditorDef } from '@/app/registry'
import { cn } from '@/lib/utils'

const GROUPS: { id: ProtocolEditorDef['group']; label: string }[] = [
  { id: 'terminal', label: 'Terminal' },
  { id: 'graphical', label: 'Remote desktop' },
  { id: 'files', label: 'Files' },
  { id: 'other', label: 'Other' },
]

export function ProtocolPicker({ value, onChange, disabled }: { value: string; onChange: (protocol: string) => void; disabled?: boolean }) {
  const editors = protocolEditors.useList()
  const refs = useRef(new Map<string, HTMLButtonElement>())
  const ordered = GROUPS.flatMap((g) => editors.filter((e) => e.group === g.id))

  const onKeyDown = (e: KeyboardEvent<HTMLButtonElement>, index: number) => {
    let next = -1
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = (index + 1) % ordered.length
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = (index - 1 + ordered.length) % ordered.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = ordered.length - 1
    if (next < 0) return
    e.preventDefault()
    const p = ordered[next].protocol
    onChange(p)
    refs.current.get(p)?.focus()
  }

  return (
    <div role="radiogroup" aria-label="Session type" className="flex flex-wrap items-end gap-x-3 gap-y-2">
      {GROUPS.map((g) => {
        const items = editors.filter((e) => e.group === g.id)
        if (!items.length) return null
        return (
          <div key={g.id} className="grid gap-1">
            <span className="px-1 text-2xs font-medium tracking-wide text-muted-foreground uppercase">{g.label}</span>
            <div className="flex flex-wrap gap-0.5 rounded-lg border bg-muted/30 p-0.5">
              {items.map((e) => {
                const Icon = e.icon
                const selected = e.protocol === value
                const index = ordered.indexOf(e)
                return (
                  <button
                    key={e.protocol}
                    ref={(el) => {
                      if (el) refs.current.set(e.protocol, el)
                      else refs.current.delete(e.protocol)
                    }}
                    type="button"
                    role="radio"
                    aria-checked={selected}
                    tabIndex={selected || (!ordered.some((x) => x.protocol === value) && index === 0) ? 0 : -1}
                    disabled={disabled}
                    title={e.description ?? e.label}
                    onClick={() => onChange(e.protocol)}
                    onKeyDown={(ev) => onKeyDown(ev, index)}
                    className={cn(
                      'flex h-14 w-16 flex-col items-center justify-center gap-1 rounded-md px-1 text-foreground/75 outline-none transition-colors',
                      'hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 disabled:opacity-50',
                      selected && 'bg-primary/15 text-primary ring-1 ring-primary/40 hover:bg-primary/20 hover:text-primary',
                    )}
                  >
                    <Icon className="size-5" strokeWidth={1.6} />
                    <span className="max-w-full truncate text-2xs leading-tight font-medium">{e.label}</span>
                  </button>
                )
              })}
            </div>
          </div>
        )
      })}
    </div>
  )
}
