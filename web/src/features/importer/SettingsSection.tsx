/*
 * Settings → Import & Export: quick actions, the optional ~/.ssh/config live sync (SSH-36, desktop mode, admin), and
 * admin backup / restore (IMP-4).
 */
import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Database, Download, FileUp, FolderInput, KeyRound, RefreshCw, TriangleAlert, Upload } from 'lucide-react'
import { isApiError } from '@/api/client'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { PasswordInput } from '@/components/ui/password-input'
import { Spinner } from '@/components/ui/spinner'
import { SwitchField } from '@/components/ui/switch'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { bufferToBase64, discardRestore, downloadBackup, restore, restoreStatus, setSync, syncStatus, type RestoreResult } from './api'
import { openExportDialog, openImportWizard } from './store'
import type { SyncStatus } from './types'

const MIN_PASSPHRASE = 8
const SYNC_KEY = ['importer', 'sync'] as const
const RESTORE_KEY = ['importer', 'restore'] as const

export default function ImporterSettings() {
  const isAdmin = useAuthStore((s) => s.user?.role === 'admin')
  const isDesktop = useAuthStore((s) => s.state?.mode === 'desktop')
  return (
    <div className="grid gap-6">
      <section className="grid gap-3">
        <div>
          <h3 className="text-md font-semibold">Import & Export</h3>
          <p className="text-sm text-muted-foreground">
            Bring sessions over from MobaXterm, PuTTY, ~/.ssh/config and other clients, or save yours to a file.
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" onClick={() => openImportWizard()}>
            <FolderInput className="size-4" /> Import sessions…
          </Button>
          <Button variant="secondary" onClick={() => openExportDialog()}>
            <Download className="size-4" /> Export sessions…
          </Button>
        </div>
      </section>

      {isDesktop && isAdmin && <SSHConfigSync />}
      {isAdmin && <BackupRestore />}
    </div>
  )
}

function SSHConfigSync() {
  const qc = useQueryClient()
  const { data, isError, error } = useQuery<SyncStatus>({
    queryKey: SYNC_KEY,
    queryFn: syncStatus,
    // While enabled, refresh so changes picked up by the watcher (every few seconds) show up here.
    refetchInterval: (q) => (q.state.data?.enabled ? 10_000 : false),
  })
  const mut = useMutation({
    mutationFn: (enabled: boolean) => setSync(enabled),
    onSuccess: (st) => {
      qc.setQueryData(SYNC_KEY, st)
      if (st.enabled && !st.lastError) toast.success(`Synced ${st.synced} host${st.synced === 1 ? '' : 's'} from ~/.ssh/config`)
    },
    onError: (err) => toast.error('Could not change live sync', { description: errorMessage(err) }),
  })
  if (isError) {
    return (
      <p className="flex items-center gap-1.5 border-t pt-5 text-sm text-destructive">
        <TriangleAlert className="size-4 shrink-0" /> Live sync status unavailable: {errorMessage(error)}
      </p>
    )
  }
  if (!data?.supported) return null
  return (
    <section className="grid gap-3 border-t pt-5">
      <div>
        <h3 className="flex items-center gap-2 text-md font-semibold">
          <RefreshCw className="size-4 text-primary" /> Live sync from ~/.ssh/config
        </h3>
        <p className="text-sm text-muted-foreground">
          Mirror the hosts of <code className="rounded bg-muted px-1">{data.path}</code> (and its Include files) into a “~/.ssh/config”
          session folder, updated whenever the file changes. Your renames, colours and favourites on synced sessions are kept; hosts
          removed from the file are removed from the folder.
        </p>
      </div>
      <SwitchField
        checked={data.enabled}
        onCheckedChange={(v) => mut.mutate(v === true)}
        disabled={mut.isPending}
        label="Keep sessions in sync with ~/.ssh/config"
        description={
          !data.exists
            ? 'The file does not exist yet; it will sync once it appears.'
            : data.lastSync
              ? `Last synced ${new Date(data.lastSync).toLocaleString()} · ${data.synced} host${data.synced === 1 ? '' : 's'}`
              : data.enabled
                ? 'Waiting for the first sync…'
                : 'Not synced yet.'
        }
      />
      {data.lastError && (
        <p role="alert" className="flex items-center gap-1.5 text-sm text-destructive">
          <TriangleAlert className="size-4 shrink-0" /> {data.lastError}
        </p>
      )}
    </section>
  )
}

