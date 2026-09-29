/*
 * RDP tab toolbar: engine badge + state, Ctrl+Alt+Del, send keys (combinations, Ctrl+Alt+F-keys, type text),
 * clipboard panel, virtual drive upload (guacd), scaling, screenshot, fullscreen (Keyboard Lock), reconnect /
 * disconnect and a menu with engine switching, .rdp export and the native client. Dense; labels collapse by container
 * width (@container on the view).
 */
import { useRef, useState } from 'react'
import {
  Camera,
  CircleDot,
  ClipboardList,
  Copy,
  Download,
  EllipsisVertical,
  ExternalLink,
  Eye,
  FileDown,
  Film,
  Keyboard,
  Lock,
  Maximize,
  Minimize,
  Monitor,
  Pencil,
  RefreshCw,
  Scaling,
  Settings2,
  Type,
  Unplug,
  Upload,
} from 'lucide-react'
import { toast } from 'sonner'
import type { RuntimeSession } from '@/api/types'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { prompt } from '@/components/ui/dialog-host'
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { IconButton } from '@/components/ui/icon-button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { statusDotClass } from '@/components/ui/status-dot'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { Toolbar, ToolbarSeparator } from '@/components/ui/toolbar'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, errorMessage } from '@/lib/utils'
import { useRunMode, useServerFeatures } from '@/stores/auth'
import { downloadUrl, launchNativeSession, sessionRdpFileUrl } from './api'
import { CTRL_ALT_FN, KEY_COMBOS, MAX_TYPED_TEXT } from './keys'
import { rdpSettings } from './settings'
import { openRecordings } from './store'
import type { RdpController, RdpEngine, RdpScaling, RdpTabParams, ViewerState, ViewerStatus } from './types'

export const ENGINE_LABEL: Record<RdpEngine, string> = {
  ironrdp: 'IronRDP',
  guacd: 'guacd',
}

const ENGINE_HINT: Record<RdpEngine, string> = {
  ironrdp: 'IronRDP — RDP runs in the browser (WebAssembly); NexTerm relays the connection',
  guacd: 'guacd — Apache Guacamole renders the session; credentials stay on the server',
}

export const STATUS_LABEL: Record<ViewerStatus, string> = {
  idle: 'Idle',
  loading: 'Loading',
  connecting: 'Connecting',
  authenticating: 'Signing in',
  connected: 'Connected',
  disconnected: 'Disconnected',
  error: 'Connection failed',
  gone: 'Session ended',
}

export function statusDot(status: ViewerStatus | undefined): string {
  switch (status) {
    case 'connected':
      return 'bg-success'
    case 'error':
      return 'bg-destructive'
    case 'disconnected':
    case 'gone':
    case 'idle':
    case undefined:
      return 'bg-muted-foreground/60'
    default:
      return statusDotClass('warning', true)
  }
}

const SCALING: Array<{ value: RdpScaling; label: string; hint: string }> = [
  {
    value: 'resize',
    label: 'Resize remote to fit',
    hint: 'The remote resolution follows the tab',
  },
  {
    value: 'fit',
    label: 'Scale to fit',
    hint: 'Keep the resolution, scale the picture',
  },
  {
    value: 'none',
    label: 'Original size (1:1)',
    hint: 'Scrollbars when larger than the tab',
  },
]

interface ToolbarProps {
  tabId: string
  params: RdpTabParams
  ctrl: RdpController | null
  viewer: ViewerState | undefined
  session: RuntimeSession | undefined
}

