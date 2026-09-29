// DelayedFlag: the timing behind every loading indicator (docs/UX.md "Loading states"), with a fake clock.
import assert from 'node:assert/strict'
import test from 'node:test'
import { DelayedFlag } from '../useDelayedFlag'

function fakeClock() {
  let now = 0
  let seq = 0
  const timers = new Map()
  return {
    now: () => now,
    setTimeout: (fn, ms) => (timers.set(++seq, { at: now + ms, fn }), seq),
    clearTimeout: (h) => void timers.delete(h),
    advance(ms) {
      const end = now + ms
      for (;;) {
        const next = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0]
        if (!next) break
        timers.delete(next[0])
        now = next[1].at
        next[1].fn()
      }
      now = end
    },
    pending: () => timers.size,
  }
}

function setup(opts) {
  const clock = fakeClock()
  const changes = []
  const flag = new DelayedFlag((v) => changes.push([clock.now(), v]), opts, clock)
  return { clock, changes, flag }
}

test('a fast operation never shows anything', () => {
  const { clock, changes, flag } = setup()
  flag.set(true)
  clock.advance(299)
  flag.set(false)
  clock.advance(1000)
  assert.deepEqual(changes, [])
  assert.equal(clock.pending(), 0)
})

test('shows after 300 ms and stays at least 600 ms', () => {
  const { clock, changes, flag } = setup()
  flag.set(true)
  clock.advance(310)
  flag.set(false) // done 10 ms after it appeared
  clock.advance(1000)
  assert.deepEqual(changes, [
    [300, true],
    [900, false],
  ])
})

test('a long operation hides as soon as it ends', () => {
  const { clock, changes, flag } = setup()
  flag.set(true)
  clock.advance(2000)
  flag.set(false)
  assert.deepEqual(changes, [
    [300, true],
    [2000, false],
  ])
})

test('busy again during the minimum time keeps it shown without a blink', () => {
  const { clock, changes, flag } = setup()
  flag.set(true)
  clock.advance(350)
  flag.set(false)
  clock.advance(100)
  flag.set(true)
  clock.advance(1000)
  assert.deepEqual(changes, [[300, true]])
  flag.set(false)
  assert.deepEqual(changes.at(-1), [1450, false])
})

test('repeated set(true) does not restart the delay', () => {
  const { clock, changes, flag } = setup()
  flag.set(true)
  clock.advance(200)
  flag.set(true)
  clock.advance(100)
  assert.deepEqual(changes, [[300, true]])
})

test('cancel (effect cleanup) keeps the state; the next set re-arms (StrictMode double effects)', () => {
  const { clock, changes, flag } = setup()
  flag.set(true)
  clock.advance(300)
  flag.set(false) // hide pending until 900
  flag.cancel() // cleanup
  flag.set(false) // effect re-run
  clock.advance(700)
  assert.deepEqual(changes, [
    [300, true],
    [900, false],
  ])
  assert.equal(flag.shown, false)
})

test('custom options; delay 0 shows at once', () => {
  const { clock, changes, flag } = setup({ delay: 0, minVisible: 100 })
  flag.set(true)
  assert.deepEqual(changes, [[0, true]])
  clock.advance(20)
  flag.set(false)
  clock.advance(200)
  assert.deepEqual(changes.at(-1), [100, false])
})

test('presets: navigation-like waits 1000 ms and stays 800 ms', async () => {
  const { DELAY_PRESETS } = await import('../useDelayedFlag')
  const { clock, changes, flag } = setup(DELAY_PRESETS.NAVIGATION)
  flag.set(true)
  clock.advance(999)
  assert.deepEqual(changes, [])
  clock.advance(1)
  flag.set(false)
  clock.advance(2000)
  assert.deepEqual(changes, [
    [1000, true],
    [1800, false],
  ])
})

test('the minimum visible time counts from the first painted frame', () => {
  const clock = fakeClock()
  const paints = []
  clock.afterPaint = (fn) => paints.push(fn)
  const changes = []
  const flag = new DelayedFlag((v) => changes.push([clock.now(), v]), {}, clock)
  flag.set(true)
  clock.advance(300) // shown at 300; the frame is painted 30 ms later
  clock.advance(30)
  paints.shift()()
  clock.advance(10)
  flag.set(false)
  clock.advance(1000)
  assert.deepEqual(changes, [
    [300, true],
    [930, false],
  ])
})

test('a hide requested before the first paint keeps the whole minimum', () => {
  const clock = fakeClock()
  const paints = []
  clock.afterPaint = (fn) => paints.push(fn)
  const changes = []
  const flag = new DelayedFlag((v) => changes.push([clock.now(), v]), {}, clock)
  flag.set(true)
  clock.advance(305)
  flag.set(false) // not painted yet
  paints.shift()() // painted now: must not shorten the pending hide
  clock.advance(1000)
  assert.deepEqual(changes, [
    [300, true],
    [905, false],
  ])
})
