/*
 * Dialogs raised by a terminal (paste safety, link confirmation, OSC 52 clipboard access, clipboard history). They
 * portal into the terminal's own document so they appear in the right window when the tab is popped out.
 */
import { useEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode, type RefObject } from 'react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { ClipboardCopy, ClipboardPaste, Info, Link2, ShieldAlert, Trash2, TriangleAlert, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { cn, formatRelativeTime, isMac } from '@/lib/utils'
import { clearClipboardHistory, removeClipboardEntry, useClipboardHistory, writeClipboard } from './clipboard'
import type { PasteDecision, TerminalController, UiRequest } from './controller'
import type { PasteWarning } from './paste'

function TermDialog({
  container,
  title,
  description,
  icon,
  children,
  footer,
  onCancel,
  wide,
  initialFocus,
}: {
  container: HTMLElement | undefined
  title: ReactNode
  description?: ReactNode
  icon?: ReactNode
  children?: ReactNode
  footer: ReactNode
  onCancel: () => void
  wide?: boolean
  initialFocus?: RefObject<HTMLElement | null>
}) {
  return (
    <DialogPrimitive.Root open onOpenChange={(o) => !o && onCancel()}>
      <DialogPrimitive.Portal container={container}>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-overlay backdrop-blur-[2px] data-[state=open]:animate-in data-[state=open]:fade-in-0" />
        <DialogPrimitive.Content
          className={cn(
            'fixed top-1/2 left-1/2 z-50 flex max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-4 rounded-lg border bg-popover p-5 text-popover-foreground shadow-popover outline-none',
            'data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=open]:zoom-in-[0.98] duration-150',
            wide ? 'max-w-2xl' : 'max-w-md',
          )}
          onOpenAutoFocus={(e) => {
            if (initialFocus?.current) {
              e.preventDefault()
              initialFocus.current.focus()
            }
          }}
          onKeyDown={(e) => e.stopPropagation()}
          onCloseAutoFocus={(e) => e.preventDefault()}
        >
          <div className="flex flex-col gap-1.5 pr-6">
            <DialogPrimitive.Title className="flex items-center gap-2 text-md leading-tight font-semibold">
              {icon}
              {title}
            </DialogPrimitive.Title>
            {description ? (
              <DialogPrimitive.Description className="text-base text-muted-foreground">{description}</DialogPrimitive.Description>
            ) : (
              <DialogPrimitive.Description className="sr-only">Terminal dialog</DialogPrimitive.Description>
            )}
          </div>
          {children ? <div className="-mx-5 flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-5">{children}</div> : null}
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:items-center sm:justify-end">{footer}</div>
          <DialogPrimitive.Close
            className="absolute top-3 right-3 rounded-sm p-1 text-muted-foreground opacity-80 outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
            aria-label="Close"
          >
            <X className="size-4" />
          </DialogPrimitive.Close>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Paste
// ---------------------------------------------------------------------------------------------------------------------

function WarningRow({ w }: { w: PasteWarning }) {
  const Icon = w.severity === 'danger' ? ShieldAlert : w.severity === 'warning' ? TriangleAlert : Info
  return (
    <li className={cn('flex items-start gap-2 text-sm', w.severity === 'danger' ? 'text-destructive' : w.severity === 'warning' ? 'text-warning' : 'text-muted-foreground')}>
      <Icon className="mt-0.5 size-3.5 shrink-0" />
      <span>{w.message}</span>
    </li>
  )
}

function PasteDialog({ req, container, onDone }: { req: Extract<UiRequest, { kind: 'paste' }>; container: HTMLElement | undefined; onDone: (d: PasteDecision | null) => void }) {
  const { analysis } = req
  const [text, setText] = useState(req.text)
  const [strip, setStrip] = useState(true)
  const [dontAsk, setDontAsk] = useState(false)
  const pasteRef = useRef<HTMLButtonElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const danger = analysis.severity === 'danger'
  const lines = text.split(/\r?\n/).length - (/\r?\n$/.test(text) ? 1 : 0)
  const title = danger ? 'Potentially dangerous paste' : analysis.multiline ? `Paste ${lines} lines?` : 'Paste text?'
  const done = (mode: PasteDecision['mode'] | null) => onDone(mode ? { text, mode, stripHidden: strip, dontAskMultiline: dontAsk } : null)
  return (
    <TermDialog
      container={container}
      wide
      title={title}
      icon={danger ? <ShieldAlert className="size-4 text-destructive" /> : <ClipboardPaste className="size-4 text-muted-foreground" />}
      description={
        <>
          Review the text before it reaches the shell{req.targets > 1 ? <strong className="font-medium text-foreground"> of {req.targets} terminals (MultiExec)</strong> : null}.
          {analysis.endsWithNewline && !req.bracketed ? ' The last line runs immediately.' : ''}
          {req.bracketed ? ' The application uses bracketed paste: lines are not executed until you press Enter.' : ''}
        </>
      }
      // Enter accepts ordinary multi-line pastes; a dangerous paste needs a deliberate click.
      initialFocus={danger ? cancelRef : pasteRef}
      onCancel={() => done(null)}
      footer={
        <>
          <Button ref={cancelRef} variant="secondary" onClick={() => done(null)}>
            Cancel
          </Button>
          {analysis.multiline && (
            <Button variant="secondary" onClick={() => done('paced')} title="Send one line at a time with a short delay (slow devices)">
              Paste line by line
            </Button>
          )}
          <Button ref={pasteRef} variant={danger ? 'destructive' : 'default'} onClick={() => done('normal')}>
            Paste
          </Button>
        </>
      }
    >
      {analysis.warnings.length > 0 && (
        <ul className="grid gap-1 rounded-md border bg-muted/40 px-3 py-2">
          {analysis.warnings.map((w) => (
            <WarningRow key={w.id} w={w} />
          ))}
        </ul>
      )}
      <textarea
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault()
            done('normal')
          }
        }}
        spellCheck={false}
        aria-label="Text to paste"
        className="min-h-32 w-full resize-y rounded-md border border-input bg-background/60 px-2.5 py-1.5 font-mono text-sm leading-relaxed whitespace-pre outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25 dark:bg-input/25"
        style={{ maxHeight: '40vh' }}
      />
      <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2 text-sm">
        <div className="flex flex-col gap-2">
          {analysis.hasHiddenChars && (
            <label className="flex items-center gap-2">
              <Checkbox checked={strip} onCheckedChange={(v) => setStrip(v === true)} />
              Remove control and invisible characters
            </label>
          )}
          {analysis.multiline && !danger && (
            <label className="flex items-center gap-2">
              <Checkbox checked={dontAsk} onCheckedChange={(v) => setDontAsk(v === true)} />
              Don&apos;t ask again for multi-line pastes
            </label>
          )}
        </div>
        <span className="text-xs text-muted-foreground">
          {text.length.toLocaleString()} characters · {Math.max(1, lines)} line{lines === 1 ? '' : 's'} · {isMac ? '⌘' : 'Ctrl'}+Enter to paste
        </span>
      </div>
    </TermDialog>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Links & OSC 52
// ---------------------------------------------------------------------------------------------------------------------

function LinkDialog({ req, container, onDone }: { req: Extract<UiRequest, { kind: 'link' }>; container: HTMLElement | undefined; onDone: (ok: boolean) => void }) {
  const okRef = useRef<HTMLButtonElement>(null)
  const title = req.action === 'connect' ? 'Connect to this address?' : req.action === 'files' ? 'Open in the file browser?' : 'Open this link?'
  const web = req.scheme === 'http:' || req.scheme === 'https:'
  return (
    <TermDialog
      container={container}
      title={title}
      icon={<Link2 className="size-4 text-muted-foreground" />}
      description={web ? 'The link was printed by the remote application.' : `This is a "${req.scheme.replace(/:$/, '')}" link printed by the remote application. Only continue if you trust it.`}
      initialFocus={okRef}
      onCancel={() => onDone(false)}
      footer={
        <>
          <Button variant="secondary" onClick={() => onDone(false)}>
            Cancel
          </Button>
          <Button ref={okRef} onClick={() => onDone(true)}>
            {req.action === 'connect' ? 'Connect' : 'Open'}
          </Button>
        </>
      }
    >
      <div className="max-h-40 overflow-auto rounded-md border bg-muted/40 px-2.5 py-2 font-mono text-sm break-all">{req.uri}</div>
    </TermDialog>
  )
}

function Osc52ReadDialog({ title, container, onDone }: { title: string; container: HTMLElement | undefined; onDone: (ok: boolean) => void }) {
  const denyRef = useRef<HTMLButtonElement>(null)
  return (
    <TermDialog
      container={container}
      title="Allow clipboard access?"
      icon={<ShieldAlert className="size-4 text-warning" />}
      description={
        <>
          An application in <strong className="font-medium text-foreground">{title}</strong> wants to read your clipboard (OSC 52). It will receive its
          current contents.
        </>
      }
      initialFocus={denyRef}
      onCancel={() => onDone(false)}
      footer={
        <>
          <Button ref={denyRef} variant="secondary" onClick={() => onDone(false)}>
            Deny
          </Button>
          <Button variant="destructive" onClick={() => onDone(true)}>
            Allow once
          </Button>
        </>
      }
    />
  )
}

function Osc52CopyDialog({ text, container, win, onDone }: { text: string; container: HTMLElement | undefined; win: Window; onDone: (ok: boolean) => void }) {
  const copyRef = useRef<HTMLButtonElement>(null)
  return (
    <TermDialog
      container={container}
      title="Copy to clipboard?"
      icon={<ClipboardCopy className="size-4 text-muted-foreground" />}
      description={`The remote application copied ${text.length.toLocaleString()} characters. The browser needs a click to put them on your clipboard.`}
      initialFocus={copyRef}
      onCancel={() => onDone(false)}
      footer={
        <>
          <Button variant="secondary" onClick={() => onDone(false)}>
            Dismiss
          </Button>
          <Button
            ref={copyRef}
            onClick={() => {
              void writeClipboard(text, win).then((ok) => onDone(ok))
            }}
          >
            Copy
          </Button>
        </>
      }
    >
      <div className="max-h-40 overflow-auto rounded-md border bg-muted/40 px-2.5 py-2 font-mono text-sm break-all whitespace-pre-wrap">
        {text.length > 2000 ? `${text.slice(0, 2000)}…` : text}
      </div>
    </TermDialog>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Clipboard history (CC-20)
// ---------------------------------------------------------------------------------------------------------------------

function ClipboardHistoryDialog({ ctrl, container, onDone }: { ctrl: TerminalController; container: HTMLElement | undefined; onDone: (ok: boolean) => void }) {
  const entries = useClipboardHistory((s) => s.entries)
  const [query, setQuery] = useState('')
  const [active, setActive] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return q ? entries.filter((e) => e.text.toLowerCase().includes(q)) : entries
  }, [entries, query])
  useEffect(() => setActive(0), [query])
  const pick = (i: number) => {
    const e = filtered[i]
    if (!e) return
    onDone(true)
    void ctrl.paste(e.text)
  }
  return (
    <TermDialog
      container={container}
      wide
      title="Paste from clipboard history"
      icon={<ClipboardPaste className="size-4 text-muted-foreground" />}
      description="Recent copies from terminals in this window (kept in memory only)."
      initialFocus={inputRef}
      onCancel={() => onDone(false)}
      footer={
        <>
          <Button variant="ghost" className="sm:mr-auto" disabled={!entries.length} onClick={() => clearClipboardHistory()}>
            <Trash2 /> Clear history
          </Button>
          <Button variant="secondary" onClick={() => onDone(false)}>
            Cancel
          </Button>
          <Button disabled={!filtered.length} onClick={() => pick(active)}>
            Paste
          </Button>
        </>
      }
    >
      <Input
        ref={inputRef}
        value={query}
        placeholder="Filter…"
        aria-label="Filter clipboard history"
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown') {
            e.preventDefault()
            setActive((a) => Math.min(filtered.length - 1, a + 1))
          } else if (e.key === 'ArrowUp') {
            e.preventDefault()
            setActive((a) => Math.max(0, a - 1))
          } else if (e.key === 'Enter') {
            e.preventDefault()
            pick(active)
          }
        }}
      />
      <ul role="listbox" aria-label="Clipboard history" className="-mx-1 max-h-[45vh] min-h-24 overflow-y-auto px-1">
        {filtered.length === 0 && <li className="px-2 py-6 text-center text-sm text-muted-foreground">{entries.length ? 'No matches' : 'Nothing copied yet'}</li>}
        {filtered.map((e, i) => (
          <li
            key={e.id}
            role="option"
            aria-selected={i === active}
            className={cn('group flex cursor-default items-start gap-2 rounded-md px-2 py-1.5', i === active && 'bg-accent')}
            onMouseEnter={() => setActive(i)}
            onClick={() => pick(i)}
          >
            <pre className="min-w-0 flex-1 overflow-hidden font-mono text-sm text-ellipsis whitespace-pre-wrap break-all" style={{ maxHeight: '4.5em' }}>
              {e.text.length > 600 ? `${e.text.slice(0, 600)}…` : e.text}
            </pre>
            <div className="flex shrink-0 flex-col items-end gap-1 text-xs text-muted-foreground">
              <span>{formatRelativeTime(e.at)}</span>
              <span className="max-w-40 truncate">{e.source === 'osc52' ? 'remote copy' : e.origin ?? ''}</span>
            </div>
            <button
              type="button"
              aria-label="Remove from history"
              className="flex size-6 shrink-0 items-center justify-center rounded-sm text-muted-foreground opacity-0 group-hover:opacity-100 hover:bg-background hover:text-foreground focus-visible:opacity-100"
              onClick={(ev) => {
                ev.stopPropagation()
                removeClipboardEntry(e.id)
              }}
            >
              <X className="size-3.5" />
            </button>
          </li>
        ))}
      </ul>
    </TermDialog>
  )
}

