/*
 * One transfer per session, answered by exactly one view.
 *
 * Every view of a session (several tabs, pop-outs, other browser windows or devices) sees the same output, so every
 * view would detect the same trzsz / ZMODEM start and answer it — two answers corrupt the transfer. The arbiter picks
 * one view:
 *
 *   in this page   the registered owner with the highest priority (active tab > focused window > visible), ties by
 *                  registration order;
 *   across pages   the candidate announces a claim on a BroadcastChannel and waits a short window; the highest
 *                  priority claim wins (ties: lowest page id). The winner sends heartbeats while the transfer runs
 *                  and a release at the end, so other views can hide the protocol bytes meanwhile.
 *
 * Other devices (a second browser elsewhere) cannot be reached; views there answer on their own — documented.
 * No app imports: unit-tested under Node (BroadcastChannel exists there too).
 */

export interface LockOwner {
  readonly id: string
  readonly sessionId: string
  /** Higher wins (e.g. 4 = active tab, 2 = window focused, 1 = visible). */
  priority(): number
}

interface ClaimMsg {
  t: 'claim' | 'active' | 'release'
  sid: string
  page: string
  prio: number
  key?: string
}

export interface ArbiterOptions {
  channelName?: string
  /** How long a cross-page claim waits for competing claims. */
  claimWindowMs?: number
  heartbeatMs?: number
  /** A remote holder without heartbeat for this long is considered gone. */
  staleMs?: number
  pageId?: string
  /** Inject for tests. */
  channel?: BroadcastChannelLike | null
  now?: () => number
}

export interface BroadcastChannelLike {
  postMessage(msg: unknown): void
  addEventListener(type: 'message', cb: (e: { data: unknown }) => void): void
  removeEventListener(type: 'message', cb: (e: { data: unknown }) => void): void
  close(): void
}

function randomId(): string {
  const a = new Uint8Array(8)
  crypto.getRandomValues(a)
  return Array.from(a, (b) => b.toString(16).padStart(2, '0')).join('')
}

export class TransferArbiter {
  readonly pageId: string
  private readonly owners = new Map<string, { owner: LockOwner; seq: number }>()
  private seq = 0
  /** Session → owner id holding a transfer in this page. */
  private readonly held = new Map<string, string>()
  /** Session → remote holder (page) and when its last heartbeat arrived. */
  private readonly remote = new Map<string, { page: string; at: number }>()
  /** Claims seen from other pages during our claim windows: session → best claim. */
  private readonly competing = new Map<string, ClaimMsg[]>()
  private readonly heartbeats = new Map<string, ReturnType<typeof setInterval>>()
  private readonly listeners = new Set<() => void>()
  private readonly channel: BroadcastChannelLike | null
  private readonly claimWindowMs: number
  private readonly heartbeatMs: number
  private readonly staleMs: number
  private readonly now: () => number
  private readonly onMessage = (e: { data: unknown }) => this.receive(e.data)

  constructor(opts: ArbiterOptions = {}) {
    this.pageId = opts.pageId ?? randomId()
    this.claimWindowMs = opts.claimWindowMs ?? 90
    this.heartbeatMs = opts.heartbeatMs ?? 1000
    this.staleMs = opts.staleMs ?? 3500
    this.now = opts.now ?? Date.now
    let ch: BroadcastChannelLike | null = null
    if (opts.channel !== undefined) ch = opts.channel
    else if (typeof BroadcastChannel !== 'undefined') {
      try {
        ch = new BroadcastChannel(opts.channelName ?? 'nexterm:termtransfer') as unknown as BroadcastChannelLike
      } catch {
        ch = null
      }
    }
    this.channel = ch
    ch?.addEventListener('message', this.onMessage)
  }

  dispose(): void {
    for (const t of this.heartbeats.values()) clearInterval(t)
    this.heartbeats.clear()
    this.channel?.removeEventListener('message', this.onMessage)
    this.channel?.close()
  }

  register(owner: LockOwner): () => void {
    this.owners.set(owner.id, { owner, seq: ++this.seq })
    return () => {
      this.owners.delete(owner.id)
      if (this.held.get(owner.sessionId) === owner.id) this.release(owner)
    }
  }

  subscribe(cb: () => void): () => void {
    this.listeners.add(cb)
    return () => this.listeners.delete(cb)
  }

  private emit(): void {
    for (const l of Array.from(this.listeners)) {
      try {
        l()
      } catch {
        /* ignore */
      }
    }
  }

