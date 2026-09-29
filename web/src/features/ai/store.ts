/*
 * AI assistant state:
 *   - status (GET /api/ai/status), refreshed on sign-in, `ai.status` events (config changes) and vault events
 *   - conversations: kept locally in the browser (localStorage, namespaced by user id), never on the server
 *   - streaming: one in-flight answer per conversation, deltas batched per frame (calm rendering, no flicker)
 */
import { create } from 'zustand'
import { isApiError } from '@/api/client'
import { events } from '@/lib/events'
import { errorMessage, storage, uid } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { describeAiError, getStatus, streamChat } from './api'
import { buildContext, chipLabelList } from './context'
import { aiSettings } from './settings'
import type { AiMode, AiStatus, ChatMessage, ContextChip, Conversation, WireMessage } from './types'

// ---------------------------------------------------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------------------------------------------------

interface StatusStore {
  status: AiStatus | null
  loaded: boolean
  error?: string
}

export const useAiStatusStore = create<StatusStore>(() => ({ status: null, loaded: false }))

let statusSeq = 0

export async function refreshAiStatus(): Promise<AiStatus | null> {
  const seq = ++statusSeq
  try {
    const s = await getStatus()
    if (seq === statusSeq) useAiStatusStore.setState({ status: s, loaded: true, error: undefined })
    return s
  } catch (err) {
    if (seq === statusSeq) useAiStatusStore.setState({ loaded: true, error: errorMessage(err) })
    return null
  }
}

export function isAiAvailable(): boolean {
  return !!useAiStatusStore.getState().status?.available
}

// ---------------------------------------------------------------------------------------------------------------------
// conversations
// ---------------------------------------------------------------------------------------------------------------------

const MAX_CONVERSATIONS = 60
const MAX_MESSAGES = 200
const MAX_STORED_CHARS = 1_500_000

interface ChatStore {
  userId: string | null
  conversations: Conversation[]
  activeId: string | null
  /** Composer context chips (apply to the next message of the active conversation). */
  chips: ContextChip[]
  /** Unsent composer text per conversation (drafts survive panel switches). */
  drafts: Record<string, string>
  /** Focus request counter for the composer (commands bump it) and when it was last bumped. */
  focusTick: number
  focusAt: number
  /** Conversations with an answer in flight. */
  streaming: Record<string, true>
}

export const useChatStore = create<ChatStore>(() => ({
  userId: null,
  conversations: [],
  activeId: null,
  chips: [],
  drafts: {},
  focusTick: 0,
  focusAt: 0,
  streaming: {},
}))

const storageKey = (userId: string) => `termstead:ai:conversations:v1:${userId}`

function load(userId: string): void {
  const saved = storage.get<{ conversations?: Conversation[]; activeId?: string | null }>(storageKey(userId), {})
  const conversations = Array.isArray(saved.conversations)
    ? saved.conversations
        .filter((c) => c && typeof c.id === 'string' && Array.isArray(c.messages))
        .map((c) => ({
          ...c,
          // An answer that was streaming when the page went away is shown as stopped.
          messages: c.messages.map((m) =>
            m.status === 'waiting' || m.status === 'thinking' || m.status === 'streaming' ? { ...m, status: 'stopped' as const } : m,
          ),
        }))
    : []
  const activeId = conversations.some((c) => c.id === saved.activeId) ? (saved.activeId ?? null) : (conversations[0]?.id ?? null)
  useChatStore.setState({ userId, conversations, activeId, chips: [], drafts: {}, streaming: {} })
}

let saveTimer: ReturnType<typeof setTimeout> | null = null

function scheduleSave(): void {
  if (saveTimer) clearTimeout(saveTimer)
  saveTimer = setTimeout(saveNow, 600)
}

