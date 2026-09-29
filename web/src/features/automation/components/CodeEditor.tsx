/*
 * CodeMirror 6 editor for scripts (lazy chunk: import this module with React.lazy). JavaScript highlighting comes from
 * @codemirror/language-data; completions list the automation script API.
 */
import { useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import CodeMirror, { EditorState, EditorView, drawSelection, keymap, type Extension } from '@uiw/react-codemirror'
import { LanguageDescription, type LanguageSupport } from '@codemirror/language'
import { languages } from '@codemirror/language-data'
import type { CompletionContext, CompletionResult } from '@codemirror/autocomplete'
import { isDarkTheme, onThemeChange } from '@/lib/theme'

const API: { label: string; detail: string; type: string; apply?: string }[] = [
  { label: 'session.send', detail: '(text) type raw text', type: 'function', apply: 'session.send("")' },
  { label: 'session.sendLine', detail: '(text) text + Enter', type: 'function', apply: 'session.sendLine("")' },
  { label: 'session.sendSecret', detail: '(key, {enter}) type a stored secret', type: 'function', apply: 'session.sendSecret("password")' },
  { label: 'session.expect', detail: '(pattern|[patterns], timeoutMs) → {index, match, groups, before}', type: 'function', apply: 'session.expect(/\\$ $/, 30000)' },
  { label: 'session.waitFor', detail: '(pattern, timeoutMs) → match or null', type: 'function', apply: 'session.waitFor(//, 10000)' },
  { label: 'session.waitIdle', detail: '(idleMs, maxMs)', type: 'function', apply: 'session.waitIdle(500)' },
  { label: 'session.waitPrompt', detail: '(timeoutMs, pattern?) → boolean', type: 'function', apply: 'session.waitPrompt(30000)' },
  { label: 'session.run', detail: '(command, {timeout, prompt}) → output', type: 'function', apply: 'session.run("")' },
  { label: 'session.screen', detail: '(lines) → recent output', type: 'function', apply: 'session.screen(24)' },
  { label: 'session.exec', detail: '(command, timeoutMs) → {stdout, stderr, code} (SSH)', type: 'function', apply: 'session.exec("")' },
  { label: 'session.close', detail: '() close the session', type: 'function', apply: 'session.close()' },
  { label: 'sessions.open', detail: '(connectionIdOrName, {timeout, keepOpen}) → session', type: 'function', apply: 'sessions.open("")' },
  { label: 'sessions.get', detail: '(sessionId) → session', type: 'function', apply: 'sessions.get("")' },
  { label: 'sessions.list', detail: '() → running sessions', type: 'function', apply: 'sessions.list()' },
  { label: 'connections.list', detail: '() → saved connections', type: 'function', apply: 'connections.list()' },
  { label: 'log', detail: '(...values)', type: 'function', apply: 'log()' },
  { label: 'sleep', detail: '(ms)', type: 'function', apply: 'sleep(1000)' },
  { label: 'prompt', detail: '(label, {secret, default}) → string|null', type: 'function', apply: 'prompt("")' },
  { label: 'confirm', detail: '(message) → boolean', type: 'function', apply: 'confirm("")' },
  { label: 'exit', detail: '(code)', type: 'function', apply: 'exit(0)' },
  { label: 'vars', detail: 'run variables', type: 'variable' },
]

function apiCompletions(ctx: CompletionContext): CompletionResult | null {
  const word = ctx.matchBefore(/[\w.]+/)
  if (!word || (word.from === word.to && !ctx.explicit)) return null
  return { from: word.from, options: API.map((a) => ({ label: a.label, detail: a.detail, type: a.type, apply: a.apply })), validFor: /^[\w.]*$/ }
}

const subscribeTheme = (cb: () => void) => onThemeChange(() => cb())

export default function CodeEditor({
  value,
  onChange,
  onSave,
  language = 'javascript',
  readOnly,
  className,
  ariaLabel = 'Script editor',
}: {
  value: string
  onChange: (v: string) => void
  onSave?: () => void
  language?: string
  readOnly?: boolean
  className?: string
  ariaLabel?: string
}) {
  const dark = useSyncExternalStore(subscribeTheme, isDarkTheme, isDarkTheme)
  const [lang, setLang] = useState<LanguageSupport | null>(null)
  useEffect(() => {
    let alive = true
    const desc = LanguageDescription.matchLanguageName(languages, language, true)
    if (desc) {
      void desc
        .load()
        .then((l) => alive && setLang(l))
        .catch(() => undefined)
    }
    return () => {
      alive = false
    }
  }, [language])
  const extensions = useMemo<Extension[]>(() => {
    const ext: Extension[] = [
      // Steady caret (docs/UX.md: nothing blinks). basicSetup's drawSelection is disabled below and replaced by this.
      drawSelection({ cursorBlinkRate: 0 }),
      EditorView.lineWrapping,
      EditorView.contentAttributes.of({ 'aria-label': ariaLabel }),
      EditorState.languageData.of(() => [{ autocomplete: apiCompletions }]),
    ]
    if (lang) ext.push(lang)
    if (onSave) {
      ext.push(
        keymap.of([
          {
            key: 'Mod-s',
            preventDefault: true,
            run: () => {
              onSave()
              return true
            },
          },
        ]),
      )
    }
    return ext
  }, [lang, onSave, ariaLabel])
  return (
    <CodeMirror
      className={className}
      value={value}
      onChange={onChange}
      height="100%"
      theme={dark ? 'dark' : 'light'}
      readOnly={readOnly}
      extensions={extensions}
      basicSetup={{ foldGutter: true, highlightActiveLine: true, autocompletion: true, tabSize: 2, drawSelection: false }}
    />
  )
}
