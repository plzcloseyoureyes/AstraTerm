/*
 * Tab kind "editor" — remote file editing in place (FILE-10): load through GET /api/fs/{id}/read, edit with
 * Monaco (or the hex editor for binary files, FILE-23), save back with PUT /api/fs/{id}/write + expectMtime.
 *
 *   conflicts (409)        → Overwrite / Reload / Compare & merge (in-tab merge view)
 *   permission denied      → Save with sudo (FILE-11) / Save as…
 *   changed on the server  → 5 s stat polling while the tab is focused → banner (Reload / Compare / Ignore)
 *   unsaved changes        → dirty marker in the tab title, close guard, beforeunload, crash backups (IndexedDB)
 *   autosave               → settings.editor.autosave: off | afterDelay | onFocusLost
 *   path bar               → breadcrumbs; every folder opens a quick-open list (Ctrl/⌘+O: files next to this one)
 *
 * The pieces live next to it: reading the file (fileread.ts), tab state and banner rules (tabstate.ts), the handle
 * (useFsHandle), change detection (useRemoteWatch), crash backups (useCrashBackup), save questions (saveprompts.tsx),
 * the full-pane states (EditorTabPanes), banners and compare toolbar (EditorTabBanners), status bar (StatusItems).
 */
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { FileSearch } from 'lucide-react'
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import type { FileEntry } from '@/api/types'
import { runCommand } from '@/app/commands'
import { commands, type TabProps } from '@/app/registry'
import { confirm } from '@/components/ui/dialog-host'
import { useLatest } from '@/lib/hooks'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { copyText, errorMessage, formatBytes, formatDateTime } from '@/lib/utils'
import { closeTab, updateTabParams, useIsTabVisible, useWorkspaceStore } from '@/stores/workspace'
import {
  downloadBlob,
  downloadUrl,
  handleLabel,
  infoLabel,
  isConflict,
  isDisconnected,
  isHandleGone,
  isNotFound,
  isNotSupported,
  isPermissionDenied,
  readFile,
  realpath,
  rememberHandle,
  statFile,
  triggerDownload,
  writeFile,
} from './api'
import { baseName, bytesToBase64, dirName, EncodeError, encodingInfo, utf8ByteLength } from './codec'
import { CodeEditor, useSessionStatus, VimStatus } from './CodeEditor'
import { applyDirtyTitle, registerController, setTabDirty, type ToggleName } from './controllers'
import { choose, saveAsDialog } from './dialogs'
import { EditorBanners, MergeToolbar } from './EditorTabBanners'
import { BinaryPane, ErrorPane, LargeFilePane } from './EditorTabPanes'
import { decodeServerVersion, readDocument, type DocFs } from './fileread'
import { statOf, type FileStat } from './filestat'
import { HexEditor, type HexEditorHandle, type HexSnapshot } from './HexEditor'
import { MergePane, type MergePaneHandle } from './MergePane'
import { loadMonaco } from './monaco/load'
import { safeFontFamily } from './monaco/palette'
import type { EditorSession } from './monaco/session'
import { openRemoteFile } from './open'
import { PathBar } from './PathBar'
import { crumbs } from './paths'
import { sameMeta, textBytes, writeBody, writeLocalHandle, type TextMeta } from './save'
import { askConflict, askPermissionDenied, askSaveAsUtf8 } from './saveprompts'
import { editorSettings, effectiveFontSize, FONT_MAX, FONT_MIN } from './settings'
import { FileLocation, saveStateLabel, TextStatusItems } from './StatusItems'
import { DEFAULT_META, HEX_MAX, MiB, TEXT_MAX, type BannerState, type BinaryFile, type MergeState, type Phase, type ReadOutcome } from './tabstate'
import { EditorToolbar } from './Toolbar'
import type { EditorMode, EditorTabParams } from './types'
import { LoadingOverlay, StatusBar, StatusSpacer } from './ui'
import { useCrashBackup } from './useCrashBackup'
import { useFsHandle } from './useFsHandle'
import { useRemoteWatch } from './useRemoteWatch'
import { useTabProgress } from './useTabProgress'

type SaveOpts = { overwrite?: boolean; sudo?: boolean; auto?: boolean; afterMerge?: boolean }

