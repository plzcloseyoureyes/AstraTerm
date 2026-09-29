/*
 * Import wizard (IMP-1..4): step 1 choose a source (drag-drop file(s), paste text, or a local config file found by the
 * desktop-mode scan), step 2 preview the tree (per-folder tri-state and per-connection checkboxes, filter, duplicate /
 * gateway / local-command markers and notes, host keys and SSH keys) and pick a target folder and duplicate strategy,
 * step 3 result summary with "Show in session tree". The format is auto-detected but can be forced; legacy 8-bit files
 * (MobaXterm.ini on a Hebrew, Cyrillic, CJK… Windows) can be re-read with another text encoding.
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ChevronDown,
  ChevronRight,
  FileSearch,
  FileUp,
  Folder,
  FolderInput,
  FolderTree,
  KeyRound,
  RefreshCw,
  Route,
  ScanLine,
  Search,
  ShieldAlert,
  ShieldCheck,
  TriangleAlert,
  Upload,
} from 'lucide-react'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { runCommand } from '@/app/commands'
import { useFolders } from '@/api/folders'
import { isApiError } from '@/api/client'
import type { Folder as FolderT } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox, CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { Textarea } from '@/components/ui/textarea'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { commit, discover, filesToBase64, preview, toBase64, type SourceRef } from './api'
import {
  CHARSET_OPTIONS,
  type CommitResponse,
  type Dedupe,
  type DiscoverEntry,
  type ImportFormat,
  type PreviewConnection,
  type PreviewOptions,
  type PreviewResponse,
} from './types'

const FORMAT_OPTIONS: { value: ImportFormat; label: string }[] = [
  { value: 'auto', label: 'Auto-detect' },
  { value: 'mobaxterm', label: 'MobaXterm (.mxtsessions / .moba / MobaXterm.ini)' },
  { value: 'putty_reg', label: 'PuTTY / KiTTY (.reg)' },
  { value: 'ssh_config', label: 'OpenSSH config (~/.ssh/config)' },
  { value: 'termius_csv', label: 'Termius (CSV)' },
  { value: 'mremoteng', label: 'mRemoteNG (confCons.xml)' },
  { value: 'remmina', label: 'Remmina (.remmina)' },
  { value: 'filezilla', label: 'FileZilla (sitemanager.xml)' },
  { value: 'winscp', label: 'WinSCP (WinSCP.ini)' },
  { value: 'securecrt', label: 'SecureCRT (XML export)' },
  { value: 'csv', label: 'Generic CSV' },
  { value: 'json', label: 'NexTerm JSON export' },
  { value: 'known_hosts', label: 'known_hosts / PuTTY host keys' },
]

/** Radix Select cannot hold an empty value: the "no folder" choice uses this sentinel. */
const ROOT = '__root__'

const FORMAT_LABEL: Record<string, string> = Object.fromEntries(FORMAT_OPTIONS.map((o) => [o.value, o.label]))

type Step = 'source' | 'preview' | 'result'
type Source = { format: ImportFormat; content?: string; path?: string; label: string }

