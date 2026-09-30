/*
 * VNC tab toolbar: connection state + security, Ctrl+Alt+Del, send-keys menu (combos, F-keys, VT switching, held
 * modifiers, power), clipboard panel, scaling (fit / resize remote / 1:1 + zoom), quality & compression, view-only,
 * screenshot, fullscreen, on-screen keyboard (touch), reconnect / disconnect. Dense; labels collapse by container
 * width (@container on the view).
 */
import { useState, type ReactNode } from 'react'
import {
  Camera,
  ClipboardX,
  ClipboardList,
  Copy,
  Download,
  Eye,
  EyeOff,
  Keyboard,
  Lock,
  LockOpen,
  Maximize,
  Minimize,
  Power,
  RefreshCw,
  Scan,
  Scaling,
  ShieldAlert,
  ShieldQuestion,
  SlidersHorizontal,
  Unplug,
  ZoomIn,
  ZoomOut,
  Expand,
  Type,
} from 'lucide-react'
import { toast } from 'sonner'
import type { Connection } from '@/api/types'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { IconButton } from '@/components/ui/icon-button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { Slider } from '@/components/ui/slider'
import { statusDotClass } from '@/components/ui/status-dot'
import { Switch } from '@/components/ui/switch'
import { Toolbar, ToolbarSeparator } from '@/components/ui/toolbar'
import { Tooltip } from '@/components/ui/tooltip'
import { getKeybindings } from '@/app/commands'
import { cn, errorMessage } from '@/lib/utils'
import { copyScreenshot, saveScreenshot, toggleViewerFullscreen } from './actions'
import type { VncController, VncState } from './controller'
import { COMBOS, CTRL_ALT_F_KEYS, F_KEYS, MODIFIER_LABELS, type HeldModifier } from './keys'
import { CLIPBOARD_DIRECTIONS, ENCRYPTION_POLICIES, clipboardDirectionLabel, encryptionPolicyLabel, setConnectionOption } from './policy'
import { vncSettings } from './settings'
import { setTabUI, useTabUI } from './store'
import type { ClipboardDirection, EncryptionPolicy, ScalingMode, VncInfo } from './types'

const PHASE_LABEL: Record<VncState['phase'], string> = {
  loading: 'Loading viewer',
  connecting: 'Connecting',
  reconnecting: 'Reconnecting',
  restarting: 'Starting session',
  credentials: 'Credentials required',
  verify: 'Verify server',
  confirm: 'Confirmation needed',
  connected: 'Connected',
  disconnected: 'Disconnected',
  error: 'Disconnected',
}

function phaseDot(phase: VncState['phase'], retrying: boolean): string {
  switch (phase) {
    case 'connected':
      return 'bg-success'
    case 'disconnected':
      return 'bg-muted-foreground/60'
    case 'error':
      return retrying ? statusDotClass('warning', true) : 'bg-destructive'
    default:
      return statusDotClass('warning', true)
  }
}

function shortcutOf(command: string): string | undefined {
  return getKeybindings(command)[0]
}

interface SecurityLevel {
  /** Full description (tooltip, popover title, aria-label). */
  label: string
  /** Compact text on the toolbar. */
  short: string
  tone: string
  Icon: typeof Lock
}

/**
 * One unambiguous verdict per connection. "Encrypted" is only said when the server was authenticated (X.509);
 * anonymous TLS is encrypted but unverified; weak TLS and every unencrypted form say so in words, not just colour.
 */
function securityLevel(info: VncInfo): SecurityLevel {
  if (!info.encrypted || !info.tls) {
    if (info.passwordCleartext) {
      return { label: 'Not encrypted — the password was sent in clear text', short: 'Not encrypted', tone: 'text-destructive', Icon: LockOpen }
    }
    if (info.downgrade) {
      return { label: 'Not encrypted — encryption failed and you allowed a fallback', short: 'Not encrypted', tone: 'text-destructive', Icon: LockOpen }
    }
    return { label: 'Not encrypted', short: 'Not encrypted', tone: 'text-destructive', Icon: LockOpen }
  }
  if (info.tls.weak) {
    return { label: `Weak encryption (${info.tls.dhBits ?? 1024}-bit key exchange, server not verified)`, short: 'Weak encryption', tone: 'text-warning', Icon: ShieldAlert }
  }
  if (info.tls.anonymous) {
    return { label: 'Encrypted, but the server is not verified (anonymous TLS)', short: 'Encrypted · unverified', tone: 'text-warning', Icon: ShieldQuestion }
  }
  return {
    label: info.tls.trust === 'system' ? 'Encrypted — certificate verified by a trusted authority' : 'Encrypted — certificate trusted by you',
    short: 'Encrypted',
    tone: 'text-success',
    Icon: Lock,
  }
}

