/*
 * 'web' tab: a proxied web page (or an Xpra application) in an iframe with a mini browser toolbar — back / forward /
 * reload / home, an editable address showing the upstream URL, zoom, open in a new window.
 *
 * The page runs on its own origin (host mode: p-<id>.localhost) or sandboxed under /proxy/ (path mode), so the tab
 * cannot read it directly: the backend injects a small bridge script that reports the location / title and executes
 * the toolbar's commands (postMessage, see internal/webproxy/pages.go). Entry URLs carry one-time tokens, so every
 * (re)load asks the API for a fresh one; an expired proxy is recreated transparently from the tab's spec.
 */
import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { toast } from 'sonner'
import {
  AlertTriangle,
  ArrowLeft,
  ArrowRight,
  AppWindow,
  Copy,
  ExternalLink,
  Globe,
  Home,
  Info,
  Lock,
  MoreHorizontal,
  Play,
  Power,
  RotateCw,
  ShieldAlert,
  X,
  ZoomIn,
  ZoomOut,
} from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { isApiError } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Spinner } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { DELAY_PRESETS, useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, copyText, errorMessage } from '@/lib/utils'
import { closeTab, setTabState, setTabTitle, updateTabParams } from '@/stores/workspace'
import { closeProxy, createProxy, onProxyEvent, proxyEntry, startXpra } from './api'
import { addressToString, displayUrl, errorCode, parseAddress, viaText } from './model'
import { registerWebView } from './views'
import type { BridgeMessage, ProxyInfo, ProxySpec, WebTabParams } from './types'

const ZOOM_STEPS = [0.5, 0.67, 0.75, 0.8, 0.9, 1, 1.1, 1.25, 1.5, 1.75, 2]

interface Entry {
  url: string
  mode: 'host' | 'path'
}

type Phase = 'init' | 'ready' | 'failed' | 'ended'

interface Banner {
  tone: 'info' | 'warning' | 'error'
  text: string
  actions?: { label: string; run: () => void; primary?: boolean }[]
}

function entryOf(info: ProxyInfo): Entry {
  return { url: info.url ?? '', mode: info.mode === 'path' ? 'path' : 'host' }
}

/** The spec's route (connection / session / tunnel) without the address. */
function routeOf(spec: ProxySpec | undefined): ProxySpec {
  const s: ProxySpec = {}
  if (spec?.tunnelId) s.tunnelId = spec.tunnelId
  else if (spec?.sessionId) s.sessionId = spec.sessionId
  else if (spec?.connectionId) s.connectionId = spec.connectionId
  if (spec?.insecureTls) s.insecureTls = true
  return s
}

