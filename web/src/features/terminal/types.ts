/*
 * Terminal feature types (SPEC §10: feature-specific types live next to the feature).
 *
 * Nothing in here imports xterm at runtime: `Terminal` is a type-only import, so the light modules of this feature
 * (open.ts, bus.ts, commands) can be imported eagerly by other features without pulling the xterm chunk.
 */
import type { Terminal } from '@xterm/xterm'
import type { Connection, Protocol, RuntimeSession, SessionState } from '@/api/types'

/**
 * Quick-connect spec kept in tab params so a tab can be duplicated / reopened. Never contains secrets (tab params are
 * persisted with the layout in localStorage); the backend prompts for missing credentials through the prompt broker.
 */
export type QuickSpec = Partial<
  Pick<Connection, 'name' | 'protocol' | 'host' | 'port' | 'username' | 'identityId' | 'keyId' | 'authMethod' | 'options' | 'color' | 'icon'>
>

/** Params of a dock tab of kind "terminal" (JSON-serialisable, persisted with the layout). */
export interface TerminalTabParams {
  /** Runtime session shown by this tab (SPEC §6.2). */
  sessionId: string
  /** Protocol (tab icon; shell convention `params.protocol`). */
  protocol?: Protocol
  /** Saved connection the session was created from (overrides, duplicate, reopen). */
  connectionId?: string
  /** Quick-connect spec without secrets (duplicate / reopen of unsaved sessions). */
  quick?: QuickSpec
  /** Local shell id (duplicate / reopen of local terminals). */
  shell?: string
  /** Environment colour (TERM-10): tab icon tint + accent strip. */
  color?: string
  /** Default tab title. */
  title?: string
  /** Per-tab font zoom delta in px (TERM-25). */
  zoom?: number
  /** Notify on output in this tab while it is in the background (TERM-21). */
  monitorActivity?: boolean
  /** Notify after this tab stayed silent for `silenceSeconds` (TERM-21). */
  monitorSilence?: boolean
  /** Keep the terminal at a fixed size instead of fitting the pane (CC-19, legacy applications). */
  fixedSize?: { cols: number; rows: number }
}

/** Transport (WebSocket) state — independent of the remote session state. */
export type TransportState = 'connecting' | 'open' | 'reconnecting' | 'closed'

/** Runtime session state as seen by a terminal tab; `gone` = the session no longer exists server-side. */
export type TerminalSessionState = SessionState | 'unknown' | 'gone'

/** Live, render-relevant state of one terminal tab (drives overlays, the status bar and the MultiExec bar). */
export interface TerminalInfo {
  tabId: string
  sessionId: string
  protocol?: Protocol
  /** Display title: connection name / OSC title according to the title setting. */
  title: string
  /** Last OSC 0/2 title reported by the remote application. */
  oscTitle: string
  cols: number
  rows: number
  /** Authoritative PTY size from the server (differs from cols/rows when another client resized it). */
  ptyCols?: number
  ptyRows?: number
  state: TerminalSessionState
  stateMessage?: string
  exitCode?: number
  transport: TransportState
  /** Consecutive failed WebSocket attempts (reconnect UI). */
  transportAttempts: number
  cwd?: string
  readOnly: boolean
  /** Output paused (Scroll Lock, CC-10). */
  paused: boolean
  /** Bytes held while paused. */
  pausedBytes: number
  encoding: string
  renderer: 'webgl' | 'dom'
  fontSize: number
  /** The terminal has rendered some output (connecting overlay vs. banner). */
  hasOutput: boolean
  /** Output is being replayed from the server ring buffer after (re)attaching. */
  replaying: boolean
  searchOpen: boolean
  /** Last error message reported by the terminal socket. */
  error?: string
  /** Progress of a line-by-line paste (null when idle). */
  pasting: { done: number; total: number } | null
  /** Session recording / text logging (from the sessions cache). */
  recording: boolean
  logging: boolean
  /** A plugin holds the input lock (e.g. "ZMODEM transfer in progress"): input from other sources is refused. */
  inputLock?: string
  /** Administrators currently viewing this session read-only (shown to the owner). */
  shadowedBy?: string[]
}

export type TerminalPasteMode = 'normal' | 'paced'

export interface TerminalPasteOptions {
  /** Line-by-line with a delay (TERM-17 paced paste). */
  mode?: TerminalPasteMode
  /** Skip the confirmation dialog (text already confirmed elsewhere). */
  skipConfirm?: boolean
  /** Per-line delay for paced pastes (default: settings.pasteLineDelayMs, min 10ms). */
  lineDelayMs?: number
}

/**
 * Public handle of a live terminal tab (terminal bus, SPEC §10). Obtained with `getActiveTerminal()` /
 * `listTerminals()` from `@/features/terminal/bus`.
 */