function SecurityBadge({
  info,
  connected,
  size,
  connection,
  onReconnect,
}: {
  info?: VncInfo
  connected: boolean
  size?: [number, number]
  /** The saved connection when the user may change its options. */
  connection?: Connection
  onReconnect?: () => void
}) {
  if (!connected || !info?.connected) return null
  const level = securityLevel(info)
  const { Icon } = level
  const anon = info.tls?.anonymous
  return (
    <Popover>
      <Tooltip content={level.label}>
        <PopoverTrigger asChild>
          <Button
            variant="ghost"
            size="xs"
            aria-label={`Connection security: ${level.label}`}
            className={cn('shrink-0 gap-1 px-1.5 text-xs font-medium', level.tone)}
          >
            <Icon />
            <span className="hidden @3xl:inline">{level.short}</span>
          </Button>
        </PopoverTrigger>
      </Tooltip>
      <PopoverContent align="start" className="w-88 max-w-[calc(100vw-2rem)] text-sm">
        <div className={cn('mb-2 flex items-start gap-2 font-medium', level.tone)}>
          <Icon className="mt-0.5 size-4 shrink-0" />
          {level.label}
        </div>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
          <Row label="Server">{info.host && `${info.host}${info.port ? `:${info.port}` : ''}`}</Row>
          <Row label="Route">{info.route}</Row>
          <Row label="Protocol">{info.serverVersion && `${info.serverVersion} (as ${info.protocol})`}</Row>
          <Row label="Security">{info.security}</Row>
          {info.tls && (
            <>
              <Row label="TLS">{`${info.tls.version} · ${info.tls.cipherSuite}${info.tls.encryptThenMac ? ' · encrypt-then-MAC' : ''}`}</Row>
              {info.tls.group && <Row label="Key exchange">{`${info.tls.group}${info.tls.dhBits ? ` (${info.tls.dhBits} bits)` : ''}`}</Row>}
              {info.tls.subject && <Row label="Certificate">{info.tls.subject}</Row>}
              {info.tls.fingerprint && (
                <Row label="Fingerprint">
                  <span className="font-mono text-xs break-all">{info.tls.fingerprint}</span>
                </Row>
              )}
              <Row label="Trust">{trustLabel(info.tls.trust)}</Row>
            </>
          )}
          <Row label="Desktop">
            {info.desktopName &&
              `${info.desktopName}${size?.[0] ? ` · ${size[0]}×${size[1]}` : info.width ? ` · ${info.width}×${info.height}` : ''}`}
          </Row>
          <Row label="Viewers">{String(info.viewers)}</Row>
          <Row label="Clipboard">{clipboardDirectionLabel(info.clipboard)}</Row>
        </dl>
        {info.downgrade && (
          <p className="mt-2 rounded-md border border-destructive/40 bg-destructive/8 px-2 py-1.5 text-xs">
            {info.downgrade}. You allowed this connection without the encryption the server offered.
          </p>
        )}
        {info.passwordCleartext && (
          <p className="mt-2 rounded-md border border-destructive/40 bg-destructive/8 px-2 py-1.5 text-xs">
            The server only accepted a clear-text login (VeNCrypt Plain without TLS): the user name and password were
            readable on the network.
          </p>
        )}
        {anon && !info.tls?.weak && (
          <p className="mt-2 text-xs text-muted-foreground">
            Anonymous TLS protects against eavesdropping but cannot prove the server&apos;s identity. Configure X.509
            certificates on the server (X509Vnc) for authenticated encryption.
          </p>
        )}
        {info.tls?.weak && (
          <p className="mt-2 text-xs text-muted-foreground">
            A {info.tls.dhBits ?? 1024}-bit Diffie-Hellman key exchange can be broken by well-resourced attackers. Configure
            the server for a 2048-bit group (or ECDH) to remove this warning.
          </p>
        )}
        {!info.encrypted && !info.passthrough && !info.passwordCleartext && (
          <p className="mt-2 text-xs text-muted-foreground">
            The desktop and your keystrokes travel unencrypted. Use an SSH gateway or a server with VeNCrypt TLS on
            untrusted networks. The VNC password itself is not sent in clear text.
          </p>
        )}
        {info.passthrough && (
          <p className="mt-2 text-xs text-muted-foreground">
            This server&apos;s authentication runs in the browser (AstraTerm does not implement it), so credentials are
            typed here and not taken from the vault.
          </p>
        )}
        <ConnectionPolicy info={info} connection={connection} onReconnect={onReconnect} />
      </PopoverContent>
    </Popover>
  )
}

