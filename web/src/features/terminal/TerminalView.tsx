/*
 * Terminal tab (kind "terminal", params {sessionId, ...}). Lazy chunk: xterm and its addons load with this module.
 * The TerminalController does the work; this component wires React state (settings, session cache, visibility,
 * dockview events) into it and renders the overlays, the search bar, dialogs and the context menu.
 */
import '@xterm/xterm/css/xterm.css'
import './terminal.css'
import { useEffect, useMemo, useRef, useState, useSyncExternalStore, type CSSProperties } from 'react'
import { Bell } from 'lucide-react'
import { useConnection } from '@/api/connections'
import { useRuntimeSession } from '@/api/sessions'
import type { TabProps } from '@/app/registry'
import { useLatest } from '@/lib/hooks'
import { isDarkTheme, onThemeChange } from '@/lib/theme'
import { useUIStore } from '@/stores/ui'
import { getPanel, useIsTabVisible } from '@/stores/workspace'
import { TerminalController } from './controller'
import { TerminalDialogs } from './Dialogs'
import { isParticipant, useMultiExecStore } from './multiexec'
import { TerminalOverlays } from './Overlays'
import { SearchBar } from './SearchBar'
import { effectiveTerminalSettings, terminalSettings } from './settings'
import { TerminalContextMenu } from './TerminalContextMenu'
import { TerminalToolbar } from './TerminalToolbar'
import { resolveScheme, toXtermTheme } from './themes'
import type { TerminalInfo, TerminalTabParams } from './types'

const noopSubscribe = () => () => undefined
/** How long the visual bell mark stays after the last bell. */
const BELL_MARK_MS = 2500
const nullInfo = (): TerminalInfo | null => null

function subscribeTheme(cb: () => void): () => void {
  return onThemeChange(() => cb())
}

function useUiDark(): boolean {
  return useSyncExternalStore(subscribeTheme, isDarkTheme, isDarkTheme)
}

export default function TerminalView({ tabId, params }: TabProps<TerminalTabParams>) {
  const hostRef = useRef<HTMLDivElement>(null)
  const [ctrl, setCtrl] = useState<TerminalController | null>(null)
  const [container, setContainer] = useState<HTMLElement | undefined>(undefined)
  const sessionId = params?.sessionId
  const session = useRuntimeSession(sessionId)
  const { data: connection } = useConnection(params?.connectionId)
  const global = terminalSettings.use()
  const uiDark = useUiDark()
  const overrides = connection?.options?.terminal ?? params?.quick?.options?.terminal
  const settings = useMemo(() => effectiveTerminalSettings(global, overrides), [global, overrides])
  const visible = useIsTabVisible(tabId)
  const locked = useUIStore((s) => s.locked)
  const multi = useMultiExecStore()
  const latest = useLatest({ params, settings, uiDark, connection, session })

  // One controller per (tab, session). A new session id (restart in place) creates a fresh terminal.
  useEffect(() => {
    const host = hostRef.current
    if (!host || !sessionId) return
    const l = latest.current
    const c = new TerminalController({
      tabId,
      params: l.params,
      settings: l.settings,
      uiDark: l.uiDark,
      connection: l.connection,
      session: l.session,
    })
    setCtrl(c)
    setContainer(host.ownerDocument.body)
    void c.mount(host).catch((err) => console.error('[terminal] mount failed', err))
    return () => {
      c.dispose()
      setCtrl((cur) => (cur === c ? null : cur))
    }
    // `latest` carries the other inputs; they are pushed into the controller by the effects below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabId, sessionId])

  useEffect(() => {
    ctrl?.applySettings(settings, uiDark)
  }, [ctrl, settings, uiDark])

  useEffect(() => {
    if (params) ctrl?.setParams(params)
  }, [ctrl, params])

  useEffect(() => {
    ctrl?.setConnection(connection)
  }, [ctrl, connection])

  useEffect(() => {
    ctrl?.setSession(session)
  }, [ctrl, session])

  useEffect(() => {
    ctrl?.setVisible(visible)
  }, [ctrl, visible])

  // Dockview panel events: activation focuses the terminal; moving to another window re-opens xterm there.
  useEffect(() => {
    if (!ctrl) return
    const panel = getPanel(tabId)
    if (!panel) return
    const subs = [
      panel.api.onDidActiveChange((e) => {
        if (e.isActive) ctrl.onActivated()
      }),
      panel.api.onDidLocationChange(() => {
        // The DOM is moved after the event; check on the next frames.
        const check = () => {
          ctrl.checkWindow()
          const host = hostRef.current
          if (host) setContainer(host.ownerDocument.body)
        }
        requestAnimationFrame(() => requestAnimationFrame(check))
        setTimeout(check, 250)
      }),
    ]
    return () => {
      for (const s of subs) s.dispose()
    }
  }, [ctrl, tabId])

  // Visual bell: a steady bell mark for a few seconds (a later bell extends it) — never a screen flash.
  const [bell, setBell] = useState(false)
  useEffect(() => {
    if (!ctrl) return undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    const off = ctrl.onBell(() => {
      setBell(true)
      clearTimeout(timer)
      timer = setTimeout(() => setBell(false), BELL_MARK_MS)
    })
    return () => {
      off()
      clearTimeout(timer)
    }
  }, [ctrl])

  const info = useSyncExternalStore(ctrl?.subscribeInfo ?? noopSubscribe, ctrl?.getInfo ?? nullInfo, ctrl?.getInfo ?? nullInfo)

  const scheme = resolveScheme(settings, uiDark)
  const theme = toXtermTheme(scheme, settings.backgroundOpacity)
  const opaque = settings.backgroundOpacity >= 1
  const pad = settings.padding
  const color = params?.color ?? connection?.color
  const participant = isParticipant(tabId, multi)

  const containerStyle: CSSProperties = opaque ? { background: theme.background } : {}
  // No bottom padding: rows are whole, so the fit's leftover (0..1 row) already pads the bottom — a fixed pad there
  // would often cost a whole row.
  const hostStyle: CSSProperties = {
    inset: `${pad}px ${pad}px ${Math.min(pad, 2)}px`,
    ...(opaque ? { background: theme.background } : { boxShadow: `0 0 0 ${pad}px ${theme.background}` }),
  }

  if (!sessionId) {
    return <div className="flex h-full items-center justify-center text-sm text-muted-foreground">This tab has no session.</div>
  }

  return (
    <div className="group/term relative h-full w-full overflow-hidden" style={containerStyle}>
      {color && <div className="pointer-events-none absolute inset-x-0 top-0 z-10 h-0.5" style={{ background: color }} aria-hidden />}
      <TerminalContextMenu ctrl={ctrl} container={container} disabled={locked}>
        <div ref={hostRef} className="nx-term-host absolute" style={hostStyle} />
      </TerminalContextMenu>
      {bell && (
        <div className="pointer-events-none absolute top-2 left-2 z-20 flex size-6 items-center justify-center rounded-md border bg-popover/90 text-warning shadow-popover" role="img" aria-label="Bell">
          <Bell className="size-3.5" />
        </div>
      )}
      {participant && <div className="nx-term-multiexec pointer-events-none absolute inset-0 z-20" aria-hidden title="Broadcast" />}
      {ctrl && info?.searchOpen && <SearchBar ctrl={ctrl} />}
      {ctrl && info && settings.showToolbar && !info.searchOpen && <TerminalToolbar ctrl={ctrl} info={info} />}
      <TerminalOverlays ctrl={ctrl} info={info} belowToolbar={settings.showToolbar} />
      {!locked && <TerminalDialogs ctrl={ctrl} container={container} />}
    </div>
  )
}
