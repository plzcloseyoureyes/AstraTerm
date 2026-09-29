import { useId, useState, type ChangeEvent } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Copy, KeyRound, ShieldAlert, ShieldCheck, ShieldQuestion } from 'lucide-react'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, Prompt, RuntimeSession } from '@/api/types'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { Tooltip } from '@/components/ui/tooltip'
import { copyText, cn } from '@/lib/utils'
import { answerPrompt, usePromptQueue } from '@/stores/prompts'

/** Where a prompt comes from ("prod-web-1 · root@10.0.0.5"), resolved from cached connections / sessions. */
function usePromptSource(p: Prompt): string | null {
  const qc = useQueryClient()
  const conns = qc.getQueryData<Connection[]>(queryKeys.connections)
  const sessions = qc.getQueryData<RuntimeSession[]>(queryKeys.sessions)
  const conn = p.connectionId ? conns?.find((c) => c.id === p.connectionId) : undefined
  const sess = p.sessionId ? sessions?.find((s) => s.id === p.sessionId) : undefined
  if (conn) return `${conn.name}${conn.host ? ` · ${conn.username ? `${conn.username}@` : ''}${conn.host}` : ''}`
  if (sess) return sess.title || [sess.username, sess.host].filter(Boolean).join('@') || null
  return null
}

function Fingerprint({ label, value, tone }: { label: string; value: string; tone?: 'danger' | 'muted' }) {
  return (
    <div className="grid gap-0.5">
      <div className="text-xs font-medium text-muted-foreground">{label}</div>
      <div
        className={cn(
          'flex items-start gap-2 rounded-md border bg-muted/50 px-2 py-1.5 font-mono text-sm break-all',
          tone === 'danger' && 'border-destructive/40 bg-destructive/8',
          tone === 'muted' && 'text-muted-foreground',
        )}
      >
        <span className="flex-1 select-all">{value}</span>
        <Tooltip content="Copy">
          <button
            type="button"
            className="mt-px shrink-0 rounded-sm p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-label={`Copy ${label}`}
            onClick={async () => {
              if (await copyText(value)) toast.success('Copied to clipboard')
            }}
          >
            <Copy className="size-3.5" />
          </button>
        </Tooltip>
      </div>
    </div>
  )
}

function HostKeyPrompt({ p, onAnswer }: { p: Prompt; onAnswer: (accept: boolean, save: boolean) => void }) {
  const hk = p.hostKey
  const mismatch = hk?.status === 'mismatch'
  const source = usePromptSource(p)
  return (
    <>
      <DialogHeader>
        <DialogTitle className={cn(mismatch && 'text-destructive')}>
          {mismatch ? <ShieldAlert className="size-5" /> : <ShieldQuestion className="size-5 text-warning" />}
          {mismatch ? 'Host key has changed!' : p.title || 'Unknown host key'}
        </DialogTitle>
        <DialogDescription>
          {mismatch
            ? 'The server presented a different host key than the one saved for this host.'
            : 'The authenticity of this host cannot be established. Verify the fingerprint with the server administrator before trusting it.'}
        </DialogDescription>
      </DialogHeader>
      {mismatch && (
        <div role="alert" className="rounded-md border border-destructive/50 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          <strong>Possible man-in-the-middle attack.</strong> Someone could be intercepting this connection, or the host key was
          legitimately replaced (server reinstall). Only continue if you know why the key changed.
        </div>
      )}
      {hk && (
        <div className="grid gap-3">
          <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-base">
            <span className="text-muted-foreground">Host</span>
            <span className="font-mono">
              {hk.host}:{hk.port}
            </span>
            <span className="text-muted-foreground">Key type</span>
            <span className="font-mono">{hk.keyType}</span>
            {source && (
              <>
                <span className="text-muted-foreground">Session</span>
                <span className="truncate">{source}</span>
              </>
            )}
          </div>
          <Fingerprint label="SHA256 fingerprint" value={hk.fingerprint} tone={mismatch ? 'danger' : undefined} />
          <Fingerprint label="MD5 fingerprint" value={hk.fingerprintMd5} tone="muted" />
          {mismatch && hk.knownFingerprint && <Fingerprint label="Previously saved fingerprint" value={hk.knownFingerprint} tone="muted" />}
        </div>
      )}
      {/* The server's text repeats the fields above; show it only when there are no structured details. */}
      {p.message && !hk && <p className="text-sm text-muted-foreground whitespace-pre-wrap">{p.message}</p>}
      <DialogFooter>
        <Button variant="secondary" onClick={() => onAnswer(false, false)} autoFocus={mismatch}>
          Cancel
        </Button>
        <Button variant={mismatch ? 'outline' : 'secondary'} onClick={() => onAnswer(true, false)}>
          Accept once
        </Button>
        {p.allowSave && (
          <Button variant={mismatch ? 'destructive' : 'default'} onClick={() => onAnswer(true, true)} autoFocus={!mismatch}>
            {mismatch ? 'Replace saved key' : 'Accept & save'}
          </Button>
        )}
      </DialogFooter>
    </>
  )
}

