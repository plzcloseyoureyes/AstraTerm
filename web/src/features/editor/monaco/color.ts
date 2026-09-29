/*
 * CSS colour values → "#rrggbb[aa]" (Monaco themes accept hex only). The design tokens are written as oklch() (the
 * production CSS build may turn them into lab()); the accent is set at runtime as oklch() or hex. Pure (no DOM): the theme reads the values, this converts them.
 */

export interface Rgba {
  r: number
  g: number
  b: number
  /** 0..1 */
  a: number
}

const clamp01 = (v: number) => Math.min(1, Math.max(0, v))

function num(s: string, percentScale = 1): number {
  const t = s.trim()
  if (t === 'none') return 0
  if (t.endsWith('%')) return (parseFloat(t) / 100) * percentScale
  return parseFloat(t)
}

function hue(s: string): number {
  const t = s.trim()
  if (t === 'none') return 0
  const v = parseFloat(t)
  if (t.endsWith('turn')) return v * 360
  if (t.endsWith('grad')) return v * 0.9
  if (t.endsWith('rad') && !t.endsWith('grad')) return (v * 180) / Math.PI
  return v
}

/** Split "a b c / d" or "a, b, c, d" into channel strings and alpha. */
function channels(body: string): { parts: string[]; alpha: number } {
  let alpha = 1
  let main = body
  const slash = body.indexOf('/')
  if (slash >= 0) {
    alpha = clamp01(num(body.slice(slash + 1), 1))
    main = body.slice(0, slash)
  }
  const parts = main.includes(',') ? main.split(',') : main.trim().split(/\s+/)
  if (slash < 0 && parts.length === 4) alpha = clamp01(num(parts.pop() ?? '1', 1))
  return { parts: parts.map((p) => p.trim()).filter(Boolean), alpha }
}

/** Linear sRGB → gamma-encoded 0..1. */
function encode(c: number): number {
  const v = Math.abs(c) <= 0.0031308 ? 12.92 * c : Math.sign(c) * (1.055 * Math.abs(c) ** (1 / 2.4) - 0.055)
  return clamp01(v)
}

function oklabToRgb(L: number, a: number, b: number): [number, number, number] {
  const l_ = L + 0.3963377774 * a + 0.2158037573 * b
  const m_ = L - 0.1055613458 * a - 0.0638541728 * b
  const s_ = L - 0.0894841775 * a - 1.291485548 * b
  const l = l_ ** 3
  const m = m_ ** 3
  const s = s_ ** 3
  return [
    encode(4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s),
    encode(-1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s),
    encode(-0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s),
  ]
}

/** CIE Lab (D50, as CSS lab()) → gamma-encoded sRGB 0..1. */
function labToRgb(L: number, a: number, b: number): [number, number, number] {
  const e = 216 / 24389
  const k = 24389 / 27
  const fy = (L + 16) / 116
  const fx = fy + a / 500
  const fz = fy - b / 200
  const xr = fx ** 3 > e ? fx ** 3 : (116 * fx - 16) / k
  const yr = L > k * e ? fy ** 3 : L / k
  const zr = fz ** 3 > e ? fz ** 3 : (116 * fz - 16) / k
  // D50 white point, then Bradford D50 → D65
  const X50 = xr * 0.96422
  const Y50 = yr
  const Z50 = zr * 0.82521
  const X = 0.9554734527042182 * X50 - 0.023098536874261423 * Y50 + 0.0632593086610217 * Z50
  const Y = -0.028369706963208136 * X50 + 1.0099954580058226 * Y50 + 0.021041398966943008 * Z50
  const Z = 0.012314001688319899 * X50 - 0.020507696433477912 * Y50 + 1.3303659366080753 * Z50
  return [
    encode(3.2409699419045226 * X - 1.537383177570094 * Y - 0.4986107602930034 * Z),
    encode(-0.9692436362808796 * X + 1.8759675015077202 * Y + 0.04155505740717559 * Z),
    encode(0.05563007969699366 * X - 0.20397695888897652 * Y + 1.0569715142428786 * Z),
  ]
}

function hslToRgb(h: number, s: number, l: number): [number, number, number] {
  const k = (n: number) => (n + h / 30) % 12
  const a = s * Math.min(l, 1 - l)
  const f = (n: number) => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)))
  return [f(0), f(8), f(4)]
}

