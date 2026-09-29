/*
 * Syntax colours of the editor for the light and dark UI themes, as Monaco (Monarch) token rules. Shared by the Monaco
 * theme (monaco/theme.ts) and HTML export / printing (export.ts). Pure: no Monaco, no DOM.
 */

/** Syntax colours ("" = the text colour). */
export const PALETTE = {
  light: {
    keyword: '#cf222e',
    string: '#0a3069',
    regexp: '#116329',
    comment: '#6e7781',
    number: '#0550ae',
    function: '#8250df',
    type: '#953800',
    property: '#0550ae',
    variable: '',
    builtin: '#0550ae',
    tag: '#116329',
    operator: '#cf222e',
    punctuation: '#57606a',
    meta: '#6e7781',
    constant: '#0550ae',
    heading: '#0550ae',
    link: '#0969da',
    inserted: '#116329',
    deleted: '#82071e',
    invalid: '#cf222e',
  },
  dark: {
    keyword: '#c678dd',
    string: '#98c379',
    regexp: '#56b6c2',
    comment: '#7f848e',
    number: '#d19a66',
    function: '#61afef',
    type: '#e5c07b',
    property: '#e06c75',
    variable: '#d7dae0',
    builtin: '#56b6c2',
    tag: '#e06c75',
    operator: '#56b6c2',
    punctuation: '#abb2bf',
    meta: '#7f848e',
    constant: '#d19a66',
    heading: '#e06c75',
    link: '#61afef',
    inserted: '#98c379',
    deleted: '#e06c75',
    invalid: '#ff6b6b',
  },
} as const

export type SyntaxColor = keyof typeof PALETTE.light

/** Bracket pair colours (nesting levels 1-3), per theme. */
export const BRACKET_COLORS = {
  light: ['#b35c00', '#8250df', '#0969da'],
  dark: ['#e5c07b', '#c678dd', '#61afef'],
} as const

/** One token rule: Monarch token prefix (dot-separated scopes, "" = default) → colour and font style. */
export interface SyntaxRule {
  token: string
  color?: SyntaxColor
  fontStyle?: 'italic' | 'bold' | 'underline' | 'strikethrough' | 'italic bold'
}

/**
 * Token rules. Monaco matches a rule when its token is a prefix of the token type at a "." boundary
 * ("keyword" matches "keyword.flow.js"); the longest (most specific) rule wins — like `tokenStyle` below.
 */
export const SYNTAX_RULES: readonly SyntaxRule[] = [
  { token: 'comment', color: 'comment', fontStyle: 'italic' },
  { token: 'comment.doc', color: 'comment', fontStyle: 'italic' },
  { token: 'string', color: 'string' },
  { token: 'string.escape', color: 'regexp' },
  { token: 'string.key', color: 'property' },
  { token: 'string.key.json', color: 'property' },
  { token: 'string.value', color: 'string' },
  { token: 'string.link', color: 'link', fontStyle: 'underline' },
  { token: 'string.target', color: 'link' },
  { token: 'string.sql', color: 'string' },
  { token: 'string.heredoc', color: 'string' },
  { token: 'regexp', color: 'regexp' },
  { token: 'keyword', color: 'keyword' },
  { token: 'keyword.json', color: 'constant' },
  { token: 'keyword.flow', color: 'keyword' },
  { token: 'keyword.md', color: 'heading', fontStyle: 'bold' },
  { token: 'storage', color: 'keyword' },
  { token: 'number', color: 'number' },
  { token: 'constant', color: 'constant' },
  { token: 'type', color: 'type' },
  { token: 'type.identifier', color: 'type' },
  { token: 'type.yaml', color: 'property' },
  { token: 'namespace', color: 'type' },
  { token: 'identifier', color: 'variable' },
  { token: 'variable', color: 'variable' },
  { token: 'variable.predefined', color: 'builtin' },
  { token: 'variable.parameter', color: 'variable' },
  { token: 'variable.name', color: 'property' },
  { token: 'variable.value', color: 'string' },
  { token: 'predefined', color: 'builtin' },
  { token: 'function', color: 'function' },
  { token: 'annotation', color: 'meta' },
  { token: 'attribute.name', color: 'property' },
  { token: 'attribute.value', color: 'string' },
  { token: 'attribute.value.number', color: 'number' },
  { token: 'attribute.value.unit', color: 'number' },
  { token: 'attribute.value.hex', color: 'number' },
  { token: 'key', color: 'property' },
  { token: 'tag', color: 'tag' },
  { token: 'tag.id', color: 'function' },
  { token: 'tag.class', color: 'function' },
  { token: 'metatag', color: 'meta' },
  { token: 'metatag.content', color: 'string' },
  { token: 'meta', color: 'meta' },
  { token: 'meta.header', color: 'meta', fontStyle: 'bold' },
  { token: 'meta.range', color: 'function' },
  { token: 'operator', color: 'operator' },
  { token: 'operators', color: 'operator' },
  { token: 'delimiter', color: 'punctuation' },
  { token: 'delimiter.html', color: 'punctuation' },
  { token: 'delimiter.xml', color: 'punctuation' },
  { token: 'emphasis', fontStyle: 'italic' },
  { token: 'strong', fontStyle: 'bold' },
  { token: 'header', color: 'heading', fontStyle: 'bold' },
  { token: 'markup.heading', color: 'heading', fontStyle: 'bold' },
  { token: 'inserted', color: 'inserted' },
  { token: 'deleted', color: 'deleted' },
  { token: 'changed', color: 'constant' },
  { token: 'invalid', color: 'invalid' },
  { token: 'info-token', color: 'function' },
  { token: 'warn-token', color: 'number' },
  { token: 'error-token', color: 'invalid' },
  { token: 'debug-token', color: 'comment' },
]

const RULE_INDEX = new Map<string, number>(SYNTAX_RULES.map((r, i) => [r.token, i]))

/**
 * Index of the rule that styles a token type ("keyword.flow.js" → the "keyword.flow" rule), -1 = default text.
 * Monaco's own matching: the longest rule that is a whole-scope prefix of the type.
 */
export function ruleIndexOf(type: string): number {
  let t = type
  for (;;) {
    const i = RULE_INDEX.get(t)
    if (i !== undefined) return i
    const dot = t.lastIndexOf('.')
    if (dot < 0) return -1
    t = t.slice(0, dot)
  }
}

/** Colour of a token type in a theme ("" = default text colour). */
export function tokenColor(type: string, theme: 'light' | 'dark'): string {
  const i = ruleIndexOf(type)
  const c = i >= 0 ? SYNTAX_RULES[i].color : undefined
  return c ? PALETTE[theme][c] : ''
}

/**
 * A user-supplied CSS font-family list, reduced to characters a font list needs (names, quotes, commas, spaces,
 * dashes) so it cannot break out of a CSS rule or an editor option. Empty when nothing usable is left.
 */
export function safeFontFamily(value: string | undefined | null): string {
  if (!value) return ''
  const cleaned = value.replace(/[^\p{L}\p{N} ,'"_.-]/gu, '').replace(/\s+/g, ' ').trim()
  // unbalanced quotes would swallow the rest of the declaration
  if ((cleaned.split('"').length - 1) % 2 || (cleaned.split("'").length - 1) % 2) return cleaned.replace(/["']/g, '')
  return cleaned.slice(0, 300)
}