/** Encryption policy and clipboard direction of the saved connection (editable by its owner or an admin). */
function ConnectionPolicy({ info, connection, onReconnect }: { info: VncInfo; connection?: Connection; onReconnect?: () => void }) {
  const [busy, setBusy] = useState(false)
  const stored = connection?.options as Record<string, unknown> | undefined
  const storedPolicy = (typeof stored?.encryption === 'string' ? stored.encryption : 'prefer') as EncryptionPolicy
  const storedClipboard = (typeof stored?.clipboardDirection === 'string' ? stored.clipboardDirection : 'both') as ClipboardDirection
  const change = async (key: 'encryption' | 'clipboardDirection', value: string, defaultValue: string) => {
    if (!connection) return
    setBusy(true)
    try {
      await setConnectionOption(connection.id, key, value === defaultValue ? undefined : value)
      toast.success('Saved — applies to the next connection', {
        action: onReconnect ? { label: 'Reconnect now', onClick: onReconnect } : undefined,
      })
    } catch (err) {
      toast.error('Could not change the connection', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="mt-3 grid gap-2 border-t pt-3">
      <div className="text-xs font-medium text-muted-foreground">Connection policy</div>
      {connection ? (
        <>
          <label className="grid gap-1 text-xs">
            <span>Encryption</span>
            <SimpleSelect<EncryptionPolicy>
              aria-label="Encryption policy"
              size="sm"
              disabled={busy}
              value={storedPolicy}
              onValueChange={(v) => void change('encryption', v, 'prefer')}
              options={ENCRYPTION_POLICIES.map((p) => ({ value: p.value, label: p.label }))}
            />
            <span className="text-muted-foreground">{ENCRYPTION_POLICIES.find((p) => p.value === storedPolicy)?.description}</span>
          </label>
          <label className="grid gap-1 text-xs">
            <span>Clipboard</span>
            <SimpleSelect<ClipboardDirection>
              aria-label="Clipboard direction"
              size="sm"
              disabled={busy}
              value={storedClipboard}
              onValueChange={(v) => void change('clipboardDirection', v, 'both')}
              options={CLIPBOARD_DIRECTIONS}
            />
          </label>
          {info.encryptionPolicy && info.encryptionPolicy !== storedPolicy && (
            <p className="text-xs text-muted-foreground">
              This session currently uses “{encryptionPolicyLabel(info.encryptionPolicy)}” (confirmed by you).
            </p>
          )}
        </>
      ) : (
        <p className="text-xs text-muted-foreground">
          Encryption: {encryptionPolicyLabel(info.encryptionPolicy)}. Save the session to change its policies.
        </p>
      )}
    </div>
  )
}

function trustLabel(t?: string): string {
  switch (t) {
    case 'system':
      return 'Verified by a trusted authority'
    case 'saved':
      return 'Matches the saved fingerprint'
    case 'accepted':
      return 'Accepted for this session'
    case 'accepted-saved':
      return 'Accepted and saved'
    case 'none':
      return 'Not verified (anonymous)'
  }
  return t ?? ''
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  if (!children) return null
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  )
}

const SCALING_OPTIONS: { value: ScalingMode; label: string; title: string; icon: typeof Scaling }[] = [
  { value: 'fit', label: 'Fit', title: 'Scale to fit the tab', icon: Scaling },
  { value: 'remote-resize', label: 'Resize', title: 'Resize the remote desktop to the tab', icon: Expand },
  { value: 'none', label: '1:1', title: 'Original size (scrollbars) with zoom', icon: Scan },
]

export interface VncToolbarProps {
  c: VncController
  s: VncState
  info?: VncInfo
  statusMessage?: string
  touch: boolean
  onShowKeyboard: () => void
  /** The saved connection when the user may change its options (security popover). */
  editableConnection?: Connection
}

export function VncToolbar({ c, s, info, statusMessage, touch, onShowKeyboard, editableConnection }: VncToolbarProps) {
  const ui = useTabUI(c.tabId)
  const connected = s.phase === 'connected'
  const canInput = connected && !s.viewOnly
  const retrying = !!s.retryAt
  const zoomPct = Math.round((s.scaling === 'none' ? s.zoom : 1) * 100)
  return (
    <Toolbar aria-label="VNC viewer" className="h-9 gap-1 overflow-x-auto overflow-y-hidden px-1.5 [scrollbar-width:none]">
      {/* The status never shrinks away: the security badge must stay visible (the row scrolls when too narrow). */}
      <div className="flex shrink-0 items-center gap-1.5 pr-1" aria-live="polite">
        <Tooltip content={PHASE_LABEL[s.phase]}>
          <span className={cn('size-2 shrink-0 rounded-full', phaseDot(s.phase, retrying))} aria-hidden />
        </Tooltip>
        <span className="sr-only text-sm font-medium whitespace-nowrap @2xl:not-sr-only">{PHASE_LABEL[s.phase]}</span>
        {connected && s.desktopName && (
          <Tooltip content={statusMessage || s.desktopName}>
            <span className="hidden max-w-40 truncate text-sm text-muted-foreground @5xl:inline">{s.desktopName}</span>
          </Tooltip>
        )}
        <SecurityBadge
          info={info}
          connected={connected}
          size={[s.fbWidth, s.fbHeight]}
          connection={editableConnection}
          onReconnect={() => c.reconnect()}
        />
        {c.readOnly && (
          <span className="rounded-sm border border-warning/40 bg-warning/10 px-1.5 text-xs text-warning">read-only</span>
        )}
      </div>
      <ToolbarSeparator />

      {!c.readOnly && (
        <>
          <Tooltip content="Send Ctrl+Alt+Del" shortcut={shortcutOf('vnc.ctrlAltDel')}>
            <Button variant="ghost" size="xs" className="shrink-0 px-1.5 text-xs" disabled={!canInput} onClick={() => c.sendCtrlAltDel()}>
              Ctrl+Alt+Del
            </Button>
          </Tooltip>
          <KeysMenu c={c} s={s} disabled={!canInput} />
          <IconButton
            icon={s.clipboardKnown && !s.clipboard.toRemote && !s.clipboard.fromRemote ? ClipboardX : ClipboardList}
            label={
              s.clipboardKnown && !s.clipboard.toRemote && !s.clipboard.fromRemote
                ? 'Clipboard disabled by policy'
                : ui.clipboardOpen
                  ? 'Hide clipboard panel'
                  : 'Clipboard panel'
            }
            shortcut={shortcutOf('vnc.clipboard')}
            size="xs"
            active={ui.clipboardOpen}
            onClick={() => setTabUI(c.tabId, { clipboardOpen: !ui.clipboardOpen })}
          />
          {touch && <IconButton icon={Keyboard} label="On-screen keyboard" size="xs" disabled={!canInput} onClick={onShowKeyboard} />}
          <ToolbarSeparator />
        </>
      )}

      <SegmentedControl
        size="sm"
        aria-label="Scaling"
        value={s.scaling}
        onValueChange={(v) => c.setScaling(v)}
        className="h-6 shrink-0"
        options={SCALING_OPTIONS.map((o) => ({ value: o.value, icon: o.icon, title: o.title, label: <span className="hidden @4xl:inline">{o.label}</span> }))}
      />
      <div className="flex shrink-0 items-center" role="group" aria-label="Zoom">
        <IconButton icon={ZoomOut} label="Zoom out" size="xs" shortcut={shortcutOf('vnc.zoomOut')} disabled={!connected} onClick={() => c.zoomBy(-1)} />
        <Tooltip content="Reset zoom (100 %)">
          <button
            type="button"
            disabled={!connected}
            onClick={() => c.zoomReset()}
            className="h-6 min-w-10 rounded-sm px-1 text-center text-xs tabular-nums text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 disabled:opacity-45"
          >
            {s.scaling === 'none' ? `${zoomPct}%` : s.scaling === 'fit' ? 'Fit' : 'Auto'}
          </button>
        </Tooltip>
        <IconButton icon={ZoomIn} label="Zoom in" size="xs" shortcut={shortcutOf('vnc.zoomIn')} disabled={!connected} onClick={() => c.zoomBy(1)} />
      </div>
      <DisplaySettings c={c} s={s} />
      <ToolbarSeparator />

      {!c.readOnly && (
        <IconButton
          icon={s.viewOnly ? EyeOff : Eye}
          label={s.viewOnly ? 'View only (input disabled) — click to allow input' : 'Disable input (view only)'}
          size="xs"
          active={s.viewOnly}
          onClick={() => c.setViewOnly(!s.viewOnly)}
        />
      )}
      <DropdownMenu>
        <Tooltip content="Screenshot">
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-xs" aria-label="Screenshot" className="text-muted-foreground hover:text-foreground" disabled={!connected}>
              <Camera />
            </Button>
          </DropdownMenuTrigger>
        </Tooltip>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => void saveScreenshot(c)}>
            <Download /> Save as PNG
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => void copyScreenshot(c)}>
            <Copy /> Copy to clipboard
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <IconButton
        icon={ui.fullscreen ? Minimize : Maximize}
        label={ui.fullscreen ? 'Exit full screen' : 'Full screen'}
        shortcut={shortcutOf('vnc.fullscreen')}
        size="xs"
        onClick={() => void toggleViewerFullscreen(c.tabId)}
      />

      <div className="ml-auto flex shrink-0 items-center gap-0.5 pl-1">
        <IconButton icon={RefreshCw} label="Reconnect" shortcut={shortcutOf('vnc.reconnect')} size="xs" onClick={() => c.reconnect()} disabled={s.phase === 'restarting' || (c.reverse && s.phase !== 'connected')} />
        <IconButton icon={Unplug} label="Disconnect" size="xs" onClick={() => c.disconnect()} disabled={s.phase === 'disconnected' || s.phase === 'error'} />
      </div>
    </Toolbar>
  )
}