function CredentialsPrompt({ p, onAnswer }: { p: Prompt; onAnswer: (accept: boolean, save: boolean, values?: string[]) => void }) {
  const fields = p.fields?.length ? p.fields : [{ label: p.kind === 'passphrase' ? 'Passphrase' : 'Password', echo: false }]
  const [values, setValues] = useState<string[]>(() => fields.map((f) => f.value ?? ''))
  const [save, setSave] = useState(false)
  const baseId = useId()
  const source = usePromptSource(p)
  const title =
    p.title || (p.kind === 'passphrase' ? 'Key passphrase required' : p.kind === 'keyboard-interactive' ? 'Authentication required' : 'Password required')

  return (
    <form
      className="contents"
      onSubmit={(e) => {
        e.preventDefault()
        onAnswer(true, save, values)
      }}
    >
      <DialogHeader>
        <DialogTitle>
          <KeyRound className="size-4.5 text-primary" />
          {title}
        </DialogTitle>
        <DialogDescription className={cn(!p.message && !source && 'sr-only')}>
          {source && <span className="block font-medium text-foreground/85">{source}</span>}
          {p.message ? <span className="whitespace-pre-wrap">{p.message}</span> : !source ? 'Enter your credentials' : null}
        </DialogDescription>
      </DialogHeader>
      <div className="grid gap-3">
        {fields.map((f, i) => {
          const id = `${baseId}-${i}`
          const common = {
            id,
            value: values[i] ?? '',
            autoFocus: i === 0,
            onChange: (e: ChangeEvent<HTMLInputElement>) =>
              setValues((v) => {
                const next = v.slice()
                next[i] = e.target.value
                return next
              }),
          }
          return (
            <div key={id} className="grid gap-1.5">
              <label htmlFor={id} className="text-sm font-medium whitespace-pre-wrap">
                {f.label.replace(/:\s*$/, '')}
              </label>
              {f.echo ? (
                <Input {...common} autoComplete="off" spellCheck={false} />
              ) : (
                <PasswordInput {...common} autoComplete={p.kind === 'password' ? 'current-password' : 'off'} />
              )}
            </div>
          )
        })}
        {p.allowSave && (
          <CheckboxField
            checked={save}
            onCheckedChange={(v) => setSave(v === true)}
            label={p.kind === 'passphrase' ? 'Remember passphrase' : 'Remember password'}
            description="Stored encrypted in the AstraTerm vault."
          />
        )}
      </div>
      <DialogFooter>
        <Button variant="secondary" onClick={() => onAnswer(false, false)}>
          Cancel
        </Button>
        <Button type="submit">Continue</Button>
      </DialogFooter>
    </form>
  )
}

function ConfirmPrompt({ p, onAnswer }: { p: Prompt; onAnswer: (accept: boolean, save: boolean) => void }) {
  const [save, setSave] = useState(false)
  return (
    <>
      <DialogHeader>
        <DialogTitle>
          <ShieldCheck className="size-4.5 text-primary" />
          {p.title || 'Confirm'}
        </DialogTitle>
        <DialogDescription className="whitespace-pre-wrap">{p.message || 'Do you want to continue?'}</DialogDescription>
      </DialogHeader>
      {p.allowSave && <CheckboxField checked={save} onCheckedChange={(v) => setSave(v === true)} label="Remember my choice" />}
      <DialogFooter>
        <Button variant="secondary" onClick={() => onAnswer(false, save)}>
          No
        </Button>
        <Button onClick={() => onAnswer(true, save)} autoFocus>
          Yes
        </Button>
      </DialogFooter>
    </>
  )
}

/** Renders prompt-broker requests (host keys, passwords, 2FA, confirmations) one at a time. */
export function PromptHost() {
  const queue = usePromptQueue()
  const p = queue[0]
  if (!p) return null
  return <PromptDialog key={p.id} p={p} queued={queue.length} />
}

function PromptDialog({ p, queued }: { p: Prompt; queued: number }) {
  const [busy, setBusy] = useState(false)

  const answer = (accept: boolean, save: boolean, values?: string[]) => {
    if (busy) return
    setBusy(true)
    if (!answerPrompt(p.id, { accept, save, values })) setBusy(false)
  }

  return (
    <Dialog open onOpenChange={(o) => !o && answer(false, false)}>
      <DialogContent size={p.kind === 'hostkey' ? 'lg' : 'md'} hideClose onInteractOutside={(e) => e.preventDefault()}>
        {queued > 1 && <div className="absolute top-3 right-4 text-xs text-muted-foreground">1 of {queued}</div>}
        {p.kind === 'hostkey' ? (
          <HostKeyPrompt p={p} onAnswer={answer} />
        ) : p.kind === 'confirm' ? (
          <ConfirmPrompt p={p} onAnswer={answer} />
        ) : (
          <CredentialsPrompt p={p} onAnswer={answer} />
        )}
      </DialogContent>
    </Dialog>
  )
}
