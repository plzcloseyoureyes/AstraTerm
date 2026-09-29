/*
 * Colour swatches (TOOL-2): CSS colour literals — #rgb, #rgba, #rrggbb, #rrggbbaa, rgb()/rgba(), hsl()/hsla() — in
 * markup, scripts and data/config files get Monaco's colour decorator and picker (style sheets get theirs from the
 * CSS language service). Not in shell scripts or Markdown, where "#abc" is usually a comment or a heading. Pure: the
 * provider itself is registered by monaco/setup.ts.
 */
import { parseColor, type Rgba } from './monaco/color'

/** Monaco language ids that get swatches from this provider. */
export const SWATCH_LANGUAGES: ReadonlySet<string> = new Set([
  'html',
  'xml',
  'javascript',
  'typescript',
  'coffeescript',
  'json',
  'yaml',
  'toml',
  'ini',
  'conf',
  'pug',
  'liquid',
  'twig',
  'handlebars',
  'razor',
  'php',
])

const NUM = String.raw`[+-]?(?:\d+\.?\d*|\.\d+)(?:deg|turn|rad|grad|%)?`
const SEP = String.raw`\s*(?:,\s*|\s+)`
const FUNC = String.raw`\b(?:rgba?|hsla?)\(\s*${NUM}${SEP}${NUM}${SEP}${NUM}(?:\s*[,/]\s*${NUM})?\s*\)`
const HEX = String.raw`(?<![\w#&$-])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})(?![\w-])`
const COLOR_RE = new RegExp(`${HEX}|${FUNC}`, 'g')

/** Longest line scanned (minified files). */
export const MAX_SWATCH_LINE = 20_000

/** Colour literals in a string: [start, end, text]. */
export function findColors(text: string): [number, number, string][] {
  const out: [number, number, string][] = []
  COLOR_RE.lastIndex = 0
  for (let m = COLOR_RE.exec(text); m; m = COLOR_RE.exec(text)) out.push([m.index, m.index + m[0].length, m[0]])
  return out
}

/** A literal's colour (channels 0..1 as Monaco wants them), null when it does not parse. */
export function literalColor(text: string): { red: number; green: number; blue: number; alpha: number } | null {
  const c = parseColor(text)
  return c ? { red: c.r / 255, green: c.g / 255, blue: c.b / 255, alpha: c.a } : null
}

const h2 = (v: number) => Math.round(v).toString(16).padStart(2, '0')
const round = (v: number, d = 2) => Number(v.toFixed(d))

/** The picked colour written in the style of the literal it replaces (hex, rgb() or hsl()). */
export function colorLiteral(original: string, c: { red: number; green: number; blue: number; alpha: number }): string {
  const rgb: Rgba = { r: c.red * 255, g: c.green * 255, b: c.blue * 255, a: c.alpha }
  const o = original.trim().toLowerCase()
  if (o.startsWith('hsl')) {
    const r = c.red
    const g = c.green
    const b = c.blue
    const max = Math.max(r, g, b)
    const min = Math.min(r, g, b)
    const l = (max + min) / 2
    let h = 0
    let s = 0
    if (max !== min) {
      const d = max - min
      s = l > 0.5 ? d / (2 - max - min) : d / (max + min)
      h = max === r ? (g - b) / d + (g < b ? 6 : 0) : max === g ? (b - r) / d + 2 : (r - g) / d + 4
      h *= 60
    }
    const body = `${Math.round(h)}, ${Math.round(s * 100)}%, ${Math.round(l * 100)}%`
    return c.alpha < 1 ? `hsla(${body}, ${round(c.alpha)})` : `hsl(${body})`
  }
  if (o.startsWith('rgb')) {
    const body = `${Math.round(rgb.r)}, ${Math.round(rgb.g)}, ${Math.round(rgb.b)}`
    return c.alpha < 1 ? `rgba(${body}, ${round(c.alpha)})` : `rgb(${body})`
  }
  const hex = `#${h2(rgb.r)}${h2(rgb.g)}${h2(rgb.b)}`
  return c.alpha < 1 ? hex + h2(c.alpha * 255) : hex
}
