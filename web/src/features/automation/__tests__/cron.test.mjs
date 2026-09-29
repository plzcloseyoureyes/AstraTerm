import { test } from 'node:test'
import assert from 'node:assert/strict'
import { describeCron, formatNextRun } from '../cron.ts'

test('describeCron: common specs', () => {
  const cases = {
    '0 3 * * *': 'Every day at 03:00',
    '*/15 * * * *': 'Every 15 minutes',
    '* * * * *': 'Every minute',
    '@hourly': 'Every hour',
    '30 * * * *': 'Every hour at :30',
    '0 */6 * * *': 'Every 6 hours',
    '0 8 * * mon-fri': 'Weekdays at 08:00',
    '0 8 * * 1-5': 'Weekdays at 08:00',
    '0 2 * * sun': 'Sundays at 02:00',
    '0 2 * * 1,4': 'Mondays and Thursdays at 02:00',
    '0 4 1 * *': 'Monthly on the 1st at 04:00',
    '15 23 22 * *': 'Monthly on the 22nd at 23:15',
    '@every 5m': 'Every 5 minutes',
    '@daily': 'Every day at 00:00',
  }
  for (const [spec, want] of Object.entries(cases)) assert.equal(describeCron(spec), want, spec)
})

test('describeCron: unusual specs are left alone', () => {
  for (const spec of ['0 3 * 6 *', '5,10 3 * * *', '0 3 1 * mon', 'nonsense', '0 25 * * *']) assert.equal(describeCron(spec), null, spec)
})

test('formatNextRun: today, tomorrow, this week, later', () => {
  const now = new Date(2026, 8, 28, 10, 0) // Monday 28 Sep 2026
  const at = (d, h, m) => new Date(2026, 8, d, h, m).toISOString()
  assert.match(formatNextRun(at(28, 15, 30), now), /^Today /)
  assert.match(formatNextRun(at(29, 3, 0), now), /^Tomorrow /)
  assert.doesNotMatch(formatNextRun(at(30, 3, 0), now), /^(Today|Tomorrow)/) // later this week: the weekday
  assert.doesNotMatch(formatNextRun(new Date(2026, 9, 20, 3, 0).toISOString(), now), /^(Today|Tomorrow)/)
})
