/*
 * Languages of the editor (Monaco language ids) and language detection: file name (incl. sysadmin conventions Monaco
 * does not know), path, shebang, modelines and content sniffing. Pure: no Monaco import, so tab code, tests and the
 * status bar can use it before the editor bundle has loaded. The ids marked "custom" are registered by
 * monaco/grammars.ts (Monarch grammars shipped with AstraTerm).
 */

export interface LanguageInfo {
  /** Monaco language id. */
  id: string
  /** Display name (status bar, picker). */
  name: string
  /** File extensions with the dot, lower case. */
  ext?: string[]
  /** Exact file names, lower case. */
  files?: string[]
  /** Other names accepted by findLanguage (lower case). */
  aliases?: string[]
}

export const PLAIN_TEXT = 'Plain Text'
export const PLAIN_ID = 'plaintext'

export const LANGUAGES: readonly LanguageInfo[] = [
  { id: PLAIN_ID, name: PLAIN_TEXT, ext: ['.txt', '.text', '.log'], aliases: ['text', 'plain', 'none', 'pgp'] },
  { id: 'abap', name: 'ABAP', ext: ['.abap'] },
  { id: 'apex', name: 'Apex', ext: ['.cls'] },
  { id: 'azcli', name: 'Azure CLI', ext: ['.azcli'] },
  { id: 'bat', name: 'Batch', ext: ['.bat', '.cmd'], aliases: ['batch', 'cmd'] },
  { id: 'bicep', name: 'Bicep', ext: ['.bicep'] },
  { id: 'c', name: 'C', ext: ['.c', '.h'] },
  { id: 'cameligo', name: 'Cameligo', ext: ['.mligo'] },
  { id: 'clojure', name: 'Clojure', ext: ['.clj', '.cljs', '.cljc', '.edn'] },
  { id: 'coffeescript', name: 'CoffeeScript', ext: ['.coffee'], aliases: ['coffee'] },
  { id: 'conf', name: 'Config', ext: ['.conf', '.cnf', '.cfg', '.rc'], aliases: ['config', 'configuration', 'properties files'] },
  {
    id: 'cpp',
    name: 'C++',
    ext: ['.cpp', '.cc', '.cxx', '.c++', '.hpp', '.hh', '.hxx', '.h++', '.inl', '.ipp', '.tpp', '.cu', '.cuh', '.ino', '.pde'],
    aliases: ['c++', 'cplusplus'],
  },
  { id: 'csharp', name: 'C#', ext: ['.cs', '.csx', '.cake'], aliases: ['c#', 'cs'] },
  { id: 'csp', name: 'CSP', ext: ['.csp'] },
  { id: 'css', name: 'CSS', ext: ['.css'], aliases: ['stylus', 'closure stylesheets (gss)'] },
  { id: 'cypher', name: 'Cypher', ext: ['.cypher', '.cyp'] },
  { id: 'dart', name: 'Dart', ext: ['.dart'] },
  { id: 'diff', name: 'Diff', ext: ['.diff', '.patch', '.rej'], aliases: ['patch', 'udiff'] },
  { id: 'dockerfile', name: 'Dockerfile', ext: ['.dockerfile', '.containerfile'], files: ['dockerfile', 'containerfile'], aliases: ['docker'] },
  { id: 'ecl', name: 'ECL', ext: ['.ecl'] },
  { id: 'elixir', name: 'Elixir', ext: ['.ex', '.exs'] },
  { id: 'flow9', name: 'Flow9', ext: ['.flow'] },
  { id: 'fsharp', name: 'F#', ext: ['.fs', '.fsi', '.fsx', '.fsscript', '.ml', '.mli'], aliases: ['f#', 'ocaml'] },
  { id: 'go', name: 'Go', ext: ['.go'], aliases: ['golang'] },
  { id: 'graphql', name: 'GraphQL', ext: ['.graphql', '.gql'] },
  { id: 'handlebars', name: 'Handlebars', ext: ['.handlebars', '.hbs', '.mustache'] },
  { id: 'hcl', name: 'HCL / Terraform', ext: ['.tf', '.tfvars', '.hcl', '.nomad'], aliases: ['terraform', 'hcl'] },
  {
    id: 'html',
    name: 'HTML',
    ext: ['.html', '.htm', '.shtml', '.xhtml', '.jsp', '.asp', '.aspx', '.jshtm', '.vue', '.svelte', '.astro'],
    aliases: ['xhtml', 'vue', 'svelte', 'angular template'],
  },
  { id: 'ini', name: 'INI', ext: ['.ini', '.properties', '.gitconfig', '.editorconfig', '.desktop', '.inf', '.reg'], files: ['.gitconfig', '.gitattributes', '.editorconfig', 'php.ini', 'my.cnf'], aliases: ['properties'] },
  { id: 'java', name: 'Java', ext: ['.java', '.jav', '.groovy', '.gradle', '.gvy'], aliases: ['groovy'] },
  {
    id: 'javascript',
    name: 'JavaScript',
    ext: ['.js', '.mjs', '.cjs', '.jsx', '.es6'],
    files: ['jakefile'],
    aliases: ['js', 'jsx', 'node'],
  },
  {
    id: 'json',
    name: 'JSON',
    ext: ['.json', '.jsonc', '.json5', '.jsonl', '.ndjson', '.geojson', '.webmanifest', '.har', '.code-workspace', '.babelrc', '.eslintrc', '.jshintrc', '.jscsrc', '.bowerrc', '.swcrc', '.prettierrc'],
    files: ['composer.lock', 'package-lock.json', '.babelrc', '.eslintrc', '.prettierrc', '.jshintrc', '.swcrc', 'flake.lock'],
    aliases: ['json-ld', 'jsonc'],
  },
  { id: 'julia', name: 'Julia', ext: ['.jl'] },
  { id: 'kotlin', name: 'Kotlin', ext: ['.kt', '.kts'] },
  { id: 'less', name: 'Less', ext: ['.less'] },
  { id: 'lexon', name: 'Lexon', ext: ['.lex'] },
  { id: 'liquid', name: 'Liquid', ext: ['.liquid'] },
  { id: 'lua', name: 'Lua', ext: ['.lua', '.rockspec'] },
  { id: 'm3', name: 'Modula-3', ext: ['.m3', '.i3', '.mg', '.ig'] },
  { id: 'makefile', name: 'Makefile', ext: ['.mk', '.mak', '.make'], files: ['makefile', 'gnumakefile', 'bsdmakefile'], aliases: ['make'] },
  { id: 'markdown', name: 'Markdown', ext: ['.md', '.markdown', '.mdown', '.mkdn', '.mkd', '.mdwn', '.mdtxt', '.mdtext', '.markdn', '.ronn'], aliases: ['md'] },
  { id: 'mdx', name: 'MDX', ext: ['.mdx'] },
  { id: 'mips', name: 'MIPS', ext: ['.s'] },
  { id: 'msdax', name: 'DAX', ext: ['.dax', '.msdax'] },
  { id: 'mysql', name: 'MySQL' },
  { id: 'nginx', name: 'Nginx', ext: ['.nginx', '.nginxconf'], files: ['nginx.conf'] },
  { id: 'objective-c', name: 'Objective-C', ext: ['.m', '.mm'] },
  { id: 'pascal', name: 'Pascal', ext: ['.pas', '.p', '.dpr', '.lpr'] },
  { id: 'pascaligo', name: 'PascaLIGO', ext: ['.ligo'] },
  { id: 'perl', name: 'Perl', ext: ['.pl', '.pm', '.t', '.pod', '.psgi'] },
  { id: 'pgsql', name: 'PostgreSQL', ext: ['.pgsql', '.psql'], aliases: ['postgres'] },
  { id: 'php', name: 'PHP', ext: ['.php', '.php4', '.php5', '.phtml', '.ctp'] },
  { id: 'pla', name: 'PLA', ext: ['.pla'] },
  { id: 'postiats', name: 'ATS', ext: ['.dats', '.sats', '.hats'] },
  { id: 'powerquery', name: 'Power Query', ext: ['.pq', '.pqm'] },
  { id: 'powershell', name: 'PowerShell', ext: ['.ps1', '.psm1', '.psd1', '.ps1xml'], aliases: ['pwsh', 'ps1'] },
  { id: 'proto', name: 'Protocol Buffers', ext: ['.proto'], aliases: ['protobuf'] },
  { id: 'pug', name: 'Pug', ext: ['.pug', '.jade'], aliases: ['jade'] },
  {
    id: 'python',
    name: 'Python',
    ext: ['.py', '.pyw', '.pyi', '.pyx', '.rpy', '.cpy', '.gyp', '.gypi', '.bzl', '.star'],
    files: ['sconstruct', 'sconscript', 'wscript', 'snakefile', 'build.bazel', 'workspace', 'tiltfile'],
    aliases: ['py', 'python3', 'starlark'],
  },
  { id: 'qsharp', name: 'Q#', ext: ['.qs'] },
  { id: 'r', name: 'R', ext: ['.r', '.rhistory', '.rmd', '.rprofile', '.rt'] },
  { id: 'razor', name: 'Razor', ext: ['.cshtml', '.razor'] },
  { id: 'redis', name: 'Redis', ext: ['.redis'] },
  { id: 'redshift', name: 'Redshift' },
  { id: 'restructuredtext', name: 'reStructuredText', ext: ['.rst'], aliases: ['rst'] },
  {
    id: 'ruby',
    name: 'Ruby',
    ext: ['.rb', '.rbx', '.rjs', '.gemspec', '.rake', '.podspec', '.ru', '.thor', '.jbuilder', '.erb'],
    files: ['rakefile', 'gemfile', 'vagrantfile', 'podfile', 'brewfile', 'guardfile', 'capfile', 'fastfile', 'appfile', 'berksfile'],
    aliases: ['rb'],
  },
  { id: 'rust', name: 'Rust', ext: ['.rs', '.rlib'] },
  { id: 'sb', name: 'Small Basic', ext: ['.sb'] },
  { id: 'scala', name: 'Scala', ext: ['.scala', '.sc', '.sbt'] },
  { id: 'scheme', name: 'Scheme', ext: ['.scm', '.ss', '.sch', '.rkt'] },
  { id: 'scss', name: 'SCSS', ext: ['.scss', '.sass'], aliases: ['sass'] },
  {
    id: 'shell',
    name: 'Shell',
    ext: ['.sh', '.bash', '.zsh', '.ksh', '.fish', '.bats', '.ebuild', '.eclass', '.command', '.env', '.envrc'],
    files: [
      '.bashrc',
      '.bash_profile',
      '.bash_login',
      '.bash_logout',
      '.bash_aliases',
      '.profile',
      '.zshrc',
      '.zprofile',
      '.zshenv',
      '.zlogin',
      '.zlogout',
      '.kshrc',
      '.mkshrc',
      '.xinitrc',
      '.xprofile',
      '.xsession',
      '.envrc',
      '.env',
      'pkgbuild',
      'apkbuild',
      'crontab',
      'environment',
    ],
    aliases: ['sh', 'bash', 'zsh', 'shell script', 'shellscript'],
  },
  { id: 'sol', name: 'Solidity', ext: ['.sol'] },
  { id: 'aes', name: 'Sophia', ext: ['.aes'] },
  { id: 'sparql', name: 'SPARQL', ext: ['.rq', '.sparql'] },
  { id: 'sql', name: 'SQL', ext: ['.sql', '.ddl', '.dml'] },
  { id: 'st', name: 'Structured Text', ext: ['.st', '.iecst'] },
  { id: 'swift', name: 'Swift', ext: ['.swift'] },
  { id: 'systemverilog', name: 'SystemVerilog', ext: ['.sv', '.svh'] },
  { id: 'tcl', name: 'Tcl', ext: ['.tcl', '.tk', '.exp'] },
  { id: 'toml', name: 'TOML', ext: ['.toml'], files: ['cargo.lock', 'poetry.lock', 'pipfile', 'uv.lock', 'pdm.lock', 'gopkg.lock'] },
  { id: 'twig', name: 'Twig / Jinja', ext: ['.twig', '.j2', '.jinja', '.jinja2', '.njk'], aliases: ['jinja', 'jinja2', 'nunjucks'] },
  { id: 'typescript', name: 'TypeScript', ext: ['.ts', '.tsx', '.cts', '.mts'], aliases: ['ts', 'tsx'] },
  { id: 'typespec', name: 'TypeSpec', ext: ['.tsp'] },
  { id: 'vb', name: 'Visual Basic', ext: ['.vb', '.vbs', '.bas'], aliases: ['vbscript'] },
  { id: 'verilog', name: 'Verilog', ext: ['.v', '.vh'] },
  { id: 'wgsl', name: 'WGSL', ext: ['.wgsl'] },
  {
    id: 'xml',
    name: 'XML',
    ext: [
      '.xml',
      '.xsd',
      '.dtd',
      '.xsl',
      '.xslt',
      '.svg',
      '.svgz',
      '.plist',
      '.xaml',
      '.csproj',
      '.vbproj',
      '.fsproj',
      '.props',
      '.targets',
      '.nuspec',
      '.resx',
      '.wsdl',
      '.rss',
      '.atom',
      '.kml',
      '.gpx',
      '.jnlp',
      '.pom',
      '.iml',
      '.config',
      '.xul',
      '.opf',
    ],
    aliases: ['svg'],
  },
  { id: 'yaml', name: 'YAML', ext: ['.yaml', '.yml', '.sls'], files: ['.clang-format', '.clang-tidy', 'yarn.lock'], aliases: ['yml'] },
]

