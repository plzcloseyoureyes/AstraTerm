/*
 * "Send file to session" (CC-8): a local file typed into the terminal — text line by line (per-line / per-character
 * delay, optional wait for the prompt; the server-side pacer when available) or raw binary in paced chunks.
 */
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { Binary, FileText, FileUp, Send, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { cn, errorMessage, formatBytes } from '@/lib/utils'
import { useTerminalInfoStore } from '@/features/terminal/bus'
import { getController } from '../instances'
import { compilePromptPattern, DEFAULT_PROMPT_PATTERN, serialPacing } from '../engine/pacer'
import { closeSendDialog, useTransferStore } from '../store'
import { currentSettings, EOL, LIMITS, transferSettings, type LineEnding, type SendMode } from '../settings'

const EOL_OPTIONS: { value: LineEnding; label: string }[] = [
  { value: 'cr', label: 'Enter (CR)' },
  { value: 'crlf', label: 'CR LF' },
  { value: 'lf', label: 'LF' },
]

export function SendFileDialog() {
  const state = useTransferStore((s) => s.sendDialog)
  const info = useTerminalInfoStore((s) => (state.tabId ? s.infos[state.tabId] : undefined))
  const open = state.open && !!state.tabId
  return (
    <Dialog open={open} onOpenChange={(o) => !o && closeSendDialog()}>
      {open && <SendFileForm key={state.tabId} tabId={state.tabId!} initialFile={state.file} title={info?.title ?? 'terminal'} />}
    </Dialog>
  )
}

function SendFileForm({ tabId, initialFile, title }: { tabId: string; initialFile?: File; title: string }) {
  const s0 = useMemo(() => currentSettings(), [])
  const ctrl = getController(tabId)
  const serialBaud = useMemo(() => ctrl?.serialBaud() ?? null, [ctrl])
  const [file, setFile] = useState<File | null>(initialFile ?? null)
  const [mode, setMode] = useState<SendMode>(s0.sendMode)
  const [lineDelay, setLineDelay] = useState(s0.lineDelayMs)
  const [charDelay, setCharDelay] = useState(s0.charDelayMs)
  const [waitPrompt, setWaitPrompt] = useState(s0.waitPrompt)
  const [pattern, setPattern] = useState(s0.promptPattern)
  const [promptTimeout, setPromptTimeout] = useState(Math.round(s0.promptTimeoutMs / 1000))
  const [eol, setEol] = useState<LineEnding>(s0.lineEnding)
  const [chunk, setChunk] = useState(s0.binaryChunkBytes)
  const [delay, setDelay] = useState(s0.binaryDelayMs)
  const [busy, setBusy] = useState(false)
  const [dragOver, setDragOver] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  const ids = { file: useId(), wait: useId(), pattern: useId() }

  const patternError = useMemo(() => {
    if (!waitPrompt || !pattern.trim()) return null
    try {
      compilePromptPattern(pattern)
      return null
    } catch (err) {
      return errorMessage(err)
    }
  }, [waitPrompt, pattern])

  const binaryProblem = mode === 'binary' ? ctrl?.binaryProblem() : null
  const connected = !!ctrl?.isConnected()
  const readOnly = !!ctrl?.readOnly
  const canSend = !!file && !busy && connected && !readOnly && !patternError && !binaryProblem && !!ctrl && !ctrl.busy

  // Rough duration estimate (text: unknown line count until read — shown for binary only).
  const binaryEta = mode === 'binary' && file && chunk > 0 ? Math.ceil(file.size / chunk) * delay : null

  useEffect(() => {
    if (!initialFile) inputRef.current?.focus()
  }, [initialFile])

  const submit = async () => {
    if (!ctrl || !file || !canSend) return
    transferSettings.set({
      sendMode: mode,
      lineDelayMs: lineDelay,
      charDelayMs: charDelay,
      waitPrompt,
      promptPattern: pattern,
      promptTimeoutMs: promptTimeout * 1000,
      lineEnding: eol,
      binaryChunkBytes: chunk,
      binaryDelayMs: delay,
    })
    setBusy(true)
    try {
      const run = ctrl.sendFile(file, {
        mode,
        lineDelayMs: lineDelay,
        charDelayMs: charDelay,
        waitPrompt,
        promptPattern: pattern,
        promptTimeoutMs: promptTimeout * 1000,
        eol: EOL[eol],
        chunkBytes: chunk,
        delayMs: delay,
      })
      closeSendDialog()
      await run
    } catch (err) {
      toast.error(`Cannot send ${file.name}`, { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }

  const applySerial = () => {
    const baud = Number(serialBaud)
    if (!baud) return
    const p = serialPacing(baud)
    setChunk(p.chunkBytes)
    setDelay(p.delayMs)
  }

  return (
    <DialogContent size="lg" onOpenAutoFocus={(e) => e.preventDefault()}>
      <DialogHeader>
        <DialogTitle>
          <Send className="size-4 text-muted-foreground" /> Send file to {title}
        </DialogTitle>
        <DialogDescription>Types a local file into the session — text line by line, or raw bytes.</DialogDescription>
      </DialogHeader>
      <DialogBody className="grid gap-4">
        <div
          className={cn(
            'flex items-center gap-3 rounded-md border border-dashed p-3 transition-colors',
            dragOver ? 'border-primary bg-primary/10' : 'border-border',
          )}
          onDragOver={(e) => {
            if (!Array.from(e.dataTransfer.types).includes('Files')) return
            e.preventDefault()
            setDragOver(true)
          }}
          onDragLeave={() => setDragOver(false)}
          onDrop={(e) => {
            e.preventDefault()
            setDragOver(false)
            const f = e.dataTransfer.files?.[0]
            if (f) setFile(f)
          }}
        >
          <FileUp className="size-5 shrink-0 text-muted-foreground" aria-hidden />
          <div className="grid min-w-0 flex-1">
            <span className="truncate text-base font-medium">{file ? file.name : 'No file chosen'}</span>
            <span className="text-xs text-muted-foreground">{file ? formatBytes(file.size) : 'Choose a file or drop it here'}</span>
          </div>
          <input
            ref={inputRef}
            id={ids.file}
            type="file"
            className="sr-only"
            onChange={(e) => {
              const f = e.target.files?.[0]
              if (f) setFile(f)
              e.target.value = ''
            }}
          />
          <Button variant="secondary" size="sm" onClick={() => inputRef.current?.click()}>
            {file ? 'Change…' : 'Choose file…'}
          </Button>
        </div>

        <SegmentedControl<SendMode>
          aria-label="Mode"
          value={mode}
          onValueChange={setMode}
          fullWidth
          options={[
            { value: 'text', label: 'Text, line by line', icon: FileText },
            { value: 'binary', label: 'Binary (raw bytes)', icon: Binary },
          ]}
        />

        {mode === 'text' ? (
          <div className="grid gap-3">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <Field label="Delay per line">
                <NumberInput value={lineDelay} onChange={(n) => setLineDelay(n ?? 0)} min={LIMITS.lineDelayMs[0]} max={LIMITS.lineDelayMs[1]} step={10} unit="ms" inputSize="sm" />
              </Field>
              <Field label="Delay per character">
                <NumberInput value={charDelay} onChange={(n) => setCharDelay(n ?? 0)} min={LIMITS.charDelayMs[0]} max={LIMITS.charDelayMs[1]} step={5} unit="ms" inputSize="sm" />
              </Field>
              <Field label="Line ending">
                <SimpleSelect aria-label="Line ending" size="sm" value={eol} onValueChange={setEol} options={EOL_OPTIONS} />
              </Field>
            </div>
            <div className="grid gap-2 rounded-md border p-3">
              <div className="flex items-center gap-2">
                <Checkbox id={ids.wait} checked={waitPrompt} onCheckedChange={(v) => setWaitPrompt(v === true)} />
                <label htmlFor={ids.wait} className="text-base select-none">
                  Wait for the prompt before each next line
                </label>
              </div>
              {waitPrompt && (
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_9rem]">
                  <Field label="Prompt pattern (regular expression)" htmlFor={ids.pattern} error={patternError ?? undefined} hint={!patternError ? `Empty = ${DEFAULT_PROMPT_PATTERN} (shell integration marks are used when present)` : undefined}>
                    <Input id={ids.pattern} inputSize="sm" className="font-mono" value={pattern} placeholder={DEFAULT_PROMPT_PATTERN} onChange={(e) => setPattern(e.target.value)} spellCheck={false} />
                  </Field>
                  <Field label="Give up after">
                    <NumberInput value={promptTimeout} onChange={(n) => setPromptTimeout(n ?? 15)} min={1} max={600} unit="s" inputSize="sm" />
                  </Field>
                </div>
              )}
            </div>
            <p className="text-sm text-muted-foreground">
              Lines are typed with {EOL_OPTIONS.find((o) => o.value === eol)?.label}. Large text files are fine; the terminal ignores your keys meanwhile — Ctrl+C
              cancels.
            </p>
          </div>
        ) : (
          <div className="grid gap-3">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <Field label="Chunk size">
                <NumberInput value={chunk} onChange={(n) => setChunk(n ?? 1024)} min={LIMITS.binaryChunkBytes[0]} max={LIMITS.binaryChunkBytes[1]} unit="bytes" inputSize="sm" />
              </Field>
              <Field label="Delay between chunks">
                <NumberInput value={delay} onChange={(n) => setDelay(n ?? 0)} min={LIMITS.binaryDelayMs[0]} max={LIMITS.binaryDelayMs[1]} unit="ms" inputSize="sm" />
              </Field>
            </div>
            {serialBaud ? (
              <Button variant="link" size="sm" className="justify-self-start" onClick={applySerial}>
                Match the serial line ({serialBaud} baud)
              </Button>
            ) : null}
            <p className="text-sm text-muted-foreground">
              The bytes go to the session exactly as they are (for bootloaders, devices waiting for a raw upload, or a remote <span className="font-mono">cat &gt; file</span>).
              {binaryEta != null && ` About ${Math.max(1, Math.round(binaryEta / 1000))} s at this pace.`}
            </p>
          </div>
        )}

        {(binaryProblem || !connected || readOnly || ctrl?.busy) && (
          <p className="flex gap-2 rounded-md border border-warning/40 bg-warning/10 p-2.5 text-sm text-warning">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
            <span>
              {readOnly
                ? 'This terminal is read-only.'
                : !connected
                  ? 'The session is not connected.'
                  : ctrl?.busy
                    ? 'A transfer is already running in this terminal.'
                    : binaryProblem}
            </span>
          </p>
        )}
      </DialogBody>
      <DialogFooter>
        <Button variant="secondary" onClick={() => closeSendDialog()}>
          Cancel
        </Button>
        <Button onClick={() => void submit()} disabled={!canSend} loading={busy}>
          <Send /> Send
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}
