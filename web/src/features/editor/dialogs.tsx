/*
 * Promise-based editor dialogs, rendered by an always-mounted overlay (registerOverlay) so the close guard, commands and
 * the lazy editor chunk can all ask questions:
 *
 *   const r = await choose({ title, choices: [{value: 'save', label: 'Save'}, ...], cancel: 'cancel' })
 *   const target = await saveAsDialog({ suggestedName: 'notes.txt' })        // remote path, download or local handle
 *   const file = await pickRemoteFile({ title: 'Compare with…' })             // {fsId, path, label} | null
 *   const hit = await findInFiles({ fsId, dir })                              // {fsId, path, label, find} | null
 *
 * The heavy file-system picker is lazy-loaded on first use.
 */
import { lazy, Suspense, useEffect, useRef, type ReactNode } from 'react'
import { create } from 'zustand'
import { CircleAlert, Info, ShieldAlert, TriangleAlert } from 'lucide-react'
import type { FsOpenRequest } from '@/api/types'
import { Button, type ButtonProps } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { suspendKeybindings } from '@/app/keybindings'
import type { IconType } from '@/app/registry'
import { cn } from '@/lib/utils'

const RemotePickerDialog = lazy(() => import('./RemotePicker'))

// ---------------------------------------------------------------------------------------------------------------------
// types
// ---------------------------------------------------------------------------------------------------------------------

export interface Choice<T extends string> {
  value: T
  label: string
  variant?: ButtonProps['variant']
  /**
   * What this choice does, in a few words. Choices with a hint are shown as a list of options (label + hint) above
   * the footer — for decisions whose buttons alone would not explain the consequences; the others stay buttons.
   */
  hint?: string
  icon?: IconType
}

export interface ChooseOptions<T extends string> {
  title: ReactNode
  description?: ReactNode
  /** Extra content under the description (details, file names). */
  details?: ReactNode
  tone?: 'info' | 'warning' | 'danger' | 'security'
  /** Buttons, left to right (put the primary action last). */
  choices: Choice<T>[]
  /** Value returned when the dialog is dismissed (Esc, outside click). */
  cancel: T
  /** Button focused initially (default: the last choice). */
  focus?: T
  size?: 'sm' | 'md' | 'lg'
}

/** Minimal File System Access API surface we use (typed locally; not in every lib.dom). */
export interface LocalFileHandle {
  kind?: 'file'
  name: string
  getFile(): Promise<File>
  createWritable?: () => Promise<{ write(data: BlobPart): Promise<void>; close(): Promise<void> }>
  queryPermission?: (opts: { mode: 'read' | 'readwrite' }) => Promise<PermissionState>
  requestPermission?: (opts: { mode: 'read' | 'readwrite' }) => Promise<PermissionState>
}

export type RemoteTarget = {
  fsId: string
  path: string
  label?: string
  source?: FsOpenRequest
  ownsFs?: boolean
  /** Find in files: the text that matched (reveal it after opening). */
  find?: string
}

export type SaveAsResult =
  | ({ target: 'remote' } & RemoteTarget)
  | { target: 'download'; filename: string }
  | { target: 'handle'; handle: LocalFileHandle; filename: string }

export interface PickerOptions {
  /** open a file · save as · find in files (search a folder for text, open a hit) */
  mode: 'open' | 'save' | 'find'
  title?: string
  /** File name to prefill (save mode). */
  suggestedName?: string
  /** Start location. */
  fsId?: string
  dir?: string
  label?: string
  /** Offer "this computer" targets (save mode). */
  allowLocal?: boolean
  /** Confirm button label. */
  confirmLabel?: string
  /** Find mode: initial search text. */
  query?: string
}

type Pending =
  | { id: number; type: 'choice'; opts: ChooseOptions<string>; resolve: (v: string) => void }
  | { id: number; type: 'picker'; opts: PickerOptions; resolve: (v: SaveAsResult | null) => void }

const useDialogs = create<{ items: Pending[] }>(() => ({ items: [] }))
let seq = 1

function push(item: Pending): void {
  useDialogs.setState((s) => ({ items: [...s.items, item] }))
}

function remove(id: number): void {
  useDialogs.setState((s) => ({ items: s.items.filter((i) => i.id !== id) }))
}

export function choose<T extends string>(opts: ChooseOptions<T>): Promise<T> {
  return new Promise<T>((resolve) => {
    push({ id: seq++, type: 'choice', opts: opts as unknown as ChooseOptions<string>, resolve: resolve as (v: string) => void })
  })
}

/** "Save as": a path on any backend file system, a download, or (File System Access API) a file on this computer. */
export function saveAsDialog(opts: Omit<PickerOptions, 'mode'>): Promise<SaveAsResult | null> {
  return new Promise((resolve) => push({ id: seq++, type: 'picker', opts: { ...opts, mode: 'save' }, resolve }))
}

/** Search a folder of a backend file system for files containing some text; resolves the hit to open (or null). */
export async function findInFiles(opts: Omit<PickerOptions, 'mode' | 'allowLocal' | 'suggestedName'> = {}): Promise<RemoteTarget | null> {
  const r = await new Promise<SaveAsResult | null>((resolve) => push({ id: seq++, type: 'picker', opts: { ...opts, mode: 'find' }, resolve }))
  return r && r.target === 'remote' ? r : null
}

