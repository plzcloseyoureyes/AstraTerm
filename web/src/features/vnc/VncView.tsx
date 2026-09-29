/*
 * The `vnc` tab (lazy chunk; noVNC itself is imported on demand by the controller). Layout: toolbar, the remote
 * desktop (noVNC target inside a scroll container), optional clipboard panel, status overlays. Keyboard focus in the
 * desktop is the "terminal" keybinding scope (plain Ctrl+<key> goes to the remote); with `keyboardCapture = all` or in
 * fullscreen with Keyboard Lock, every key goes to the remote desktop.
 */
import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore, type CSSProperties } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useConnections } from '@/api/connections'
import { getSession, useRuntimeSession } from '@/api/sessions'
import { queryKeys } from '@/api/queryKeys'
import type { TabProps } from '@/app/registry'
import { restartSessionInTab } from '@/features/terminal/open'
import { useMediaQuery } from '@/lib/hooks'
import { cn } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { requestVaultUnlock } from '@/stores/ui'
import { closeTab, setTabState, useIsTabVisible, useWorkspaceStore } from '@/stores/workspace'
import { registerRoot } from './actions'
import { ClipboardPanel } from './ClipboardPanel'
import { VncController, type VncState } from './controller'
import { K, type Key } from './keys'
import { StatusOverlay } from './Overlay'
import { canEditConnection } from './policy'
import { clampLevel, combineClipboard, isScalingMode, vncSettings } from './settings'
import { registerController, setTabUI, useTabUI } from './store'
import { VncToolbar } from './Toolbar'
import { useVncInfo, vncKeys } from './api'
import type { VncTabParams } from './types'

function useController(c: VncController | null): VncState | null {
  const subscribe = useCallback((cb: () => void) => (c ? c.store.subscribe(cb) : () => undefined), [c])
  const get = useCallback(() => (c ? c.store.getState() : null), [c])
  return useSyncExternalStore(subscribe, get, get)
}

const SPECIAL_KEYS: Record<string, Key> = {
  Enter: K.Return,
  Backspace: K.BackSpace,
  Tab: K.Tab,
  Escape: K.Escape,
  Delete: K.Delete,
  ArrowLeft: K.Left,
  ArrowRight: K.Right,
  ArrowUp: K.Up,
  ArrowDown: K.Down,
  Home: K.Home,
  End: K.End,
  PageUp: K.PageUp,
  PageDown: K.PageDown,
}

