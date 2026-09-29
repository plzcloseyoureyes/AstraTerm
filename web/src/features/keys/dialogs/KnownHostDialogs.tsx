/*
 * Known hosts dialogs (SSH-19/20): add a trusted host key (also accepts ssh-keyscan output), add a certificate
 * authority / revoked-key entry, and import known_hosts (OpenSSH, PuTTY registry export, MobaXterm.ini, or this
 * computer's ~/.ssh/known_hosts).
 */
import { useState } from 'react'
import { toast } from 'sonner'
import { CircleCheck, FileUp, ShieldCheck, ShieldX } from 'lucide-react'
import { isApiError } from '@/api/client'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { RadioField, RadioGroup } from '@/components/ui/radio-group'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { errorMessage } from '@/lib/utils'
import { useRunMode } from '@/stores/auth'
import { addKnownHost, addMarker, importKnownHosts, invalidateKnownHosts, useKeys } from '../api'
import { KeySelect, KeyTextInput } from '../components'
import type { KnownHostsConflict, KnownHostsImportResult, MarkerKind } from '../types'

const KEY_TYPE = /^(ssh-|ecdsa-|sk-)/

/** Split "host keytype base64 [comment]" (ssh-keyscan / known_hosts) into host and key. */
function splitScanLine(text: string): { host?: string; key: string } {
  const line = text.split('\n').find((l) => l.trim() && !l.trim().startsWith('#'))?.trim() ?? ''
  const f = line.split(/\s+/)
  if (f.length >= 3 && !KEY_TYPE.test(f[0]) && KEY_TYPE.test(f[1])) return { host: f[0].split(',')[0], key: f.slice(1).join(' ') }
  return { key: text.trim() }
}