const byId = new Map<string, LanguageInfo>()
const byKey = new Map<string, LanguageInfo>()
const byExt = new Map<string, LanguageInfo>()
const byFile = new Map<string, LanguageInfo>()
for (const l of LANGUAGES) {
  byId.set(l.id, l)
  byKey.set(l.id, l)
  byKey.set(l.name.toLowerCase(), l)
  for (const a of l.aliases ?? []) if (!byKey.has(a)) byKey.set(a, l)
  for (const e of l.ext ?? []) if (!byExt.has(e)) byExt.set(e, l)
  for (const f of l.files ?? []) byFile.set(f, l)
}

/** A language by Monaco id, display name or alias (case-insensitive; also CodeMirror-era names). Null: unknown. */
export function findLanguage(name: string | null | undefined): LanguageInfo | null {
  if (!name) return null
  const n = name.trim().toLowerCase()
  return byKey.get(n) ?? byKey.get(n.replace(/\s+/g, ' ')) ?? null
}

export function languageById(id: string | null | undefined): LanguageInfo | null {
  return id ? (byId.get(id) ?? null) : null
}

/** Display name of a Monaco language id (the id itself for languages outside the table). */
export function languageName(id: string | null | undefined): string {
  if (!id) return PLAIN_TEXT
  return byId.get(id)?.name ?? id
}