export default function VncView({ tabId, params }: TabProps<VncTabParams>) {
  const sessionId = params?.sessionId
  const qc = useQueryClient()
  const user = useCurrentUser()
  const session = useRuntimeSession(sessionId)
  const connsQuery = useConnections(!!params?.connectionId)
  const conn = params?.connectionId ? connsQuery.data?.find((x) => x.id === params.connectionId) : undefined
  const optionsReady = !params?.connectionId || connsQuery.isFetched
  // Sessions of other users (admin shadowing) are not in the own-sessions list: look the owner up once.
  const detail = useQuery({
    queryKey: queryKeys.session(sessionId ?? ''),
    placeholderData: undefined, // never show another session's details under this key
    queryFn: () => getSession(sessionId!),
    enabled: !!sessionId && !session,
    retry: false,
    staleTime: 60_000,
  })
  const ownerId = session?.ownerId ?? detail.data?.ownerId
  const readOnly = !!ownerId && !!user && ownerId !== user.id

  const rootRef = useRef<HTMLDivElement>(null)
  const viewportRef = useRef<HTMLDivElement>(null)
  const targetRef = useRef<HTMLDivElement>(null)
  const keyboardRef = useRef<HTMLTextAreaElement>(null)
  const [controller, setController] = useState<VncController | null>(null)
  const s = useController(controller)
  const ui = useTabUI(tabId)
  const visible = useIsTabVisible(tabId)
  const active = useWorkspaceStore((st) => st.activeTabId === tabId)
  const touch = useMediaQuery('(pointer: coarse)')
  const settings = vncSettings.use()
  const info = useVncInfo(sessionId, s?.phase === 'connected')
  // Saved connections the user may change get the policy controls (security popover, confirmation dialog).
  const editableConnection = !readOnly && canEditConnection(conn, user) ? conn : undefined

  const options = useMemo(() => (conn?.options ?? params?.quick?.options ?? {}) as Record<string, unknown>, [conn, params?.quick])
  const reverse = !!params?.reverse || options.reverse === true
  const target =
    session?.host || conn?.host || params?.quick?.host
      ? `${session?.host || conn?.host || params?.quick?.host}`
      : 'the VNC server'

  // One controller per session id (a restarted session gets a new one).
  useEffect(() => {
    const el = targetRef.current
    if (!sessionId || !el || !optionsReady) return
    const st = vncSettings.get()
    const c = new VncController({
      tabId,
      sessionId,
      target: el,
      readOnly: false,
      reverse,
      shared: options.shared !== false,
      autoReconnect: !reverse && (typeof options.autoReconnect === 'boolean' ? options.autoReconnect : st.autoReconnect),
      scaling: isScalingMode(options.scaling) ? options.scaling : st.scaling,
      quality: clampLevel(options.quality, st.quality),
      compression: clampLevel(options.compression, st.compression),
      viewOnly: options.viewOnly === true || params?.viewOnly === true,
      onSessionGone: (_reason, automatic) => {
        void restartSessionInTab(tabId).then((ok) => {
          if (!ok) c.restartFailed(automatic)
        })
      },
      onVaultLocked: requestVaultUnlock,
      onConnected: () => void qc.invalidateQueries({ queryKey: vncKeys.info(sessionId) }),
    })
    const unregister = registerController(tabId, c)
    setController(c)
    void c.start()
    return () => {
      unregister()
      c.dispose()
      setController((cur) => (cur === c ? null : cur))
    }
    // Connection options are read when the viewer is created; changing them takes effect on the next session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabId, sessionId, optionsReady])

  useEffect(() => {
    if (controller && readOnly) controller.setReadOnly()
  }, [controller, readOnly])

  useEffect(() => {
    const el = rootRef.current
    return el ? registerRoot(tabId, el) : undefined
  }, [tabId])

  // Tab status dot.
  const phase = s?.phase
  useEffect(() => {
    if (!phase) return
    const status =
      phase === 'connected' ? 'connected' : phase === 'disconnected' ? 'disconnected' : phase === 'error' && !s?.retryAt ? 'error' : 'connecting'
    setTabState(tabId, { status })
  }, [tabId, phase, s?.retryAt])
  useEffect(() => () => setTabState(tabId, { status: undefined, activity: undefined }), [tabId])

  // Fullscreen bookkeeping (the element may live in a pop-out window).
  useEffect(() => {
    const el = rootRef.current
    if (!el) return
    const doc = el.ownerDocument
    const onChange = () => {
      const fs = doc.fullscreenElement === el
      setTabUI(tabId, fs ? { fullscreen: true } : { fullscreen: false, keyboardLocked: false })
      if (fs) controller?.focus()
    }
    doc.addEventListener('fullscreenchange', onChange)
    return () => doc.removeEventListener('fullscreenchange', onChange)
  }, [tabId, controller])

  useEffect(() => {
    controller?.setVisible(visible)
  }, [controller, visible])

  // Clipboard direction: the connection's / administrator's policy (vnc-info) combined with the user's setting.
  const policyClipboard = info.data?.clipboard
  const infoFresh = !!info.data && info.data.connected && !info.isFetching
  useEffect(() => {
    if (controller && phase === 'connected' && infoFresh) {
      controller.setClipboardPolicy(combineClipboard(policyClipboard, settings.clipboardDirection))
    }
  }, [controller, phase, infoFresh, policyClipboard, settings.clipboardDirection])

  // HiDPI remote resize: the setting, and the screen's pixel ratio (the window may move to another monitor).
  useEffect(() => {
    controller?.setHiDpi(settings.hiDpi)
  }, [controller, settings.hiDpi])
  useEffect(() => {
    const win = rootRef.current?.ownerDocument.defaultView
    if (!controller || !win?.matchMedia) return
    let mq: MediaQueryList | null = null
    const onChange = () => {
      controller.requestRemoteResize()
      listen()
    }
    const listen = () => {
      mq?.removeEventListener('change', onChange)
      mq = win.matchMedia(`(resolution: ${win.devicePixelRatio}dppx)`)
      mq.addEventListener('change', onChange)
    }
    listen()
    return () => mq?.removeEventListener('change', onChange)
  }, [controller])

  // Focus the desktop and sync the clipboard when the tab becomes the visible, active one.
  useEffect(() => {
    if (!controller || !visible || !active || phase !== 'connected') return
    controller.focus()
    void controller.syncLocalClipboard()
  }, [controller, visible, active, phase])

  useEffect(() => {
    if (!controller) return
    const win = rootRef.current?.ownerDocument.defaultView ?? window
    const onFocus = () => {
      if (visible) void controller.syncLocalClipboard()
    }
    const onOnline = () => controller.retryNow()
    win.addEventListener('focus', onFocus)
    win.addEventListener('online', onOnline)
    return () => {
      win.removeEventListener('focus', onFocus)
      win.removeEventListener('online', onOnline)
    }
  }, [controller, visible])

  // Bell: flash (and optionally beep); mark background tabs.
  const [flash, setFlash] = useState(false)
  const bellAt = s?.bellAt
  useEffect(() => {
    if (!bellAt) return
    const mode = vncSettings.get().bell
    if (mode === 'off') return
    setFlash(true)
    const t = setTimeout(() => setFlash(false), 180)
    if (mode === 'sound') beep()
    if (!visible) setTabState(tabId, { activity: true })
    return () => clearTimeout(t)
  }, [bellAt, tabId, visible])

  // Fullscreen: auto-hiding toolbar.
  const [revealBar, setRevealBar] = useState(false)
  const hideTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const onPointerMove = (e: React.PointerEvent) => {
    if (!ui.fullscreen) return
    if (e.clientY <= 6) {
      if (hideTimer.current) clearTimeout(hideTimer.current)
      setRevealBar(true)
    }
  }
  const scheduleHide = () => {
    if (hideTimer.current) clearTimeout(hideTimer.current)
    hideTimer.current = setTimeout(() => setRevealBar(false), 1200)
  }

  const size = controller && s ? controller.targetSize() : undefined
  const targetStyle: CSSProperties = size ? { width: size.width, height: size.height, flex: 'none' } : { width: '100%', height: '100%' }
  const showToolbar = settings.showToolbar && ui.toolbar

  // On-screen keyboard (touch devices / mobile IMEs).
  const onKeyboardKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const k = SPECIAL_KEYS[e.key]
    if (k && controller) {
      e.preventDefault()
      controller.sendKey(k)
    }
  }
  const onKeyboardInput = (e: React.FormEvent<HTMLTextAreaElement>) => {
    const ne = e.nativeEvent as InputEvent
    const el = e.currentTarget
    if (ne.isComposing) return
    if (ne.inputType === 'deleteContentBackward') controller?.sendKey(K.BackSpace)
    else if (ne.inputType === 'insertLineBreak') controller?.sendKey(K.Return)
    else if (el.value) void controller?.typeText(el.value)
    el.value = ''
  }

  if (!sessionId) {
    return <div className="flex h-full items-center justify-center text-sm text-muted-foreground">This tab has no VNC session.</div>
  }

  return (
    <div
      ref={rootRef}
      data-vnc-view={tabId}
      className={cn('@container relative flex h-full w-full flex-col overflow-hidden bg-background', ui.fullscreen && 'bg-remote-backdrop-fullscreen')}
      onPointerMove={onPointerMove}
    >
      {showToolbar && controller && s && (
        <div
          className={cn(
            ui.fullscreen &&
              'absolute inset-x-0 top-0 z-30 -translate-y-full shadow-popover transition-transform duration-150 focus-within:translate-y-0',
            ui.fullscreen && revealBar && 'translate-y-0',
          )}
          onPointerEnter={() => ui.fullscreen && hideTimer.current && clearTimeout(hideTimer.current)}
          onPointerLeave={() => ui.fullscreen && scheduleHide()}
        >
          <VncToolbar
            c={controller}
            s={s}
            info={info.data}
            statusMessage={session?.stateMessage}
            touch={touch}
            onShowKeyboard={() => keyboardRef.current?.focus()}
            editableConnection={editableConnection}
          />
        </div>
      )}
      <div className="relative flex min-h-0 flex-1">
        <div className="relative min-w-0 flex-1">
          <div
            ref={viewportRef}
            data-terminal=""
            className={cn(
              'absolute inset-0 overflow-auto outline-none [touch-action:none]',
              flash && 'ring-2 ring-inset ring-warning/80',
            )}
            style={{ '--vnc-surround': ui.fullscreen ? 'var(--remote-backdrop-fullscreen)' : 'var(--remote-backdrop)', background: 'var(--vnc-surround)' } as CSSProperties}
          >
            <div className={cn('flex', size ? 'min-h-full min-w-full w-max h-max items-center justify-center' : 'h-full w-full')}>
              <div ref={targetRef} style={targetStyle} />
            </div>
          </div>
          {controller && s && (
            <StatusOverlay
              c={controller}
              s={s}
              sessionMessage={session?.stateMessage}
              target={target}
              onRestart={() => controller.restartSession()}
              onCloseTab={() => void closeTab(tabId)}
              editableConnection={editableConnection}
            />
          )}
          {!controller && (
            <div className="absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">Preparing the viewer…</div>
          )}
        </div>
        {ui.clipboardOpen && controller && s && !controller.readOnly && <ClipboardPanel c={controller} s={s} />}
      </div>
      <textarea
        ref={keyboardRef}
        aria-label="On-screen keyboard input"
        autoCapitalize="off"
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        className="pointer-events-none absolute bottom-0 left-0 size-px opacity-0"
        onKeyDown={onKeyboardKey}
        onInput={onKeyboardInput}
        onCompositionEnd={(e) => {
          const el = e.currentTarget
          if (el.value) void controller?.typeText(el.value)
          el.value = ''
        }}
        tabIndex={-1}
      />
    </div>
  )
}

let audioCtx: AudioContext | null = null

function beep(): void {
  try {
    audioCtx ??= new AudioContext()
    const osc = audioCtx.createOscillator()
    const gain = audioCtx.createGain()
    osc.frequency.value = 880
    gain.gain.value = 0.05
    osc.connect(gain).connect(audioCtx.destination)
    osc.start()
    osc.stop(audioCtx.currentTime + 0.08)
  } catch {
    /* audio unavailable */
  }
}
