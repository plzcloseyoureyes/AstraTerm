/*
 * Tab kind "text": scratch documents (editor.new / editor.openText) and files from this computer (editor.openLocal).
 * Content lives in the editor's IndexedDB document store (survives reloads, reopen-closed-tab). Local files opened
 * through the File System Access API are saved back in place; everything else goes through "Save as…" (a remote
 * path, a download, or a local file).
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { FileText, HardDrive, Laptop } from 'lucide-react'
import { toast } from 'sonner'
import { getKeybindings, runCommand } from '@/app/commands'
import type { TabProps } from '@/app/registry'
import { formatKeybinding } from '@/lib/keys'
import { confirm } from '@/components/ui/dialog-host'
import { LoadingPane } from '@/components/ui/spinner'
import { useLatest } from '@/lib/hooks'
import { DELAY_PRESETS, useDelayedFlag, useLoadingGate } from '@/lib/useDelayedFlag'
import { errorMessage, formatRelativeTime, uid } from '@/lib/utils'
import { closeTab, setTabTitle, updateTabParams, useWorkspaceStore } from '@/stores/workspace'
import { isHandleGone, isNotFound, rememberHandle, statFile, writeFile, downloadBlob } from './api'
import { baseName, bytesToBase64, decodeBytes, detectEol, sniffBytes, type Eol } from './codec'
import { CodeEditor, useSessionStatus, VimStatus } from './CodeEditor'
import type { EditorSession } from './monaco/session'
import { applyDirtyTitle, registerController, setTabDirty, type ToggleName } from './controllers'
import { saveAsDialog, type LocalFileHandle } from './dialogs'
import { getDoc, putDoc } from './docstore'
import { HexEditor, type HexEditorHandle, type HexSnapshot } from './HexEditor'
import { openRemoteFile } from './open'
import { ensureWritable, sameMeta, textBytes, writeBody, writeLocalHandle, type TextMeta } from './save'
import { editorSettings, effectiveFontSize, FONT_MAX, FONT_MIN } from './settings'
import { safeFontFamily } from './monaco/palette'
import { EditorToolbar } from './Toolbar'
import type { TextTabParams } from './types'
import { TextStatusItems } from './StatusItems'
import { Banner, StatusBar, StatusSpacer, StatusText } from './ui'

type Phase = { kind: 'loading' } | { kind: 'ready'; text: string; bytes?: Uint8Array; version: number; missing?: boolean }

interface StoredMeta extends Partial<TextMeta> {
  name?: string
  dirty?: boolean
  lastModified?: number
  savedAt?: number
}

export default function TextTab({ tabId, params }: TabProps<TextTabParams>) {
  const settings = editorSettings.use()
  const title = params.title || 'Untitled'
  const isLocal = !!params.local
  const hex = params.mode === 'hex'
  const zoom = params.zoom ?? 0
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [docId, setDocId] = useState<string | undefined>(params.docId)
  const [session, setSession] = useState<EditorSession | null>(null)
  const status = useSessionStatus(session)
  const [meta, setMeta] = useState<TextMeta>({ encoding: 'utf-8', bom: false, eol: settings.defaultEol, mixedEol: false })
  const [savedMeta, setSavedMeta] = useState<TextMeta>(meta)
  const [baseDirty, setBaseDirty] = useState(false)
  const [hexDirty, setHexDirty] = useState(false)
  const [handle, setHandle] = useState<LocalFileHandle | null>(null)
  const [saving, setSaving] = useState(false)
  const [readOnly, setReadOnly] = useState(false)
  const [savedAt, setSavedAt] = useState<number | null>(null)
  const sessionRef = useRef<EditorSession | null>(null)
  const hexRef = useRef<HexEditorHandle>(null)
  const persistTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const versionRef = useRef(0)
  /** Save in progress (a second Ctrl+S or the close guard waits for it). */
  const inflight = useRef<Promise<boolean> | null>(null)
  /** Latest hex buffer (the hex editor is gone by the time unmount cleanups run). */
  const bytesRef = useRef<Uint8Array | null>(null)

  const metaDirty = !hex && !sameMeta(meta, savedMeta)
  const isActiveTab = () => useWorkspaceStore.getState().activeTabId === tabId
  const saveKey = formatKeybinding(getKeybindings('editor.save')[0] ?? '$mod+s')
  const dirty = phase.kind === 'ready' && (hex ? hexDirty || baseDirty : baseDirty || !!status?.docDirty || metaDirty)
  const live = useLatest({ dirty, meta, savedMeta, baseDirty, handle, docId, title, readOnly, zoom, hex, phase, params, isLocal })

  /** Unsaved changes right now (the rendered `dirty` lags one frame behind typing). */
  const dirtyNow = (): boolean => {
    const L = live.current
    if (L.phase.kind !== 'ready') return false
    if (L.hex) return L.baseDirty || (hexRef.current?.isDirty() ?? false)
    const s = sessionRef.current
    return L.baseDirty || (!!s && s.isDocDirty()) || !sameMeta(L.meta, L.savedMeta)
  }

  // ---------------------------------------------------------------------------------------------------------------
  // load

  useEffect(() => {
    let cancelled = false
    void (async () => {
      let id = params.docId
      if (!id) {
        id = uid('doc')
        await putDoc({ id, content: params.content ?? '' })
        updateTabParams<TextTabParams>(tabId, { docId: id, content: undefined })
      } else if (params.content !== undefined) {
        updateTabParams<TextTabParams>(tabId, { content: undefined })
      }
      const rec = await getDoc(id)
      if (cancelled) return
      setDocId(id)
      const m = (rec?.meta ?? {}) as StoredMeta
      if (rec?.handle) setHandle(rec.handle as LocalFileHandle)
      setBaseDirty(!!m.dirty)
      if (hex) {
        setPhase({ kind: 'ready', text: '', bytes: rec?.bytes ?? new Uint8Array(0), version: ++versionRef.current, missing: !rec })
        return
      }
      const text = rec?.content ?? params.content ?? ''
      const e = detectEol(text, editorSettings.get().defaultEol)
      const loaded: TextMeta = { encoding: m.encoding ?? 'utf-8', bom: !!m.bom, eol: (m.eol as Eol) ?? e.eol, mixedEol: e.mixed }
      setMeta(loaded)
      setSavedMeta(loaded)
      setPhase({ kind: 'ready', text, version: ++versionRef.current, missing: !rec && !params.content })
    })()
    return () => {
      cancelled = true
    }
    // Loaded once per tab; the doc id never changes afterwards.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // ---------------------------------------------------------------------------------------------------------------
  // persistence (working copy in IndexedDB)

  const persist = useCallback(
    (extra: Partial<StoredMeta> = {}) => {
      const L = live.current
      if (!L.docId) return
      const s = sessionRef.current
      const common = { id: L.docId, handle: L.handle ?? undefined }
      const m: StoredMeta = { ...L.meta, name: L.title, dirty: dirtyNow(), ...extra }
      if (L.hex) {
        const bytes = hexRef.current?.getBytes() ?? bytesRef.current
        if (bytes) void putDoc({ ...common, bytes, meta: { ...m } })
      } else if (s) {
        // With its line endings (mixed ones included), so a reload restores them.
        void putDoc({ ...common, content: s.textForSave(L.meta), meta: { ...m } })
      }
    },
    [live],
  )

  const schedulePersist = () => {
    if (persistTimer.current) clearTimeout(persistTimer.current)
    persistTimer.current = setTimeout(() => persist(), 600)
  }

  useEffect(
    () => () => {
      if (persistTimer.current) {
        clearTimeout(persistTimer.current)
        persist()
      }
      setTabDirty(tabId, false)
    },
    [persist, tabId],
  )

  useEffect(() => {
    setTabDirty(tabId, dirty)
    applyDirtyTitle(tabId, dirty)
  }, [tabId, dirty])

  // ---------------------------------------------------------------------------------------------------------------
  // saving

  const currentBytes = (m: TextMeta, hexSnap?: HexSnapshot): Uint8Array | null => {
    if (live.current.hex) return hexSnap?.bytes ?? hexRef.current?.getBytes() ?? null
    const s = sessionRef.current
    return s ? textBytes(s.textForSave(m), m) : null
  }

  /** What a save is about to write (edits made while it runs stay unsaved). */
  const takeSnapshot = () => ({ doc: sessionRef.current?.snapshot(), hex: live.current.hex ? hexRef.current?.snapshot() : undefined })

  const markClean = (m: TextMeta, snap: ReturnType<typeof takeSnapshot>) => {
    if (snap.doc) sessionRef.current?.markSaved(snap.doc)
    if (snap.hex) hexRef.current?.markSaved(snap.hex)
    setSavedMeta(m)
    setBaseDirty(false)
    setSavedAt(Date.now())
    // persist the clean state
    setTimeout(() => persist({ dirty: false, savedAt: Date.now() }), 0)
  }

  const saveAs = async (): Promise<boolean> => {
    const L = live.current
    const r = await saveAsDialog({ suggestedName: L.title.replace(/^● /, ''), allowLocal: true })
    if (!r) return false
    const snap = takeSnapshot()
    let m = L.meta
    let bytes: Uint8Array | null
    try {
      bytes = currentBytes(m, snap.hex)
    } catch (err) {
      toast.error('Cannot encode the document', { description: errorMessage(err) })
      m = { ...m, encoding: 'utf-8', bom: false }
      bytes = currentBytes(m, snap.hex)
    }
    if (!bytes) return false
    if (r.target === 'remote') {
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
      setSaving(true)
      try {
        const s = sessionRef.current
        const body = !L.hex && s ? writeBody(s.textForSave(m), m) : { content: bytesToBase64(bytes), encoding: 'base64' as const }
        await writeFile(r.fsId, { path: r.path, content: body.content, encoding: body.encoding })
      } catch (err) {
        toast.error(`Could not save ${baseName(r.path)}`, { description: errorMessage(err) })
        return false
      } finally {
        setSaving(false)
      }
      if (r.label) rememberHandle({ id: r.fsId, label: r.label })
      markClean(m, snap)
      openRemoteFile({ fsId: r.fsId, path: r.path, label: r.label, source: r.source, ownsFs: r.ownsFs, mode: L.hex ? 'hex' : undefined })
      toast.success(`Saved as ${baseName(r.path)}`)
      await closeTab(tabId, { force: true })
      return true
    }
    try {
      if (r.target === 'download') {
        downloadBlob(bytes as unknown as BlobPart, r.filename)
        markClean(m, snap)
        return true
      }
      await writeLocalHandle(r.handle, bytes)
      setHandle(r.handle)
      updateTabParams<TextTabParams>(tabId, { title: r.handle.name, local: { name: r.handle.name } })
      setTabTitle(tabId, r.handle.name)
      markClean(m, snap)
      toast.success(`Saved ${r.handle.name}`)
      return true
    } catch (err) {
      toast.error('Could not save the file', { description: errorMessage(err) })
      return false
    }
  }

  const save = (): Promise<boolean> => {
    // One save at a time; a save requested meanwhile runs afterwards (it may have more to write).
    if (inflight.current) return inflight.current.then((ok) => (ok && dirtyNow() ? save() : ok))
    const p = saveOnce().finally(() => {
      inflight.current = null
    })
    inflight.current = p
    return p
  }

  const saveOnce = async (): Promise<boolean> => {
    const L = live.current
    if (L.phase.kind !== 'ready') return false
    const h = L.handle
    if (!h) return saveAs()
    if (!dirtyNow()) return true
    setSaving(true)
    try {
      if (!(await ensureWritable(h))) {
        toast.error(`No permission to write ${h.name}`, { description: 'Use Save as… to save it somewhere else.' })
        return false
      }
      let m = L.meta
      let snap = takeSnapshot()
      let bytes: Uint8Array | null
      try {
        bytes = currentBytes(m, snap.hex)
      } catch (err) {
        const ok = await confirm({ title: 'Save as UTF-8?', description: errorMessage(err), confirmLabel: 'Save as UTF-8' })
        if (!ok) return false
        m = { ...m, encoding: 'utf-8', bom: false }
        setMeta(m)
        snap = takeSnapshot()
        bytes = currentBytes(m, snap.hex)
      }
      if (!bytes) return false
      await writeLocalHandle(h, bytes)
      markClean(m, snap)
      return true
    } catch (err) {
      toast.error(`Could not save ${h.name}`, { description: errorMessage(err) })
      return false
    } finally {
      setSaving(false)
    }
  }

  const reloadFromDisk = async () => {
    const h = live.current.handle
    if (!h) return
    if (live.current.dirty) {
      const ok = await confirm({ title: `Reload ${h.name}?`, description: 'Your unsaved changes will be lost.', confirmLabel: 'Discard & reload', destructive: true })
      if (!ok) return
    }
    try {
      const file = await h.getFile()
      const bytes = new Uint8Array(await file.arrayBuffer())
      if (live.current.hex) {
        setPhase({ kind: 'ready', text: '', bytes, version: ++versionRef.current })
        setHexDirty(false)
        setBaseDirty(false)
        return
      }
      const sn = sniffBytes(bytes)
      // Keep a legacy encoding the user chose; otherwise use what the file looks like now (and save back with it).
      const encoding = live.current.meta.encoding !== 'utf-8' ? live.current.meta.encoding : sn.kind === 'text' ? sn.encoding : 'windows-1252'
      const d = decodeBytes(bytes, encoding)
      const e = detectEol(d.text, editorSettings.get().defaultEol)
      const m: TextMeta = { encoding, bom: d.bom, eol: e.eol, mixedEol: e.mixed }
      sessionRef.current?.setText(d.text, { reset: true, markSaved: true, eol: e.eol })
      setMeta(m)
      setSavedMeta(m)
      setBaseDirty(false)
      setTimeout(() => persist({ dirty: false }), 0)
    } catch (err) {
      toast.error(`Could not read ${h.name}`, { description: errorMessage(err) })
    }
  }

  const toggle = (what: ToggleName) => {
    const s = sessionRef.current
    if (what === 'readOnly') return setReadOnly((v) => !v)
    if (what === 'vim') return editorSettings.set({ vimMode: !editorSettings.get().vimMode })
    if (!s) return
    const st = s.getStatus()
    if (what === 'wrap') {
      s.setWrap(!st.wrap)
      updateTabParams<TextTabParams>(tabId, { wrap: !st.wrap })
    } else if (what === 'whitespace') {
      s.setWhitespace(!st.whitespace)
      updateTabParams<TextTabParams>(tabId, { whitespace: !st.whitespace })
    } else if (what === 'minimap') {
      if (st.large) {
        toast.info('The minimap is off for large files')
        return
      }
      s.setMinimap(!st.minimap)
      updateTabParams<TextTabParams>(tabId, { minimap: !st.minimap })
    }
  }

  const setZoom = (delta: number) => {
    const base = editorSettings.get().fontSize
    const next = delta === 0 ? 0 : Math.min(FONT_MAX - base, Math.max(FONT_MIN - base, live.current.zoom + delta))
    updateTabParams<TextTabParams>(tabId, { zoom: next || undefined })
  }

  const actions = useLatest({ save, saveAs, toggle, setZoom, reloadFromDisk, dirtyNow })

  useEffect(
    () =>
      registerController({
        tabId,
        kind: 'text',
        isDirty: () => actions.current.dirtyNow(),
        save: () => actions.current.save(),
        saveAs: () => actions.current.saveAs(),
        find: (replace) => (hexRef.current ? hexRef.current.find() : sessionRef.current?.openSearch(replace)),
        gotoLine: () => (hexRef.current ? hexRef.current.gotoOffset() : sessionRef.current?.gotoLine()),
        zoom: (d) => actions.current.setZoom(d),
        toggle: (w) => actions.current.toggle(w),
        reload: live.current.handle ? () => void actions.current.reloadFromDisk() : undefined,
        revealLine: (line) => sessionRef.current?.revealLine(line),
        format: () => (hexRef.current ? undefined : sessionRef.current?.formatDocument()),
        editorCommands: () => (hexRef.current ? undefined : sessionRef.current?.commandPalette()),
        focus: () => (hexRef.current ? hexRef.current.focus() : sessionRef.current?.focus()),
        getText: () => sessionRef.current?.getText() ?? '',
        printable: () => {
          const s = sessionRef.current
          const L = live.current
          return s && !L.hex ? s.printable(L.title.replace(/^● /, '')) : null
        },
      }),
    // re-register when a file handle appears (enables "Reload from disk")
    [actions, handle, live, tabId],
  )

  useEffect(() => {
    if (!session) return
    if (params.wrap !== undefined) session.setWrap(!!params.wrap)
    if (params.whitespace !== undefined) session.setWhitespace(!!params.whitespace)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session])

  // ---------------------------------------------------------------------------------------------------------------
  // render

  const loadGate = useLoadingGate(phase.kind === 'loading', DELAY_PRESETS.NAVIGATION)
  // "Saving…" only for a save that takes a while (a quick one goes straight to "Saved").
  const savingShown = useDelayedFlag(saving)

  // Blank for a second, then a pane spinner that stays ≥ 0.8 s even when the document arrives meanwhile.
  if (loadGate.hold || phase.kind === 'loading') return <LoadingPane active={loadGate.show} immediate label={`Opening ${title}…`} className="bg-panel" />

  const fontSize = effectiveFontSize(settings, zoom)
  const where = isLocal ? (handle ? `This computer · ${handle.name}` : `This computer · ${params.local?.name ?? title} (copy)`) : 'Scratch document'
  const stateText = savingShown ? 'Saving…' : dirty ? (isLocal && handle ? 'Unsaved changes' : 'Not saved to a file') : savedAt ? `Saved ${formatRelativeTime(savedAt)}` : ''
  const statusLeft = (
    <>
      <StatusText title={where} className="min-w-0 shrink overflow-hidden">
        {isLocal ? <Laptop className="size-3 shrink-0" aria-hidden /> : <FileText className="size-3 shrink-0" aria-hidden />}
        <span className="truncate">{where}</span>
      </StatusText>
      {stateText && (
        <StatusText className={dirty ? 'text-warning' : undefined} aria-live="polite">
          {stateText}
        </StatusText>
      )}
    </>
  )

  return (
    <div className="flex h-full min-h-0 flex-col" style={{ ['--nx-editor-font' as string]: safeFontFamily(settings.fontFamily) || undefined }}>
      <EditorToolbar
        mode={hex ? 'hex' : 'text'}
        dirty={dirty}
        saving={saving}
        canSave
        onSave={() => void save()}
        onSaveAs={() => void saveAs()}
        canUndo={hex ? true : !!status?.canUndo}
        canRedo={hex ? true : !!status?.canRedo}
        onUndo={() => (hex ? hexRef.current?.undo() : session?.undo())}
        onRedo={() => (hex ? hexRef.current?.redo() : session?.redo())}
        onFind={() => (hex ? hexRef.current?.find() : session?.openSearch(false))}
        onReplace={hex ? undefined : () => session?.openSearch(true)}
        onGoto={() => (hex ? hexRef.current?.gotoOffset() : session?.gotoLine())}
        wrap={status?.wrap}
        whitespace={status?.whitespace}
        minimap={status ? status.minimap : undefined}
        vim={settings.vimMode}
        onFormat={hex || !session?.canFormat() ? undefined : () => session.formatDocument()}
        onCommandPalette={hex ? undefined : () => session?.commandPalette()}
        onToggle={hex ? undefined : (w) => toggle(w)}
        onZoom={setZoom}
        readOnly={readOnly}
        onToggleReadOnly={() => toggle('readOnly')}
        onReload={handle ? () => void reloadFromDisk() : undefined}
        onExportHtml={hex ? undefined : () => void runCommand('editor.exportHtml', { tabId })}
        onPrint={hex ? undefined : () => void runCommand('editor.print', { tabId })}
      />
      {phase.missing && (
        <Banner tone="warning">The stored content of this document is no longer available in this browser. It starts empty.</Banner>
      )}
      {isLocal && !handle && !phase.missing && (
        <Banner tone="info" icon={HardDrive}>
          This is a copy of {params.local?.name}: this browser cannot write local files directly. Use Save as to download or upload it.
        </Banner>
      )}
      <div className="flex min-h-0 flex-1 flex-col">
        {hex ? (
          <HexEditor
            key={phase.version}
            ref={hexRef}
            data={phase.bytes ?? new Uint8Array(0)}
            readOnly={readOnly}
            fontSize={fontSize}
            onDirtyChange={(d) => {
              setHexDirty(d)
              bytesRef.current = hexRef.current?.getBytes() ?? bytesRef.current
              schedulePersist()
            }}
            statusLeft={statusLeft}
            label={title}
            autoFocus={isActiveTab()}
          />
        ) : (
          <>
            <CodeEditor
              key={phase.version}
              tabId={tabId}
              initialText={phase.text}
              eol={meta.eol}
              path={title}
              language={params.language}
              readOnly={readOnly}
              zoom={zoom}
              minimap={params.minimap}
              placeholder={isLocal ? undefined : `Start typing… (${saveKey} saves to a server, this computer or a download)`}
              autoFocus={isActiveTab()}
              onReady={(s) => {
                if (!s && persistTimer.current) {
                  // unmounting: flush the working copy while the old session is still reachable
                  clearTimeout(persistTimer.current)
                  persistTimer.current = null
                  persist()
                }
                sessionRef.current = s
                setSession(s)
              }}
              onDocChange={schedulePersist}
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
                  onSaveWith={(enc, bom) => {
                    setMeta((m) => ({ ...m, encoding: enc, bom }))
                    schedulePersist()
                  }}
                  onEol={(eol) => {
                    setMeta((m) => ({ ...m, eol, mixedEol: false }))
                    schedulePersist()
                  }}
                  onLanguage={(n) => {
                    void session?.setLanguageByName(n)
                    updateTabParams<TextTabParams>(tabId, { language: n })
                  }}
                />
              )}
            </StatusBar>
          </>
        )}
      </div>
    </div>
  )
}
