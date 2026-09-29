/*
 * Inline command bar UI: floats over the bottom of its terminal (portal into the terminal's own document, so it also
 * works in pop-out windows), follows the terminal's size and hides while the terminal tab is hidden.
 */
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { CornerDownLeft, MessageSquare, Play, ShieldAlert, ShieldCheck, ShieldQuestion, Sparkles, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Kbd } from '@/components/ui/kbd'
import { Spinner } from '@/components/ui/spinner'
import { getTerminalByTab } from '@/features/terminal/bus'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { ask } from './actions'
import {
  browseHistory,
  closeCommandBar,
  generate,
  insertFromBar,
  runFromBar,
  setBarCommand,
  setBarInput,
  useCommandBar,
} from './barStore'
import { terminalChip } from './context'
import { useAutoGrow } from './hooks'
import type { Risk } from './types'

const RISK: Record<Risk, { label: string; variant: 'success' | 'warning' | 'destructive'; icon: typeof ShieldCheck }> = {
  low: { label: 'Low risk', variant: 'success', icon: ShieldCheck },
  medium: { label: 'Review first', variant: 'warning', icon: ShieldQuestion },
  high: { label: 'High risk', variant: 'destructive', icon: ShieldAlert },
}

export function RiskBadge({ risk }: { risk: Risk }) {
  const r = RISK[risk] ?? RISK.medium
  return (
    <Badge variant={r.variant} className="h-5 gap-1">
      <r.icon /> {r.label}
    </Badge>
  )
}

interface Anchor {
  left: number
  width: number
  bottom: number
  doc: Document
}

/** Track the on-screen box of the terminal the bar belongs to (null while hidden). */
function useAnchor(tabId: string | undefined): Anchor | null {
  const [anchor, setAnchor] = useState<Anchor | null>(null)
  useLayoutEffect(() => {
    const el = getTerminalByTab(tabId)?.term.element
    if (!el) {
      setAnchor(null)
      return
    }
    const win = el.ownerDocument.defaultView ?? window
    let raf = 0
    const measure = () => {
      raf = 0
      const r = el.getBoundingClientRect()
      if (r.width < 40 || r.height < 40) {
        setAnchor(null)
        return
      }
      const width = Math.min(680, r.width - 24)
      const next = { left: r.left + (r.width - width) / 2, width, bottom: win.innerHeight - r.bottom + 12, doc: el.ownerDocument }
      setAnchor((prev) =>
        prev && prev.left === next.left && prev.width === next.width && prev.bottom === next.bottom && prev.doc === next.doc ? prev : next,
      )
    }
    const schedule = () => {
      if (!raf) raf = win.requestAnimationFrame(measure)
    }
    measure()
    const ro = new ResizeObserver(schedule)
    ro.observe(el)
    win.addEventListener('resize', schedule)
    // Dock layout changes (splits, tab switches) move the element without resizing it.
    const iv = win.setInterval(schedule, 500)
    return () => {
      ro.disconnect()
      win.removeEventListener('resize', schedule)
      win.clearInterval(iv)
      if (raf) win.cancelAnimationFrame(raf)
    }
  }, [tabId])
  return anchor
}

