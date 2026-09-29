/*
 * Safety heuristics of the AI assistant (pure module):
 *   cd web && node --import ./src/features/termtransfer/tests/register.mjs --test src/features/ai/tests/
 */
import assert from 'node:assert/strict'
import { describe, it } from 'node:test'
import { cleanCommand, isShellPromptPrefix, looksLikeSecretEntry } from '../heuristics.ts'

describe('cleanCommand', () => {
  it('strips escape sequences, C1 and bidi/zero-width characters', () => {
    assert.equal(cleanCommand('ls\x1b[201~; rm -rf ~\u202e\n'), 'ls[201~; rm -rf ~')
    assert.equal(cleanCommand('echo\u200b hi\x9b'), 'echo hi')
    assert.equal(cleanCommand('a\r\nb\r\n'), 'a\nb')
    assert.equal(cleanCommand('$ ls -la'), 'ls -la')
    assert.equal(cleanCommand('printf "a\tb"'), 'printf "a\tb"')
  })
})

describe('isShellPromptPrefix', () => {
  for (const p of ['bob@web1:~$ ', 'root@web1:/etc# ', 'host% ', '❯ ', 'PS C:\\Users\\bob> ', '[bob@web1 ~]$ ']) {
    it(`accepts ${JSON.stringify(p)}`, () => assert.equal(isShellPromptPrefix(p), true))
  }
  for (const p of ['', '> ', '>>> ', '... ', 'mysql> ', 'MariaDB [app]> ', 'sqlite> ', 'postgres=# ', 'app=> ', 'irb(main):001:0> ', 'some output ']) {
    it(`rejects ${JSON.stringify(p)}`, () => assert.equal(isShellPromptPrefix(p), false))
  }
})

describe('looksLikeSecretEntry', () => {
  it('detects a password typed after a sudo prompt', () => {
    assert.equal(looksLikeSecretEntry('hunter2', ['[sudo] password for bob:', 'sudo: timed out reading password', 'bob@web1:~$ hunter2'], 127), true)
    assert.equal(looksLikeSecretEntry('Password:', []), false)
  })
  it('detects password-like tokens that were "not found"', () => {
    assert.equal(looksLikeSecretEntry('Tr0ub4dor&3', [], 127), true)
    assert.equal(looksLikeSecretEntry('Xk9#pQ2m', [], undefined, 'bash: Xk9#pQ2m: command not found'), true)
  })
  it('keeps real failed commands', () => {
    assert.equal(looksLikeSecretEntry('gti', [], 127), false)
    assert.equal(looksLikeSecretEntry('make build', ['Password:'], 2), false)
    assert.equal(looksLikeSecretEntry('./deploy.sh', [], 127), false)
    assert.equal(looksLikeSecretEntry('kubectl', [], 1), false)
  })
})
