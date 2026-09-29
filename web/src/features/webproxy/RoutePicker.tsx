/* "Reach it through" picker: the Termstead host (direct), a live SSH session, or a saved SSH connection. */
import { useMemo, type ReactNode } from 'react'
import { Server, TerminalSquare, Waypoints } from 'lucide-react'
import { useConnections } from '@/api/connections'
import { useSessions } from '@/api/sessions'
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectSeparator, SelectTrigger, SelectValue } from '@/components/ui/select'

/** 'direct' | `s:<sessionId>` | `c:<connectionId>` | `t:<tunnelId>` (tunnels are shown only when preselected). */
export type RouteValue = string

export function routeToSpec(v: RouteValue): { sessionId?: string; connectionId?: string; tunnelId?: string } {
  if (v.startsWith('s:')) return { sessionId: v.slice(2) }
  if (v.startsWith('c:')) return { connectionId: v.slice(2) }
  if (v.startsWith('t:')) return { tunnelId: v.slice(2) }
  return {}
}

export function specToRoute(s: { sessionId?: string; connectionId?: string; tunnelId?: string }): RouteValue {
  if (s.tunnelId) return `t:${s.tunnelId}`
  if (s.sessionId) return `s:${s.sessionId}`
  if (s.connectionId) return `c:${s.connectionId}`
  return 'direct'
}

const SSH_FAMILY = new Set(['ssh', 'sftp', 'mosh'])

/** One option: icon, label and a muted detail, laid out on one line (also inside the trigger). */
function Row({ icon, children, detail }: { icon: ReactNode; children: ReactNode; detail?: string }) {
  return (
    <span className="inline-flex max-w-full min-w-0 items-center gap-2 align-middle">
      {icon}
      <span className="truncate">
        {children}
        {detail && <span className="text-muted-foreground"> — {detail}</span>}
      </span>
    </span>
  )
}

export function RoutePicker({
  value,
  onChange,
  allowDirect = true,
  id,
  tunnelLabel,
  'aria-label': ariaLabel,
}: {
  value: RouteValue
  onChange: (v: RouteValue) => void
  allowDirect?: boolean
  /** Id of the trigger: a <label htmlFor> (Field) names it; otherwise pass aria-label. */
  id?: string
  tunnelLabel?: string
  'aria-label'?: string
}) {
  const sessions = useSessions()
  const connections = useConnections()
  const live = useMemo(
    () => (sessions.data ?? []).filter((s) => s.protocol === 'ssh' && s.state === 'connected').sort((a, b) => a.title.localeCompare(b.title)),
    [sessions.data],
  )
  const saved = useMemo(
    () => (connections.data ?? []).filter((c) => SSH_FAMILY.has(c.protocol)).sort((a, b) => a.name.localeCompare(b.name)),
    [connections.data],
  )
  const empty = !allowDirect && !live.length && !saved.length && !value.startsWith('t:')
  return (
    <Select value={value || undefined} onValueChange={onChange}>
      <SelectTrigger id={id} aria-label={ariaLabel}>
        <SelectValue placeholder={empty ? 'No SSH connection yet' : 'Choose…'} />
      </SelectTrigger>
      <SelectContent>
        {allowDirect && (
          <SelectItem value="direct">
            <Row icon={<Server className="size-3.5 text-muted-foreground" />}>This Termstead host (direct)</Row>
          </SelectItem>
        )}
        {value.startsWith('t:') && (
          <SelectItem value={value}>
            <Row icon={<Waypoints className="size-3.5 text-muted-foreground" />}>{tunnelLabel ?? 'Tunnel'}</Row>
          </SelectItem>
        )}
        {live.length > 0 && (
          <>
            {allowDirect && <SelectSeparator />}
            <SelectGroup>
              <SelectLabel>Open SSH sessions</SelectLabel>
              {live.map((s) => (
                <SelectItem key={s.id} value={`s:${s.id}`}>
                  <Row icon={<TerminalSquare className="size-3.5 text-muted-foreground" />} detail={s.host ? `${s.username ? `${s.username}@` : ''}${s.host}` : undefined}>
                    {s.title}
                  </Row>
                </SelectItem>
              ))}
            </SelectGroup>
          </>
        )}
        {saved.length > 0 && (
          <>
            {(allowDirect || live.length > 0 || value.startsWith('t:')) && <SelectSeparator />}
            <SelectGroup>
              <SelectLabel>Saved SSH connections</SelectLabel>
              {saved.map((c) => (
                <SelectItem key={c.id} value={`c:${c.id}`}>
                  <Row icon={<Waypoints className="size-3.5 text-muted-foreground" />} detail={c.host || undefined}>
                    {c.name}
                  </Row>
                </SelectItem>
              ))}
            </SelectGroup>
          </>
        )}
      </SelectContent>
    </Select>
  )
}
