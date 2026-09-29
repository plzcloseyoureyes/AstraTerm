/*
 * MobaKeyGen-style generator (TOOL-1): type / size / comment / passphrase → the key is generated as a server-side draft
 * (never stored until asked) and shown with its public key, SHA256 + MD5 fingerprints and OpenSSH randomart; it can
 * then be saved as files (public, OpenSSH / PuTTY / PEM private) and/or stored in the AstraTerm vault.
 */
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ChevronDown, Download, KeyRound, RotateCcw, Server, ShieldCheck, Sparkles } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput, PasswordStrengthMeter } from '@/components/ui/password-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { discardDraft, exportDraft, generateDraft, invalidateKeys, storeDraft } from '../api'
import { CopyField, PublicKeyBox, Randomart, SectionLabel } from '../components'
import { keysSettings } from '../settings'
import { openKeysDialog } from '../store'
import type { GeneratedKeyType, KeyDraft, StoredKey } from '../types'
import { defaultKeyComment, downloadText, keyTypeLabel, toastError } from '../util'

const TYPES: { value: GeneratedKeyType; label: string }[] = [
  { value: 'ed25519', label: 'Ed25519' },
  { value: 'rsa', label: 'RSA' },
  { value: 'ecdsa', label: 'ECDSA' },
]

const BITS: Record<GeneratedKeyType, { value: string; label: string }[]> = {
  ed25519: [{ value: '256', label: '256 bits' }],
  rsa: [
    { value: '2048', label: '2048 bits' },
    { value: '3072', label: '3072 bits' },
    { value: '4096', label: '4096 bits' },
  ],
  ecdsa: [
    { value: '256', label: 'P-256' },
    { value: '384', label: 'P-384' },
    { value: '521', label: 'P-521' },
  ],
}

const HINTS: Record<GeneratedKeyType, string> = {
  ed25519: 'Recommended: small, fast and secure. Supported by OpenSSH 6.5+ and PuTTY 0.68+.',
  rsa: 'For older servers and devices. Use at least 3072 bits for new keys.',
  ecdsa: 'NIST curves; for environments that require them (e.g. FIPS).',
}

