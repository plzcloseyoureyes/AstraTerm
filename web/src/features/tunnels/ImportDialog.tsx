/*
 * Import tunnels from a AstraTerm JSON export: pick a file, preview how each tunnel maps to one of your SSH connections
 * (same id, name + address, address, name, or a default connection), then import. Passwords are never part of an
 * export, so imported SOCKS proxies come without authentication.
 */
import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { CircleAlert, FileJson, ShieldAlert, Upload } from 'lucide-react'
import { queryClient } from '@/api/queryClient'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Spinner } from '@/components/ui/spinner'
import { cn, errorMessage } from '@/lib/utils'
import { importTunnels, tunnelKeys } from './api'
import { ConnectionPicker } from './components'
import type { ImportResult } from './types'

const MATCH_LABEL: Record<string, string> = {
  id: 'same connection',
  'name+address': 'same name & address',
  address: 'same address',
  name: 'same name',
  default: 'default connection',
}

export default function ImportDialog({ onClose }: { onClose: () => void }) {
  const [file, setFile] = useState<{ name: string; data: unknown } | null>(null)
  const [parseError, setParseError] = useState<string | null>(null)
  const [defaultConnectionId, setDefaultConnectionId] = useState('')
  const [preview, setPreview] = useState<ImportResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [importing, setImporting] = useState(false)
  const [dragging, setDragging] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  const readFile = async (f: File) => {
    setParseError(null)
    setPreview(null)
    if (f.size > 8 << 20) {
      setParseError('The file is too large (8 MiB max).')
      return
    }
    try {
      const data = JSON.parse(await f.text())
      if (!data || data.format !== 'astraterm-tunnels' || !Array.isArray(data.tunnels)) {
        setParseError('This is not a AstraTerm tunnels export (format "astraterm-tunnels").')
        return
      }
      setFile({ name: f.name, data })
    } catch {
      setParseError('The file is not valid JSON.')
    }
  }

  // Dry run whenever the file or the default connection changes.
  useEffect(() => {
    if (!file) return
    let cancelled = false
    setLoading(true)
    importTunnels({ file: file.data, defaultConnectionId: defaultConnectionId || undefined, dryRun: true })
      .then((r) => !cancelled && setPreview(r))
      .catch((err) => !cancelled && setParseError(errorMessage(err)))
      .finally(() => !cancelled && setLoading(false))
    return () => {
      cancelled = true
    }
  }, [file, defaultConnectionId])

  const doImport = async () => {
    if (!file) return
    setImporting(true)
    try {
      const r = await importTunnels({ file: file.data, defaultConnectionId: defaultConnectionId || undefined })
      await queryClient.invalidateQueries({ queryKey: tunnelKeys.list, exact: true })
      toast.success(`Imported ${r.created.length} tunnel${r.created.length === 1 ? '' : 's'}`, {
        description: r.skipped.length ? `${r.skipped.length} skipped` : undefined,
      })
      onClose()
    } catch (err) {
      setParseError(errorMessage(err))
    } finally {
      setImporting(false)
    }
  }

  const count = (file?.data as { tunnels?: unknown[] } | undefined)?.tunnels?.length ?? 0

  return (
    <Dialog open onOpenChange={(o) => !o && !importing && onClose()}>
      <DialogContent size="lg" className="max-h-[calc(100dvh-2rem)]">
        <DialogHeader>
          <DialogTitle>
            <Upload className="size-4 text-primary" /> Import tunnels
          </DialogTitle>
          <DialogDescription>From a AstraTerm export (Tunnels → Export). Imported tunnels are stopped and do not autostart.</DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-3">
          <button
            type="button"
            onClick={() => inputRef.current?.click()}
            onDragOver={(e) => {
              e.preventDefault()
              setDragging(true)
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={(e) => {
              e.preventDefault()
              setDragging(false)
              const f = e.dataTransfer.files?.[0]
              if (f) void readFile(f)
            }}
            className={cn(
              'flex flex-col items-center justify-center gap-1.5 rounded-lg border border-dashed px-4 py-6 text-center outline-none hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring/40',
              dragging && 'border-primary bg-primary/8',
            )}
          >
            <FileJson className="size-6 text-muted-foreground" />
            {file ? (
              <span className="text-base">
                <span className="font-medium">{file.name}</span> — {count} tunnel{count === 1 ? '' : 's'}
              </span>
            ) : (
              <span className="text-base">Drop a JSON export here or click to choose a file</span>
            )}
          </button>
          <input
            ref={inputRef}
            type="file"
            accept="application/json,.json"
            className="hidden"
            onChange={(e) => {
              const f = e.target.files?.[0]
              if (f) void readFile(f)
              e.target.value = ''
            }}
          />
          {parseError && (
            <p role="alert" className="flex items-start gap-1.5 text-sm text-destructive">
              <CircleAlert className="mt-0.5 size-3.5 shrink-0" /> {parseError}
            </p>
          )}
          {file && (
            <Field label="Default SSH connection" hint="Used for tunnels whose connection cannot be matched to one of yours.">
              <ConnectionPicker value={defaultConnectionId} onChange={(id) => setDefaultConnectionId(id)} />
            </Field>
          )}
          {loading && (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Spinner className="size-3.5" /> Matching connections…
            </p>
          )}
          {preview && !loading && preview.planned.some((p) => p.warning) && (
            <p className="flex items-start gap-1.5 text-sm text-warning">
              <ShieldAlert className="mt-0.5 size-3.5 shrink-0" /> Some tunnels listen on non-loopback addresses: once started, other machines can
              connect to them. Check them before starting.
            </p>
          )}
          {preview && !loading && (
            <div className="overflow-hidden rounded-md border">
              <table className="w-full border-collapse text-base">
                <thead className="bg-muted/40">
                  <tr className="text-left text-xs text-muted-foreground">
                    <th className="h-7 px-2 font-medium">Tunnel</th>
                    <th className="px-2 font-medium">Connection</th>
                  </tr>
                </thead>
                <tbody>
                  {preview.planned.map((p, i) => (
                    <tr key={`p${i}`} className="border-t">
                      <td className="max-w-0 px-2 py-1">
                        <span className="block truncate">{p.name}</span>
                        {p.warning && (
                          <span className="flex min-w-0 items-center gap-1 text-xs text-warning" title={p.warning}>
                            <ShieldAlert className="size-3 shrink-0" />
                            <span className="truncate">{p.warning}</span>
                          </span>
                        )}
                      </td>
                      <td className="max-w-0 px-2">
                        <span className="truncate">{p.connectionName}</span>{' '}
                        <Badge variant={p.matched === 'default' ? 'warning' : 'success'}>{MATCH_LABEL[p.matched] ?? p.matched}</Badge>
                      </td>
                    </tr>
                  ))}
                  {preview.skipped.map((s, i) => (
                    <tr key={`s${i}`} className="border-t text-muted-foreground">
                      <td className="max-w-0 truncate px-2 py-1 line-through">{s.name}</td>
                      <td className="max-w-0 truncate px-2 text-sm text-destructive" title={s.reason}>
                        {s.reason}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose} disabled={importing}>
            Cancel
          </Button>
          <Button onClick={() => void doImport()} loading={importing} disabled={!preview || !preview.planned.length || loading}>
            Import {preview?.planned.length ? preview.planned.length : ''}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