export function CommandBar() {
  const s = useCommandBar()
  const anchor = useAnchor(s.open ? s.tabId : undefined)
  const inputRef = useRef<HTMLInputElement>(null)
  const cmdRef = useRef<HTMLTextAreaElement>(null)
  const busy = s.phase === 'waiting' || s.phase === 'thinking'
  const showBusy = useDelayedFlag(busy)
  useAutoGrow(cmdRef, s.command, 160)

  useEffect(() => {
    if (s.open) inputRef.current?.focus()
  }, [s.open, s.focusTick, anchor?.doc])

  // Terminal tab closed while the bar is open.
  useEffect(() => {
    if (!s.open) return
    const t = setInterval(() => {
      if (!getTerminalByTab(useCommandBar.getState().tabId)) closeCommandBar(false)
    }, 1000)
    return () => clearInterval(t)
  }, [s.open])

  if (!s.open || !anchor) return null
  const done = s.phase === 'done' && !!s.result
  const canInsert = done && !!s.command.trim()
  const stale = s.input.trim() !== s.generatedFor
  const enterInserts = canInsert && !stale

  const onInputKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      closeCommandBar()
    } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      if (canInsert) void runFromBar()
    } else if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
      e.preventDefault()
      if (enterInserts) void insertFromBar()
      else void generate()
    } else if ((e.key === 'ArrowUp' || e.key === 'ArrowDown') && (!s.input || s.historyIndex >= 0)) {
      e.preventDefault()
      browseHistory(e.key === 'ArrowUp' ? 1 : -1)
    } else if (e.key === 'Tab' && !e.shiftKey && canInsert) {
      e.preventDefault()
      cmdRef.current?.focus()
    }
  }
  const onCmdKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      closeCommandBar()
    } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      void runFromBar()
    } else if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      void insertFromBar()
    }
  }
  const openInChat = () => {
    const h = getTerminalByTab(s.tabId)
    const intent = s.input.trim()
    closeCommandBar(false)
    void ask(intent || 'Help me with this terminal', { chips: h ? [terminalChip(h)] : [] })
  }

  const bar = (
    <div
      role="dialog"
      aria-label="AI command bar"
      className="@container fixed z-50 animate-in fade-in-0 slide-in-from-bottom-1 duration-150"
      style={{ left: anchor.left, width: anchor.width, bottom: anchor.bottom }}
      onMouseDown={(e) => e.stopPropagation()}
    >
      <div className="overflow-hidden rounded-xl border bg-popover text-popover-foreground shadow-popover">
        <div className="relative flex h-10 items-center gap-2 px-3">
          {/* A long wait swaps the mark for a calm ring in the same slot (nothing for quick answers). */}
          {showBusy ? <Spinner immediate label="Generating a command" className="text-primary" /> : <Sparkles className="size-4 shrink-0 text-primary" />}
          <input
            ref={inputRef}
            value={s.input}
            onChange={(e) => setBarInput(e.target.value)}
            onKeyDown={onInputKey}
            placeholder={done ? 'Refine: e.g. “only in /var/log”, “without sudo”…' : 'Describe what you want to do…'}
            aria-label="What do you want to do?"
            spellCheck={false}
            className="min-w-0 flex-1 bg-transparent text-base outline-none placeholder:text-muted-foreground/70"
          />
          {s.redactions > 0 && (
            <span className="hidden items-center gap-1 text-xs text-muted-foreground @md:flex" title="Secrets were redacted from the terminal context">
              <ShieldCheck className="size-3 text-success" /> {s.redactions}
            </span>
          )}
          <button
            type="button"
            aria-label="Close"
            onClick={() => closeCommandBar()}
            className="rounded-sm p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          >
            <X className="size-3.5" />
          </button>
        </div>

        {s.phase === 'error' && s.error && (
          <div className="border-t px-3 py-2 text-sm animate-in fade-in-0 duration-150">
            <span className="font-medium text-destructive">{s.error.title}</span>
            {s.error.hint && <span className="text-muted-foreground"> — {s.error.hint}</span>}
          </div>
        )}

        {done && s.result && (
          <div className="border-t animate-in fade-in-0 duration-200">
            {s.command || s.result.command ? (
              <div className="px-3 pt-2.5">
                <div className="flex items-start gap-2 rounded-md border bg-muted/40 px-2.5 py-1.5 focus-within:border-ring/60">
                  <span className="pt-px font-mono text-sm text-muted-foreground select-none">$</span>
                  <textarea
                    ref={cmdRef}
                    rows={1}
                    value={s.command}
                    onChange={(e) => setBarCommand(e.target.value)}
                    onKeyDown={onCmdKey}
                    spellCheck={false}
                    aria-label="Suggested command (editable)"
                    className="min-w-0 flex-1 resize-none bg-transparent font-mono text-sm leading-relaxed outline-none"
                  />
                </div>
              </div>
            ) : null}
            <div className="flex items-start gap-2 px-3 py-2">
              {s.result.command && <RiskBadge risk={s.command !== s.result.command ? 'medium' : s.result.risk} />}
              <p className="min-w-0 flex-1 text-sm leading-relaxed text-muted-foreground">
                {s.result.explanation || (s.result.command ? '' : 'No command was suggested.')}
                {s.result.riskReason && s.result.risk !== 'low' && <span className="text-foreground/80"> {s.result.riskReason}</span>}
              </p>
            </div>
          </div>
        )}

        <div className="flex h-8 items-center gap-3 border-t bg-muted/30 px-3 text-xs text-muted-foreground">
          {busy ? (
            <span>Generating…</span>
          ) : (
            !enterInserts && (
              <span className="flex items-center gap-1">
                <Kbd>⏎</Kbd> {done ? 'Refine' : 'Suggest a command'}
              </span>
            )
          )}
          <span className="hidden items-center gap-1 @sm:flex">
            <Kbd>Esc</Kbd> Close
          </span>
          <span className="flex-1" />
          {canInsert && (
            <>
              <Button size="xs" variant="ghost" onClick={() => void insertFromBar()}>
                <CornerDownLeft /> Insert {enterInserts && <Kbd className="ml-0.5">⏎</Kbd>}
              </Button>
              <Button size="xs" variant="secondary" onClick={() => void runFromBar()}>
                <Play /> Run… {enterInserts && <Kbd keys="$mod+Enter" className="ml-0.5" />}
              </Button>
            </>
          )}
          {!canInsert && (
            <button type="button" onClick={openInChat} className="flex items-center gap-1 hover:text-foreground">
              <MessageSquare className="size-3" /> Ask in chat
            </button>
          )}
        </div>
      </div>
    </div>
  )
  return createPortal(bar, anchor.doc.body)
}
