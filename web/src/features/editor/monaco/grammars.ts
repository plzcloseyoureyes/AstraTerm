/*
 * Monarch grammars for the sysadmin formats Monaco does not ship: nginx, TOML, unified diffs, Makefiles and a generic
 * "Config" language (sshd_config, hosts, fstab, *.conf, ignore files...). Registered once by monaco/setup.ts.
 */
import type * as Monaco from 'monaco-editor/editor'

type Lang = Monaco.languages.IMonarchLanguage
type Conf = Monaco.languages.LanguageConfiguration

const hashComments: Conf = {
  comments: { lineComment: '#' },
  brackets: [
    ['{', '}'],
    ['[', ']'],
    ['(', ')'],
  ],
  autoClosingPairs: [
    { open: '{', close: '}' },
    { open: '[', close: ']' },
    { open: '(', close: ')' },
    { open: '"', close: '"', notIn: ['string', 'comment'] },
    { open: "'", close: "'", notIn: ['string', 'comment'] },
  ],
  surroundingPairs: [
    { open: '{', close: '}' },
    { open: '[', close: ']' },
    { open: '(', close: ')' },
    { open: '"', close: '"' },
    { open: "'", close: "'" },
  ],
}

const nginx: Lang = {
  defaultToken: '',
  ignoreCase: false,
  keywords: [
    'server',
    'location',
    'upstream',
    'http',
    'events',
    'stream',
    'mail',
    'if',
    'map',
    'geo',
    'types',
    'split_clients',
    'limit_except',
    'include',
    'return',
    'rewrite',
    'set',
    'break',
  ],
  tokenizer: {
    root: [
      // the first word of a statement is the directive
      [/^\s*(?=[A-Za-z_])/, { token: '', next: '@directive' }],
      [/#.*$/, 'comment'],
      [/[{}]/, { token: 'delimiter.bracket', next: '@directive' }],
      [/;/, { token: 'delimiter', next: '@directive' }],
      [/"/, 'string', '@dq'],
      [/'/, 'string', '@sq'],
      [/\$\{?[A-Za-z_][\w]*\}?/, 'variable'],
      [/~\*?|\^~|=(?=\s)/, 'operator'],
      [/\b(on|off)\b/, 'constant'],
      [/\b(\d{1,3}\.){3}\d{1,3}(\/\d+)?(:\d+)?\b/, 'number'],
      [/\b\d+(\.\d+)?(ms|s|m|h|d|w|M|y|k|K|g|G)?\b/, 'number'],
      [/[^\s;{}#"'$]+/, ''],
    ],
    directive: [
      [/\s+/, ''],
      [/[A-Za-z_][\w-]*/, { cases: { '@keywords': { token: 'keyword', next: '@pop' }, '@default': { token: 'type.identifier', next: '@pop' } } }],
      [/(?=.)/, { token: '', next: '@pop' }],
    ],
    dq: [
      [/[^\\"$]+/, 'string'],
      [/\$\{?[A-Za-z_]\w*\}?/, 'variable'],
      [/\\./, 'string.escape'],
      [/"/, 'string', '@pop'],
      [/\$/, 'string'],
    ],
    sq: [
      [/[^\\']+/, 'string'],
      [/\\./, 'string.escape'],
      [/'/, 'string', '@pop'],
    ],
  },
}

const toml: Lang = {
  defaultToken: '',
  tokenizer: {
    root: [
      [/#.*$/, 'comment'],
      [/^\s*\[\[[^\]]*\]\]/, 'metatag'],
      [/^\s*\[[^\]]*\]/, 'metatag'],
      [/^(\s*)([A-Za-z0-9_.-]+|"[^"]*"|'[^']*')(\s*)(=)/, ['', 'key', '', 'delimiter']],
      { include: '@value' },
    ],
    value: [
      [/"""/, 'string', '@mlBasic'],
      [/'''/, 'string', '@mlLiteral'],
      [/"/, 'string', '@basic'],
      [/'[^']*'/, 'string'],
      [/\b(true|false)\b/, 'keyword'],
      [/\d{4}-\d{2}-\d{2}([Tt ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?([Zz]|[+-]\d{2}:\d{2})?)?/, 'number'],
      [/\d{2}:\d{2}:\d{2}(\.\d+)?/, 'number'],
      [/[+-]?(0x[0-9A-Fa-f_]+|0o[0-7_]+|0b[01_]+|inf|nan|\d[\d_]*(\.[\d_]+)?([eE][+-]?\d+)?)/, 'number'],
      [/[[\]{}]/, 'delimiter.bracket'],
      [/[,=.]/, 'delimiter'],
      [/#.*$/, 'comment'],
    ],
    basic: [
      [/[^\\"]+/, 'string'],
      [/\\./, 'string.escape'],
      [/"/, 'string', '@pop'],
    ],
    mlBasic: [
      [/[^\\"]+/, 'string'],
      [/\\./, 'string.escape'],
      [/"""/, 'string', '@pop'],
      [/"/, 'string'],
    ],
    mlLiteral: [
      [/[^']+/, 'string'],
      [/'''/, 'string', '@pop'],
      [/'/, 'string'],
    ],
  },
}

const diff: Lang = {
  defaultToken: '',
  tokenizer: {
    root: [
      [/^(diff|index|similarity|rename|new file|deleted file|old mode|new mode|Index:|={4,}|Binary files).*$/, 'meta.header'],
      [/^(---|\+\+\+) .*$/, 'meta.header'],
      [/^@@.*@@/, 'meta.range'],
      [/^\+.*$/, 'inserted'],
      [/^-.*$/, 'deleted'],
      [/^[<].*$/, 'deleted'],
      [/^[>].*$/, 'inserted'],
      [/^!.*$/, 'changed'],
      [/^\\ No newline.*$/, 'comment'],
    ],
  },
}

const makefile: Lang = {
  defaultToken: '',
  keywords: ['ifeq', 'ifneq', 'ifdef', 'ifndef', 'else', 'endif', 'include', '-include', 'sinclude', 'define', 'endef', 'export', 'unexport', 'override', 'vpath', 'private'],
  tokenizer: {
    root: [
      [/#.*$/, 'comment'],
      [/^(?=\t)/, { token: '', next: '@recipe' }],
      [/^(\s*)([A-Za-z_][\w.-]*)(\s*)(:=|::=|\?=|\+=|!=|=)/, ['', 'variable.name', '', 'operator']],
      [/^(\.?[^:#=\s][^:#=]*?)(::?)(?!=)/, ['type.identifier', 'delimiter']],
      [/\$[({][A-Za-z_][\w.-]*[)}]|\$[@<^+?*%]|\$\$/, 'variable'],
      [/\$[({]/, 'variable', '@func'],
      [/[A-Za-z_-]+/, { cases: { '@keywords': 'keyword', '@default': '' } }],
      [/"([^"\\]|\\.)*"|'[^']*'/, 'string'],
    ],
    recipe: [
      // a recipe is the run of tab-indented lines after a rule: a line without the tab ends it
      [/^(?=[^\t])/, { token: '', next: '@pop' }],
      [/^\t[@-]*/, 'operator'],
      [/#.*$/, 'comment'],
      [/\$[({][A-Za-z_][\w.-]*[)}]|\$[@<^+?*%]|\$\$/, 'variable'],
      [/\$[({]/, 'variable', '@func'],
      [/"([^"\\]|\\.)*"|'[^']*'/, 'string'],
      [/./, ''],
    ],
    func: [
      [/[)}]/, 'variable', '@pop'],
      [/\$[({]/, 'variable', '@push'],
      [/[A-Za-z_-]+/, 'predefined'],
      [/[^)}$]+/, 'string'],
    ],
  },
}

/** Generic "key value" configuration files: comments, sections, key = value / key value, strings, numbers. */
const conf: Lang = {
  defaultToken: '',
  tokenizer: {
    root: [
      [/^\s*[#;!].*$/, 'comment'],
      [/\s#.*$/, 'comment'],
      [/^\s*\[[^\]]*\]/, 'metatag'],
      [/^\s*<\/?[\w-]+[^>]*>/, 'tag'],
      [/^(\s*)([A-Za-z_][\w.\-/]*)(\s*)([=:])/, ['', 'key', '', 'delimiter']],
      [/^(\s*)([A-Za-z_][\w.-]*)(?=\s)/, ['', 'key']],
      [/"([^"\\]|\\.)*"/, 'string'],
      [/'[^']*'/, 'string'],
      [/\$\{?[A-Za-z_]\w*\}?/, 'variable'],
      [/\b(yes|no|true|false|on|off|none|any|all)\b/i, 'constant'],
      [/\b(\d{1,3}\.){3}\d{1,3}(\/\d+)?\b|\b[0-9a-f]{0,4}(:[0-9a-f]{0,4}){2,7}\b/i, 'number'],
      [/\b\d+(\.\d+)?[kKmMgGsh%]?\b/, 'number'],
    ],
  },
}

/** Register the grammars (ids match languages.ts). */
export function registerGrammars(monaco: typeof Monaco): void {
  const defs: [string, string[], Lang, Conf][] = [
    ['nginx', ['Nginx'], nginx, hashComments],
    ['toml', ['TOML'], toml, hashComments],
    ['diff', ['Diff', 'patch'], diff, {}],
    ['makefile', ['Makefile', 'make'], makefile, { ...hashComments, autoClosingPairs: [{ open: '(', close: ')' }, { open: '{', close: '}' }] }],
    ['conf', ['Config'], conf, hashComments],
  ]
  for (const [id, aliases, lang, cfg] of defs) {
    if (monaco.languages.getLanguages().some((l) => l.id === id)) continue
    monaco.languages.register({ id, aliases })
    monaco.languages.setMonarchTokensProvider(id, lang)
    monaco.languages.setLanguageConfiguration(id, cfg)
  }
}
