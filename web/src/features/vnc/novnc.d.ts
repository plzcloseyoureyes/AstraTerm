/*
 * Types for @novnc/novnc 1.7 (package entry `core/rfb.js`). @types/novnc__novnc lags at 1.6 and declares the old
 * `@novnc/novnc/lib/rfb` path, so the parts AstraTerm uses are declared here (RESEARCH §3.12).
 */
declare module '@novnc/novnc' {
  export interface RFBCredentials {
    username?: string
    password?: string
    target?: string
  }

  export interface RFBOptions {
    shared?: boolean
    credentials?: RFBCredentials
    repeaterID?: string
    wsProtocols?: string[]
  }

  export interface RFBEventMap {
    connect: CustomEvent<Record<string, never>>
    disconnect: CustomEvent<{ clean: boolean }>
    credentialsrequired: CustomEvent<{ types: Array<'username' | 'password' | 'target'> }>
    securityfailure: CustomEvent<{ status: number; reason?: string }>
    serververification: CustomEvent<{ type: string; publickey: Uint8Array }>
    clipboard: CustomEvent<{ text: string }>
    bell: CustomEvent<Record<string, never>>
    desktopname: CustomEvent<{ name: string }>
    capabilities: CustomEvent<{ capabilities: { power: boolean } }>
    clippingviewport: CustomEvent<boolean>
  }

  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, urlOrChannel: string | WebSocket, options?: RFBOptions)

    viewOnly: boolean
    focusOnClick: boolean
    clipViewport: boolean
    dragViewport: boolean
    scaleViewport: boolean
    resizeSession: boolean
    showDotCursor: boolean
    background: string
    qualityLevel: number
    compressionLevel: number
    readonly capabilities: { power: boolean }
    readonly clippingViewport: boolean

    addEventListener<K extends keyof RFBEventMap>(type: K, listener: (ev: RFBEventMap[K]) => void): void
    removeEventListener<K extends keyof RFBEventMap>(type: K, listener: (ev: RFBEventMap[K]) => void): void

    disconnect(): void
    sendCredentials(credentials: RFBCredentials): void
    approveServer(): void
    sendKey(keysym: number, code: string | null, down?: boolean): void
    sendCtrlAltDel(): void
    focus(options?: FocusOptions): void
    blur(): void
    machineShutdown(): void
    machineReboot(): void
    machineReset(): void
    clipboardPasteFrom(text: string): void
    getImageData(): ImageData
    toDataURL(type?: string, encoderOptions?: number): string
    toBlob(callback: (blob: Blob | null) => void, type?: string, quality?: number): void
  }
}