export default function WebView({ tabId, params }: TabProps<WebTabParams>) {
  const isXpra = params.kind === 'xpra'
  const zoom = params.zoom || 1

  const iframeRef = useRef<HTMLIFrameElement>(null)
  const [phase, setPhase] = useState<Phase>('init')
  const [failure, setFailure] = useState<string>('')
  const [entry, setEntry] = useState<Entry | null>(null)
  const [loading, setLoading] = useState(true)
  const [bridge, setBridge] = useState(false)
  const [page, setPage] = useState<{ path?: string; title?: string }>({ path: params.path })
  const [pageError, setPageError] = useState<BridgeMessage['error'] | null>(null)
  const [banner, setBanner] = useState<Banner | null>(null)
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const authRetries = useRef(0)
  const recreated = useRef(0)
  const loadedOnce = useRef(false)
  const paramsRef = useRef(params)
  paramsRef.current = params
  const pathRef = useRef<string | undefined>(params.path)

  // One calm loading hint from opening the proxy through the page's load (no gap between the steps): the reload button
  // turns into a ring and the tab shows its indicator — only for a slow page (navigation timing). Failed / ended views
  // show their buttons' own spinners instead.
  const showProgress = useDelayedFlag(phase === 'init' || (phase === 'ready' && (loading || busy)), DELAY_PRESETS.NAVIGATION)

  // ---- proxy / entry lifecycle ----------------------------------------------------------------------------------

  const adopt = useCallback(
    (info: ProxyInfo, spec?: ProxySpec) => {
      const patch: Partial<WebTabParams> = { proxyId: info.id, target: info.target, via: info.via, url: undefined }
      if (spec) patch.spec = spec
      updateTabParams<WebTabParams>(tabId, patch)
      setEntry(entryOf(info))
      setPhase('ready')
      setLoading(true)
      setBridge(false)
      setPageError(null)
    },
    [tabId],
  )

  /** Recreate an expired proxy from the tab's spec (web pages) or report the ended application (Xpra). */
  const recreate = useCallback(async () => {
    const p = paramsRef.current
    if (p.kind === 'xpra') {
      setPhase('ended')
      return
    }
    if (!p.spec) {
      setFailure('This web page was closed and cannot be reopened automatically.')
      setPhase('failed')
      return
    }
    recreated.current++
    setBusy(true)
    try {
      const info = await createProxy({ ...p.spec, path: pathRef.current || p.spec.path, mode: p.pathMode ? 'path' : undefined })
      adopt(info)
    } catch (err) {
      setFailure(errorMessage(err))
      setPhase('failed')
    } finally {
      setBusy(false)
    }
  }, [adopt])

  /** Ask for a fresh entry URL (one-time token) for path. */
  const load = useCallback(
    async (path?: string, opts: { mode?: 'path' | 'auto' } = {}) => {
      const p = paramsRef.current
      setBusy(true)
      try {
        const info = await proxyEntry(p.proxyId, { path: path ?? pathRef.current, mode: opts.mode ?? (p.pathMode ? 'path' : undefined) })
        if (opts.mode === 'path' && !p.pathMode) updateTabParams<WebTabParams>(tabId, { pathMode: true })
        setEntry(entryOf(info))
        setPhase('ready')
        setLoading(true)
        setBridge(false)
        setPageError(null)
      } catch (err) {
        if (isApiError(err) && err.isNotFound) {
          await recreate()
        } else {
          setFailure(errorMessage(err))
          setPhase('failed')
        }
      } finally {
        setBusy(false)
      }
    },
    [recreate, tabId],
  )

  // First load: the tab's initial URL still carries an unused token; otherwise ask for a fresh one.
  useEffect(() => {
    const p = paramsRef.current
    if (p.url) {
      setEntry({ url: p.url, mode: p.pathMode ? 'path' : 'host' })
      setPhase('ready')
      updateTabParams<WebTabParams>(tabId, { url: undefined })
    } else {
      void load()
    }
    // proxyId changes are handled by adopt() (entry already set)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // The proxy was closed elsewhere (idle, session closed, application exited, another window).
  useEffect(
    () =>
      onProxyEvent((ev) => {
        if (ev.change !== 'closed' || ev.proxy.id !== paramsRef.current.proxyId) return
        if (paramsRef.current.kind === 'xpra') {
          setPhase('ended')
          setBanner(null)
          setFailure(ev.reason ?? '')
          return
        }
        setBanner({
          tone: 'info',
          text: `The web proxy was closed${ev.reason ? ` (${ev.reason})` : ''}. The page keeps showing its last state.`,
          actions: [{ label: 'Reconnect', primary: true, run: () => void recreate() }],
        })
      }),
    [recreate],
  )

  // ---- bridge messages ------------------------------------------------------------------------------------------

  const [ownerWin, setOwnerWin] = useState<Window | null>(null)
  useEffect(() => {
    const wins = new Set<Window>([window])
    if (ownerWin) wins.add(ownerWin)
    const onMessage = (e: MessageEvent) => {
      const frame = iframeRef.current
      if (!frame || e.source !== frame.contentWindow) return
      const d = e.data as BridgeMessage
      if (!d || d.source !== 'astraterm-webproxy') return
      switch (d.type) {
        case 'hello':
          setBridge(true)
          setBanner((b) => (b?.tone === 'info' && b.text.startsWith('No answer') ? null : b))
          break
        case 'location':
          setBridge(true)
          setLoading(false)
          setPageError(null)
          setPage({ path: d.path, title: d.title })
          if (d.path) {
            pathRef.current = d.path
            updateTabParams<WebTabParams>(tabId, { path: d.path })
          }
          if (d.title) setTabTitle(tabId, d.title)
          authRetries.current = 0
          break
        case 'unload':
          setLoading(true)
          break
        case 'error':
          setBridge(true)
          setLoading(false)
          setPageError(d.error ?? null)
          onPageError(d.error?.code ?? '')
          break
      }
    }
    for (const w of wins) w.addEventListener('message', onMessage)
    return () => {
      for (const w of wins) w.removeEventListener('message', onMessage)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ownerWin, tabId])

  const onPageError = (code: string) => {
    switch (code) {
      case 'auth':
        // The iframe reloaded a spent entry URL (moved between windows) or the cookie expired: get a fresh one once.
        if (authRetries.current++ < 1) {
          void load()
        } else {
          setBanner({
            tone: 'warning',
            text: 'Your browser blocks the proxy’s cookie inside AstraTerm tabs.',
            actions: [
              { label: 'Open in new window', primary: true, run: () => void openExternal() },
              { label: 'Compatibility mode', run: () => void load(undefined, { mode: 'path' }) },
            ],
          })
        }
        break
      case 'gone':
        if (recreated.current < 2) void recreate()
        break
      case 'tls':
        setBanner({
          tone: 'warning',
          text: 'The site’s certificate is not trusted (self-signed or for another name).',
          actions: [{ label: 'Continue without verification', run: () => void reopenWith({ insecureTls: true }) }],
        })
        break
    }
  }

  // No bridge after the first load: the browser may not resolve *.localhost (or the page is not HTML).
  useEffect(() => {
    if (phase !== 'ready' || bridge || loading || entry?.mode !== 'host' || loadedOnce.current === false) return
    const t = setTimeout(() => {
      setBanner(
        (b) =>
          b ?? {
            tone: 'info',
            text: 'No answer from the page. If it stays blank, your browser may not resolve *.localhost addresses.',
            actions: [
              { label: 'Compatibility mode', run: () => void load(undefined, { mode: 'path' }) },
              { label: 'Open in new window', run: () => void openExternal() },
            ],
          },
      )
    }, 6000)
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase, bridge, loading, entry])

  // ---- actions ----------------------------------------------------------------------------------------------------

  const post = useCallback(
    (cmd: string, extra: Record<string, unknown> = {}) => {
      const w = iframeRef.current?.contentWindow
      if (!w || !entry) return false
      let target = '*' // path mode: the sandboxed page has an opaque origin
      if (entry.mode === 'host') {
        try {
          target = new URL(entry.url, location.href).origin
        } catch {
          return false
        }
      }
      w.postMessage({ source: 'astraterm-webproxy', cmd, ...extra }, target)
      return true
    },
    [entry],
  )

  const reload = useCallback(() => {
    setPageError(null)
    if (bridge && !pageError && post('reload')) {
      setLoading(true)
      return
    }
    void load()
  }, [bridge, pageError, post, load])

  const goHome = useCallback(() => {
    const home = paramsRef.current.spec?.path || paramsRef.current.target?.path || '/'
    if (bridge && post('navigate', { path: home })) return
    void load(home)
  }, [bridge, post, load])

  /** Replace the tab's proxy with a new one (other address or TLS choice), keeping its route. */
  const reopenWith = useCallback(
    async (patch: ProxySpec) => {
      const p = paramsRef.current
      const spec: ProxySpec = { ...routeOf(p.spec), ...(p.spec?.connectionId && p.via?.kind === 'web' ? { connectionId: p.spec.connectionId } : {}) }
      if (!patch.url && p.target) spec.url = displayUrl(p.target, pathRef.current)
      Object.assign(spec, patch)
      setBusy(true)
      try {
        const info = await createProxy({ ...spec, mode: p.pathMode ? 'path' : undefined })
        const old = p.proxyId
        adopt(info, spec)
        setBanner(null)
        pathRef.current = info.target.path
        setPage({ path: info.target.path })
        if (old !== info.id) closeProxy(old).catch(() => {})
      } catch (err) {
        toast.error('Could not open the address', { description: errorMessage(err) })
      } finally {
        setBusy(false)
      }
    },
    [adopt],
  )

  const navigate = useCallback(
    (input: string) => {
      const a = parseAddress(input)
      if (!a) {
        toast.error('Not a web address', { description: 'Enter an http or https address, e.g. http://localhost:8080/' })
        return
      }
      const t = paramsRef.current.target
      if (t && a.scheme === t.scheme && a.host.toLowerCase() === t.host.toLowerCase() && a.port === t.port) {
        pathRef.current = a.path
        if (bridge && post('navigate', { path: a.path })) return
        void load(a.path)
        return
      }
      void reopenWith({ url: addressToString(a) })
    },
    [bridge, post, load, reopenWith],
  )

  const openExternal = useCallback(async () => {
    // Open the window synchronously (popup blockers), then point it at a fresh entry URL.
    const win = window.open('about:blank', '_blank')
    try {
      const info = await proxyEntry(paramsRef.current.proxyId, { path: pathRef.current, mode: paramsRef.current.pathMode ? 'path' : undefined })
      if (!info.url) throw new Error('no entry URL')
      const abs = new URL(info.url, location.href).toString()
      if (win) {
        win.opener = null
        win.location.href = abs
      } else {
        window.open(abs, '_blank', 'noopener')
      }
    } catch (err) {
      win?.close()
      toast.error('Could not open a new window', { description: errorMessage(err) })
    }
  }, [])

  const setZoom = useCallback(
    (z: number) => updateTabParams<WebTabParams>(tabId, { zoom: Math.min(3, Math.max(0.25, Math.round(z * 100) / 100)) }),
    [tabId],
  )
  const zoomBy = useCallback(
    (dir: 1 | -1) => {
      const cur = paramsRef.current.zoom || 1
      const next = dir > 0 ? ZOOM_STEPS.find((s) => s > cur + 0.001) : [...ZOOM_STEPS].reverse().find((s) => s < cur - 0.001)
      setZoom(next ?? cur)
    },
    [setZoom],
  )

  const stopApp = useCallback(async () => {
    try {
      await closeProxy(paramsRef.current.proxyId)
    } catch (err) {
      if (errorCode(err) !== 'not_found') toast.error('Could not stop the application', { description: errorMessage(err) })
    }
  }, [])

  const startAgain = useCallback(async () => {
    const req = paramsRef.current.xpra
    if (!req) return
    setBusy(true)
    setFailure('')
    try {
      const info = await startXpra(req)
      adopt(info)
    } catch (err) {
      setFailure(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }, [adopt])

  // Commands (palette / shortcuts) act on the active web tab through this handle.
  useEffect(
    () =>
      registerWebView(tabId, {
        back: () => post('back'),
        forward: () => post('forward'),
        reload,
        home: goHome,
        zoomIn: () => zoomBy(1),
        zoomOut: () => zoomBy(-1),
        zoomReset: () => setZoom(1),
        openExternal: () => void openExternal(),
        focusAddress: () => {
          const el = document.getElementById(`webview-address-${tabId}`) as HTMLInputElement | null
          el?.focus()
          el?.select()
        },
      }),
    [tabId, post, reload, goHome, zoomBy, setZoom, openExternal],
  )

  // Tab indicators: a calm progress hint while a page loads, error dot for failures.
  useEffect(() => {
    setTabState(tabId, {
      progress: showProgress ? 'indeterminate' : null,
      status: phase === 'failed' || pageError ? 'error' : undefined,
    })
  }, [tabId, showProgress, phase, pageError])

  // ---- render -----------------------------------------------------------------------------------------------------

  const target = params.target
  const shownUrl = isXpra ? (params.xpra?.command ?? params.title ?? '') : displayUrl(target, page.path ?? target?.path)
  const secure = target?.scheme === 'https'
  const insecure = !!params.spec?.insecureTls
  const via = params.via

  const onAddressKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      setEditing(false)
      navigate(draft)
      ;(e.target as HTMLInputElement).blur()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      setEditing(false)
      ;(e.target as HTMLInputElement).blur()
    }
  }

  const frameStyle = useMemo(
    () =>
      zoom === 1
        ? undefined
        : { width: `${100 / zoom}%`, height: `${100 / zoom}%`, transform: `scale(${zoom})`, transformOrigin: '0 0' },
    [zoom],
  )

  return (
    <div className="@container flex h-full min-h-0 flex-col bg-background">
      <div className="flex h-9 shrink-0 items-center gap-1 border-b bg-toolbar px-1.5" role="toolbar" aria-label="Browser">
        {!isXpra && (
          <>
            <IconButton icon={ArrowLeft} label="Back" shortcut="Alt+ArrowLeft" disabled={!bridge || phase !== 'ready'} onClick={() => post('back')} />
            <IconButton icon={ArrowRight} label="Forward" shortcut="Alt+ArrowRight" disabled={!bridge || phase !== 'ready'} onClick={() => post('forward')} />
          </>
        )}
        <Tooltip content={showProgress ? 'Loading the page… (click to reload)' : 'Reload'}>
          <Button variant="ghost" size="icon-sm" aria-label="Reload" className="text-muted-foreground hover:text-foreground" disabled={phase === 'init' || phase === 'ended'} onClick={reload}>
            {showProgress ? <Spinner immediate label="Loading the page" /> : <RotateCw />}
          </Button>
        </Tooltip>
        {!isXpra && <IconButton icon={Home} label="Home page" disabled={phase !== 'ready'} onClick={goHome} className="hidden @md:inline-flex" />}

        <div className="relative mx-1 flex h-7 min-w-0 flex-1 items-center rounded-md border bg-background focus-within:ring-2 focus-within:ring-ring/40">
          <Tooltip content={insecure ? 'Certificate not verified' : secure ? 'HTTPS' : 'Not encrypted between the proxy and the site'}>
            <span className="flex h-full items-center pr-1 pl-2 text-muted-foreground">
              {isXpra ? (
                <AppWindow className="size-3.5" />
              ) : insecure ? (
                <ShieldAlert className="size-3.5 text-warning" />
              ) : secure ? (
                <Lock className="size-3.5" />
              ) : (
                <Globe className="size-3.5" />
              )}
            </span>
          </Tooltip>
          <input
            id={`webview-address-${tabId}`}
            className="h-full min-w-0 flex-1 bg-transparent pr-2 font-mono text-sm outline-none placeholder:text-muted-foreground read-only:cursor-default"
            aria-label={isXpra ? 'Application' : 'Address'}
            spellCheck={false}
            autoComplete="off"
            readOnly={isXpra}
            value={editing ? draft : shownUrl}
            onFocus={(e) => {
              if (isXpra) return
              setDraft(shownUrl)
              setEditing(true)
              requestAnimationFrame(() => e.target.select())
            }}
            onBlur={() => setEditing(false)}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onAddressKey}
          />
          {via && (
            <Tooltip content={`Reached ${viaText(via)}`}>
              <Badge variant="outline" className="mr-1 hidden max-w-40 truncate @lg:inline-flex">
                {via.kind === 'direct' ? 'direct' : `via ${via.label}`}
              </Badge>
            </Tooltip>
          )}
        </div>

        <div className="hidden items-center @sm:flex">
          <IconButton icon={ZoomOut} label="Zoom out" disabled={zoom <= 0.5} onClick={() => zoomBy(-1)} />
          <Tooltip content="Reset zoom">
            <button
              type="button"
              className="h-6 min-w-11 rounded-sm px-1 text-center text-xs text-muted-foreground tabular-nums hover:bg-accent hover:text-foreground"
              onClick={() => setZoom(1)}
            >
              {Math.round(zoom * 100)}%
            </button>
          </Tooltip>
          <IconButton icon={ZoomIn} label="Zoom in" disabled={zoom >= 2} onClick={() => zoomBy(1)} />
        </div>
        <IconButton icon={ExternalLink} label="Open in new window" disabled={phase !== 'ready'} onClick={() => void openExternal()} />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label="More" className="text-muted-foreground hover:text-foreground">
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-56">
            <DropdownMenuLabel>{via ? `Reached ${viaText(via)}` : 'Web page'}</DropdownMenuLabel>
            {!isXpra && (
              <DropdownMenuItem
                onSelect={async () => {
                  if (await copyText(shownUrl)) toast.success('Address copied')
                }}
              >
                <Copy /> Copy address
              </DropdownMenuItem>
            )}
            {entry?.mode === 'host' && (
              <DropdownMenuItem onSelect={() => void load(undefined, { mode: 'path' })}>
                <Info /> Compatibility mode (same origin, sandboxed)
              </DropdownMenuItem>
            )}
            {entry?.mode === 'path' && (
              <DropdownMenuItem
                onSelect={() => {
                  updateTabParams<WebTabParams>(tabId, { pathMode: undefined })
                  paramsRef.current = { ...paramsRef.current, pathMode: undefined }
                  void load(undefined, { mode: 'auto' })
                }}
              >
                <Info /> Isolated mode (own origin)
              </DropdownMenuItem>
            )}
            {!isXpra && target?.scheme === 'https' && !insecure && (
              <DropdownMenuItem onSelect={() => void reopenWith({ insecureTls: true })}>
                <ShieldAlert /> Reload without certificate check
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            {isXpra ? (
              <DropdownMenuItem className="text-destructive" onSelect={() => void stopApp()}>
                <Power /> Stop application
              </DropdownMenuItem>
            ) : (
              <DropdownMenuItem onSelect={() => void closeTab(tabId)}>
                <X /> Close tab
              </DropdownMenuItem>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {banner && (
        <div
          role={banner.tone === 'info' ? 'status' : 'alert'}
          className={cn(
            'flex shrink-0 animate-in items-center gap-2 border-b px-3 py-1.5 text-sm duration-150 fade-in-0',
            banner.tone === 'warning' && 'bg-warning/10',
            banner.tone === 'error' && 'bg-destructive/10',
            banner.tone === 'info' && 'bg-muted/50',
          )}
        >
          {banner.tone === 'info' ? <Info className="size-3.5 shrink-0 text-info" /> : <AlertTriangle className="size-3.5 shrink-0 text-warning" />}
          <span className="min-w-0 flex-1">{banner.text}</span>
          {banner.actions?.map((a) => (
            <Button key={a.label} size="xs" variant={a.primary ? 'secondary' : 'ghost'} onClick={a.run}>
              {a.label}
            </Button>
          ))}
          <IconButton icon={X} label="Dismiss" size="xs" onClick={() => setBanner(null)} />
        </div>
      )}

      <div className="relative min-h-0 flex-1 overflow-hidden">
        {phase === 'ready' && entry && (
          <iframe
            key={entry.url}
            ref={iframeRef}
            src={entry.url}
            title={page.title || params.title || 'Web page'}
            className={cn('block size-full border-0', loadedOnce.current && 'bg-white')}
            style={frameStyle}
            // Clipboard reading is delegated to Xpra applications only (clipboard sync); ordinary pages may write.
            allow={params.kind === 'xpra' ? 'clipboard-read; clipboard-write; fullscreen; autoplay' : 'clipboard-write; fullscreen; autoplay'}
            referrerPolicy="no-referrer"
            onLoad={(e) => {
              loadedOnce.current = true
              setLoading(false)
              const w = e.currentTarget.ownerDocument?.defaultView ?? null
              if (w && w !== ownerWin) setOwnerWin(w)
            }}
          />
        )}
        {phase === 'failed' && (
          <EmptyState
            className="h-full"
            icon={AlertTriangle}
            title="The page could not be opened"
            description={failure}
            action={
              <div className="flex gap-2">
                <Button size="sm" variant="secondary" loading={busy} onClick={() => void recreate()}>
                  <RotateCw /> Try again
                </Button>
                <Button size="sm" variant="ghost" onClick={() => void closeTab(tabId)}>
                  Close tab
                </Button>
              </div>
            }
          />
        )}
        {phase === 'ended' && (
          <EmptyState
            className="h-full"
            icon={AppWindow}
            title="The application has ended"
            description={failure || 'The X11 application was closed on the remote host.'}
            action={
              <div className="flex gap-2">
                {params.xpra && (
                  <Button size="sm" variant="secondary" loading={busy} onClick={() => void startAgain()}>
                    <Play /> Start again
                  </Button>
                )}
                <Button size="sm" variant="ghost" onClick={() => void closeTab(tabId)}>
                  Close tab
                </Button>
              </div>
            }
          />
        )}
      </div>
    </div>
  )
}
