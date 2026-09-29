// formatCalendarTime: "Today 14:10", "Tomorrow 03:00", "Yesterday …", weekday within a week, else the date.
import assert from 'node:assert/strict'
import test from 'node:test'
import { formatCalendarTime } from '../utils'

const now = new Date(2026, 8, 28, 12, 0) // Mon 28 Sep 2026, local time
const at = (y, m, d, h, min) => new Date(y, m, d, h, min)
const time = (d) => new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' }).format(d)

test('today, tomorrow, yesterday', () => {
  const t = at(2026, 8, 28, 14, 10)
  assert.equal(formatCalendarTime(t, now), `Today ${time(t)}`)
  const tm = at(2026, 8, 29, 3, 0)
  assert.equal(formatCalendarTime(tm, now), `Tomorrow ${time(tm)}`)
  const y = at(2026, 8, 27, 23, 59)
  assert.equal(formatCalendarTime(y.toISOString(), now), `Yesterday ${time(y)}`)
})

test('a weekday within a week, either way', () => {
  const past = at(2026, 8, 23, 9, 30)
  const wd = new Intl.DateTimeFormat(undefined, { weekday: 'short' }).format(past)
  assert.equal(formatCalendarTime(past, now), `${wd} ${time(past)}`)
  const next = at(2026, 9, 3, 8, 0)
  assert.ok(formatCalendarTime(next, now).startsWith(new Intl.DateTimeFormat(undefined, { weekday: 'short' }).format(next) + ' '))
})

test('further away: the date, with the year only when it differs', () => {
  const d = at(2026, 7, 1, 10, 0)
  assert.ok(!formatCalendarTime(d, now).includes('2026'))
  assert.ok(formatCalendarTime(at(2025, 7, 1, 10, 0), now).includes('2025'))
})

test('missing or invalid', () => {
  assert.equal(formatCalendarTime(null, now), '—')
  assert.equal(formatCalendarTime('', now), '—')
  assert.equal(formatCalendarTime('not a date', now), '—')
})
