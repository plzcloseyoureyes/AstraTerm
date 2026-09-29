/*
 * The assistant chat (sidebar panel "ai" and tab kind "ai" share it): conversation history (local), streamed
 * Markdown answers with Insert / Run / Copy on code blocks, context chips (terminal output, selection, editor file,
 * failed command), a model picker, and calm states for setup / loading / errors.
 */
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import {
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  Check,
  ChevronDown,
  Copy,
  FileText,
  History,
  MessageSquarePlus,
  MoreHorizontal,
  PanelRight,
  Paperclip,
  Pencil,
  RotateCcw,
  Settings2,
  ShieldCheck,
  Sparkles,
  Square,
  SquareTerminal,
  TextSelect,
  Trash2,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { prompt } from '@/components/ui/dialog-host'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Kbd } from '@/components/ui/kbd'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane, Spinner } from '@/components/ui/spinner'
import { getActiveTerminal, useTerminals } from '@/features/terminal/bus'
import { useDelayedFlag, useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, copyText, errorMessage, formatRelativeTime } from '@/lib/utils'
import { showSidebarPanel } from '@/layout/Sidebar'
import { closeTab, openTab } from '@/stores/workspace'
import { AI_PANEL, AI_TAB, openSettings, startChat } from './actions'
import { editorTabs, fileChip, selectionChip, terminalChip } from './context'
import { useAutoGrow } from './hooks'
import { AnswerMarkdown } from './Markdown'
import { aiSettings } from './settings'
import {
  addChip,
  clearConversations,
  deleteConversation,
  errorText,
  newConversation,
  removeChip,
  renameConversation,
  restoreConversation,
  restoreConversations,
  retryLast,
  selectConversation,
  sendMessage,
  setChips,
  setDraft,
  stopConversation,
  useAiStatusStore,
  useChatStore,
} from './store'
import type { AiStatus, ChatMessage, ContextChip, Conversation } from './types'

const CHIP_ICON = { terminal: SquareTerminal, selection: TextSelect, file: FileText, failure: AlertTriangle } as const

// ---------------------------------------------------------------------------------------------------------------------
// setup / unavailable
// ---------------------------------------------------------------------------------------------------------------------

