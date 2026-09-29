/*
 * OpenSSH user certificate of a stored key (SSH-5): details with a live expiry countdown, attach / replace (the
 * id_*-cert.pub file, validated against the key and its CA signature), download, detach. Termstead presents the
 * certificate (before the plain key) whenever the key is used to log in.
 */
import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { BadgeCheck, Copy, Download, PenLine, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Spinner } from '@/components/ui/spinner'
import { errorMessage } from '@/lib/utils'
import { inspectKey, invalidateKeys, updateKey, useKeys } from '../api'
import { CertificateDetails, KeyTextInput, SectionLabel } from '../components'
import { openKeysDialog } from '../store'
import type { InspectResult } from '../types'
import { copyWithToast, downloadText } from '../util'

export default function CertificateDialog({ keyId, onClose }: { keyId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const { data: keys, isLoading } = useKeys()
  const k = keys?.find((x) => x.id === keyId)
  const [replacing, setReplacing] = useState(false)
  const [text, setText] = useState('')
  const [info, setInfo] = useState<InspectResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let cancelled = false
    const t = setTimeout(() => {
      if (!text.trim()) {
        setInfo(null)
        setError(null)
        return
      }
      inspectKey(text)
        .then((r) => {
          if (cancelled) return
          setInfo(r)
          setError(
            r.kind !== 'certificate'
              ? 'Not an OpenSSH certificate (expected the contents of an id_*-cert.pub file).'
              : r.certificate?.type !== 'user'
                ? 'This is a host certificate; only user certificates can be attached to a key.'
                : k && r.fingerprint !== k.fingerprint
                  ? 'This certificate was issued for another key.'
                  : null,
          )
        })
        .catch((err) => {
          if (cancelled) return
          setInfo(null)
          setError(errorMessage(err))
        })
    }, 250)
    return () => {
      cancelled = true
      clearTimeout(t)
    }
  }, [text, k])

  const attach = async () => {
    if (!k || error || !info) return
    setBusy(true)
    try {
      await updateKey(k.id, { certificate: text })
      invalidateKeys(qc)
      toast.success('Certificate attached')
      setReplacing(false)
      setText('')
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const detach = async () => {
    if (!k) return
    if (!(await confirm({ title: 'Detach the certificate?', description: 'Termstead will log in with the plain key only.', confirmLabel: 'Detach', destructive: true }))) return
    try {
      await updateKey(k.id, { certificate: '' })
      invalidateKeys(qc)
      toast.success('Certificate detached')
    } catch (err) {
      toast.error('Could not detach the certificate', { description: errorMessage(err) })
    }
  }

  const certFile = k ? `${k.name.replace(/[^\w.-]+/g, '-').toLowerCase() || 'id_' + k.type}-cert.pub` : 'cert.pub'
  const editing = replacing || !k?.certificate

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <BadgeCheck className="size-4.5 text-primary" /> Certificate{k ? ` of “${k.name}”` : ''}
          </DialogTitle>
          <DialogDescription>
            An OpenSSH user certificate lets servers that trust its CA accept this key without listing it in authorized_keys.
          </DialogDescription>
        </DialogHeader>
        {!k ? (
          isLoading ? (
            <div className="flex justify-center py-8">
              <Spinner />
            </div>
          ) : (
            <EmptyState size="sm" title="Key not found" />
          )
        ) : (
          <DialogBody className="grid gap-4">
            {k.certificateInfo && !replacing && <CertificateDetails info={k.certificateInfo} />}
            {k.certificate && !k.certificateInfo && !replacing && (
              <p className="text-sm text-warning">The stored certificate can no longer be read; replace it.</p>
            )}
            {editing && (
              <div className="grid gap-2">
                <SectionLabel>{k.certificate ? 'Replace with' : 'Attach a certificate'}</SectionLabel>
                <KeyTextInput value={text} onChange={setText} rows={4} accept=".pub" placeholder="ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQt…" />
                {error && (
                  <p role="alert" className="text-sm text-destructive">
                    {error}
                  </p>
                )}
                {!error && info?.certificate && <CertificateDetails info={info.certificate} />}
              </div>
            )}
          </DialogBody>
        )}
        <DialogFooter className="sm:justify-between">
          <div className="flex flex-wrap gap-2">
            {k?.certificate && !replacing && (
              <>
                <Button variant="ghost" size="sm" onClick={() => void copyWithToast(k.certificate!, 'Certificate copied')}>
                  <Copy /> Copy
                </Button>
                <Button variant="ghost" size="sm" onClick={() => downloadText(k.certificate! + '\n', certFile)}>
                  <Download /> Save
                </Button>
                <Button variant="ghost" size="sm" className="text-destructive" onClick={() => void detach()}>
                  <Trash2 /> Detach
                </Button>
              </>
            )}
            {k && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  onClose()
                  openKeysDialog('sign', { subjectKeyId: k.id })
                }}
              >
                <PenLine /> Sign with a CA key…
              </Button>
            )}
          </div>
          <div className="flex gap-2">
            {k?.certificate && !replacing ? (
              <>
                <Button variant="secondary" onClick={() => setReplacing(true)}>
                  Replace…
                </Button>
                <Button onClick={onClose}>Done</Button>
              </>
            ) : (
              <>
                <Button variant="secondary" onClick={() => (replacing ? setReplacing(false) : onClose())}>
                  Cancel
                </Button>
                <Button onClick={() => void attach()} loading={busy} disabled={!info || !!error || !text.trim()}>
                  Attach
                </Button>
              </>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
