/*
 * REST + WebSocket endpoints of the monitor module (SPEC §6.0 Monitoring, §9 monitor notes) and react-query hooks.
 * Targets are runtime session ids or 'local' (the Termstead host, desktop mode / admins).
 */
import { useQuery } from '@tanstack/react-query'
import { api, seg, wsUrl } from '@/api/client'
import type {
  CaffeineStatus,
  DiskUsage,
  HostInfo,
  ListeningPort,
  ProcessInfo,
  ServiceAction,
  ServiceList,
  SSHInfo,
  Stats,
  SystemInfo,
  TargetId,
} from './types'

const base = (id: TargetId) => `/api/monitor/${seg(id)}`

export const monitorKeys = {
  all: ['monitor'] as const,
  host: (id: TargetId) => ['monitor', id, 'host'] as const,
  processes: (id: TargetId) => ['monitor', id, 'processes'] as const,
  services: (id: TargetId) => ['monitor', id, 'services'] as const,
  ports: (id: TargetId, sudo: boolean) => ['monitor', id, 'ports', sudo] as const,
  du: (id: TargetId, path: string, sudo: boolean) => ['monitor', id, 'du', path, sudo] as const,
  sshInfo: (id: TargetId) => ['monitor', id, 'ssh-info'] as const,
  systemInfo: ['monitor', 'local', 'system-info'] as const,
  caffeine: ['monitor', 'caffeine'] as const,
}

export const getHost = (id: TargetId) => api.get<HostInfo>(`${base(id)}/host`)
export const getSnapshot = (id: TargetId) => api.get<Stats>(`${base(id)}/snapshot`)
export const getProcesses = (id: TargetId, signal?: AbortSignal) => api.get<ProcessInfo[]>(`${base(id)}/processes`, { signal })
export const killProcess = (id: TargetId, pid: number, signal: string, sudo = false) =>
  api.post<void>(`${base(id)}/kill`, { pid, signal, sudo })
export const reniceProcess = (id: TargetId, pid: number, nice: number, sudo = false) =>
  api.post<void>(`${base(id)}/renice`, { pid, nice, sudo })
export const getServices = (id: TargetId) => api.get<ServiceList>(`${base(id)}/services`)
export const serviceAction = (id: TargetId, name: string, action: ServiceAction, sudo = false) =>
  api.post<void>(`${base(id)}/services/${seg(name)}/${action}`, { sudo })
export const getServiceLogs = (id: TargetId, name: string, lines = 200, sudo = false) =>
  api.get<{ lines: string[] }>(`${base(id)}/services/${seg(name)}/logs`, { query: { lines, sudo: sudo ? 1 : undefined } })
export const getPorts = (id: TargetId, sudo = false) => api.get<ListeningPort[]>(`${base(id)}/ports`, { query: { sudo: sudo ? 1 : undefined } })
export const getDiskUsage = (id: TargetId, path: string, sudo = false) =>
  api.get<DiskUsage>(`${base(id)}/du`, { query: { path, sudo: sudo ? 1 : undefined } })
export const getSSHInfo = (id: TargetId) => api.get<SSHInfo>(`${base(id)}/ssh-info`)
export const getSystemInfo = () => api.get<SystemInfo>('/api/monitor/local')
export const getCaffeine = () => api.get<CaffeineStatus>('/api/system/caffeine')
export const setCaffeine = (enabled: boolean, durationMin = 0) => api.post<CaffeineStatus>('/api/system/caffeine', { enabled, durationMin })

/** WebSocket URL of the log follower. */
export function tailUrl(id: TargetId, opts: { paths?: string[]; journal?: boolean; unit?: string; lines?: number; sudo?: boolean }): string {
  return wsUrl(`/ws/monitor/${seg(id)}/tail`, {
    path: opts.paths?.length ? opts.paths : undefined,
    journal: opts.journal && !opts.unit ? 1 : undefined,
    unit: opts.unit || undefined,
    lines: opts.lines,
    sudo: opts.sudo ? 1 : undefined,
  })
}

// --- hooks -----------------------------------------------------------------------------------------------------------

export function useHost(id: TargetId | undefined, enabled = true) {
  return useQuery({ queryKey: monitorKeys.host(id ?? ''), queryFn: () => getHost(id!), enabled: !!id && enabled, staleTime: 5 * 60_000, placeholderData: undefined }) // never show another host's facts
}

export function useProcesses(id: TargetId, opts: { enabled: boolean; refetchMs: number | false }) {
  return useQuery({
    queryKey: monitorKeys.processes(id),
    queryFn: ({ signal }) => getProcesses(id, signal),
    enabled: opts.enabled,
    refetchInterval: opts.refetchMs,
    refetchIntervalInBackground: false,
    staleTime: 1_000,
    placeholderData: (prev) => prev,
  })
}

export function useServices(id: TargetId, enabled: boolean) {
  return useQuery({ queryKey: monitorKeys.services(id), queryFn: () => getServices(id), enabled, staleTime: 5_000, placeholderData: undefined }) // never show another host's services
}

export function usePorts(id: TargetId, sudo: boolean, enabled: boolean) {
  return useQuery({ queryKey: monitorKeys.ports(id, sudo), queryFn: () => getPorts(id, sudo), enabled, staleTime: 5_000, placeholderData: undefined }) // never show another host's ports
}

export function useDiskUsage(id: TargetId, path: string, sudo: boolean, enabled: boolean) {
  return useQuery({
    queryKey: monitorKeys.du(id, path, sudo),
    queryFn: () => getDiskUsage(id, path, sudo),
    enabled,
    staleTime: 60_000,
    placeholderData: (prev) => prev,
  })
}

export function useSystemInfo(enabled: boolean, refetchMs: number | false = false) {
  return useQuery({
    queryKey: monitorKeys.systemInfo,
    queryFn: getSystemInfo,
    enabled,
    refetchInterval: refetchMs,
    staleTime: 2_000,
    placeholderData: (prev) => prev,
  })
}

export function useCaffeineStatus(enabled = true) {
  return useQuery({ queryKey: monitorKeys.caffeine, queryFn: getCaffeine, enabled, staleTime: 30_000 })
}
