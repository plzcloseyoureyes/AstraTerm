/*
 * Compose window (AUTO-6): a multi-line editor docked above the status bar that sends to the active terminal, the
 * broadcast group, every terminal or chosen sessions. Optional pacing (per-line delay, waiting for the prompt)
 * runs in the backend (TERM-17 pacer) with live progress and cancel. History with Alt+↑/↓; snippets can be inserted.
 */
import * as React from 'react'
import { Braces, ChevronDown, History, Send, SquarePen, X } from 'lucide-react'
import { toast } from 'sonner'
import { cancelJob } from '@/api/jobs'
import type { Snippet } from '@/api/types'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { IconButton } from '@/components/ui/icon-button'
import { Kbd } from '@/components/ui/kbd'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { Tooltip } from '@/components/ui/tooltip'
import { useTerminals } from '@/features/terminal/bus'
import { useMultiExecStore } from '@/features/terminal/multiexec'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage, isMac, plural, storage } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { useSnippets, watchJobEvents } from '../api'
import { collectVariables, resolveTargets, sendComposed, targetForSession, type Target } from '../send'
import { automationSettings } from '../settings'
import { pickSessions, setComposeOpen, useAutomationUI } from '../store'
import { renderTemplate } from '../template'
import type { ProgressEvent } from '../types'

type Mode = 'active' | 'multiexec' | 'all' | 'sessions'

const HISTORY_MAX = 50

function historyKey(userId: string | undefined): string {
  return `nexterm:automation:compose-history:${userId ?? 'anon'}`
}

export function ComposePanel() {
  const open = useAutomationUI((s) => s.composeOpen)
  if (!open) return null
  return <Compose />
}

