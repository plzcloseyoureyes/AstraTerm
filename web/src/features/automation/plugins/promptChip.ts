/*
 * Timing of the password-prompt chip (see passwordChip.ts), kept free of DOM / xterm so it can be unit-tested with a
 * fake clock (__tests__/promptChip.test.mjs).
 *
 * The chip must be calm: it appears once output has been quiet at a password prompt for SHOW_DELAY ms, and while it is
 * shown new output (a redraw, a title update, a bell) only re-checks the prompt after the output has been quiet for
 * SETTLE ms — the chip stays put when the prompt is still there and goes only when it is really gone. Typing hides it
 * at once (the prompt is being answered).
 */

export const CHIP_SHOW_DELAY = 150
export const CHIP_SETTLE = 250

export interface ChipClock {
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(handle: unknown): void
}

const realClock: ChipClock = {
  setTimeout: (fn, ms) => setTimeout(fn, ms),
  clearTimeout: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
}

export interface PromptChipOptions {
  /** The secret to offer right now (the cursor sits at a matching password prompt), or null. */
  resolve: () => Promise<string | null> | string | null
  /** The offered secret changed: show the chip for `key`, or hide it (null). */
  onChange: (key: string | null) => void
  /** The chip stays for the same secret after a re-check (e.g. re-place it after a redraw). */
  onKeep?: () => void
  clock?: ChipClock
}

export class PromptChipController {
  /** The secret the chip currently offers (null = hidden). */
  key: string | null = null
  private timer: unknown = null
  private seq = 0
  private readonly opts: PromptChipOptions
  private readonly clock: ChipClock

  constructor(opts: PromptChipOptions) {
    this.opts = opts
    this.clock = opts.clock ?? realClock
  }

  /** New output arrived: (re-)check once it has been quiet for a moment. */
  output(): void {
    this.cancel()
    this.timer = this.clock.setTimeout(() => {
      this.timer = null
      void this.check()
    }, this.key ? CHIP_SETTLE : CHIP_SHOW_DELAY)
  }

  /** The user typed: the prompt is being answered. */
  input(): void {
    this.cancel()
    this.seq++
    this.set(null)
  }

  /** Hide now (viewport scrolled away, chip turned off, secret sent). */
  hide(): void {
    this.cancel()
    this.seq++
    this.set(null)
  }

  dispose(): void {
    this.cancel()
    this.seq++
  }

  /** Re-evaluate the prompt now (exposed for tests; normally driven by output()). */
  async check(): Promise<void> {
    const seq = ++this.seq
    const key = await this.opts.resolve()
    if (seq !== this.seq) return // typed, hidden or re-checked meanwhile
    if (key === this.key) {
      if (key) this.opts.onKeep?.()
      return
    }
    this.set(key)
  }

  private set(key: string | null): void {
    if (key === this.key) return
    this.key = key
    this.opts.onChange(key)
  }

  private cancel(): void {
    if (this.timer !== null) {
      this.clock.clearTimeout(this.timer)
      this.timer = null
    }
  }
}
