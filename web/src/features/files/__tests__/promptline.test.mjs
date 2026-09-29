// "Open terminal here" only types `cd` into a shell waiting at its prompt (promptLine.ts).
import assert from 'node:assert/strict'
import test from 'node:test'
import { cursorLineState, endsLine } from '../promptLine'

test('prompts: bash, root, zsh, fish, PowerShell, cmd, starship', () => {
  for (const p of ['ssh1:~$ ', 'test@host:/tmp$ ', 'root@box:~# ', 'host% ', 'user@h ~> ', 'PS C:\\Users\\me> ', 'C:\\>', 'C:\\Users\\me>', '❯ ', 'λ ']) {
    assert.equal(cursorLineState(p, ''), 'prompt', p)
  }
})

test('questions are never prompts: passwords, confirmations', () => {
  for (const q of ['[sudo] password for test: ', 'Password:', 'Are you sure you want to continue connecting (yes/no)? ', 'Continue? [Y/n] ']) {
    assert.equal(cursorLineState(q, ''), 'busy', q)
  }
})

test('themed prompts and output lines are undecided (input / output activity decides)', () => {
  assert.equal(cursorLineState('➜  proj git:(main) ✗ ', ''), 'maybe')
  assert.equal(cursorLineState('Downloading 45%', ''), 'maybe')
  assert.equal(cursorLineState('user@host ~/src (main)', ''), 'maybe')
})

test('busy: output in progress, the cursor inside a line', () => {
  assert.equal(cursorLineState('', ''), 'busy') // a running command printing lines
  assert.equal(cursorLineState('   ', '   '), 'busy')
  assert.equal(cursorLineState('ssh1:~$ ', 'ls'), 'busy') // cursor moved back into typed text
})

test('a right-side prompt (zsh RPROMPT) is not typed text', () => {
  assert.equal(cursorLineState('host% ', '                      [10:23:45]'), 'prompt')
})

test('line-ending input: Enter, Ctrl-C, Ctrl-D, Ctrl-U; not ordinary keys', () => {
  for (const d of ['\r', 'ls\r', '\x03', '\x04', '\x15', 'a\n']) assert.equal(endsLine(d), true, JSON.stringify(d))
  for (const d of ['a', 'ls -l', '\x1b[A', '\x7f', '\t']) assert.equal(endsLine(d), false, JSON.stringify(d))
})