/** Language names for the picker (sorted, "Plain Text" first). */
export function languageNames(): string[] {
  return [PLAIN_TEXT, ...LANGUAGES.filter((l) => l.id !== PLAIN_ID).map((l) => l.name).sort((a, b) => a.localeCompare(b))]
}

/** Extensions shown next to names in the picker. */
export function languageHint(name: string): string {
  const l = findLanguage(name)
  if (!l) return ''
  return (l.ext ?? []).slice(0, 4).join(' ')
}

// ---------------------------------------------------------------------------------------------------------------------
// detection
// ---------------------------------------------------------------------------------------------------------------------

/** Full-path rules (first match wins), evaluated before the file name. */
const PATH_RULES: [RegExp, string][] = [
  [/(^|\/)nginx\/(sites-(available|enabled)|conf\.d|snippets|modules-(available|enabled)|streams-(available|enabled)|http\.d|stream\.d)\/[^/]+$|(^|\/)nginx\/[^/]+\.conf$|(^|\/)openresty\/.*\.conf$/i, 'nginx'],
  [/(^|\/)(systemd|systemd\/(system|user|network))\/.+\.(service|socket|timer|mount|automount|target|path|slice|network|netdev|link|swap|scope)$/i, 'ini'],
  [/(^|\/)\.ssh\/(config|authorized_keys|known_hosts)$|(^|\/)ssh\/(sshd?_config|sshd_config\.d\/[^/]+|ssh_config\.d\/[^/]+)$/i, 'conf'],
  [/(^|\/)(cron\.d|cron\.daily|cron\.hourly|cron\.weekly|cron\.monthly|profile\.d|init\.d|rc\.d)\/[^/.]+$|(^|\/)etc\/(profile|bashrc|bash\.bashrc|zshrc|environment|rc\.local)$/i, 'shell'],
  [/(^|\/)(apache2|httpd)\/.+\.conf$|(^|\/)\.htaccess$/i, 'conf'],
  [/(^|\/)\.github\/workflows\/[^/]+$|(^|\/)(docker-)?compose[^/]*\.ya?ml$/i, 'yaml'],
  [/(^|\/)\.kube\/config$/i, 'yaml'],
]

