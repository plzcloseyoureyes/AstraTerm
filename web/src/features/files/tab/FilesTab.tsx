/*
 * Tab kind "files": the file browser in a dock tab — for a saved sftp / ftp / s3 connection (opened by the terminal
 * feature as {connectionId, protocol} or {quick, protocol}), an SSH session, the local host or a handle — with an
 * optional dual-pane commander (FILE-13: each pane on any file system, F5 copy, F6 move, F7 mkdir, F8 delete,
 * Tab switches panes) and folder compare (FILE-15).
 */
import { useCallback, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowLeftRight, ArrowRightLeft, ArrowUpDown, CircleAlert, Columns2, FolderGit2, PanelLeft, RefreshCw, Unplug, X } from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Kbd } from '@/components/ui/kbd'
import { ResizeHandle } from '@/components/ui/resizable'
import { cn, isEditableTarget } from '@/lib/utils'
import { closeTab, updateTabParams } from '@/stores/workspace'
import { Delayed, Spinner } from '@/components/ui/spinner'
import { compareFolders } from '../actions'
import { FileBrowser } from '../browser/FileBrowser'
import type { BrowserController } from '../browser/controller'
import { openTransfers } from '../transfers/store'
import { paramsForSource, sourceFromParams } from '../open'
import { useFsContext } from '../place'
import { SourcePicker } from './SourcePicker'
import type { FilesTabParams, FsSource } from '../types'

export default function FilesTab({ tabId, params }: TabProps<FilesTabParams>) {
  const p = params ?? {}
  const leftSource = useStableSource(sourceFromParams(p) ?? { kind: 'local' })
  if (!p.dual) {
    return (
      <Pane
        tabId={tabId}
        side="left"
        source={leftSource}
        initialPath={p.path}
        variant="tab"
        onPath={(path) => updateTabParams<FilesTabParams>(tabId, { path })}
        trailing={
          <>
            <IconButton icon={Columns2} label="Dual-pane commander" size="xs" onClick={() => updateTabParams<FilesTabParams>(tabId, { dual: true })} />
            <IconButton icon={ArrowUpDown} label="Transfers" size="xs" onClick={openTransfers} />
          </>
        }
      />
    )
  }
  return <Commander tabId={tabId} params={p} leftSource={leftSource} />
}

/** Keep a source object stable while its identity (JSON) is unchanged. */
function useStableSource(src: FsSource): FsSource {
  const json = JSON.stringify(src)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => src, [json])
}

// ---------------------------------------------------------------------------------------------------------------------
// pane
// ---------------------------------------------------------------------------------------------------------------------