function saveNow(): void {
  saveTimer = null
  const { userId, conversations, activeId } = useChatStore.getState()
  if (!userId) return
  let list = conversations.slice(0, MAX_CONVERSATIONS).map((c) => ({ ...c, messages: c.messages.slice(-MAX_MESSAGES) }))
  // Keep the stored history bounded: drop the oldest conversations first.
  let size = JSON.stringify(list).length
  while (size > MAX_STORED_CHARS && list.length > 1) {
    list = list.slice(0, -1)
    size = JSON.stringify(list).length
  }
  storage.set(storageKey(userId), { conversations: list, activeId })
}

function patchConversation(id: string, fn: (c: Conversation) => Conversation): void {
  useChatStore.setState((s) => ({ conversations: s.conversations.map((c) => (c.id === id ? fn(c) : c)) }))
  scheduleSave()
}

function patchMessage(convId: string, msgId: string, patch: Partial<ChatMessage> | ((m: ChatMessage) => Partial<ChatMessage>)): void {
  patchConversation(convId, (c) => ({
    ...c,
    updatedAt: Date.now(),
    messages: c.messages.map((m) => (m.id === msgId ? { ...m, ...(typeof patch === 'function' ? patch(m) : patch) } : m)),
  }))
}

export function getConversation(id: string | null | undefined): Conversation | undefined {
  return id ? useChatStore.getState().conversations.find((c) => c.id === id) : undefined
}

export function newConversation(): string {
  const now = Date.now()
  const cur = getConversation(useChatStore.getState().activeId)
  // Reuse an untouched empty conversation instead of piling up blank ones.
  if (cur && cur.messages.length === 0) return cur.id
  const conv: Conversation = { id: uid('c'), title: 'New chat', createdAt: now, updatedAt: now, messages: [] }
  useChatStore.setState((s) => ({ conversations: [conv, ...s.conversations], activeId: conv.id }))
  scheduleSave()
  return conv.id
}

export function selectConversation(id: string): void {
  useChatStore.setState({ activeId: id })
  scheduleSave()
}

export function deleteConversation(id: string): Conversation | undefined {
  const conv = getConversation(id)
  stopConversation(id)
  useChatStore.setState((s) => {
    const conversations = s.conversations.filter((c) => c.id !== id)
    return { conversations, activeId: s.activeId === id ? (conversations[0]?.id ?? null) : s.activeId }
  })
  scheduleSave()
  return conv
}

/** Undo for deleteConversation. */
export function restoreConversation(conv: Conversation): void {
  useChatStore.setState((s) => ({
    conversations: [conv, ...s.conversations.filter((c) => c.id !== conv.id)].sort((a, b) => b.updatedAt - a.updatedAt),
    activeId: conv.id,
  }))
  scheduleSave()
}

export function clearConversations(): Conversation[] {
  const all = useChatStore.getState().conversations
  for (const c of all) stopConversation(c.id)
  useChatStore.setState({ conversations: [], activeId: null })
  scheduleSave()
  return all
}

export function restoreConversations(list: Conversation[]): void {
  useChatStore.setState({ conversations: list, activeId: list[0]?.id ?? null })
  scheduleSave()
}

export function renameConversation(id: string, title: string): void {
  patchConversation(id, (c) => ({ ...c, title: title.trim().slice(0, 120) || c.title }))
}

export function setDraft(convId: string, text: string): void {
  useChatStore.setState((s) => ({ drafts: { ...s.drafts, [convId]: text } }))
}

export function focusComposer(): void {
  useChatStore.setState((s) => ({ focusTick: s.focusTick + 1, focusAt: Date.now() }))
}

// --- chips ----------------------------------------------------------------------------------------------------------

export function setChips(chips: ContextChip[]): void {
  useChatStore.setState({ chips })
}

export function addChip(chip: ContextChip): void {
  useChatStore.setState((s) => {
    // One chip per terminal / file; selections and failures replace previous ones of their kind.
    const rest = s.chips.filter((c) => {
      if (c.kind !== chip.kind) return true
      if (chip.kind === 'terminal' && c.kind === 'terminal') return c.tabId !== chip.tabId
      if (chip.kind === 'file' && c.kind === 'file') return c.path !== chip.path
      return false
    })
    return { chips: [...rest, chip] }
  })
}