/** File-name rules (first match wins), evaluated before the extension table. */
const NAME_RULES: [RegExp, string][] = [
  [/^(dockerfile|containerfile)([.-].+)?$|\.(dockerfile|containerfile)$/i, 'dockerfile'],
  [/^nginx.*\.conf$|\.nginx$/i, 'nginx'],
  [/^(makefile|gnumakefile|bsdmakefile)$|\.(mk|mak)$/i, 'makefile'],
  [/^\.env(\..+)?$|\.env$/i, 'shell'],
  [/^(jenkinsfile|.+\.jenkinsfile)$|\.groovy$/i, 'java'],
  [/\.(ya?ml)\.(j2|jinja2?|tmpl|tpl)$/i, 'yaml'],
  [/\.(json)\.(j2|jinja2?|tmpl|tpl)$/i, 'json'],
  [/\.(conf|cfg|cnf)\.(j2|jinja2?|tmpl|tpl|erb)$/i, 'conf'],
  [/\.(j2|jinja2?)$/i, 'twig'],
  [/\.(service|socket|timer|mount|automount|target|path|slice|network|netdev|link|swap)$/i, 'ini'],
  [/\.(repo|list)$|^(sources\.list)$/i, 'conf'],
  [/^(sshd?_config|ssh_config|sysctl\.conf|limits\.conf|hosts|hosts\.(allow|deny)|fstab|crypttab|resolv\.conf|nsswitch\.conf|sudoers|exports|inittab|logrotate\.conf|rsyslog\.conf|syslog\.conf|ntp\.conf|chrony\.conf|haproxy\.cfg|squid\.conf|named\.conf|smb\.conf|krb5\.conf|ldap\.conf|login\.defs|adduser\.conf|mke2fs\.conf|redis\.conf|sentinel\.conf|postgresql\.conf|pg_hba\.conf|pg_ident\.conf|mosquitto\.conf|wgetrc|\.wgetrc|\.curlrc|\.htpasswd|\.inputrc|inputrc|\.screenrc|\.tmux\.conf|tmux\.conf|\.vimrc|vimrc|\.gitignore|\.dockerignore|\.npmignore|\.gitmodules|\.npmrc|npmrc|\.pypirc|\.yarnrc)$/i, 'conf'],
  [/\.(ignore|gitignore|dockerignore)$/i, 'conf'],
  [/\.(gradle\.kts)$/i, 'kotlin'],
  [/\.d\.ts$/i, 'typescript'],
]

