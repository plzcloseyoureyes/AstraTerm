/*
 * Prompt broker client side (SPEC §4 "Prompt broker", §6.1): prompts pushed over /ws/events are queued here and
 * rendered one at a time by <PromptHost/>; answering sends `prompt.response`.
 */
import { create } from 'zustand'
import { toast } from 'sonner'
import type { Prompt } from '@/api/types'
import { events } from '@/lib/events'

interface PromptsStore {
  queue: Prompt[]
}

export const usePromptsStore = create<PromptsStore>(() => ({ queue: [] }))

export function pushPrompt(p: Prompt): void {
  if (!p || typeof p.id !== 'string') return
  usePromptsStore.setState((s) => {
    const i = s.queue.findIndex((x) => x.id === p.id)
    if (i >= 0) {
      const queue = s.queue.slice()
      queue[i] = p
      return { queue }
    }
    return { queue: [...s.queue, p] }
  })
}

export function removePrompt(id: string): void {
  usePromptsStore.setState((s) => ({ queue: s.queue.filter((p) => p.id !== id) }))
}

export interface PromptAnswer {
  accept: boolean
  values?: string[]
  save?: boolean
}

/** Answer a prompt. Returns false (and keeps it queued) when the events socket is not connected. */
export function answerPrompt(id: string, answer: PromptAnswer): boolean {
  const ok = events.send({ type: 'prompt.response', id, accept: answer.accept, values: answer.values, save: answer.save })
  if (!ok) {
    toast.error('Not connected to the server', { description: 'The answer could not be sent. Reconnecting…' })
    events.reconnectNow()
    return false
  }
  removePrompt(id)
  return true
}

export const usePromptQueue = () => usePromptsStore((s) => s.queue)