function Unavailable({ status }: { status: AiStatus | null }) {
  if (status?.reason === 'disabled' && !status.canConfigureGlobal && !status.canConfigure) {
    return (
      <EmptyState
        icon={Sparkles}
        size="sm"
        title="The AI assistant is off"
        description="Your administrator has not enabled the assistant on this server."
      />
    )
  }
  const disabled = status?.reason === 'disabled'
  return (
    <div className="flex h-full flex-col items-center justify-center px-5 py-8 text-center">
      <div className="mb-3 flex size-11 items-center justify-center rounded-xl border bg-gradient-to-b from-primary/15 to-primary/5 text-primary">
        <Sparkles className="size-5" />
      </div>
      <h2 className="text-md font-semibold">{disabled ? 'The assistant is turned off' : 'Your terminal copilot'}</h2>
      <p className="mt-1.5 max-w-72 text-sm text-balance text-muted-foreground">
        Turn plain English into shell commands, get failed commands explained and fixed, and ask sysadmin questions with
        your terminal as context.
      </p>
      <ul className="mt-4 grid max-w-72 gap-2 text-left text-sm text-muted-foreground">
        <li className="flex gap-2">
          <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-success" />
          Nothing leaves NexTerm until you choose a provider — Claude, OpenAI, or a local model (Ollama, LM Studio).
        </li>
        <li className="flex gap-2">
          <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-success" />
          Keys are encrypted in the vault; passwords, private keys and tokens are redacted before anything is sent.
        </li>
        <li className="flex gap-2">
          <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-success" />
          Suggestions never run on their own — you insert or confirm every command.
        </li>
      </ul>
      <Button className="mt-5" size="sm" onClick={openSettings}>
        <Settings2 /> {disabled ? 'Open AI settings' : 'Set up the assistant'}
      </Button>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// header
// ---------------------------------------------------------------------------------------------------------------------

function Header({ conv, variant, tabId }: { conv?: Conversation; variant: 'panel' | 'tab'; tabId?: string }) {
  const conversations = useChatStore((s) => s.conversations)
  const onDelete = () => {
    if (!conv) return
    const removed = deleteConversation(conv.id)
    if (removed && removed.messages.length) toast('Chat deleted', { action: { label: 'Undo', onClick: () => restoreConversation(removed) } })
  }
  const onClearAll = () => {
    const all = clearConversations()
    if (all.length) toast('Chat history cleared', { action: { label: 'Undo', onClick: () => restoreConversations(all) } })
  }
  const onRename = async () => {
    if (!conv) return
    const t = await prompt({ title: 'Rename chat', defaultValue: conv.title, confirmLabel: 'Rename' })
    if (t) renameConversation(conv.id, t)
  }
  const toggleLocation = () => {
    if (variant === 'panel') openTab({ kind: AI_TAB, params: {} })
    else if (tabId) void closeTab(tabId).then((closed) => closed && showSidebarPanel(AI_PANEL))
  }
  return (
    <div className="flex h-9 shrink-0 items-center gap-1 border-b px-2">
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button
            type="button"
            className="flex min-w-0 flex-1 items-center gap-1 rounded-sm px-1.5 py-1 text-left text-sm font-medium hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60 focus-visible:outline-none"
            aria-label="Chat history"
          >
            <span className="truncate">{conv?.title ?? 'New chat'}</span>
            <ChevronDown className="size-3.5 shrink-0 text-muted-foreground" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-72">
          <DropdownMenuLabel className="flex items-center gap-1.5">
            <History className="size-3.5" /> Recent chats
          </DropdownMenuLabel>
          {conversations.length === 0 && <div className="px-2 py-1.5 text-sm text-muted-foreground">No chats yet</div>}
          {conversations.slice(0, 30).map((c) => (
            <DropdownMenuItem key={c.id} onSelect={() => selectConversation(c.id)} className="gap-2">
              <span className={cn('min-w-0 flex-1 truncate', c.id === conv?.id && 'font-medium')}>{c.title}</span>
              <span className="shrink-0 text-xs text-muted-foreground tabular-nums">{formatRelativeTime(c.updatedAt)}</span>
            </DropdownMenuItem>
          ))}
          {conversations.length > 0 && (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onSelect={onClearAll}>
                <Trash2 /> Clear history
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
      <IconButton icon={MessageSquarePlus} label="New chat" size="sm" onClick={() => startChat()} />
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <IconButton icon={MoreHorizontal} label="More" size="sm" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-52">
          <DropdownMenuItem onSelect={toggleLocation}>
            <PanelRight /> {variant === 'panel' ? 'Open in a tab' : 'Move to the sidebar'}
          </DropdownMenuItem>
          <DropdownMenuItem disabled={!conv} onSelect={() => void onRename()}>
            <Pencil /> Rename chat
          </DropdownMenuItem>
          <DropdownMenuItem disabled={!conv} variant="destructive" onSelect={onDelete}>
            <Trash2 /> Delete chat
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={openSettings}>
            <Settings2 /> AI settings…
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------------------------------------------------

function ContextLabels({ items }: { items: NonNullable<ChatMessage['context']> }) {
  return (
    <div className="mb-1.5 flex flex-wrap gap-1">
      {items.map((c, i) => {
        const Icon = CHIP_ICON[c.kind]
        return (
          <span key={i} className="inline-flex max-w-full items-center gap-1 rounded-sm bg-background/70 px-1.5 py-px text-xs text-muted-foreground">
            <Icon className="size-3 shrink-0" />
            <span className="truncate">{c.label}</span>
          </span>
        )
      })}
    </div>
  )
}

function UserMessage({ m }: { m: ChatMessage }) {
  return (
    <div className="group/msg ml-6 rounded-lg bg-muted/60 px-3 py-2">
      {m.context && <ContextLabels items={m.context} />}
      <div className="text-base whitespace-pre-wrap break-words">{m.content}</div>
      {!!m.redactions && (
        <div className="mt-1.5 flex items-center gap-1 text-xs text-muted-foreground" title="Replaced before sending to the provider">
          <ShieldCheck className="size-3 text-success" />
          {m.redactions === 1 ? '1 secret redacted' : `${m.redactions} secrets redacted`}
        </div>
      )}
    </div>
  )
}

/** Waiting for the first token (shown by AssistantMessage only after the delay, then for the minimum time). */
function Pending({ thinking }: { thinking: boolean }) {
  return (
    <div className="flex h-6 items-center gap-2 text-sm text-muted-foreground">
      <Spinner immediate className="size-3.5" label={thinking ? 'Thinking' : 'Waiting for the model'} />
      {thinking ? 'Thinking…' : 'Waiting for the model…'}
    </div>
  )
}

function AssistantMessage({ m, last, convId }: { m: ChatMessage; last: boolean; convId: string }) {
  const [copied, setCopied] = useState(false)
  const active = m.status === 'waiting' || m.status === 'thinking' || m.status === 'streaming'
  // The wait indicator appears only for a slow first token and, once shown, stays its minimum time (the first words
  // then replace it) — a fast answer shows no indicator at all. The row's height is reserved meanwhile.
  const waiting = active && !m.content
  const showPending = useDelayedFlag(waiting)
  const err = errorText(m)
  return (
    <div className="group/msg">
      {showPending ? (
        <Pending thinking={m.status === 'thinking'} />
      ) : m.content ? (
        <AnswerMarkdown text={m.content} streaming={active} />
      ) : waiting ? (
        <div className="h-6" aria-hidden />
      ) : null}
      {/* A steady caret while tokens arrive (live data never pulses). */}
      {m.status === 'streaming' && !showPending && <span aria-hidden className="ml-0.5 inline-block h-3.5 w-1.5 translate-y-0.5 rounded-[1px] bg-primary/60" />}
      {err && (
        <div className="mt-1 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/8 px-3 py-2 text-sm">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0 text-destructive" />
          <div className="min-w-0 flex-1">
            <div className="font-medium">{err.title}</div>
            {err.hint && <div className="mt-0.5 break-words text-muted-foreground">{err.hint}</div>}
            <div className="mt-2 flex gap-1.5">
              {last && (
                <Button size="xs" variant="secondary" onClick={() => void retryLast(convId)}>
                  <RotateCcw /> Retry
                </Button>
              )}
              {['provider_auth', 'ai_not_configured', 'provider_not_found', 'provider_unreachable', 'model_not_allowed'].includes(m.error!.code) && (
                <Button size="xs" variant="ghost" onClick={openSettings}>
                  <Settings2 /> Settings
                </Button>
              )}
            </div>
          </div>
        </div>
      )}
      {m.status === 'stopped' && (
        <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
          Stopped
          {last && (
            <button type="button" className="text-primary hover:underline" onClick={() => void retryLast(convId)}>
              Retry
            </button>
          )}
        </div>
      )}
      {m.stopReason === 'max_tokens' && <div className="mt-1 text-xs text-warning">The answer hit the length limit and may be incomplete.</div>}
      {m.stopReason === 'refusal' && <div className="mt-1 text-xs text-muted-foreground">The model declined to answer this request.</div>}
      {m.status === 'done' && m.content && (
        <div className="mt-1 flex h-6 items-center gap-0.5 text-muted-foreground opacity-0 transition-opacity duration-150 group-hover/msg:opacity-100 focus-within:opacity-100">
          <IconButton
            icon={copied ? Check : Copy}
            label={copied ? 'Copied' : 'Copy answer'}
            size="xs"
            onClick={async () => {
              if (await copyText(m.content)) {
                setCopied(true)
                setTimeout(() => setCopied(false), 1400)
              }
            }}
          />
          {last && <IconButton icon={RotateCcw} label="Regenerate" size="xs" onClick={() => void retryLast(convId)} />}
          {m.model && <span className="ml-1 truncate text-2xs">{m.model}</span>}
        </div>
      )}
    </div>
  )
}

const SUGGESTIONS_TERMINAL = [
  { label: 'Why did my last command fail?', prompt: 'Look at my terminal output. Why did the last command fail, and how do I fix it?', mode: 'explain' as const },
  { label: 'Explain the output above', prompt: 'Explain what the recent output in my terminal means.', mode: 'explain' as const },
  { label: 'What is using the most disk space here?', prompt: 'Give me commands to find what uses the most disk space on this host.', mode: 'chat' as const },
]
const SUGGESTIONS_GENERAL = [
  { label: 'Harden an SSH server', prompt: 'Give me a concise checklist with commands to harden an OpenSSH server.', mode: 'chat' as const },
  { label: 'Find what listens on port 8080', prompt: 'How do I find which process listens on port 8080 (Linux, macOS and Windows)?', mode: 'chat' as const },
  { label: 'Write a systemd service', prompt: 'Write a systemd service unit that runs a Node.js app from /srv/app with restart on failure, and the commands to enable it.', mode: 'chat' as const },
]

function Welcome({ convId, hasTerminal }: { convId: string; hasTerminal: boolean }) {
  const list = hasTerminal ? SUGGESTIONS_TERMINAL : SUGGESTIONS_GENERAL
  return (
    <div className="flex flex-1 flex-col justify-end gap-3 px-1 pb-2">
      <div className="flex items-center gap-2">
        <div className="flex size-7 items-center justify-center rounded-lg bg-primary/12 text-primary">
          <Sparkles className="size-3.5" />
        </div>
        <div>
          <div className="text-sm font-medium">How can I help?</div>
          <div className="text-xs text-muted-foreground">
            Tip: <Kbd keys="$mod+KeyI" /> in a terminal turns a sentence into a command.
          </div>
        </div>
      </div>
      <div className="grid gap-1.5">
        {list.map((s) => (
          <button
            key={s.label}
            type="button"
            onClick={() => {
              // Terminal questions need the terminal: attach the active one when the composer has none.
              let chips = useChatStore.getState().chips
              const h = getActiveTerminal()
              if (hasTerminal && h && !chips.some((c) => c.kind === 'terminal')) {
                chips = [...chips, terminalChip(h)]
                setChips(chips)
              }
              void sendMessage(convId, s.prompt, { mode: s.mode, display: s.label, chips })
            }}
            className="rounded-md border border-border/70 px-2.5 py-1.5 text-left text-sm text-foreground/85 transition-colors duration-150 hover:border-border hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60 focus-visible:outline-none"
          >
            {s.label}
          </button>
        ))}
      </div>
    </div>
  )
}

function useStickToBottom(dep: unknown) {
  const ref = useRef<HTMLDivElement>(null)
  const atBottom = useRef(true)
  const [away, setAway] = useState(false)
  useLayoutEffect(() => {
    const el = ref.current
    if (el && atBottom.current) el.scrollTop = el.scrollHeight
  }, [dep])
  const onScroll = () => {
    const el = ref.current
    if (!el) return
    const bottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
    atBottom.current = bottom
    setAway(!bottom)
  }
  const jump = () => {
    const el = ref.current
    if (!el) return
    el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
    atBottom.current = true
    setAway(false)
  }
  return { ref, onScroll, away, jump }
}

// ---------------------------------------------------------------------------------------------------------------------
// composer
// ---------------------------------------------------------------------------------------------------------------------

function ChipView({ chip }: { chip: ContextChip }) {
  const Icon = CHIP_ICON[chip.kind]
  const hint =
    chip.kind === 'terminal'
      ? `Last ${chip.lines} lines of "${chip.label}", captured when you send`
      : chip.kind === 'file'
        ? chip.path
        : chip.kind === 'failure'
          ? `${chip.command}${chip.exitCode != null ? ` (exit ${chip.exitCode})` : ''}`
          : chip.text.slice(0, 300)
  return (
    <span
      className="inline-flex h-6 max-w-full items-center gap-1 rounded-md border border-border/70 bg-background pr-0.5 pl-1.5 text-xs animate-in fade-in-0 duration-150"
      title={hint}
    >
      <Icon className={cn('size-3 shrink-0', chip.kind === 'failure' ? 'text-destructive' : 'text-muted-foreground')} />
      <span className="max-w-40 truncate">{chip.label}</span>
      {chip.kind === 'terminal' && <span className="shrink-0 text-muted-foreground tabular-nums">· {chip.lines} lines</span>}
      <button
        type="button"
        aria-label={`Remove ${chip.label}`}
        className="ml-0.5 rounded-sm p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
        onClick={() => removeChip(chip.id)}
      >
        <X className="size-3" />
      </button>
    </span>
  )
}

function AttachMenu() {
  useTerminals() // re-render when terminals come and go
  const h = getActiveTerminal()
  const selection = h?.getSelection() ?? ''
  const files = editorTabs()
  const attachFile = async (fsId: string, path: string) => {
    try {
      addChip(await fileChip(fsId, path))
    } catch (err) {
      toast.error('Cannot attach the file', { description: errorMessage(err) })
    }
  }
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <IconButton icon={Paperclip} label="Add context" size="xs" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" side="top" className="w-64">
        <DropdownMenuLabel>Add context</DropdownMenuLabel>
        <DropdownMenuItem disabled={!h} onSelect={() => h && addChip(terminalChip(h))}>
          <SquareTerminal /> Terminal output
          <span className="ml-auto truncate pl-2 text-xs text-muted-foreground">{h?.info().title}</span>
        </DropdownMenuItem>
        <DropdownMenuItem disabled={!selection.trim()} onSelect={() => addChip(selectionChip(selection, h))}>
          <TextSelect /> Terminal selection
        </DropdownMenuItem>
        {files.length > 0 && <DropdownMenuSeparator />}
        {files.slice(0, 8).map((f) => (
          <DropdownMenuItem key={f.tabId} onSelect={() => void attachFile(f.fsId, f.path)}>
            <FileText /> <span className="truncate">{f.title || f.path}</span>
          </DropdownMenuItem>
        ))}
        {files.length === 0 && (
          <div className="px-2 py-1 text-xs text-muted-foreground">Open a file in the editor to attach it.</div>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function ModelPicker({ status }: { status: AiStatus }) {
  const chosen = aiSettings.useValue('chatModel')
  if (!status.canPickModel || status.models.length < 2) return null
  const value = status.models.some((m) => m.id === chosen) ? chosen : (status.model ?? '')
  return (
    <SimpleSelect
      size="sm"
      aria-label="Model"
      className="h-6 w-auto max-w-44 gap-1 border-transparent bg-transparent px-1.5 text-xs text-muted-foreground shadow-none hover:bg-accent"
      value={value}
      onValueChange={(v) => aiSettings.set({ chatModel: v === status.model ? '' : v })}
      options={status.models.map((m) => ({ value: m.id, label: m.label || m.id }))}
    />
  )
}

function Composer({ conv, status, streaming }: { conv: Conversation; status: AiStatus; streaming: boolean }) {
  const chips = useChatStore((s) => s.chips)
  const draft = useChatStore((s) => s.drafts[conv.id] ?? '')
  const focusTick = useChatStore((s) => s.focusTick)
  const ref = useRef<HTMLTextAreaElement>(null)
  useAutoGrow(ref, draft, 180)
  // Focus when asked (commands, "new chat"), never by surprise: a panel mounting must not steal the terminal's focus.
  useEffect(() => {
    if (Date.now() - useChatStore.getState().focusAt < 1500) ref.current?.focus()
  }, [focusTick])
  useEffect(() => {
    const el = ref.current
    if (el && el.closest('[data-ai-chat]')?.contains(el.ownerDocument.activeElement)) el.focus()
  }, [conv.id])
  const send = () => {
    const text = draft.trim()
    if (!text || streaming) return
    setDraft(conv.id, '')
    void sendMessage(conv.id, text, { chips })
  }
  return (
    <div className="shrink-0 px-2 pb-2">
      <div className="rounded-lg border bg-background shadow-xs transition-[border-color,box-shadow] duration-150 focus-within:border-ring/60 focus-within:ring-2 focus-within:ring-ring/15">
        {chips.length > 0 && (
          <div className="flex flex-wrap gap-1 px-2 pt-2">
            {chips.map((c) => (
              <ChipView key={c.id} chip={c} />
            ))}
          </div>
        )}
        <textarea
          ref={ref}
          rows={1}
          value={draft}
          onChange={(e) => setDraft(conv.id, e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault()
              send()
            } else if (e.key === 'Escape' && streaming) {
              e.preventDefault()
              stopConversation(conv.id)
            }
          }}
          placeholder={conv.messages.length ? 'Ask a follow-up…' : 'Ask about your servers, errors, commands…'}
          aria-label="Message the assistant"
          className="block max-h-44 min-h-9 w-full resize-none bg-transparent px-2.5 py-2 text-base outline-none placeholder:text-muted-foreground/70"
        />
        <div className="flex h-8 items-center gap-1 px-1.5 pb-1">
          <AttachMenu />
          <ModelPicker status={status} />
          <span className="flex-1" />
          {streaming ? (
            <IconButton icon={Square} label="Stop" shortcut="Escape" size="xs" variant="secondary" onClick={() => stopConversation(conv.id)} />
          ) : (
            <IconButton icon={ArrowUp} label="Send" shortcut="Enter" size="xs" variant="default" disabled={!draft.trim()} onClick={send} />
          )}
        </div>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// view
// ---------------------------------------------------------------------------------------------------------------------

export default function ChatView({ variant = 'panel', tabId }: { variant?: 'panel' | 'tab'; tabId?: string }) {
  const status = useAiStatusStore((s) => s.status)
  const loaded = useAiStatusStore((s) => s.loaded)
  const activeId = useChatStore((s) => s.activeId)
  const conv = useChatStore((s) => s.conversations.find((c) => c.id === s.activeId))
  const terminals = useTerminals()
  const gate = useLoadingGate(!loaded)

  // Always have a conversation to type into.
  useEffect(() => {
    if (status?.available && !activeId) newConversation()
  }, [status?.available, activeId])

  const streaming = useChatStore((s) => (s.activeId ? !!s.streaming[s.activeId] : false))
  const lastContent = conv?.messages[conv.messages.length - 1]
  const scroll = useStickToBottom(`${conv?.id}:${conv?.messages.length}:${lastContent?.content.length}:${lastContent?.status}`)

  if (gate.hold) return <LoadingPane immediate active={gate.show} />
  if (!status?.available) {
    return (
      <div className="flex h-full flex-col">
        <Unavailable status={status} />
      </div>
    )
  }
  return (
    <div data-ai-chat className={cn('@container flex h-full min-h-0 flex-col', variant === 'tab' && 'mx-auto w-full max-w-3xl')}>
      <Header conv={conv} variant={variant} tabId={tabId} />
      <div className="relative min-h-0 flex-1">
        <div ref={scroll.ref} onScroll={scroll.onScroll} className="flex h-full flex-col gap-4 overflow-y-auto px-3 py-3">
          {conv && conv.messages.length === 0 && <Welcome convId={conv.id} hasTerminal={terminals.length > 0} />}
          {conv?.messages.map((m, i) =>
            m.role === 'user' ? (
              <UserMessage key={m.id} m={m} />
            ) : (
              <AssistantMessage key={m.id} m={m} convId={conv.id} last={i === conv.messages.length - 1} />
            ),
          )}
        </div>
        {scroll.away && streaming && (
          <button
            type="button"
            onClick={scroll.jump}
            className="absolute bottom-2 left-1/2 flex -translate-x-1/2 items-center gap-1 rounded-full border bg-popover px-2.5 py-1 text-xs shadow-md animate-in fade-in-0 slide-in-from-bottom-1 duration-150"
          >
            <ArrowDown className="size-3" /> Latest
          </button>
        )}
      </div>
      {conv && <Composer conv={conv} status={status} streaming={streaming} />}
    </div>
  )
}

export function AiTabView({ tabId }: { tabId: string }) {
  return <ChatView variant="tab" tabId={tabId} />
}
