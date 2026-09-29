/*
 * Sign an OpenSSH certificate with a stored key acting as CA (TOOL-1 "sign certificates", like ssh-keygen -s):
 * user certificates (principals = login names, permissions, force-command / source-address) or host certificates
 * (principals = host names), with a validity period. A user certificate of one of your keys can be attached to it.
 */
import { useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Copy, Download, PenLine } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox, CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { TagInput } from '@/components/ui/tag-input'
import { Textarea } from '@/components/ui/textarea'
import { errorMessage } from '@/lib/utils'
import { invalidateKeys, signCertificate, useKeys } from '../api'
import { CertificateDetails, KeySelect, KeyTextInput, SectionLabel } from '../components'
import type { SignResult } from '../types'
import { copyWithToast, downloadText } from '../util'

const VALIDITY: { value: string; label: string; ms?: number }[] = [
  { value: '1h', label: '1 hour', ms: 3600_000 },
  { value: '8h', label: '8 hours', ms: 8 * 3600_000 },
  { value: '1d', label: '1 day', ms: 86_400_000 },
  { value: '7d', label: '1 week', ms: 7 * 86_400_000 },
  { value: '30d', label: '30 days', ms: 30 * 86_400_000 },
  { value: '365d', label: '1 year', ms: 365 * 86_400_000 },
  { value: 'forever', label: 'Forever' },
  { value: 'custom', label: 'Custom…' },
]

const PERMISSIONS: { ext: string; label: string }[] = [
  { ext: 'permit-pty', label: 'Terminal (PTY)' },
  { ext: 'permit-port-forwarding', label: 'Port forwarding' },
  { ext: 'permit-agent-forwarding', label: 'Agent forwarding' },
  { ext: 'permit-X11-forwarding', label: 'X11 forwarding' },
  { ext: 'permit-user-rc', label: 'Run ~/.ssh/rc' },
]

