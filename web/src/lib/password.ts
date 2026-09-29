/*
 * Password generation (CSPRNG, unbiased) and a lightweight strength estimate for UI meters.
 */

const LOWER = 'abcdefghijkmnopqrstuvwxyz' // no l (ambiguous)
const UPPER = 'ABCDEFGHJKLMNPQRSTUVWXYZ' // no I, O
const DIGITS = '23456789' // no 0, 1
const SYMBOLS = '!#$%&*+-=?@^_~'

export interface GenerateOptions {
  length?: number
  lower?: boolean
  upper?: boolean
  digits?: boolean
  symbols?: boolean
}

/** Uniform random integer in [0, max) using rejection sampling over crypto.getRandomValues. */
export function randomInt(max: number): number {
  if (!Number.isInteger(max) || max <= 0 || max > 0x100000000) throw new RangeError('randomInt: bad range')
  const limit = Math.floor(0x100000000 / max) * max
  const buf = new Uint32Array(1)
  for (;;) {
    crypto.getRandomValues(buf)
    if (buf[0] < limit) return buf[0] % max
  }
}

/** Generate a random password containing at least one character of every enabled class. */
export function generatePassword(opts: GenerateOptions = {}): string {
  const length = Math.max(8, Math.min(256, opts.length ?? 20))
  const classes = [
    opts.lower !== false ? LOWER : '',
    opts.upper !== false ? UPPER : '',
    opts.digits !== false ? DIGITS : '',
    opts.symbols !== false ? SYMBOLS : '',
  ].filter(Boolean)
  if (!classes.length) classes.push(LOWER)
  const all = classes.join('')
  const chars: string[] = classes.map((c) => c[randomInt(c.length)])
  while (chars.length < length) chars.push(all[randomInt(all.length)])
  // Fisher–Yates shuffle so the guaranteed characters are not at fixed positions.
  for (let i = chars.length - 1; i > 0; i--) {
    const j = randomInt(i + 1)
    ;[chars[i], chars[j]] = [chars[j], chars[i]]
  }
  return chars.join('')
}

const COMMON = new Set([
  'password',
  'passw0rd',
  '123456',
  '12345678',
  '123456789',
  'qwerty',
  'qwertyuiop',
  'letmein',
  'admin',
  'administrator',
  'welcome',
  'iloveyou',
  'monkey',
  'dragon',
  'changeme',
  'root',
  'toor',
  'termstead',
  'secret',
  'abc123',
  '111111',
  '000000',
])

export interface Strength {
  /** 0 (very weak) … 4 (very strong) */
  score: 0 | 1 | 2 | 3 | 4
  label: string
  /** Estimated entropy bits (rough). */
  bits: number
  hints: string[]
}

const LABELS = ['Very weak', 'Weak', 'Fair', 'Strong', 'Very strong'] as const

/** Heuristic strength estimate (charset × length, minus repetition / sequences / common words). */
export function estimateStrength(pw: string, context: string[] = []): Strength {
  const hints: string[] = []
  if (!pw) return { score: 0, label: LABELS[0], bits: 0, hints: ['Enter a password'] }
  let pool = 0
  if (/[a-z]/.test(pw)) pool += 26
  if (/[A-Z]/.test(pw)) pool += 26
  if (/\d/.test(pw)) pool += 10
  if (/[^a-zA-Z0-9]/.test(pw)) pool += 33
  // Effective length: collapse runs and simple sequences.
  let effective = 0
  for (let i = 0; i < pw.length; i++) {
    const c = pw.charCodeAt(i)
    const prev = i > 0 ? pw.charCodeAt(i - 1) : -99
    const prev2 = i > 1 ? pw.charCodeAt(i - 2) : -99
    if (c === prev) effective += 0.25
    else if (c - prev === prev - prev2 && Math.abs(c - prev) === 1) effective += 0.35
    else effective += 1
  }
  let bits = effective * Math.log2(Math.max(pool, 1))
  const lower = pw.toLowerCase()
  const leet = lower.replace(/0/g, 'o').replace(/[1!]/g, 'i').replace(/3/g, 'e').replace(/[4@]/g, 'a').replace(/[5$]/g, 's')
  if (COMMON.has(lower) || COMMON.has(leet)) {
    bits = Math.min(bits, 10)
    hints.push('This is a very common password')
  }
  for (const word of context) {
    const w = word.trim().toLowerCase()
    if (w.length >= 3 && lower.includes(w)) {
      bits -= 12
      hints.push('Avoid using your name or username')
      break
    }
  }
  if (pw.length < 12) hints.push('Use at least 12 characters')
  if (pool <= 36) hints.push('Mix upper/lower case, digits and symbols')
  bits = Math.max(0, bits)
  const score = (bits < 28 ? 0 : bits < 40 ? 1 : bits < 60 ? 2 : bits < 80 ? 3 : 4) as Strength['score']
  return { score, label: LABELS[score], bits: Math.round(bits), hints }
}