// ---------------------------------------------------------------------------------------------------------------------

const noopSubscribe = () => () => undefined
const nullUi = () => null
const requestIds = new WeakMap<object, number>()
let nextRequestId = 1
function requestKey(req: object): number {
  let id = requestIds.get(req)
  if (!id) {
    id = nextRequestId++
    requestIds.set(req, id)
  }
  return id
}

/** Renders the controller's current UI request, if any. */
export function TerminalDialogs({ ctrl, container }: { ctrl: TerminalController | null; container: HTMLElement | undefined }) {
  const req = useSyncExternalStore(ctrl?.subscribeUi ?? noopSubscribe, ctrl?.getUi ?? nullUi, ctrl?.getUi ?? nullUi)
  if (!ctrl || !req) return null
  const done = (v: unknown) => ctrl.resolveUi(v)
  const key = requestKey(req)
  switch (req.kind) {
    case 'paste':
      return <PasteDialog key={key} req={req} container={container} onDone={done} />
    case 'link':
      return <LinkDialog key={key} req={req} container={container} onDone={done} />
    case 'osc52-read':
      return <Osc52ReadDialog key={key} title={ctrl.info().title} container={container} onDone={done} />
    case 'osc52-copy':
      return <Osc52CopyDialog key={key} text={req.text} container={container} win={ctrl.win()} onDone={done} />
    case 'history':
      return <ClipboardHistoryDialog key={key} ctrl={ctrl} container={container} onDone={done} />
    default:
      return null
  }
}
