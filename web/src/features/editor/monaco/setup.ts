/*
 * The Monaco bundle: editor core, every editor contribution (find, folding, sticky scroll, suggest, command palette,
 * ...), all language definitions, the CSS / HTML / JSON / TypeScript language services and their web workers — all
 * bundled locally by Vite (no CDN, works offline and under the app's CSP: workers are same-origin module scripts).
 * This module is only reached through `loadMonaco()` (monaco/load.ts), so none of it is in the app's initial bundle.
 */
import * as monaco from 'monaco-editor/editor'
import 'monaco-editor/features/register.all'
import 'monaco-editor/languages/definitions/register.all'
import 'monaco-editor/languages/features/css/register'
import 'monaco-editor/languages/features/html/register'
import { jsonDefaults } from 'monaco-editor/languages/features/json/register'
import {
  JsxEmit,
  javascriptDefaults,
  ModuleKind,
  ModuleResolutionKind,
  ScriptTarget,
  typescriptDefaults,
} from 'monaco-editor/languages/features/typescript/register'
import { loader } from '@monaco-editor/react'
import EditorWorker from 'monaco-editor/editor/editor.worker.js?worker'
import JsonWorker from 'monaco-editor/languages/features/json/json.worker.js?worker'
import CssWorker from 'monaco-editor/languages/features/css/css.worker.js?worker'
import HtmlWorker from 'monaco-editor/languages/features/html/html.worker.js?worker'
import TsWorker from 'monaco-editor/languages/features/typescript/ts.worker.js?worker'
import { colorLiteral, findColors, literalColor, MAX_SWATCH_LINE, SWATCH_LANGUAGES } from '../colors'
import { registerGrammars } from './grammars'
import { installServices } from './services'

export { bindEditorTab, forgetDiff, onCopyToLeft, setIgnoreAllWhitespace, type GutterBlock } from './services'
import { JSON_SCHEMAS } from './schemas'
import { applyTheme, watchTheme } from './theme'

export type MonacoApi = typeof monaco

// ---------------------------------------------------------------------------------------------------------------------
// workers (bundled by Vite, served from the app's own origin)
// ---------------------------------------------------------------------------------------------------------------------

const WORKERS: Record<string, new (opts?: { name?: string }) => Worker> = {
  json: JsonWorker,
  css: CssWorker,
  scss: CssWorker,
  less: CssWorker,
  html: HtmlWorker,
  handlebars: HtmlWorker,
  razor: HtmlWorker,
  typescript: TsWorker,
  javascript: TsWorker,
}

;(self as unknown as { MonacoEnvironment: monaco.Environment }).MonacoEnvironment = {
  getWorker(_moduleId: string, label: string) {
    const W = WORKERS[label] ?? EditorWorker
    return new W({ name: `monaco-${label}` })
  },
}

// Anything in the app that uses @monaco-editor/react gets this bundled instance, never the CDN copy.
loader.config({ monaco })

// ---------------------------------------------------------------------------------------------------------------------
// language services
// ---------------------------------------------------------------------------------------------------------------------

function configureLanguages(): void {
  registerGrammars(monaco)

  // JSON: comments are tolerated (tsconfig.json, VS Code settings, JSONC); well-known files get their schema. The
  // schemas are bundled: no schema is ever downloaded.
  jsonDefaults.setDiagnosticsOptions({
    validate: true,
    allowComments: true,
    comments: 'ignore',
    trailingCommas: 'warning',
    schemaValidation: 'warning',
    schemaRequest: 'ignore',
    enableSchemaRequest: false,
    schemas: JSON_SCHEMAS,
  })

  // JavaScript / TypeScript: a remote file has no project around it (no node_modules, no tsconfig), so semantic
  // errors would be noise ("Cannot find module …"). Syntax errors, completions, hovers, formatting stay on.
  typescriptDefaults.setCompilerOptions(TS_OPTIONS)
  javascriptDefaults.setCompilerOptions(TS_OPTIONS)
  setSemanticValidation(false)

  // Colour swatches + picker for literals in markup, scripts and config files (style sheets have their own).
  const provider: monaco.languages.DocumentColorProvider = {
    provideDocumentColors(model) {
      if (model.getValueLength() > 4 * 1024 * 1024) return []
      const out: monaco.languages.IColorInformation[] = []
      const n = model.getLineCount()
      for (let line = 1; line <= n && out.length < 2000; line++) {
        const text = model.getLineContent(line)
        if (text.length > MAX_SWATCH_LINE || (!text.includes('#') && !text.includes('('))) continue
        for (const [s, e, lit] of findColors(text)) {
          const color = literalColor(lit)
          if (color) out.push({ color, range: { startLineNumber: line, startColumn: s + 1, endLineNumber: line, endColumn: e + 1 } })
        }
      }
      return out
    },
    provideColorPresentations(model, info) {
      return [{ label: colorLiteral(model.getValueInRange(info.range), info.color) }]
    },
  }
  for (const id of SWATCH_LANGUAGES) monaco.languages.registerColorProvider(id, provider)
}

