/*
 * HTML export and printing with syntax colours (TOOL-2). The document is rendered as escaped, script-free HTML with the
 * editor's own syntax palette (monaco/palette.ts): the current UI theme's colours for export, light colours (dark text
 * on white) for printing. Tokens come from Monaco's tokenizer (monaco.editor.tokenize); this module is pure. Line
 * numbers are CSS counters, so copying from the page gives the plain text.
 */
import { PALETTE, ruleIndexOf, SYNTAX_RULES } from './monaco/palette'

/** Larger documents are not exported (the HTML would be several times their size). */
const MAX_EXPORT_CHARS = 16 * 1024 * 1024

const ESC: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }
export function escapeHtml(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ESC[c])
}

export interface RenderOptions {
  title: string
  theme: 'light' | 'dark'
  lineNumbers: boolean
  tabSize: number
  fontSize?: number
}

/** A token of a line: starts at `offset`, runs to the next token (Monaco's IToken). */
export interface LineToken {
  offset: number
  type: string
}

function styles(o: RenderOptions, lines: number): string {
  const p = PALETTE[o.theme]
  const bg = o.theme === 'dark' ? '#1e2127' : '#ffffff'
  const fg = o.theme === 'dark' ? '#d7dae0' : '#1f2328'
  const muted = o.theme === 'dark' ? '#6b717d' : '#8c959f'
  const rules = SYNTAX_RULES.map((r, i) => {
    const decl: string[] = []
    const color = r.color ? p[r.color] : ''
    if (color) decl.push(`color:${color}`)
    const fs = r.fontStyle ?? ''
    if (fs.includes('italic')) decl.push('font-style:italic')
    if (fs.includes('bold')) decl.push('font-weight:700')
    if (fs.includes('underline')) decl.push('text-decoration:underline')
    if (fs.includes('strikethrough')) decl.push('text-decoration:line-through')
    return decl.length ? `.s${i}{${decl.join(';')}}` : ''
  }).join('')
  const digits = String(Math.max(1, lines)).length
  const size = Math.max(6, Math.min(40, Math.round(o.fontSize ?? 12)))
  return [
    `:root{color-scheme:${o.theme}}`,
    `body{margin:0;background:${bg};color:${fg}}`,
    `pre{margin:0;padding:12px 16px;font:${size}px/1.5 "JetBrains Mono Variable","JetBrains Mono",ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:pre-wrap;overflow-wrap:anywhere;tab-size:${o.tabSize};-moz-tab-size:${o.tabSize};counter-reset:ln}`,
    o.lineNumbers
      ? `.l::before{counter-increment:ln;content:counter(ln);display:inline-block;min-width:${digits}ch;margin-right:2ch;text-align:right;color:${muted};-webkit-user-select:none;user-select:none}`
      : '',
    `@media print{body{-webkit-print-color-adjust:exact;print-color-adjust:exact}pre{padding:0}}`,
    rules,
  ].join('\n')
}

/** One line as HTML: tokens with a style become `<span class="sN">`. */
function renderLine(text: string, tokens: readonly LineToken[] | undefined): string {
  if (!tokens?.length) return escapeHtml(text)
  let html = ''
  for (let i = 0; i < tokens.length; i++) {
    const from = Math.max(0, tokens[i].offset)
    const to = i + 1 < tokens.length ? Math.min(text.length, tokens[i + 1].offset) : text.length
    if (to <= from) continue
    const part = escapeHtml(text.slice(from, to))
    const rule = ruleIndexOf(tokens[i].type)
    html += rule >= 0 ? `<span class="s${rule}">${part}</span>` : part
  }
  // text before the first token (tokenizers always start at 0, but be safe)
  const first = tokens[0].offset
  return first > 0 ? escapeHtml(text.slice(0, first)) + html : html
}

/**
 * A complete HTML document of `lines` with syntax colours from `tokens` (one list per line, as monaco.editor.tokenize
 * returns them; null = no highlighting). Throws for documents over MAX_EXPORT_CHARS.
 */
export function renderHtml(lines: readonly string[], tokens: readonly (readonly LineToken[])[] | null, o: RenderOptions): string {
  let size = 0
  for (const l of lines) size += l.length + 1
  if (size > MAX_EXPORT_CHARS) throw new Error('The document is too large to export (16 MiB of text at most).')
  const out: string[] = []
  for (let n = 0; n < lines.length; n++) out.push(`<span class="l">${renderLine(lines[n], tokens?.[n])}</span>`)
  return [
    '<!DOCTYPE html>',
    '<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">',
    '<meta name="generator" content="AstraTerm">',
    `<title>${escapeHtml(o.title)}</title>`,
    `<style>${styles(o, lines.length)}</style>`,
    '</head><body><pre>',
    out.join('\n'),
    '</pre></body></html>',
  ].join('')
}

/** Print an HTML document through a hidden same-origin frame (the browser's print dialog). */
export async function printHtml(html: string): Promise<void> {
  const frame = document.createElement('iframe')
  frame.setAttribute('aria-hidden', 'true')
  frame.tabIndex = -1
  frame.title = 'Print'
  Object.assign(frame.style, { position: 'fixed', right: '0', bottom: '0', width: '0', height: '0', border: '0' })
  frame.srcdoc = html
  const loaded = new Promise<void>((resolve) => frame.addEventListener('load', () => resolve(), { once: true }))
  document.body.appendChild(frame)
  await loaded
  const w = frame.contentWindow
  if (!w) {
    frame.remove()
    throw new Error('Printing is not available in this browser.')
  }
  let removed = false
  const cleanup = () => {
    if (removed) return
    removed = true
    frame.remove()
  }
  w.addEventListener('afterprint', () => setTimeout(cleanup, 0), { once: true })
  // Browsers without afterprint (or a dialog left open): remove the frame eventually.
  setTimeout(cleanup, 10 * 60_000)
  w.focus()
  w.print()
}