export function removeChip(id: string): void {
  useChatStore.setState((s) => ({ chips: s.chips.filter((c) => c.id !== id) }))
}

// --- streaming ---------------------------------------------------------------------------------------------------

const controllers = new Map<string, AbortController>()
const pendingText = new Map<string, { convId: string; text: string }>()
let flushTimer: ReturnType<typeof setTimeout> | null = null

function flushDeltas(): void {
  flushTimer = null
  if (!pendingText.size) return
  const batch = Array.from(pendingText.entries())
  pendingText.clear()
  useChatStore.setState((s) => ({
    conversations: s.conversations.map((c) => {
      const mine = batch.filter(([, v]) => v.convId === c.id)
      if (!mine.length) return c
      return {
        ...c,
        messages: c.messages.map((m) => {
          const hit = mine.find(([id]) => id === m.id)
          return hit ? { ...m, content: m.content + hit[1].text, status: 'streaming' as const } : m
        }),
      }
    }),
  }))
}

function queueDelta(convId: string, msgId: string, text: string): void {
  const cur = pendingText.get(msgId)
  pendingText.set(msgId, { convId, text: (cur?.text ?? '') + text })
  // ~30 fps: smooth typing without re-rendering the markdown for every token.
  if (!flushTimer) flushTimer = setTimeout(flushDeltas, 33)
}

export function stopConversation(convId: string): void {
  controllers.get(convId)?.abort()
}

function titleFrom(text: string): string {
  const t = text.replace(/\s+/g, ' ').trim()
  return t.length > 60 ? `${t.slice(0, 57)}…` : t || 'New chat'
}

export interface SendOptions {
  mode?: AiMode
  chips?: ContextChip[]
  /** Text shown in the conversation instead of the prompt sent (e.g. "Explain this error"). */
  display?: string
}

/** Wire history of a conversation (completed turns only). */
function history(conv: Conversation): WireMessage[] {
  const out: WireMessage[] = []
  for (const m of conv.messages) {
    if (m.role === 'assistant' && (m.status === 'error' || !m.content.trim())) {
      // Drop failed answers together with the question that produced them? Keep the question: the user may retry.
      continue
    }
    out.push({ role: m.role, content: m.role === 'user' && m.prompt ? m.prompt : m.content })
  }
  return out
}

/** Context chips a user message was sent with (in memory only: retrying re-captures the same sources). */
const sentChips = new Map<string, ContextChip[]>()

/** Send a user message in a conversation and stream the answer. */
export async function sendMessage(convId: string, text: string, opts: SendOptions = {}): Promise<void> {
  const conv = getConversation(convId)
  const content = text.trim()
  if (!conv || !content || controllers.has(convId)) return
  const chips = opts.chips ?? useChatStore.getState().chips
  const mode = opts.mode ?? 'chat'
  const now = Date.now()
  const userMsg: ChatMessage = {
    id: uid('m'),
    role: 'user',
    content: opts.display ?? content,
    prompt: opts.display && opts.display !== content ? content : undefined,
    createdAt: now,
    mode,
    context: chipLabelList(chips),
  }
  const answer: ChatMessage = { id: uid('m'), role: 'assistant', content: '', createdAt: now, status: 'waiting' }
  const wire = [...history(conv), { role: 'user' as const, content }]
  patchConversation(convId, (c) => ({
    ...c,
    title: c.messages.length === 0 ? titleFrom(opts.display ?? content) : c.title,
    updatedAt: now,
    messages: [...c.messages, userMsg, answer],
  }))
  // Move the conversation to the top of the history.
  useChatStore.setState((s) => {
    const c = s.conversations.find((x) => x.id === convId)
    return c ? { conversations: [c, ...s.conversations.filter((x) => x.id !== convId)] } : s
  })
  sentChips.set(userMsg.id, chips)
  if (sentChips.size > 200) sentChips.delete(sentChips.keys().next().value!)
  await runStream(convId, userMsg.id, answer.id, wire, chips, mode)
}

