/*
 * Settings → Monitoring (settings section `monitor`).
 */
import { RotateCcw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { BAR_ITEMS, MONITOR_DEFAULTS, monitorSettings, type BarItem, type MonitorSettings } from './settings'

type BoolKey = { [K in keyof MonitorSettings]: MonitorSettings[K] extends boolean ? K : never }[keyof MonitorSettings]

function Toggle({ k, label, description }: { k: BoolKey; label: string; description?: string }) {
  const v = monitorSettings.useValue(k)
  const id = `mon-${k}`
  return (
    <SettingRow label={label} description={description} htmlFor={id}>
      <Switch id={id} checked={v} onCheckedChange={(c) => monitorSettings.set({ [k]: c } as Partial<MonitorSettings>)} />
    </SettingRow>
  )
}

export default function MonitoringSettingsSection() {
  const s = monitorSettings.use()
  const setItems = (id: BarItem, on: boolean) => {
    const set = new Set(s.barItems)
    if (on) set.add(id)
    else set.delete(id)
    monitorSettings.set({ barItems: BAR_ITEMS.map((i) => i.id).filter((i) => set.has(i)) })
  }
  return (
    <SettingsPage
      title="Monitoring"
      description="The remote monitoring bar of SSH sessions, host monitor views and Caffeine."
      actions={
        <Button size="sm" variant="outline" onClick={() => monitorSettings.reset()}>
          <RotateCcw /> Reset
        </Button>
      }
    >
      <SettingsGroup title="Remote monitoring bar" description="Live host statistics of the active SSH tab in the status bar.">
        <Toggle k="showBar" label="Show the remote monitoring bar" description="Samples CPU, memory, disk, network, users and uptime every 2 seconds over the tab's SSH connection." />
        <Toggle k="barForLocal" label="Also for local terminals" description="Show the Termstead computer's statistics when a local shell tab is active." />
        <SettingRow label="Items" description="What the bar shows (hover an item for details, click for the monitor tab)." stacked>
          <div className="grid grid-cols-2 gap-x-6 gap-y-1.5 @xl:grid-cols-3">
            {BAR_ITEMS.map((i) => (
              <label key={i.id} className="flex items-center gap-2 text-sm">
                <Checkbox checked={s.barItems.includes(i.id)} onCheckedChange={(c) => setItems(i.id, c === true)} />
                {i.label}
              </label>
            ))}
          </div>
        </SettingRow>
        <Toggle k="sparklines" label="Sparklines" description="Tiny history graphs next to CPU, memory and network." />
        <SettingRow label="Warning threshold" description="Usage (%) turning an item orange." htmlFor="mon-warn">
          <NumberInput id="mon-warn" className="w-28" inputSize="sm" value={s.warnPct} min={1} max={99} unit="%" onChange={(v) => v != null && monitorSettings.set({ warnPct: v, critPct: Math.max(s.critPct, v + 1) })} />
        </SettingRow>
        <SettingRow label="Critical threshold" description="Usage (%) turning an item red." htmlFor="mon-crit">
          <NumberInput id="mon-crit" className="w-28" inputSize="sm" value={s.critPct} min={2} max={100} unit="%" onChange={(v) => v != null && monitorSettings.set({ critPct: v, warnPct: Math.min(s.warnPct, v - 1) })} />
        </SettingRow>
        <Toggle
          k="pauseHidden"
          label="Pause while Termstead is hidden"
          description="Stops sampling when no Termstead window is visible; the sampler uses one SSH channel, which counts toward the server's MaxSessions."
        />
      </SettingsGroup>
      <SettingsGroup title="Host monitor">
        <SettingRow label="Process list refresh" description="How often the Processes view updates while it is visible." htmlFor="mon-proc">
          <SimpleSelect
            id="mon-proc"
            size="sm"
            className="w-36"
            value={String(s.processRefreshSec)}
            onValueChange={(v) => monitorSettings.set({ processRefreshSec: Number(v) })}
            options={[
              { value: '0', label: 'Manual' },
              { value: '2', label: 'Every 2 s' },
              { value: '3', label: 'Every 3 s' },
              { value: '5', label: 'Every 5 s' },
              { value: '10', label: 'Every 10 s' },
            ]}
          />
        </SettingRow>
        <Toggle k="showKernelThreads" label="Show kernel threads" description="Include Linux kernel threads ([kworker/…] etc.) in process lists." />
        <SettingRow label="Log history" description="Lines loaded before following a log live." htmlFor="mon-lines">
          <NumberInput id="mon-lines" className="w-28" inputSize="sm" value={s.logLines} min={0} max={5000} step={50} onChange={(v) => v != null && monitorSettings.set({ logLines: v })} />
        </SettingRow>
        <SettingRow label="Suggested log files" description="Offered in the log viewer." stacked>
          <TagInput
            value={s.logFiles}
            onChange={(v) => monitorSettings.set({ logFiles: v })}
            normalize={(t) => {
              const v = t.trim()
              return v.startsWith('/') ? v : ''
            }}
            placeholder="/var/log/…"
            aria-label="Suggested log files"
          />
        </SettingRow>
      </SettingsGroup>
      <SettingsGroup title="Caffeine" description="Keep this computer (the Termstead host) and the screen awake during long jobs.">
        <Toggle k="caffeineStatusItem" label="Caffeine toggle in the status bar" />
      </SettingsGroup>
      <p className="text-xs text-muted-foreground">
        Per connection, the bar can be turned off with the SSH option “monitoring” (Session editor). Defaults: warning {MONITOR_DEFAULTS.warnPct}%, critical {MONITOR_DEFAULTS.critPct}%.
      </p>
    </SettingsPage>
  )
}