const SHEBANG: [RegExp, string][] = [
  [/^(ba|z|k|da|a|mk|c|tc|fi)?sh$|^busybox$|^openrc-run$|^fish$/, 'shell'],
  [/^python[\d.]*$|^pypy[\d.]*$|^uv$/, 'python'],
  [/^(node|nodejs|deno|bun|qjs|rhino|zx)$/, 'javascript'],
  [/^(ts-node|tsx)$/, 'typescript'],
  [/^perl[\d.]*$/, 'perl'],
  [/^ruby[\d.]*$|^jruby$/, 'ruby'],
  [/^php[\d.]*$/, 'php'],
  [/^lua[\d.]*$|^luajit$/, 'lua'],
  [/^rscript$/i, 'r'],
  [/^(pwsh|powershell)$/i, 'powershell'],
  [/^(tclsh|wish|expect)[\d.]*$/, 'tcl'],
  [/^groovy$/, 'java'],
  [/^scala$/, 'scala'],
  [/^julia$/, 'julia'],
  [/^(make|gmake)$/, 'makefile'],
  [/^nft$/, 'conf'],
]

/** Language of a "#!" first line (null when it is not a shebang or names no known interpreter). */
export function fromShebang(firstLine: string): LanguageInfo | null {
  if (!firstLine.startsWith('#!')) return null
  const parts = firstLine.slice(2).trim().split(/\s+/)
  let prog = parts[0]?.split('/').pop() ?? ''
  if (prog === 'env') {
    const rest = parts.slice(1).filter((p) => !p.startsWith('-') && !p.includes('='))
    prog = rest[0]?.split('/').pop() ?? ''
  }
  for (const [re, id] of SHEBANG) if (re.test(prog)) return byId.get(id) ?? null
  return null
}

