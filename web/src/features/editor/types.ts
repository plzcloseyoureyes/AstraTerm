/*
 * Editor feature types (TOOL-2 text editor, TOOL-3 text diff, FILE-10 remote editing, FILE-23 hex editor).
 * Tab params are persisted with the workspace layout: keep them small and JSON-serialisable (document bodies live in
 * the feature's IndexedDB store, see docstore.ts).
 */
import type { FsOpenRequest } from '@/api/types'

/** How a remote file is shown. */
export type EditorMode = 'text' | 'hex'

/** Tab kind "editor": a file on a backend file system handle (SFTP / FTP / S3 / server-local). */
export interface EditorTabParams {
  /** File system handle id (POST /api/fs). */
  fsId: string
  /** Absolute path on that file system. */
  path: string
  /** Open read-only (the user can still unlock editing from the toolbar). */
  readOnly?: boolean
  /** Handle label shown in the status bar, e.g. "user@host" (optional; the FsHandle label). */
  label?: string
  /** How to re-open the handle if it expired (backend restart); optional. */
  source?: FsOpenRequest
  /** The editor opened this handle itself and closes it with the last tab using it. */
  ownsFs?: boolean
  /** "hex" opens the hex editor directly. */
  mode?: EditorMode
  /** Text encoding chosen with "Reopen with encoding" (default: auto). */
  encoding?: string
  /** Language name override (language picker), "" = plain text. */
  language?: string
  /** 1-based line to reveal after loading. */
  line?: number
  /** Text to find and select after loading (find in files); cleared once shown. */
  find?: string
  /** Font size delta (Ctrl +/-) for this tab. */
  zoom?: number
  /** Per-tab word wrap / whitespace toggles (default: settings). */
  wrap?: boolean
  whitespace?: boolean
  minimap?: boolean
  /** The user already chose "Open anyway" for this (large) file. */
  large?: boolean
  /** Saves need sudo (a previous "Save with sudo" succeeded): write through sudo directly. */
  sudo?: boolean
}

/** Tab kind "text": scratch document or a file from the local computer. */
export interface TextTabParams {
  title: string
  /** Initial content (moved into the document store on first mount, then dropped from the params). */
  content?: string
  /** Language name (language-data name, e.g. "JSON"), "" = plain text; default: detected from the title. */
  language?: string
  /** Document id in the store. */
  docId?: string
  /** Opened from a file on this computer. */
  local?: { name: string }
  mode?: EditorMode
  zoom?: number
  wrap?: boolean
  whitespace?: boolean
  minimap?: boolean
}

/** One side of a diff. `source` re-opens an expired file system handle. */
export type DiffSource =
  | { kind: 'remote'; fsId: string; path: string; label?: string; source?: FsOpenRequest }
  | { kind: 'text'; title: string; docId: string }

/** Tab kind "diff" (TOOL-3). */
export interface DiffTabParams {
  left?: DiffSource
  right?: DiffSource
  /** Inline (unified) view instead of side by side. */
  unified?: boolean
  /** true: ignore leading / trailing whitespace; 'all': ignore every whitespace difference. */
  ignoreWhitespace?: boolean | 'all'
  /** Collapse long unchanged stretches. */
  collapse?: boolean
  zoom?: number
}

/** Input accepted by `editor.diff` for each side (normalised to DiffSource). */
export type DiffSourceInput =
  | { fsId: string; path: string; label?: string; source?: FsOpenRequest }
  | { content: string; title?: string }
  | { text: string; title?: string }
  | DiffSource

/** Settings section "editor". */
export interface EditorSettings {
  fontSize: number
  /** CSS font-family; empty = the app's monospace font. */
  fontFamily: string
  tabSize: number
  insertSpaces: boolean
  /** Detect tabs/spaces and indent width from the file content. */
  detectIndentation: boolean
  wordWrap: boolean
  vimMode: boolean
  /** Code overview at the right edge (toggle per tab). */
  minimap: boolean
  /** Keep the enclosing blocks' first lines at the top while scrolling. */
  stickyScroll: boolean
  /** Colour matching bracket pairs by nesting level. */
  bracketPairs: boolean
  autosave: 'off' | 'afterDelay' | 'onFocusLost'
  /** Delay for autosave "afterDelay" (ms). */
  autosaveDelay: number
  showWhitespace: boolean
  /** Vertical guides at every indentation level. */
  indentGuides: boolean
  /** Colour previews (and a colour picker) for CSS colour literals (style sheets, markup, config files). */
  colorSwatches: boolean
  lineNumbers: boolean
  highlightActiveLine: boolean
  closeBrackets: boolean
  autocomplete: boolean
  /** JavaScript / TypeScript semantic checking (type errors); syntax errors are always shown. */
  semanticValidation: boolean
  /** Poll the remote file every 5 s while the tab is focused and warn when it changed on the server. */
  watchRemote: boolean
  /** Ask before opening files larger than this (MiB). */
  largeFileMiB: number
  /** Line ending for new documents. */
  defaultEol: 'lf' | 'crlf'
}

export interface RecentFile {
  fsId: string
  path: string
  label?: string
  source?: FsOpenRequest
  openedAt: number
}
