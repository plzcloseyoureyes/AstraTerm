#!/usr/bin/env node
/*
 * UI guardrail (docs/UX.md "Loading states"): loading feedback is built from the shared primitives in
 * web/src/components/ui (Spinner, Delayed, LoadingPane, BusyIcon, LoadingState, QueryState, Skeleton, ProgressBar,
 * StatusDot) and web/src/lib/useDelayedFlag — never from raw animation classes or private copies of the timing hook.
 *
 *   node scripts/lint-ui.mjs            (also: make lint-ui, cd web && npm run lint:ui)
 *   node scripts/lint-ui.mjs --update   rewrite the allowlist from the current findings (only to shrink it!)
 *
 * Rules (checked in web/src outside components/ui, lib and index.css):
 *   raw-animation   animate-spin / animate-pulse / animate-pulse-dot / animate-indeterminate class names
 *   skeleton        ad-hoc skeleton components or classes (use components/ui/skeleton through LoadingState / QueryState)
 *   delayed-hook    a local useDelayedFlag (or Delayed…/…Spinner component) definition instead of the shared one
 * Checked everywhere in web/src, the shared layer included (docs/UX.md "Motion & feedback": zero oscillation):
 *   oscillation     looping / blinking motion: pulse, ping, bounce, blink, breathe, heartbeat, caret-blink or
 *                   indeterminate animation classes; keyframes named like them; any `infinite` animation other than
 *                   the spinner's rotation (`spin`) and the ProgressBar's slide (`progress-slide`); tw-animate's
 *                   repeat-infinite
 *   caret           a blinking text caret: `cursorBlink: true` (xterm), Monaco `cursorBlinking` other than 'solid',
 *                   and CodeMirror (a file importing @codemirror/view, codemirror or @uiw/react-codemirror) without
 *                   `cursorBlinkRate: 0` (native fields are steady through `caret-animation: manual` in index.css)
 *
 * Escape hatch: a comment containing `lint-ui-allow: <reason>` on the same line or the line above.
 * Known pending findings (paths still under review by their owners) live in scripts/lint-ui-allowlist.json as
 * {file: {rule: count}}; a count may only go down. Anything new or above its count fails.
 */
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const src = path.join(root, 'web', 'src')
const allowlistPath = path.join(root, 'scripts', 'lint-ui-allowlist.json')
const exempt = [/^components\/ui\//, /^lib\//, /^index\.css$/]

const OSCILLATION_WORDS = 'pulse(?:-dot)?|ping|bounce|blink|caret-blink|breathe|heartbeat|flash|glow|shimmer|indeterminate'

/** Rules that apply to every file, the shared layer included. */
const globalRules = [
  {
    id: 'oscillation',
    test: (text) =>
      new RegExp(`(?<![-\\w])animate-(?:${OSCILLATION_WORDS})\\b`).test(text) ||
      new RegExp(`@keyframes\\s+[\\w-]*(?:${OSCILLATION_WORDS})`).test(text) ||
      /\brepeat-infinite\b/.test(text) ||
      (/\binfinite\b/.test(text) && /animation|--animate-/.test(text) && !/\b(?:spin|progress-slide)\b/.test(text)),
    hint: 'nothing may oscillate (docs/UX.md "Motion & feedback"): steady StatusDot / text, a delayed Spinner for long waits, a determinate ProgressBar',
  },
]

const CARET_HINT =
  'carets are steady (docs/UX.md "Motion & feedback"): xterm cursorBlink false, Monaco cursorBlinking "solid", CodeMirror drawSelection({ cursorBlinkRate: 0 })'
globalRules.push({
  id: 'caret',
  test: (text) => /\bcursorBlink\s*:\s*true\b/.test(text) || /\bcursorBlinking\s*:\s*['"`](?!solid['"`])/.test(text),
  hint: CARET_HINT,
})

/** Whole-file rules (every file): return the 0-based line to report, or -1. */
const fileRules = [
  {
    id: 'caret',
    find: (text, lines) =>
      /cursorBlinkRate\s*:\s*0\b/.test(text) ? -1 : lines.findIndex((l) => /from\s+['"](?:@codemirror\/view|codemirror|@uiw\/react-codemirror)['"]/.test(l)),
    hint: CARET_HINT,
  },
]

const rules = [
  { id: 'raw-animation', re: /\banimate-(?:spin|pulse(?:-dot)?|indeterminate)\b/, hint: 'use Spinner / BusyIcon / StatusDot / ProgressBar (components/ui)' },
  {
    id: 'skeleton',
    re: /\b(?:function|const)\s+[A-Z]\w*Skeleton\w*\b|className=["'`{][^\n]*\bskeleton\b/,
    hint: 'use Skeleton / SkeletonRows / SkeletonText (components/ui/skeleton) inside LoadingState / QueryState',
  },
  {
    id: 'delayed-hook',
    re: /\b(?:function|const)\s+(?:useDelayed\w*|Delayed\w+|\w*DelayedSpinner|\w*DelayedLoadingPane)\b/,
    hint: 'import useDelayedFlag from lib/useDelayedFlag, or use Spinner / Delayed / LoadingState (components/ui)',
  },
]

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name)
    if (statSync(p).isDirectory()) walk(p, out)
    else if (/\.(tsx?|css)$/.test(name)) out.push(p)
  }
  return out
}

const findings = [] // {file, rule, line, text}
for (const file of walk(src)) {
  const rel = path.relative(src, file).split(path.sep).join('/')
  const scoped = !exempt.some((re) => re.test(rel))
  const content = readFileSync(file, 'utf8')
  const lines = content.split('\n')
  for (const rule of fileRules) {
    const i = rule.find(content, lines)
    if (i < 0 || /lint-ui-allow:/.test(lines[i]) || (i > 0 && /lint-ui-allow:/.test(lines[i - 1]))) continue
    findings.push({ file: rel, rule: rule.id, line: i + 1, text: lines[i].trim() })
  }
  lines.forEach((text, i) => {
    for (const rule of [...globalRules, ...(scoped ? rules : [])]) {
      if (rule.re ? !rule.re.test(text) : !rule.test(text)) continue
      if (/lint-ui-allow:/.test(text) || (i > 0 && /lint-ui-allow:/.test(lines[i - 1]))) continue
      findings.push({ file: rel, rule: rule.id, line: i + 1, text: text.trim() })
    }
  })
}

const counts = {}
for (const f of findings) {
  counts[f.file] ??= {}
  counts[f.file][f.rule] = (counts[f.file][f.rule] ?? 0) + 1
}

if (process.argv.includes('--update')) {
  const sorted = Object.fromEntries(Object.keys(counts).sort().map((k) => [k, counts[k]]))
  writeFileSync(allowlistPath, JSON.stringify(sorted, null, 2) + '\n')
  console.log(`lint-ui: allowlist rewritten (${findings.length} findings in ${Object.keys(counts).length} files)`)
  process.exit(0)
}

let allow = {}
try {
  allow = JSON.parse(readFileSync(allowlistPath, 'utf8'))
} catch {
  /* no allowlist: everything counts */
}

let failed = 0
let pending = 0
const shrinkable = []
for (const [file, byRule] of Object.entries(counts)) {
  for (const [rule, n] of Object.entries(byRule)) {
    const allowed = allow[file]?.[rule] ?? 0
    if (n <= allowed) {
      pending += n
      if (n < allowed) shrinkable.push(`${file} (${rule}: ${allowed} → ${n})`)
      continue
    }
    const hint = [...globalRules, ...fileRules, ...rules].find((r) => r.id === rule).hint
    for (const f of findings.filter((x) => x.file === file && x.rule === rule)) {
      console.error(`web/src/${f.file}:${f.line}: [${rule}] ${f.text}\n    → ${hint}`)
    }
    if (allowed) console.error(`    (web/src/${file} is allowlisted for ${allowed} × ${rule}; found ${n})`)
    failed += n - allowed
  }
}
for (const [file, byRule] of Object.entries(allow)) {
  for (const rule of Object.keys(byRule)) if (!counts[file]?.[rule]) shrinkable.push(`${file} (${rule}: ${byRule[rule]} → 0)`)
}

if (shrinkable.length) console.log(`lint-ui: the allowlist can shrink (run with --update):\n  ${shrinkable.join('\n  ')}`)
if (failed) {
  console.error(`lint-ui: ${failed} finding(s). See docs/UX.md "Loading states".`)
  process.exit(1)
}
console.log(`lint-ui: ok${pending ? ` (${pending} known pending finding(s) in scripts/lint-ui-allowlist.json)` : ''}`)