function Compose() {
  const user = useCurrentUser()
  const settings = automationSettings.use()
  const terminals = useTerminals()
  const multi = useMultiExecStore((s) => s.active)
  const { data: snippets } = useSnippets()
  const [text, setText] = React.useState('')
  const [mode, setMode] = React.useState<Mode>(multi ? 'multiexec' : 'active')
  const [picked, setPicked] = React.useState<string[]>([])
  const [enter, setEnter] = React.useState(true)
  const [lineDelay, setLineDelay] = React.useState(settings.composeLineDelayMs)
  const [waitPrompt, setWaitPrompt] = React.useState(settings.composeWaitPrompt)
  const [history, setHistory] = React.useState<string[]>(() => storage.get<string[]>(historyKey(user?.id), []))
  const [histPos, setHistPos] = React.useState(-1)
  const [job, setJob] = React.useState<{ id: string; done: number; total: number; sessions: number; finished: number } | null>(null)
  const [sending, setSending] = React.useState(false)
  const ref = React.useRef<HTMLTextAreaElement>(null)

  React.useEffect(() => {
    ref.current?.focus()
  }, [])

  // Keep the chosen mode valid when broadcasting switches on/off.
  React.useEffect(() => {
    if (multi && mode === 'active') setMode('multiexec')
    if (!multi && mode === 'multiexec') setMode('active')
  }, [multi]) // eslint-disable-line react-hooks/exhaustive-deps

  const targets: Target[] = React.useMemo(() => {
    if (mode === 'sessions') return picked.map(targetForSession)
    return resolveTargets(mode)
    // terminals: recompute when tabs change
  }, [mode, picked, terminals, multi]) // eslint-disable-line react-hooks/exhaustive-deps

  React.useEffect(() => {
    if (!job) return
    return watchJobEvents(job.id, (ev) => {
      if (ev.event === 'data') {
        const d = ev.data as ProgressEvent | undefined
        if (!d) return
        if (d.kind === 'progress') setJob((j) => (j ? { ...j, done: Math.max(j.done, d.done), total: d.total } : j))
        else if (d.kind === 'done') {
          setJob((j) => (j ? { ...j, finished: j.finished + 1 } : j))
          if (!d.ok && d.error) toast.error(`Sending stopped in ${targetForSession(d.sessionId).title}`, { description: d.error })
        } else if (d.kind === 'warning' && d.message) {
          toast.warning(`${targetForSession(d.sessionId).title}: ${d.message}`)
        }
      } else {
        setJob(null)
        if (ev.event === 'error' && ev.error && ev.error !== 'canceled') toast.error('Sending failed', { description: ev.error })
      }
    })
  }, [job?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  const remember = (t: string) => {
    const next = [t, ...history.filter((h) => h !== t)].slice(0, HISTORY_MAX)
    setHistory(next)
    storage.set(historyKey(user?.id), next)
    setHistPos(-1)
  }

  const send = async () => {
    const body = text
    if (!body.trim() || sending) return
    if (!targets.length) {
      toast.info('No target session', { description: mode === 'sessions' ? 'Choose sessions first.' : 'Open or focus a terminal.' })
      return
    }
    setSending(true)
    try {
      const r = await sendComposed(body, targets, { enter, lineDelayMs: lineDelay, charDelayMs: 0, waitPrompt })
      if (!r.sent) return
      remember(body)
      if (r.jobId) setJob({ id: r.jobId, done: 0, total: 0, sessions: targets.length, finished: 0 })
      if (settings.composeClearAfterSend) setText('')
      if (!r.jobId && targets.length > 1) toast.success(`Sent to ${plural(r.sent, 'session')}`)
    } catch (err) {
      toast.error('Could not send', { description: errorMessage(err) })
    } finally {
      setSending(false)
      ref.current?.focus()
    }
  }

  const insert = async (s: Snippet) => {
    const values = await collectVariables(s)
    if (!values) return
    const ta = ref.current
    const content = renderTemplate(s.content, values).text
    if (!ta) {
      setText((t) => t + content)
      return
    }
    const start = ta.selectionStart ?? text.length
    const end = ta.selectionEnd ?? text.length
    const next = text.slice(0, start) + content + text.slice(end)
    setText(next)
    requestAnimationFrame(() => {
      ta.focus()
      ta.selectionStart = ta.selectionEnd = start + content.length
    })
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const mod = isMac ? e.metaKey : e.ctrlKey
    if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
      const sendKey = settings.composeSendKey === 'enter' ? !e.shiftKey && !mod && !e.altKey : mod
      if (sendKey) {
        e.preventDefault()
        void send()
        return
      }
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      if (!text) setComposeOpen(false)
      else (e.currentTarget as HTMLTextAreaElement).blur()
      return
    }
    if (e.altKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown') && history.length) {
      e.preventDefault()
      const pos = e.key === 'ArrowUp' ? Math.min(history.length - 1, histPos + 1) : Math.max(-1, histPos - 1)
      setHistPos(pos)
      setText(pos < 0 ? '' : history[pos])
    }
  }

  const choose = async () => {
    const ids = await pickSessions({ title: 'Compose: target sessions', confirmLabel: 'Use', selected: picked })
    if (ids) {
      setPicked(ids)
      setMode('sessions')
    }
  }

  const sendLabel = settings.composeSendKey === 'enter' ? 'Enter' : isMac ? '⌘ Enter' : 'Ctrl+Enter'
  // A paced send's progress replaces the Send button only once it has run 300 ms (then ≥ 400 ms, showing its last
  // state if it ends meanwhile): a quick send never flashes a status line and a Stop button.
  const lastJob = React.useRef(job)
  if (job) lastJob.current = job
  const jobShown = useDelayedFlag(!!job)
  const shownJob = jobShown ? (job ?? lastJob.current) : null
  const progress = shownJob ? (shownJob.total ? `${shownJob.done}/${shownJob.total} lines` : 'starting…') : null

  return (
    <section
      aria-label="Compose"
      className="fixed bottom-8 left-1/2 z-40 flex w-[min(960px,calc(100vw-1.5rem))] -translate-x-1/2 flex-col gap-2 rounded-lg border bg-popover p-2.5 text-popover-foreground shadow-popover"
    >
      <header className="flex flex-wrap items-center gap-2">
        <SquarePen className="size-4 text-primary" aria-hidden />
        <h2 className="text-sm font-semibold">Compose</h2>
        <SegmentedControl<Mode>
          size="sm"
          value={mode}
          onValueChange={(v) => (v === 'sessions' ? void choose() : setMode(v))}
          aria-label="Send to"
          options={[
            { value: 'active', label: 'Active terminal' },
            ...(multi ? [{ value: 'multiexec' as Mode, label: 'Broadcast', title: 'Every terminal in the broadcast group' }] : []),
            { value: 'all', label: 'All terminals' },
            { value: 'sessions', label: mode === 'sessions' ? `${picked.length} sessions…` : 'Sessions…' },
          ]}
        />
        <span className={cn('text-xs', targets.length ? 'text-muted-foreground' : 'text-warning')}>
          → {targets.length ? targets.map((t) => t.title).slice(0, 3).join(', ') + (targets.length > 3 ? ` +${targets.length - 3}` : '') : 'no target'}
        </span>
        <IconButton icon={X} label="Close compose (Esc when empty)" size="xs" className="ml-auto" onClick={() => setComposeOpen(false)} />
      </header>
      <Textarea
        ref={ref}
        mono
        rows={4}
        value={text}
        onChange={(e) => {
          setText(e.target.value)
          setHistPos(-1)
        }}
        onKeyDown={onKeyDown}
        placeholder={`Type commands — ${sendLabel} sends, Alt+↑/↓ history`}
        spellCheck={false}
        aria-label="Commands to send"
        className="max-h-[40vh] min-h-20 resize-y"
      />
      <footer className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-sm">
        <label className="flex items-center gap-1.5">
          <Switch size="sm" checked={enter} onCheckedChange={setEnter} aria-label="Press Enter after the last line" />
          Enter at end
        </label>
        <label className="flex items-center gap-1.5">
          <Switch size="sm" checked={waitPrompt} onCheckedChange={setWaitPrompt} aria-label="Wait for the prompt between lines" />
          Wait for prompt
        </label>
        <span className="flex items-center gap-1.5">
          Line delay
          <span className="w-24">
            <NumberInput inputSize="sm" value={lineDelay} min={0} max={60000} step={50} unit="ms" onChange={(v) => setLineDelay(v ?? 0)} aria-label="Delay between lines" />
          </span>
        </span>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button size="xs" variant="ghost" disabled={!snippets?.length}>
              <Braces /> Snippet <ChevronDown />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="max-h-80 overflow-y-auto">
            <DropdownMenuLabel>Insert a snippet</DropdownMenuLabel>
            {(snippets ?? []).map((s) => (
              <DropdownMenuItem key={s.id} onSelect={() => void insert(s)}>
                <span className="truncate">{s.folder ? `${s.folder} / ` : ''}{s.name}</span>
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button size="xs" variant="ghost" disabled={!history.length}>
              <History /> History <ChevronDown />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="max-h-80 max-w-md overflow-y-auto">
            {history.map((h, i) => (
              <DropdownMenuItem key={i} onSelect={() => setText(h)}>
                <span className="truncate font-mono text-xs">{h.replace(/\n/g, ' ⏎ ')}</span>
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
        <span className="ml-auto flex items-center gap-2">
          {shownJob ? (
            <>
              <span className="flex items-center gap-1.5 text-xs text-muted-foreground tabular-nums" role="status">
                <Spinner immediate active={!!job} reserve className="size-3.5" label="Sending" /> Sending {progress}
                {shownJob.sessions > 1 ? ` · ${shownJob.finished}/${shownJob.sessions} sessions done` : ''}
              </span>
              <Button size="sm" variant="secondary" disabled={!job} onClick={() => void cancelJob(shownJob.id).catch(() => undefined)}>
                Stop
              </Button>
            </>
          ) : (
            <Tooltip content="Send" shortcut={settings.composeSendKey === 'enter' ? 'Enter' : '$mod+Enter'}>
              <Button size="sm" onClick={() => void send()} loading={sending || !!job} disabled={!text.trim()}>
                <Send /> Send <Kbd className="ml-1 border-primary-foreground/30 bg-transparent text-primary-foreground/80">{sendLabel}</Kbd>
              </Button>
            </Tooltip>
          )}
        </span>
      </footer>
    </section>
  )
}
