import assert from 'node:assert/strict'
import test from 'node:test'
import { InputLocks } from '../inputLock'

test('locks nest; the most recent reason wins; release is idempotent', () => {
  const seen = []
  const l = new InputLocks((r) => seen.push(r))
  assert.equal(l.locked, false)
  const a = l.acquire('ZMODEM transfer in progress')
  const b = l.acquire('trzsz upload in progress')
  assert.equal(l.reason, 'trzsz upload in progress')
  b()
  b()
  assert.equal(l.reason, 'ZMODEM transfer in progress')
  a()
  assert.equal(l.locked, false)
  assert.deepEqual(seen, ['ZMODEM transfer in progress', 'trzsz upload in progress', 'ZMODEM transfer in progress', null])
})

test('clear drops every lock', () => {
  const l = new InputLocks()
  const release = l.acquire('x')
  l.clear()
  assert.equal(l.locked, false)
  release()
  assert.equal(l.reason, null)
})