const TS_OPTIONS = {
  target: ScriptTarget.ESNext,
  module: ModuleKind.ESNext,
  moduleResolution: ModuleResolutionKind.NodeJs,
  allowJs: true,
  checkJs: false,
  allowNonTsExtensions: true,
  jsx: JsxEmit.Preserve,
  esModuleInterop: true,
  allowSyntheticDefaultImports: true,
  noEmit: true,
  lib: ['esnext', 'dom'],
}

let semantic: boolean | null = null

/**
 * JavaScript / TypeScript semantic checking (type errors, unknown names, ...) on or off; syntax errors are always
 * reported. Off by default: a single remote file has no project around it, so most semantic errors are noise
 * ("Cannot find module"). Settings → Editor → "Type-check JavaScript / TypeScript".
 */
export function setSemanticValidation(on: boolean): void {
  if (semantic === on) return
  semantic = on
  const diagnostics = {
    noSemanticValidation: !on,
    noSyntaxValidation: false,
    noSuggestionDiagnostics: !on,
    // Module resolution cannot work for a lone remote file: never report missing imports.
    diagnosticCodesToIgnore: [2307, 2792, 7016],
  }
  typescriptDefaults.setDiagnosticsOptions(diagnostics)
  javascriptDefaults.setDiagnosticsOptions(diagnostics)
  // JavaScript is only type-checked with checkJs.
  javascriptDefaults.setCompilerOptions({ ...TS_OPTIONS, checkJs: on })
}

// ---------------------------------------------------------------------------------------------------------------------
// "lite" languages: syntax colouring without a language service, for very large files
// ---------------------------------------------------------------------------------------------------------------------

interface MonarchModule {
  conf: monaco.languages.LanguageConfiguration
  language: monaco.languages.IMonarchLanguage
}

const LITE: Record<string, () => Promise<MonarchModule>> = {
  javascript: () => import('monaco-editor/languages/definitions/javascript/javascript'),
  typescript: () => import('monaco-editor/languages/definitions/typescript/typescript'),
  json: () => import('monaco-editor/languages/definitions/javascript/javascript'),
  css: () => import('monaco-editor/languages/definitions/css/css'),
  scss: () => import('monaco-editor/languages/definitions/scss/scss'),
  less: () => import('monaco-editor/languages/definitions/less/less'),
  html: () => import('monaco-editor/languages/definitions/html/html'),
  handlebars: () => import('monaco-editor/languages/definitions/handlebars/handlebars'),
  razor: () => import('monaco-editor/languages/definitions/razor/razor'),
}

const liteRegistered = new Set<string>()

/**
 * For languages served by a worker (JS/TS/JSON/CSS/HTML): an id with the same syntax colouring but no language
 * service — big files are not shipped to a worker and analysed on every keystroke. Null for other languages.
 */
export function liteLanguage(id: string): string | null {
  const load = LITE[id]
  if (!load) return null
  const lite = `${id}-lite`
  if (!liteRegistered.has(lite)) {
    liteRegistered.add(lite)
    monaco.languages.register({ id: lite, aliases: [`${id} (large file)`] })
    const mod = load()
    monaco.languages.setMonarchTokensProvider(lite, mod.then((m) => m.language))
    void mod.then(
      (m) => monaco.languages.setLanguageConfiguration(lite, m.conf),
      () => undefined,
    )
  }
  return lite
}

/** The language a "lite" id stands for (the id itself otherwise). */
export function baseLanguage(id: string): string {
  return id.endsWith('-lite') ? id.slice(0, -5) : id
}

// ---------------------------------------------------------------------------------------------------------------------
// one-time setup
// ---------------------------------------------------------------------------------------------------------------------

configureLanguages()
installServices(monaco)
applyTheme(monaco)
watchTheme(monaco)

// Glyph widths are measured once: measure again when the (bundled, lazily loaded) editor font arrives.
if (typeof document !== 'undefined' && 'fonts' in document) {
  document.fonts.addEventListener('loadingdone', () => monaco.editor.remeasureFonts())
}

export { monaco }
