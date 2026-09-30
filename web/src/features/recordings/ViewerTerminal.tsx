/*
 * A lightweight terminal viewer speaking the terminal WebSocket protocol (SPEC §6.2) — used by the public share page
 * (/ws/share/<token>) and the admin "Shadow" tab (/ws/terminal/<id>, read-only admin attach). Unlike the terminal tab
 * it never owns the session: it follows the authoritative PTY size (never resizes the session), and closing it never
 * ends the session. Interactive shares send keystrokes; read-only views never send input.
 *
 * Offsets / acks follow the terminal feature: bytes are acknowledged after xterm parsed them; replayed history
 * (attach from offset 0 or a reset) is rendered with xterm's automatic replies suppressed.
 */
import '@xterm/xterm/css/xterm.css'
import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import type { SessionState } from '@/api/types'
import { isDarkTheme, onThemeChange } from '@/lib/theme'
import { BUNDLED_FONT_FAMILY, effectiveTerminalSettings, terminalSettings } from '@/features/terminal/settings'
import { resolveScheme, toXtermTheme } from '@/features/terminal/themes'

type ViewerTransport = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface ViewerStatus {
  transport: ViewerTransport
  state?: SessionState
  message?: string
  exitCode?: number
  readOnly: boolean
  cols: number
  rows: number
  oscTitle?: string
  /** The server closed the stream for good (session ended, link revoked / expired, access lost). */
  ended: boolean
  /** Close code of the last socket close (4410 = share link revoked / expired). */
  closeCode?: number
  /** Close reason of the last socket close. */
  closeReason?: string
  /** Consecutive failed attempts. */
  attempts: number
}

export interface ViewerTerminalHandle {
  focus(): void
  copySelection(): Promise<boolean>
  selectAll(): void
  /** Visible screen as text. */
  screenText(): string
  reconnect(): void
  /** Stop for good (keep the screen, no more reconnects). */
  stop(): void
  term(): Terminal | null
}

export interface ViewerTerminalProps {
  /** WebSocket URL for a given resume offset. */
  url: (offset: number) => string
  /** Allow typing (interactive share links). */
  interactive?: boolean
  /** 'fit' scales the font so the whole PTY fits; 'actual' keeps the font size and scrolls. */
  fit?: 'fit' | 'actual'
  /** Font size in 'actual' mode (and the upper bound in 'fit' mode). */
  fontSize?: number
  onStatus?: (s: ViewerStatus) => void
  /** Called when a connection attempt fails before it opened (lets the page re-check the link). */
  onConnectFailed?: (attempt: number) => void
  className?: string
}

const ACK_STEP = 16 * 1024
const BACKOFF = [500, 1000, 2000, 4000, 8000, 15000]
const encoder = new TextEncoder()

