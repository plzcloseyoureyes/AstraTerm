/*
 * Snippet placeholders — the same grammar as internal/automation/template.go:
 *
 *   {{name}}  {{name|default}}  {{name|a|b|c}} (choices)  {{name:secret}}  \{{ (literal "{{")
 *   built-ins: {{host}} {{port}} {{user}} {{title}} {{protocol}} {{date}} {{time}} {{datetime}} {{timestamp}} {{clipboard}}
 *
 * Names are [A-Za-z_][A-Za-z0-9_-]*; anything else between braces (e.g. Go templates "{{.State}}") stays literal.
 */
import type { Connection, RuntimeSession } from '@/api/types'

export interface TemplateVar {
  name: string
  default?: string
  choices?: string[]
  secret?: boolean
  builtin?: boolean
}

export const BUILTIN_VARS = ['host', 'port', 'user', 'title', 'protocol', 'date', 'time', 'datetime', 'timestamp', 'clipboard'] as const
const BUILTINS = new Set<string>(BUILTIN_VARS)

type Part = { text: string } | { v: TemplateVar }

const NAME_RE = /^[A-Za-z_][A-Za-z0-9_-]{0,63}$/

function parsePlaceholder(inner: string): TemplateVar | null {
  const parts = inner.split('|')
  let head = parts[0].trim()
  let secret = false
  const colon = head.indexOf(':')
  if (colon >= 0) {
    const flag = head.slice(colon + 1).trim().toLowerCase()
    if (flag !== 'secret' && flag !== 'password') return null
    secret = true
    head = head.slice(0, colon).trim()
  }
  if (!NAME_RE.test(head)) return null
  const v: TemplateVar = { name: head, secret, builtin: BUILTINS.has(head) }
  const alts = parts.slice(1)
  if (alts.length === 1) {
    v.default = alts[0].trim()
  } else if (alts.length > 1) {
    v.choices = alts.map((a) => a.trim()).filter(Boolean)
    if (v.choices.length) v.default = v.choices[0]
  }
  return v
}

function parse(s: string): Part[] {
  const parts: Part[] = []
  let lit = ''
  let i = 0
  while (i < s.length) {
    if (s.startsWith('\\{{', i)) {
      lit += '{{'
      i += 3
      continue
    }
    if (s.startsWith('{{', i)) {
      const end = s.indexOf('}}', i + 2)
      if (end >= 0) {
        const inner = s.slice(i + 2, end)
        if (!/[\r\n{]/.test(inner)) {
          const v = parsePlaceholder(inner)
          if (v) {
            if (lit) parts.push({ text: lit })
            lit = ''
            parts.push({ v })
            i = end + 2
            continue
          }
        }
      }
      lit += '{{'
      i += 2
      continue
    }
    lit += s[i]
    i++
  }
  if (lit) parts.push({ text: lit })
  return parts
}

/** Distinct placeholders in order of appearance (built-ins included, flagged). */
export function templateVars(s: string): TemplateVar[] {
  const out: TemplateVar[] = []
  const seen = new Map<string, TemplateVar>()
  for (const p of parse(s)) {
    if (!('v' in p)) continue
    const cur = seen.get(p.v.name)
    if (cur) {
      if (!cur.default && p.v.default) cur.default = p.v.default
      if (!cur.choices?.length && p.v.choices?.length) cur.choices = p.v.choices
      cur.secret = cur.secret || p.v.secret
      continue
    }
    const v = { ...p.v }
    seen.set(v.name, v)
    out.push(v)
  }
  return out
}

/** Placeholders the user has to fill in (not built-ins). */
export function promptVars(s: string): TemplateVar[] {
  return templateVars(s).filter((v) => !v.builtin)
}

/** Substitute placeholders: values first, then built-ins, then defaults. Returns the text and unfilled names. */
export function renderTemplate(s: string, values: Record<string, string>, builtins: Record<string, string> = {}): { text: string; missing: string[] } {
  let text = ''
  const missing: string[] = []
  for (const p of parse(s)) {
    if (!('v' in p)) {
      text += p.text
      continue
    }
    const { name } = p.v
    if (Object.prototype.hasOwnProperty.call(values, name)) {
      text += values[name]
    } else if (p.v.builtin && Object.prototype.hasOwnProperty.call(builtins, name)) {
      text += builtins[name]
    } else if (p.v.default) {
      text += p.v.default
    } else if (!p.v.builtin && !missing.includes(name)) {
      missing.push(name)
    }
  }
  return { text, missing }
}

const pad = (n: number) => String(n).padStart(2, '0')

/** Built-in values for a target session (connection optional). */
export function builtinValues(session: RuntimeSession | undefined, conn: Connection | undefined, clipboard = ''): Record<string, string> {
  const d = new Date()
  const date = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  const port = conn?.port || (conn?.protocol === 'ssh' ? 22 : conn?.protocol === 'telnet' ? 23 : 0)
  return {
    host: conn?.host || session?.host || '',
    user: conn?.username || session?.username || '',
    port: port ? String(port) : '',
    title: session?.title || conn?.name || '',
    protocol: session?.protocol || conn?.protocol || '',
    date,
    time,
    datetime: `${date} ${time}`,
    timestamp: String(Math.floor(d.getTime() / 1000)),
    clipboard,
  }
}

/** Does the template use {{clipboard}}? (the clipboard is read only when needed). */
export function usesClipboard(s: string): boolean {
  return templateVars(s).some((v) => v.name === 'clipboard')
}
