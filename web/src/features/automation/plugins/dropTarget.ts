/*
 * Direct manipulation (docs/UX.md): drag a snippet or a macro from the sidebar onto any terminal to run it there —
 * also a terminal that is not the active one (split panes). Only Termstead's own drag types are handled; file drops
 * (upload to the session) are left to the file-transfer plugin.
 */
import type { Terminal } from '@xterm/xterm'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import type { Snippet } from '@/api/types'
import type { TerminalPluginContext } from '@/app/registry'
import { autoKeys } from '../api'
import { playMacroOnSessions } from '../macros/play'
import { sendSnippet, targetForSession } from '../send'
import type { Macro } from '../types'

export const SNIPPET_MIME = 'application/x-termstead-snippet'
export const MACRO_MIME = 'application/x-termstead-macro'

/** Start dragging a snippet / macro (sidebar rows). */
export function startLibraryDrag(e: React.DragEvent, kind: 'snippet' | 'macro', id: string, label: string): void {
  e.dataTransfer.setData(kind === 'snippet' ? SNIPPET_MIME : MACRO_MIME, id)
  e.dataTransfer.setData('text/plain', label)
  e.dataTransfer.effectAllowed = 'copy'
}

function dragKind(dt: DataTransfer | null): 'snippet' | 'macro' | null {
  if (!dt) return null
  if (dt.types.includes(SNIPPET_MIME)) return 'snippet'
  if (dt.types.includes(MACRO_MIME)) return 'macro'
  return null
}

export function setupDropTarget(term: Terminal, ctx: TerminalPluginContext): () => void {
  const host = term.element
  if (!host) return () => {}
  let overlay: HTMLDivElement | null = null
  let depth = 0

  const show = (kind: 'snippet' | 'macro') => {
    if (!overlay) {
      overlay = document.createElement('div')
      overlay.className =
        'pointer-events-none absolute inset-0 z-30 flex items-center justify-center rounded-sm bg-primary/5 ring-2 ring-primary/60 ring-inset ' +
        'opacity-0 transition-opacity duration-150 ease-out'
      const pill = document.createElement('span')
      pill.className = 'rounded-md border border-primary/40 bg-popover/95 px-2 py-1 font-sans text-xs text-popover-foreground shadow-popover'
      overlay.appendChild(pill)
      host.appendChild(overlay)
      requestAnimationFrame(() => overlay && (overlay.style.opacity = '1'))
    }
    const title = ctx.session()?.title ?? 'this session'
    ;(overlay.firstChild as HTMLElement).textContent = kind === 'snippet' ? `Drop to run the snippet in ${title}` : `Drop to play the macro in ${title}`
  }
  const hide = () => {
    depth = 0
    overlay?.remove()
    overlay = null
  }

  const onEnter = (e: DragEvent) => {
    const kind = dragKind(e.dataTransfer)
    if (!kind) return
    e.preventDefault()
    depth++
    show(kind)
  }
  const onOver = (e: DragEvent) => {
    if (!dragKind(e.dataTransfer)) return
    e.preventDefault()
    e.dataTransfer!.dropEffect = 'copy'
  }
  const onLeave = (e: DragEvent) => {
    if (!dragKind(e.dataTransfer)) return
    depth = Math.max(0, depth - 1)
    if (depth === 0) hide()
  }
  const onDrop = (e: DragEvent) => {
    const kind = dragKind(e.dataTransfer)
    if (!kind) return
    e.preventDefault()
    e.stopPropagation()
    const id = e.dataTransfer!.getData(kind === 'snippet' ? SNIPPET_MIME : MACRO_MIME)
    hide()
    const title = ctx.session()?.title ?? ctx.sessionId
    if (kind === 'snippet') {
      const s = queryClient.getQueryData<Snippet[]>(autoKeys.snippets)?.find((x) => x.id === id)
      if (!s) return void toast.error('That snippet no longer exists')
      void sendSnippet(s, [targetForSession(ctx.sessionId)])
    } else {
      const m = queryClient.getQueryData<Macro[]>(autoKeys.macros)?.find((x) => x.id === id)
      if (!m) return void toast.error('That macro no longer exists')
      void playMacroOnSessions(m, [ctx.sessionId], [title])
    }
    term.focus()
  }

  host.addEventListener('dragenter', onEnter)
  host.addEventListener('dragover', onOver)
  host.addEventListener('dragleave', onLeave)
  host.addEventListener('drop', onDrop, true)
  window.addEventListener('dragend', hide)
  return () => {
    host.removeEventListener('dragenter', onEnter)
    host.removeEventListener('dragover', onOver)
    host.removeEventListener('dragleave', onLeave)
    host.removeEventListener('drop', onDrop, true)
    window.removeEventListener('dragend', hide)
    hide()
  }
}
