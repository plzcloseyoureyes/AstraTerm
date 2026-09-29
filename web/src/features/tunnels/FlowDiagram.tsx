/*
 * Live flow diagram of a tunnel definition (TUN-1): clients → listener (on this machine or on the SSH server) ═ SSH ═
 * exit machine → destination. Plain SVG themed through Tailwind fill/stroke utilities, redrawn as the editor changes.
 */
import { useId } from 'react'
import { AppWindow, Globe, Laptop, Lock, Server, TriangleAlert, Users } from 'lucide-react'
import { cn } from '@/lib/utils'
import { KINDS } from './model'
import type { TunnelKind } from './types'

export type DiagramFocus = 'listen' | 'dest' | 'server' | null

export interface FlowDiagramProps {
  kind: TunnelKind
  /** Listen address / socket path. */
  listen: string
  /** Destination label. */
  dest: string
  /** SSH server label (user@host). */
  server: string
  /** Jump hosts / proxy the SSH connection goes through ("bastion → jump2"). */
  via?: string
  focus?: DiagramFocus
  /** The listener is reachable from other machines. */
  exposed?: boolean
  className?: string
}

function clip(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s
}

const W = 720
const H = 150

export function FlowDiagram({ kind, listen, dest, server, via, focus, exposed, className }: FlowDiagramProps) {
  const uid = useId().replace(/:/g, '')
  const onServer = KINDS[kind].listenOn === 'server'
  const proxy = kind === 'dynamic' || kind === 'rdynamic'
  const hostTitle = 'This machine'
  const hostSub = 'AstraTerm host'
  const serverTitle = 'SSH server'
  const entry = onServer ? { title: serverTitle, sub: server } : { title: hostTitle, sub: hostSub }
  const exit = onServer ? { title: hostTitle, sub: hostSub } : { title: serverTitle, sub: server }
  const clients = onServer
    ? { title: 'Remote clients', sub: proxy ? 'SOCKS / HTTP clients' : 'on the server’s network' }
    : { title: 'Your apps', sub: proxy ? 'SOCKS / HTTP clients' : 'browser, DB client…' }
  const destSub = proxy ? (onServer ? 'resolved on this machine' : 'resolved by the server') : onServer ? 'reached from this machine' : 'reached from the server'

  const hot = 'fill-primary/12 stroke-primary'
  const box = 'fill-card stroke-border'
  const text = 'fill-foreground'
  const sub = 'fill-muted-foreground'
  const arrow = `url(#arrow-${uid})`

  // Geometry.
  const cx = { x: 6, y: 45, w: 116, h: 60 }
  const en = { x: 146, y: 14, w: 196, h: 122 }
  const ex = { x: 416, y: 14, w: 150, h: 122 }
  const de = { x: 596, y: 45, w: 118, h: 60 }
  const pill = { x: en.x + 14, y: 72, w: en.w - 28, h: 40 }
  const serverBox = onServer ? en : ex

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={`${KINDS[kind].label} forwarding: ${clients.title} connect to ${listen} on ${entry.title.toLowerCase()}, carried over SSH${via ? ` (via ${via})` : ''} to ${exit.title.toLowerCase()}, then to ${dest}`}
      className={cn('h-auto w-full select-none text-[11px]', className)}
    >
      <defs>
        <marker id={`arrow-${uid}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M0,0 L10,5 L0,10 z" className="fill-muted-foreground" />
        </marker>
      </defs>

      {/* clients */}
      <rect x={cx.x} y={cx.y} width={cx.w} height={cx.h} rx={8} className={box} strokeWidth={1} />
      {onServer ? (
        <Users x={cx.x + 10} y={cx.y + 12} width={16} height={16} className="text-muted-foreground" />
      ) : (
        <AppWindow x={cx.x + 10} y={cx.y + 12} width={16} height={16} className="text-muted-foreground" />
      )}
      <text x={cx.x + 32} y={cx.y + 24} className={cn(text, 'font-medium')}>
        {clients.title}
      </text>
      <text x={cx.x + 10} y={cx.y + 44} className={sub} fontSize={10}>
        {clip(clients.sub, 20)}
      </text>

      {/* entry machine with the listener */}
      <rect
        x={en.x}
        y={en.y}
        width={en.w}
        height={en.h}
        rx={10}
        className={cn(onServer && focus === 'server' ? hot : box)}
        strokeWidth={onServer && focus === 'server' ? 1.6 : 1}
      />
      {onServer ? (
        <Server x={en.x + 12} y={en.y + 12} width={15} height={15} className="text-muted-foreground" />
      ) : (
        <Laptop x={en.x + 12} y={en.y + 12} width={15} height={15} className="text-muted-foreground" />
      )}
      <text x={en.x + 34} y={en.y + 23} className={cn(text, 'font-medium')}>
        {entry.title}
      </text>
      <text x={en.x + 12} y={en.y + 44} className={sub} fontSize={10}>
        {clip(entry.sub, 32)}
      </text>
      <rect
        x={pill.x}
        y={pill.y}
        width={pill.w}
        height={pill.h}
        rx={7}
        className={focus === 'listen' ? hot : 'fill-primary/6 stroke-primary/50'}
        strokeWidth={focus === 'listen' ? 1.6 : 1}
      />
      <text x={pill.x + 10} y={pill.y + 15} className={sub} fontSize={9.5}>
        {proxy ? 'SOCKS / HTTP proxy on' : 'listens on'}
      </text>
      <text x={pill.x + 10} y={pill.y + 31} className={cn(text, 'font-mono')} fontSize={11}>
        {clip(listen || '—', 26)}
      </text>
      {exposed && (
        <g>
          <title>Reachable from other machines</title>
          <TriangleAlert x={pill.x + pill.w - 20} y={pill.y + 4} width={14} height={14} className="text-warning" />
        </g>
      )}

      {/* clients → listener */}
      <line x1={cx.x + cx.w} y1={cx.y + cx.h / 2} x2={pill.x - 2} y2={pill.y + pill.h / 2} className="stroke-muted-foreground" strokeWidth={1.3} markerEnd={arrow} />

      {/* SSH pipe */}
      <line x1={en.x + en.w} y1={pill.y + pill.h / 2 - 3} x2={ex.x - 2} y2={pill.y + pill.h / 2 - 3} className="stroke-success" strokeWidth={1.4} />
      <line x1={en.x + en.w} y1={pill.y + pill.h / 2 + 3} x2={ex.x - 2} y2={pill.y + pill.h / 2 + 3} className="stroke-success" strokeWidth={1.4} markerEnd={arrow} />
      <line x1={pill.x + pill.w} y1={pill.y + pill.h / 2} x2={en.x + en.w} y2={pill.y + pill.h / 2} className="stroke-success/70" strokeWidth={1.3} strokeDasharray="3 3" />
      <Lock x={(en.x + en.w + ex.x) / 2 - 7} y={pill.y + pill.h / 2 - 24} width={14} height={14} className="text-success" />
      <text x={(en.x + en.w + ex.x) / 2} y={pill.y + pill.h / 2 + 20} textAnchor="middle" className="fill-success" fontSize={10}>
        SSH
      </text>
      {via && (
        <text x={(en.x + en.w + ex.x) / 2} y={pill.y + pill.h / 2 + 33} textAnchor="middle" className={sub} fontSize={9}>
          <title>{`Through ${via}`}</title>
          {clip(`via ${via}`, 14)}
        </text>
      )}

      {/* exit machine */}
      <rect
        x={ex.x}
        y={ex.y}
        width={ex.w}
        height={ex.h}
        rx={10}
        className={cn(!onServer && focus === 'server' ? hot : box)}
        strokeWidth={!onServer && focus === 'server' ? 1.6 : 1}
      />
      {onServer ? (
        <Laptop x={ex.x + 12} y={ex.y + 12} width={15} height={15} className="text-muted-foreground" />
      ) : (
        <Server x={ex.x + 12} y={ex.y + 12} width={15} height={15} className="text-muted-foreground" />
      )}
      <text x={ex.x + 34} y={ex.y + 23} className={cn(text, 'font-medium')}>
        {exit.title}
      </text>
      <text x={ex.x + 12} y={ex.y + 44} className={sub} fontSize={10}>
        {clip(exit.sub, 24)}
      </text>
      <text x={ex.x + 12} y={ex.y + 96} className={sub} fontSize={9.5}>
        {proxy ? 'opens each requested' : 'opens the connection'}
      </text>
      <text x={ex.x + 12} y={ex.y + 109} className={sub} fontSize={9.5}>
        {proxy ? 'connection' : 'to the destination'}
      </text>

      {/* exit → destination */}
      <line x1={ex.x + ex.w} y1={de.y + de.h / 2} x2={de.x - 2} y2={de.y + de.h / 2} className="stroke-muted-foreground" strokeWidth={1.3} markerEnd={arrow} />

      {/* destination */}
      <rect
        x={de.x}
        y={de.y}
        width={de.w}
        height={de.h}
        rx={8}
        className={focus === 'dest' ? hot : box}
        strokeWidth={focus === 'dest' ? 1.6 : 1}
        strokeDasharray={proxy ? '4 3' : undefined}
      />
      <Globe x={de.x + 10} y={de.y + 12} width={15} height={15} className="text-muted-foreground" />
      <text x={de.x + 31} y={de.y + 24} className={cn(text, 'font-medium')}>
        Destination
      </text>
      <text x={de.x + 10} y={de.y + 41} className={cn(text, 'font-mono')} fontSize={10.5}>
        {clip(dest || '—', 18)}
      </text>
      <text x={de.x + 10} y={de.y + 54} className={sub} fontSize={9}>
        {clip(destSub, 22)}
      </text>
      {/* the SSH server box is highlighted when the connection field has focus */}
      {focus === 'server' && <rect x={serverBox.x - 3} y={serverBox.y - 3} width={serverBox.w + 6} height={serverBox.h + 6} rx={12} className="fill-none stroke-primary/40" strokeWidth={1} />}
    </svg>
  )
}
