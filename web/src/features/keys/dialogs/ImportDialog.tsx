/*
 * Import a private key (OpenSSH, PEM PKCS#1 / PKCS#8 / EC, PuTTY PPK v2 / v3) — pasted or loaded from a file — into the
 * vault, optionally with its OpenSSH certificate. "Convert" mode (MobaKeyGen "Load" + "Save") converts the key to
 * another format without storing it. The text is inspected server-side as it is typed (format, type, fingerprint,
 * whether a passphrase is needed, whether the key is already stored).
 */
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ArrowRightLeft, BadgeCheck, CircleAlert, FileKey2, KeyRound, Upload } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { RadioField, RadioGroup } from '@/components/ui/radio-group'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { errorMessage } from '@/lib/utils'
import { convertKey, importKey, inspectKey, invalidateKeys } from '../api'
import { CertificateDetails, KeyTextInput, SectionLabel } from '../components'
import { keysSettings } from '../settings'
import type { ExportFormat, InspectResult } from '../types'
import { downloadText, keyTypeLabel, shortFingerprint } from '../util'

const FORMAT_LABELS: Record<string, string> = { openssh: 'OpenSSH', pem: 'PEM', pkcs8: 'PKCS#8', ppk: 'PuTTY' }

const CONVERT_FORMATS: { value: string; label: string }[] = [
  { value: 'openssh', label: 'OpenSSH private key' },
  { value: 'ppk3', label: 'PuTTY private key (.ppk v3)' },
  { value: 'ppk2', label: 'PuTTY private key (.ppk v2)' },
  { value: 'pem', label: 'PEM (traditional)' },
  { value: 'pkcs8', label: 'PKCS#8' },
  { value: 'public', label: 'Public key (authorized_keys)' },
  { value: 'rfc4716', label: 'Public key (RFC 4716 / SSH2)' },
]

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