function toLocalInput(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

export default function SignDialog({ caKeyId, subjectKeyId, onClose }: { caKeyId?: string; subjectKeyId?: string; onClose: () => void }) {
  const qc = useQueryClient()
  const { data: keys } = useKeys()
  const [caId, setCaId] = useState(caKeyId ?? '')
  const [subjectMode, setSubjectMode] = useState<'stored' | 'paste'>('stored')
  const [subjectId, setSubjectId] = useState(subjectKeyId ?? '')
  const [subjectText, setSubjectText] = useState('')
  const [certType, setCertType] = useState<'user' | 'host'>('user')
  const [identity, setIdentity] = useState('')
  const [principals, setPrincipals] = useState<string[]>([])
  const [validity, setValidity] = useState('30d')
  const [from, setFrom] = useState(() => toLocalInput(new Date()))
  const [until, setUntil] = useState(() => toLocalInput(new Date(Date.now() + 30 * 86_400_000)))
  const [exts, setExts] = useState<string[]>(PERMISSIONS.map((p) => p.ext))
  const [forceCommand, setForceCommand] = useState('')
  const [sourceAddress, setSourceAddress] = useState('')
  const [caPass, setCaPass] = useState('')
  const [attach, setAttach] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<SignResult | null>(null)

  const ca = keys?.find((k) => k.id === caId)
  const subject = keys?.find((k) => k.id === subjectId)
  const needCaPass = !!ca && ca.hasPassphrase && !ca.passphraseSaved
  const canAttach = subjectMode === 'stored' && certType === 'user'
  const subjectReady = subjectMode === 'stored' ? !!subjectId && subjectId !== caId : subjectText.trim() !== ''

  const validityWindow = useMemo(() => {
    const now = Date.now()
    if (validity === 'forever') return {}
    if (validity === 'custom') {
      const a = Date.parse(from)
      const b = Date.parse(until)
      return { validAfter: Number.isNaN(a) ? undefined : new Date(a).toISOString(), validBefore: Number.isNaN(b) ? undefined : new Date(b).toISOString() }
    }
    const v = VALIDITY.find((x) => x.value === validity)
    return { validBefore: new Date(now + (v?.ms ?? 0)).toISOString() }
  }, [validity, from, until])

  const sign = async () => {
    if (!ca) return
    setBusy(true)
    setError(null)
    try {
      const res = await signCertificate(ca.id, {
        ...(subjectMode === 'stored' ? { subjectKeyId: subjectId } : { publicKey: subjectText }),
        certType,
        identity: identity.trim() || (certType === 'host' ? principals[0] ?? '' : subject?.name ?? ''),
        principals,
        ...validityWindow,
        criticalOptions: certType === 'user' ? { 'force-command': forceCommand.trim(), 'source-address': sourceAddress.trim() } : undefined,
        extensions: certType === 'user' ? exts : undefined,
        caPassphrase: needCaPass ? caPass : undefined,
        attach: canAttach && attach,
      })
      setResult(res)
      if (res.attached) {
        invalidateKeys(qc)
        toast.success(`Certificate attached to “${subject?.name}”`)
      }
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="xl">
        <DialogHeader>
          <DialogTitle>
            <PenLine className="size-4.5 text-primary" /> Sign a certificate
          </DialogTitle>
          <DialogDescription>
            The CA key signs another key's public key. Servers that trust the CA (TrustedUserCAKeys, or @cert-authority for host
            certificates) accept the certificate.
          </DialogDescription>
        </DialogHeader>
        {result ? (
          <DialogBody className="grid gap-4">
            <div className="grid gap-1.5">
              <SectionLabel>Certificate</SectionLabel>
              <Textarea mono readOnly rows={4} value={result.certificate} className="text-xs break-all" onFocus={(e) => e.currentTarget.select()} />
            </div>
            <CertificateDetails info={result.info} />
          </DialogBody>
        ) : (
          <DialogBody className="@container grid gap-4">
            <div className="grid gap-4 @lg:grid-cols-2">
              <Field label="CA key (signs)">
                <KeySelect keys={keys} value={caId} onChange={setCaId} noneLabel="" filter={(k) => k.hasPrivateKey && k.type !== 'dsa'} />
              </Field>
              <Field label="Certificate type">
                <SegmentedControl<'user' | 'host'>
                  aria-label="Certificate type"
                  value={certType}
                  onValueChange={setCertType}
                  options={[
                    { value: 'user', label: 'User' },
                    { value: 'host', label: 'Host' },
                  ]}
                  fullWidth
                />
              </Field>
            </div>
            <div className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <span className="text-sm font-medium">Key to certify</span>
                <SegmentedControl<'stored' | 'paste'>
                  size="sm"
                  aria-label="Key to certify"
                  value={subjectMode}
                  onValueChange={setSubjectMode}
                  options={[
                    { value: 'stored', label: 'My key' },
                    { value: 'paste', label: 'Public key' },
                  ]}
                />
              </div>
              {subjectMode === 'stored' ? (
                <KeySelect keys={keys} value={subjectId} onChange={setSubjectId} noneLabel="" filter={(k) => k.id !== caId} />
              ) : (
                <KeyTextInput value={subjectText} onChange={setSubjectText} rows={3} accept=".pub" placeholder="ssh-ed25519 AAAA… user@host" />
              )}
            </div>
            <div className="grid gap-4 @lg:grid-cols-2">
              <Field label="Principals" hint={certType === 'user' ? 'Login names the certificate is valid for.' : 'Host names the certificate is valid for.'}>
                <TagInput value={principals} onChange={setPrincipals} placeholder={certType === 'user' ? 'e.g. deploy, root' : 'e.g. db1.example.com'} />
              </Field>
              <Field label="Identity (key ID)" hint="Logged by the server when the certificate is used.">
                <Input value={identity} onChange={(e) => setIdentity(e.target.value)} placeholder={certType === 'host' ? principals[0] || 'host.example.com' : subject?.name || 'alice@example.com'} maxLength={256} />
              </Field>
            </div>
            <div className="grid items-end gap-4 @lg:grid-cols-[12rem_1fr]">
              <Field label="Valid for">
                <SimpleSelect value={validity} onValueChange={setValidity} options={VALIDITY.map((v) => ({ value: v.value, label: v.label }))} aria-label="Validity" />
              </Field>
              {validity === 'custom' && (
                <div className="grid grid-cols-2 gap-2">
                  <Field label="From">
                    <Input type="datetime-local" value={from} onChange={(e) => setFrom(e.target.value)} />
                  </Field>
                  <Field label="Until">
                    <Input type="datetime-local" value={until} onChange={(e) => setUntil(e.target.value)} />
                  </Field>
                </div>
              )}
            </div>
            {certType === 'user' && (
              <div className="grid gap-3 rounded-lg border bg-card/60 p-3.5">
                <SectionLabel>Permissions</SectionLabel>
                <div className="flex flex-wrap gap-x-5 gap-y-2">
                  {PERMISSIONS.map((p) => (
                    <label key={p.ext} className="flex items-center gap-2 text-base select-none">
                      <Checkbox checked={exts.includes(p.ext)} onCheckedChange={(v) => setExts((cur) => (v === true ? [...cur, p.ext] : cur.filter((x) => x !== p.ext)))} />
                      {p.label}
                    </label>
                  ))}
                </div>
                <div className="grid gap-3 @lg:grid-cols-2">
                  <Field label="Force command" hint="Only this command may run (optional).">
                    <Input value={forceCommand} onChange={(e) => setForceCommand(e.target.value)} placeholder="/usr/local/bin/backup" className="font-mono" />
                  </Field>
                  <Field label="Source addresses" hint="Comma-separated IPs / CIDRs allowed to use it (optional).">
                    <Input value={sourceAddress} onChange={(e) => setSourceAddress(e.target.value)} placeholder="10.0.0.0/8, 192.168.1.5" className="font-mono" />
                  </Field>
                </div>
              </div>
            )}
            {needCaPass && (
              <Field label="CA key passphrase">
                <PasswordInput value={caPass} onChange={(e) => setCaPass(e.target.value)} autoComplete="off" />
              </Field>
            )}
            {canAttach && subject && (
              <CheckboxField checked={attach} onCheckedChange={(v) => setAttach(v === true)} label={`Attach the certificate to “${subject.name}”`} description="Termstead then presents it when the key logs in." />
            )}
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
        )}
        <DialogFooter>
          {result ? (
            <>
              <Button variant="secondary" onClick={() => void copyWithToast(result.certificate.trim(), 'Certificate copied')}>
                <Copy /> Copy
              </Button>
              <Button variant="secondary" onClick={() => downloadText(result.certificate, result.filename)}>
                <Download /> Save {result.filename}
              </Button>
              <Button onClick={onClose}>Done</Button>
            </>
          ) : (
            <>
              <Button variant="secondary" onClick={onClose}>
                Cancel
              </Button>
              <Button onClick={() => void sign()} loading={busy} disabled={!ca || !subjectReady || principals.length === 0 || (needCaPass && !caPass)}>
                <PenLine /> Sign
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
