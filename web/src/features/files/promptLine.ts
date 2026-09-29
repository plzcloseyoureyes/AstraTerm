/*
 * Does a terminal's cursor line look like a shell waiting at its prompt? (Pure: unit-tested by
 * __tests__/promptline.test.mjs; used by terminalProbe.ts for "Open terminal here".)
 */

/** A prompt character followed by a space: bash / sh `$ `, root `# `, zsh `% `, fish / PowerShell `> `, `❯ ` `» ` `λ ` `➜ `. */
const PROMPT_END = /[$#%>❯»λ➜]\s+$/
/** cmd.exe: `C:\Users\me>` (no space). */
const CMD_PROMPT = /^\s*[A-Za-z]:\\[^<>|"]*>$/
/** Questions waiting for an answer — never a shell prompt: "Password: ", "(yes/no)? ", "Continue? [Y/n] ". */
const QUESTION_END = /[:?\]]\s*$/

/**
 * Classify the cursor line from the text before / after the cursor:
 *   busy     nothing before the cursor (a command printing lines), text right at the cursor (it moved into a
 *            line), or a question (password, confirmation)
 *   prompt   the text before the cursor ends like a prompt ("user@host:~$ ")
 *   maybe    something else (themed prompts such as "➜  proj git:(main) ✗ ", or a program's output line): the caller
 *            decides with what it knows about input and output activity
 */
export function cursorLineState(beforeCursor: string, afterCursor: string): 'prompt' | 'maybe' | 'busy' {
  // Text right at the cursor = the cursor moved into a line; text further right (zsh RPROMPT) does not count.
  if (/^\S/.test(afterCursor) || beforeCursor.trim() === '') return 'busy'
  if (PROMPT_END.test(beforeCursor) || CMD_PROMPT.test(beforeCursor)) return 'prompt'
  if (QUESTION_END.test(beforeCursor)) return 'busy'
  return 'maybe'
}

/** Input (as typed) that submits or discards the current line: Enter, Ctrl-C, Ctrl-D, Ctrl-U. */
export function endsLine(data: string): boolean {
  for (let i = 0; i < data.length; i++) {
    const c = data.charCodeAt(i)
    if (c === 0x0d || c === 0x0a || c === 0x03 || c === 0x04 || c === 0x15) return true
  }
  return false
}
