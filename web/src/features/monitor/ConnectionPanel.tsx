/*
 * SSH connection details (SSH-38) and connection health (MON-4): negotiated algorithms (post-quantum badge), host key,
 * route, and a latency graph that measures NexTerm → server (keepalive round trip) and browser → NexTerm separately,
 * with stall detection.
 */
import { useEffect, useRef, useState } from 'react'
import { Activity, BadgeCheck, Copy, KeyRound, Link2, Network, RefreshCw, Repeat, ShieldCheck, TriangleAlert, Waypoints } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { LoadingPane } from '@/components/ui/spinner'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { copyText, errorMessage, formatCalendarTime, formatDuration } from '@/lib/utils'
import { useNow } from '@/lib/hooks'
import { getSSHInfo } from './api'
import { InfoRows, Section } from './components'
import { bytes, compactRate, rate } from './format'
import { TimeChart } from './TimeChart'
import type { SSHInfo, TargetId } from './types'

interface Point {
  t: number
  server: number // ms, NaN = stall
  browser: number
  /** Terminal traffic since the previous measurement (bytes/s, NaN = unknown). */
  rx: number
  tx: number
}

const WINDOW_MS = 5 * 60_000

function stats(values: number[]) {
  const v = values.filter((x) => Number.isFinite(x))
  if (!v.length) return null
  const min = Math.min(...v)
  const max = Math.max(...v)
  const avg = v.reduce((a, b) => a + b, 0) / v.length
  let jitter = 0
  for (let i = 1; i < v.length; i++) jitter += Math.abs(v[i] - v[i - 1])
  jitter = v.length > 1 ? jitter / (v.length - 1) : 0
  return { min, max, avg, jitter, last: v[v.length - 1] }
}

const ms = (v: number | undefined) => (v == null || !Number.isFinite(v) ? '—' : v < 10 ? `${v.toFixed(1)} ms` : `${Math.round(v)} ms`)

