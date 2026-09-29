/*
 * File system picker (commander panes, folder compare): the local host, open SSH sessions, saved file connections.
 */
import { Check, ChevronDown, HardDrive } from 'lucide-react'
import { useConnections } from '@/api/connections'
import { useSessions } from '@/api/sessions'
import type { Connection, RuntimeSession } from '@/api/types'
import { protocolIcon } from '@/app/protocols'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import type { FsSource } from '../types'

// ---------------------------------------------------------------------------------------------------------------------
// source picker (any file system: local host, open SSH sessions, saved file connections)
// ---------------------------------------------------------------------------------------------------------------------

export function SourcePicker({ source, onPick }: { source: FsSource; onPick: (s: FsSource) => void }) {
  const sessions = useSessions()
  const conns = useConnections()
  const live = (sessions.data ?? []).filter((s: RuntimeSession) => s.protocol === 'ssh' && s.state === 'connected')
  const saved = (conns.data ?? []).filter((c: Connection) => ['sftp', 'ftp', 's3', 'ssh'].includes(c.protocol)).sort((a, b) => a.name.localeCompare(b.name))
  const current = labelOf(source, sessions.data, conns.data)
  const Icon = source.kind === 'local' ? HardDrive : protocolIcon(current.protocol ?? 'sftp')
  const isCur = (s: FsSource) => JSON.stringify(s) === JSON.stringify(source)
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="xs" className="max-w-64 min-w-0 gap-1 px-1.5 font-medium" aria-label={`File system: ${current.label}. Change`}>
          <Icon className="size-3.5 shrink-0" />
          <span className="truncate">{current.label}</span>
          <ChevronDown className="size-3 shrink-0 opacity-60" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="max-h-96 min-w-60">
        <DropdownMenuItem onSelect={() => onPick({ kind: 'local' })}>
          <HardDrive /> <span className="flex-1">Local host</span>
          {isCur({ kind: 'local' }) && <Check className="size-3.5" />}
        </DropdownMenuItem>
        {live.length > 0 && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuLabel>Open SSH sessions</DropdownMenuLabel>
            {live.map((s) => {
              const src: FsSource = { kind: 'session', sessionId: s.id }
              const SIcon = protocolIcon(s.protocol)
              return (
                <DropdownMenuItem key={s.id} onSelect={() => onPick(src)}>
                  <SIcon /> <span className="flex-1 truncate">{s.title || `${s.username ?? ''}@${s.host ?? ''}`}</span>
                  {isCur(src) && <Check className="size-3.5" />}
                </DropdownMenuItem>
              )
            })}
          </>
        )}
        {saved.length > 0 && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuLabel>Saved sessions</DropdownMenuLabel>
            {saved.slice(0, 80).map((c) => {
              const src: FsSource = { kind: 'connection', connectionId: c.id }
              const CIcon = protocolIcon(c.protocol)
              return (
                <DropdownMenuItem key={c.id} onSelect={() => onPick(src)}>
                  <CIcon /> <span className="flex-1 truncate">{c.name}</span>
                  <span className="text-2xs text-muted-foreground uppercase">{c.protocol}</span>
                  {isCur(src) && <Check className="size-3.5" />}
                </DropdownMenuItem>
              )
            })}
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function labelOf(source: FsSource, sessions: RuntimeSession[] | undefined, conns: Connection[] | undefined): { label: string; protocol?: string } {
  switch (source.kind) {
    case 'local':
      return { label: 'Local host' }
    case 'session': {
      const s = sessions?.find((x) => x.id === source.sessionId)
      return { label: s ? s.title || `${s.username ?? ''}@${s.host ?? ''}` : 'SSH session', protocol: 'ssh' }
    }
    case 'connection': {
      const c = conns?.find((x) => x.id === source.connectionId)
      return { label: c?.name ?? 'Connection', protocol: c?.protocol }
    }
    case 'quick':
      return { label: `${source.quick.username ? `${source.quick.username}@` : ''}${source.quick.host ?? ''}`, protocol: source.quick.protocol }
    case 'handle':
      return { label: 'Files' }
  }
}
