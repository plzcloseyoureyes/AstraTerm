/*
 * Export dialog (IMP-3): choose a format (JSON / CSV / ssh_config), an optional folder scope, and — for JSON — whether
 * to include secrets, which requires a passphrase (≥ 8 characters) and produces a passphrase-encrypted file (secrets
 * never leave the server in cleartext). The request is a POST, so the passphrase never appears in a URL.
 */
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Download, FileDown, Lock, TriangleAlert } from 'lucide-react'
import { useFolders } from '@/api/folders'
import type { Folder } from '@/api/types'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { PasswordInput } from '@/components/ui/password-input'
import { RadioField, RadioGroup } from '@/components/ui/radio-group'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { downloadExport } from './api'
import type { ExportFormat } from './types'

const MIN_PASSPHRASE = 8
/** Radix Select cannot hold an empty value: "everything / the given selection" uses this sentinel. */
const ALL = '__all__'

const FORMATS: { value: ExportFormat; label: string; description: string; secrets: boolean }[] = [
  {
    value: 'json',
    label: 'Termstead JSON',
    description: 'Full fidelity — folders, sessions, identities, snippets. Re-importable in Termstead.',
    secrets: true,
  },
  { value: 'csv', label: 'CSV', description: 'A spreadsheet of sessions (Termius-compatible columns). No secrets.', secrets: false },
  { value: 'ssh_config', label: 'OpenSSH config', description: 'Host blocks for SSH / SFTP / Mosh sessions, with jump hosts and forwards. No secrets.', secrets: false },
]

export default function ExportDialog({
  initialFormat,
  folderId,
  connectionIds,
  onClose,
}: {
  initialFormat?: ExportFormat
  folderId?: string
  connectionIds?: string[]
  onClose: () => void
}) {
  const { data: allFolders } = useFolders()
  const userId = useAuthStore((s) => s.user?.id)
  const [format, setFormat] = useState<ExportFormat>(initialFormat ?? 'json')
  const [scope, setScope] = useState<string>(folderId ?? ALL)
  const [includeSecrets, setIncludeSecrets] = useState(false)
  const [pass, setPass] = useState('')
  const [pass2, setPass2] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const showBusy = useDelayedFlag(busy)

  const def = FORMATS.find((f) => f.value === format)!
  const secrets = def.secrets && includeSecrets
  const tooShort = secrets && pass.length > 0 && pass.length < MIN_PASSPHRASE
  const mismatch = secrets && pass2.length > 0 && pass !== pass2
  const invalid = secrets && (pass.length < MIN_PASSPHRASE || pass !== pass2)

  // Only the caller's own folders are exported (shared folders of others are not theirs to export).
  const folders = useMemo(() => (allFolders ?? []).filter((f) => f.ownerId === userId), [allFolders, userId])
  const folderOptions = useMemo(() => {
    const opts = [{ value: ALL, label: connectionIds?.length ? `Selected sessions (${connectionIds.length})` : 'Everything I own' }]
    for (const f of folderPaths(folders)) opts.push({ value: f.id, label: f.path })
    return opts
  }, [folders, connectionIds])

  const run = async () => {
    if (invalid) return
    setBusy(true)
    setError(null)
    try {
      await downloadExport({
        format,
        includeSecrets: secrets,
        passphrase: secrets ? pass : undefined,
        folderId: scope === ALL ? undefined : scope,
        connectionIds: scope === ALL ? connectionIds : undefined,
      })
      toast.success(secrets ? 'Encrypted export downloaded' : 'Export downloaded')
      onClose()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <FileDown className="size-4.5 text-primary" /> Export sessions
          </DialogTitle>
          <DialogDescription>Save your sessions to a file you can back up or import elsewhere.</DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-4">
          <RadioGroup value={format} onValueChange={(v) => setFormat(v as ExportFormat)} aria-label="Format">
            {FORMATS.map((f) => (
              <RadioField key={f.value} value={f.value} label={f.label} description={f.description} />
            ))}
          </RadioGroup>

          <Field label="Scope" orientation="horizontal">
            <SimpleSelect value={scope} onValueChange={setScope} options={folderOptions} aria-label="Export scope" />
          </Field>

          {def.secrets && (
            <div className="grid gap-3 rounded-lg border bg-card/60 p-3.5">
              <CheckboxField
                checked={includeSecrets}
                onCheckedChange={(v) => setIncludeSecrets(v === true)}
                label="Include passwords and private keys"
                description="The file is encrypted with a passphrase (argon2id + XChaCha20-Poly1305). You will need it to import."
              />
              {secrets && (
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="Passphrase" hint={`At least ${MIN_PASSPHRASE} characters.`} error={tooShort ? `Use at least ${MIN_PASSPHRASE} characters` : undefined}>
                    <PasswordInput value={pass} onChange={(e) => setPass(e.target.value)} autoComplete="new-password" autoFocus />
                  </Field>
                  <Field label="Confirm" error={mismatch ? 'Passphrases do not match' : undefined}>
                    <PasswordInput
                      value={pass2}
                      onChange={(e) => setPass2(e.target.value)}
                      autoComplete="new-password"
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' && !invalid && !busy) void run()
                      }}
                    />
                  </Field>
                </div>
              )}
              {!secrets && (
                <p className="text-sm text-muted-foreground">Without this option the file contains no passwords, keys or other secrets.</p>
              )}
            </div>
          )}

          {error && (
            <p role="alert" className="flex items-center gap-1.5 text-sm text-destructive">
              <TriangleAlert className="size-4 shrink-0" /> {error}
            </p>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={run} disabled={busy || invalid}>
            {showBusy ? <Spinner immediate className="size-4" /> : secrets ? <Lock className="size-4" /> : <Download className="size-4" />} Export
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function folderPaths(folders: Folder[]): { id: string; path: string }[] {
  const byId = new Map(folders.map((f) => [f.id, f]))
  const pathOf = (f: Folder): string => {
    const parts: string[] = []
    let cur: Folder | undefined = f
    let guard = 0
    while (cur && guard++ < 64) {
      parts.unshift(cur.name)
      cur = cur.parentId ? byId.get(cur.parentId) : undefined
    }
    return parts.join(' / ')
  }
  return folders.map((f) => ({ id: f.id, path: pathOf(f) })).sort((a, b) => a.path.localeCompare(b.path))
}
