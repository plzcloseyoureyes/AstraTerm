/*
 * React host for an EditorSession (Monaco). Loads the Monaco bundle on first use (in parallel with the tab reading its
 * file), creates the session once per mount and applies settings / zoom / read-only live.
 */
import { useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { useLatest } from '@/lib/hooks'
import { cn, errorMessage } from '@/lib/utils'
import type { Eol } from './codec'
import { loadedMonaco, loadMonaco, type MonacoModule } from './monaco/load'
import { EditorSession, type SessionStatus } from './monaco/session'
import { editorSettings } from './settings'
import { LoadingOverlay } from './ui'
import './editor.css'

export interface CodeEditorProps {
  tabId: string
  /** Text as read (any line endings: they are kept for untouched lines, see eol.ts). */
  initialText: string
  /** Main line ending of the document (new line breaks; default: the most frequent one). */
  eol?: Eol
  path: string
  /** Explicit language (name or Monaco id, "" = plain text); undefined = detect from path/content. */
  language?: string
  readOnly: boolean
  zoom: number
  /** Minimap for this tab (default: settings). */
  minimap?: boolean
  placeholder?: string
  onReady: (session: EditorSession | null) => void
  onDocChange?: () => void
  onFocusChange?: (focused: boolean) => void
  className?: string
  hidden?: boolean
  /** Focus the editor once it exists (new / activated tabs). */
  autoFocus?: boolean
}

/** The Monaco module: loaded (with retry after a failure), or null while loading. Applies the language settings. */
export function useMonaco(): { api: MonacoModule | null; error: unknown; retry: () => void } {
  const semantic = editorSettings.use().semanticValidation

  const [api, setApi] = useState<MonacoModule | null>(() => loadedMonaco())
  const [error, setError] = useState<unknown>(null)
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    if (api) return
    let cancelled = false
    loadMonaco().then(
      (m) => !cancelled && setApi(m),
      (err: unknown) => !cancelled && setError(err ?? new Error('The editor could not be loaded')),
    )
    return () => {
      cancelled = true
    }
  }, [api, attempt])
  useEffect(() => {
    api?.setSemanticValidation(!!semantic)
  }, [api, semantic])
  return {
    api,
    error,
    retry: () => {
      setError(null)
      setAttempt((a) => a + 1)
    },
  }
}

export function EditorLoadError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <div className="absolute inset-0 z-10 flex flex-col items-center justify-center gap-2 bg-panel p-6 text-center text-sm" role="alert">
      <div className="font-medium">The editor could not be loaded</div>
      <div className="max-w-md text-muted-foreground">{errorMessage(error)} — the app may have been updated: retry, or reload the page.</div>
      <Button size="sm" variant="secondary" onClick={onRetry}>
        <RefreshCw /> Retry
      </Button>
    </div>
  )
}

export function CodeEditor(props: CodeEditorProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const sessionRef = useRef<EditorSession | null>(null)
  const settings = editorSettings.use()
  const latest = useLatest(props)
  const prevSettings = useRef(settings)
  const { api, error, retry } = useMonaco()

  useLayoutEffect(() => {
    const host = hostRef.current
    if (!host || !api) return
    const p = latest.current
    const s = new EditorSession(
      host,
      {
        tabId: p.tabId,
        doc: p.initialText,
        eol: p.eol,
        path: p.path,
        language: p.language,
        readOnly: p.readOnly,
        settings: editorSettings.get(),
        zoom: p.zoom,
        minimap: p.minimap,
        placeholder: p.placeholder,
        onDocChange: () => latest.current.onDocChange?.(),
        onFocusChange: (f) => latest.current.onFocusChange?.(f),
      },
      api,
    )
    sessionRef.current = s
    prevSettings.current = editorSettings.get()
    p.onReady(s)
    let timer: ReturnType<typeof setTimeout> | undefined
    if (p.autoFocus) {
      // Twice: a closing dialog / palette restores focus to its trigger a moment after the tab mounted.
      const tryFocus = () => {
        if (sessionRef.current !== s || !host.isConnected || host.offsetParent === null) return
        const ae = document.activeElement
        if (!ae || ae === document.body || !ae.closest('[role=dialog],[role=menu],input,textarea,[contenteditable=true]')) s.focus()
      }
      requestAnimationFrame(tryFocus)
      timer = setTimeout(tryFocus, 350)
    }
    return () => {
      if (timer) clearTimeout(timer)
      latest.current.onReady(null)
      s.destroy()
      sessionRef.current = null
    }
    // The session is created once per mount (and Monaco load); later prop changes are applied by the effects below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api])

  useEffect(() => {
    const s = sessionRef.current
    if (!s || prevSettings.current === settings) return
    s.applySettings(settings, prevSettings.current)
    prevSettings.current = settings
  }, [settings])

  useEffect(() => {
    sessionRef.current?.setZoom(props.zoom)
  }, [props.zoom])

  useEffect(() => {
    const s = sessionRef.current
    if (s && s.getStatus().readOnly !== props.readOnly) s.setReadOnly(props.readOnly)
  }, [props.readOnly])

  return (
    <div className={cn('nx-editor relative min-h-0 flex-1 overflow-hidden', props.hidden && 'hidden', props.className)}>
      {/* Keyboard owner (app/keybindings.ts): Monaco keeps its keys; monaco/keys.ts, installed here, adds the editor's. */}
      <div ref={hostRef} className="absolute inset-0" data-keyboard-owner="editor" />
      <LoadingOverlay active={!api && !error} label="Loading the editor…" />
      {!!error && !api && <EditorLoadError error={error} onRetry={retry} />}
    </div>
  )
}

const noopSubscribe = () => () => undefined
const nullStatus = () => null

/** Live status snapshot of a session (null while there is none). */
export function useSessionStatus(session: EditorSession | null): SessionStatus | null {
  return useSyncExternalStore(session ? session.subscribe : noopSubscribe, session ? session.getStatus : nullStatus, session ? session.getStatus : nullStatus)
}

/** Vim's mode line and ":" command line, shown in the tab's status bar while vim mode is on. */
export function VimStatus({ session, active }: { session: EditorSession | null; active: boolean }) {
  const slot = useRef<HTMLSpanElement>(null)
  useLayoutEffect(() => {
    const el = slot.current
    const node = session?.vimStatusNode
    if (!el || !node || !active) return
    el.appendChild(node)
    return () => {
      if (node.parentNode === el) el.removeChild(node)
    }
  }, [session, active])
  if (!active) return null
  return <span ref={slot} className="inline-flex h-5 max-w-[45%] min-w-[11ch] shrink-0 items-center overflow-hidden px-1.5" aria-label="Vim status" />
}
