/*
 * Trigger sounds, synthesized with WebAudio (no audio assets, works offline).
 */

export const SOUNDS = [
  { id: 'beep', label: 'Beep' },
  { id: 'chime', label: 'Chime' },
  { id: 'alert', label: 'Alert' },
  { id: 'ping', label: 'Ping' },
] as const

type Note = { freq: number; start: number; dur: number; type?: OscillatorType; gain?: number }

const PATTERNS: Record<string, Note[]> = {
  beep: [{ freq: 880, start: 0, dur: 0.15, type: 'square', gain: 0.06 }],
  chime: [
    { freq: 1046.5, start: 0, dur: 0.35, type: 'sine', gain: 0.12 },
    { freq: 1318.5, start: 0.12, dur: 0.45, type: 'sine', gain: 0.1 },
  ],
  alert: [
    { freq: 740, start: 0, dur: 0.12, type: 'sawtooth', gain: 0.05 },
    { freq: 740, start: 0.18, dur: 0.12, type: 'sawtooth', gain: 0.05 },
    { freq: 988, start: 0.36, dur: 0.2, type: 'sawtooth', gain: 0.05 },
  ],
  ping: [{ freq: 1567.98, start: 0, dur: 0.25, type: 'triangle', gain: 0.12 }],
}

let ctx: AudioContext | null = null
let lastPlay = 0

export function playSound(id: string): void {
  const now = Date.now()
  if (now - lastPlay < 150) return // several triggers at once: one sound
  lastPlay = now
  try {
    const AC = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
    if (!AC) return
    ctx ??= new AC()
    if (ctx.state === 'suspended') void ctx.resume()
    const t0 = ctx.currentTime + 0.01
    for (const n of PATTERNS[id] ?? PATTERNS.beep) {
      const osc = ctx.createOscillator()
      const gain = ctx.createGain()
      osc.type = n.type ?? 'sine'
      osc.frequency.value = n.freq
      const g = n.gain ?? 0.08
      gain.gain.setValueAtTime(0, t0 + n.start)
      gain.gain.linearRampToValueAtTime(g, t0 + n.start + 0.01)
      gain.gain.exponentialRampToValueAtTime(0.0001, t0 + n.start + n.dur)
      osc.connect(gain).connect(ctx.destination)
      osc.start(t0 + n.start)
      osc.stop(t0 + n.start + n.dur + 0.02)
    }
  } catch {
    /* audio unavailable */
  }
}