export default function ImportDialog({ mode: initialMode, initialText, onClose }: { mode: 'import' | 'convert'; initialText?: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [mode, setMode] = useState(initialMode)
  const [text, setText] = useState(initialText ?? '')
  const [fileName, setFileName] = useState('')
  const [pass, setPass] = useState('')
  const [remember, setRemember] = useState(keysSettings.get().rememberPassphrase)
  const [name, setName] = useState('')
  const [certText, setCertText] = useState('')
  const [showCert, setShowCert] = useState(false)
  const [info, setInfo] = useState<InspectResult | null>(null)
  const [inspectError, setInspectError] = useState<string | null>(null)
  const [inspecting, setInspecting] = useState(false)
  const [certInfo, setCertInfo] = useState<InspectResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [format, setFormat] = useState('openssh')
  const [protect, setProtect] = useState<'keep' | 'new' | 'none'>('keep')
  const [newPass, setNewPass] = useState('')
  const [newPass2, setNewPass2] = useState('')
  const passRef = useRef<HTMLInputElement>(null)

  const dText = useDebounced(text, 300)
  const dPass = useDebounced(pass, 400)
  const dCert = useDebounced(certText, 300)

  useEffect(() => {
    let cancelled = false
    if (!dText.trim()) {
      setInfo(null)
      setInspectError(null)
      return
    }
    setInspecting(true)
    inspectKey(dText, dPass || undefined)
      .then((r) => {
        if (cancelled) return
        setInfo(r)
        setInspectError(null)
      })
      .catch((err) => {
        if (cancelled) return
        setInfo(null)
        setInspectError(errorMessage(err))
      })
      .finally(() => !cancelled && setInspecting(false))
    return () => {
      cancelled = true
    }
  }, [dText, dPass])

  useEffect(() => {
    let cancelled = false
    if (!dCert.trim()) {
      setCertInfo(null)
      return
    }
    inspectKey(dCert)
      .then((r) => !cancelled && setCertInfo(r))
      .catch(() => !cancelled && setCertInfo(null))
    return () => {
      cancelled = true
    }
  }, [dCert])

  useEffect(() => {
    if (info?.needsPassphrase && !pass) passRef.current?.focus()
  }, [info?.needsPassphrase, pass])

  const isPrivate = info?.kind === 'private'
  const ready = isPrivate && !info.needsPassphrase && !inspecting && dText === text && dPass === pass
  const certOk = !certText.trim() || (certInfo?.kind === 'certificate' && certInfo.certificate?.type === 'user' && certInfo.fingerprint === info?.fingerprint)
  const newPassMismatch = protect === 'new' && newPass !== newPass2
  const defaultName = info?.comment || fileName.replace(/\.(ppk|pem|key)$/i, '')

  const doImport = async () => {
    if (!ready) return
    setBusy(true)
    try {
      const k = await importKey({
        name: name.trim() || defaultName || undefined,
        privateKey: text,
        passphrase: info?.encrypted ? pass : undefined,
        rememberPassphrase: remember,
        certificate: certText.trim() || undefined,
      })
      invalidateKeys(qc)
      toast.success(`Imported “${k.name}”`)
      onClose()
    } catch (err) {
      toast.error('Import failed', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }

  const doConvert = async () => {
    if (!ready || newPassMismatch) return
    setBusy(true)
    try {
      const ppk = format === 'ppk3' ? 3 : format === 'ppk2' ? 2 : undefined
      const f = (ppk ? 'ppk' : format) as ExportFormat
      const newPassphrase = protect === 'keep' ? (info?.encrypted ? pass : '') : protect === 'new' ? newPass : ''
      const res = await convertKey({ privateKey: text, passphrase: info?.encrypted ? pass : undefined, format: f, newPassphrase, ppkVersion: ppk, name: name.trim() || defaultName || undefined })
      downloadText(res.content, res.filename, res.mime)
      toast.success(`Saved ${res.filename}`)
    } catch (err) {
      toast.error('Conversion failed', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }

  const publicOut = format === 'public' || format === 'rfc4716'

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="xl" onInteractOutside={(e) => text && e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>
            {mode === 'import' ? <Upload className="size-4.5 text-primary" /> : <ArrowRightLeft className="size-4.5 text-primary" />}
            {mode === 'import' ? 'Import private key' : 'Convert key file'}
          </DialogTitle>
          <DialogDescription>
            OpenSSH, PEM (PKCS#1, PKCS#8, EC) and PuTTY (.ppk v2 / v3) keys are supported. The key never leaves NexTerm.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="@container grid gap-4">
          <KeyTextInput
            value={text}
            onChange={setText}
            onFileName={setFileName}
            placeholder={'-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----\n\nor PuTTY-User-Key-File-3: …'}
          />
          <Preview info={info} error={inspectError} inspecting={inspecting} />
          {info?.encrypted && (
            <div className="grid items-start gap-3 @lg:grid-cols-2">
              <Field
                label="Passphrase"
                error={info.wrongPassphrase && pass === dPass && pass ? 'Incorrect passphrase' : undefined}
                hint={info.needsPassphrase ? 'The key is encrypted.' : 'Passphrase accepted.'}
              >
                <PasswordInput ref={passRef} value={pass} onChange={(e) => setPass(e.target.value)} autoComplete="off" />
              </Field>
              {mode === 'import' && (
                <CheckboxField
                  className="@lg:pt-6"
                  checked={remember}
                  onCheckedChange={(v) => setRemember(v === true)}
                  label="Remember the passphrase in the vault"
                  description="Otherwise NexTerm asks for it whenever the key is used."
                />
              )}
            </div>
          )}
          {mode === 'import' ? (
            <>
              <Field label="Name" hint="How the key is listed in NexTerm.">
                <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={defaultName || 'My key'} maxLength={200} />
              </Field>
              {!showCert ? (
                <Button variant="link" size="sm" className="justify-self-start" onClick={() => setShowCert(true)}>
                  Attach an OpenSSH certificate (id_*-cert.pub)…
                </Button>
              ) : (
                <div className="grid gap-2">
                  <SectionLabel>Certificate (optional)</SectionLabel>
                  <KeyTextInput value={certText} onChange={setCertText} rows={3} accept=".pub" placeholder="ssh-ed25519-cert-v01@openssh.com AAAA…" />
                  {certText.trim() && !certOk && (
                    <p className="text-sm text-destructive" role="alert">
                      {certInfo?.kind !== 'certificate'
                        ? 'Not an OpenSSH certificate.'
                        : certInfo.certificate?.type !== 'user'
                          ? 'This is a host certificate.'
                          : 'The certificate belongs to another key.'}
                    </p>
                  )}
                  {certOk && certInfo?.certificate && <CertificateDetails info={certInfo.certificate} />}
                </div>
              )}
            </>
          ) : (
            <div className="grid gap-4 rounded-lg border bg-card/60 p-3.5">
              <div className="grid items-start gap-4 @lg:grid-cols-2">
                <Field label="Save as">
                  <SimpleSelect value={format} onValueChange={setFormat} options={CONVERT_FORMATS} aria-label="Output format" />
                </Field>
                <Field label="File name">
                  <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={defaultName || 'id_key'} maxLength={200} />
                </Field>
              </div>
              {!publicOut && (
                <RadioGroup value={protect} onValueChange={(v) => setProtect(v as typeof protect)} aria-label="Protection">
                  <RadioField value="keep" label={info?.encrypted ? 'Keep the current passphrase' : 'Keep it unprotected'} />
                  <RadioField value="new" label="New passphrase" />
                  <RadioField value="none" label="No passphrase" description="Anyone who gets the file can use the key." />
                </RadioGroup>
              )}
              {!publicOut && protect === 'new' && (
                <div className="grid gap-3 @lg:grid-cols-2">
                  <Field label="New passphrase">
                    <PasswordInput value={newPass} onChange={(e) => setNewPass(e.target.value)} generate onGenerate={setNewPass2} autoComplete="new-password" />
                  </Field>
                  <Field label="Confirm" error={newPassMismatch && newPass2 ? 'The passphrases differ' : undefined}>
                    <PasswordInput value={newPass2} onChange={(e) => setNewPass2(e.target.value)} autoComplete="new-password" />
                  </Field>
                </div>
              )}
            </div>
          )}
        </DialogBody>
        <DialogFooter className="sm:justify-between">
          <Button variant="ghost" size="sm" onClick={() => setMode(mode === 'import' ? 'convert' : 'import')}>
            {mode === 'import' ? (
              <>
                <ArrowRightLeft /> Convert only (don't store)
              </>
            ) : (
              <>
                <Upload /> Import into the vault instead
              </>
            )}
          </Button>
          <div className="flex gap-2">
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            {mode === 'import' ? (
              <Button onClick={() => void doImport()} loading={busy} disabled={!ready || !certOk || !!info?.existingKeyId}>
                <KeyRound /> Import
              </Button>
            ) : (
              <Button onClick={() => void doConvert()} loading={busy} disabled={!ready || newPassMismatch || (protect === 'new' && !newPass)}>
                <FileKey2 /> Save file
              </Button>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function Preview({ info, error, inspecting }: { info: InspectResult | null; error: string | null; inspecting: boolean }) {
  if (inspecting && !info && !error) {
    return (
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Spinner className="size-3.5" /> Reading the key…
      </div>
    )
  }
  if (error) {
    return (
      <div role="alert" className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/8 px-3 py-2 text-sm text-destructive">
        <CircleAlert className="mt-0.5 size-4 shrink-0" /> {error}
      </div>
    )
  }
  if (!info) return null
  if (info.kind !== 'private') {
    return (
      <div role="alert" className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm">
        <CircleAlert className="mt-0.5 size-4 shrink-0 text-warning" />
        This is a {info.kind === 'certificate' ? 'certificate' : 'public key'}; paste the private key (the file without .pub).
      </div>
    )
  }
  return (
    <div className="grid gap-2 rounded-md border bg-muted/30 px-3 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        {info.format && <Badge variant="outline">{FORMAT_LABELS[info.format] ?? info.format}{info.ppkVersion ? ` v${info.ppkVersion}` : ''}</Badge>}
        {info.type && <Badge variant="outline" className="font-mono">{keyTypeLabel(info.type, info.bits)}</Badge>}
        {info.encrypted && <Badge variant="info">Encrypted</Badge>}
        {info.fingerprint && <span className="font-mono text-muted-foreground">{shortFingerprint(info.fingerprint, 14)}</span>}
        {info.comment && <span className="truncate text-muted-foreground">“{info.comment}”</span>}
        {!info.needsPassphrase && <BadgeCheck className="ml-auto size-4 text-success" aria-label="Readable" />}
      </div>
      {info.existingKeyId && (
        <p className="text-sm text-warning">This key is already stored as “{info.existingKeyName}”.</p>
      )}
    </div>
  )
}