function KeysMenu({ c, s, disabled }: { c: VncController; s: VncState; disabled: boolean }) {
  return (
    <DropdownMenu>
      <Tooltip content="Send keys">
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon-xs" aria-label="Send keys" disabled={disabled} className={cn('text-muted-foreground hover:text-foreground', s.held.length && 'bg-primary/15 text-primary')}>
            <Keyboard />
          </Button>
        </DropdownMenuTrigger>
      </Tooltip>
      <DropdownMenuContent align="start" className="w-56" onCloseAutoFocus={(e) => { e.preventDefault(); c.focus() }}>
        <DropdownMenuLabel>Send</DropdownMenuLabel>
        <DropdownMenuItem onSelect={() => c.sendCtrlAltDel()}>Ctrl+Alt+Del</DropdownMenuItem>
        {COMBOS.map((k) => (
          <DropdownMenuItem key={k.id} onSelect={() => c.sendCombo(k)}>
            {k.label}
          </DropdownMenuItem>
        ))}
        <DropdownMenuSub>
          <DropdownMenuSubTrigger>Function keys</DropdownMenuSubTrigger>
          <DropdownMenuSubContent className="max-h-80 overflow-y-auto">
            {F_KEYS.map((k) => (
              <DropdownMenuItem key={k.id} onSelect={() => c.sendCombo(k)}>
                {k.label}
              </DropdownMenuItem>
            ))}
          </DropdownMenuSubContent>
        </DropdownMenuSub>
        <DropdownMenuSub>
          <DropdownMenuSubTrigger>Ctrl+Alt+F1…F12</DropdownMenuSubTrigger>
          <DropdownMenuSubContent className="max-h-80 overflow-y-auto">
            {CTRL_ALT_F_KEYS.map((k) => (
              <DropdownMenuItem key={k.id} onSelect={() => c.sendCombo(k)}>
                {k.label}
              </DropdownMenuItem>
            ))}
          </DropdownMenuSubContent>
        </DropdownMenuSub>
        <DropdownMenuSeparator />
        <DropdownMenuLabel>Hold down</DropdownMenuLabel>
        {(Object.keys(MODIFIER_LABELS) as HeldModifier[]).map((m) => (
          <DropdownMenuCheckboxItem key={m} checked={s.held.includes(m)} onSelect={(e) => e.preventDefault()} onCheckedChange={() => c.toggleHeld(m)}>
            {MODIFIER_LABELS[m]}
          </DropdownMenuCheckboxItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => setTabUI(c.tabId, { clipboardOpen: true })}>
          <Type /> Type text…
        </DropdownMenuItem>
        {s.power && (
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>
              <Power /> Power
            </DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuItem onSelect={() => c.power('reboot')}>Reboot</DropdownMenuItem>
              <DropdownMenuItem variant="destructive" onSelect={() => c.power('shutdown')}>
                Shut down
              </DropdownMenuItem>
              <DropdownMenuItem variant="destructive" onSelect={() => c.power('reset')}>
                Reset
              </DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function DisplaySettings({ c, s }: { c: VncController; s: VncState }) {
  const settings = vncSettings.use()
  const [open, setOpen] = useState(false)
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <Tooltip content="Quality & display">
        <PopoverTrigger asChild>
          <Button variant="ghost" size="icon-xs" aria-label="Quality and display settings" className="shrink-0 text-muted-foreground hover:text-foreground">
            <SlidersHorizontal />
          </Button>
        </PopoverTrigger>
      </Tooltip>
      <PopoverContent align="start" className="grid w-72 gap-4" onCloseAutoFocus={(e) => { e.preventDefault(); c.focus() }}>
        <LevelSlider
          label="JPEG quality"
          hint="0 = smallest, 9 = best image"
          value={s.quality}
          onChange={(v) => c.setQuality(v)}
        />
        <LevelSlider
          label="Compression level"
          hint="0 = fastest, 9 = least bandwidth"
          value={s.compression}
          onChange={(v) => c.setCompression(v)}
        />
        <label className="flex items-center justify-between gap-3 text-sm">
          <span>
            Show a dot for hidden cursors
            <span className="block text-xs text-muted-foreground">When the server makes the cursor invisible</span>
          </span>
          <Switch
            size="sm"
            checked={settings.dotCursor}
            onCheckedChange={(v) => {
              vncSettings.set({ dotCursor: v })
              c.setDotCursor(v)
            }}
          />
        </label>
        <label className="flex items-center justify-between gap-3 text-sm">
          <span>
            Send all keys to the remote desktop
            <span className="block text-xs text-muted-foreground">AstraTerm shortcuts do not fire while the desktop has focus</span>
          </span>
          <Switch
            size="sm"
            checked={settings.keyboardCapture === 'all'}
            onCheckedChange={(v) => vncSettings.set({ keyboardCapture: v ? 'all' : 'standard' })}
          />
        </label>
      </PopoverContent>
    </Popover>
  )
}

function LevelSlider({ label, hint, value, onChange }: { label: string; hint: string; value: number; onChange: (v: number) => void }) {
  return (
    <div className="grid gap-2">
      <div className="flex items-baseline justify-between text-sm">
        <span>{label}</span>
        <span className="tabular-nums text-muted-foreground">{value}</span>
      </div>
      <Slider min={0} max={9} step={1} value={[value]} onValueChange={(v) => onChange(v[0] ?? value)} aria-label={label} />
      <span className="text-xs text-muted-foreground">{hint}</span>
    </div>
  )
}
