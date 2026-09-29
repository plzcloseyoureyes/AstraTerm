/*
 * Hex monitor (PROTO-12): a dual hex + ASCII dump of a serial/raw session's traffic with RX/TX colouring, per-row
 * timestamps and byte counters, plus a "send hex" box. Rendered as a bottom drawer by the protocols overlay. The dump
 * is virtualised so long captures stay responsive.
 */
import { useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDownToLine, ArrowUpFromLine, Eraser, SendHorizonal, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { cn, formatBytes } from '@/lib/utils'
import { getTerminalsBySession, useTerminals } from '@/features/terminal/bus'
import { appendHex, clearHex, getBuffer, hexVersion, subscribeHex } from './hexStore'
import { closeHexMonitor } from './store'

const COLS = 16

interface Row {
  offset: number
  bytes: number[]
  rx: boolean[]
  ts: number
}

function buildRows(sessionId: string): { rows: Row[]; rx: number; tx: number } {
  const buf = getBuffer(sessionId)
  if (!buf) return { rows: [], rx: 0, tx: 0 }
  const rows: Row[] = []
  let cur: Row | null = null
  let offset = 0
  for (const ev of buf.events) {
    for (let i = 0; i < ev.data.length; i++) {
      if (!cur || cur.bytes.length === COLS) {
        cur = { offset, bytes: [], rx: [], ts: ev.ts }
        rows.push(cur)
      }
      cur.bytes.push(ev.data[i])
      cur.rx.push(ev.rx)
      offset++
    }
  }
  return { rows, rx: buf.rxBytes, tx: buf.txBytes }
}

const hex2 = (n: number) => n.toString(16).padStart(2, '0')
const ascii = (n: number) => (n >= 0x20 && n <= 0x7e ? String.fromCharCode(n) : '·')

function timeOf(ts: number): string {
  const d = new Date(ts)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}.${String(d.getMilliseconds()).padStart(3, '0')}`
}

/**
 * Parse hex input into bytes: "01 03 00 0A", "0x01,0x02", "0103000a" or single digits separated by spaces ("1 2 3").
 * Returns null on malformed input.
 */
export function parseHex(input: string): Uint8Array | null {
  const out: number[] = []
  for (let tok of input.split(/[\s,;:]+/)) {
    if (tok === '') continue
    tok = tok.replace(/^0x/i, '')
    if (!/^[0-9a-f]+$/i.test(tok)) return null
    if (tok.length === 1) tok = '0' + tok
    if (tok.length % 2 !== 0) return null
    for (let i = 0; i < tok.length; i += 2) out.push(parseInt(tok.slice(i, i + 2), 16))
  }
  return new Uint8Array(out)
}

export function HexView({ sessionId }: { sessionId: string }) {
  const version = useSyncExternalStore(subscribeHex, hexVersion, hexVersion)
  // `version` is the store's change signal: rebuild the rows whenever it moves.
  const { rows, rx, tx } = useMemo(() => buildRows(sessionId), [sessionId, version])
  useTerminals() // re-render when terminals come and go (title, send availability)
  const term = getTerminalsBySession(sessionId)[0]
  const title = term?.info().title
  const [sendText, setSendText] = useState('')
  const [sendError, setSendError] = useState<string | null>(null)
  const [appendCr, setAppendCr] = useState(false)
  const parentRef = useRef<HTMLDivElement>(null)
  const stick = useRef(true)

  const rowVirtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 20,
    overscan: 12,
  })

  // Keep the view pinned to the newest bytes while data arrives, unless the user scrolled up.
  useLayoutEffect(() => {
    if (stick.current && rows.length > 0) rowVirtualizer.scrollToIndex(rows.length - 1, { align: 'end' })
  }, [rows.length, rowVirtualizer])

  const send = () => {
    const bytes = parseHex(sendText)
    if (!bytes) {
      setSendError('Enter hex bytes, e.g. 01 03 00 00 00 0A C5 CD')
      return
    }
    const payload = appendCr ? Uint8Array.from([...bytes, 0x0d]) : bytes
    if (payload.length === 0) return
    if (!term || !term.send(payload)) {
      setSendError('The session is not connected')
      return
    }
    // Raw sends bypass the terminal's input observers: record them as TX here.
    appendHex(sessionId, false, payload)
    setSendError(null)
    setSendText('')
  }

  return (
    <section
      aria-label="Hex monitor"
      className="pointer-events-auto flex h-72 flex-col overflow-hidden rounded-t-lg border border-b-0 bg-popover text-popover-foreground shadow-popover"
    >
      <div className="flex items-center gap-2 border-b px-3 py-1.5">
        <span className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">Hex monitor</span>
        {title && <span className="max-w-64 truncate text-xs text-muted-foreground">{title}</span>}
        <span className="inline-flex items-center gap-1 text-xs text-success" title="Received bytes">
          <ArrowDownToLine className="size-3" aria-hidden /> RX {formatBytes(rx)}
        </span>
        <span className="inline-flex items-center gap-1 text-xs text-info" title="Sent bytes">
          <ArrowUpFromLine className="size-3" aria-hidden /> TX {formatBytes(tx)}
        </span>
        <div className="ml-auto flex items-center gap-1">
          <Button variant="ghost" size="xs" onClick={() => clearHex(sessionId)} title="Clear the capture">
            <Eraser className="size-3.5" /> Clear
          </Button>
          <Button variant="ghost" size="icon-sm" onClick={closeHexMonitor} aria-label="Close hex monitor">
            <X className="size-4" />
          </Button>
        </div>
      </div>

      <div
        ref={parentRef}
        className="flex-1 overflow-auto font-mono text-xs leading-5"
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
        }}
      >
        {rows.length === 0 ? (
          <div className="flex h-full items-center justify-center px-4 text-center text-muted-foreground">
            No traffic captured yet — received bytes show in green, sent bytes in blue.
          </div>
        ) : (
          <div style={{ height: rowVirtualizer.getTotalSize(), position: 'relative', width: '100%' }}>
            {rowVirtualizer.getVirtualItems().map((vi) => {
              const row = rows[vi.index]
              return (
                <div
                  key={vi.key}
                  className="absolute left-0 flex w-full gap-3 px-3 whitespace-pre"
                  style={{ top: 0, transform: `translateY(${vi.start}px)`, height: vi.size }}
                >
                  <span className="text-muted-foreground/70 select-none" title={new Date(row.ts).toLocaleString()}>
                    {timeOf(row.ts)}
                  </span>
                  <span className="text-muted-foreground/60 select-none">{row.offset.toString(16).padStart(8, '0')}</span>
                  <span>
                    {Array.from({ length: COLS }, (_, i) =>
                      i < row.bytes.length ? (
                        <span key={i} className={row.rx[i] ? 'text-success' : 'text-info'}>
                          {hex2(row.bytes[i])}{' '}
                        </span>
                      ) : (
                        <span key={i}>{'   '}</span>
                      ),
                    )}
                  </span>
                  <span>
                    {row.bytes.map((b, i) => (
                      <span key={i} className={cn(row.rx[i] ? 'text-success' : 'text-info')}>
                        {ascii(b)}
                      </span>
                    ))}
                  </span>
                </div>
              )
            })}
          </div>
        )}
      </div>

      <form
        className="flex items-center gap-2 border-t px-3 py-1.5"
        onSubmit={(e) => {
          e.preventDefault()
          send()
        }}
      >
        <Input
          value={sendText}
          onChange={(e) => {
            setSendText(e.target.value)
            setSendError(null)
          }}
          placeholder="Send hex — e.g. 01 03 00 00 00 0A C5 CD"
          className="h-7 flex-1 font-mono text-xs"
          spellCheck={false}
          autoComplete="off"
          aria-label="Hex bytes to send"
          aria-invalid={!!sendError}
        />
        <label className="flex items-center gap-1 text-xs text-muted-foreground select-none">
          <input type="checkbox" checked={appendCr} onChange={(e) => setAppendCr(e.target.checked)} className="accent-primary" />
          +CR
        </label>
        <Button type="submit" size="xs" disabled={sendText.trim() === '' || !term}>
          <SendHorizonal className="size-3.5" /> Send
        </Button>
      </form>
      {sendError && (
        <div role="alert" className="border-t border-destructive/30 bg-destructive/10 px-3 py-1 text-xs text-destructive">
          {sendError}
        </div>
      )}
    </section>
  )
}
