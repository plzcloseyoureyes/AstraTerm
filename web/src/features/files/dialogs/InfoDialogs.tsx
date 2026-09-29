/*
 * Properties (with folder size and free space), Checksum (FILE-16: remote hash, compare with a local file hashed in
 * the browser, or a pasted value) and Compress (FILE-9).
 */
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Archive, Check, Copy, Hash, Info, Shield, X } from 'lucide-react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Spinner } from '@/components/ui/spinner'
import { cn, copyText, errorMessage, formatBytes, formatRelativeTime } from '@/lib/utils'
import { checksumOf, invalidateDir, permissionsOf } from '../actions'
import { fsApi, fsKeys, type ChecksumAlgo } from '../api'
import { entryPerm, fileKind, formatExactBytes, formatMtimeLong, isDirLike, modeToOctal } from '../format'
import { withFs } from '../fsHandles'
import { basename, dirname, joinPath, splitName, validateName } from '../paths'
import type { FsContext } from '../types'

function Row({ label, children, mono }: { label: string; children: React.ReactNode; mono?: boolean }) {
  return (
    <>
      <dt className="text-sm text-muted-foreground">{label}</dt>
      <dd className={cn('min-w-0 text-sm break-words', mono && 'font-mono text-xs leading-5')}>{children}</dd>
    </>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// properties
// ---------------------------------------------------------------------------------------------------------------------

export function PropertiesDialog({ ctx, entry, onClose }: { ctx: FsContext; entry: FileEntry; onClose: () => void }) {
  const stat = useQuery({
    queryKey: fsKeys.stat(ctx.handle.id, entry.path),
    queryFn: ({ signal }) => fsApi.stat(ctx.handle.id, entry.path, signal),
    retry: false,
  })
  const e = stat.data ?? entry
  const dir = isDirLike(e)
  const space = useQuery({
    queryKey: ['fs', ctx.handle.id, 'space', entry.path],
    queryFn: () => fsApi.space(ctx.handle.id, entry.path),
    enabled: dir && !!ctx.handle.capabilities.space,
    retry: false,
  })
  const [size, setSize] = useState<{ bytes: number; files: number; dirs: number; done: boolean; capped: boolean } | null>(null)
  const cancel = useRef(false)
  useEffect(
    () => () => {
      cancel.current = true
    },
    [],
  )

  const calculate = async () => {
    cancel.current = false
    const acc = { bytes: 0, files: 0, dirs: 0, done: false, capped: false }
    setSize({ ...acc })
    const queue = [e.path]
    let seen = 0
    const LIMIT = 20_000
    try {
      while (queue.length && !cancel.current) {
        const batch = queue.splice(0, 4)
        const results = await Promise.all(batch.map((p) => withFs(ctx.key, (id) => fsApi.list(id, p)).catch(() => null)))
        for (const r of results) {
          if (!r) continue
          for (const x of r.entries) {
            seen++
            if (x.type === 'dir') {
              acc.dirs++
              queue.push(x.path)
            } else {
              acc.files++
              acc.bytes += x.size || 0
            }
          }
        }
        if (seen > LIMIT) {
          acc.capped = true
          break
        }
        setSize({ ...acc })
      }
    } finally {
      acc.done = true
      if (!cancel.current) setSize({ ...acc })
    }
  }

  const kind = dir ? 'Folder' : e.type === 'symlink' ? 'Symbolic link' : e.type === 'other' ? 'Special file' : `${fileKind(e)[0].toUpperCase()}${fileKind(e).slice(1)} file`
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <Info className="size-4 text-muted-foreground" /> <span className="truncate">{e.name || e.path}</span>
          </DialogTitle>
          <DialogDescription>Properties on {ctx.label}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <dl className="grid grid-cols-[8rem_minmax(0,1fr)] gap-x-4 gap-y-2">
            <Row label="Location" mono>
              {e.path === '/' ? '/' : dirname(e.path)}
            </Row>
            <Row label="Type">
              {kind}
              <Spinner active={stat.isFetching} className="ml-2 inline size-3" />
            </Row>
            {e.type === 'symlink' && (
              <Row label="Points to" mono>
                {e.linkTarget ?? '—'}
                {e.linkType === 'broken' && <span className="ml-1 text-destructive">(broken)</span>}
              </Row>
            )}
            <Row label="Size">
              {dir ? (
                size ? (
                  <span className="tabular">
                    {formatBytes(size.bytes)} in {size.files.toLocaleString()} files, {size.dirs.toLocaleString()} folders
                    <Spinner active={!size.done} className="ml-2 inline size-3" />
                    {size.capped && <span className="text-muted-foreground"> (stopped after 20,000 items)</span>}
                  </span>
                ) : (
                  <Button size="xs" variant="secondary" onClick={() => void calculate()}>
                    Calculate
                  </Button>
                )
              ) : (
                <span className="tabular">
                  {formatBytes(e.size)} <span className="text-muted-foreground">({formatExactBytes(e.size)})</span>
                </span>
              )}
            </Row>
            {dir && space.data && (
              <Row label="Free space">
                <span className="tabular">
                  {formatBytes(space.data.avail)} of {formatBytes(space.data.total)}
                </span>
              </Row>
            )}
            <Row label="Modified">
              {e.mtime ? (
                <>
                  {formatMtimeLong(e.mtime)} <span className="text-muted-foreground">({formatRelativeTime(e.mtime)})</span>
                </>
              ) : (
                '—'
              )}
            </Row>
            <Row label="Permissions" mono>
              {entryPerm(e)} <span className="text-muted-foreground">({modeToOctal(e.mode)})</span>
            </Row>
            <Row label="Owner">
              {e.owner ?? '—'}
              {e.uid != null && <span className="text-muted-foreground"> (uid {e.uid})</span>}
            </Row>
            <Row label="Group">
              {e.group ?? '—'}
              {e.gid != null && <span className="text-muted-foreground"> (gid {e.gid})</span>}
            </Row>
          </dl>
        </DialogBody>
        <DialogFooter className="flex-wrap">
          <Button
            variant="secondary"
            onClick={async () => {
              if (await copyText(e.path)) toast.success('Path copied')
            }}
          >
            <Copy /> Copy path
          </Button>
          {(ctx.handle.capabilities.chmod || ctx.handle.capabilities.chown) && (
            <Button
              variant="secondary"
              onClick={() => {
                onClose()
                permissionsOf(ctx, [e])
              }}
            >
              <Shield /> Permissions…
            </Button>
          )}
          {!dir && ctx.handle.capabilities.checksum && (
            <Button
              variant="secondary"
              onClick={() => {
                onClose()
                checksumOf(ctx, e)
              }}
            >
              <Hash /> Checksum…
            </Button>
          )}
          <Button onClick={onClose}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// checksum
// ---------------------------------------------------------------------------------------------------------------------

const ALGOS: { value: ChecksumAlgo; label: string }[] = [
  { value: 'md5', label: 'MD5' },
  { value: 'sha1', label: 'SHA-1' },
  { value: 'sha256', label: 'SHA-256' },
  { value: 'sha512', label: 'SHA-512' },
]

async function hashLocalFile(file: File, algo: ChecksumAlgo, onProgress: (p: number) => void): Promise<string> {
  const hw = await import('hash-wasm')
  const hasher = algo === 'md5' ? await hw.createMD5() : algo === 'sha1' ? await hw.createSHA1() : algo === 'sha256' ? await hw.createSHA256() : await hw.createSHA512()
  hasher.init()
  const CHUNK = 4 * 1024 * 1024
  for (let off = 0; off < file.size; off += CHUNK) {
    const buf = new Uint8Array(await file.slice(off, off + CHUNK).arrayBuffer())
    hasher.update(buf)
    onProgress(Math.min(1, (off + buf.length) / Math.max(1, file.size)))
  }
  return hasher.digest('hex')
}

export function ChecksumDialog({ ctx, entry, onClose }: { ctx: FsContext; entry: FileEntry; onClose: () => void }) {
  const [algo, setAlgo] = useState<ChecksumAlgo>('sha256')
  const remote = useQuery({
    queryKey: ['fs', ctx.handle.id, 'checksum', entry.path, algo],
    placeholderData: undefined, // never show another algorithm's hash under this key
    queryFn: ({ signal }) => withFs(ctx.key, (id) => fsApi.checksum(id, entry.path, algo, signal)),
    retry: false,
    staleTime: 60_000,
  })
  const [expected, setExpected] = useState('')
  const [local, setLocal] = useState<{ name: string; hash?: string; progress: number; error?: string } | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const id = useId()
  const hash = remote.data?.toLowerCase() ?? ''
  const exp = expected.trim().toLowerCase()

  // The local hash depends on the algorithm: a hash computed for another one (switched meanwhile) is dropped.
  const algoRef = useRef(algo)
  algoRef.current = algo
  const pickLocal = async (file: File | undefined) => {
    if (!file) return
    const forAlgo = algo
    setLocal({ name: file.name, progress: 0 })
    try {
      const h = await hashLocalFile(file, forAlgo, (p) => {
        if (algoRef.current === forAlgo) setLocal((s) => (s ? { ...s, progress: p } : s))
      })
      if (algoRef.current === forAlgo) setLocal({ name: file.name, hash: h, progress: 1 })
    } catch (err) {
      if (algoRef.current === forAlgo) setLocal({ name: file.name, progress: 0, error: errorMessage(err) })
    }
  }
  useEffect(() => setLocal(null), [algo])

  const Match = ({ other }: { other: string }) =>
    !hash ? null : other === hash ? (
      <span className="flex items-center gap-1 text-sm text-success">
        <Check className="size-4" /> Match
      </span>
    ) : (
      <span className="flex items-center gap-1 text-sm text-destructive">
        <X className="size-4" /> Different
      </span>
    )

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <Hash className="size-4 text-muted-foreground" /> Checksum of “{entry.name}”
          </DialogTitle>
          <DialogDescription>
            {formatBytes(entry.size)} on {ctx.label}
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-4">
          <SegmentedControl value={algo} onValueChange={setAlgo} options={ALGOS} aria-label="Algorithm" size="sm" />
          <div className="grid gap-1">
            <span className="text-sm font-medium">Remote {ALGOS.find((a) => a.value === algo)?.label}</span>
            <div className="flex min-h-8 items-center gap-2 rounded-md border bg-muted/40 px-2.5 py-1">
              {remote.isFetching ? (
                <span className="flex items-center gap-2 text-sm text-muted-foreground">
                  <Spinner className="size-3.5" /> Computing on the server…
                </span>
              ) : remote.isError ? (
                <span className="text-sm text-destructive">{errorMessage(remote.error)}</span>
              ) : (
                <code className="min-w-0 flex-1 font-mono text-xs break-all select-all">{hash}</code>
              )}
              {hash && (
                <Button
                  variant="ghost"
                  size="icon-xs"
                  aria-label="Copy checksum"
                  onClick={async () => {
                    if (await copyText(hash)) toast.success('Checksum copied')
                  }}
                >
                  <Copy />
                </Button>
              )}
            </div>
          </div>
          <div className="grid gap-1">
            <label htmlFor={`${id}-exp`} className="text-sm font-medium">
              Compare with a checksum
            </label>
            <div className="flex items-center gap-2">
              <Input id={`${id}-exp`} value={expected} onChange={(e) => setExpected(e.target.value)} placeholder="Paste the expected value" className="font-mono text-xs" />
              {exp && <Match other={exp} />}
            </div>
          </div>
          <div className="grid gap-1">
            <span className="text-sm font-medium">Compare with a file on this computer</span>
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" variant="secondary" onClick={() => fileRef.current?.click()}>
                Choose file…
              </Button>
              <input ref={fileRef} type="file" hidden onChange={(e) => void pickLocal(e.target.files?.[0])} />
              {local && (
                <span className="min-w-0 flex-1 truncate text-sm text-muted-foreground">
                  {local.name}
                  {!local.hash && !local.error && ` — ${Math.round(local.progress * 100)}%`}
                  {local.error && <span className="text-destructive"> — {local.error}</span>}
                </span>
              )}
              {local?.hash && <Match other={local.hash} />}
            </div>
            {local?.hash && <code className="font-mono text-xs break-all text-muted-foreground">{local.hash}</code>}
          </div>
        </DialogBody>
        <DialogFooter>
          <Button onClick={onClose}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// compress
// ---------------------------------------------------------------------------------------------------------------------

export function CompressDialog({ ctx, entries, dir, onClose }: { ctx: FsContext; entries: FileEntry[]; dir: string; onClose: () => void }) {
  const [format, setFormat] = useState<'zip' | 'tar.gz'>('zip')
  const base = useMemo(() => (entries.length === 1 ? splitName(entries[0].name).stem || entries[0].name : basename(dir) || 'archive'), [entries, dir])
  const [name, setName] = useState(`${base}.zip`)
  const [busy, setBusy] = useState(false)
  const id = useId()
  const nameRef = useRef<HTMLInputElement>(null)
  const error = validateName(name)

  const changeFormat = (f: 'zip' | 'tar.gz') => {
    setFormat(f)
    setName((n) => n.replace(/\.(zip|tar\.gz|tgz)$/i, '') + (f === 'zip' ? '.zip' : '.tar.gz'))
  }
  const run = async () => {
    if (error) return
    setBusy(true)
    const dest = joinPath(dir, name.trim())
    try {
      await withFs(ctx.key, (fsId) =>
        fsApi.archive(
          fsId,
          entries.map((e) => e.path),
          dest,
          format,
        ),
      )
      toast.success(`Created ${name.trim()}`, { description: dir })
      invalidateDir(ctx, dir)
      onClose()
    } catch (err) {
      toast.error('Could not create the archive', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        size="sm"
        onOpenAutoFocus={(e) => {
          // Type a name right away (Enter compresses).
          e.preventDefault()
          const el = nameRef.current
          if (el) {
            el.focus()
            el.setSelectionRange(0, splitName(el.value).stem.length || el.value.length)
          }
        }}
      >
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault()
            void run()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <Archive className="size-4 text-muted-foreground" /> Compress {entries.length === 1 ? `“${entries[0].name}”` : `${entries.length} items`}
            </DialogTitle>
            <DialogDescription>The archive is created on the server, in {dir}.</DialogDescription>
          </DialogHeader>
          <SegmentedControl
            value={format}
            onValueChange={changeFormat}
            options={[
              { value: 'zip', label: 'ZIP' },
              { value: 'tar.gz', label: 'tar.gz' },
            ]}
            aria-label="Archive format"
            size="sm"
          />
          <div className="grid gap-1">
            <label htmlFor={`${id}-name`} className="text-sm font-medium">
              Archive name
            </label>
            <Input id={`${id}-name`} ref={nameRef} value={name} onChange={(e) => setName(e.target.value)} aria-invalid={!!error || undefined} spellCheck={false} />
            {error && <p className="text-sm text-destructive">{error}</p>}
          </div>
          <DialogFooter>
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={busy} disabled={!!error}>
              Compress
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

