/* oxlint-disable no-control-regex -- detecting and removing control characters is the purpose of this module */
/*
 * Paste safety (TERM-17): analysis of clipboard text before it reaches a shell, and sanitising.
 *
 *   analyzePaste(text) → { lines, multiline, warnings[], hasHiddenChars, ... }
 *   sanitizePaste(text, {stripHidden}) → text without bracketed-paste markers (always) and, optionally, without
 *                                        control / invisible / bidi characters.
 *
 * The analysis is heuristic: it flags common "copy-paste from the web" attacks (hidden characters, bracketed-paste
 * escape injection, homographs, curl|sh...) and obviously destructive commands. It is a speed bump, not a sandbox.
 */

export type PasteSeverity = 'danger' | 'warning' | 'info'

export interface PasteWarning {
  id: string
  severity: PasteSeverity
  message: string
}

export interface PasteAnalysis {
  length: number
  /** Number of lines that would be submitted (a trailing newline counts as submitting the last line). */
  lines: number
  multiline: boolean
  /** Ends with a newline: the last line executes immediately without bracketed paste. */
  endsWithNewline: boolean
  /** Contains control / invisible / bidi characters or escape sequences. */
  hasHiddenChars: boolean
  warnings: PasteWarning[]
  /** Highest severity among the warnings. */
  severity: PasteSeverity | null
}

