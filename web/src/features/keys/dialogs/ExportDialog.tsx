/*
 * Export a stored key: OpenSSH / PuTTY (.ppk v3 or v2) / PEM / PKCS#8 private key files — protected with the key's
 * current passphrase, a new one, or none — and public keys (authorized_keys line, RFC 4716).
 */
import { useState } from 'react'
import { toast } from 'sonner'
import { Copy, Download, FileDown, TriangleAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { PasswordInput } from '@/components/ui/password-input'
import { RadioField, RadioGroup } from '@/components/ui/radio-group'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { errorMessage } from '@/lib/utils'
import { exportKey, useKeys } from '../api'
import { keysSettings } from '../settings'
import type { ExportFormat } from '../types'
import { copyWithToast, downloadText, keyTypeLabel } from '../util'

const FORMATS: { value: ExportFormat; label: string; description: string; private: boolean }[] = [
  { value: 'openssh', label: 'OpenSSH private key', description: 'For ssh, git and most tools (id_ed25519, id_rsa…).', private: true },
  { value: 'ppk', label: 'PuTTY private key (.ppk)', description: 'For PuTTY, WinSCP, FileZilla and other PuTTY-format tools.', private: true },
  { value: 'pem', label: 'PEM (traditional)', description: 'PKCS#1 / SEC1 — for older tools (ssh-keygen -m PEM).', private: true },
  { value: 'pkcs8', label: 'PKCS#8', description: 'Standard private key container used by OpenSSL and Java.', private: true },
  { value: 'public', label: 'Public key (authorized_keys)', description: 'One line for ~/.ssh/authorized_keys on servers.', private: false },
  { value: 'rfc4716', label: 'Public key (RFC 4716)', description: 'SSH2 format, for commercial SSH servers and some devices.', private: false },
]

export default function ExportDialog({ keyId, initialFormat, onClose }: { keyId: string; initialFormat?: ExportFormat; onClose: () => void }) {
  const { data: keys, isLoading } = useKeys()
  const k = keys?.find((x) => x.id === keyId)
  const [format, setFormat] = useState<ExportFormat>(initialFormat ?? 'openssh')
  const [ppk, setPpk] = useState<'2' | '3'>(String(keysSettings.get().ppkVersion) as '2' | '3')
  const [protectChoice, setProtect] = useState<'keep' | 'new' | 'none' | null>(null)
  const protect = protectChoice ?? (k?.hasPassphrase ? 'keep' : 'new')
  const [current, setCurrent] = useState('')
  const [pass, setPass] = useState('')
  const [pass2, setPass2] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const def = FORMATS.find((f) => f.value === format)!
  const needCurrent = def.private && !!k?.hasPassphrase && !k.passphraseSaved
  const mismatch = def.private && protect === 'new' && pass !== pass2
  const effectiveProtect = !def.private ? 'none' : protect === 'keep' && !k?.hasPassphrase ? 'none' : protect

  const run = async (action: 'save' | 'copy') => {
    if (!k) return
    setBusy(true)
    setError(null)
    try {
      const res = await exportKey(k.id, {
        format,
        keepPassphrase: def.private && effectiveProtect === 'keep',
        passphrase: def.private && effectiveProtect === 'new' ? pass : '',
        currentPassphrase: needCurrent ? current : undefined,
        ppkVersion: format === 'ppk' ? (Number(ppk) as 2 | 3) : undefined,
      })
      if (action === 'copy') await copyWithToast(res.content, def.private ? 'Private key copied' : 'Public key copied')
      else {
        downloadText(res.content, res.filename, res.mime)
        toast.success(`Saved ${res.filename}`)
      }
      if (def.private && !res.encrypted) toast.warning('The exported private key is not protected by a passphrase')
      if (action === 'save') onClose()
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
            <FileDown className="size-4.5 text-primary" /> Export key{k ? ` “${k.name}”` : ''}
          </DialogTitle>
          <DialogDescription>{k ? `${keyTypeLabel(k.type, k.bits)} · ${k.fingerprint}` : 'Choose a format.'}</DialogDescription>
        </DialogHeader>
        {!k ? (
          isLoading ? (
            <div className="flex justify-center py-8">
              <Spinner />
            </div>
          ) : (
            <EmptyState size="sm" title="Key not found" description="It may have been deleted." />
          )
        ) : (
          <DialogBody className="grid gap-4">
            <RadioGroup value={format} onValueChange={(v) => setFormat(v as ExportFormat)} aria-label="Format">
              {FORMATS.map((f) => (
                <RadioField key={f.value} value={f.value} label={f.label} description={f.description} disabled={f.private && !k.hasPrivateKey} />
              ))}
            </RadioGroup>
            {k.type === 'ed25519' && (format === 'pem' || format === 'pkcs8') && (
              <p className="flex items-start gap-2 text-sm text-muted-foreground">
                <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" />
                Ed25519 keys are written as PKCS#8 here, which OpenSSL-based tools read but ssh and ssh-keygen do not; use the
                OpenSSH format for them.
              </p>
            )}
            {format === 'ppk' && (
              <Field label="PuTTY format version" orientation="horizontal" hint="Version 2 is needed for PuTTY older than 0.75 (2021).">
                <SimpleSelect
                  value={ppk}
                  onValueChange={(v) => setPpk(v as '2' | '3')}
                  options={[
                    { value: '3', label: 'Version 3 (Argon2)' },
                    { value: '2', label: 'Version 2 (legacy)' },
                  ]}
                  aria-label="PuTTY format version"
                />
              </Field>
            )}
            {def.private && (
              <div className="grid gap-3 rounded-lg border bg-card/60 p-3.5">
                <RadioGroup value={protect} onValueChange={(v) => setProtect(v as 'keep' | 'new' | 'none')} aria-label="Protection">
                  {k.hasPassphrase && <RadioField value="keep" label="Protect with the key's current passphrase" />}
                  <RadioField value="new" label={k.hasPassphrase ? 'Protect with a new passphrase' : 'Protect with a passphrase'} />
                  <RadioField value="none" label="No passphrase" description="Anyone who gets the file can use the key." />
                </RadioGroup>
                {protect === 'new' && (
                  <div className="grid gap-3 sm:grid-cols-2">
                    <Field label="Passphrase">
                      <PasswordInput value={pass} onChange={(e) => setPass(e.target.value)} generate onGenerate={setPass2} autoComplete="new-password" />
                    </Field>
                    <Field label="Confirm" error={mismatch && pass2 ? 'The passphrases differ' : undefined}>
                      <PasswordInput value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" />
                    </Field>
                  </div>
                )}
                {protect === 'none' && (
                  <p className="flex items-center gap-2 text-sm text-warning">
                    <TriangleAlert className="size-4" /> The private key will be written in clear text.
                  </p>
                )}
                {needCurrent && (
                  <Field label="Current passphrase" hint="The passphrase of this key is not remembered in the vault.">
                    <PasswordInput value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="off" />
                  </Field>
                )}
              </div>
            )}
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
        )}
        <DialogFooter>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          {k && (
            <>
              <Button variant="secondary" onClick={() => void run('copy')} disabled={busy || mismatch || (needCurrent && !current) || (def.private && protect === 'new' && !pass)}>
                <Copy /> Copy
              </Button>
              <Button onClick={() => void run('save')} loading={busy} disabled={mismatch || (needCurrent && !current) || (def.private && protect === 'new' && !pass)}>
                <Download /> Save file
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