export default function EditorTab({ tabId, params }: TabProps<EditorTabParams>) {
  const { fsId, path } = params
  const name = baseName(path) || path
  const settings = editorSettings.use()
  const visible = useIsTabVisible(tabId)
  const fs = useFsHandle(tabId, params)
  const backups = useCrashBackup(path, params, fs)
  const trackProgress = useTabProgress(tabId)
  const host = handleLabel(fsId, params.label) ?? (fs.info ? infoLabel(fs.info) : undefined)
  const zoom = params.zoom ?? 0
  const mode: EditorMode = params.mode ?? 'text'

  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [session, setSession] = useState<EditorSession | null>(null)
  const status = useSessionStatus(session)
  const [meta, setMeta] = useState<TextMeta>(DEFAULT_META)
  const [savedMeta, setSavedMeta] = useState<TextMeta>(DEFAULT_META)
  const [fileStat, setFileStat] = useState<FileStat>({ size: 0 })
  const [readOnly, setReadOnly] = useState(!!params.readOnly)
  const [lockedRO, setLockedRO] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // "Saving…" in the status bar only for a save that takes a while (a quick one goes straight to "Saved").
  const savingShown = useDelayedFlag(saving)
  const [hexDirty, setHexDirty] = useState(false)
  const [banner, setBanner] = useState<BannerState | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [merge, setMerge] = useState<MergeState | null>(null)
  const [mergeChunks, setMergeChunks] = useState(0)
  const [lastSaved, setLastSaved] = useState<number | null>(null)
  /** Path bar segment whose folder list is open. */
  const [crumbOpen, setCrumbOpen] = useState<number | null>(null)

  const hexRef = useRef<HexEditorHandle>(null)
  const mergeRef = useRef<MergePaneHandle>(null)
  const sessionRef = useRef<EditorSession | null>(null)
  /** What the editor loaded or saved last (conflict checks, change detection). */
  const statRef = useRef<FileStat>({ size: 0 })
  const loadToken = useRef(0)
  const versionRef = useRef(0)
  const inflight = useRef<Promise<boolean> | null>(null)
  const ignoredMtime = useRef<string | null>(null)
  const pendingLine = useRef<number | undefined>(params.line)
  const pendingFind = useRef<string | undefined>(params.find)
  const backupTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const autosaveTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const discarded = useRef(false)
  const unmounted = useRef(false)
  /** Path to stat for change detection: the symlink target when the file is a symlink (writes follow links). */
  const statPath = useRef(path)

  const isHex = phase.kind === 'ready' && phase.doc.kind === 'hex'
  const isText = phase.kind === 'ready' && phase.doc.kind === 'text'
  const metaDirty = isText && !sameMeta(meta, savedMeta)
  const dirty = phase.kind === 'ready' && (isHex ? hexDirty : !!status?.docDirty || metaDirty)

  const canSudo = fs.info ? fs.info.capabilities?.sudo !== false : true
  const isActiveTab = () => useWorkspaceStore.getState().activeTabId === tabId
  const live = useLatest({ phase, meta, savedMeta, readOnly, dirty, merge, banner, settings, visible, lockedRO, zoom, mode, params, host, name, canSudo })

  /** Unsaved changes right now (the rendered `dirty` lags one frame behind typing). */
  const dirtyNow = (): boolean => {
    const p = live.current.phase
    if (p.kind !== 'ready') return false
    if (p.doc.kind === 'hex') return hexRef.current?.isDirty() ?? false
    const s = sessionRef.current
    if ((!!s && s.isDocDirty()) || !sameMeta(live.current.meta, live.current.savedMeta)) return true
    // Edits made in the compare view are not in the editor until applied.
    const m = live.current.merge
    return !!m && !!mergeRef.current && mergeRef.current.isModified('b', m.mine)
  }

  // ---------------------------------------------------------------------------------------------------------------
  // loading

  /** The file operations, on the current handle (re-opened transparently when it expired). */
  const docFs = useMemo<DocFs>(
    () => ({
      stat: (p) => fs.call((id) => statFile(id, p)),
      realpath: (p) => fs.call((id) => realpath(id, p)),
      read: (p, maxBytes) => fs.call((id) => readFile(id, p, maxBytes)),
    }),
    [fs.call],
  )

  const open = useCallback(
    async (opts: { reason: 'initial' | 'reload'; force?: boolean; mode?: EditorMode; encoding?: string }) => {
      const token = ++loadToken.current
      const m = opts.mode ?? live.current.mode
      const cur = live.current.phase
      const keepEditor = opts.reason === 'reload' && cur.kind === 'ready' && cur.doc.kind === 'text' && m === 'text' && !!sessionRef.current
      // A reload keeps the document on screen (progress in the tab header); anything else shows the loading pane.
      if (!keepEditor) setPhase({ kind: 'loading' })
      const s = editorSettings.get()
      const reading = readDocument(docFs, path, {
        mode: m,
        encoding: opts.encoding ?? live.current.params.encoding,
        force: opts.force || !!live.current.params.large,
        largeBytes: Math.max(1, s.largeFileMiB) * MiB,
        defaultEol: s.defaultEol,
      })
      let r: ReadOutcome
      try {
        const res = await (keepEditor ? trackProgress(reading) : reading)
        if (token === loadToken.current) statPath.current = res.statPath
        r = res.outcome
      } catch (err) {
        r = { kind: 'error', message: errorMessage(err) }
      }
      if (token !== loadToken.current) return
      if (r.kind !== 'ready') {
        if (keepEditor && r.kind === 'error') {
          toast.error(`Could not reload ${name}`, { description: r.message })
          return
        }
        setPhase(r)
        return
      }
      statRef.current = r.stat
      setFileStat(r.stat)
      setBanner(null)
      setMerge(null)
      ignoredMtime.current = null
      discarded.current = false
      const doc = r.doc
      if (doc.truncated) {
        setReadOnly(true)
        setLockedRO(`Only the first ${formatBytes(doc.kind === 'hex' ? doc.bytes.length : utf8ByteLength(doc.text))} of ${formatBytes(r.stat.size)} were loaded.`)
      } else if (doc.kind === 'text' && doc.lossy) {
        setReadOnly(true)
        setLockedRO(
          `${name} is not valid ${encodingInfo(doc.meta.encoding).label}: some bytes could not be decoded and saving would change them. Reopen it with another encoding (status bar) or in the hex editor.`,
        )
      } else {
        setLockedRO(null)
        // a previous lossy / truncated load forced read-only
        if (live.current.lockedRO) setReadOnly(!!live.current.params.readOnly)
      }
      if (doc.kind === 'text') {
        setMeta(doc.meta)
        setSavedMeta(doc.meta)
        setNote(doc.note ?? null)
        if (keepEditor && sessionRef.current) {
          sessionRef.current.setText(doc.text, { reset: true, markSaved: true, eol: doc.meta.eol })
          setPhase((p) => (p.kind === 'ready' ? { ...p, doc } : p))
        } else {
          setPhase({ kind: 'ready', doc, version: ++versionRef.current })
        }
        if (opts.reason === 'initial') void backups.check(doc.text)
      } else {
        setNote(null)
        setHexDirty(false)
        setPhase({ kind: 'ready', doc, version: ++versionRef.current })
      }
    },
    [backups.check, docFs, live, name, path, trackProgress],
  )

  // Load Monaco while the file is read rather than after it, so one loading pane covers both (monaco/load.ts).
  useEffect(() => {
    if (mode === 'text') void loadMonaco().catch(() => undefined)
  }, [mode])

  useEffect(() => {
    // A reconnect only swaps the handle: the buffer stays.
    if (fs.takeReconnected(fsId)) return
    void open({ reason: 'initial' })
    // Re-open when the tab is pointed at another file.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fsId, path])

  // A load still running when the tab closes is dropped.
  useEffect(
    () => () => {
      loadToken.current++
    },
    [],
  )

  // reveal a requested line once the editor exists
  useEffect(() => {
    if (!session) return
    if (pendingLine.current) {
      const line = pendingLine.current
      pendingLine.current = undefined
      requestAnimationFrame(() => session.revealLine(line))
      updateTabParams<EditorTabParams>(tabId, { line: undefined })
    }
    if (pendingFind.current) {
      const text = pendingFind.current
      pendingFind.current = undefined
      setTimeout(() => session.revealText(text), 0)
      updateTabParams<EditorTabParams>(tabId, { find: undefined })
    }
    if (live.current.params.wrap !== undefined) session.setWrap(!!live.current.params.wrap)
    if (live.current.params.whitespace !== undefined) session.setWhitespace(!!live.current.params.whitespace)
  }, [session, live, tabId])

  // ---------------------------------------------------------------------------------------------------------------
  // dirty state, title, backups

  useEffect(() => {
    setTabDirty(tabId, dirty)
    applyDirtyTitle(tabId, dirty)
  }, [tabId, dirty])

  useEffect(() => {
    unmounted.current = false
    return () => {
      unmounted.current = true
      setTabDirty(tabId, false)
      if (backupTimer.current) clearTimeout(backupTimer.current)
      if (autosaveTimer.current) clearTimeout(autosaveTimer.current)
    }
  }, [tabId])

  const writeBackup = () => {
    const s = sessionRef.current
    if (!s || discarded.current) return
    const L = live.current
    if (!s.isDocDirty() && sameMeta(L.meta, L.savedMeta)) {
      backups.drop()
      return
    }
    // Stored with the document's line endings (mixed ones included), so a restore is exact.
    backups.write(s.textForSave(L.meta), L.meta, { baseMtime: statRef.current.mtime, label: L.host })
  }

  // ---------------------------------------------------------------------------------------------------------------
  // saving

  const currentBody = (hexSnap?: HexSnapshot): { content: string; encoding: 'utf-8' | 'base64'; meta?: TextMeta } | null => {
    const p = live.current.phase
    if (p.kind !== 'ready') return null
    if (p.doc.kind === 'hex') {
      const bytes = hexSnap?.bytes ?? hexRef.current?.getBytes()
      return bytes ? { content: bytesToBase64(bytes), encoding: 'base64' } : null
    }
    const s = sessionRef.current
    if (!s) return null
    const m = live.current.meta
    return { ...writeBody(s.textForSave(m), m), meta: m }
  }

  const doSave = async (opts: SaveOpts): Promise<boolean> => {
    const L = live.current
    if (L.phase.kind !== 'ready') return false
    if (L.readOnly) {
      if (!opts.auto) toast.info(`${name} is read-only`, { description: L.lockedRO ?? 'Allow editing from the toolbar (lock icon) first.' })
      return false
    }
    if (L.merge && !opts.afterMerge) {
      if (!opts.auto) toast.info('Finish or cancel the comparison first')
      return false
    }
    const s = sessionRef.current
    // What this save writes: edits made while it runs stay unsaved.
    let snapshot = L.phase.doc.kind === 'text' ? s?.snapshot() : undefined
    const hexSnap = L.phase.doc.kind === 'hex' ? hexRef.current?.snapshot() : undefined
    let metaAtSave = L.meta
    let body: { content: string; encoding: 'utf-8' | 'base64' } | null
    try {
      body = currentBody(hexSnap)
    } catch (err) {
      if (!(err instanceof EncodeError)) throw err
      if (opts.auto) {
        setBanner({ kind: 'error', message: err.message })
        return false
      }
      if (!(await askSaveAsUtf8(encodingInfo(metaAtSave.encoding).label, err.message)) || !s) return false
      metaAtSave = { ...metaAtSave, encoding: 'utf-8', bom: false }
      setMeta(metaAtSave)
      snapshot = s.snapshot()
      body = writeBody(s.textForSave(metaAtSave), metaAtSave)
    }
    if (!body) return false

    setSaving(true)
    let error: unknown = null
    let entry: FileEntry | undefined
    try {
      const payload = body
      entry = await fs.call((id) =>
        writeFile(id, {
          path,
          content: payload.content,
          encoding: payload.encoding,
          expectMtime: opts.overwrite ? undefined : statRef.current.mtime,
          expectSize: opts.overwrite ? undefined : statRef.current.size,
          sudo: opts.sudo || !!L.params.sudo,
        }),
      )
    } catch (err) {
      error = err
    } finally {
      setSaving(false)
    }

    if (!error) {
      let next = statOf(entry, { ...statRef.current, mtime: undefined })
      // The write reply describes the file written (symlinks followed); older servers answered for the link itself.
      if (!entry?.mtime || entry.type === 'symlink') {
        try {
          next = statOf(await fs.call((id) => statFile(id, statPath.current)), next)
        } catch {
          /* keep what we have */
        }
      }
      statRef.current = next
      setFileStat(next)
      if (L.phase.doc.kind === 'hex') hexRef.current?.markSaved(hexSnap)
      else if (s && snapshot) {
        s.markSaved(snapshot)
        setSavedMeta(metaAtSave)
      }
      setBanner(null)
      ignoredMtime.current = null
      setLastSaved(Date.now())
      // Changes typed while the upload ran are not on disk: keep (or create) their crash backup.
      if (dirtyNow()) writeBackup()
      else backups.drop()
      if (opts.sudo) {
        toast.success(`Saved ${name} with sudo`, { description: 'Later saves of this file use sudo automatically.' })
        if (!L.params.sudo) updateTabParams<EditorTabParams>(tabId, { sudo: true })
      }
      // Typed while the upload was running: autosave those changes too.
      if (live.current.settings.autosave === 'afterDelay' && dirtyNow()) scheduleAutosave()
      return true
    }

    if (isConflict(error)) {
      if (opts.auto) {
        setBanner({ kind: 'conflict' })
        return false
      }
      const serverMtime = isApiError(error) ? (error.body as { mtime?: string } | undefined)?.mtime : undefined
      const r = await askConflict(name, serverMtime)
      if (r === 'overwrite') return doSave({ ...opts, overwrite: true })
      if (r === 'reload') {
        // "discard mine": the crash backup of those changes goes too
        backups.drop()
        backups.dismiss()
        void open({ reason: 'reload' })
        return false
      }
      if (r === 'diff') void enterMerge('conflict')
      else setBanner({ kind: 'conflict' })
      return false
    }
    if (isPermissionDenied(error) && !opts.sudo) {
      if (opts.auto) {
        setBanner({ kind: 'denied', message: errorMessage(error) })
        return false
      }
      const r = await askPermissionDenied({ path, host: live.current.host, message: errorMessage(error), canSudo: live.current.canSudo })
      if (r === 'sudo') return doSave({ ...opts, sudo: true })
      if (r === 'saveas') return saveAs()
      return false
    }
    if (opts.sudo && isNotSupported(error)) {
      toast.error('Save with sudo is not available here', { description: 'This file system cannot run commands on the server (sudo needs an SSH connection with exec access).' })
      return false
    }
    if (isDisconnected(error)) {
      const msg = 'The connection to the server was lost. Your changes are kept in the editor — reconnect the session, then save again.'
      if (opts.auto) setBanner({ kind: 'error', message: msg })
      else toast.error(`Could not save ${name}`, { description: msg })
      return false
    }
    if (isHandleGone(error)) {
      setBanner({ kind: 'gone' })
      if (!opts.auto) toast.error(`Could not save ${name}`, { description: 'The file system connection is no longer available. Reconnect and try again.' })
      return false
    }
    if (opts.auto) setBanner({ kind: 'error', message: `Autosave failed: ${errorMessage(error)}` })
    else toast.error(`Could not save ${name}`, { description: errorMessage(error) })
    return false
  }

  const save = (opts: SaveOpts = {}): Promise<boolean> => {
    if (inflight.current) {
      // An autosave retries after the running save; an explicit save (Ctrl+S, the close guard's "Save") waits for it
      // and then saves whatever changed meanwhile — resolving true only once everything is on disk.
      if (opts.auto) return Promise.resolve(false)
      return inflight.current.then((ok) => (ok ? save(opts) : false))
    }
    // Nothing to write (Ctrl+S on an unmodified file); a missing / deleted file is still (re)created.
    const B = live.current.banner
    if (!dirtyNow() && !opts.overwrite && !opts.sudo && statRef.current.mtime && B?.kind !== 'deleted') return Promise.resolve(true)
    if (opts.auto) {
      const L = live.current
      if (!dirtyNow() || L.readOnly || L.merge || (L.banner && L.banner.kind !== 'changed')) return Promise.resolve(false)
    }
    const p = doSave(opts)
      .catch((err: unknown) => {
        toast.error(`Could not save ${name}`, { description: errorMessage(err) })
        return false
      })
      .finally(() => {
        inflight.current = null
      })
    inflight.current = p
    return p
  }

  const contentBytes = (): Uint8Array | null => {
    const p = live.current.phase
    if (p.kind !== 'ready') return null
    if (p.doc.kind === 'hex') return hexRef.current?.getBytes() ?? null
    const s = sessionRef.current
    if (!s) return null
    const m = live.current.meta
    try {
      return textBytes(s.textForSave(m), m)
    } catch {
      return textBytes(s.textForSave(m), { encoding: 'utf-8', bom: false })
    }
  }

  const saveAs = async (): Promise<boolean> => {
    const r = await saveAsDialog({ suggestedName: name, fsId, dir: dirName(path), label: live.current.host, allowLocal: true })
    if (!r) return false
    if (r.target === 'remote') {
      if (r.fsId === fsId && r.path === path) return save()
      let body: { content: string; encoding: 'utf-8' | 'base64' } | null = null
      try {
        body = currentBody()
      } catch (err) {
        toast.error('Cannot encode the document', { description: errorMessage(err) })
        return false
      }
      if (!body) return false
      try {
        await statFile(r.fsId, r.path)
        const ok = await confirm({ title: `Replace ${baseName(r.path)}?`, description: `${r.path} already exists. Replacing it overwrites its contents.`, confirmLabel: 'Replace', destructive: true })
        if (!ok) return false
      } catch (err) {
        if (!isNotFound(err) || isHandleGone(err)) {
          toast.error('Cannot save there', { description: errorMessage(err) })
          return false
        }
      }
      try {
        await writeFile(r.fsId, { path: r.path, content: body.content, encoding: body.encoding })
      } catch (err) {
        toast.error(`Could not save ${baseName(r.path)}`, { description: errorMessage(err) })
        return false
      }
      discarded.current = true
      backups.drop()
      if (r.label) rememberHandle({ id: r.fsId, label: r.label })
      openRemoteFile({ fsId: r.fsId, path: r.path, label: r.label, source: r.source, ownsFs: r.ownsFs, zoom: live.current.zoom || undefined })
      toast.success(`Saved as ${baseName(r.path)}`)
      await closeTab(tabId, { force: true })
      return true
    }
    const bytes = contentBytes()
    if (!bytes) return false
    try {
      if (r.target === 'download') downloadBlob(bytes as unknown as BlobPart, r.filename)
      else await writeLocalHandle(r.handle, bytes)
      toast.success(r.target === 'download' ? `Downloading ${r.filename}` : `Saved a copy as ${r.filename}`)
      return true
    } catch (err) {
      toast.error('Could not save the file', { description: errorMessage(err) })
      return false
    }
  }

  // ---------------------------------------------------------------------------------------------------------------
  // merge / compare

  const enterMerge = async (reason: MergeState['reason']) => {
    const s = sessionRef.current
    const L = live.current
    if (!s || L.phase.kind !== 'ready' || L.phase.doc.kind !== 'text') {
      if (L.phase.kind === 'ready' && L.phase.doc.kind === 'hex') toast.info('Comparing is available in the text view')
      return
    }
    try {
      const res = await trackProgress(fs.call((id) => readFile(id, path, TEXT_MAX)))
      const server = decodeServerVersion(res, L.meta.encoding)
      setMergeChunks(-1)
      setMerge({ theirs: server.text, theirsRaw: server.raw, theirsStat: statOf({ mtime: res.mtime, size: res.size, mode: res.mode }, statRef.current), reason, mine: s.getText() })
    } catch (err) {
      if (isNotFound(err) && !isHandleGone(err)) toast.error(`${name} no longer exists on the server`)
      else toast.error('Could not read the server version', { description: errorMessage(err) })
    }
  }

  /** Take the merged text into the editor (the server version becomes the base); resolves whether it was saved. */
  const applyMerge = async (thenSave: boolean): Promise<boolean> => {
    const m = live.current.merge
    const s = sessionRef.current
    if (!m || !s) return false
    const merged = mergeRef.current?.getText('b') ?? m.mine
    // Only the differing parts are replaced: untouched lines keep their line endings, and lines that match the server
    // version take the server's line endings (a mixed-EOL file stays byte-exact where nobody changed it).
    if (merged !== s.getText()) s.setText(merged)
    s.markSavedText(m.theirs)
    const mixed = s.adoptLineEndings(m.theirsRaw)
    const L = live.current
    // Keep per-line endings when the result mixes them, unless the user chose to convert the line endings.
    if (mixed && !L.meta.mixedEol && sameMeta(L.meta, L.savedMeta)) {
      const next = { ...L.meta, mixedEol: true }
      setMeta(next)
      setSavedMeta(next)
    }
    statRef.current = m.theirsStat
    setFileStat(m.theirsStat)
    setMerge(null)
    setBanner(null)
    ignoredMtime.current = null
    requestAnimationFrame(() => s.focus())
    return thenSave ? save({ afterMerge: true }) : true
  }

  const closeMerge = () => {
    const m = live.current.merge
    setMerge(null)
    if (m?.reason === 'conflict') setBanner({ kind: 'conflict' })
    requestAnimationFrame(() => sessionRef.current?.focus())
  }

  // ---------------------------------------------------------------------------------------------------------------
  // external changes (FILE-10: poll while focused)

  useRemoteWatch({
    enabled: settings.watchRemote && phase.kind === 'ready',
    target: `${fsId}\n${path}`,
    canPoll: () => live.current.visible && !inflight.current && !live.current.merge,
    stat: () => fs.call((id) => statFile(id, statPath.current)),
    base: () => statRef.current,
    ignoredMtime: () => ignoredMtime.current,
    setBanner,
  })

  // ---------------------------------------------------------------------------------------------------------------
  // autosave on focus loss / tab hidden

  useEffect(() => {
    if (!visible && settings.autosave === 'onFocusLost') void save({ auto: true })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [visible])

  useEffect(() => {
    if (settings.autosave !== 'onFocusLost') return
    const onBlur = () => void save({ auto: true })
    window.addEventListener('blur', onBlur)
    return () => window.removeEventListener('blur', onBlur)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [settings.autosave])

  function scheduleAutosave() {
    if (unmounted.current) return
    if (autosaveTimer.current) clearTimeout(autosaveTimer.current)
    autosaveTimer.current = setTimeout(() => {
      autosaveTimer.current = null
      // A save is running: try again once it is done (the changes typed meanwhile still need saving).
      if (inflight.current) void inflight.current.then(() => live.current.settings.autosave === 'afterDelay' && dirtyNow() && scheduleAutosave())
      else void save({ auto: true })
    }, Math.max(300, live.current.settings.autosaveDelay))
  }

  const onDocChange = () => {
    if (backupTimer.current) clearTimeout(backupTimer.current)
    backupTimer.current = setTimeout(writeBackup, 800)
    if (live.current.settings.autosave === 'afterDelay') scheduleAutosave()
  }

  // ---------------------------------------------------------------------------------------------------------------
  // actions

  const reload = async () => {
    if (live.current.dirty) {
      const ok = await confirm({ title: `Reload ${name}?`, description: 'Your unsaved changes will be lost.', confirmLabel: 'Discard & reload', destructive: true })
      if (!ok) return
    }
    backups.drop()
    backups.dismiss()
    void open({ reason: 'reload' })
  }

  /** Reopen the file in the text or hex view (no questions asked). */
  const openAs = (next: EditorMode) => {
    updateTabParams<EditorTabParams>(tabId, { mode: next })
    void open({ reason: 'initial', mode: next, force: true })
  }

  const switchMode = async () => {
    const next: EditorMode = live.current.mode === 'hex' ? 'text' : 'hex'
    if (live.current.dirty) {
      const r = await choose({
        title: 'Unsaved changes',
        description: `Save ${name} before switching to the ${next === 'hex' ? 'hex' : 'text'} view?`,
        choices: [
          { value: 'cancel', label: 'Cancel' },
          { value: 'discard', label: "Don't save", variant: 'destructive' },
          { value: 'save', label: 'Save', variant: 'default' },
        ],
        cancel: 'cancel',
      })
      if (r === 'cancel') return
      if (r === 'save' && !(await save())) return
    }
    openAs(next)
  }

  const reopenWithEncoding = async (enc: string) => {
    if (live.current.dirty) {
      const ok = await confirm({ title: 'Reopen with another encoding?', description: 'Your unsaved changes will be lost.', confirmLabel: 'Reopen', destructive: true })
      if (!ok) return
    }
    updateTabParams<EditorTabParams>(tabId, { encoding: enc === 'utf-8' ? undefined : enc })
    void open({ reason: 'initial', encoding: enc, force: true })
  }

  /** The bytes are already here: show them in the hex editor without reading the file again. */
  const openBinaryAsHex = (file: BinaryFile) => {
    updateTabParams<EditorTabParams>(tabId, { mode: 'hex' })
    statRef.current = file.stat
    setFileStat(file.stat)
    setHexDirty(false)
    if (file.bytes.length > HEX_MAX) {
      setReadOnly(true)
      setLockedRO(`Files over ${formatBytes(HEX_MAX)} are shown read-only in the hex editor.`)
    }
    setPhase({ kind: 'ready', doc: { kind: 'hex', bytes: file.bytes, truncated: false }, version: ++versionRef.current })
  }

  /** "Create empty file" for a missing file: an empty document that saving creates. */
  const startEmpty = () => {
    const m: TextMeta = { encoding: 'utf-8', bom: false, eol: settings.defaultEol, mixedEol: false }
    statRef.current = { size: 0 }
    setFileStat({ size: 0 })
    setMeta(m)
    setSavedMeta(m)
    setPhase({ kind: 'ready', doc: { kind: 'text', text: '', meta: m, truncated: false }, version: ++versionRef.current })
    setBanner({ kind: 'deleted' })
  }

  const restoreBackup = () => {
    const b = backups.recovered
    const s = sessionRef.current
    if (!b || !s) return
    s.setText(b.content, { exactBreaks: true })
    setMeta(b.meta)
    if (b.baseMtime) statRef.current = { ...statRef.current, mtime: b.baseMtime }
    backups.dismiss()
    toast.success('Unsaved changes restored', { description: 'Save to write them to the server.' })
  }

  const toggle = (what: ToggleName) => {
    const s = sessionRef.current
    if (what === 'readOnly') {
      if (live.current.lockedRO) {
        toast.info('This file cannot be edited', { description: live.current.lockedRO })
        return
      }
      setReadOnly((v) => !v)
      return
    }
    if (what === 'vim') {
      editorSettings.set({ vimMode: !editorSettings.get().vimMode })
      return
    }
    if (!s) return
    const st = s.getStatus()
    if (what === 'wrap') {
      s.setWrap(!st.wrap)
      updateTabParams<EditorTabParams>(tabId, { wrap: !st.wrap })
    } else if (what === 'whitespace') {
      s.setWhitespace(!st.whitespace)
      updateTabParams<EditorTabParams>(tabId, { whitespace: !st.whitespace })
    } else if (what === 'minimap') {
      if (st.large) {
        toast.info('The minimap is off for large files')
        return
      }
      s.setMinimap(!st.minimap)
      updateTabParams<EditorTabParams>(tabId, { minimap: !st.minimap })
    }
  }

  const setZoom = (delta: number) => {
    const cur = live.current.zoom
    const base = editorSettings.get().fontSize
    const next = delta === 0 ? 0 : Math.min(FONT_MAX - base, Math.max(FONT_MIN - base, cur + delta))
    updateTabParams<EditorTabParams>(tabId, { zoom: next || undefined })
  }

  const reconnect = async () => {
    if (!live.current.params.source) return
    // With a buffer the handle is swapped under it; from the error pane the new handle id triggers a fresh load.
    const id = await fs.reconnect(live.current.phase.kind === 'ready')
    if (!id) {
      toast.error('Could not reconnect', { description: 'The session or connection this file came from is not available. Reopen the file from the file browser.' })
      return
    }
    setBanner(null)
  }

  const download = () => triggerDownload(downloadUrl(fs.id(), path), name)

  /** "Show in file browser" for the path bar, when the files feature can open this tab's origin. */
  const browseFolder = (): ((dir: string) => void) | undefined => {
    const src = params.source
    if (src?.sessionId && commands.get('files.openForSession')) return (dir) => void runCommand('files.openForSession', { sessionId: src.sessionId, path: dir })
    if (src?.connectionId && commands.get('files.openForConnection')) return (dir) => void runCommand('files.openForConnection', { connectionId: src.connectionId, path: dir })
    if (src?.local && commands.get('files.openLocal')) return (dir) => void runCommand('files.openLocal', { path: dir })
    return undefined
  }

  const actions = useLatest({ save, saveAs, reload, switchMode, toggle, setZoom, enterMerge, dirtyNow, applyMerge, dropBackups: backups.drop })

  useEffect(
    () =>
      registerController({
        tabId,
        kind: 'editor',
        isDirty: () => actions.current.dirtyNow(),
        // In the compare view Ctrl+S means "apply the merged text and save".
        save: () => (live.current.merge ? actions.current.applyMerge(true) : actions.current.save()),
        saveAs: () => actions.current.saveAs(),
        discard: () => {
          discarded.current = true
          if (backupTimer.current) clearTimeout(backupTimer.current)
          actions.current.dropBackups()
        },
        reload: () => void actions.current.reload(),
        find: (replace) => {
          if (live.current.merge) mergeRef.current?.openSearch()
          else if (hexRef.current) hexRef.current.find()
          else sessionRef.current?.openSearch(replace)
        },
        gotoLine: () => {
          if (live.current.merge) mergeRef.current?.gotoLine()
          else if (hexRef.current) hexRef.current.gotoOffset()
          else sessionRef.current?.gotoLine()
        },
        zoom: (d) => actions.current.setZoom(d),
        toggle: (w) => actions.current.toggle(w),
        nextChange: (dir) => {
          if (live.current.merge) mergeRef.current?.nextChange(dir)
        },
        compareWithSaved: () => void actions.current.enterMerge('compare'),
        format: () => (live.current.merge || hexRef.current ? undefined : sessionRef.current?.formatDocument()),
        editorCommands: () => (live.current.merge || hexRef.current ? undefined : sessionRef.current?.commandPalette()),
        switchMode: () => void actions.current.switchMode(),
        goToFile: () => {
          if (!live.current.merge && live.current.phase.kind === 'ready') setCrumbOpen(crumbs(path).length - 1)
        },
        revealLine: (line) => sessionRef.current?.revealLine(line),
        revealText: (text) => sessionRef.current?.revealText(text),
        focus: () => (hexRef.current ? hexRef.current.focus() : sessionRef.current?.focus()),
        getText: () => sessionRef.current?.getText() ?? '',
        printable: () => {
          const s = sessionRef.current
          const L = live.current
          return s && !L.merge && L.phase.kind === 'ready' && L.phase.doc.kind === 'text' ? s.printable(L.name) : null
        },
      }),
    [actions, fsId, live, path, tabId],
  )

  // ---------------------------------------------------------------------------------------------------------------
  // render

  const fontSize = effectiveFontSize(settings, zoom)
  const statusLeft = (
    <FileLocation stat={fileStat} sudo={params.sudo} saveState={saveStateLabel({ saving: savingShown, readOnly, dirty, lastSaved })} />
  )

  let body: ReactNode = null
  if (phase.kind === 'error') {
    body = (
      <ErrorPane
        error={phase}
        name={name}
        path={path}
        host={host}
        canReconnect={!!params.source}
        onRetry={() => void open({ reason: 'initial' })}
        onReconnect={() => void reconnect()}
        onCreate={startEmpty}
        onDownload={download}
        onClose={() => void closeTab(tabId, { force: true })}
      />
    )
  } else if (phase.kind === 'large') {
    body = (
      <LargeFilePane
        name={name}
        size={phase.size}
        onOpen={() => {
          updateTabParams<EditorTabParams>(tabId, { large: true })
          void open({ reason: 'initial', force: true })
        }}
        onHex={() => openAs('hex')}
        onDownload={download}
      />
    )
  } else if (phase.kind === 'binary') {
    body = <BinaryPane name={name} file={phase} onHex={() => openBinaryAsHex(phase)} onDecode={(enc) => void reopenWithEncoding(enc)} onDownload={download} />
  } else if (phase.kind === 'ready' && phase.doc.kind === 'hex') {
    body = (
      <HexEditor
        key={phase.version}
        ref={hexRef}
        data={phase.doc.bytes}
        readOnly={readOnly}
        fontSize={fontSize}
        onDirtyChange={setHexDirty}
        statusLeft={statusLeft}
        label={name}
        autoFocus={isActiveTab()}
      />
    )
  } else if (phase.kind === 'ready' && phase.doc.kind === 'text') {
    body = (
      <>
        {merge && (
          <MergeToolbar
            merge={merge}
            chunks={mergeChunks}
            onPrev={() => mergeRef.current?.nextChange(-1)}
            onNext={() => mergeRef.current?.nextChange(1)}
            onTakeServer={() => mergeRef.current?.setText('b', merge.theirs)}
            onApply={() => void applyMerge(false)}
            onSave={() => void applyMerge(true)}
            onCancel={closeMerge}
          />
        )}
        {merge && (
          <MergePane
            ref={mergeRef}
            tabId={tabId}
            a={{ text: merge.theirs, readOnly: true, label: <>On the server · {merge.theirsStat.mtime ? `changed ${formatDateTime(merge.theirsStat.mtime)}` : 'current version'}</> }}
            b={{ text: merge.mine, label: <>Your version · {dirty ? 'unsaved, editable' : 'editable'}</> }}
            path={path}
            language={session?.getStatus().language}
            unified={false}
            ignoreWhitespace={false}
            collapse
            revert="a-to-b"
            zoom={zoom}
            onChunks={setMergeChunks}
          />
        )}
        <CodeEditor
          key={phase.version}
          tabId={tabId}
          initialText={phase.doc.text}
          eol={phase.doc.meta.eol}
          path={path}
          language={params.language}
          readOnly={readOnly}
          zoom={zoom}
          minimap={params.minimap}
          hidden={!!merge}
          autoFocus={isActiveTab()}
          onReady={(s) => {
            sessionRef.current = s
            setSession(s)
          }}
          onDocChange={onDocChange}
          onFocusChange={(f) => {
            if (!f && live.current.settings.autosave === 'onFocusLost') void save({ auto: true })
          }}
        />
        <StatusBar>
          {statusLeft}
          <StatusSpacer />
          <VimStatus session={session} active={!!status?.vim} />
          {status && (
            <TextStatusItems
              session={session}
              status={status}
              meta={meta}
              tabSize={settings.tabSize}
              readOnly={readOnly}
              onReopen={(enc) => void reopenWithEncoding(enc)}
              onSaveWith={(enc, bom) => setMeta((m) => ({ ...m, encoding: enc, bom }))}
              onEol={(eol) => setMeta((m) => ({ ...m, eol, mixedEol: false }))}
              onLanguage={(n) => {
                void session?.setLanguageByName(n)
                updateTabParams<EditorTabParams>(tabId, { language: n })
              }}
            />
          )}
        </StatusBar>
      </>
    )
  }

  // The tab's chrome (toolbar, path bar, status bar) is there from the first frame while the file opens — inert until
  // it is ready — so opening a file never blanks and re-draws the toolbar.
  const opening = phase.kind === 'loading'
  const showToolbar = (phase.kind === 'ready' || opening) && !merge
  const textStatus = isText ? status : null

  return (
    <div className="flex h-full min-h-0 flex-col" data-editor-tab={tabId}>
      {showToolbar && (
        <div inert={opening} className="contents">
          <EditorToolbar
            mode={isHex || (opening && mode === 'hex') ? 'hex' : 'text'}
            dirty={dirty}
            saving={saving}
            canSave={!opening && (dirty || isHex || !statRef.current.mtime)}
            onSave={() => void save()}
            onSaveAs={() => void saveAs()}
            canUndo={isHex ? true : !!textStatus?.canUndo}
            canRedo={isHex ? true : !!textStatus?.canRedo}
            onUndo={() => (isHex ? hexRef.current?.undo() : session?.undo())}
            onRedo={() => (isHex ? hexRef.current?.redo() : session?.redo())}
            onFind={() => (isHex ? hexRef.current?.find() : session?.openSearch(false))}
            onReplace={isHex ? undefined : () => session?.openSearch(true)}
            onGoto={() => (isHex ? hexRef.current?.gotoOffset() : session?.gotoLine())}
            wrap={textStatus?.wrap}
            whitespace={textStatus?.whitespace}
            minimap={textStatus ? textStatus.minimap : opening && mode === 'text' ? false : undefined}
            vim={settings.vimMode}
            onFormat={isHex || !session?.canFormat() ? undefined : () => session.formatDocument()}
            onCommandPalette={isHex ? undefined : () => session?.commandPalette()}
            onToggle={isHex ? undefined : (w) => toggle(w)}
            onZoom={setZoom}
            readOnly={readOnly}
            onToggleReadOnly={lockedRO ? undefined : () => toggle('readOnly')}
            onReload={() => void reload()}
            onCompare={isHex ? undefined : () => void enterMerge('compare')}
            onSwitchMode={() => void switchMode()}
            onDownload={download}
            onCopyPath={() => void copyText(path).then((ok) => ok && toast.success('Path copied'))}
            onExportHtml={isHex ? undefined : () => void runCommand('editor.exportHtml', { tabId })}
            onPrint={isHex ? undefined : () => void runCommand('editor.print', { tabId })}
            more={[{ label: 'Find in files…', icon: FileSearch, onSelect: () => void runCommand('editor.findInFiles', { fsId: fs.id(), dir: dirName(path), label: host }) }]}
          >
            {isHex && <span className="mr-2 hidden truncate text-xs text-muted-foreground @2xl:inline">Tab: switch column · Insert: INS/OVR</span>}
          </EditorToolbar>
        </div>
      )}
      {showToolbar && (
        <PathBar
          fsId={fsId}
          path={path}
          host={host}
          open={crumbOpen}
          onOpenChange={setCrumbOpen}
          onOpenFile={(p) => openRemoteFile({ fsId: fs.id(), path: p, label: params.label ?? host, source: params.source, ownsFs: params.ownsFs, zoom: zoom || undefined })}
          onBrowse={browseFolder()}
          onReturnFocus={() => (hexRef.current ? hexRef.current.focus() : sessionRef.current?.focus())}
        />
      )}
      {phase.kind === 'ready' && !merge && (
        <EditorBanners
          name={name}
          dirty={dirty}
          banner={banner}
          note={note}
          lockedRO={lockedRO}
          backup={backups.recovered}
          canReconnect={!!params.source}
          onReload={() => void reload()}
          onCompare={() => void enterMerge(banner?.kind === 'conflict' ? 'conflict' : 'external')}
          onIgnore={() => {
            if (banner?.kind === 'changed') ignoredMtime.current = banner.mtime ?? null
            setBanner(null)
          }}
          onOverwrite={() => void save({ overwrite: true })}
          onSudo={canSudo ? () => void save({ sudo: true }) : undefined}
          onSaveAs={() => void saveAs()}
          onRecreate={() => void save({ overwrite: true })}
          onReconnect={() => void reconnect()}
          onDismissNote={() => setNote(null)}
          onRestore={restoreBackup}
          onDiscardBackup={() => {
            backups.drop()
            backups.dismiss()
          }}
          onClose={() => void closeTab(tabId)}
        />
      )}
      <div className="relative flex min-h-0 flex-1 flex-col" style={{ ['--nx-editor-font' as string]: safeFontFamily(settings.fontFamily) || undefined }}>
        {body}
        {/* Where the text editor's status bar will be (same box, so the overlay below covers the same area throughout). */}
        {opening && <StatusBar className="mt-auto">{statusLeft}</StatusBar>}
        <LoadingOverlay active={opening} label={`Opening ${name}…`} className="bg-panel" />
      </div>
    </div>
  )
}
