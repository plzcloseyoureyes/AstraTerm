/*
 * Built-in protocol metadata (labels, icons, default ports). Protocol editors registered by features can override the
 * icon/label through the protocol-editor registry; `protocolIcon()` prefers those.
 */
import {
  Boxes,
  Cable,
  Cloud,
  Container,
  FolderOpen,
  Globe,
  Monitor,
  MonitorSmartphone,
  Network,
  Plug,
  Radio,
  Server,
  SquareTerminal,
  Terminal,
  Usb,
  Waypoints,
  Workflow,
} from 'lucide-react'
import type { Protocol } from '@/api/types'
import { protocolEditors, type IconType } from './registry'

export interface ProtocolInfo {
  label: string
  icon: IconType
  defaultPort: number
  group: 'terminal' | 'files' | 'graphical' | 'other'
}

export const PROTOCOL_INFO: Record<Protocol, ProtocolInfo> = {
  ssh: { label: 'SSH', icon: Terminal, defaultPort: 22, group: 'terminal' },
  telnet: { label: 'Telnet', icon: Cable, defaultPort: 23, group: 'terminal' },
  rlogin: { label: 'Rlogin', icon: Cable, defaultPort: 513, group: 'terminal' },
  raw: { label: 'Raw TCP', icon: Plug, defaultPort: 0, group: 'terminal' },
  serial: { label: 'Serial', icon: Usb, defaultPort: 0, group: 'terminal' },
  local: { label: 'Local shell', icon: SquareTerminal, defaultPort: 0, group: 'terminal' },
  mosh: { label: 'Mosh', icon: Radio, defaultPort: 22, group: 'terminal' },
  docker: { label: 'Docker', icon: Container, defaultPort: 0, group: 'terminal' },
  kube: { label: 'Kubernetes', icon: Boxes, defaultPort: 0, group: 'terminal' },
  sftp: { label: 'SFTP', icon: FolderOpen, defaultPort: 22, group: 'files' },
  ftp: { label: 'FTP', icon: Network, defaultPort: 21, group: 'files' },
  s3: { label: 'S3', icon: Cloud, defaultPort: 443, group: 'files' },
  vnc: { label: 'VNC', icon: MonitorSmartphone, defaultPort: 5900, group: 'graphical' },
  rdp: { label: 'RDP', icon: Monitor, defaultPort: 3389, group: 'graphical' },
  winrm: { label: 'WinRM', icon: Workflow, defaultPort: 5985, group: 'terminal' },
  ipmi: { label: 'IPMI SOL', icon: Server, defaultPort: 623, group: 'terminal' },
  web: { label: 'Web', icon: Globe, defaultPort: 443, group: 'other' },
}

/** Icon for a protocol (registered editor icon first, then the built-in table). */
export function protocolIcon(protocol: string | undefined | null): IconType {
  if (!protocol) return Waypoints
  return protocolEditors.get(protocol)?.icon ?? PROTOCOL_INFO[protocol as Protocol]?.icon ?? Waypoints
}

export function protocolLabel(protocol: string | undefined | null): string {
  if (!protocol) return ''
  return protocolEditors.get(protocol)?.label ?? PROTOCOL_INFO[protocol as Protocol]?.label ?? protocol.toUpperCase()
}

export function defaultPort(protocol: string): number {
  return protocolEditors.get(protocol)?.defaultPort ?? PROTOCOL_INFO[protocol as Protocol]?.defaultPort ?? 0
}