function BackupRestore() {
  const [encrypt, setEncrypt] = useState(true)
  const [includeKey, setIncludeKey] = useState(false)
  const [pass, setPass] = useState('')
  const [pass2, setPass2] = useState('')
  const [busy, setBusy] = useState<'backup' | 'restore' | null>(null)
  const [result, setResult] = useState<RestoreResult | null>(null)
  const [pending, setPending] = useState<{ name: string; b64: string } | null>(null)
  const [restorePass, setRestorePass] = useState('')
  const [restoreError, setRestoreError] = useState<string | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const showBackup = useDelayedFlag(busy === 'backup')
  const showRestore = useDelayedFlag(busy === 'restore')
  const qc = useQueryClient()
  const staged = useQuery({ queryKey: RESTORE_KEY, queryFn: restoreStatus })
  const discard = useMutation({
    mutationFn: discardRestore,
    onSuccess: () => {
      setResult(null)
      toast.success('Staged restore discarded')
    },
    onError: (err) => toast.error('Could not discard the staged restore', { description: errorMessage(err) }),
    onSettled: () => void qc.invalidateQueries({ queryKey: RESTORE_KEY }),
  })
  const showDiscard = useDelayedFlag(discard.isPending)

  const passInvalid = encrypt && (pass.length < MIN_PASSPHRASE || pass !== pass2)

  const doBackup = async () => {
    if (passInvalid) return
    setBusy('backup')
    try {
      await downloadBackup({ includeSystemKey: encrypt && includeKey, passphrase: encrypt ? pass : undefined })
      toast.success(encrypt ? 'Encrypted backup downloaded' : 'Backup downloaded')
    } catch (err) {
      toast.error('Backup failed', { description: errorMessage(err) })
    } finally {
      setBusy(null)
    }
  }

  const sendRestore = async (file: { name: string; b64: string }, passphrase?: string) => {
    setBusy('restore')
    setRestoreError(null)
    try {
      const res = await restore(file.b64, passphrase)
      setResult(res)
      setPending(null)
      setRestorePass('')
      void qc.invalidateQueries({ queryKey: RESTORE_KEY })
      toast.success('Restore staged', { description: 'It is applied the next time NexTerm starts.' })
    } catch (err) {
      if (isApiError(err) && (err.code === 'passphrase_required' || err.code === 'wrong_password')) {
        setPending(file)
        setRestoreError(err.code === 'wrong_password' ? 'Wrong passphrase — try again.' : null)
      } else {
        setPending(null)
        toast.error('Restore failed', { description: errorMessage(err) })
      }
    } finally {
      setBusy(null)
    }
  }

  const onRestoreFile = async (file: File) => {
    setResult(null)
    setPending(null)
    try {
      await sendRestore({ name: file.name, b64: bufferToBase64(await file.arrayBuffer()) })
    } catch (err) {
      toast.error('Could not read the file', { description: errorMessage(err) })
    }
  }

  return (
    <section className="grid gap-3 border-t pt-5">
      <div>
        <h3 className="flex items-center gap-2 text-md font-semibold">
          <Database className="size-4 text-primary" /> Backup & restore
        </h3>
        <p className="text-sm text-muted-foreground">
          A backup is a consistent snapshot of the whole database (sessions, keys, settings, users, audit log). Secrets inside stay
          encrypted; to read them on another machine the archive must also carry the system key — only inside a passphrase-encrypted
          archive.
        </p>
      </div>
      <div className="grid gap-3 rounded-lg border bg-card/60 p-3.5">
        <CheckboxField
          checked={encrypt}
          onCheckedChange={(v) => {
            setEncrypt(v === true)
            if (v !== true) setIncludeKey(false)
          }}
          label="Encrypt the backup with a passphrase"
          description="Recommended. The archive is sealed with argon2id + XChaCha20-Poly1305."
        />
        <CheckboxField
          checked={includeKey}
          disabled={!encrypt}
          onCheckedChange={(v) => setIncludeKey(v === true)}
          label="Include the system key"
          description="Needed to restore on a different machine (without a master password). Keep the archive and its passphrase safe."
        />
        {encrypt && (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Archive passphrase" hint={`At least ${MIN_PASSPHRASE} characters.`} error={pass.length > 0 && pass.length < MIN_PASSPHRASE ? `Use at least ${MIN_PASSPHRASE} characters` : undefined}>
              <PasswordInput value={pass} onChange={(e) => setPass(e.target.value)} autoComplete="new-password" />
            </Field>
            <Field label="Confirm" error={pass2.length > 0 && pass !== pass2 ? 'Passphrases do not match' : undefined}>
              <PasswordInput value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" />
            </Field>
          </div>
        )}
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" onClick={doBackup} disabled={busy !== null || passInvalid}>
            {showBackup ? <Spinner immediate className="size-4" /> : <Download className="size-4" />} Download backup
          </Button>
          <Button variant="secondary" onClick={() => fileRef.current?.click()} disabled={busy !== null}>
            {showRestore ? <Spinner immediate className="size-4" /> : <Upload className="size-4" />} Restore from file…
          </Button>
          <input
            ref={fileRef}
            type="file"
            accept=".ntbak,.db,.sqlite,application/octet-stream"
            className="hidden"
            onChange={(e) => {
              if (e.target.files?.[0]) void onRestoreFile(e.target.files[0])
              e.target.value = ''
            }}
          />
        </div>
      </div>

      {pending && (
        <div className="grid gap-2 rounded-lg border border-info/40 bg-info/10 p-3">
          <p className="flex items-center gap-1.5 text-sm font-medium">
            <KeyRound className="size-4 text-info" /> {pending.name} is encrypted
          </p>
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Passphrase" className="min-w-56 flex-1">
              <PasswordInput
                value={restorePass}
                onChange={(e) => setRestorePass(e.target.value)}
                autoFocus
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && restorePass && busy === null) void sendRestore(pending, restorePass)
                }}
              />
            </Field>
            <Button onClick={() => sendRestore(pending, restorePass)} disabled={!restorePass || busy !== null}>
              {showRestore ? <Spinner immediate className="size-4" /> : <Upload className="size-4" />} Stage restore
            </Button>
            <Button variant="ghost" onClick={() => setPending(null)} disabled={busy !== null}>
              Cancel
            </Button>
          </div>
          {restoreError && (
            <p role="alert" className="text-sm text-destructive">
              {restoreError}
            </p>
          )}
        </div>
      )}

      {(result || staged.data?.pending) && (
        <div className="rounded-lg border border-info/40 bg-info/10 p-3">
          <p className="flex items-center gap-1.5 text-sm font-medium">
            <FileUp className="size-4 text-info" /> Restore staged — nothing was changed yet. It is applied the next time NexTerm starts.
          </p>
          {result ? (
            <ol className="mt-1 grid gap-0.5 text-sm text-muted-foreground">
              {result.instructions.map((s, i) => (
                <li key={i} className="break-words">
                  {s}
                </li>
              ))}
            </ol>
          ) : (
            staged.data?.stagedAt && (
              <p className="mt-1 text-sm text-muted-foreground">
                Staged {new Date(staged.data.stagedAt).toLocaleString()}
                {staged.data.stagedBy ? ` by ${staged.data.stagedBy}` : ''}
                {staged.data.systemKey ? ' (with the system key)' : ''}.
              </p>
            )
          )}
          {result?.warnings?.map((w, i) => (
            <p key={i} className="mt-1 flex items-start gap-1.5 text-sm text-warning">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" /> {w}
            </p>
          ))}
          <Button variant="ghost" size="sm" className="mt-2" onClick={() => discard.mutate()} disabled={discard.isPending}>
            {showDiscard && <Spinner immediate className="size-4" />} Discard staged restore
          </Button>
        </div>
      )}
    </section>
  )
}
