/*
 * Promise-based dialogs usable from anywhere (commands, event handlers):
 *
 *   if (await confirm({ title: 'Delete 3 sessions?', destructive: true, confirmLabel: 'Delete' })) ...
 *   const name = await prompt({ title: 'Rename tab', defaultValue: tab.title })   // string | null
 *   const r = await confirmEx({ title: 'Close?', checkboxLabel: "Don't ask again" })  // {ok, checked}
 *
 * <DialogHost/> (mounted once by the app shell) renders the pending dialogs.
 */
import * as React from 'react'
import { create } from 'zustand'
import { TriangleAlert } from 'lucide-react'
import { cn } from '@/lib/utils'
import { Button } from './button'
import { Checkbox } from './checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from './dialog'
import { Input } from './input'
import { Textarea } from './textarea'

export interface ConfirmOptions {
  title: React.ReactNode
  description?: React.ReactNode
  confirmLabel?: string
  cancelLabel?: string
  destructive?: boolean
  /** Optional checkbox (e.g. "Don't ask again"); read it via confirmEx. */
  checkboxLabel?: string
}

export interface PromptOptions {
  title: React.ReactNode
  description?: React.ReactNode
  label?: string
  defaultValue?: string
  placeholder?: string
  confirmLabel?: string
  cancelLabel?: string
  type?: 'text' | 'password' | 'number'
  multiline?: boolean
  /** Return an error message to block submission. */
  validate?: (value: string) => string | undefined | null
  /** Select the default value when the dialog opens (default true). */
  selectOnOpen?: boolean
}

type Pending =
  | { id: number; kind: 'confirm'; opts: ConfirmOptions; resolve: (r: { ok: boolean; checked: boolean }) => void }
  | { id: number; kind: 'prompt'; opts: PromptOptions; resolve: (v: string | null) => void }

const useDialogQueue = create<{ items: Pending[] }>(() => ({ items: [] }))
let nextId = 1

function remove(id: number) {
  useDialogQueue.setState((s) => ({ items: s.items.filter((i) => i.id !== id) }))
}

export function confirmEx(opts: ConfirmOptions): Promise<{ ok: boolean; checked: boolean }> {
  return new Promise((resolve) => {
    useDialogQueue.setState((s) => ({ items: [...s.items, { id: nextId++, kind: 'confirm', opts, resolve }] }))
  })
}

export async function confirm(opts: ConfirmOptions): Promise<boolean> {
  return (await confirmEx(opts)).ok
}

export function prompt(opts: PromptOptions): Promise<string | null> {
  return new Promise((resolve) => {
    useDialogQueue.setState((s) => ({ items: [...s.items, { id: nextId++, kind: 'prompt', opts, resolve }] }))
  })
}

export function DialogHost() {
  const items = useDialogQueue((s) => s.items)
  return (
    <>
      {items.map((item) =>
        item.kind === 'confirm' ? (
          <ConfirmDialog
            key={item.id}
            opts={item.opts}
            onDone={(r) => {
              remove(item.id)
              item.resolve(r)
            }}
          />
        ) : (
          <PromptDialog
            key={item.id}
            opts={item.opts}
            onDone={(v) => {
              remove(item.id)
              item.resolve(v)
            }}
          />
        ),
      )}
    </>
  )
}

function ConfirmDialog({ opts, onDone }: { opts: ConfirmOptions; onDone: (r: { ok: boolean; checked: boolean }) => void }) {
  const [checked, setChecked] = React.useState(false)
  const done = React.useRef(false)
  const finish = (ok: boolean) => {
    if (done.current) return
    done.current = true
    onDone({ ok, checked })
  }
  const cbId = React.useId()
  return (
    <Dialog open onOpenChange={(o) => !o && finish(false)}>
      <DialogContent size="sm" hideClose onOpenAutoFocus={(e) => e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>
            {opts.destructive && <TriangleAlert className="size-4 text-destructive" />}
            {opts.title}
          </DialogTitle>
          {opts.description ? (
            <DialogDescription>{opts.description}</DialogDescription>
          ) : (
            <DialogDescription className="sr-only">Confirm the action</DialogDescription>
          )}
        </DialogHeader>
        {opts.checkboxLabel && (
          <div className="flex items-center gap-2">
            <Checkbox id={cbId} checked={checked} onCheckedChange={(v) => setChecked(v === true)} />
            <label htmlFor={cbId} className="text-base select-none">
              {opts.checkboxLabel}
            </label>
          </div>
        )}
        <DialogFooter>
          <Button variant="secondary" onClick={() => finish(false)}>
            {opts.cancelLabel ?? 'Cancel'}
          </Button>
          <Button variant={opts.destructive ? 'destructive' : 'default'} onClick={() => finish(true)} autoFocus>
            {opts.confirmLabel ?? 'OK'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function PromptDialog({ opts, onDone }: { opts: PromptOptions; onDone: (v: string | null) => void }) {
  const [value, setValue] = React.useState(opts.defaultValue ?? '')
  const [error, setError] = React.useState<string | null>(null)
  const done = React.useRef(false)
  const inputRef = React.useRef<HTMLInputElement & HTMLTextAreaElement>(null)
  const id = React.useId()

  const finish = (v: string | null) => {
    if (done.current) return
    if (v !== null && opts.validate) {
      const err = opts.validate(v)
      if (err) {
        setError(err)
        return
      }
    }
    done.current = true
    onDone(v)
  }

  return (
    <Dialog open onOpenChange={(o) => !o && finish(null)}>
      <DialogContent
        size="sm"
        onOpenAutoFocus={(e) => {
          e.preventDefault()
          const el = inputRef.current
          if (el) {
            el.focus()
            if (opts.selectOnOpen !== false) el.select()
          }
        }}
      >
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            finish(value)
          }}
        >
          <DialogHeader>
            <DialogTitle>{opts.title}</DialogTitle>
            {opts.description ? (
              <DialogDescription>{opts.description}</DialogDescription>
            ) : (
              <DialogDescription className="sr-only">Enter a value</DialogDescription>
            )}
          </DialogHeader>
          <div className="grid gap-1.5">
            {opts.label && (
              <label htmlFor={id} className="text-sm font-medium">
                {opts.label}
              </label>
            )}
            {opts.multiline ? (
              <Textarea
                id={id}
                ref={inputRef}
                value={value}
                rows={5}
                placeholder={opts.placeholder}
                aria-invalid={!!error || undefined}
                onChange={(e) => {
                  setValue(e.target.value)
                  setError(null)
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) finish(value)
                }}
              />
            ) : (
              <Input
                id={id}
                ref={inputRef}
                type={opts.type ?? 'text'}
                value={value}
                placeholder={opts.placeholder}
                autoComplete="off"
                spellCheck={false}
                aria-invalid={!!error || undefined}
                onChange={(e) => {
                  setValue(e.target.value)
                  setError(null)
                }}
              />
            )}
            <p className={cn('min-h-4 text-sm text-destructive', !error && 'invisible')} role="alert">
              {error}
            </p>
          </div>
          <DialogFooter>
            <Button variant="secondary" onClick={() => finish(null)}>
              {opts.cancelLabel ?? 'Cancel'}
            </Button>
            <Button type="submit">{opts.confirmLabel ?? 'OK'}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