/** Pick an existing file on a backend file system. */
export async function pickRemoteFile(opts: Omit<PickerOptions, 'mode' | 'allowLocal' | 'suggestedName'> = {}): Promise<RemoteTarget | null> {
  const r = await new Promise<SaveAsResult | null>((resolve) => push({ id: seq++, type: 'picker', opts: { ...opts, mode: 'open' }, resolve }))
  return r && r.target === 'remote' ? r : null
}

// ---------------------------------------------------------------------------------------------------------------------
// host
// ---------------------------------------------------------------------------------------------------------------------

/** Overlay component (registered by index.ts). Hidden, but kept, while the screen is locked. */
export function EditorDialogHost({ locked }: { locked: boolean }) {
  const items = useDialogs((s) => s.items)
  if (locked || !items.length) return null
  // One at a time, oldest first (a close guard may queue several).
  const item = items[0]
  if (item.type === 'choice') {
    return (
      <ChoiceDialog
        key={item.id}
        opts={item.opts}
        onDone={(v) => {
          remove(item.id)
          item.resolve(v)
        }}
      />
    )
  }
  return (
    <Suspense fallback={null}>
      <RemotePickerDialog
        key={item.id}
        opts={item.opts}
        onDone={(v) => {
          remove(item.id)
          item.resolve(v)
        }}
      />
    </Suspense>
  )
}

const TONE_ICON = {
  info: { icon: Info, className: 'text-info' },
  warning: { icon: TriangleAlert, className: 'text-warning' },
  danger: { icon: CircleAlert, className: 'text-destructive' },
  security: { icon: ShieldAlert, className: 'text-warning' },
} as const

function ChoiceDialog({ opts, onDone }: { opts: ChooseOptions<string>; onDone: (v: string) => void }) {
  const done = useRef(false)
  const finish = (v: string) => {
    if (done.current) return
    done.current = true
    onDone(v)
  }
  useEffect(() => suspendKeybindings(), [])
  const tone = opts.tone ? TONE_ICON[opts.tone] : null
  const focus = opts.focus ?? opts.choices[opts.choices.length - 1]?.value
  const options = opts.choices.filter((c) => c.hint)
  const buttons = opts.choices.filter((c) => !c.hint)
  return (
    <Dialog open onOpenChange={(o) => !o && finish(opts.cancel)}>
      <DialogContent size={opts.size ?? 'md'} hideClose>
        <DialogHeader>
          <DialogTitle>
            {tone && <tone.icon className={cn('size-4 shrink-0', tone.className)} />}
            {opts.title}
          </DialogTitle>
          {opts.description ? (
            <DialogDescription>{opts.description}</DialogDescription>
          ) : (
            <DialogDescription className="sr-only">Choose an action</DialogDescription>
          )}
        </DialogHeader>
        {opts.details}
        {options.length > 0 && <OptionList options={options} focus={focus} onPick={finish} />}
        {buttons.length > 0 && (
          <DialogFooter className="flex-wrap">
            {buttons.map((c) => (
              <Button key={c.value} variant={c.variant ?? 'secondary'} autoFocus={c.value === focus} onClick={() => finish(c.value)}>
                {c.label}
              </Button>
            ))}
          </DialogFooter>
        )}
      </DialogContent>
    </Dialog>
  )
}

/** Choices with a hint: one row each (label, what it does), ↑/↓ between them. The primary one is marked. */
function OptionList({ options, focus, onPick }: { options: Choice<string>[]; focus?: string; onPick: (v: string) => void }) {
  return (
    <div
      role="group"
      aria-label="Options"
      className="grid overflow-hidden rounded-lg border"
      onKeyDown={(e) => {
        if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
        const rows = Array.from(e.currentTarget.querySelectorAll<HTMLButtonElement>('button'))
        const i = rows.indexOf(document.activeElement as HTMLButtonElement)
        const next = rows[(i + (e.key === 'ArrowDown' ? 1 : rows.length - 1)) % rows.length]
        if (next) {
          e.preventDefault()
          next.focus()
        }
      }}
    >
      {options.map((c) => {
        const primary = c.variant === 'default'
        const danger = c.variant === 'destructive'
        return (
          <button
            key={c.value}
            type="button"
            autoFocus={c.value === focus}
            onClick={() => onPick(c.value)}
            className={cn(
              'flex items-start gap-3 border-b px-3.5 py-2.5 text-left outline-none last:border-b-0',
              'transition-colors duration-100 hover:bg-accent/60 focus-visible:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:ring-inset',
              primary && 'bg-primary/6',
            )}
          >
            {c.icon && <c.icon className={cn('mt-0.5 size-4 shrink-0', primary ? 'text-primary' : danger ? 'text-destructive' : 'text-muted-foreground')} />}
            <span className="grid min-w-0 flex-1 gap-0.5">
              <span className={cn('flex items-center gap-2 text-base font-medium', danger && 'text-destructive')}>
                {c.label}
                {primary && <span className="rounded-sm bg-primary/15 px-1.5 text-2xs font-semibold text-primary uppercase">Recommended</span>}
              </span>
              <span className="text-sm text-muted-foreground">{c.hint}</span>
            </span>
          </button>
        )
      })}
    </div>
  )
}
