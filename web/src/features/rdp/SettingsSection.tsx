/*
 * Settings → Remote desktop: viewer preferences (per user, section "rdpViewer") and — for administrators — the
 * engines: default engine, guacd address and the guacd Docker sidecar (CORE-16), stored in the global section "rdp".
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { CircleAlert, CircleCheck, Container, Film, Play, Square } from 'lucide-react'
import { toast } from 'sonner'
import { watchJob } from '@/api/jobs'
import { useAdminSettings, useUpdateAdminSettings } from '@/api/settings'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { cn, errorMessage, isPlainObject } from '@/lib/utils'
import { refreshAuth, useIsAdmin, useRunMode } from '@/stores/auth'
import { rdpKeys, sidecarAction, useGuacdStatus } from './api'
import { rdpSettings, type RdpViewerSettings } from './settings'
import { openRecordings } from './store'
import type { GuacdStatusInfo, RdpEngine, RdpGlobalSettings, RdpScaling } from './types'

type BoolKey = { [K in keyof RdpViewerSettings]: RdpViewerSettings[K] extends boolean ? K : never }[keyof RdpViewerSettings]

function Toggle({ k, label, description }: { k: BoolKey; label: string; description?: string }) {
  const value = rdpSettings.useValue(k)
  const id = `rdp-${k}`
  return (
    <SettingRow label={label} description={description} htmlFor={id}>
      <Switch id={id} checked={value} onCheckedChange={(v) => rdpSettings.set({ [k]: v } as Partial<RdpViewerSettings>)} />
    </SettingRow>
  )
}

export default function RdpSettingsSection() {
  const isAdmin = useIsAdmin()
  const scaling = rdpSettings.useValue('scaling')
  return (
    <SettingsPage
      title="Remote desktop"
      description="RDP sessions run in the browser with the built-in IronRDP engine, or through Apache Guacamole's guacd for full fidelity (audio, drives, printing)."
    >
      <SettingsGroup title="Viewer">
        <SettingRow label="Scaling" description="How new remote desktop tabs fit the desktop into the tab.">
          <SegmentedControl<RdpScaling>
            size="sm"
            aria-label="Scaling"
            value={scaling}
            onValueChange={(v) => rdpSettings.set({ scaling: v })}
            options={[
              { value: 'resize', label: 'Resize remote' },
              { value: 'fit', label: 'Scale to fit' },
              { value: 'none', label: '1:1' },
            ]}
          />
        </SettingRow>
        <Toggle k="hiDpi" label="High-DPI resolution" description="On high-density screens, request the remote resolution in device pixels (sharper, more bandwidth)." />
        <Toggle
          k="autoClipboard"
          label="Automatic clipboard"
          description="Synchronise the clipboard while the remote desktop has focus. The browser asks once for clipboard access; without it, use the clipboard panel."
        />
        <Toggle k="keyboardLock" label="Capture system keys in fullscreen" description="Esc, Alt+Tab and the Windows key go to the remote desktop (Chromium browsers)." />
        <Toggle k="localCursor" label="Native cursor" description="Show the remote mouse cursor with the browser's cursor (guacd engine)." />
      </SettingsGroup>
      <SettingsGroup title="Recordings">
        <SettingRow
          label="Session recordings"
          description='Connections with "Record sessions" are recorded by Termstead when they use the guacd engine.'
        >
          <Button size="sm" variant="secondary" onClick={() => openRecordings()}>
            <Film /> Open recordings
          </Button>
        </SettingRow>
      </SettingsGroup>
      <SettingsGroup title="Keyboard shortcuts in the remote desktop">
        <SettingRow label="Ctrl+Alt+End" description="Sends Ctrl+Alt+Del to the remote computer.">
          <span />
        </SettingRow>
        <SettingRow label="Ctrl+Alt+Enter" description="Toggles fullscreen (also Ctrl+Alt+Break).">
          <span />
        </SettingRow>
      </SettingsGroup>
      {isAdmin ? <EngineAdmin /> : <EngineInfo />}
    </SettingsPage>
  )
}

function StatusLine({ st, loading }: { st?: GuacdStatusInfo; loading: boolean }) {
  if (loading && !st) return <Spinner />
  if (!st) return null
  if (!st.configured) return <Badge variant="secondary">Not configured</Badge>
  if (st.reachable)
    return (
      <Badge variant="success">
        <CircleCheck /> Reachable{st.version ? ` · protocol ${st.version}` : ''}
      </Badge>
    )
  return (
    <Badge variant="destructive">
      <CircleAlert /> Not reachable
    </Badge>
  )
}

function EngineInfo() {
  const { data: st, isLoading } = useGuacdStatus(true)
  return (
    <SettingsGroup title="Engines" description="Engines are configured by an administrator.">
      <SettingRow label="Default engine">
        <span className="text-sm">{st?.defaultEngine === 'guacd' ? 'guacd' : 'IronRDP (built in)'}</span>
      </SettingRow>
      <SettingRow label="guacd">
        <StatusLine st={st} loading={isLoading} />
      </SettingRow>
    </SettingsGroup>
  )
}

function EngineAdmin() {
  const qc = useQueryClient()
  const { data: st, isLoading, refetch } = useGuacdStatus(true, 10_000)
  const admin = useAdminSettings(true)
  const update = useUpdateAdminSettings()
  const global = useMemo<RdpGlobalSettings>(() => {
    const raw = admin.data?.rdp
    return isPlainObject(raw) ? (raw as RdpGlobalSettings) : {}
  }, [admin.data])
  const [address, setAddress] = useState('')
  const [forwardHost, setForwardHost] = useState('')
  const [dataPath, setDataPath] = useState('')
  useEffect(() => {
    setAddress(global.guacdAddress ?? '')
    setForwardHost(global.guacdForwardHost ?? '')
    setDataPath(global.guacdDataPath ?? '')
  }, [global.guacdAddress, global.guacdForwardHost, global.guacdDataPath])

  const save = (patch: Record<string, unknown>, ok: string) => {
    update.mutate(
      { rdp: patch },
      {
        onSuccess: () => {
          toast.success(ok)
          void qc.invalidateQueries({ queryKey: rdpKeys.guacdStatus })
          void refreshAuth() // features.guacd
        },
        onError: (err) => toast.error('Could not save', { description: errorMessage(err) }),
      },
    )
  }

  const validAddress = (v: string) => v === '' || v === 'off' || /^(\[[0-9a-fA-F:.]+\]|[A-Za-z0-9._-]+):\d{1,5}$/.test(v)

  return (
    <>
      <SettingsGroup title="Engines" description="Connections can pick an engine; the default applies to the others.">
        <SettingRow label="Default engine" description="IronRDP needs nothing else; guacd must be running and reachable from this server.">
          <SimpleSelect<RdpEngine>
            size="sm"
            className="w-48"
            aria-label="Default engine"
            value={global.defaultEngine ?? 'ironrdp'}
            onValueChange={(v) => save({ defaultEngine: v === 'ironrdp' ? null : v }, 'Default engine saved')}
            options={[
              { value: 'ironrdp', label: 'IronRDP (built in)' },
              { value: 'guacd', label: 'guacd', disabled: !st?.reachable && global.defaultEngine !== 'guacd' },
            ]}
          />
        </SettingRow>
        <SettingRow
          label="guacd"
          description={
            <>
              {st?.address ? (
                <>
                  <span className="font-mono">{st.address}</span>
                  {st.source === 'flag' ? ' (--guacd)' : st.source === 'sidecar' ? ' (Docker sidecar)' : ' (settings)'}
                </>
              ) : (
                'No guacd address configured.'
              )}
              {st?.error && <span className="block text-destructive">{st.error}</span>}
            </>
          }
        >
          <StatusLine st={st} loading={isLoading} />
        </SettingRow>
        <SettingRow label="guacd address" description='host:port of guacd; empty uses the --guacd flag, "off" disables guacd.' stacked>
          <form
            className="flex w-full gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              const v = address.trim()
              if (!validAddress(v)) {
                toast.error('Enter host:port, "off" or leave it empty')
                return
              }
              save({ guacdAddress: v || null, guacdSidecar: null }, 'guacd address saved')
            }}
          >
            <Input inputSize="sm" className="font-mono" value={address} onChange={(e) => setAddress(e.target.value)} placeholder="127.0.0.1:4822" aria-label="guacd address" aria-invalid={!validAddress(address.trim()) || undefined} />
            <Button size="sm" type="submit" loading={update.isPending} disabled={address.trim() === (global.guacdAddress ?? '')}>
              Save
            </Button>
          </form>
        </SettingRow>
        <SettingRow
          label="Gateway forwarding host"
          description="For connections through SSH gateways or proxies, guacd connects to a forwarder on this server at this address (127.0.0.1 for a local guacd, host.docker.internal for a container)."
          stacked
        >
          <form
            className="flex w-full gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              save({ guacdForwardHost: forwardHost.trim() || null }, 'Forwarding host saved')
            }}
          >
            <Input inputSize="sm" className="font-mono" value={forwardHost} onChange={(e) => setForwardHost(e.target.value)} placeholder="127.0.0.1" aria-label="Gateway forwarding host" />
            <Button size="sm" type="submit" variant="secondary" disabled={forwardHost.trim() === (global.guacdForwardHost ?? '')}>
              Save
            </Button>
          </form>
        </SettingRow>
        <SettingRow label="guacd data directory" description="Writable directory in guacd's filesystem for virtual drives and session recordings." stacked>
          <form
            className="flex w-full gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              const v = dataPath.trim()
              if (v && (!v.startsWith('/') || v.includes('..'))) {
                toast.error('Use an absolute path')
                return
              }
              save({ guacdDataPath: v || null }, 'Data directory saved')
            }}
          >
            <Input inputSize="sm" className="font-mono" value={dataPath} onChange={(e) => setDataPath(e.target.value)} placeholder="/tmp/termstead" aria-label="guacd data directory" />
            <Button size="sm" type="submit" variant="secondary" disabled={dataPath.trim() === (global.guacdDataPath ?? '')}>
              Save
            </Button>
          </form>
        </SettingRow>
      </SettingsGroup>
      <Sidecar st={st} onChanged={() => void refetch()} />
    </>
  )
}

function Sidecar({ st, onChanged }: { st?: GuacdStatusInfo; onChanged: () => void }) {
  const qc = useQueryClient()
  const mode = useRunMode()
  const sc = st?.sidecar
  const [busy, setBusy] = useState<'start' | 'stop' | null>(null)
  const [log, setLog] = useState<string[]>([])
  const unsub = useRef<(() => void) | null>(null)
  useEffect(() => () => unsub.current?.(), [])

  const run = async (action: 'start' | 'stop') => {
    setBusy(action)
    setLog([])
    try {
      const { jobId } = await sidecarAction(action)
      unsub.current?.()
      unsub.current = watchJob(jobId, (ev) => {
        if (ev.event === 'data') {
          const d = ev.data as { message?: string }
          if (d?.message) setLog((l) => [...l.slice(-7), d.message!])
          return
        }
        unsub.current?.()
        unsub.current = null
        setBusy(null)
        if (ev.event === 'done') toast.success(action === 'start' ? 'guacd is running' : 'guacd stopped')
        else toast.error(action === 'start' ? 'Could not start guacd' : 'Could not stop guacd', { description: ev.error })
        void qc.invalidateQueries({ queryKey: ['admin', 'settings'] })
        void refreshAuth()
        onChanged()
      })
    } catch (err) {
      setBusy(null)
      toast.error('Docker', { description: errorMessage(err) })
    }
  }

  return (
    <SettingsGroup
      title="guacd sidecar (Docker)"
      description={`Runs ${sc?.image ?? 'guacamole/guacd'} in a local Docker container published on ${sc?.address ?? '127.0.0.1:4822'} and makes it this server's guacd.${mode === 'server' ? ' The container runs on the Termstead server host.' : ''}`}
    >
      <SettingRow
        label={
          <span className="flex items-center gap-2">
            <Container className="size-4 text-muted-foreground" /> {sc?.container ?? 'termstead-guacd'}
          </span>
        }
        description={
          sc
            ? sc.dockerAvailable
              ? `Docker ${sc.dockerVersion ?? ''} · ${sc.running ? 'running' : sc.exists ? 'stopped' : 'not created'}${sc.active ? ' · in use' : ''}`
              : sc.error || 'Docker is not available'
            : 'Checking Docker…'
        }
      >
        <div className="flex items-center gap-1.5">
          {sc?.running ? (
            <Button size="sm" variant="secondary" loading={busy === 'stop'} disabled={!!busy} onClick={() => void run('stop')}>
              <Square /> Stop
            </Button>
          ) : (
            <Button size="sm" loading={busy === 'start'} disabled={!!busy || !sc?.dockerAvailable} onClick={() => void run('start')}>
              <Play /> {sc?.exists ? 'Start' : 'Download & start'}
            </Button>
          )}
        </div>
      </SettingRow>
      {(busy || log.length > 0) && (
        <div className={cn('grid gap-0.5 px-4 py-3 font-mono text-xs text-muted-foreground')} aria-live="polite">
          {log.map((l, i) => (
            <div key={i} className="truncate">
              {l}
            </div>
          ))}
          <Spinner active={!!busy && log.length === 0} className="size-3.5" />
        </div>
      )}
    </SettingsGroup>
  )
}
