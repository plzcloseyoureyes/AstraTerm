/*
 * Folder compare and sync (FILE-15, "MobaFoldersDiff"): POST /api/fs/compare between any two folders (any file
 * systems), by size + mtime or by checksum, recursive, with exclusions; filter by status and copy selected items to
 * the other side through the transfer queue.
 */
import { useMemo, useState } from 'react'
import { ArrowLeft, ArrowRight, FolderGit2, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Delayed, Spinner } from '@/components/ui/spinner'
import { cn, errorMessage, formatBytes, plural } from '@/lib/utils'
import { fsApi, type CompareItem, type CompareResult, type CompareStatus } from '../api'
import { liveFsId } from '../download'
import { formatMtime, isDirLike } from '../format'
import { dirname, joinPath, normalizePath } from '../paths'
import { useFsContext } from '../place'
import { SourcePicker } from '../tab/SourcePicker'
import { startServerTransfer } from '../transfers/store'
import type { FsContext, FsSource } from '../types'
import type { CompareSide } from './store'

const STATUS: Record<CompareStatus, { label: string; variant: 'default' | 'secondary' | 'success' | 'warning' | 'destructive' | 'info' | 'outline' }> = {
  same: { label: 'Same', variant: 'outline' },
  different: { label: 'Different', variant: 'warning' },
  'left-only': { label: 'Left only', variant: 'info' },
  'right-only': { label: 'Right only', variant: 'default' },
  'type-mismatch': { label: 'Type differs', variant: 'destructive' },
}

