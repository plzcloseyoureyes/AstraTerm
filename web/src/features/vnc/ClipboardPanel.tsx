/*
 * Clipboard panel (GFX-2): remote → local (last text the server sent, with Copy) and local → remote (send to the
 * remote clipboard, or type it as keystrokes for consoles without clipboard support). Automatic sync (settings
 * `vnc.clipboard = auto`) works where the browser allows clipboard access; this panel always works — within the
 * effective direction policy (connection / administrator policy, NexTerm-enforced for local → remote, and the
 * user's own setting). Typing text is disabled with local → remote too, so the policy is not bypassed by keystrokes.
 */
import { useState } from 'react'
import { Ban, ClipboardPaste, Copy, Send, Type, X } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { IconButton } from '@/components/ui/icon-button'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { formatRelativeTime } from '@/lib/utils'
import { useNow } from '@/lib/hooks'
import type { VncController, VncState } from './controller'
import { vncSettings } from './settings'
import { setTabUI } from './store'

const MAX_TYPE = 20_000

export function ClipboardPanel({ c, s }: { c: VncController; s: VncState }) {
  const [local, setLocal] = useState('')
  const settings = vncSettings.use()
  useNow(15_000)
  const canInput = s.phase === 'connected' && !s.viewOnly
  const known = s.clipboardKnown
  const toRemote = known && s.clipboard.toRemote
  const fromRemote = known && s.clipboard.fromRemote

  const pasteLocal = async () => {
    try {
      const text = await navigator.clipboard.readText()
      if (!text) toast.info('The clipboard is empty')
      setLocal(text)
    } catch {
      toast.error('Cannot read the clipboard here', { description: 'Paste into the text box with Ctrl+V / ⌘V instead.' })
    }
  }

  return (
    <aside
      aria-label="Clipboard"
      className="flex w-72 shrink-0 flex-col gap-3 overflow-y-auto border-l bg-panel p-3 text-sm @max-2xl:absolute @max-2xl:inset-y-0 @max-2xl:right-0 @max-2xl:z-20 @max-2xl:shadow-popover"
    >
      <div className="flex items-center justify-between">
        <h2 className="font-medium">Clipboard</h2>
        <IconButton icon={X} label="Close clipboard panel" size="xs" onClick={() => setTabUI(c.tabId, { clipboardOpen: false })} />
      </div>

      <section className="grid gap-1.5" aria-disabled={known && !fromRemote}>
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <span>From the remote desktop</span>
          {s.remoteClipboardAt && <span>{formatRelativeTime(s.remoteClipboardAt)}</span>}
        </div>
        {known && !fromRemote && <PolicyNote blocked={s.clipboardBlocked}>Remote → local is disabled by policy.</PolicyNote>}
        <Textarea
          readOnly
          value={s.remoteClipboard ?? ''}
          placeholder="Copy something on the remote desktop"
          className="h-28 resize-none font-mono text-xs"
          aria-label="Remote clipboard"
        />
        <Button
          size="sm"
          variant="secondary"
          disabled={!s.remoteClipboard || !fromRemote}
          onClick={async () => {
            if (await c.copyRemoteClipboard()) toast.success('Copied to the local clipboard')
            else toast.error('Could not write to the clipboard')
          }}
        >
          <Copy /> Copy to local clipboard
        </Button>
      </section>

      <section className="grid gap-1.5" aria-disabled={known && !toRemote}>
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <span>To the remote desktop</span>
          <button
            type="button"
            className="inline-flex items-center gap-1 hover:text-foreground disabled:pointer-events-none disabled:opacity-45"
            disabled={!toRemote}
            onClick={() => void pasteLocal()}
          >
            <ClipboardPaste className="size-3" /> Paste local
          </button>
        </div>
        {known && !toRemote && <PolicyNote>Local → remote (including typing it) is disabled by policy.</PolicyNote>}
        <Textarea
          disabled={!toRemote}
          value={local}
          onChange={(e) => setLocal(e.target.value)}
          placeholder="Text to send"
          className="h-28 resize-none font-mono text-xs"
          aria-label="Text for the remote desktop"
          spellCheck={false}
        />
        <div className="grid grid-cols-2 gap-1.5">
          <Button
            size="sm"
            disabled={!canInput || !local || !toRemote}
            onClick={() => {
              if (c.sendClipboard(local)) toast.success('Sent to the remote clipboard', { description: 'Paste it there with Ctrl+V.' })
            }}
          >
            <Send /> Send
          </Button>
          {s.typing ? (
            <Button size="sm" variant="secondary" onClick={() => c.cancelTyping()}>
              <X /> Stop typing
            </Button>
          ) : (
            <Button
              size="sm"
              variant="secondary"
              disabled={!canInput || !local || !toRemote}
              title="Types the text as key presses (for BIOS, iLO/iDRAC and login screens without clipboard)"
              onClick={() => {
                if (local.length > MAX_TYPE) {
                  toast.error(`Too long to type (${local.length} characters, max ${MAX_TYPE})`)
                  return
                }
                c.focus()
                void c.typeText(local).then((n) => n > 0 && toast.success(`Typed ${n} characters`))
              }}
            >
              <Type /> Type
            </Button>
          )}
        </div>
      </section>

      <label className="mt-auto flex items-center justify-between gap-3 border-t pt-3 text-xs">
        <span>
          Automatic sync
          <span className="block text-muted-foreground">Both ways, where the browser allows clipboard access</span>
        </span>
        <Switch size="sm" checked={settings.clipboard === 'auto'} onCheckedChange={(v) => vncSettings.set({ clipboard: v ? 'auto' : 'manual' })} />
      </label>
    </aside>
  )
}

function PolicyNote({ children, blocked }: { children: React.ReactNode; blocked?: number }) {
  return (
    <p className="flex items-start gap-1.5 rounded-md border border-warning/40 bg-warning/10 px-2 py-1 text-xs">
      <Ban className="mt-0.5 size-3 shrink-0 text-warning" />
      <span>
        {children}
        {!!blocked && ` ${blocked} update${blocked === 1 ? '' : 's'} ignored.`}
      </span>
    </p>
  )
}
