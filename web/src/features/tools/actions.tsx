/*
 * Row actions for discovered hosts / ports (RESEARCH TOOL-4 "one-click create or open a session"): connect through
 * the sessions feature's quick connect, create a saved session, open web ports through the web proxy, or hand a host
 * to the port scanner. Other features' commands are used when registered and hidden otherwise.
 */
import { MoreHorizontal, Plus, Radar, Copy, Plug, Globe } from 'lucide-react'
import { toast } from 'sonner'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { copyText } from '@/lib/utils'
import { openTool } from './navigate'

export interface ServiceTarget {
  label: string
  /** Quick-connect URL scheme (ssh, telnet, rdp, vnc, ftp, winrm, winrms). */
  scheme?: string
  /** Saved-session protocol for "New session". */
  protocol?: string
  /** Web proxy scheme (http / https). */
  web?: 'http' | 'https'
}

const WEB_PORTS: Record<number, 'http' | 'https'> = { 80: 'http', 81: 'http', 8000: 'http', 8008: 'http', 8080: 'http', 8081: 'http', 8888: 'http', 443: 'https', 8443: 'https', 9443: 'https' }

/** What can be opened on host:port. */
export function serviceFor(port: number): ServiceTarget | undefined {
  if (port === 22 || port === 2222) return { label: 'SSH', scheme: 'ssh', protocol: 'ssh' }
  if (port === 23) return { label: 'Telnet', scheme: 'telnet', protocol: 'telnet' }
  if (port === 3389) return { label: 'RDP', scheme: 'rdp', protocol: 'rdp' }
  if (port >= 5900 && port <= 5909) return { label: 'VNC', scheme: 'vnc', protocol: 'vnc' }
  if (port === 21) return { label: 'FTP', scheme: 'ftp', protocol: 'ftp' }
  if (port === 5985) return { label: 'WinRM', scheme: 'winrm', protocol: 'winrm' }
  if (port === 5986) return { label: 'WinRM (HTTPS)', scheme: 'winrms', protocol: 'winrm' }
  if (WEB_PORTS[port]) return { label: WEB_PORTS[port] === 'https' ? 'HTTPS' : 'HTTP', web: WEB_PORTS[port] }
  return undefined
}

function hostPart(host: string): string {
  return host.includes(':') && !host.startsWith('[') ? `[${host}]` : host
}

export async function connectTo(host: string, port: number, svc: ServiceTarget): Promise<void> {
  if (svc.web) {
    if (!(await runCommand('webproxy.open', { host, port, scheme: svc.web }))) toast.error('The web proxy is not available')
    return
  }
  if (!svc.scheme) return
  if (!(await runCommand('sessions.quickConnect', `${svc.scheme}://${hostPart(host)}:${port}`))) toast.error('Quick connect is not available')
}

export async function newSession(host: string, port: number, svc: ServiceTarget, name?: string): Promise<void> {
  if (!(await runCommand('sessions.new', { protocol: svc.protocol, initial: { host, port, name: name || host } }))) toast.error('The session editor is not available')
}

/** A per-row "…" menu for a host (and optionally its open ports). */
export function HostActionsMenu({ host, ports, name }: { host: string; ports: number[]; name?: string }) {
  const services = ports.map((p) => ({ port: p, svc: serviceFor(p) })).filter((x): x is { port: number; svc: ServiceTarget } => !!x.svc)
  const canQuick = isCommandEnabled('sessions.quickConnect')
  const canNew = isCommandEnabled('sessions.new')
  const canWeb = isCommandEnabled('webproxy.open')
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon-xs" aria-label={`Actions for ${host}`}>
          <MoreHorizontal />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {services.length > 0 && <DropdownMenuLabel>Connect</DropdownMenuLabel>}
        {services.map(({ port, svc }) => (
          <DropdownMenuItem key={`c${port}`} disabled={svc.web ? !canWeb : !canQuick} onSelect={() => void connectTo(host, port, svc)}>
            {svc.web ? <Globe /> : <Plug />} {svc.label} ({port})
          </DropdownMenuItem>
        ))}
        {services.some((s) => s.svc.protocol) && canNew && (
          <>
            <DropdownMenuSeparator />
            {services
              .filter((s) => s.svc.protocol)
              .map(({ port, svc }) => (
                <DropdownMenuItem key={`n${port}`} onSelect={() => void newSession(host, port, svc, name)}>
                  <Plus /> New {svc.label} session…
                </DropdownMenuItem>
              ))}
          </>
        )}
        {services.length > 0 && <DropdownMenuSeparator />}
        <DropdownMenuItem onSelect={() => openTool('portscan', { targets: host })}>
          <Radar /> Scan ports…
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => void copyText(host).then((ok) => ok && toast.success('Copied'))}>
          <Copy /> Copy address
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
