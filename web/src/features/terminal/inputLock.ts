/*
 * Input gate of one terminal view (TerminalPluginContextEx.acquireInputLock): while an in-band transfer (ZMODEM, trzsz)
 * or another plugin owns the session's input stream, input from *other* sources — MultiExec fan-out from other tabs,
 * bus sendToSession / broadcast (snippets, automation, AI insert), paced pastes, TerminalHandle.input / send — is
 * refused instead of being mixed into the protocol bytes. The lock holder keeps sending through `ctx.send`.
 * Locks nest (several holders); the most recent reason is shown.
 */
export class InputLocks {
  private seq = 0
  private readonly held = new Map<number, string>()
  private readonly onChange: (reason: string | null) => void

  constructor(onChange: (reason: string | null) => void = () => {}) {
    this.onChange = onChange
  }

  /** Take a lock; the returned function releases it (idempotent). */
  acquire(reason: string): () => void {
    const id = ++this.seq
    this.held.set(id, reason || 'Input is locked')
    this.onChange(this.reason)
    return () => {
      if (this.held.delete(id)) this.onChange(this.reason)
    }
  }

  /** The reason of the most recent lock still held, or null when unlocked. */
  get reason(): string | null {
    let last: string | null = null
    for (const r of this.held.values()) last = r
    return last
  }

  get locked(): boolean {
    return this.held.size > 0
  }

  clear(): void {
    if (!this.held.size) return
    this.held.clear()
    this.onChange(null)
  }
}
