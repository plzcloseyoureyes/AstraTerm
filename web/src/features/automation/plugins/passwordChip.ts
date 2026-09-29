/*
 * Password-prompt detection (TERM-33): when a terminal's output stops at a password prompt ("Password:",
 * "[sudo] password for x:", "Enter passphrase for key …:") and the session's connection has a matching stored secret,
 * an unobtrusive chip offers to type it. The secret is injected by the backend (POST /api/sessions/{id}/inject-secret)
 * and never reaches the browser. The command automation.sendPassword does the same from the keyboard. The chip's
 * timing (no hide / show cycles while output redraws the prompt) lives in promptChip.ts.
 */
import type { Terminal } from '@xterm/xterm'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import type { TerminalPluginContext } from '@/app/registry'
import type { TerminalPluginContextEx } from '@/features/terminal/types'
import { errorMessage } from '@/lib/utils'
import { autoKeys, getSecretKeys, injectSecret } from '../api'
import { automationSettings } from '../settings'
import type { SecretKeys } from '../types'
import { PromptChipController } from './promptChip'

const PROMPT_RE = /(?:password|passphrase|passcode|passwort|mot de passe|contraseña|senha|пароль|pin)\b[^\n]{0,120}?[:：]\s*$/i

/** Whether text (the cursor row left of the cursor) is a password / passphrase prompt. */
export function isPasswordPrompt(text: string): boolean {
  return PROMPT_RE.test(text)
}

/** The secret to offer for a prompt text: sudo prompts prefer sudoPassword, key prompts the passphrase. */
export function secretForPrompt(prompt: string, keys: string[]): string | undefined {
  const has = (k: string) => keys.includes(k)
  if (/\[sudo\]|\bsudo\b/i.test(prompt)) return has('sudoPassword') ? 'sudoPassword' : has('password') ? 'password' : undefined
  if (/passphrase/i.test(prompt)) return has('passphrase') ? 'passphrase' : has('password') ? 'password' : undefined
  if (/enable/i.test(prompt) && has('enablePassword')) return 'enablePassword'
  return has('password') ? 'password' : has('sudoPassword') ? 'sudoPassword' : undefined
}

/** Text left of the cursor on the cursor row (the prompt the shell is waiting on). */
export function promptBeforeCursor(term: Terminal): string {
  const buf = term.buffer.active
  const line = buf.getLine(buf.baseY + buf.cursorY)
  if (!line) return ''
  return line.translateToString(true, 0, buf.cursorX).slice(-200)
}

/** Secret keys of a session (cached for a minute; reconnects refresh them). */
export async function sessionSecretKeys(sessionId: string): Promise<SecretKeys | undefined> {
  try {
    return await queryClient.fetchQuery({ queryKey: autoKeys.secretKeys(sessionId), queryFn: () => getSecretKeys(sessionId), staleTime: 60_000 })
  } catch {
    return undefined
  }
}

const KEY_LABEL: Record<string, string> = { password: 'password', sudoPassword: 'sudo password', passphrase: 'passphrase', enablePassword: 'enable password' }

export function secretLabel(key: string): string {
  return KEY_LABEL[key] ?? key
}

// Tabs whose chip is visible → the secret it offers (for the keyboard command).
const visibleChips = new Map<string, { sessionId: string; key: string; send: () => void }>()

/** Send the secret offered by the chip of a tab. Returns false when no chip is shown. */
export function sendChipSecret(tabId: string): boolean {
  const c = visibleChips.get(tabId)
  if (!c) return false
  c.send()
  return true
}