export default function CompareDialog({ left, right, onClose }: { left: CompareSide; right?: CompareSide; onClose: () => void }) {
  const [rightSource, setRightSource] = useState<FsSource>(right?.ctx.source ?? left.ctx.source)
  const [rightPath, setRightPath] = useState(right?.path ?? left.path)
  const [leftPath, setLeftPath] = useState(left.path)
  // The pane given by the caller (commander) is used as is; another source picked here is opened by the dialog.
  const givenRight = !!right && JSON.stringify(right.ctx.source) === JSON.stringify(rightSource)
  const rightFs = useFsContext(givenRight ? null : rightSource)
  const rightCtx: FsContext | null = givenRight && right ? right.ctx : rightFs.ctx
  const [recursive, setRecursive] = useState(true)
  const [mode, setMode] = useState<'size-mtime' | 'checksum'>('size-mtime')
  // (The server skips partial uploads, "*.termstead-part", itself.)
  const [excludes, setExcludes] = useState('.git, node_modules')
  const [result, setResult] = useState<CompareResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [show, setShow] = useState<Record<CompareStatus, boolean>>({ same: false, different: true, 'left-only': true, 'right-only': true, 'type-mismatch': true })
  const [selected, setSelected] = useState<Set<string>>(new Set())

  const run = async () => {
    if (!rightCtx) return
    setBusy(true)
    setError(null)
    setSelected(new Set())
    try {
      const [leftFs, rightFsId] = await Promise.all([liveFsId(left.ctx), liveFsId(rightCtx)])
      const r = await fsApi.compare({
        left: { fsId: leftFs, path: normalizePath(leftPath) },
        right: { fsId: rightFsId, path: normalizePath(rightPath) },
        recursive,
        mode,
        excludes: excludes
          .split(/[,\n]/)
          .map((s) => s.trim())
          .filter(Boolean),
      })
      setResult(r)
    } catch (err) {
      setError(errorMessage(err))
      setResult(null)
    } finally {
      setBusy(false)
    }
  }

  const items = useMemo(() => (result?.items ?? []).filter((i) => show[i.status]), [result, show])
  const toggle = (p: string, v: boolean) =>
    setSelected((s) => {
      const n = new Set(s)
      if (v) n.add(p)
      else n.delete(p)
      return n
    })

  const copy = async (dir: 'ltr' | 'rtl') => {
    if (!rightCtx || !result) return
    const src = dir === 'ltr' ? { ctx: left.ctx, base: normalizePath(leftPath) } : { ctx: rightCtx, base: normalizePath(rightPath) }
    const dst = dir === 'ltr' ? { ctx: rightCtx, base: normalizePath(rightPath) } : { ctx: left.ctx, base: normalizePath(leftPath) }
    const chosen = result.items.filter((i) => selected.has(i.path) && (dir === 'ltr' ? !!i.left : !!i.right))
    if (!chosen.length) {
      toast.info(`Select items that exist on the ${dir === 'ltr' ? 'left' : 'right'} side`)
      return
    }
    // Skip children of selected folders (the folder copy includes them); group by destination folder.
    const folders = chosen.filter((i) => i.type === 'dir').map((i) => i.path)
    const top = chosen.filter((i) => !folders.some((f) => i.path !== f && i.path.startsWith(`${f}/`)))
    const byDest = new Map<string, string[]>()
    for (const i of top) {
      const d = dirname(joinPath(dst.base, i.path))
      const list = byDest.get(d) ?? []
      list.push(joinPath(src.base, i.path))
      byDest.set(d, list)
    }
    try {
      const [srcFs, dstFs] = await Promise.all([liveFsId(src.ctx), liveFsId(dst.ctx)])
      for (const [dstDir, srcPaths] of byDest) {
        await startServerTransfer(
          { srcFs, srcPaths, dstFs, dstDir, overwrite: 'overwrite' },
          { label: srcPaths.length === 1 ? srcPaths[0].split('/').pop() : plural(srcPaths.length, 'item'), dstLabel: dst.ctx.label, announce: false },
        )
      }
      toast.info(`Copying ${plural(top.length, 'item')} ${dir === 'ltr' ? 'to the right' : 'to the left'}`, { description: 'Refresh the comparison when the transfers finish.' })
    } catch (err) {
      toast.error('Could not start the copy', { description: errorMessage(err) })
    }
  }

  const s = result?.summary
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="full">
        <DialogHeader>
          <DialogTitle>
            <FolderGit2 className="size-4 text-muted-foreground" /> Compare folders
          </DialogTitle>
          <DialogDescription>Differences by {mode === 'checksum' ? 'content (checksum)' : 'size and modification time'}.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 md:grid-cols-2">
          <div className="grid gap-1 rounded-md border p-2">
            <div className="flex h-6 items-center gap-1 text-xs font-medium text-muted-foreground">Left · {left.ctx.label}</div>
            <Input value={leftPath} onChange={(e) => setLeftPath(e.target.value)} className="font-mono" aria-label="Left folder" />
          </div>
          <div className="grid gap-1 rounded-md border p-2">
            <div className="flex h-6 items-center gap-1 text-xs font-medium text-muted-foreground">
              Right ·
              <SourcePicker source={rightSource} onPick={setRightSource} />
              <Spinner active={!rightCtx} className="size-3" />
            </div>
            <Input value={rightPath} onChange={(e) => setRightPath(e.target.value)} className="font-mono" aria-label="Right folder" />
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <label className="flex items-center gap-1.5 text-sm">
            <Checkbox checked={recursive} onCheckedChange={(v) => setRecursive(v === true)} /> Include sub-folders
          </label>
          <SegmentedControl
            size="sm"
            value={mode}
            onValueChange={setMode}
            options={[
              { value: 'size-mtime', label: 'Size & date' },
              { value: 'checksum', label: 'Checksum' },
            ]}
            aria-label="Comparison mode"
          />
          <Input value={excludes} onChange={(e) => setExcludes(e.target.value)} className="h-7 w-56 text-sm" inputSize="sm" placeholder="Exclude: .git, *.tmp" aria-label="Exclusions" />
          <span className="flex-1" />
          <Button size="sm" onClick={() => void run()} loading={busy} disabled={!rightCtx}>
            <RefreshCw /> {result ? 'Compare again' : 'Compare'}
          </Button>
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
        {s && (
          <div className="flex flex-wrap items-center gap-1.5" role="group" aria-label="Show">
            {(Object.keys(STATUS) as CompareStatus[]).map((k) => {
              const n = k === 'same' ? s.same : k === 'different' ? s.different : k === 'left-only' ? s.leftOnly : k === 'right-only' ? s.rightOnly : s.typeMismatch
              return (
                <button
                  key={k}
                  type="button"
                  aria-pressed={show[k]}
                  onClick={() => setShow((x) => ({ ...x, [k]: !x[k] }))}
                  className={cn('rounded-full border px-2 py-0.5 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring/50', show[k] ? 'border-primary/50 bg-primary/15 text-foreground' : 'text-muted-foreground')}
                >
                  {STATUS[k].label} · {n}
                </button>
              )
            })}
            {result?.truncated && <span className="text-xs text-warning">Truncated: narrow the comparison.</span>}
            <span className="flex-1" />
            <Button size="xs" variant="secondary" disabled={!selected.size} onClick={() => void copy('rtl')}>
              <ArrowLeft /> Copy to left
            </Button>
            <Button size="xs" variant="secondary" disabled={!selected.size} onClick={() => void copy('ltr')}>
              Copy to right <ArrowRight />
            </Button>
          </div>
        )}
        <div className="min-h-0 flex-1 overflow-auto rounded-md border">
          {busy && !result ? (
            <div className="flex h-full items-center justify-center gap-2 text-sm text-muted-foreground">
              <Delayed fallback={<span>Comparing…</span>}>
                <Spinner /> Comparing…
              </Delayed>
            </div>
          ) : !result ? (
            <div className="flex h-full items-center justify-center p-6 text-center text-sm text-muted-foreground">Choose both folders and press Compare.</div>
          ) : items.length === 0 ? (
            <div className="flex h-full items-center justify-center p-6 text-center text-sm text-muted-foreground">
              {result.items.length ? 'Nothing to show with the current filters.' : 'The folders are identical.'}
            </div>
          ) : (
            <table className="w-full text-sm">
              <thead className="sticky top-0 z-10 bg-popover text-xs text-muted-foreground">
                <tr className="border-b">
                  <th className="w-8 px-2 py-1">
                    <Checkbox
                      aria-label="Select all"
                      checked={items.length > 0 && items.every((i) => selected.has(i.path)) ? true : items.some((i) => selected.has(i.path)) ? 'indeterminate' : false}
                      onCheckedChange={(v) => setSelected(v === true ? new Set(items.map((i) => i.path)) : new Set())}
                    />
                  </th>
                  <th className="px-2 py-1 text-left font-medium">Status</th>
                  <th className="px-2 py-1 text-left font-medium">Path</th>
                  <th className="px-2 py-1 text-right font-medium">Left</th>
                  <th className="px-2 py-1 text-right font-medium">Right</th>
                </tr>
              </thead>
              <tbody>
                {items.slice(0, 5000).map((i) => (
                  <CompareRow key={i.path} item={i} checked={selected.has(i.path)} onCheck={(v) => toggle(i.path, v)} />
                ))}
              </tbody>
            </table>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}

function Side({ e }: { e?: FileEntry }) {
  if (!e) return <span className="text-muted-foreground/60">—</span>
  return (
    <span className="tabular">
      {isDirLike(e) ? 'folder' : formatBytes(e.size)}
      <span className="ml-2 text-xs text-muted-foreground">{formatMtime(e.mtime)}</span>
    </span>
  )
}

function CompareRow({ item, checked, onCheck }: { item: CompareItem; checked: boolean; onCheck: (v: boolean) => void }) {
  const st = STATUS[item.status]
  return (
    <tr className={cn('border-b last:border-0 hover:bg-accent/40', checked && 'bg-primary/8')}>
      <td className="px-2 py-1">
        <Checkbox aria-label={`Select ${item.path}`} checked={checked} onCheckedChange={(v) => onCheck(v === true)} />
      </td>
      <td className="px-2 py-1">
        <Badge variant={st.variant}>{st.label}</Badge>
      </td>
      <td className="max-w-0 px-2 py-1">
        <span className="block truncate font-mono text-xs" title={item.reason ? `${item.path} — ${item.reason}` : item.path}>
          {item.path}
          {item.type === 'dir' && '/'}
        </span>
      </td>
      <td className="px-2 py-1 text-right whitespace-nowrap">
        <Side e={item.left} />
      </td>
      <td className="px-2 py-1 text-right whitespace-nowrap">
        <Side e={item.right} />
      </td>
    </tr>
  )
}
