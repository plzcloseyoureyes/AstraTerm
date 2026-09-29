/*
 * Pure safety heuristics of the assistant (no DOM, no imports — unit-tested in tests/heuristics.test.mjs):
 *   cleanCommand           strip control / invisible characters from a suggested command
 *   isShellPromptPrefix    whether text before a typed `# request` is a shell prompt
 *   looksLikeSecretEntry   whether a "failed command" is a password typed at the wrong place
 */

// Characters a suggestion must never carry into a terminal: C0 controls except TAB/LF (escape sequences could hide
// text in the confirmation or end a bracketed paste early), DEL, C1 controls, zero-width and bidi-override characters.
// oxlint-disable-next-line no-control-regex
const UNSAFE_CHARS = /[\x00-\x08\x0b-\x1f\x7f-\x9f\u200b-\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]/g

/**
 * Clean a suggested command for the terminal: no control / invisible characters, no trailing newline, no leading
 * "$ " prompt copied by the model.
 */
export function cleanCommand(cmd: string): string {
  let t = cmd.replace(/\r\n?/g, '\n').replace(UNSAFE_CHARS, '').replace(/\n+$/, '')
  if (/^\$ \S/.test(t) && !t.includes('\n$ ')) t = t.slice(2)
  return t
}

// Interactive programs where `#` is a comment but which are not a shell: typing a comment there must not reach the
// assistant (PS2 continuation "> " inside heredocs, Python/SQL REPLs, irb…).
const NOT_SHELL_PROMPT_RE = /(^\s*>|>>>|\.\.\.|\b(?:mysql|mariadb|sqlite|sqlite3|redis[^>]*|mongo|irb[^>]*|pry[^>]*|iex[^>]*|gdb|lldb)>|\[[^\]]*\]>|\w+[=-][#>])\s*$/i
const SHELL_PROMPT_END_RE = /[$#%>❯»➜λ→]\s*$/

/** Whether the text before a typed `# request` is a shell prompt (heuristic, used without OSC 133). */
export function isShellPromptPrefix(prefix: string): boolean {
  if (!prefix.trim()) return false
  return SHELL_PROMPT_END_RE.test(prefix) && !NOT_SHELL_PROMPT_RE.test(prefix)
}

const SECRET_PROMPT_RE = /(pass(word|phrase|code)?|\bpin\b|token|secret|otp|verification code)[^:\n]{0,40}:\s*$|sorry, try again|timed out reading password|authentication fail/i

/**
 * A "failed command" that is really a secret typed at the wrong place (the classic `hunter2: command not found` after
 * a sudo prompt timed out): one token, and a password prompt just above it or a password-like token. Never offered to
 * the assistant.
 */
export function looksLikeSecretEntry(command: string, linesAbove: string[], exitCode?: number, output = ''): boolean {
  const cmd = command.trim()
  if (!cmd || /\s/.test(cmd)) return false
  if (linesAbove.some((l) => SECRET_PROMPT_RE.test(l.trimEnd()))) return true
  const notFound = exitCode === 127 || /not found|not recognized/i.test(output)
  if (!notFound || cmd.length < 6 || cmd.includes('/')) return false
  const classes = [/[a-z]/, /[A-Z]/, /[0-9]/, /[^A-Za-z0-9]/].filter((re) => re.test(cmd)).length
  return classes >= 3
}
