/*
 * Types for the parts of zmodem.js 0.1.10 (CommonJS, no bundled types) used by the term-transfer feature.
 */
declare module 'zmodem.js' {
  export type ZmOctets = number[]

  export interface ZmHeader {
    readonly NAME: string
    _bytes4?: number[]
    get_offset?(): number
  }

  export interface ZmFileDetails {
    name: string
    size: number | null
    mtime: Date | null
    mode: number | null
    serial: number | null
    files_remaining: number | null
    bytes_remaining: number | null
  }

  export interface ZmOfferParams {
    name: string
    size?: number
    /** Epoch seconds (or a Date). */
    mtime?: number | Date
    mode?: number
    files_remaining?: number
    bytes_remaining?: number
  }

  export interface ZmOffer {
    get_details(): ZmFileDetails
    get_options(): Record<string, unknown>
    get_offset(): number
    accept(opts?: { offset?: number; on_input?: 'spool_uint8array' | 'spool_array' | ((payload: ZmOctets) => void) }): Promise<unknown>
    skip(): Promise<unknown> | undefined
  }

  export interface ZmTransfer {
    send(bytes: ZmOctets | Uint8Array): void
    end(bytes?: ZmOctets | Uint8Array): Promise<void>
    get_offset(): number
    get_details(): ZmFileDetails
  }

  interface ZmSessionBase {
    has_ended(): boolean
    aborted(): boolean
    abort(): void
    consume(octets: ZmOctets): void
    set_sender(fn: (octets: ZmOctets) => void): this
    on(evt: 'session_end', cb: () => void): this
    on(evt: 'garbage', cb: (octets: ZmOctets) => void): this
    on(evt: 'receive', cb: (hdrOrPacket: unknown) => void): this
  }

  export interface ZmReceiveSession extends ZmSessionBase {
    readonly type: 'receive'
    start(): Promise<ZmOffer | undefined>
    get_trailing_bytes(): ZmOctets
    on(evt: 'offer', cb: (offer: ZmOffer) => void): this
    on(evt: 'session_end', cb: () => void): this
    on(evt: 'garbage', cb: (octets: ZmOctets) => void): this
    on(evt: 'receive', cb: (hdrOrPacket: unknown) => void): this
  }

  export interface ZmSendSession extends ZmSessionBase {
    readonly type: 'send'
    send_offer(params: ZmOfferParams): Promise<ZmTransfer | undefined>
    close(): Promise<void>
  }

  export type ZmSession = ZmReceiveSession | ZmSendSession

  export interface ZmDetection {
    confirm(): ZmSession
    deny(): void
    is_valid(): boolean
    get_session_role(): 'receive' | 'send'
  }

  export interface ZmSentryOptions {
    to_terminal(octets: ZmOctets): void
    sender(octets: ZmOctets): void
    on_detect(detection: ZmDetection): void
    on_retract(): void
  }

  export interface ZmSentry {
    consume(input: ZmOctets | ArrayBuffer | Uint8Array): void
    get_confirmed_session(): ZmSession | null
  }

  export interface ZmodemModule {
    Sentry: new (opts: ZmSentryOptions) => ZmSentry
    ZMLIB: { ABORT_SEQUENCE: number[] }
    DEBUG: boolean
  }

  const Zmodem: ZmodemModule
  export default Zmodem
}