/** Parse a CSS colour (hex, rgb[a], hsl[a], lab, lch, oklab, oklch, color(srgb …), transparent). Null when unsupported. */
export function parseColor(input: string): Rgba | null {
  const s = input.trim().toLowerCase()
  if (!s) return null
  if (s === 'transparent') return { r: 0, g: 0, b: 0, a: 0 }
  if (s.startsWith('#')) {
    let h = s.slice(1)
    if (!/^[0-9a-f]+$/.test(h) || ![3, 4, 6, 8].includes(h.length)) return null
    if (h.length <= 4) h = h.replace(/./g, (c) => c + c)
    const n = (i: number) => parseInt(h.slice(i, i + 2), 16)
    return { r: n(0), g: n(2), b: n(4), a: h.length === 8 ? n(6) / 255 : 1 }
  }
  const m = /^([a-z-]+)\((.*)\)$/.exec(s)
  if (!m) return null
  const fn = m[1]
  let body = m[2]
  const to255 = ([r, g, b]: [number, number, number], a: number): Rgba => ({ r: Math.round(clamp01(r) * 255), g: Math.round(clamp01(g) * 255), b: Math.round(clamp01(b) * 255), a })
  if (fn === 'color') {
    const space = body.trim().split(/\s+/)[0]
    if (space !== 'srgb' && space !== 'srgb-linear') return null
    body = body.trim().slice(space.length)
    const { parts, alpha } = channels(body)
    if (parts.length < 3) return null
    const [r, g, b] = parts.map((p) => num(p, 1))
    return space === 'srgb' ? to255([r, g, b], alpha) : to255([encode(r), encode(g), encode(b)], alpha)
  }
  const { parts, alpha } = channels(body)
  if (parts.length < 3) return null
  switch (fn) {
    case 'rgb':
    case 'rgba': {
      const [r, g, b] = parts.map((p) => num(p, 255) / 255)
      return to255([r, g, b], alpha)
    }
    case 'hsl':
    case 'hsla':
      return to255(hslToRgb(hue(parts[0]), num(parts[1], 1) / (parts[1].endsWith('%') ? 1 : 100), num(parts[2], 1) / (parts[2].endsWith('%') ? 1 : 100)), alpha)
    case 'lab':
      return to255(labToRgb(num(parts[0], 100), num(parts[1], 125), num(parts[2], 125)), alpha)
    case 'lch': {
      const C = num(parts[1], 150)
      const H = (hue(parts[2]) * Math.PI) / 180
      return to255(labToRgb(num(parts[0], 100), C * Math.cos(H), C * Math.sin(H)), alpha)
    }
    case 'oklab':
      return to255(oklabToRgb(num(parts[0], 1), num(parts[1], 0.4), num(parts[2], 0.4)), alpha)
    case 'oklch': {
      const L = num(parts[0], 1)
      const C = num(parts[1], 0.4)
      const H = (hue(parts[2]) * Math.PI) / 180
      return to255(oklabToRgb(L, C * Math.cos(H), C * Math.sin(H)), alpha)
    }
    default:
      return null
  }
}

const hex2 = (n: number) => Math.round(Math.min(255, Math.max(0, n))).toString(16).padStart(2, '0')

/** "#rrggbb", or "#rrggbbaa" when not opaque. */
export function toHex(c: Rgba, alpha = 1): string {
  const a = clamp01(c.a * alpha)
  const base = `#${hex2(c.r)}${hex2(c.g)}${hex2(c.b)}`
  return a >= 0.999 ? base : base + hex2(a * 255)
}

/** Mix `c` over `bg` (both opaque results), `amount` of `c` (0..1). */
export function mix(c: Rgba, bg: Rgba, amount: number): Rgba {
  const t = clamp01(amount)
  return { r: c.r * t + bg.r * (1 - t), g: c.g * t + bg.g * (1 - t), b: c.b * t + bg.b * (1 - t), a: 1 }
}

/** Relative luminance (WCAG). */
export function luminance(c: Rgba): number {
  const ch = (v: number) => {
    const x = v / 255
    return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * ch(c.r) + 0.7152 * ch(c.g) + 0.0722 * ch(c.b)
}