export function RdpToolbar({ params, ctrl, viewer, session }: ToolbarProps) {
  const status = viewer?.status
  const connected = status === 'connected'
  const ended = status === 'disconnected' || status === 'error' || status === 'gone'
  const features = useServerFeatures()
  const mode = useRunMode()
  const settings = rdpSettings.use()
  const fileRef = useRef<HTMLInputElement>(null)
  const engine = viewer?.engine
  const desktop = viewer?.desktop
  const readOnly = !!viewer?.readOnly || !!params.shadow
  const destination = viewer?.destination ?? (session?.host ? session.host : undefined)

  const typeText = async () => {
    const text = await prompt({
      title: 'Type text on the remote desktop',
      description: 'The text is sent as keystrokes (useful for consoles and logon screens without clipboard).',
      label: 'Text',
      multiline: true,
      confirmLabel: 'Type',
      validate: (v) => (v.length > MAX_TYPED_TEXT ? `At most ${MAX_TYPED_TEXT.toLocaleString()} characters` : null),
    })
    if (text) void ctrl?.typeText(text)
  }

  const typeClipboard = async () => {
    try {
      const text = await navigator.clipboard.readText()
      if (!text) {
        toast.info('The clipboard is empty')
        return
      }
      void ctrl?.typeText(text)
    } catch (err) {
      toast.error('Cannot read the local clipboard', {
        description: errorMessage(err),
      })
    }
  }

  const launchNative = async () => {
    try {
      const r = await launchNativeSession(params.sessionId)
      toast.success(`Opened in ${r.client}`, {
        description: [r.forwarded ? `Through the gateway at ${r.forwarded}.` : '', r.passwordInjected ? '' : 'Sign in when the client asks.']
          .filter(Boolean)
          .join(' '),
      })
    } catch (err) {
      toast.error('Could not open the native client', {
        description: errorMessage(err),
      })
    }
  }

  return (
    <Toolbar aria-label="Remote desktop" className="gap-1 px-1.5">
      <div className="flex min-w-0 flex-1 items-center gap-2 overflow-hidden pr-1">
        <Tooltip content={engine ? ENGINE_HINT[engine] : 'Engine is chosen when connecting'}>
          <Badge variant="outline" className="gap-1 font-mono text-[11px]">
            <Monitor className="size-3" />
            {engine ? ENGINE_LABEL[engine] : 'RDP'}
          </Badge>
        </Tooltip>
        <span className="flex min-w-0 items-center gap-1.5 text-sm text-muted-foreground" aria-live="polite">
          <span className={cn('size-2 shrink-0 rounded-full', statusDot(status))} aria-hidden />
          <span className="truncate text-foreground/90">{STATUS_LABEL[status ?? 'idle']}</span>
          {destination && <span className="hidden truncate font-mono text-xs @2xl:inline">{destination}</span>}
          {viewer?.via && (
            <Badge variant="secondary" className="hidden @3xl:inline-flex">
              via {viewer.via === 'proxy' || viewer.via === 'proxy-command' ? 'proxy' : 'SSH'}
            </Badge>
          )}
          {connected && desktop && (
            <span className="hidden font-mono text-xs tabular-nums @4xl:inline">
              {desktop.width}×{desktop.height}
            </span>
          )}
          {readOnly && (
            <Tooltip content="An administrator's read-only view: no keyboard, mouse or clipboard reaches the remote desktop">
              <Badge variant="secondary" className="gap-1">
                <Eye className="size-3" /> View only
              </Badge>
            </Tooltip>
          )}
          {viewer?.recording && connected && (
            <Tooltip content="This session is being recorded (Remote desktop recordings)">
              <Badge variant="outline" className="gap-1 text-destructive">
                <CircleDot className="size-3" /> REC
              </Badge>
            </Tooltip>
          )}
        </span>
      </div>

      <div className="flex shrink-0 items-center gap-0.5">
        {!readOnly && (
          <>
            <Tooltip content="Send Ctrl+Alt+Del (Ctrl+Alt+End while the desktop has focus)">
              <Button
                variant="ghost"
                size="xs"
                className="hidden font-mono text-[11px] @xl:inline-flex"
                disabled={!connected}
                onClick={() => ctrl?.ctrlAltDel()}
              >
                Ctrl+Alt+Del
              </Button>
            </Tooltip>

            <DropdownMenu>
              <Tooltip content="Send keys">
                <DropdownMenuTrigger asChild>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label="Send keys"
                    disabled={!connected}
                    className="text-muted-foreground hover:text-foreground"
                  >
                    <Keyboard />
                  </Button>
                </DropdownMenuTrigger>
              </Tooltip>
              <DropdownMenuContent align="end" className="w-60">
                <DropdownMenuLabel>Send to the remote desktop</DropdownMenuLabel>
                {KEY_COMBOS.map((c) => (
                  <DropdownMenuItem key={c.id} onSelect={() => ctrl?.sendCombo(c)}>
                    {c.label}
                  </DropdownMenuItem>
                ))}
                <DropdownMenuSub>
                  <DropdownMenuSubTrigger>Ctrl+Alt+F1 … F12</DropdownMenuSubTrigger>
                  <DropdownMenuSubContent>
                    {CTRL_ALT_FN.map((c) => (
                      <DropdownMenuItem key={c.id} onSelect={() => ctrl?.sendCombo(c)}>
                        {c.label}
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuSubContent>
                </DropdownMenuSub>
                <DropdownMenuSeparator />
                <DropdownMenuItem onSelect={() => void typeText()}>
                  <Type /> Type text…
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => void typeClipboard()}>
                  <ClipboardList /> Type clipboard as keystrokes
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>

            <ClipboardPanel ctrl={ctrl} viewer={viewer} />
          </>
        )}

        {viewer?.driveEnabled && !readOnly && (
          <>
            <IconButton
              icon={Upload}
              label={`Upload files to the remote drive "${viewer.driveName || 'NexTerm'}" (or drop them on the desktop)`}
              disabled={!connected}
              onClick={() => fileRef.current?.click()}
            />
            <input
              ref={fileRef}
              type="file"
              multiple
              hidden
              onChange={(e) => {
                const files = Array.from(e.target.files ?? [])
                e.target.value = ''
                if (files.length) ctrl?.uploadFiles(files)
              }}
            />
          </>
        )}

        <DropdownMenu>
          <Tooltip content="Display scaling">
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" aria-label="Display scaling" className="text-muted-foreground hover:text-foreground">
                <Scaling />
              </Button>
            </DropdownMenuTrigger>
          </Tooltip>
          <DropdownMenuContent align="end" className="w-64">
            <DropdownMenuLabel>Display</DropdownMenuLabel>
            <DropdownMenuRadioGroup value={viewer?.scaling ?? 'resize'} onValueChange={(v) => ctrl?.setScaling(v as RdpScaling)}>
              {SCALING.map((s) => (
                <DropdownMenuRadioItem
                  key={s.value}
                  value={s.value}
                  disabled={s.value === 'resize' && !!viewer && viewer.status === 'connected' && !viewer.canResize}
                >
                  <div className="grid">
                    <span>{s.label}</span>
                    <span className="text-xs text-muted-foreground">{s.hint}</span>
                  </div>
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
            <DropdownMenuSeparator />
            <DropdownMenuCheckboxItem checked={settings.hiDpi} onCheckedChange={(v) => rdpSettings.set({ hiDpi: !!v })}>
              High-DPI resolution
            </DropdownMenuCheckboxItem>
          </DropdownMenuContent>
        </DropdownMenu>

        <DropdownMenu>
          <Tooltip content="Screenshot">
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="Screenshot"
                disabled={!connected}
                className="text-muted-foreground hover:text-foreground"
              >
                <Camera />
              </Button>
            </DropdownMenuTrigger>
          </Tooltip>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => void ctrl?.saveScreenshot()}>
              <Download /> Save as PNG
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => void ctrl?.copyScreenshot()}>
              <Copy /> Copy to clipboard
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>

        <IconButton
          icon={viewer?.fullscreen ? Minimize : Maximize}
          label={viewer?.fullscreen ? 'Exit fullscreen' : 'Fullscreen'}
          shortcut="Control+Alt+Enter"
          onClick={() => void ctrl?.toggleFullscreen()}
        />
        {viewer?.keyboardLocked && (
          <Tooltip content="System keys (Esc, Alt+Tab, Win…) go to the remote desktop. Hold Esc to leave fullscreen.">
            <span className="flex size-7 items-center justify-center text-primary" aria-label="Keyboard locked">
              <Lock className="size-3.5" />
            </span>
          </Tooltip>
        )}

        <ToolbarSeparator />
        {ended || !viewer ? (
          <IconButton icon={RefreshCw} label="Reconnect" onClick={() => ctrl?.reconnect()} disabled={!ctrl || status === 'gone'} />
        ) : (
          <IconButton icon={Unplug} label="Disconnect" onClick={() => ctrl?.disconnect()} disabled={!ctrl || status === 'idle'} />
        )}

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label="More actions" className="text-muted-foreground hover:text-foreground">
              <EllipsisVertical />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-64">
            <DropdownMenuItem onSelect={() => ctrl?.reconnect()} disabled={!ctrl || status === 'gone'}>
              <RefreshCw /> Reconnect
            </DropdownMenuItem>
            {!readOnly && (
              <DropdownMenuItem
                onSelect={() =>
                  ctrl?.reconnect({
                    engine: engine === 'guacd' ? 'ironrdp' : 'guacd',
                  })
                }
                disabled={!ctrl || status === 'gone' || (engine !== 'guacd' && features?.guacd === false)}
              >
                <Monitor /> {engine === 'guacd' ? 'Reconnect with IronRDP' : 'Reconnect with guacd'}
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => downloadUrl(sessionRdpFileUrl(params.sessionId))}>
              <FileDown /> Download .rdp file
            </DropdownMenuItem>
            {mode === 'desktop' && !readOnly && (
              <DropdownMenuItem onSelect={() => void launchNative()} disabled={status === 'gone'}>
                <ExternalLink /> Open in the native client
              </DropdownMenuItem>
            )}
            {params.connectionId && isCommandEnabled('sessions.edit') && (
              <DropdownMenuItem onSelect={() => void runCommand('sessions.edit', { id: params.connectionId })}>
                <Pencil /> Edit session…
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => openRecordings()}>
              <Film /> Recordings…
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => void runCommand('settings.open', { section: 'rdp' })}>
              <Settings2 /> Remote desktop settings
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </Toolbar>
  )
}

function ClipboardPanel({ ctrl, viewer }: { ctrl: RdpController | null; viewer: ViewerState | undefined }) {
  const [open, setOpen] = useState(false)
  const [text, setText] = useState('')
  const settings = rdpSettings.use()
  const connected = viewer?.status === 'connected'
  const enabled = viewer?.clipboardEnabled !== false
  const pending = !!viewer?.remoteClipboardPending
  const remote = viewer?.remoteClipboardText

  const run = async (fn: () => Promise<void>, ok: string) => {
    try {
      await fn()
      toast.success(ok)
    } catch (err) {
      toast.error('Clipboard', { description: errorMessage(err) })
    }
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <Tooltip content={pending ? 'Clipboard — the remote clipboard changed' : 'Clipboard'}>
        <PopoverTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Clipboard"
            disabled={!viewer}
            className={cn('relative text-muted-foreground hover:text-foreground', open && 'bg-accent text-foreground')}
          >
            <ClipboardList />
            {pending && <span className="absolute top-1 right-1 size-1.5 rounded-full bg-primary" aria-hidden />}
          </Button>
        </PopoverTrigger>
      </Tooltip>
      <PopoverContent align="end" className="grid w-80 gap-3 p-3">
        <div className="flex items-center justify-between gap-2">
          <div className="text-base font-medium">Clipboard</div>
          <label className="flex items-center gap-2 text-sm text-muted-foreground">
            Automatic
            <Switch checked={settings.autoClipboard} onCheckedChange={(v) => rdpSettings.set({ autoClipboard: v })} disabled={!enabled} />
          </label>
        </div>
        {!enabled ? (
          <p className="text-sm text-muted-foreground">The clipboard is disabled for this connection.</p>
        ) : (
          <>
            {(pending || remote) && (
              <div className="grid gap-1.5">
                <div className="text-xs font-medium text-muted-foreground">From the remote desktop</div>
                {remote !== undefined && (
                  <pre className="max-h-24 overflow-auto rounded-md border bg-muted/50 px-2 py-1 font-mono text-xs break-all whitespace-pre-wrap">
                    {remote || '(empty)'}
                  </pre>
                )}
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => void run(() => ctrl!.copyRemoteClipboard(), 'Copied to the local clipboard')}
                  disabled={!ctrl}
                >
                  <Copy /> Copy to the local clipboard
                </Button>
              </div>
            )}
            <div className="grid gap-1.5">
              <div className="text-xs font-medium text-muted-foreground">To the remote desktop</div>
              <Textarea
                value={text}
                onChange={(e) => setText(e.target.value)}
                rows={4}
                placeholder="Text to send…"
                className="font-mono text-xs"
                onKeyDown={(e) => e.stopPropagation()}
              />
              <div className="flex flex-wrap gap-1.5">
                <Button
                  size="sm"
                  disabled={!connected || !text}
                  onClick={() => void run(() => ctrl!.sendClipboardText(text), 'Sent to the remote clipboard')}
                >
                  Send to clipboard
                </Button>
                <Button size="sm" variant="secondary" disabled={!connected || !text} onClick={() => void ctrl?.typeText(text)}>
                  <Type /> Type it
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={async () => {
                    try {
                      setText(await navigator.clipboard.readText())
                    } catch (err) {
                      toast.error('Cannot read the local clipboard', {
                        description: errorMessage(err),
                      })
                    }
                  }}
                >
                  Paste local
                </Button>
              </div>
            </div>
            {!window.isSecureContext && <p className="text-xs text-warning">Automatic clipboard access needs HTTPS (or localhost).</p>}
          </>
        )}
      </PopoverContent>
    </Popover>
  )
}