export function setupPasswordChip(term: Terminal, ctx: TerminalPluginContext): () => void {
  const ex = ctx as TerminalPluginContext & Partial<TerminalPluginContextEx>
  let chip: HTMLButtonElement | null = null

  const remove = () => {
    visibleChips.delete(ctx.tabId)
    chip?.remove()
    chip = null
  }

  const send = async (key: string) => {
    controller.hide()
    try {
      await injectSecret(ctx.sessionId, key, true)
      ex.handle?.focus()
    } catch (err) {
      toast.error('Could not send the stored secret', { description: errorMessage(err) })
    }
  }

  const place = (el: HTMLElement) => {
    const screen = term.element?.querySelector<HTMLElement>('.xterm-screen')
    if (!screen || !term.cols || !term.rows) return
    const cw = screen.clientWidth / term.cols
    const ch = screen.clientHeight / term.rows
    const buf = term.buffer.active
    const y = buf.cursorY + (buf.baseY - buf.viewportY)
    const left = Math.min(Math.max(0, (buf.cursorX + 1) * cw), Math.max(0, screen.clientWidth - el.offsetWidth - 4))
    el.style.left = `${left}px`
    el.style.top = `${Math.max(0, y * ch + (ch - el.offsetHeight) / 2)}px`
  }

  const show = (key: string) => {
    const host = term.element
    if (!host) return
    if (!chip) {
      chip = document.createElement('button')
      chip.type = 'button'
      chip.className =
        'nx-password-chip absolute z-30 inline-flex items-center gap-1 rounded-md border border-primary/40 bg-popover/95 px-1.5 py-0.5 ' +
        'font-sans text-xs text-popover-foreground shadow-popover outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60'
      chip.addEventListener('mousedown', (e) => e.preventDefault()) // keep focus in the terminal
      host.appendChild(chip)
    }
    const label = secretLabel(key)
    chip.innerHTML = ''
    const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg')
    icon.setAttribute('viewBox', '0 0 24 24')
    icon.setAttribute('class', 'size-3 text-primary')
    icon.setAttribute('fill', 'none')
    icon.setAttribute('stroke', 'currentColor')
    icon.setAttribute('stroke-width', '2')
    icon.setAttribute('aria-hidden', 'true')
    icon.innerHTML = '<circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6"/><path d="m15.5 7.5 3 3L22 7l-3-3"/>'
    chip.appendChild(icon)
    chip.appendChild(document.createTextNode(`Send stored ${label}`))
    chip.title = `Type the ${label} stored for this connection (it is never shown)`
    chip.setAttribute('aria-label', `Send the stored ${label} to this session`)
    chip.onclick = () => void send(key)
    visibleChips.set(ctx.tabId, { sessionId: ctx.sessionId, key, send: () => void send(key) })
    place(chip)
  }

  /** The secret to offer now: the cursor sits at a password prompt the session has a stored secret for. */
  const resolve = async (): Promise<string | null> => {
    if (!automationSettings.get().passwordChip) return null
    const info = ex.handle?.info()
    if (info && (info.readOnly || info.state !== 'connected' || info.replaying)) return null
    const buf = term.buffer.active
    if (buf.type !== 'normal' || buf.viewportY !== buf.baseY) return null
    const prompt = promptBeforeCursor(term)
    if (!PROMPT_RE.test(prompt)) return null
    const keys = await sessionSecretKeys(ctx.sessionId)
    if (!keys?.injectable) return null
    return secretForPrompt(prompt, keys.keys) ?? null
  }

  // Shown / hidden only when the offered secret really changes: redraws at a prompt keep the chip in place.
  const controller = new PromptChipController({
    resolve,
    onChange: (key) => (key ? show(key) : remove()),
    onKeep: () => chip && place(chip),
  })

  const subs = [ctx.onOutput(() => controller.output()), ctx.onInput(() => controller.input())]
  const disposables = [
    term.onScroll(() => {
      if (!chip) return
      const buf = term.buffer.active
      if (buf.viewportY !== buf.baseY) controller.hide()
      else place(chip)
    }),
    term.onResize(() => chip && place(chip)),
  ]
  const unsub = automationSettings.subscribe((s) => {
    if (!s.passwordChip) controller.hide()
  })
  return () => {
    controller.dispose()
    for (const off of subs) off()
    for (const d of disposables) d.dispose()
    unsub()
    remove()
  }
}