export default function ImportWizard({ initialFormat, onClose }: { initialFormat?: ImportFormat; onClose: () => void }) {
  const desktop = useAuthStore((s) => s.state?.mode === 'desktop')
  const user = useAuthStore((s) => s.user)
  const { data: allFolders } = useFolders()
  const [step, setStep] = useState<Step>('source')
  const [format, setFormat] = useState<ImportFormat>(initialFormat ?? 'auto')
  const [charset, setCharset] = useState('auto')
  const [source, setSource] = useState<Source | null>(null)
  const [passphrase, setPassphrase] = useState('')
  const [needPass, setNeedPass] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const showBusy = useDelayedFlag(busy)

  const [prev, setPrev] = useState<PreviewResponse | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [targetFolder, setTargetFolder] = useState(ROOT)
  const [dedupe, setDedupe] = useState<Dedupe>('skip')
  const [importKeys, setImportKeys] = useState(true)
  const [importKnown, setImportKnown] = useState(true)
  const [result, setResult] = useState<CommitResponse | null>(null)

  // Folders the caller may import into (own folders; admins may use any).
  const folders = useMemo(
    () => (allFolders ?? []).filter((f) => user?.role === 'admin' || f.ownerId === user?.id),
    [allFolders, user],
  )

  const optionsFor = useCallback(
    (pass: string | undefined, cs: string): PreviewOptions | undefined => {
      const o: PreviewOptions = {}
      if (pass) o.passphrase = pass
      if (cs && cs !== 'auto') o.charset = cs
      return Object.keys(o).length ? o : undefined
    },
    [],
  )

  const runPreview = useCallback(
    async (src: Source, pass: string | undefined, cs: string) => {
      setBusy(true)
      setError(null)
      setSource(src)
      try {
        const res = await preview({ format: src.format, content: src.content, path: src.path, options: optionsFor(pass, cs) })
        setPrev(res)
        setSelected(new Set(res.connections.map((c) => c.id)))
        setNeedPass(false)
        setStep('preview')
      } catch (err) {
        if (isApiError(err) && (err.code === 'passphrase_required' || err.code === 'wrong_password')) {
          setNeedPass(true)
          setStep('source')
          setError(err.code === 'wrong_password' ? 'Wrong passphrase — try again.' : 'This export is encrypted. Enter its passphrase to continue.')
        } else {
          setError(errorMessage(err))
        }
      } finally {
        setBusy(false)
      }
    },
    [optionsFor],
  )

  const onFiles = useCallback(
    async (files: FileList | File[]) => {
      const list = Array.from(files)
      if (!list.length) return
      try {
        const content = await filesToBase64(list)
        await runPreview({ format, content, label: list.length === 1 ? list[0].name : `${list.length} files` }, passphrase || undefined, charset)
      } catch (err) {
        setError(errorMessage(err))
      }
    },
    [format, passphrase, charset, runPreview],
  )

  const onPaste = useCallback(
    async (text: string) => {
      if (!text.trim()) {
        setError('Paste some content or drop a file first.')
        return
      }
      await runPreview({ format, content: toBase64(text), label: 'pasted text' }, passphrase || undefined, charset)
    },
    [format, passphrase, charset, runPreview],
  )

  const onDiscovered = useCallback(
    async (entry: DiscoverEntry) => {
      setFormat(entry.format)
      await runPreview({ format: entry.format, path: entry.path, label: entry.label }, undefined, charset)
    },
    [charset, runPreview],
  )

  const onCharset = useCallback(
    (cs: string) => {
      setCharset(cs)
      if (source && step === 'preview') void runPreview(source, passphrase || undefined, cs)
    },
    [source, step, passphrase, runPreview],
  )

  const knownNew = prev?.knownHosts?.filter((k) => !k.duplicate && !k.conflict).length ?? 0
  const keysNew = prev?.keys?.filter((k) => !k.duplicate).length ?? 0
  const extras = (prev?.counts.identities ?? 0) + (prev?.counts.snippets ?? 0)
  // With "skip", selected sessions that already exist are not imported: count (and label) only the new ones.
  const dupIds = useMemo(() => new Set(prev?.connections.filter((c) => c.duplicate).map((c) => c.id)), [prev])
  const sessionsToImport = dedupe === 'skip' ? [...selected].filter((id) => !dupIds.has(id)).length : selected.size
  const importCount = sessionsToImport + (importKnown ? knownNew : 0) + (importKeys ? keysNew : 0) + extras
  const canImport = !!prev && importCount > 0

  const doImport = useCallback(async () => {
    if (!source || !prev) return
    setBusy(true)
    setError(null)
    try {
      const src: SourceRef = { format: source.format, content: source.content, path: source.path, options: optionsFor(passphrase || undefined, charset) }
      const res = await commit({
        ...src,
        targetFolderId: targetFolder === ROOT ? undefined : targetFolder,
        selectedIds: selected.size === prev.connections.length ? undefined : Array.from(selected),
        dedupe,
        importKeys,
        importKnownHosts: importKnown,
      })
      // The result step is the feedback (no toast over its buttons).
      setResult(res)
      setStep('result')
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }, [source, prev, optionsFor, passphrase, charset, targetFolder, selected, dedupe, importKeys, importKnown])

  const reveal = () => {
    const id = result?.connectionIds?.[0]
    if (id) void runCommand('sessions.reveal', { id }, { source: 'api' })
    onClose()
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent size="2xl" className="h-[min(90dvh,52rem)]">
        <DialogHeader>
          <DialogTitle>
            <FolderInput className="size-4.5 text-primary" /> Import sessions
          </DialogTitle>
          <DialogDescription>
            {step === 'source' && 'Choose a file or paste an export from another client. Passwords are never read from other clients’ files.'}
            {step === 'preview' && 'Review what will be imported, then choose where it lands.'}
            {step === 'result' && 'Import complete.'}
          </DialogDescription>
        </DialogHeader>

        <Steps step={step} />

        {step === 'source' && (
          <SourceStep
            desktop={desktop}
            format={format}
            setFormat={setFormat}
            charset={charset}
            setCharset={setCharset}
            busy={busy}
            showBusy={showBusy}
            needPass={needPass}
            passphrase={passphrase}
            setPassphrase={setPassphrase}
            onFiles={onFiles}
            onPaste={onPaste}
            onDiscovered={onDiscovered}
            onRetryPass={() => source && runPreview(source, passphrase || undefined, charset)}
            error={error}
          />
        )}

        {step === 'preview' && prev && (
          <PreviewStep
            prev={prev}
            sourceLabel={source?.label ?? ''}
            selected={selected}
            setSelected={setSelected}
            folders={folders}
            targetFolder={targetFolder}
            setTargetFolder={setTargetFolder}
            dedupe={dedupe}
            setDedupe={setDedupe}
            charset={charset}
            onCharset={onCharset}
            importKeys={importKeys}
            setImportKeys={setImportKeys}
            importKnown={importKnown}
            setImportKnown={setImportKnown}
            busy={busy}
            error={error}
          />
        )}

        {step === 'result' && result && <ResultStep result={result} />}

        <DialogFooter>
          {step === 'preview' && (
            <Button variant="ghost" onClick={() => setStep('source')} disabled={busy}>
              Back
            </Button>
          )}
          <div className="flex-1" />
          {step === 'result' ? (
            <>
              {result && result.connectionIds.length > 0 && (
                <Button variant="secondary" onClick={reveal}>
                  <FolderTree className="size-4" /> Show in session tree
                </Button>
              )}
              <Button onClick={onClose}>Done</Button>
            </>
          ) : (
            <>
              <Button variant="ghost" onClick={onClose} disabled={busy}>
                Cancel
              </Button>
              {step === 'preview' && (
                <Button onClick={doImport} disabled={busy || !canImport}>
                  {showBusy ? <Spinner immediate className="size-4" /> : <FolderInput className="size-4" />}
                  {sessionsToImport > 0
                    ? `Import ${sessionsToImport} session${sessionsToImport === 1 ? '' : 's'}`
                    : canImport
                      ? 'Import'
                      : selected.size > 0
                        ? 'Already imported'
                        : 'Import'}
                </Button>
              )}
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function Steps({ step }: { step: Step }) {
  const items: { id: Step; label: string }[] = [
    { id: 'source', label: 'Source' },
    { id: 'preview', label: 'Preview' },
    { id: 'result', label: 'Result' },
  ]
  const idx = items.findIndex((s) => s.id === step)
  return (
    <ol className="flex items-center gap-1.5 text-sm text-muted-foreground" aria-label="Import steps">
      {items.map((s, i) => (
        <li key={s.id} className="flex items-center gap-1.5" aria-current={i === idx ? 'step' : undefined}>
          {i > 0 && <ChevronRight className="size-3.5 opacity-60" aria-hidden />}
          <span className={i <= idx ? 'font-medium text-foreground' : ''}>{s.label}</span>
        </li>
      ))}
    </ol>
  )
}

// ---- step 1: source ------------------------------------------------------------------------------------------------

function SourceStep(props: {
  desktop: boolean
  format: ImportFormat
  setFormat: (f: ImportFormat) => void
  charset: string
  setCharset: (c: string) => void
  busy: boolean
  showBusy: boolean
  needPass: boolean
  passphrase: string
  setPassphrase: (v: string) => void
  onFiles: (files: FileList | File[]) => void
  onPaste: (text: string) => void
  onDiscovered: (e: DiscoverEntry) => void
  onRetryPass: () => void
  error: string | null
}) {
  const { desktop, format, setFormat, charset, setCharset, busy, showBusy, needPass, passphrase, setPassphrase, onFiles, onPaste, onDiscovered, onRetryPass, error } = props
  const [text, setText] = useState('')
  const [dragging, setDragging] = useState(false)
  const dragDepth = useRef(0)
  const fileRef = useRef<HTMLInputElement>(null)

  return (
    <DialogBody className="grid content-start gap-4 animate-in fade-in-0 duration-150">
      <MobaXtermHint />
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Format">
          <SimpleSelect value={format} onValueChange={(v) => setFormat(v as ImportFormat)} options={FORMAT_OPTIONS} aria-label="Import format" />
        </Field>
        <Field label="Text encoding" hint="For MobaXterm / PuTTY / WinSCP files from a non-Western Windows.">
          <SimpleSelect value={charset} onValueChange={setCharset} options={CHARSET_OPTIONS} aria-label="Text encoding" />
        </Field>
      </div>

      <div
        onDragEnter={(e) => {
          e.preventDefault()
          dragDepth.current++
          setDragging(true)
        }}
        onDragOver={(e) => e.preventDefault()}
        onDragLeave={() => {
          dragDepth.current = Math.max(0, dragDepth.current - 1)
          if (dragDepth.current === 0) setDragging(false)
        }}
        onDrop={(e) => {
          e.preventDefault()
          dragDepth.current = 0
          setDragging(false)
          if (e.dataTransfer.files.length && !busy) onFiles(e.dataTransfer.files)
        }}
        className={cn(
          'flex flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed p-5 text-center transition-colors',
          dragging ? 'border-primary bg-primary/5' : 'border-border',
        )}
      >
        <FileUp className="size-6 text-muted-foreground" aria-hidden />
        <p className="text-base">Drop one or more files here, or</p>
        <Button variant="secondary" size="sm" onClick={() => fileRef.current?.click()} disabled={busy}>
          {showBusy ? <Spinner immediate className="size-4" /> : <Upload className="size-4" />} Choose files…
        </Button>
        <input
          ref={fileRef}
          type="file"
          multiple
          className="hidden"
          accept=".mxtsessions,.moba,.ini,.reg,.xml,.csv,.json,.remmina,.conf,.txt,*"
          onChange={(e) => {
            if (e.target.files?.length) onFiles(e.target.files)
            e.target.value = ''
          }}
        />
        <p className="text-xs text-muted-foreground">MobaXterm .mxtsessions / MobaXterm.ini, PuTTY .reg, ssh_config, CSV, XML, NexTerm JSON…</p>
      </div>

      {desktop && <DiscoverPanel onPick={onDiscovered} busy={busy} />}

      <Field label="…or paste the export text">
        <Textarea
          mono
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="Paste MobaXterm.ini, ssh_config, a .reg export, CSV…"
          rows={4}
        />
      </Field>

      {needPass && (
        <Field label="Passphrase" hint="This NexTerm export is encrypted.">
          <PasswordInput
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
            placeholder="Export passphrase"
            autoFocus
            onKeyDown={(e) => {
              if (e.key === 'Enter' && passphrase && !busy) onRetryPass()
            }}
          />
        </Field>
      )}

      {error && <ErrorLine message={error} />}

      <div className="flex justify-end">
        {needPass ? (
          <Button onClick={onRetryPass} disabled={busy || !passphrase}>
            {showBusy ? <Spinner immediate className="size-4" /> : <KeyRound className="size-4" />} Decrypt & preview
          </Button>
        ) : (
          <Button onClick={() => onPaste(text)} disabled={busy || !text.trim()}>
            {showBusy ? <Spinner immediate className="size-4" /> : <FileSearch className="size-4" />} Preview
          </Button>
        )}
      </div>
    </DialogBody>
  )
}

/** One-line MobaXterm migration guide (the most common source): where the file is and how to export it. */
function MobaXtermHint() {
  const [open, setOpen] = useState(false)
  return (
    <div className="rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 text-sm">
      <button
        type="button"
        className="flex w-full items-center gap-1.5 text-left font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring/50 rounded-sm"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {open ? <ChevronDown className="size-3.5" aria-hidden /> : <ChevronRight className="size-3.5" aria-hidden />}
        Coming from MobaXterm?
      </button>
      {open && (
        <ol className="mt-1.5 grid list-decimal gap-0.5 pl-5 text-muted-foreground animate-in fade-in-0 duration-150">
          <li>In MobaXterm, right-click <span className="text-foreground">User sessions</span> → <span className="text-foreground">Export all sessions to file</span>, and drop the .mxtsessions file here.</li>
          <li>Or drop <span className="font-mono text-foreground">MobaXterm.ini</span> (Documents\MobaXterm, or next to the portable MobaXterm.exe) — its trusted host keys come along.</li>
          <li>Folders, SSH gateways, proxies, keys, icons, tab colours and terminal settings are kept; passwords stay in MobaXterm (re-enter them once, or use keys).</li>
        </ol>
      )}
    </div>
  )
}

function DiscoverPanel({ onPick, busy }: { onPick: (e: DiscoverEntry) => void; busy: boolean }) {
  const [files, setFiles] = useState<DiscoverEntry[] | null>(null)
  const [scanning, setScanning] = useState(false)
  const [scanError, setScanError] = useState<string | null>(null)
  const showScanning = useDelayedFlag(scanning)
  const scan = useCallback(async () => {
    setScanning(true)
    setScanError(null)
    try {
      const res = await discover()
      setFiles(res.supported ? res.files : [])
    } catch (err) {
      setScanError(errorMessage(err))
    } finally {
      setScanning(false)
    }
  }, [])
  useEffect(() => {
    void scan()
  }, [scan])

  return (
    <section className="rounded-lg border bg-card/60 p-3" aria-label="Import from this computer">
      <div className="flex items-center justify-between gap-2">
        <p className="flex items-center gap-1.5 text-sm font-medium">
          <ScanLine className="size-4 text-muted-foreground" aria-hidden /> On this computer
        </p>
        <Button variant="ghost" size="xs" onClick={scan} disabled={scanning || busy} aria-label="Scan again">
          {showScanning ? <Spinner immediate className="size-3.5" /> : <RefreshCw className="size-3.5" />} Rescan
        </Button>
      </div>
      {scanError && <p className="mt-2 text-sm text-destructive">{scanError}</p>}
      {files === null && !scanError && <div className="h-7" aria-hidden />}
      {files && files.length === 0 && !scanError && (
        <p className="mt-2 text-sm text-muted-foreground">
          No ~/.ssh/config, MobaXterm, PuTTY, FileZilla, WinSCP, Remmina or mRemoteNG files were found.
        </p>
      )}
      {files && files.length > 0 && (
        <ul className="mt-2 grid max-h-40 gap-0.5 overflow-y-auto">
          {files.map((f) => (
            <li key={f.path}>
              <button
                type="button"
                onClick={() => onPick(f)}
                disabled={busy}
                title={f.path}
                className="flex w-full items-center justify-between gap-2 rounded-md px-2 py-1.5 text-left text-base outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50 disabled:opacity-50"
              >
                <span className="min-w-0 truncate">{f.label}</span>
                <span className="flex shrink-0 items-center gap-1.5">
                  <Badge variant={f.format === 'mobaxterm' ? 'default' : 'outline'}>{shortFormat(f.format)}</Badge>
                  <ChevronRight className="size-4 opacity-60" aria-hidden />
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function shortFormat(f: string): string {
  const label = FORMAT_LABEL[f] ?? f
  return label.replace(/\s*\(.*\)$/, '')
}

// ---- step 2: preview -----------------------------------------------------------------------------------------------

interface TreeItem {
  key: string
  kind: 'folder' | 'conn'
  name: string
  conn?: PreviewConnection
  children: TreeItem[]
  /** Connection ids in this subtree (folders). */
  ids: string[]
}

function buildTree(prev: PreviewResponse, filter: string): TreeItem[] {
  const q = filter.trim().toLowerCase()
  const match = (c: PreviewConnection) =>
    !q || c.name.toLowerCase().includes(q) || c.host.toLowerCase().includes(q) || (c.username ?? '').toLowerCase().includes(q)
  const folderNode = new Map<string, TreeItem>()
  for (const f of prev.folders) folderNode.set(f.id, { key: 'f:' + f.id, kind: 'folder', name: f.name, children: [], ids: [] })
  const roots: TreeItem[] = []
  for (const f of prev.folders) {
    const node = folderNode.get(f.id)!
    const parent = f.parentId ? folderNode.get(f.parentId) : undefined
    if (parent) parent.children.push(node)
    else roots.push(node)
  }
  for (const c of prev.connections) {
    if (!match(c)) continue
    const item: TreeItem = { key: 'c:' + c.id, kind: 'conn', name: c.name, conn: c, children: [], ids: [c.id] }
    const parent = c.folderId ? folderNode.get(c.folderId) : undefined
    if (parent) parent.children.push(item)
    else roots.push(item)
  }
  // Collect subtree ids and drop empty folders (none of their connections match / exist).
  const finish = (nodes: TreeItem[]): TreeItem[] =>
    nodes.filter((n) => {
      if (n.kind === 'conn') return true
      n.children = finish(n.children)
      n.ids = n.children.flatMap((c) => c.ids)
      return n.ids.length > 0
    })
  return finish(roots)
}

function PreviewStep(props: {
  prev: PreviewResponse
  sourceLabel: string
  selected: Set<string>
  setSelected: (s: Set<string>) => void
  folders: FolderT[]
  targetFolder: string
  setTargetFolder: (v: string) => void
  dedupe: Dedupe
  setDedupe: (v: Dedupe) => void
  charset: string
  onCharset: (c: string) => void
  importKeys: boolean
  setImportKeys: (v: boolean) => void
  importKnown: boolean
  setImportKnown: (v: boolean) => void
  busy: boolean
  error: string | null
}) {
  const { prev, sourceLabel, selected, setSelected, folders, targetFolder, setTargetFolder, dedupe, setDedupe, charset, onCharset, busy, error } = props
  const [filter, setFilter] = useState('')
  const tree = useMemo(() => buildTree(prev, filter), [prev, filter])
  const visibleIds = useMemo(() => tree.flatMap((n) => n.ids), [tree])

  const setMany = useCallback(
    (ids: string[], on: boolean) => {
      const next = new Set(selected)
      for (const id of ids) {
        if (on) next.add(id)
        else next.delete(id)
      }
      setSelected(next)
    },
    [selected, setSelected],
  )
  const allVisibleSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.has(id))

  const folderOptions = useMemo(() => {
    const opts = [{ value: ROOT, label: 'Top level (no folder)' }]
    for (const f of folderPaths(folders)) opts.push({ value: f.id, label: f.path })
    return opts
  }, [folders])

  const keyConns = prev.connections.filter((c) => c.keyName).length
  const localCmd = prev.connections.filter((c) => c.runsLocalCommand).length
  const dupSelected = prev.connections.filter((c) => c.duplicate && selected.has(c.id)).length

  return (
    <DialogBody className="flex min-h-0 flex-col gap-3 animate-in fade-in-0 duration-150">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">
        <span>
          <span className="font-medium text-foreground">{FORMAT_LABEL[prev.format] ?? prev.format}</span>
          {sourceLabel && <> · {sourceLabel}</>}
        </span>
        <span>
          {prev.counts.connections} session{prev.counts.connections === 1 ? '' : 's'}
          {prev.counts.folders > 0 && ` · ${prev.counts.folders} folder${prev.counts.folders === 1 ? '' : 's'}`}
          {prev.counts.duplicates > 0 && ` · ${prev.counts.duplicates} already exist`}
          {prev.counts.unsupported > 0 && ` · ${prev.counts.unsupported} skipped`}
        </span>
        {prev.format !== 'json' && (
          <span className="ml-auto flex items-center gap-1.5">
            <label htmlFor="importer-charset" className="text-xs">
              Encoding
            </label>
            <SimpleSelect id="importer-charset" size="sm" value={charset} onValueChange={onCharset} options={CHARSET_OPTIONS} aria-label="Text encoding" disabled={busy} />
          </span>
        )}
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Import into folder">
          <SimpleSelect value={targetFolder} onValueChange={setTargetFolder} options={folderOptions} aria-label="Target folder" />
        </Field>
        <Field label="If a session already exists" hint={dupSelected > 0 ? `${dupSelected} selected session${dupSelected === 1 ? '' : 's'} already exist.` : undefined}>
          <SimpleSelect
            value={dedupe}
            onValueChange={(v) => setDedupe(v as Dedupe)}
            options={[
              { value: 'skip', label: 'Skip it' },
              { value: 'update', label: 'Update it (address, login, options)' },
              { value: 'duplicate', label: 'Import a copy' },
            ]}
            aria-label="Duplicate handling"
          />
        </Field>
      </div>

      {prev.connections.length > 0 && (
        <div className="flex items-center gap-2">
          <Checkbox
            checked={allVisibleSelected ? true : visibleIds.some((id) => selected.has(id)) ? 'indeterminate' : false}
            onCheckedChange={() => setMany(visibleIds, !allVisibleSelected)}
            aria-label={filter ? 'Select all matching sessions' : 'Select all sessions'}
            className="ml-1.5"
          />
          <Input
            inputSize="sm"
            leading={<Search />}
            placeholder="Filter by name, host or user"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="Filter sessions"
            className="flex-1"
          />
          <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
            {selected.size}/{prev.connections.length} selected
          </span>
        </div>
      )}

      <div className="min-h-32 flex-1 overflow-y-auto rounded-lg border" role="tree" aria-label="Sessions to import" aria-multiselectable>
        {prev.connections.length === 0 ? (
          <EmptyState
            size="sm"
            title="No sessions in this source"
            description={
              (prev.knownHosts?.length ?? 0) + (prev.keys?.length ?? 0) + prev.counts.identities + prev.counts.snippets > 0
                ? 'It only carries the items listed below.'
                : 'Nothing importable was found.'
            }
          />
        ) : tree.length === 0 ? (
          <EmptyState size="sm" title="No match" description="No session matches the filter." />
        ) : (
          <ul className="p-1">
            {tree.map((node) => (
              <TreeNode key={node.key} node={node} selected={selected} setMany={setMany} depth={0} />
            ))}
          </ul>
        )}
      </div>

      {localCmd > 0 && (
        <p className="flex items-start gap-1.5 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-sm">
          <ShieldAlert className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden />
          <span>
            {localCmd === 1 ? '1 session runs' : `${localCmd} sessions run`} a program on this computer when opened (proxy command or local
            shell). Only import {localCmd === 1 ? 'it' : 'them'} if you trust this file.
          </span>
        </p>
      )}

      <ExtrasSection {...props} keyConns={keyConns} />

      {prev.warnings.length > 0 && <Notes title="Notes" items={prev.warnings} />}
      {error && <ErrorLine message={error} />}
    </DialogBody>
  )
}

function ExtrasSection(props: {
  prev: PreviewResponse
  importKeys: boolean
  setImportKeys: (v: boolean) => void
  importKnown: boolean
  setImportKnown: (v: boolean) => void
  keyConns: number
}) {
  const { prev, importKeys, setImportKeys, importKnown, setImportKnown, keyConns } = props
  const hosts = prev.knownHosts ?? []
  const keys = prev.keys ?? []
  const newHosts = hosts.filter((h) => !h.duplicate && !h.conflict).length
  const conflicts = hosts.filter((h) => h.conflict).length
  const [showHosts, setShowHosts] = useState(false)
  if (!hosts.length && !keys.length && !keyConns && !prev.counts.identities && !prev.counts.snippets) return null
  return (
    <div className="grid gap-2 rounded-lg border bg-card/60 p-3">
      {(keys.length > 0 || keyConns > 0) && (
        <CheckboxField
          checked={importKeys}
          onCheckedChange={(v) => setImportKeys(v === true)}
          label={
            keys.length > 0
              ? `Import ${keys.length} SSH key${keys.length === 1 ? '' : 's'}${keys.some((k) => k.duplicate) ? ' (already stored ones are reused)' : ''}`
              : `Import the private key files ${keyConns} session${keyConns === 1 ? '' : 's'} refer to`
          }
          description={keys.length > 0 ? undefined : 'Read from this computer when they exist (desktop mode); stored encrypted in the vault.'}
        />
      )}
      {hosts.length > 0 && (
        <div className="grid gap-1.5">
          <CheckboxField
            checked={importKnown}
            onCheckedChange={(v) => setImportKnown(v === true)}
            disabled={newHosts === 0}
            label={newHosts > 0 ? `Trust ${newHosts} new host key${newHosts === 1 ? '' : 's'}` : 'No new host keys to trust'}
            description={
              [
                hosts.length - newHosts - conflicts > 0 ? `${hosts.length - newHosts - conflicts} already trusted` : '',
                conflicts > 0 ? `${conflicts} differ from the key already trusted and are not imported` : '',
              ]
                .filter(Boolean)
                .join(' · ') || undefined
            }
          />
          <button
            type="button"
            className="flex w-fit items-center gap-1 text-sm text-primary hover:underline"
            aria-expanded={showHosts}
            onClick={() => setShowHosts((v) => !v)}
          >
            {showHosts ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />} {showHosts ? 'Hide' : 'Show'} host keys
          </button>
          {showHosts && (
            <ul className="grid max-h-36 gap-0.5 overflow-y-auto rounded-md border bg-background/40 p-1.5 text-sm">
              {hosts.map((h, i) => (
                <li key={i} className="flex min-w-0 items-center gap-2">
                  <span className="shrink-0 font-medium">{h.port === 22 ? h.host : `[${h.host}]:${h.port}`}</span>
                  <span className="shrink-0 text-xs text-muted-foreground">{h.keyType}</span>
                  <span className="min-w-0 truncate font-mono text-xs text-muted-foreground" title={h.fingerprint}>
                    {h.fingerprint}
                  </span>
                  {h.duplicate && <Badge variant="secondary">trusted</Badge>}
                  {h.conflict && <Badge variant="destructive">different key trusted</Badge>}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {(prev.counts.identities > 0 || prev.counts.snippets > 0) && (
        <p className="text-sm text-muted-foreground">
          {[
            prev.counts.identities > 0 && `${prev.counts.identities} identit${prev.counts.identities === 1 ? 'y' : 'ies'}`,
            prev.counts.snippets > 0 && `${prev.counts.snippets} snippet${prev.counts.snippets === 1 ? '' : 's'}`,
          ]
            .filter(Boolean)
            .join(' and ')}{' '}
          will be imported too.
        </p>
      )}
    </div>
  )
}

const TreeNode = memo(function TreeNode({
  node,
  selected,
  setMany,
  depth,
}: {
  node: TreeItem
  selected: Set<string>
  setMany: (ids: string[], on: boolean) => void
  depth: number
}) {
  const [open, setOpen] = useState(true)
  const [details, setDetails] = useState(false)
  const pad = { paddingLeft: `${depth * 16 + 6}px` }

  if (node.kind === 'folder') {
    const count = node.ids.filter((id) => selected.has(id)).length
    const state = count === 0 ? false : count === node.ids.length ? true : 'indeterminate'
    return (
      <li role="treeitem" aria-expanded={open} aria-selected={state === true}>
        <div className="flex items-center gap-2 rounded-md px-1.5 py-1 hover:bg-accent/60" style={pad}>
          <Checkbox checked={state} onCheckedChange={() => setMany(node.ids, state !== true)} aria-label={`Import folder ${node.name}`} />
          <button
            type="button"
            className="flex min-w-0 items-center gap-1.5 rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
            onClick={() => setOpen((v) => !v)}
            aria-label={`${open ? 'Collapse' : 'Expand'} ${node.name}`}
          >
            {open ? <ChevronDown className="size-3.5 shrink-0 opacity-70" /> : <ChevronRight className="size-3.5 shrink-0 opacity-70" />}
            <Folder className="size-4 shrink-0 text-muted-foreground" aria-hidden />
            <span className="truncate text-base font-medium">{node.name}</span>
          </button>
          <span className="ml-auto shrink-0 text-xs text-muted-foreground tabular-nums">
            {count}/{node.ids.length}
          </span>
        </div>
        {open && (
          <ul role="group">
            {node.children.map((c) => (
              <TreeNode key={c.key} node={c} selected={selected} setMany={setMany} depth={depth + 1} />
            ))}
          </ul>
        )}
      </li>
    )
  }

  const c = node.conn!
  const Icon = protocolIcon(c.protocol)
  const checked = selected.has(c.id)
  const detail = [c.username ? `${c.username}@` : '', c.host, c.port ? `:${c.port}` : ''].join('')
  const notes = c.warnings ?? []
  return (
    <li role="treeitem" aria-selected={checked}>
      <div className="flex items-center gap-2 rounded-md px-1.5 py-1 hover:bg-accent/60" style={pad}>
        <Checkbox checked={checked} onCheckedChange={() => setMany([c.id], !checked)} aria-label={`Import ${c.name}`} />
        <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        {c.color && <span className="size-2 shrink-0 rounded-full" style={{ background: c.color }} aria-hidden />}
        <span className="min-w-0 truncate text-base">{c.name}</span>
        {detail && <span className="min-w-0 truncate text-sm text-muted-foreground">{detail}</span>}
        <span className="ml-auto flex shrink-0 items-center gap-1.5">
          {c.via && c.via.length > 0 && (
            <span className="hidden items-center gap-1 text-xs text-muted-foreground sm:flex" title={`Through ${c.via.join(' → ')}`}>
              <Route className="size-3.5" aria-hidden /> {c.via.length === 1 ? c.via[0] : `${c.via.length} hops`}
            </span>
          )}
          {c.keyName && <KeyRound className="size-3.5 text-muted-foreground" aria-label={`Key: ${c.keyName}`} />}
          {c.runsLocalCommand && <Badge variant="destructive">local command</Badge>}
          {c.duplicate && <Badge variant="warning">exists</Badge>}
          <span className="text-xs text-muted-foreground">{protocolLabel(c.protocol)}</span>
          {notes.length > 0 && (
            <button
              type="button"
              className="flex items-center gap-0.5 rounded-sm px-0.5 text-xs text-warning outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring/50"
              aria-expanded={details}
              aria-label={`${notes.length} note${notes.length === 1 ? '' : 's'} for ${c.name}`}
              onClick={() => setDetails((v) => !v)}
            >
              <TriangleAlert className="size-3.5" aria-hidden /> {notes.length}
            </button>
          )}
        </span>
      </div>
      {details && notes.length > 0 && (
        <ul className="mb-1 grid gap-0.5 text-sm text-muted-foreground" style={{ paddingLeft: `${depth * 16 + 54}px` }}>
          {c.via && c.via.length > 0 && <li>· Through {c.via.join(' → ')}</li>}
          {notes.map((w, i) => (
            <li key={i} className="break-words">
              · {w}
            </li>
          ))}
        </ul>
      )}
    </li>
  )
})

// ---- step 3: result ------------------------------------------------------------------------------------------------

function ResultStep({ result }: { result: CommitResponse }) {
  const rows: { label: string; value: number; always?: boolean }[] = [
    { label: 'Created', value: result.created, always: true },
    { label: 'Updated', value: result.updated },
    { label: 'Skipped (already existed)', value: result.skipped },
    { label: 'Folders created', value: result.foldersCreated },
    { label: 'SSH gateways saved', value: result.gatewaysCreated ?? 0 },
    { label: 'Keys imported', value: result.keysImported },
    { label: 'Host keys trusted', value: result.knownHostsAdded },
    { label: 'Host keys not replaced', value: result.knownHostsConflicts ?? 0 },
    { label: 'Identities', value: result.identitiesCreated ?? 0 },
    { label: 'Snippets', value: result.snippetsCreated ?? 0 },
  ]
  return (
    <DialogBody className="grid content-start gap-4 animate-in fade-in-0 duration-150">
      <div className="flex items-center gap-2 text-md font-medium">
        <ShieldCheck className="size-5 text-success" aria-hidden />{' '}
        {result.created + result.updated + result.knownHostsAdded + result.keysImported + (result.snippetsCreated ?? 0) > 0
          ? 'Import finished'
          : 'Nothing new — everything in this file is already in NexTerm'}
      </div>
      <dl className="grid grid-cols-2 gap-2 sm:grid-cols-3">
        {rows
          .filter((r) => r.value > 0 || r.always)
          .map((r) => (
            <div key={r.label} className="flex flex-col-reverse rounded-lg border bg-card/60 p-3">
              <dt className="text-sm text-muted-foreground">{r.label}</dt>
              <dd className="text-lg font-semibold tabular-nums">{r.value}</dd>
            </div>
          ))}
      </dl>
      {result.warnings && result.warnings.length > 0 && <Notes title={`${result.warnings.length} note${result.warnings.length === 1 ? '' : 's'}`} items={result.warnings} />}
    </DialogBody>
  )
}

// ---- shared ---------------------------------------------------------------------------------------------------------

function Notes({ title, items }: { title: string; items: string[] }) {
  const [all, setAll] = useState(false)
  const shown = all ? items : items.slice(0, 6)
  return (
    <div className="rounded-lg border border-warning/40 bg-warning/10 p-2.5">
      <p className="flex items-center gap-1.5 text-sm font-medium">
        <TriangleAlert className="size-3.5 text-warning" aria-hidden /> {title}
      </p>
      <ul className="mt-1 grid max-h-40 gap-0.5 overflow-y-auto text-sm text-muted-foreground">
        {shown.map((w, i) => (
          <li key={i} className="break-words">
            · {w}
          </li>
        ))}
      </ul>
      {items.length > 6 && (
        <button type="button" className="mt-1 text-sm text-primary hover:underline" onClick={() => setAll((v) => !v)}>
          {all ? 'Show fewer' : `Show all ${items.length}`}
        </button>
      )}
    </div>
  )
}

function ErrorLine({ message }: { message: string }) {
  return (
    <p role="alert" className="flex items-center gap-1.5 text-sm text-destructive">
      <TriangleAlert className="size-4 shrink-0" aria-hidden /> {message}
    </p>
  )
}

// folderPaths returns folders with their "/"-joined display path, sorted by path.
function folderPaths(folders: FolderT[]): { id: string; path: string }[] {
  const byId = new Map(folders.map((f) => [f.id, f]))
  const pathOf = (f: FolderT): string => {
    const parts: string[] = []
    let cur: FolderT | undefined = f
    let guard = 0
    while (cur && guard++ < 64) {
      parts.unshift(cur.name)
      cur = cur.parentId ? byId.get(cur.parentId) : undefined
    }
    return parts.join(' / ')
  }
  return folders.map((f) => ({ id: f.id, path: pathOf(f) })).sort((a, b) => a.path.localeCompare(b.path))
}