export interface TerminalHandle {
  readonly tabId: string
  readonly sessionId: string
  /** The xterm instance (do not dispose; prefer the plugin registry for long-lived extensions). */
  readonly term: Terminal
  /** Runtime session from the sessions cache. */
  session(): RuntimeSession | undefined
  info(): TerminalInfo
  /** Raw input to this session only (no MultiExec fan-out, no input observers). */
  send(data: string | Uint8Array): boolean
  /** Input as if typed by the user (MultiExec fan-out, input observers, macro recorders). */
  input(data: string): void
  /** Paste text through the paste-safety pipeline (confirmation, sanitising, bracketed paste). Resolves false if cancelled. */
  paste(text: string, opts?: TerminalPasteOptions): Promise<boolean>
  /** Write text locally into the terminal (not sent to the session). */
  writeLocal(data: string | Uint8Array): void
  /** Paste the system clipboard (asks for clipboard permission when needed). */
  pasteFromClipboard(opts?: TerminalPasteOptions): Promise<boolean>
  focus(): void
  getSelection(): string
  copySelection(): Promise<boolean>
  selectAll(): void
  clear(): void
  reset(): void
  openSearch(): void
  /** Reconnect the session (or start a new one when it vanished). */
  reconnect(): void
  /** Session is connecting / connected. */
  isRunning(): boolean
  zoomBy(delta: number): void
  zoomReset(): void
  /** Pause / resume output rendering (toggle without argument). */
  togglePause(on?: boolean): void
  saveOutput(format: 'text' | 'html'): void
  copyAll(format: 'text' | 'html'): Promise<void>
  openClipboardHistory(): void
  /** Scroll to the previous (-1) / next (1) shell prompt (OSC 133); false when there is none. */
  jumpToPrompt(dir: -1 | 1): boolean
  sendSignal(name: 'INT' | 'TERM' | 'KILL' | 'HUP' | 'QUIT'): void
  sendBreak(): void
  /** Why input from other sources is refused right now (a plugin's input lock), or null. */
  inputLocked(): string | null
}

/** Input event on the terminal bus (user input sent to a session). */
export interface TerminalInputEvent {
  tabId: string
  sessionId: string
  data: string
  /** Fan-out copy produced by MultiExec (the source terminal emits one event per target). */
  broadcast: boolean
}

/** Output event on the terminal bus (bytes received from a session, before they are rendered). */
export interface TerminalOutputEvent {
  tabId: string
  sessionId: string
  data: Uint8Array
  /** Bytes replayed from the ring buffer while (re)attaching. */
  replay: boolean
}

/**
 * Extra members available on the TerminalPluginContext passed to registerTerminalPlugin setups (a superset of
 * `TerminalPluginContext` in src/app/registry.ts; cast `ctx as TerminalPluginContextEx` to use them).
 */
export interface TerminalPluginContextEx {
  /** The terminal handle (paste, input, focus...). */
  handle: TerminalHandle
  /** Write locally into xterm (status lines of file transfers, etc.). */
  writeLocal(data: string | Uint8Array): void
  /**
   * Filter output bytes before they reach xterm: return the bytes to render (possibly empty) or null to swallow the
   * chunk. Offsets and acks always count the raw bytes. Returns an unregister function.
   */
  addOutputFilter(filter: (data: Uint8Array) => Uint8Array | null): () => void
  /** True while the chunk being parsed was replayed from the ring buffer. */
  isReplaying(): boolean
  /**
   * Hold the session's input stream (an in-band ZMODEM / trzsz transfer): until the returned function is called,
   * input from other sources — MultiExec fan-out from other tabs, `sendToSession` / `broadcast` (snippets,
   * automation, AI), pastes and `handle.input` / `handle.send` — is refused with a calm hint, and the terminal shows
   * `reason`. The holder keeps writing through `ctx.send` / `ctx.sendRaw`. Keys typed into this terminal are the
   * plugin's to guard.
   * Not covered: server-side senders (REST /input, automation's server pacer, logon actions, triggers).
   */
  acquireInputLock(reason: string): () => void
  /** Raw input to this session that passes the input lock (for the lock holder); true when handed to the socket. */
  sendRaw(data: string | Uint8Array): boolean
}

/** A terminal colour scheme (16 ANSI colours + specials), stored as #rrggbb strings. */
export interface TerminalScheme {
  id: string
  name: string
  background: string
  foreground: string
  cursor?: string
  cursorAccent?: string
  selectionBackground?: string
  selectionForeground?: string
  black: string
  red: string
  green: string
  yellow: string
  blue: string
  magenta: string
  cyan: string
  white: string
  brightBlack: string
  brightRed: string
  brightGreen: string
  brightYellow: string
  brightBlue: string
  brightMagenta: string
  brightCyan: string
  brightWhite: string
}

export const ANSI_KEYS = [
  'black',
  'red',
  'green',
  'yellow',
  'blue',
  'magenta',
  'cyan',
  'white',
  'brightBlack',
  'brightRed',
  'brightGreen',
  'brightYellow',
  'brightBlue',
  'brightMagenta',
  'brightCyan',
  'brightWhite',
] as const

export type AnsiKey = (typeof ANSI_KEYS)[number]
