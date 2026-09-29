import { test } from 'node:test'
import assert from 'node:assert/strict'
import { castMarkers, idleTimeline, parseCast } from '../cast.ts'

const ev = (times) => ({ events: times.map((t) => [t, 'o', 'x']), duration: times.at(-1) })

test('idleTimeline: no limit is the identity', () => {
  const tl = idleTimeline(ev([1, 10, 30]), 0)
  assert.equal(tl.duration, 30)
  assert.equal(tl.toPlayer(12.5), 12.5)
  assert.equal(tl.toRaw(12.5), 12.5)
})

test('idleTimeline: caps every long gap, the first one included', () => {
  // gaps: 0→5 (5 s, capped to 2), 5→6 (1 s), 6→16 (10 s, capped to 2)
  const tl = idleTimeline(ev([5, 6, 16]), 2)
  assert.equal(tl.duration, 16 - 3 - 8)
  assert.equal(tl.toPlayer(5), 2)
  assert.equal(tl.toPlayer(6), 3)
  assert.equal(tl.toPlayer(16), 5)
  assert.equal(tl.toPlayer(7), 4) // 1 s into a capped gap
  assert.equal(tl.toPlayer(12), 5) // past the cap: waits at the gap's end
  assert.equal(tl.toPlayer(20), 9) // after the last event time runs 1:1
})

test('idleTimeline: toRaw inverts toPlayer on event times', () => {
  const times = [0.2, 0.5, 4, 4.1, 30, 31, 90]
  const tl = idleTimeline(ev(times), 1.5)
  for (const t of times) assert.ok(Math.abs(tl.toRaw(tl.toPlayer(t)) - t) < 1e-9, `t=${t}`)
  let last = -1
  for (let p = 0; p <= tl.duration; p += 0.25) {
    const r = tl.toRaw(p)
    assert.ok(r >= last, 'monotonic')
    last = r
  }
})

test('parseCast: v3 relative times become absolute', () => {
  const c = parseCast('{"version":3,"term":{"cols":80,"rows":24}}\n[0.5,"o","a"]\n[1.5,"m","cmd"]\n[2,"o","b"]\n')
  assert.deepEqual(c.events.map((e) => e[0]), [0.5, 2, 4])
  assert.equal(c.duration, 4)
})

test('castMarkers: prompt marks are named after their prompt line (the recorder writes the mark after the chunk)', () => {
  const A = '\\u001b]133;A\\u0007'
  const lines = [
    '{"version":3,"term":{"cols":80,"rows":24}}',
    `[0.1,"o","${A}\\u001b[1;32m~/app\\u001b[0m$ "]`,
    '[0,"m",""]',
    '[0.5,"o","maek"]',
    '[0.2,"o","\\b \\b\\b\\u001b[Kke build"]',
    `[0.3,"o","\\r\\ndone\\r\\n${A}$ ls\\r\\n"]`,
    '[0,"m",""]',
    '[0.1,"m","deploy"]',
    `[1,"o","a b\\r\\n${A}$ \\u001b]133;B\\u0007"]`,
    '[0,"m",""]',
    '[0.4,"o","git status"]',
    '[0.1,"o","\\r\\n"]',
    `[1,"o","${A}$ "]`,
    '[0,"m",""]',
  ]
  const c = parseCast(lines.join('\n') + '\n')
  assert.deepEqual(castMarkers(c).map((m) => m.label), ['~/app$ make build', '$ ls', 'deploy', 'git status', '$'])
})