export function ConnectionPanel({ target, active }: { target: TargetId; active: boolean }) {
  const [info, setInfo] = useState<SSHInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [points, setPoints] = useState<Point[]>([])
  const [stalls, setStalls] = useState(0)
  const firstLoad = useLoadingGate(!info && !error)
  const busy = useRef(false)
  const lastBytes = useRef<{ t: number; in: number; out: number } | null>(null)
  const now = useNow(1_000)

  useEffect(() => {
    if (!active) return
    let alive = true
    const tick = async () => {
      if (busy.current) return
      busy.current = true
      const t0 = performance.now()
      try {
        const r = await getSSHInfo(target)
        if (!alive) return
        const total = performance.now() - t0
        const server = r.probeMs >= 0 ? r.probeMs : NaN
        const browser = Math.max(0, total - (r.probeMs >= 0 ? r.probeMs : 0))
        const t = Date.now()
        // Terminal throughput from the byte counters (a counter going backwards means a new session: no rate).
        const prev = lastBytes.current
        const dt = prev ? (t - prev.t) / 1000 : 0
        const rx = prev && dt > 0 && r.termBytesIn >= prev.in ? (r.termBytesIn - prev.in) / dt : NaN
        const tx = prev && dt > 0 && r.termBytesOut >= prev.out ? (r.termBytesOut - prev.out) / dt : NaN
        lastBytes.current = { t, in: r.termBytesIn ?? 0, out: r.termBytesOut ?? 0 }
        setInfo(r)
        setError(null)
        if (!Number.isFinite(server)) setStalls((n) => n + 1)
        setPoints((p) => [...p.filter((x) => x.t >= t - WINDOW_MS), { t, server, browser, rx, tx }])
      } catch (err) {
        if (alive) setError(errorMessage(err))
      } finally {
        busy.current = false
      }
    }
    lastBytes.current = null
    void tick()
    const id = setInterval(() => void tick(), 2_000)
    return () => {
      alive = false
      clearInterval(id)
    }
  }, [target, active])

  if (!info && error) {
    return <EmptyState icon={TriangleAlert} title="Connection details unavailable" description={error} />
  }
  if (!info || firstLoad.hold) return firstLoad.show ? <LoadingPane immediate label="Measuring the connection…" /> : null

  const srv = stats(points.map((p) => p.server))
  const lastTraffic = [...points].reverse().find((p) => Number.isFinite(p.rx))
  const brw = stats(points.map((p) => p.browser))
  const last = points[points.length - 1]
  const stalled = last && !Number.isFinite(last.server)
  const connectedFor = info.connectedAt ? now - Date.parse(info.connectedAt) : undefined

  return (
    <div className="grid min-h-0 flex-1 auto-rows-min gap-3 overflow-auto p-3 @3xl:grid-cols-2">
      <div className="@3xl:col-span-2">
        <TimeChart
          title="Latency"
          headline={
            stalled ? (
              <span className="flex items-center gap-1 text-destructive">
                <TriangleAlert className="size-4" /> No answer from the server
              </span>
            ) : (
              <span>{ms(last?.server)}</span>
            )
          }
          xs={points.map((p) => p.t)}
          series={[
            { label: 'NexTerm → server', color: '--primary', values: points.map((p) => p.server), fill: true },
            { label: 'Browser → NexTerm', color: '--success', values: points.map((p) => p.browser) },
          ]}
          format={(v) => ms(v)}
          windowMs={WINDOW_MS}
          height={170}
        />
      </div>
      <div className="@3xl:col-span-2">
        <TimeChart
          title="Terminal traffic"
          headline={
            lastTraffic ? (
              <span>
                ↓ {rate(lastTraffic.rx)} ↑ {rate(lastTraffic.tx)}
              </span>
            ) : (
              <span className="text-muted-foreground">measuring…</span>
            )
          }
          xs={points.map((p) => p.t)}
          series={[
            { label: 'Received (output)', color: '--success', values: points.map((p) => p.rx), fill: true },
            { label: 'Sent (input)', color: '--primary', values: points.map((p) => p.tx) },
          ]}
          format={(v) => compactRate(v)}
          windowMs={WINDOW_MS}
          height={120}
        />
      </div>
      <Section title="Round trip" actions={<Activity className="size-4 text-muted-foreground" />}>
        <InfoRows
          rows={[
            ['Server (now)', ms(last?.server)],
            ['Server min / avg / max', srv ? `${ms(srv.min)} / ${ms(srv.avg)} / ${ms(srv.max)}` : '—'],
            ['Server jitter', srv ? ms(srv.jitter) : '—'],
            ['Browser (now)', ms(last?.browser)],
            ['Browser avg', brw ? ms(brw.avg) : '—'],
            ['Keepalive RTT', info.latencyMs ? ms(info.latencyMs) : '—'],
            ['Stalls (> 5 s)', stalls ? <span className="text-destructive">{stalls}</span> : '0'],
          ]}
        />
      </Section>
      <Section title="Session" actions={<Repeat className="size-4 text-muted-foreground" />}>
        <InfoRows
          rows={[
            ['Reconnects', info.reconnects ? <span className="text-warning">{info.reconnects}</span> : '0'],
            ['First connected', info.firstConnectedAt ? formatCalendarTime(info.firstConnectedAt) : '—'],
            [
              'Last disconnect',
              info.lastDisconnectAt ? (
                <span>
                  {formatCalendarTime(info.lastDisconnectAt)}
                  {info.lastDisconnect ? <span className="text-muted-foreground"> — {info.lastDisconnect}</span> : null}
                </span>
              ) : (
                'never'
              ),
            ],
            ['Terminal received', bytes(info.termBytesIn)],
            ['Terminal sent', bytes(info.termBytesOut)],
          ]}
        />
        <p className="text-xs text-muted-foreground">Traffic of the terminal channel only (not SFTP, port forwards or monitoring).</p>
      </Section>
      <Section
        title="Security"
        actions={
          info.pq ? (
            <Badge variant="success" title="Hybrid post-quantum key exchange">
              <ShieldCheck /> Post-quantum
            </Badge>
          ) : (
            <Badge variant="secondary">Classic key exchange</Badge>
          )
        }
      >
        <InfoRows
          rows={[
            ['Key exchange', <code className="text-xs">{info.kex || '—'}</code>],
            ['Cipher', <code className="text-xs">{info.cipher || '—'}</code>],
            ['MAC', <code className="text-xs">{info.mac || (info.cipher?.includes('gcm') || info.cipher?.includes('poly1305') ? '(AEAD cipher)' : '—')}</code>],
            [
              'Host key',
              <span className="flex flex-wrap items-center gap-1">
                <code className="text-xs">{info.hostKeyAlgo || '—'}</code>
                {info.hostCert && (
                  <Badge variant="outline" title="The server presented an OpenSSH host certificate">
                    <BadgeCheck /> certificate
                  </Badge>
                )}
              </span>,
            ],
            [
              'Fingerprint',
              info.hostKeyFingerprint ? (
                <span className="flex items-center gap-1">
                  <code className="min-w-0 truncate text-xs" title={info.hostKeyFingerprint}>
                    {info.hostKeyFingerprint}
                  </code>
                  <IconButton icon={Copy} label="Copy fingerprint" size="xs" onClick={() => void copyText(info.hostKeyFingerprint)} />
                </span>
              ) : (
                '—'
              ),
            ],
            ['Compression', info.compression ? 'on' : 'off'],
          ]}
        />
      </Section>
      <Section title="Endpoints" actions={<Network className="size-4 text-muted-foreground" />}>
        <InfoRows
          rows={[
            ['Server', <code className="text-xs">{info.serverVersion || '—'}</code>],
            ['Client', <code className="text-xs">{info.clientVersion || '—'}</code>],
            ['Target', info.host ? `${info.user ? `${info.user}@` : ''}${info.host}:${info.port ?? 22}` : '—'],
            ['Remote address', info.remoteAddr || '—'],
            ['Local address', info.localAddr || '—'],
            ['Connected', connectedFor != null ? `${formatDuration(connectedFor)} ago (${formatCalendarTime(info.connectedAt)})` : '—'],
          ]}
        />
      </Section>
      <Section title="Route & options" actions={<Waypoints className="size-4 text-muted-foreground" />}>
        <InfoRows
          rows={[
            [
              'Jump hosts',
              info.jumpHosts?.length ? (
                <span className="flex flex-wrap items-center gap-1">
                  {info.jumpHosts.map((h, i) => (
                    <span key={i} className="flex items-center gap-1">
                      {i > 0 && <Link2 className="size-3 text-muted-foreground" />}
                      <Badge variant="outline">{h}</Badge>
                    </span>
                  ))}
                  <Link2 className="size-3 text-muted-foreground" />
                  <Badge variant="default">{info.host}</Badge>
                </span>
              ) : (
                'direct'
              ),
            ],
            ['Proxy', info.proxy || 'none'],
            ['Authentication', info.authMethod || 'auto'],
            ['Agent forwarding', info.agentForwarding ? 'on' : 'off'],
            ['X11 forwarding', info.x11Forwarding ? 'on' : 'off'],
            ['Keepalive', info.keepAliveSec ? `every ${info.keepAliveSec} s` : 'off'],
          ]}
        />
      </Section>
      {error && (
        <div className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-1.5 text-xs text-warning @3xl:col-span-2">
          <KeyRound className="size-3.5" /> Last measurement failed: {error}
          <Button size="xs" variant="ghost" className="ml-auto" onClick={() => setError(null)}>
            <RefreshCw /> Dismiss
          </Button>
        </div>
      )}
    </div>
  )
}
