// The password-prompt chip must not blink: redraws at a prompt keep it, it goes only when the prompt is gone.
import assert from 'node:assert/strict'
import test from 'node:test'
import { CHIP_SETTLE, CHIP_SHOW_DELAY, PromptChipController } from '../plugins/promptChip'

function fakeClock() {
  let now = 0
  let seq = 0
  const timers = new Map()
  return {
    setTimeout: (fn, ms) => (timers.set(++seq, { at: now + ms, fn }), seq),
    clearTimeout: (h) => void timers.delete(h),
    async advance(ms) {
      const end = now + ms
      for (;;) {
        const next = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0]
        if (!next) break
        timers.delete(next[0])
        now = next[1].at
        next[1].fn()
        await new Promise((r) => setImmediate(r)) // let the async resolve() finish
      }
      now = end
    },
  }
}

function setup(prompt = { key: 'password' }) {
  const clock = fakeClock()
  const changes = []
  let keeps = 0
  const c = new PromptChipController({
    clock,
    resolve: async () => prompt.key,
    onChange: (k) => changes.push(k),
    onKeep: () => keeps++,
  })
  return { c, clock, changes, prompt, keeps: () => keeps }
}

test('appears once output is quiet at a prompt', async () => {
  const { c, clock, changes } = setup()
  c.output()
  await clock.advance(CHIP_SHOW_DELAY - 1)
  assert.deepEqual(changes, [])
  await clock.advance(1)
  assert.deepEqual(changes, ['password'])
})

test('redraws at the prompt never hide the chip', async () => {
  const s = setup()
  s.c.output()
  await s.clock.advance(CHIP_SHOW_DELAY)
  for (let i = 0; i < 20; i++) {
    s.c.output() // a redraw every 100 ms (spinner, title update, resize)
    await s.clock.advance(100)
  }
  await s.clock.advance(CHIP_SETTLE)
  assert.deepEqual(s.changes, ['password']) // shown once, never hidden in between
  assert.equal(s.keeps(), 1) // re-placed once after the output settled
})

test('goes when the prompt is gone, only after the output settled', async () => {
  const s = setup()
  s.c.output()
  await s.clock.advance(CHIP_SHOW_DELAY)
  s.prompt.key = null // e.g. the program printed an error and a new shell prompt
  s.c.output()
  await s.clock.advance(CHIP_SETTLE - 1)
  assert.deepEqual(s.changes, ['password'])
  await s.clock.advance(1)
  assert.deepEqual(s.changes, ['password', null])
})

test('typing hides at once and cancels a pending check', async () => {
  const s = setup()
  s.c.output()
  await s.clock.advance(CHIP_SHOW_DELAY)
  s.c.output()
  s.c.input()
  assert.deepEqual(s.changes, ['password', null])
  await s.clock.advance(CHIP_SETTLE * 2)
  assert.deepEqual(s.changes, ['password', null])
})

test('a slow secret lookup that finishes after typing does not bring the chip back', async () => {
  const clock = fakeClock()
  const changes = []
  let release
  const c = new PromptChipController({ clock, resolve: () => new Promise((r) => (release = r)), onChange: (k) => changes.push(k) })
  c.output()
  await clock.advance(CHIP_SHOW_DELAY)
  c.input()
  release('password')
  await new Promise((r) => setImmediate(r))
  assert.deepEqual(changes, [])
})
