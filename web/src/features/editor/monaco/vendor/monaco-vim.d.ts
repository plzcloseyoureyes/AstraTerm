/* Types of the vendored monaco-vim build (the subset AstraTerm uses; see monaco-vim.js). */
import type * as Monaco from 'monaco-editor/editor'

export interface VimAdapter {
  editor: Monaco.editor.IStandaloneCodeEditor
  dispose(): void
  on(event: string, handler: (...args: unknown[]) => void): void
}

export interface VimApi {
  defineEx(name: string, prefix: string, fn: (cm: VimAdapter, params: { args?: string[]; argString?: string }) => void): void
  map(lhs: string, rhs: string, context?: string): void
}

export declare const VimMode: { Vim: VimApi }

export declare function initVimMode(editor: Monaco.editor.IStandaloneCodeEditor, statusbarNode?: HTMLElement | null): VimAdapter
