/*
 * Plain-language names for the common cron specs ("0 3 * * *" → "Every day at 03:00"). Anything unusual returns null
 * and the UI shows the spec itself. Pure (unit-tested in __tests__/cron.test.mjs).
 */

const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
const DAY_NAMES: Record<string, number> = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 }
const MACROS: Record<string, string> = {
  '@yearly': 'Every year on 1 January at 00:00',
  '@annually': 'Every year on 1 January at 00:00',
  '@monthly': 'Every month on the 1st at 00:00',
  '@weekly': 'Every Sunday at 00:00',
  '@daily': 'Every day at 00:00',
  '@midnight': 'Every day at 00:00',
  '@hourly': 'Every hour',
}

const num = (s: string, max: number): number | null => (/^\d+$/.test(s) && Number(s) <= max ? Number(s) : null)
const two = (n: number) => String(n).padStart(2, '0')
const ordinal = (n: number) => `${n}${n % 10 === 1 && n !== 11 ? 'st' : n % 10 === 2 && n !== 12 ? 'nd' : n % 10 === 3 && n !== 13 ? 'rd' : 'th'}`

/** Day-of-week field → "Mondays to Fridays" / "Mondays and Thursdays"; null when not a plain list or range. */
function days(field: string): string | null {
  const day = (s: string) => DAY_NAMES[s.toLowerCase()] ?? (num(s, 7) === null ? null : Number(s) % 7)
  const range = /^(\w+)-(\w+)$/.exec(field)
  if (range) {
    const [a, b] = [day(range[1]), day(range[2])]
    if (a == null || b == null) return null
    return a === 1 && b === 5 ? 'weekdays' : `${DAYS[a]}s to ${DAYS[b]}s`
  }
  const list = field.split(',').map(day)
  if (list.some((d) => d == null)) return null
  const names = (list as number[]).map((d) => `${DAYS[d]}s`)
  return names.length === 1 ? names[0] : `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`
}

export function describeCron(spec: string): string | null {
  const s = spec.trim().replace(/\s+/g, ' ')
  if (MACROS[s.toLowerCase()]) return MACROS[s.toLowerCase()]
  const every = /^@every (\d+)([smh])$/i.exec(s)
  if (every) {
    const n = Number(every[1])
    const unit = { s: 'second', m: 'minute', h: 'hour' }[every[2].toLowerCase() as 's' | 'm' | 'h']
    return n === 1 ? `Every ${unit}` : `Every ${n} ${unit}s`
  }
  const f = s.split(' ')
  if (f.length !== 5) return null
  const [min, hour, dom, mon, dow] = f
  if (mon !== '*') return null
  const step = (field: string) => (/^\*\/(\d+)$/.exec(field) ? Number(field.slice(2)) : null)
  if (hour === '*' && dom === '*' && dow === '*') {
    if (min === '*') return 'Every minute'
    const n = step(min)
    if (n) return n === 1 ? 'Every minute' : `Every ${n} minutes`
    const m = num(min, 59)
    return m == null ? null : m === 0 ? 'Every hour' : `Every hour at :${two(m)}`
  }
  const m = num(min, 59)
  if (m == null) return null
  const hs = step(hour)
  if (hs && dom === '*' && dow === '*') return m === 0 ? `Every ${hs} hours` : `Every ${hs} hours at :${two(m)}`
  const h = num(hour, 23)
  if (h == null) return null
  const at = `at ${two(h)}:${two(m)}`
  if (dom === '*' && dow === '*') return `Every day ${at}`
  if (dom === '*') {
    const d = days(dow)
    return d ? `${d[0].toUpperCase()}${d.slice(1)} ${at}` : null
  }
  const day = num(dom, 31)
  if (day && dow === '*') return `Monthly on the ${ordinal(day)} ${at}`
  return null
}

/** A coming run time as people say it: "Today 15:30", "Tomorrow 03:00", "Fri 03:00", else the short date and time. */
export function formatNextRun(iso: string, now = new Date()): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const time = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const days = Math.round((day(d) - day(now)) / 86_400_000)
  if (days === 0) return `Today ${time}`
  if (days === 1) return `Tomorrow ${time}`
  if (days > 1 && days < 7) return `${d.toLocaleDateString(undefined, { weekday: 'short' })} ${time}`
  return `${d.toLocaleDateString(undefined, { day: 'numeric', month: 'short' })} ${time}`
}