const MODE_ALIASES: Record<string, string> = { sh: 'shell', bash: 'shell', zsh: 'shell', js: 'javascript', py: 'python', ts: 'typescript', yml: 'yaml', dosini: 'ini', cfg: 'conf', jinja: 'twig', make: 'makefile' }

function fromContent(text: string): LanguageInfo | null {
  const head = text.slice(0, 2000).trimStart()
  if (/^<\?xml[\s?]/.test(head)) return byId.get('xml') ?? null
  if (/^<!doctype html|^<html[\s>]/i.test(head)) return byId.get('html') ?? null
  if (/^<svg[\s>]/i.test(head)) return byId.get('xml') ?? null
  if (/^(diff --git |diff -[a-z]*u|--- \S.*\n\+\+\+ |Index: \S.*\n={10})/.test(head)) return byId.get('diff') ?? null
  if (/^\{\s*"|^\[\s*[{"\d\]]/.test(head) && text.length < 2_000_000) {
    try {
      JSON.parse(text)
      return byId.get('json') ?? null
    } catch {
      /* not JSON */
    }
  }
  if (/^%YAML|^---\s*\n[\w-]+:\s|^apiVersion:\s|^kind:\s\w+/.test(head)) return byId.get('yaml') ?? null
  if (/^(user|worker_processes|events|http|server|upstream)\s[^\n]*[;{]/m.test(head.slice(0, 600)) && /\bserver\s*\{|\blocation\s+[^{]+\{|\bworker_processes\b/.test(head)) {
    return byId.get('nginx') ?? null
  }
  // Emacs / vim modelines in the first lines
  const top = head.slice(0, 300)
  const mode = /-\*-\s*(?:mode:\s*)?([\w+-]+)\s*;?.*-\*-/.exec(top)?.[1] ?? /\bvim?:.*\b(?:ft|filetype)=([\w+-]+)/.exec(top)?.[1]
  if (mode) {
    const m = mode.toLowerCase()
    const l = findLanguage(MODE_ALIASES[m] ?? m)
    if (l) return l
  }
  return null
}

function byExtension(name: string): LanguageInfo | null {
  const lower = name.toLowerCase()
  // longest extension first: "archive.tar.gz" → ".tar.gz", ".gz"
  for (let i = lower.indexOf('.', 1); i >= 0; i = lower.indexOf('.', i + 1)) {
    const l = byExt.get(lower.slice(i))
    if (l) return l
  }
  // dot files named like an extension (".babelrc")
  if (lower.startsWith('.')) return byExt.get(lower) ?? null
  return null
}

/**
 * Best language for a file: path rules → name rules → exact file names → extension → shebang → content. Null means
 * plain text.
 */
export function detectLanguage(path: string, text: string): LanguageInfo | null {
  const name = path.split(/[\\/]/).pop() ?? path
  const norm = path.replace(/\\/g, '/')
  for (const [re, id] of PATH_RULES) if (re.test(norm)) return byId.get(id) ?? null
  for (const [re, id] of NAME_RULES) if (re.test(name)) return byId.get(id) ?? null
  const exact = byFile.get(name.toLowerCase())
  if (exact) return exact
  const ext = name ? byExtension(name) : null
  // Plain-text extensions (.txt, .log) and generic config ones still let a shebang or the content decide.
  if (ext && ext.id !== PLAIN_ID && ext.id !== 'conf') return ext
  const nl = text.indexOf('\n')
  const first = (nl < 0 ? text : text.slice(0, nl)).replace(/\r$/, '')
  const sb = fromShebang(first)
  if (sb) return sb
  const c = fromContent(text)
  if (c) return c
  return ext && ext.id !== PLAIN_ID ? ext : null
}
