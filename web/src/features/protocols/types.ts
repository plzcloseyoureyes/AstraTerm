/*
 * Feature-local types for the protocols module (SPEC §10: extra types live next to the feature). The shared JSON
 * types (Connection, DockerContainer, …) come from @/api/types.
 */

/** Serial flow-control modes (connection option `flowControl`). */
type SerialFlowControl = 'none' | 'rtscts' | 'xonxoff' | 'dsrdtr'

/** Modem status + output lines of a live serial session (GET /api/sessions/:id/serial/status). */
export interface SerialStatus {
  device: string
  dtr: boolean
  rts: boolean
  cts: boolean
  dsr: boolean
  dcd: boolean
  ri: boolean
  /** Active flow control: RTS (rtscts) or DTR (dsrdtr) are then driven by the driver and cannot be toggled. */
  flowControl: SerialFlowControl
}

/** One auto-baud probe attempt. */
interface AutoBaudAttempt {
  baud: number
  score: number
  bytes: number
}

/** Result of POST /api/serial/autobaud (CC-18). */
export interface AutoBaudResult {
  baud: number
  score: number
  sample: string
  tried: AutoBaudAttempt[]
}

/** One entry of GET /api/kube/namespaces. */
export interface KubeNamespace {
  name: string
  status?: string
}

/** Result of POST /api/ipmi/:connectionId/power. */
export interface IpmiPowerResult {
  action: string
  powerOn: boolean
}

/** One captured chunk of serial/raw traffic for the hex monitor (PROTO-12). */
export interface HexEvent {
  /** true = received from the remote (RX), false = sent by the user (TX). */
  rx: boolean
  /** Epoch milliseconds. */
  ts: number
  data: Uint8Array
}