export default function GenerateDialog({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const s = keysSettings.get()
  const [type, setType] = useState<GeneratedKeyType>(s.defaultType)
  const [bits, setBits] = useState<Record<GeneratedKeyType, string>>({ ed25519: '256', rsa: String(s.defaultRsaBits), ecdsa: String(s.defaultEcdsaBits) })
  const [comment, setComment] = useState('')
  const [pass, setPass] = useState('')
  const [pass2, setPass2] = useState('')
  const [busy, setBusy] = useState(false)
  const [draft, setDraft] = useState<KeyDraft | null>(null)
  const [name, setName] = useState('')
  const [remember, setRemember] = useState(s.rememberPassphrase)
  const [stored, setStored] = useState<StoredKey | null>(null)
  const [storing, setStoring] = useState(false)
  const draftRef = useRef<KeyDraft | null>(null)
  draftRef.current = stored ? null : draft

  // Discard an unstored draft when the dialog goes away.
  useEffect(() => () => {
    if (draftRef.current) void discardDraft(draftRef.current.draftId).catch(() => undefined)
  }, [])

  const mismatch = pass2 !== '' && pass !== pass2
  const unconfirmed = pass !== '' && pass2 === ''
  const commentPlaceholder = defaultKeyComment(type)

  const generate = async () => {
    if (pass !== pass2) return
    setBusy(true)
    try {
      if (draft) await discardDraft(draft.draftId).catch(() => undefined)
      const d = await generateDraft({ type, bits: Number(bits[type]), comment: comment.trim() || commentPlaceholder, passphrase: pass })
      setDraft(d)
      setStored(null)
      setName((n) => n || d.comment)
    } catch (err) {
      toastError('Could not generate the key', err)
    } finally {
      setBusy(false)
    }
  }

  const save = async (format: string, ppkVersion?: number) => {
    if (!draft) return
    try {
      const res = await exportDraft(draft.draftId, { format, ppkVersion, name: name || draft.comment })
      downloadText(res.content, res.filename, res.mime)
      if (!res.encrypted && format !== 'public' && format !== 'rfc4716') toast.warning('The private key file is not protected by a passphrase')
    } catch (err) {
      toastError('Could not save the key', err)
    }
  }

  const store = async () => {
    if (!draft) return
    setStoring(true)
    try {
      const k = await storeDraft(draft.draftId, { name: name.trim() || draft.comment, rememberPassphrase: remember })
      setStored(k)
      invalidateKeys(qc)
      toast.success(`Stored “${k.name}” in the vault`)
    } catch (err) {
      toastError('Could not store the key', err)
    } finally {
      setStoring(false)
    }
  }

  const reset = () => {
    if (draft && !stored) void discardDraft(draft.draftId).catch(() => undefined)
    setDraft(null)
    setStored(null)
    setName('')
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="xl" onInteractOutside={(e) => draft && e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>
            <Sparkles className="size-4.5 text-primary" /> Generate SSH key
          </DialogTitle>
          <DialogDescription>
            {draft ? 'Save the key files you need, and store the key in the AstraTerm vault to use it in sessions.' : 'Choose the key type and protect it with an optional passphrase.'}
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="@container grid gap-5">
          {!draft ? (
            <form
              id="keygen-form"
              className="grid gap-4"
              onSubmit={(e) => {
                e.preventDefault()
                void generate()
              }}
            >
              <div className="grid gap-4 @md:grid-cols-[1fr_12rem]">
                <Field label="Key type" hint={HINTS[type]}>
                  <SegmentedControl<GeneratedKeyType> aria-label="Key type" value={type} onValueChange={setType} options={TYPES} fullWidth />
                </Field>
                <Field label="Size">
                  <SimpleSelect
                    value={bits[type]}
                    onValueChange={(v) => setBits((b) => ({ ...b, [type]: v }))}
                    options={BITS[type]}
                    disabled={type === 'ed25519'}
                    aria-label="Key size"
                  />
                </Field>
              </div>
              <Field label="Comment" hint="Stored in the public key; usually who the key belongs to.">
                <Input value={comment} onChange={(e) => setComment(e.target.value)} placeholder={commentPlaceholder} maxLength={200} spellCheck={false} />
              </Field>
              <div className="grid gap-4 @md:grid-cols-2">
                <Field label="Passphrase" hint="Optional. Protects exported key files (and the stored key).">
                  <PasswordInput value={pass} onChange={(e) => setPass(e.target.value)} generate onGenerate={(p) => setPass2(p)} autoComplete="new-password" />
                </Field>
                <Field label="Confirm passphrase" error={mismatch ? 'The passphrases differ' : undefined} hint={unconfirmed ? 'Type the passphrase again.' : undefined}>
                  <PasswordInput value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" />
                </Field>
              </div>
              {pass && <PasswordStrengthMeter password={pass} />}
            </form>
          ) : (
            <div className="grid gap-5 @xl:grid-cols-[1fr_auto]">
              <div className="grid content-start gap-4">
                <div className="grid gap-1.5">
                  <SectionLabel>Public key for pasting into authorized_keys</SectionLabel>
                  <PublicKeyBox value={draft.publicKey} filename={`${(name || draft.comment).replace(/[^\w.-]+/g, '-') || 'id_' + draft.type}.pub`} rows={4} />
                </div>
                <CopyField label="SHA256 fingerprint" value={draft.fingerprint} />
                <CopyField label="MD5 fingerprint" value={draft.fingerprintMd5} />
              </div>
              <div className="grid content-start justify-items-center gap-2">
                <SectionLabel>Randomart</SectionLabel>
                <Randomart fingerprint={draft.fingerprint} type={draft.type} bits={draft.bits} />
                <div className="text-sm text-muted-foreground">
                  {keyTypeLabel(draft.type, draft.bits)}
                  {draft.hasPassphrase ? ' · passphrase' : ' · no passphrase'}
                </div>
              </div>
              <div className="grid gap-3 rounded-lg border bg-card/60 p-3.5 @xl:col-span-2">
                <SectionLabel>Store in AstraTerm</SectionLabel>
                {stored ? (
                  <div className="flex flex-wrap items-center gap-2 text-base">
                    <ShieldCheck className="size-4 text-success" />
                    Stored as <strong>“{stored.name}”</strong> — select it as the key of your sessions or identities.
                    <Button
                      size="sm"
                      variant="secondary"
                      className="ml-auto"
                      onClick={() => {
                        onClose()
                        openKeysDialog('install', { keyId: stored.id })
                      }}
                    >
                      <Server /> Install on a server…
                    </Button>
                  </div>
                ) : (
                  <div className="grid items-end gap-3 @md:grid-cols-[1fr_auto]">
                    <Field label="Name">
                      <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={draft.comment} maxLength={200} />
                    </Field>
                    <Button onClick={() => void store()} loading={storing}>
                      <KeyRound /> Store in vault
                    </Button>
                    {draft.hasPassphrase && (
                      <CheckboxField
                        className="@md:col-span-2"
                        checked={remember}
                        onCheckedChange={(v) => setRemember(v === true)}
                        label="Remember the passphrase in the vault"
                        description="Otherwise AstraTerm asks for it whenever the key is used."
                      />
                    )}
                  </div>
                )}
              </div>
            </div>
          )}
        </DialogBody>
        <DialogFooter className="sm:justify-between">
          {draft ? (
            <>
              <Button variant="ghost" onClick={reset}>
                <RotateCcw /> Generate another
              </Button>
              <div className="flex flex-wrap gap-2">
                <Button variant="secondary" onClick={() => void save('public')}>
                  <Download /> Save public key
                </Button>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button variant="secondary">
                      <Download /> Save private key <ChevronDown className="size-3.5 opacity-70" />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuLabel>{draft.hasPassphrase ? 'Protected by your passphrase' : 'Unprotected (no passphrase)'}</DropdownMenuLabel>
                    <DropdownMenuItem onSelect={() => void save('openssh')}>OpenSSH (id_{draft.type})</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => void save('ppk', 3)}>PuTTY .ppk (version 3)</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => void save('ppk', 2)}>PuTTY .ppk (version 2, older PuTTY)</DropdownMenuItem>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem onSelect={() => void save('pem')}>PEM (traditional)</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => void save('pkcs8')}>PKCS#8</DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
                <Button onClick={onClose}>{stored ? 'Done' : 'Close'}</Button>
              </div>
            </>
          ) : (
            <>
              <span className="text-sm text-muted-foreground">{type === 'rsa' && bits.rsa === '4096' ? 'RSA 4096 can take a few seconds.' : ''}</span>
              <div className="flex gap-2">
                <Button variant="secondary" onClick={onClose}>
                  Cancel
                </Button>
                <Button type="submit" form="keygen-form" loading={busy} disabled={mismatch || unconfirmed}>
                  <Sparkles /> Generate
                </Button>
              </div>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