function Pane({
  tabId,
  side,
  source,
  initialPath,
  variant,
  onPath,
  trailing,
  header,
  peer,
  active,
  onActivate,
  controllerRef,
}: {
  tabId: string
  side: 'left' | 'right'
  source: FsSource
  initialPath?: string
  variant: 'tab' | 'pane'
  onPath: (path: string) => void
  trailing?: ReactNode
  header?: ReactNode
  peer?: () => BrowserController | undefined
  active?: boolean
  onActivate?: () => void
  controllerRef?: (c: BrowserController) => void
}) {
  const { fs, ctx, place } = useFsContext(source)
  const viewId = `tab:${tabId}:${side}`
  if (ctx) {
    return (
      <FileBrowser
        key={ctx.key}
        viewId={viewId}
        ctx={ctx}
        variant={variant}
        initialPath={initialPath || ctx.handle.home}
        onPathChange={onPath}
        toolbarTrailing={trailing}
        header={header}
        peer={peer}
        active={active}
        onActivate={onActivate}
        controllerRef={controllerRef}
      />
    )
  }
  const label = place?.label ?? 'files'
  return (
    <div className="@container flex h-full min-h-0 flex-col">
      {header}
      <div className="flex min-h-0 flex-1 items-center justify-center">
        {fs.status === 'error' && source.kind === 'session' && (fs.error?.status === 404 || fs.error?.status === 409) ? (
          <EmptyState
            size="sm"
            icon={Unplug}
            title={fs.error?.status === 404 ? 'Session ended' : 'Session not connected'}
            description={fs.error?.status === 404 ? 'The SSH session this tab was browsing no longer exists.' : fs.error?.message}
            action={
              <>
                {fs.error?.status === 409 && (
                  <Button size="xs" variant="secondary" onClick={fs.retry}>
                    <RefreshCw /> Retry
                  </Button>
                )}
                <Button size="xs" variant="secondary" onClick={() => void closeTab(tabId)}>
                  <X /> Close tab
                </Button>
              </>
            }
          />
        ) : fs.status === 'error' ? (
          <EmptyState
            size="sm"
            icon={CircleAlert}
            title={`Could not open ${label}`}
            description={fs.error?.message}
            action={
              <Button size="xs" variant="secondary" onClick={fs.retry}>
                <RefreshCw /> Retry
              </Button>
            }
          />
        ) : (
          <Delayed>
            <div className="flex flex-col items-center gap-2 text-sm text-muted-foreground animate-in fade-in-0 duration-200" role="status">
              <Spinner className="size-5" label="Connecting" />
              <span>Connecting to {label}…</span>
              <span className="text-xs">Host keys and passwords are asked for when needed.</span>
            </div>
          </Delayed>
        )}
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// commander
// ---------------------------------------------------------------------------------------------------------------------

function Commander({ tabId, params, leftSource }: { tabId: string; params: FilesTabParams; leftSource: FsSource }) {
  const rightSource = useStableSource(sourceFromParams(params.right) ?? { kind: 'local' })
  const [activePane, setActivePane] = useState<'left' | 'right'>(params.activePane ?? 'left')
  const left = useRef<BrowserController | null>(null)
  const right = useRef<BrowserController | null>(null)
  const rootRef = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  const [split, setSplit] = useState<number | null>(null)

  useLayoutEffect(() => {
    const el = rootRef.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(el.clientWidth))
    ro.observe(el)
    setWidth(el.clientWidth)
    return () => ro.disconnect()
  }, [])

  const activate = useCallback(
    (side: 'left' | 'right') => {
      setActivePane(side)
      updateTabParams<FilesTabParams>(tabId, { activePane: side })
    },
    [tabId],
  )
  const current = () => (activePane === 'left' ? left.current : right.current)

  const setLeftSource = (src: FsSource) => {
    const pp = paramsForSource(src)
    updateTabParams<FilesTabParams>(tabId, {
      fsId: pp.fsId,
      connectionId: pp.connectionId,
      sessionId: pp.sessionId,
      local: pp.local,
      quick: pp.quick,
      protocol: pp.protocol,
      path: undefined,
    })
  }
  const setRightSource = (src: FsSource) => updateTabParams<FilesTabParams>(tabId, { right: { ...paramsForSource(src), path: undefined } })

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (isEditableTarget(e.target)) return
    const c = current()
    if (!c) return
    const handled = () => {
      e.preventDefault()
      e.stopPropagation()
    }
    switch (e.key) {
      case 'F5':
        handled()
        void c.toPeer('copy')
        return
      case 'F6':
        handled()
        void c.toPeer('move')
        return
      case 'F7':
        handled()
        c.newFolder()
        return
      case 'F8':
        handled()
        void c.deleteSelection()
        return
      case 'Tab':
        if (e.ctrlKey || e.altKey || e.metaKey) return
        handled()
        {
          const next = activePane === 'left' ? 'right' : 'left'
          activate(next)
          ;(next === 'left' ? left.current : right.current)?.focusList()
        }
        return
    }
  }

  const leftWidth = split ?? Math.round(width / 2)
  const header = (side: 'left' | 'right', source: FsSource, onPick: (s: FsSource) => void) => (
    <div
      className={cn(
        'flex h-8 shrink-0 items-center gap-1 border-b px-1.5',
        activePane === side ? 'bg-primary/8 shadow-[inset_0_-2px_0_var(--primary)]' : 'bg-transparent',
      )}
    >
      <SourcePicker source={source} onPick={onPick} />
      <span className="flex-1" />
      <IconButton
        icon={side === 'left' ? ArrowRightLeft : ArrowLeftRight}
        label={side === 'left' ? 'Show this folder in the right pane' : 'Show this folder in the left pane'}
        size="xs"
        onClick={() => {
          const from = side === 'left' ? left.current : right.current
          const to = side === 'left' ? right.current : left.current
          if (!from?.dir) return
          if (side === 'left') updateTabParams<FilesTabParams>(tabId, { right: { ...paramsForSource(source), path: from.dir } })
          else setLeftSourceWithPath(source, from.dir)
          to?.navigate(from.dir)
        }}
      />
    </div>
  )
  const setLeftSourceWithPath = (src: FsSource, path: string) => {
    const pp = paramsForSource(src)
    updateTabParams<FilesTabParams>(tabId, { fsId: pp.fsId, connectionId: pp.connectionId, sessionId: pp.sessionId, local: pp.local, quick: pp.quick, protocol: pp.protocol, path })
  }

  const doCompare = () => {
    const l = left.current
    const r = right.current
    if (!l?.ctx || !l.dir) return
    compareFolders({ ctx: l.ctx, path: l.dir }, r?.ctx && r.dir ? { ctx: r.ctx, path: r.dir } : undefined)
  }

  return (
    <div className="@container flex h-full min-h-0 flex-col" onKeyDown={onKeyDown}>
      <div className="flex h-8 shrink-0 items-center gap-0.5 border-b bg-toolbar px-1.5">
        <IconButton icon={PanelLeft} label="Single pane" size="xs" onClick={() => updateTabParams<FilesTabParams>(tabId, { dual: undefined })} />
        <span className="mx-1 h-4 w-px bg-border" aria-hidden />
        <Button variant="ghost" size="xs" onClick={() => void current()?.toPeer('copy')} title="Copy the selection to the other pane (F5)">
          F5 Copy
        </Button>
        <Button variant="ghost" size="xs" onClick={() => void current()?.toPeer('move')} title="Move the selection to the other pane (F6)">
          F6 Move
        </Button>
        <Button variant="ghost" size="xs" onClick={() => current()?.newFolder()} title="New folder (F7)" className="hidden @lg:inline-flex">
          F7 New folder
        </Button>
        <Button variant="ghost" size="xs" onClick={() => void current()?.deleteSelection()} title="Delete (F8)" className="hidden @lg:inline-flex">
          F8 Delete
        </Button>
        <span className="flex-1" />
        <Button variant="ghost" size="xs" onClick={doCompare} title="Compare the two folders">
          <FolderGit2 /> <span className="hidden @md:inline">Compare folders</span>
        </Button>
        <IconButton icon={ArrowUpDown} label="Transfers" size="xs" onClick={openTransfers} />
      </div>
      <div ref={rootRef} className="flex min-h-0 flex-1">
        <div className="flex min-h-0 min-w-0 flex-col" style={{ width: width ? leftWidth : '50%' }} onMouseDownCapture={() => activate('left')}>
          <Pane
            tabId={tabId}
            side="left"
            source={leftSource}
            initialPath={params.path}
            variant="pane"
            onPath={(path) => updateTabParams<FilesTabParams>(tabId, { path })}
            header={header('left', leftSource, setLeftSource)}
            peer={() => right.current ?? undefined}
            active={activePane === 'left'}
            onActivate={() => activate('left')}
            controllerRef={(c) => {
              left.current = c
            }}
          />
        </div>
        <ResizeHandle
          edge="right"
          size={leftWidth}
          min={220}
          max={Math.max(260, width - 220)}
          label="Resize panes"
          onResize={setSplit}
          onToggle={() => setSplit(null)}
          className="w-1 shrink-0 border-x bg-border/40"
        />
        <div className="flex min-h-0 min-w-0 flex-1 flex-col" onMouseDownCapture={() => activate('right')}>
          <Pane
            tabId={tabId}
            side="right"
            source={rightSource}
            initialPath={params.right?.path}
            variant="pane"
            onPath={(path) => updateTabParams<FilesTabParams>(tabId, { right: { ...(params.right ?? { local: true }), path } })}
            header={header('right', rightSource, setRightSource)}
            peer={() => left.current ?? undefined}
            active={activePane === 'right'}
            onActivate={() => activate('right')}
            controllerRef={(c) => {
              right.current = c
            }}
          />
        </div>
      </div>
      <FunctionKeyBar />
    </div>
  )
}

function FunctionKeyBar() {
  const keys: [string, string][] = [
    ['F3', 'View'],
    ['F4', 'Edit'],
    ['F5', 'Copy'],
    ['F6', 'Move'],
    ['F7', 'New folder'],
    ['F8', 'Delete'],
    ['Tab', 'Switch pane'],
  ]
  return (
    <div className="flex h-6 shrink-0 items-center gap-3 overflow-hidden border-t px-2 text-xs text-muted-foreground" aria-label="Keyboard shortcuts">
      {keys.map(([k, v]) => (
        <span key={k} className="flex shrink-0 items-center gap-1">
          <Kbd>{k}</Kbd>
          {v}
        </span>
      ))}
    </div>
  )
}
