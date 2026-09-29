/*
 * Settings → Web pages & Xpra. User preferences (section `webView`) and, for administrators, the global `webproxy`
 * configuration the backend reads (hostSuffix, pathMode, idleMinutes).
 */
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { useAdminSettings, useUpdateAdminSettings } from '@/api/settings'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { QueryState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { errorMessage } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { webViewSettings } from './settings'

interface AdminConfig {
  hostSuffix?: string
  pathMode?: boolean
  idleMinutes?: number
}

const ZOOMS = ['0.75', '0.9', '1', '1.1', '1.25', '1.5'] as const

function AdminGroup() {
  const q = useAdminSettings()
  const update = useUpdateAdminSettings()
  const cfg = ((q.data ?? {}) as Record<string, unknown>).webproxy as AdminConfig | undefined
  const [suffix, setSuffix] = useState(cfg?.hostSuffix ?? '')
  useEffect(() => setSuffix(cfg?.hostSuffix ?? ''), [cfg?.hostSuffix])
  const save = (patch: AdminConfig) =>
    update.mutateAsync({ webproxy: patch }).then(
      () => toast.success('Saved'),
      (err) => toast.error('Could not save', { description: errorMessage(err) }),
    )
  return (
    <SettingsGroup
      title="Server (all users)"
      description="How proxied pages are served when NexTerm is opened from another machine. On localhost every page gets its own origin p-<id>.localhost automatically."
    >
      {/* Controls appear with the saved values (no default → saved flip while the settings load). */}
      <QueryState query={q} skeleton={<SkeletonRows rows={3} rowHeight={56} icon={false} />} errorTitle="Could not load the server settings">
        {() => (
          <>
            <SettingRow
              label="Wildcard domain"
              description="Serve each page as p-<id>.<domain> (own origin, full fidelity). Needs a wildcard DNS record — and with HTTPS a wildcard certificate — pointing at NexTerm."
              htmlFor="webproxy-suffix"
              stacked
            >
              <Input
                id="webproxy-suffix"
                inputSize="sm"
                className="max-w-80 font-mono"
                placeholder="apps.nexterm.example.com"
                value={suffix}
                onChange={(e) => setSuffix(e.target.value)}
                onBlur={() => suffix.trim() !== (cfg?.hostSuffix ?? '') && void save({ hostSuffix: suffix.trim() })}
                onKeyDown={(e) => e.key === 'Enter' && (e.target as HTMLInputElement).blur()}
              />
            </SettingRow>
            <SettingRow
              label="Compatibility (path) mode"
              description="Without a wildcard domain, pages are served under /proxy/… on NexTerm's own origin in a sandbox. Scripts that build absolute URLs or need cookies may not work there."
              htmlFor="webproxy-path"
            >
              <Switch id="webproxy-path" checked={cfg?.pathMode !== false} onCheckedChange={(v) => void save({ pathMode: v })} />
            </SettingRow>
            <SettingRow label="Close idle proxies after" description="Minutes without any request (open WebSockets count as activity)." htmlFor="webproxy-idle">
              <NumberInput
                id="webproxy-idle"
                className="w-28"
                min={1}
                max={1440}
                unit="min"
                value={cfg?.idleMinutes ?? 30}
                onChange={(n) => n && void save({ idleMinutes: n })}
              />
            </SettingRow>
          </>
        )}
      </QueryState>
    </SettingsGroup>
  )
}

export default function WebproxySettingsSection() {
  const s = webViewSettings.use()
  const admin = useIsAdmin()
  return (
    <SettingsPage title="Web pages & Xpra" description="Browser tabs that show web services reached from NexTerm or through SSH, and X11 applications run with Xpra.">
      <SettingsGroup title="Tabs">
        <SettingRow label="Default zoom" htmlFor="webview-zoom">
          <SimpleSelect
            id="webview-zoom"
            size="sm"
            className="w-28"
            value={String(s.defaultZoom) as (typeof ZOOMS)[number]}
            onValueChange={(v) => webViewSettings.set({ defaultZoom: Number(v) })}
            options={ZOOMS.map((z) => ({ value: z, label: `${Math.round(Number(z) * 100)} %` }))}
          />
        </SettingRow>
        <SettingRow
          label="Close the proxy with its tab"
          description="Otherwise a closed tab's proxy stays available (Reopen closed tab) until it idles out. X11 applications stop with their proxy."
          htmlFor="webview-close"
        >
          <Switch id="webview-close" checked={s.closeWithTab} onCheckedChange={(v) => webViewSettings.set({ closeWithTab: v })} />
        </SettingRow>
        <SettingRow label="Recent addresses" description={`${s.recent.length} remembered (never with passwords).`}>
          <button
            type="button"
            className="text-sm text-primary hover:underline disabled:text-muted-foreground disabled:no-underline"
            disabled={!s.recent.length}
            onClick={() => webViewSettings.set({ recent: [] })}
          >
            Clear
          </button>
        </SettingRow>
      </SettingsGroup>
      {admin && <AdminGroup />}
    </SettingsPage>
  )
}
