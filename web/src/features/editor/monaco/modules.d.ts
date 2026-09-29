/* Monaco's Monarch language definitions ship without type declarations: they all export `conf` and `language`. */
declare module 'monaco-editor/languages/definitions/*' {
  import type * as Monaco from 'monaco-editor/editor'
  export const conf: Monaco.languages.LanguageConfiguration
  export const language: Monaco.languages.IMonarchLanguage
}

/*
 * Internal Monaco modules used by monaco/services.ts (no published types). Only the members AstraTerm touches are
 * declared; the shapes match monaco-editor 0.57 (esm/vs/...).
 */
declare module 'monaco-editor/editor/standalone/browser/standaloneServices.js' {
  export const StandaloneServices: { get<T>(id: unknown): T }
}
declare module 'monaco-editor/platform/layout/browser/layoutService.js' {
  export const ILayoutService: unknown
}
declare module 'monaco-editor/editor/browser/services/codeEditorService.js' {
  export const ICodeEditorService: unknown
}
declare module 'monaco-editor/editor/common/services/editorWorker.js' {
  export const IEditorWorkerService: unknown
}
declare module 'monaco-editor/platform/actions/common/actions.js' {
  export const MenuId: { DiffEditorHunkToolbar: unknown }
  export const MenuRegistry: { appendMenuItem(menu: unknown, item: unknown): { dispose(): void } }
}
declare module 'monaco-editor/platform/commands/common/commands.js' {
  export const CommandsRegistry: { registerCommand(id: string, handler: (accessor: unknown, ...args: unknown[]) => unknown): { dispose(): void } }
}
declare module 'monaco-editor/platform/contextkey/common/contextkey.js' {
  export const ContextKeyExpr: { and(...exprs: unknown[]): unknown }
}
declare module 'monaco-editor/editor/common/editorContextKeys.js' {
  interface RawContextKey {
    toNegated(): unknown
  }
  export const EditorContextKeys: { diffEditorOriginalWritable: RawContextKey; diffEditorInlineMode: RawContextKey }
}
declare module 'monaco-editor/base/common/codicons.js' {
  export const Codicon: Record<string, unknown>
}
declare module 'monaco-editor/editor/common/config/editorZoom.js' {
  export const EditorZoom: { getZoomLevel(): number; setZoomLevel(level: number): void }
}
declare module 'monaco-editor/editor/browser/controller/editContext/textArea/textAreaEditContextInput.js' {
  export class TextAreaWrapper {
    readonly _actual: HTMLTextAreaElement
    hasFocus(): boolean
    setIgnoreSelectionChangeTime(reason: string): void
    setSelectionRange(reason: string, selectionStart: number, selectionEnd: number): void
  }
}
declare module 'monaco-editor/base/browser/dom.js' {
  export function saveParentsScrollTop(node: Element): number[]
  export function restoreParentsScrollTop(node: Element, state: number[]): void
}