export function KnownHostDialog({ onClose }: { onClose: () => void }) {
  const [host, setHost] = useState('')
  const [port, setPort] = useState<number | null>(22)
  const [key, setKey] = useState('')
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onKey = (text: string) => {
    const s = splitScanLine(text)
    if (s.host && !host) {
      const m = /^\[(.+)\]:(\d+)$/.exec(s.host)
      if (m) {
        setHost(m[1])
        setPort(Number(m[2]))
      } else setHost(s.host)
      setKey(s.key)
    } else setKey(text)
  }

  const submit = async (replace = false) => {
    setBusy(true)
    setError(null)
    let conflict: string | null = null
    try {
      await addKnownHost({ host: host.trim(), port: port ?? 22, publicKey: key, comment: comment.trim() || undefined, replace })
      invalidateKnownHosts()
      toast.success('Host key trusted')
      onClose()
    } catch (err) {
      if (isApiError(err) && err.code === 'host_key_conflict' && !replace) conflict = err.message
      else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
    if (conflict && (await confirm({ title: 'Replace the trusted key?', description: conflict, confirmLabel: 'Replace', destructive: true }))) {
      await submit(true)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <ShieldCheck className="size-4.5 text-primary" /> Trust a host key
            </DialogTitle>
            <DialogDescription>Connections to this host accept the key without asking. Paste the key or a line of ssh-keyscan output.</DialogDescription>
          </DialogHeader>
          <DialogBody className="grid gap-4">
            <div className="grid grid-cols-[1fr_7rem] gap-3">
              <Field label="Host" required>
                <Input value={host} onChange={(e) => setHost(e.target.value)} placeholder="server.example.com or 10.0.0.5" className="font-mono" spellCheck={false} autoFocus />
              </Field>
              <Field label="Port">
                <NumberInput value={port} onChange={setPort} min={1} max={65535} />
              </Field>
            </div>
            <Field label="Host public key" required>
              <KeyTextInput value={key} onChange={onKey} rows={4} fileLabel="host key file" placeholder="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…" />
            </Field>
            <Field label="Comment">
              <Input value={comment} onChange={(e) => setComment(e.target.value)} placeholder="e.g. verified with the admin on 2026-09-27" maxLength={300} />
            </Field>
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={busy} disabled={!host.trim() || !key.trim()}>
              Trust key
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function MarkerDialog({ marker: initialMarker, keyId: initialKeyId, onClose }: { marker?: MarkerKind; keyId?: string; onClose: () => void }) {
  const keys = useKeys()
  const [marker, setMarker] = useState<MarkerKind>(initialMarker ?? 'cert-authority')
  const [hosts, setHosts] = useState(initialMarker === 'revoked' ? '*' : '')
  const [source, setSource] = useState<'paste' | 'stored'>(initialKeyId ? 'stored' : 'paste')
  const [key, setKey] = useState('')
  const [keyId, setKeyId] = useState(initialKeyId ?? '')
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const ca = marker === 'cert-authority'

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      await addMarker({ marker, hosts, comment: comment.trim() || undefined, ...(source === 'stored' ? { keyId } : { publicKey: key }) })
      invalidateKnownHosts()
      toast.success(ca ? 'Certificate authority trusted' : 'Key revoked')
      onClose()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              {ca ? <ShieldCheck className="size-4.5 text-primary" /> : <ShieldX className="size-4.5 text-destructive" />}
              {ca ? 'Trust a host certificate authority' : 'Revoke a host key'}
            </DialogTitle>
            <DialogDescription>
              {ca
                ? 'Host certificates signed by this CA are accepted without a prompt for matching hosts (known_hosts @cert-authority).'
                : 'Connections presenting this key (or a certificate of it, or signed by it) are always refused (known_hosts @revoked).'}
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="grid gap-4">
            <SegmentedControl<MarkerKind>
              aria-label="Entry type"
              value={marker}
              onValueChange={(m) => {
                setMarker(m)
                if (m === 'revoked' && !hosts) setHosts('*')
              }}
              options={[
                { value: 'cert-authority', label: 'Certificate authority' },
                { value: 'revoked', label: 'Revoked key' },
              ]}
            />
            <Field
              label="Hosts"
              required
              hint="Comma-separated patterns: * and ? wildcards, ! to exclude, [host]:port to limit to one port. A pattern covers every port of matching hosts."
            >
              <Input value={hosts} onChange={(e) => setHosts(e.target.value)} placeholder={ca ? '*.example.com,!legacy.example.com' : '*'} className="font-mono" spellCheck={false} />
            </Field>
            <div className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <span className="text-sm font-medium">{ca ? 'CA public key' : 'Revoked public key'}</span>
                <SegmentedControl<'paste' | 'stored'>
                  size="sm"
                  aria-label="Key source"
                  value={source}
                  onValueChange={setSource}
                  options={[
                    { value: 'paste', label: 'Paste' },
                    { value: 'stored', label: 'My keys' },
                  ]}
                />
              </div>
              {source === 'stored' ? (
                <KeySelect keys={keys.data} value={keyId} onChange={setKeyId} noneLabel="" />
              ) : (
                <KeyTextInput value={key} onChange={setKey} rows={3} accept=".pub" placeholder="ssh-ed25519 AAAA… host-ca" />
              )}
            </div>
            <Field label="Comment">
              <Input value={comment} onChange={(e) => setComment(e.target.value)} maxLength={300} />
            </Field>
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" variant={ca ? 'default' : 'destructive'} loading={busy} disabled={!hosts.trim() || (source === 'stored' ? !keyId : !key.trim())}>
              {ca ? 'Trust CA' : 'Revoke key'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function KnownHostsImportDialog({ onClose }: { onClose: () => void }) {
  const mode = useRunMode()
  const [source, setSource] = useState<'text' | 'system'>('text')
  const [text, setText] = useState('')
  const [format, setFormat] = useState<'auto' | 'openssh' | 'putty'>('auto')
  const [onConflict, setOnConflict] = useState<KnownHostsConflict>('skip')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<KnownHostsImportResult | null>(null)

  const run = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await importKnownHosts(source === 'system' ? { source: 'system', format, onConflict } : { text, format, onConflict })
      setResult(res)
      invalidateKnownHosts()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <FileUp className="size-4.5 text-primary" /> Import known hosts
          </DialogTitle>
          <DialogDescription>OpenSSH known_hosts (including @cert-authority and @revoked lines), a PuTTY registry export or MobaXterm.ini.</DialogDescription>
        </DialogHeader>
        {result ? (
          <DialogBody className="grid gap-3">
            <div role="status" className="flex items-start gap-2 rounded-md border border-success/40 bg-success/8 px-3 py-2 text-base">
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-success" />
              <span>
                {result.added} host key{result.added === 1 ? '' : 's'} added
                {result.replaced ? `, ${result.replaced} replaced` : ''}
                {result.markers ? `, ${result.markers} CA / revocation entr${result.markers === 1 ? 'y' : 'ies'}` : ''} ({result.format === 'putty' ? 'PuTTY format' : 'OpenSSH format'}).
              </span>
            </div>
            <ul className="grid gap-1 text-sm text-muted-foreground">
              {result.skipped > 0 && <li>{result.skipped} already trusted — skipped.</li>}
              {result.conflicts > 0 && <li className="text-warning">{result.conflicts} differ from a trusted key of the same host and type — not imported (choose “Replace” to take them).</li>}
              {result.hashed > 0 && <li className="text-warning">{result.hashed} hashed entries (HashKnownHosts) cannot be read — skipped.</li>}
              {result.patterns > 0 && <li>{result.patterns} wildcard host entries are not supported — skipped.</li>}
              {result.invalid > 0 && <li className="text-destructive">{result.invalid} invalid line{result.invalid === 1 ? '' : 's'}.</li>}
            </ul>
            {result.errors.length > 0 && (
              <div className="max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 font-mono text-xs">
                {result.errors.map((e, i) => (
                  <div key={i}>
                    line {e.line}: {e.error}
                  </div>
                ))}
              </div>
            )}
          </DialogBody>
        ) : (
          <DialogBody className="grid gap-4">
            {mode === 'desktop' && (
              <RadioGroup value={source} onValueChange={(v) => setSource(v as 'text' | 'system')} aria-label="Source">
                <RadioField value="text" label="Paste or load a file" />
                <RadioField value="system" label="This computer's ~/.ssh/known_hosts" description="The OpenSSH file of the user running Termstead." />
              </RadioGroup>
            )}
            {source === 'text' && (
              <KeyTextInput
                value={text}
                onChange={setText}
                rows={8}
                maxBytes={8 * 1024 * 1024}
                fileLabel="known_hosts file"
                placeholder={'host.example.com ssh-ed25519 AAAA…\n@cert-authority *.example.com ssh-ed25519 AAAA…'}
              />
            )}
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Format">
                <SimpleSelect
                  value={format}
                  onValueChange={setFormat}
                  options={[
                    { value: 'auto', label: 'Detect automatically' },
                    { value: 'openssh', label: 'OpenSSH known_hosts' },
                    { value: 'putty', label: 'PuTTY / MobaXterm host keys' },
                  ]}
                  aria-label="Format"
                />
              </Field>
              <Field label="When a host already has another key">
                <SimpleSelect
                  value={onConflict}
                  onValueChange={setOnConflict}
                  options={[
                    { value: 'skip', label: 'Keep the trusted key' },
                    { value: 'replace', label: 'Replace it' },
                    { value: 'add', label: 'Trust both' },
                  ]}
                  aria-label="Conflicts"
                />
              </Field>
            </div>
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
        )}
        <DialogFooter>
          {result ? (
            <Button onClick={onClose}>Done</Button>
          ) : (
            <>
              <Button variant="secondary" onClick={onClose}>
                Cancel
              </Button>
              <Button onClick={() => void run()} loading={busy} disabled={source === 'text' && !text.trim()}>
                Import
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