  /** The owner of this page that should answer a transfer on `sessionId` (null: nobody registered). */
  leaderInPage(sessionId: string): LockOwner | null {
    let best: { owner: LockOwner; seq: number; prio: number } | null = null
    for (const e of this.owners.values()) {
      if (e.owner.sessionId !== sessionId) continue
      let prio = 0
      try {
        prio = e.owner.priority()
      } catch {
        prio = 0
      }
      if (!best || prio > best.prio || (prio === best.prio && e.seq < best.seq)) best = { ...e, prio }
    }
    return best?.owner ?? null
  }

  /**
   * Try to become the view answering a transfer on the owner's session. Resolves false when another view of this page
   * is preferred, another view already holds a transfer, or another page wins the claim.
   */
  async claim(owner: LockOwner): Promise<boolean> {
    const sid = owner.sessionId
    const holder = this.held.get(sid)
    if (holder && holder !== owner.id) return false
    if (holder === owner.id) return true
    const leader = this.leaderInPage(sid)
    if (leader && leader.id !== owner.id) return false
    if (this.heldRemotely(sid)) return false
    // Hold it locally right away (a second detection in this page during the claim window must not race us).
    this.held.set(sid, owner.id)
    if (this.channel) {
      const prio = safePriority(owner)
      this.competing.set(sid, [])
      this.post({ t: 'claim', sid, page: this.pageId, prio })
      await new Promise((r) => setTimeout(r, this.claimWindowMs))
      const others = this.competing.get(sid) ?? []
      this.competing.delete(sid)
      const beaten = others.some((c) => c.prio > prio || (c.prio === prio && c.page < this.pageId))
      if (beaten || this.heldRemotely(sid) || this.held.get(sid) !== owner.id) {
        if (this.held.get(sid) === owner.id) this.held.delete(sid)
        this.emit()
        return false
      }
      this.post({ t: 'active', sid, page: this.pageId, prio })
      this.heartbeats.set(
        sid,
        setInterval(() => this.post({ t: 'active', sid, page: this.pageId, prio }), this.heartbeatMs),
      )
    }
    this.emit()
    return true
  }

  release(owner: LockOwner): void {
    const sid = owner.sessionId
    if (this.held.get(sid) !== owner.id) return
    this.held.delete(sid)
    const hb = this.heartbeats.get(sid)
    if (hb) clearInterval(hb)
    this.heartbeats.delete(sid)
    this.post({ t: 'release', sid, page: this.pageId, prio: 0 })
    this.emit()
  }

  /** True when a transfer on the session is answered by someone else (another view here or another page). */
  heldElsewhere(owner: LockOwner): boolean {
    const h = this.held.get(owner.sessionId)
    if (h && h !== owner.id) return true
    return this.heldRemotely(owner.sessionId)
  }

  isHolder(owner: LockOwner): boolean {
    return this.held.get(owner.sessionId) === owner.id
  }

  private heldRemotely(sid: string): boolean {
    const r = this.remote.get(sid)
    if (!r) return false
    if (this.now() - r.at > this.staleMs) {
      this.remote.delete(sid)
      return false
    }
    return true
  }

  private post(msg: ClaimMsg): void {
    try {
      this.channel?.postMessage(msg)
    } catch {
      /* closed */
    }
  }

  private receive(data: unknown): void {
    const m = data as Partial<ClaimMsg> | null
    if (!m || typeof m !== 'object' || typeof m.sid !== 'string' || typeof m.page !== 'string' || m.page === this.pageId) return
    switch (m.t) {
      case 'claim': {
        const list = this.competing.get(m.sid)
        if (list) list.push(m as ClaimMsg)
        // Someone else claims a session we are actively transferring on: remind them.
        const holder = this.held.get(m.sid)
        if (holder && !list && this.heartbeats.has(m.sid)) this.post({ t: 'active', sid: m.sid, page: this.pageId, prio: 99 })
        break
      }
      case 'active':
        this.remote.set(m.sid, { page: m.page, at: this.now() })
        this.emit()
        break
      case 'release': {
        const r = this.remote.get(m.sid)
        if (r && r.page === m.page) {
          this.remote.delete(m.sid)
          this.emit()
        }
        break
      }
    }
  }
}

function safePriority(o: LockOwner): number {
  try {
    return o.priority()
  } catch {
    return 0
  }
}