// Bracketed paste markers: an embedded end marker lets pasted text escape the paste "quote" (injection).
const BRACKET_MARKERS = /\x1b\[20[01]~/g
// C0 controls except TAB, LF, CR; DEL; C1 controls.
const CONTROL_CHARS = /[\x00-\x08\x0b\x0c\x0e-\x1f\x7f\x80-\x9f]/g
// Zero-width, word joiner, BOM, soft hyphen, invisible separators / operators.
const INVISIBLE_CHARS = /[\u00ad\u180e\u200b-\u200d\u2060-\u2064\ufeff]/g
// Bidirectional overrides / isolates ("Trojan Source").
const BIDI_CHARS = /[\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/g
const NBSP = /[\u00a0\u2007\u202f]/
const LINE_SEP = /[\u2028\u2029]/

interface Rule {
  id: string
  severity: PasteSeverity
  re: RegExp
  message: string
}

// Command patterns are matched per line (after joining backslash continuations).
const RULES: Rule[] = [
  { id: 'pipe-shell', severity: 'danger', re: /\b(curl|wget|fetch|iwr|invoke-webrequest)\b[^|\n]*\|\s*(sudo\s+)?(ba|z|da|k|fi)?sh\b/i, message: 'Downloads a script and pipes it into a shell.' },
  { id: 'pipe-interpreter', severity: 'danger', re: /\b(curl|wget)\b[^|\n]*\|\s*(sudo\s+)?(python\d?|perl|ruby|node|php)\b/i, message: 'Downloads code and pipes it into an interpreter.' },
  { id: 'iex', severity: 'danger', re: /\b(iex|invoke-expression)\b.*\b(downloadstring|iwr|invoke-webrequest|irm|invoke-restmethod)\b|\b(irm|iwr)\b[^|\n]*\|\s*iex\b/i, message: 'Downloads and executes PowerShell code.' },
  { id: 'rm-rf', severity: 'danger', re: /\brm\s+(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|(-[a-z]+\s+)*(-r|--recursive)\s+(-[a-z]+\s+)*(-f|--force)|(-[a-z]+\s+)*(-f|--force)\s+(-[a-z]+\s+)*(-r|--recursive))\b/i, message: 'Recursively force-deletes files (rm -rf).' },
  { id: 'rm-root', severity: 'danger', re: /\brm\s+(-\S+\s+)*(\/|~|\/\*|\$home|\*)(\s|$)/i, message: 'Deletes the root, home or every file in the directory.' },
  { id: 'mkfs', severity: 'danger', re: /\b(mkfs(\.\w+)?|mke2fs|wipefs|fdisk|parted|sfdisk)\b/i, message: 'Creates or rewrites filesystems / partition tables.' },
  { id: 'dd-dev', severity: 'danger', re: /\bdd\b[^\n]*\bof=\/dev\//i, message: 'Writes raw data to a device (dd of=/dev/...).' },
  { id: 'redirect-dev', severity: 'danger', re: />\s*\/dev\/(sd|nvme|hd|vd|xvd|mmcblk|disk)/i, message: 'Overwrites a block device.' },
  { id: 'fork-bomb', severity: 'danger', re: /:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:/, message: 'Fork bomb.' },
  { id: 'chmod-root', severity: 'danger', re: /\b(chmod|chown|chgrp)\s+(-\S+\s+)*-R\b[^\n]*\s\/(\s|$)/i, message: 'Recursively changes permissions / ownership from the root directory.' },
  { id: 'shell-rc', severity: 'warning', re: />>?\s*~?\/?[\w./-]*\.(bashrc|bash_profile|profile|zshrc|zprofile|zshenv|config\/fish\/config\.fish)\b/i, message: 'Modifies a shell startup file.' },
  { id: 'authorized-keys', severity: 'danger', re: />>?\s*~?\/?[\w./-]*\.ssh\/authorized_keys\b/i, message: 'Adds an SSH key to authorized_keys.' },
  { id: 'crontab', severity: 'warning', re: /\bcrontab\s+(-r\b|-\s|\S+\.\w+)/i, message: 'Replaces or removes the crontab.' },
  { id: 'power', severity: 'warning', re: /(^|[;&|]\s*|sudo\s+)(shutdown|reboot|halt|poweroff|init\s+[06])\b/i, message: 'Shuts down or reboots the machine.' },
  { id: 'kill-all', severity: 'warning', re: /\bkill(all)?\s+(-\S+\s+)*-1\b|\bkillall5\b/i, message: 'Kills every process.' },
  { id: 'firewall-flush', severity: 'warning', re: /\b(iptables|ip6tables)\s+(-\S+\s+)*-F\b|\bnft\s+flush\s+ruleset\b|\bufw\s+disable\b/i, message: 'Flushes / disables the firewall.' },
  { id: 'sql-drop', severity: 'warning', re: /\b(drop\s+(database|schema|table)|truncate\s+table)\b/i, message: 'Drops or truncates database objects.' },
  { id: 'history-off', severity: 'warning', re: /\bunset\s+HISTFILE\b|\bHISTFILE=\/dev\/null\b|\bhistory\s+-c\b/, message: 'Disables or clears the shell history.' },
  { id: 'base64-exec', severity: 'danger', re: /\bbase64\s+(-d|--decode)\b[^\n|]*\|\s*(sudo\s+)?(ba|z)?sh\b/i, message: 'Decodes hidden content and executes it.' },
  { id: 'eval-remote', severity: 'danger', re: /\beval\s+["'`]?\$\((curl|wget)\b/i, message: 'Evaluates code downloaded from the network.' },
  { id: 'sudo', severity: 'info', re: /(^|[;&|(]\s*)sudo\s+\S/, message: 'Runs a command with elevated privileges (sudo).' },
]

function lineCount(text: string): { lines: number; endsWithNewline: boolean } {
  const normalized = text.replace(/\r\n?/g, '\n')
  const endsWithNewline = normalized.endsWith('\n')
  const parts = normalized.split('\n')
  if (endsWithNewline) parts.pop()
  return { lines: Math.max(1, parts.length), endsWithNewline }
}

/** Words mixing Latin letters with Cyrillic / Greek look-alikes (homograph attacks). */
function hasMixedScriptWord(text: string): boolean {
  const words = text.match(/[\p{L}\p{M}\d_.-]{2,}/gu)
  if (!words) return false
  for (const w of words) {
    const latin = /\p{Script=Latin}/u.test(w)
    const other = /[\p{Script=Cyrillic}\p{Script=Greek}\p{Script=Armenian}\p{Script=Cherokee}]/u.test(w)
    if (latin && other) return true
  }
  return false
}

export function analyzePaste(text: string): PasteAnalysis {
  const warnings: PasteWarning[] = []
  const add = (w: PasteWarning) => {
    if (!warnings.some((x) => x.id === w.id)) warnings.push(w)
  }

  BRACKET_MARKERS.lastIndex = 0
  const bracket = BRACKET_MARKERS.test(text)
  if (bracket) add({ id: 'bracket-injection', severity: 'danger', message: 'Contains bracketed-paste escape sequences that can break out of the paste and run commands.' })
  const esc = /\x1b/.test(text)
  if (esc && !bracket) add({ id: 'escape', severity: 'danger', message: 'Contains terminal escape sequences.' })
  CONTROL_CHARS.lastIndex = 0
  const ctrl = CONTROL_CHARS.test(text.replace(/\x1b/g, ''))
  if (ctrl) add({ id: 'control', severity: 'danger', message: 'Contains hidden control characters.' })
  INVISIBLE_CHARS.lastIndex = 0
  const invisible = INVISIBLE_CHARS.test(text)
  if (invisible) add({ id: 'invisible', severity: 'danger', message: 'Contains invisible (zero-width) characters.' })
  BIDI_CHARS.lastIndex = 0
  const bidi = BIDI_CHARS.test(text)
  if (bidi) add({ id: 'bidi', severity: 'danger', message: 'Contains bidirectional control characters that can hide what really runs.' })
  if (NBSP.test(text)) add({ id: 'nbsp', severity: 'warning', message: 'Contains non-breaking spaces (they look like spaces but are not).' })
  if (LINE_SEP.test(text)) add({ id: 'line-sep', severity: 'warning', message: 'Contains Unicode line separators.' })
  if (hasMixedScriptWord(text)) add({ id: 'homograph', severity: 'warning', message: 'Contains words mixing Latin with look-alike Cyrillic / Greek letters (possible homograph).' })

  // Command heuristics on logical lines (backslash-newline continuations joined), limited for very large pastes.
  const logical = text.slice(0, 200_000).replace(/\\\r?\n/g, ' ').split(/\r?\n/)
  for (const line of logical) {
    if (!line.trim()) continue
    for (const r of RULES) if (r.re.test(line)) add({ id: r.id, severity: r.severity, message: r.message })
  }

  const { lines, endsWithNewline } = lineCount(text)
  if (text.length > 100_000) add({ id: 'large', severity: 'warning', message: `Large paste (${Math.round(text.length / 1024)} KiB).` })

  const rank: Record<PasteSeverity, number> = { info: 0, warning: 1, danger: 2 }
  let severity: PasteSeverity | null = null
  for (const w of warnings) if (!severity || rank[w.severity] > rank[severity]) severity = w.severity
  warnings.sort((a, b) => rank[b.severity] - rank[a.severity])

  return {
    length: text.length,
    lines,
    multiline: lines > 1 || endsWithNewline,
    endsWithNewline,
    hasHiddenChars: esc || ctrl || invisible || bidi || bracket,
    warnings,
    severity,
  }
}

/**
 * Prepare pasted text: bracketed-paste markers are always removed (they can never be legitimate paste content);
 * with `stripHidden`, control characters (except TAB/LF/CR), escape sequences, zero-width and bidi characters are
 * removed and non-breaking spaces become spaces.
 */
export function sanitizePaste(text: string, opts: { stripHidden: boolean }): string {
  let out = text.replace(BRACKET_MARKERS, '')
  if (!opts.stripHidden) return out
  // Whole CSI / OSC sequences first, so their parameters do not remain as visible junk.
  out = out
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '')
    .replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)?/g, '')
    .replace(/\x1b[@-_]/g, '')
    .replace(CONTROL_CHARS, '')
    .replace(INVISIBLE_CHARS, '')
    .replace(BIDI_CHARS, '')
    .replace(/[\u00a0\u2007\u202f]/g, ' ')
    .replace(/[\u2028\u2029]/g, '\n')
  return out
}

/** Split text into lines for paced pasting (each line is sent followed by CR). */
export function splitPasteLines(text: string): string[] {
  const normalized = text.replace(/\r\n?/g, '\n')
  const parts = normalized.split('\n')
  if (normalized.endsWith('\n')) parts.pop()
  return parts
}