async function runStream(convId: string, userMsgId: string, answerId: string, wire: WireMessage[], chips: ContextChip[], mode: AiMode) {
  const ctrl = new AbortController()
  controllers.set(convId, ctrl)
  useChatStore.setState((s) => ({ streaming: { ...s.streaming, [convId]: true } }))
  const model = aiSettings.get().chatModel || undefined
  try {
    const context = await buildContext(chips)
    await streamChat(
      { messages: wire, context, mode, model },
      (ev) => {
        switch (ev.type) {
          case 'meta':
            patchMessage(convId, userMsgId, { redactions: ev.data.redactions })
            patchMessage(convId, answerId, { model: ev.data.model })
            break
          case 'thinking':
            patchMessage(convId, answerId, (m) => (m.status === 'waiting' ? { status: 'thinking' } : {}))
            break
          case 'delta':
            queueDelta(convId, answerId, ev.text)
            break
          case 'done':
            flushDeltas()
            patchMessage(convId, answerId, { status: 'done', stopReason: ev.stopReason })
            break
          case 'error':
            flushDeltas()
            patchMessage(convId, answerId, { status: 'error', error: { message: ev.error, code: ev.code } })
            break
        }
      },
      ctrl.signal,
    )
  } catch (err) {
    flushDeltas()
    if (err instanceof DOMException && err.name === 'AbortError') {
      patchMessage(convId, answerId, { status: 'stopped' })
    } else {
      const code = isApiError(err) ? err.code : 'error'
      const message = errorMessage(err)
      patchMessage(convId, answerId, { status: 'error', error: { message, code } })
      if (code === 'ai_not_configured' || code === 'ai_disabled') void refreshAiStatus()
    }
  } finally {
    controllers.delete(convId)
    useChatStore.setState((s) => {
      const streaming = { ...s.streaming }
      delete streaming[convId]
      return { streaming }
    })
    scheduleSave()
  }
}

/** Retry the last question of a conversation (after an error or a stop). */
export async function retryLast(convId: string): Promise<void> {
  const conv = getConversation(convId)
  if (!conv || controllers.has(convId)) return
  const lastUser = [...conv.messages].reverse().find((m) => m.role === 'user')
  if (!lastUser) return
  const idx = conv.messages.indexOf(lastUser)
  const answer: ChatMessage = { id: uid('m'), role: 'assistant', content: '', createdAt: Date.now(), status: 'waiting' }
  const kept = conv.messages.slice(0, idx + 1)
  patchConversation(convId, (c) => ({ ...c, messages: [...kept, answer] }))
  const wire = history({ ...conv, messages: kept })
  // Retry with the context the question was asked with (re-captured now), else whatever the composer holds.
  const chips = sentChips.get(lastUser.id) ?? useChatStore.getState().chips
  await runStream(convId, lastUser.id, answer.id, wire, chips, lastUser.mode ?? 'chat')
}

export function errorText(m: ChatMessage): { title: string; hint?: string } | null {
  return m.error ? describeAiError(m.error.code, m.error.message) : null
}

// ---------------------------------------------------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------------------------------------------------

let installed = false

export function installAiState(): void {
  if (installed) return
  installed = true
  let lastUser: string | null = null
  const sync = () => {
    const { status, user } = useAuthStore.getState()
    const id = status === 'authenticated' && user ? user.id : null
    if (id === lastUser) return
    if (saveTimer) saveNow()
    lastUser = id
    if (id) {
      load(id)
      void refreshAiStatus()
    } else {
      for (const c of controllers.values()) c.abort()
      useChatStore.setState({ userId: null, conversations: [], activeId: null, chips: [], drafts: {}, streaming: {} })
      useAiStatusStore.setState({ status: null, loaded: false })
    }
  }
  useAuthStore.subscribe(sync)
  sync()
  events.onAny((ev) => {
    const type = (ev as { type: string }).type
    if (type === 'ai.status' || type === 'vault') void refreshAiStatus()
  })
  window.addEventListener('beforeunload', () => {
    if (saveTimer) saveNow()
  })
}
