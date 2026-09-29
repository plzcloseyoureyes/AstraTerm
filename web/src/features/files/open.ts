/*
 * Opening file browsers in dock tabs (tab kind "files") and revealing folders in the SFTP side panel.
 */
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, Protocol, RuntimeSession } from '@/api/types'
import { showSidebarPanel } from '@/layout/Sidebar'
import { findTabs, focusTab, openTab, updateTabParams } from '@/stores/workspace'
import { getController } from './browser/controller'
import { patchView } from './browser/viewStore'
import type { FilesTabParams, FsContext, FsSource, PaneParams } from './types'

export const FILES_TAB = 'files'
export const SFTP_PANEL = 'sftp'

/** Tab / pane params describing a source. */
export function paramsForSource(source: FsSource, protocol?: Protocol): PaneParams {
  switch (source.kind) {
    case 'session':
      return { sessionId: source.sessionId, protocol: protocol ?? 'ssh' }
    case 'connection':
      return { connectionId: source.connectionId, protocol }
    case 'local':
      return { local: true }
    case 'quick':
      return { quick: source.quick, protocol: source.quick.protocol ?? protocol }
    case 'handle':
      return { fsId: source.fsId, protocol }
  }
}

/** Source described by tab / pane params (null when the params name none). */
export function sourceFromParams(p: PaneParams | undefined): FsSource | null {
  if (!p) return null
  if (p.sessionId) return { kind: 'session', sessionId: p.sessionId }
  if (p.connectionId) return { kind: 'connection', connectionId: p.connectionId }
  if (p.quick) return { kind: 'quick', quick: p.quick }
  if (p.local) return { kind: 'local' }
  if (p.fsId) return { kind: 'handle', fsId: p.fsId }
  return null
}

function sameSource(p: PaneParams | undefined, s: FsSource): boolean {
  const o = sourceFromParams(p)
  if (!o || o.kind !== s.kind) return false
  switch (s.kind) {
    case 'session':
      return o.kind === 'session' && o.sessionId === s.sessionId
    case 'connection':
      return o.kind === 'connection' && o.connectionId === s.connectionId
    case 'local':
      return true
    case 'handle':
      return o.kind === 'handle' && o.fsId === s.fsId
    default:
      return false
  }
}

/**
 * Open a files tab for a source (focusing an existing single-pane tab of the same source and navigating it to
 * `path`). Resolves the tab id.
 */
export function openFilesTab(source: FsSource, opts: { path?: string; protocol?: Protocol; title?: string; dual?: boolean; position?: 'tab' | 'right' | 'below' } = {}): string {
  const existing = !opts.dual ? findTabs((t) => t.kind === FILES_TAB && !(t.params as FilesTabParams)?.dual && sameSource(t.params as FilesTabParams, source))[0] : undefined
  if (existing) {
    focusTab(existing.id)
    if (opts.path) {
      const c = getController(`tab:${existing.id}:left`)
      if (c) c.navigate(opts.path)
      else updateTabParams<FilesTabParams>(existing.id, { path: opts.path })
    }
    return existing.id
  }
  const params: FilesTabParams = { ...paramsForSource(source, opts.protocol), path: opts.path, dual: opts.dual || undefined }
  return openTab<FilesTabParams>({ kind: FILES_TAB, params, title: opts.title, position: opts.position })
}

/** Files tab at a folder of the same place as a view (context menu "Open in new tab"). */
export function openFolderInTab(ctx: FsContext, path: string): string {
  return openFilesTab(ctx.source, { path, protocol: protocolOf(ctx.source) })
}

function protocolOf(source: FsSource): Protocol | undefined {
  if (source.kind === 'session') {
    return queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((s) => s.id === source.sessionId)?.protocol ?? 'ssh'
  }
  if (source.kind === 'connection') {
    return queryClient.getQueryData<Connection[]>(queryKeys.connections)?.find((c) => c.id === source.connectionId)?.protocol
  }
  if (source.kind === 'quick') return source.quick.protocol
  return undefined
}

/**
 * Show the SFTP side panel for a session and navigate it to `path`: focuses the session's terminal tab (the panel
 * follows the active terminal) and remembers the folder for the panel view.
 */
export function revealInPanel(sessionId: string, path?: string): void {
  const tab = findTabs((t) => t.kind === 'terminal' && (t.params as { sessionId?: string })?.sessionId === sessionId)[0]
  if (!tab) {
    toast.info('Open the session in a terminal tab to browse it in the side panel')
    return
  }
  focusTab(tab.id)
  if (path) {
    const viewId = `panel:${sessionId}`
    const c = getController(viewId)
    if (c) c.navigate(path)
    else patchView(viewId, { pending: path, pendingMode: 'push', pendingSelect: null })
  }
  showSidebarPanel(SFTP_PANEL)
}
