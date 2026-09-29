/*
 * REST wrappers for the protocols module's own backend endpoints (internal/proto/{serial,docker,kube,ipmi}).
 */
import { api, seg } from '@/api/client'
import type { DockerContainer } from '@/api/types'
import type { AutoBaudResult, IpmiPowerResult, KubeNamespace, SerialStatus } from './types'

// --- serial (internal/proto/serial) ----------------------------------------------------------------------------------

export const getSerialStatus = (sessionId: string) => api.get<SerialStatus>(`/api/sessions/${seg(sessionId)}/serial/status`)

export const setSerialLines = (sessionId: string, lines: { dtr?: boolean; rts?: boolean }) =>
  api.post<SerialStatus>(`/api/sessions/${seg(sessionId)}/serial`, lines)

/** Probe baud rates on a free port; `probe` sends Enter at each rate so a silent console prints a prompt. */
export const autobaudSerial = (device: string, opts: { rates?: number[]; probe?: boolean } = {}) =>
  api.post<AutoBaudResult>('/api/serial/autobaud', { device, rates: opts.rates, probe: opts.probe || undefined })

// --- docker (internal/proto/docker) -----------------------------------------------------------------------------------

export interface DockerScope {
  host?: string
  connectionId?: string
}

export const listDockerContainers = (scope: DockerScope = {}, all = true) =>
  api.get<DockerContainer[]>('/api/docker/containers', {
    query: { host: scope.host || undefined, connectionId: scope.connectionId || undefined, all: all ? 1 : undefined },
  })

export const dockerAction = (id: string, action: 'start' | 'stop' | 'restart', scope: DockerScope = {}) =>
  api.post<void>(`/api/docker/containers/${seg(id)}/${action}`, undefined, {
    query: { host: scope.host || undefined, connectionId: scope.connectionId || undefined },
  })

// --- kube (internal/proto/kube) ---------------------------------------------------------------------------------------

export const listKubeNamespaces = (context?: string) =>
  api.get<KubeNamespace[]>('/api/kube/namespaces', { query: { context: context || undefined } })

// --- ipmi (internal/proto/ipmi) ---------------------------------------------------------------------------------------

export type IpmiPowerAction = 'status' | 'on' | 'off' | 'cycle' | 'reset' | 'soft'

export const ipmiPower = (connectionId: string, action: IpmiPowerAction) =>
  api.post<IpmiPowerResult>(`/api/ipmi/${seg(connectionId)}/power`, { action })
