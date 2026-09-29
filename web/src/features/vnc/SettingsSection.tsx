/*
 * Settings → VNC: viewer defaults (a connection's own options win), keyboard and clipboard behaviour, incoming
 * connections, the trusted VeNCrypt server certificates (TOFU store, like known hosts for SSH) and — for
 * administrators — the server-wide clipboard policy (global settings section `vncPolicy`, enforced by the backend).
 */
import { Ear, RotateCcw, ShieldCheck, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useAdminSettings, useUpdateAdminSettings } from '@/api/settings'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { NumberInput } from '@/components/ui/number-input'
import { LoadingState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { Tooltip } from '@/components/ui/tooltip'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { errorMessage, formatDateTime } from '@/lib/utils'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { useDeleteTrustedCert, useTrustedCerts } from './api'
import { CLIPBOARD_DIRECTIONS } from './policy'
import { isClipboardDirection, vncSettings, type VncSettings } from './settings'
import { openListeners } from './store'
import type { ClipboardDirection, ScalingMode } from './types'

const LEVELS = Array.from({ length: 10 }, (_, i) => ({ value: String(i), label: String(i) }))

const set = (p: Partial<VncSettings>) => vncSettings.set(p)

export default function VncSettingsSection() {
  const s = vncSettings.use()
  return (
    <SettingsPage
      title="VNC"
      description="Defaults for VNC viewers. A session's own options (scaling, quality, compression, view only, auto-reconnect) take precedence."
      actions={
        <Button variant="secondary" size="sm" onClick={() => vncSettings.reset()}>
          <RotateCcw /> Reset to defaults
        </Button>
      }
    >
      <SettingsGroup title="Display">
        <SettingRow label="Scaling" description="Fit scales the desktop to the tab; Resize asks the server to match the tab size; Original shows 1:1 with zoom.">
          <SimpleSelect<ScalingMode>
            aria-label="Scaling"
            size="sm"
            className="w-48"
            value={s.scaling}
            onValueChange={(scaling) => set({ scaling })}
            options={[
              { value: 'fit', label: 'Fit to tab' },
              { value: 'remote-resize', label: 'Resize remote desktop' },
              { value: 'none', label: 'Original size (1:1)' },
            ]}
          />
        </SettingRow>
        <SettingRow label="JPEG quality" description="0 = smallest, 9 = best image (Tight / JPEG encodings).">
          <SimpleSelect aria-label="JPEG quality" size="sm" className="w-20" value={String(s.quality)} onValueChange={(v) => set({ quality: Number(v) })} options={LEVELS} />
        </SettingRow>
        <SettingRow label="Compression level" description="0 = fastest, 9 = least bandwidth.">
          <SimpleSelect aria-label="Compression level" size="sm" className="w-20" value={String(s.compression)} onValueChange={(v) => set({ compression: Number(v) })} options={LEVELS} />
        </SettingRow>
        <SettingRow
          label="Sharp remote resize on high-DPI screens"
          description="“Resize remote desktop” asks for the tab size in device pixels, so every remote pixel maps to a screen pixel. The remote desktop looks smaller unless it scales its own UI."
          htmlFor="vnc-hidpi"
        >
          <Switch id="vnc-hidpi" checked={s.hiDpi} onCheckedChange={(hiDpi) => set({ hiDpi })} />
        </SettingRow>
        <SettingRow label="Dot for hidden cursors" description="Show a small dot when the server makes the cursor invisible." htmlFor="vnc-dot">
          <Switch id="vnc-dot" checked={s.dotCursor} onCheckedChange={(dotCursor) => set({ dotCursor })} />
        </SettingRow>
        <SettingRow label="Toolbar" description="Show the viewer toolbar (it also hides automatically in full screen)." htmlFor="vnc-toolbar">
          <Switch id="vnc-toolbar" checked={s.showToolbar} onCheckedChange={(showToolbar) => set({ showToolbar })} />
        </SettingRow>
        <SettingRow label="Bell" description="What happens when the remote desktop rings the bell.">
          <SimpleSelect
            aria-label="Bell"
            size="sm"
            className="w-36"
            value={s.bell}
            onValueChange={(bell) => set({ bell })}
            options={[
              { value: 'visual', label: 'Flash' },
              { value: 'sound', label: 'Flash and beep' },
              { value: 'off', label: 'Off' },
            ]}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Keyboard and clipboard">
        <SettingRow
          label="Send all keys to the remote desktop"
          description="While the desktop has focus, Termstead shortcuts do not fire. Otherwise only plain Ctrl+key combinations go to the remote (like terminals). Full screen with keyboard lock always sends everything."
          htmlFor="vnc-capture"
        >
          <Switch id="vnc-capture" checked={s.keyboardCapture === 'all'} onCheckedChange={(v) => set({ keyboardCapture: v ? 'all' : 'standard' })} />
        </SettingRow>
        <SettingRow label="Automatic clipboard sync" description="Copy remote clipboard text locally and send local changes when the desktop gains focus (where the browser allows it). The clipboard panel always works." htmlFor="vnc-clip">
          <Switch id="vnc-clip" checked={s.clipboard === 'auto'} onCheckedChange={(v) => set({ clipboard: v ? 'auto' : 'manual' })} />
        </SettingRow>
        <SettingRow
          label="Clipboard direction"
          description="Your own limit, on top of the connection's and the administrator's policy (the strictest wins). Typing clipboard text as keystrokes counts as local → remote."
        >
          <SimpleSelect<ClipboardDirection>
            aria-label="Clipboard direction"
            size="sm"
            className="w-48"
            value={s.clipboardDirection}
            onValueChange={(clipboardDirection) => set({ clipboardDirection })}
            options={CLIPBOARD_DIRECTIONS}
          />
        </SettingRow>
        <SettingRow label="Typing speed" description="Delay between characters when typing text as keystrokes.">
          <NumberInput aria-label="Typing delay" className="w-28" inputSize="sm" unit="ms" value={s.typingDelay} min={0} max={500} onChange={(n) => set({ typingDelay: n ?? 12 })} />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Connection">
        <SettingRow label="Reconnect automatically" description="After network interruptions (never after authentication failures). A session's own auto-reconnect option wins." htmlFor="vnc-reconnect">
          <Switch id="vnc-reconnect" checked={s.autoReconnect} onCheckedChange={(autoReconnect) => set({ autoReconnect })} />
        </SettingRow>
        <SettingRow label="Open incoming connections" description="Open a tab automatically when a VNC server connects to a listener." htmlFor="vnc-incoming">
          <Switch id="vnc-incoming" checked={s.openIncoming} onCheckedChange={(openIncoming) => set({ openIncoming })} />
        </SettingRow>
        <SettingRow label="Listening for connections" description="Accept reverse connections from VNC servers (x11vnc -connect, UltraVNC SC).">
          <Button variant="secondary" size="sm" onClick={() => openListeners(true)}>
            <Ear /> Manage listeners…
          </Button>
        </SettingRow>
      </SettingsGroup>

      <TrustedCertificates />
      <AdminPolicy />
    </SettingsPage>
  )
}

/** Server-wide policy (administrators): stored in the global settings section `vncPolicy`, read by the backend only. */
function AdminPolicy() {
  const admin = useIsAdmin()
  const settings = useAdminSettings(admin)
  const update = useUpdateAdminSettings()
  if (!admin) return null
  const section = (settings.data?.vncPolicy ?? {}) as { clipboardDirection?: unknown }
  const current: ClipboardDirection = isClipboardDirection(section.clipboardDirection) ? section.clipboardDirection : 'both'
  return (
    <SettingsGroup
      title="Policy for all users"
      description="Administrators only. Applies to every VNC connection on this server, on top of each connection's own settings."
    >
      <SettingRow
        label="Clipboard"
        description="Local → remote is enforced by Termstead (clipboard messages never reach the server); remote → local is enforced by the viewer."
      >
        <LoadingState busy={settings.isLoading} skeleton={<Spinner />}>
          {(
          <SimpleSelect<ClipboardDirection>
            aria-label="Clipboard policy for all users"
            size="sm"
            className="w-48"
            disabled={update.isPending || settings.isError}
            value={current}
            onValueChange={(v) =>
              update.mutate(
                { vncPolicy: { clipboardDirection: v === 'both' ? null : v } },
                {
                  onSuccess: () => toast.success('Clipboard policy saved', { description: 'Applies to new VNC connections.' }),
                  onError: (err) => toast.error('Could not save the policy', { description: errorMessage(err) }),
                },
              )
            }
            options={CLIPBOARD_DIRECTIONS}
          />
        )}
        </LoadingState>
      </SettingRow>
    </SettingsGroup>
  )
}

function TrustedCertificates() {
  const certs = useTrustedCerts()
  const del = useDeleteTrustedCert()
  const admin = useIsAdmin()
  const mode = useRunMode()
  const canDelete = admin || mode === 'desktop'
  return (
    <SettingsGroup
      title="Trusted server certificates"
      description="VeNCrypt X.509 certificates accepted with “Accept & save”. They apply to every user, like SSH known hosts."
    >
      <LoadingState busy={certs.isLoading} skeleton={<div className="flex justify-center py-6">
          <Spinner />
        </div>}>
        {certs.isError ? (
        <p className="px-4 py-3 text-sm text-destructive">{errorMessage(certs.error)}</p>
      ) : !certs.data?.length ? (
        <EmptyState size="sm" icon={ShieldCheck} title="No saved certificates" description="Certificates signed by a trusted authority need no entry." />
      ) : (
        <ul className="divide-y">
          {certs.data.map((c) => (
            <li key={c.id} className="flex items-start gap-3 px-4 py-2.5">
              <ShieldCheck className="mt-0.5 size-4 shrink-0 text-success" />
              <div className="grid min-w-0 flex-1 gap-0.5">
                <div className="font-mono text-sm">
                  {c.host}:{c.port}
                </div>
                <div className="truncate text-xs text-muted-foreground" title={c.subject}>
                  {c.subject || 'No subject'}
                  {c.notAfter && ` · valid until ${formatDateTime(c.notAfter)}`}
                </div>
                <div className="font-mono text-2xs break-all text-muted-foreground">{c.fingerprint}</div>
                {c.comment && <div className="text-xs text-muted-foreground">{c.comment}</div>}
              </div>
              <Tooltip content={canDelete ? 'Forget this certificate' : 'Only administrators can remove certificates in server mode'}>
                <span>
                  <Button
                    size="icon-xs"
                    variant="ghost"
                    aria-label={`Forget the certificate of ${c.host}:${c.port}`}
                    disabled={!canDelete || del.isPending}
                    onClick={async () => {
                      const ok = await confirm({
                        title: `Forget the certificate of ${c.host}:${c.port}?`,
                        description: 'You will be asked to verify the server certificate again on the next connection.',
                        confirmLabel: 'Forget',
                        destructive: true,
                      })
                      if (!ok) return
                      del.mutate(c.id, {
                        onSuccess: () => toast.success('Certificate removed'),
                        onError: (err) => toast.error('Could not remove the certificate', { description: errorMessage(err) }),
                      })
                    }}
                  >
                    <Trash2 />
                  </Button>
                </span>
              </Tooltip>
            </li>
          ))}
        </ul>
      )}
      </LoadingState>
    </SettingsGroup>
  )
}