export const ViewerTerminal = forwardRef<ViewerTerminalHandle, ViewerTerminalProps>(function ViewerTerminal(
  { url, interactive = false, fit = 'fit', fontSize = 14, onStatus, onConnectFailed, className },
  ref,
) {
  const hostRef = useRef<HTMLDivElement>(null)
  const wrapRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const [bg, setBg] = useState<string | undefined>(undefined)
  const ctl = useRef<{ reconnect: () => void; refit: () => void; stop: () => void } | null>(null)
  const opts = useRef({ url, interactive, fit, fontSize, onStatus, onConnectFailed })
  opts.current = { url, interactive, fit, fontSize, onStatus, onConnectFailed }

  useImperativeHandle(ref, () => ({
    focus: () => termRef.current?.focus(),
    copySelection: async () => {
      const text = termRef.current?.getSelection() ?? ''
      if (!text) return false
      try {
        await navigator.clipboard.writeText(text)
        return true
      } catch {
        return false
      }
    },
    selectAll: () => termRef.current?.selectAll(),
    screenText: () => {
      const t = termRef.current
      if (!t) return ''
      const b = t.buffer.active
      const lines: string[] = []
      for (let i = 0; i < t.rows; i++) lines.push(b.getLine(b.viewportY + i)?.translateToString(true) ?? '')
      return lines.join('\n').replace(/\s+$/, '')
    },
    reconnect: () => ctl.current?.reconnect(),
    stop: () => ctl.current?.stop(),
    term: () => termRef.current,
  }))

  useEffect(() => {
    const host = hostRef.current
    const wrap = wrapRef.current
    if (!host || !wrap) return
    const settings = effectiveTerminalSettings(terminalSettings.get())
    const scheme = () => resolveScheme(settings, isDarkTheme())
    const term = new Terminal({
      cols: 80,
      rows: 24,
      fontFamily: settings.fontFamily || BUNDLED_FONT_FAMILY,
      fontSize: opts.current.fontSize,
      lineHeight: 1.1,
      cursorBlink: false,
      disableStdin: !opts.current.interactive,
      scrollback: 5000,
      allowProposedApi: true,
      theme: toXtermTheme(scheme()),
      logLevel: 'off',
    })
    termRef.current = term
    term.open(host)
    setBg(scheme().background)

    const status: ViewerStatus = { transport: 'connecting', readOnly: !opts.current.interactive, cols: 80, rows: 24, ended: false, attempts: 0 }
    const emit = (patch: Partial<ViewerStatus>) => {
      Object.assign(status, patch)
      opts.current.onStatus?.({ ...status })
    }

    let ws: WebSocket | null = null
    let disposed = false
    let retry: ReturnType<typeof setTimeout> | null = null
    let ackTimer: ReturnType<typeof setTimeout> | null = null
    let received = 0
    let rendered = 0
    let acked = 0
    let replayHead = 0
    let skip = 0
    let requested = 0

    const sendCtl = (msg: object) => {
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg))
    }
    const maybeAck = (force = false) => {
      const un = rendered - acked
      if (un <= 0) return
      if (un >= ACK_STEP || force) {
        sendCtl({ type: 'ack', offset: rendered })
        acked = rendered
        if (ackTimer) clearTimeout(ackTimer)
        ackTimer = null
      } else if (!ackTimer) {
        ackTimer = setTimeout(() => {
          ackTimer = null
          maybeAck(true)
        }, 150)
      }
    }

    // --- fitting -------------------------------------------------------------------------------------------------
    let fitFrame = 0
    const refit = () => {
      cancelAnimationFrame(fitFrame)
      fitFrame = requestAnimationFrame(() => doFit(3))
    }
    const doFit = (tries: number) => {
      if (disposed) return
      const base = opts.current.fontSize
      if (opts.current.fit !== 'fit') {
        if (term.options.fontSize !== base) term.options.fontSize = base
        return
      }
      const screen = host.querySelector('.xterm-screen') as HTMLElement | null
      if (!screen || !screen.offsetWidth || !screen.offsetHeight) return
      const aw = wrap.clientWidth - 16
      const ah = wrap.clientHeight - 16
      const cur = term.options.fontSize ?? base
      const ratio = Math.min(aw / screen.offsetWidth, ah / screen.offsetHeight)
      const next = Math.max(6, Math.min(base * 1.25, Math.floor(cur * ratio * 2) / 2))
      if (Math.abs(next - cur) >= 0.5) {
        term.options.fontSize = next
        if (tries > 0) fitFrame = requestAnimationFrame(() => doFit(tries - 1))
      }
    }
    const stop = () => {
      if (retry) clearTimeout(retry)
      retry = null
      const s = ws
      ws = null
      if (s) {
        s.onclose = null
        s.close(1000, 'stopped')
      }
      emit({ transport: 'closed', ended: true })
    }
    ctl.current = { reconnect: () => reconnectNow(), refit, stop }
    const ro = new ResizeObserver(refit)
    ro.observe(wrap)
    const offTheme = onThemeChange(() => {
      term.options.theme = toXtermTheme(scheme())
      setBg(scheme().background)
    })

    // --- input ---------------------------------------------------------------------------------------------------
    const onData = term.onData((d) => {
      if (!opts.current.interactive || status.readOnly) return
      if (rendered < replayHead) return // automatic replies to replayed history
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(d))
    })
    const onBinary = term.onBinary((d) => {
      if (!opts.current.interactive || status.readOnly || rendered < replayHead) return
      if (ws && ws.readyState === WebSocket.OPEN) {
        const bytes = new Uint8Array(d.length)
        for (let i = 0; i < d.length; i++) bytes[i] = d.charCodeAt(i) & 0xff
        ws.send(bytes)
      }
    })

    // --- socket --------------------------------------------------------------------------------------------------
    const onMessage = (msg: Record<string, unknown>) => {
      switch (msg.type) {
        case 'attach': {
          const from = Math.max(0, Number(msg.from) || 0)
          const head = Math.max(from, Number(msg.head) || from)
          const reset = msg.mode === 'reset'
          replayHead = reset || requested === 0 ? head : from
          skip = 0
          if (reset) {
            term.write(new Uint8Array(0), () => term.reset())
            received = rendered = from
          } else if (from < received) {
            skip = received - from
          } else {
            received = from
          }
          acked = from
          break
        }
        case 'state':
          emit({ state: msg.state as SessionState, message: (msg.message as string) || undefined, exitCode: msg.exitCode as number | undefined })
          break
        case 'readonly':
          emit({ readOnly: !!msg.value || !opts.current.interactive })
          break
        case 'resize': {
          const cols = Math.max(2, Math.min(1000, Number(msg.cols) || 80))
          const rows = Math.max(1, Math.min(1000, Number(msg.rows) || 24))
          if (cols !== term.cols || rows !== term.rows) term.resize(cols, rows)
          emit({ cols, rows })
          refit()
          break
        }
        case 'title':
          emit({ oscTitle: String(msg.title ?? '') })
          break
        case 'error':
          emit({ message: String(msg.message ?? '') })
          break
      }
    }

    const connect = () => {
      if (disposed || ws) return
      requested = received
      emit({ transport: status.attempts > 0 ? 'reconnecting' : 'connecting' })
      let sock: WebSocket
      try {
        sock = new WebSocket(opts.current.url(received))
      } catch {
        schedule(false)
        return
      }
      sock.binaryType = 'arraybuffer'
      ws = sock
      let opened = false
      let stable: ReturnType<typeof setTimeout> | null = null
      sock.onopen = () => {
        opened = true
        stable = setTimeout(() => emit({ attempts: 0 }), 5000)
        emit({ transport: 'open', closeCode: undefined })
      }
      sock.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          try {
            const m = JSON.parse(ev.data)
            if (m && typeof m === 'object') onMessage(m)
          } catch {
            /* ignore */
          }
          return
        }
        let data = new Uint8Array(ev.data as ArrayBuffer)
        if (skip > 0) {
          if (data.length <= skip) {
            skip -= data.length
            return
          }
          data = data.subarray(skip)
          skip = 0
        }
        received += data.length
        const end = received
        term.write(data, () => {
          rendered = end
          maybeAck()
        })
      }
      sock.onclose = (ev) => {
        if (stable) clearTimeout(stable)
        if (ws !== sock) return
        ws = null
        if (disposed) return
        // 1000 = the session ended (the server closes after the final state); 4404 = unknown session;
        // 4410 = share link revoked or expired.
        const final = ev.code === 1000 || ev.code === 4404 || ev.code === 4410 || status.state === 'closed'
        if (final) {
          emit({ transport: 'closed', ended: true, closeCode: ev.code, closeReason: ev.reason || undefined })
          return
        }
        emit({ closeCode: ev.code })
        if (!opened) opts.current.onConnectFailed?.(status.attempts + 1)
        schedule(!opened)
      }
    }
    const schedule = (_failedBeforeOpen: boolean) => {
      if (disposed || retry || status.ended) return
      const attempts = status.attempts + 1
      emit({ transport: 'reconnecting', attempts })
      const base = BACKOFF[Math.min(attempts - 1, BACKOFF.length - 1)]
      retry = setTimeout(() => {
        retry = null
        connect()
      }, base + Math.random() * 0.3 * base)
    }
    const reconnectNow = () => {
      if (disposed) return
      if (retry) clearTimeout(retry)
      retry = null
      status.ended = false
      if (ws) {
        const old = ws
        ws = null
        old.onclose = null
        old.close(1000, 'reconnect')
      }
      connect()
    }
    const onOnline = () => {
      if (status.transport === 'reconnecting') reconnectNow()
    }
    window.addEventListener('online', onOnline)
    const ping = setInterval(() => sendCtl({ type: 'ping' }), 20_000)
    connect()

    return () => {
      disposed = true
      clearInterval(ping)
      if (retry) clearTimeout(retry)
      if (ackTimer) clearTimeout(ackTimer)
      cancelAnimationFrame(fitFrame)
      window.removeEventListener('online', onOnline)
      ro.disconnect()
      offTheme()
      onData.dispose()
      onBinary.dispose()
      const s = ws
      ws = null
      if (s) {
        s.onclose = null
        try {
          s.close(1000, 'viewer closed')
        } catch {
          /* ignore */
        }
      }
      term.dispose()
      termRef.current = null
    }
  }, [])

  // Font / fit mode changes.
  useEffect(() => {
    const t = termRef.current
    if (!t) return
    if (fit === 'actual') t.options.fontSize = fontSize
    ctl.current?.refit()
  }, [fit, fontSize])

  useEffect(() => {
    const t = termRef.current
    if (t) t.options.disableStdin = !interactive
  }, [interactive])

  return (
    <div
      ref={wrapRef}
      className={className ?? 'relative h-full w-full overflow-auto'}
      style={{ background: bg }}
      data-terminal=""
    >
      <div ref={hostRef} className={fit === 'fit' ? 'mx-auto w-fit p-2' : 'w-fit p-2'} />
    </div>
  )
})
