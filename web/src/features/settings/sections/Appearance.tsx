import { Monitor, Moon, RotateCcw, Sun } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ColorSwatchPicker } from '@/components/ui/color-swatch-picker'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Slider } from '@/components/ui/slider'
import { Switch } from '@/components/ui/switch'
import { ACCENT_PRESETS } from '@/lib/theme'
import { appearanceSettings, type AppearanceSettings } from '@/stores/settings'
import { SettingRow, SettingsGroup, SettingsPage } from '../ui'

const ACCENTS = Object.entries(ACCENT_PRESETS).map(([value, p]) => ({ value, label: p.label, swatch: p.swatch }))

function Toggle({ k, label, description }: { k: keyof AppearanceSettings; label: string; description?: string }) {
  const value = appearanceSettings.useValue(k) as boolean
  const id = `appearance-${k}`
  return (
    <SettingRow label={label} description={description} htmlFor={id}>
      <Switch id={id} checked={value} onCheckedChange={(v) => appearanceSettings.set({ [k]: v } as Partial<AppearanceSettings>)} />
    </SettingRow>
  )
}

export default function AppearanceSection() {
  const a = appearanceSettings.use()
  const pct = Math.round(a.uiScale * 100)
  return (
    <SettingsPage
      title="Appearance"
      description="Theme, colours and how dense the interface is. Terminal colours are configured separately."
      actions={
        <Button variant="ghost" size="sm" onClick={() => appearanceSettings.reset()}>
          <RotateCcw /> Reset to defaults
        </Button>
      }
    >
      <SettingsGroup title="Theme">
        <SettingRow label="Colour theme" description="Follow system switches automatically with your OS setting.">
          <SegmentedControl
            aria-label="Colour theme"
            value={a.theme}
            onValueChange={(theme) => appearanceSettings.set({ theme })}
            options={[
              { value: 'dark', label: 'Dark', icon: Moon },
              { value: 'light', label: 'Light', icon: Sun },
              { value: 'system', label: 'System', icon: Monitor },
            ]}
          />
        </SettingRow>
        <SettingRow label="Accent colour" description="Used for focus, selection and active tabs." stacked>
          <ColorSwatchPicker
            aria-label="Accent colour"
            value={a.accent}
            swatches={ACCENTS}
            onChange={(accent) => appearanceSettings.set({ accent: accent ?? 'blue' })}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Size & density">
        <SettingRow label="Interface zoom" description="Scales the whole interface (not the terminal font).">
          <div className="flex w-64 items-center gap-3">
            <Slider
              aria-label="Interface zoom"
              min={80}
              max={150}
              step={5}
              value={[pct]}
              onValueChange={([v]) => appearanceSettings.set({ uiScale: v / 100 })}
            />
            <span className="w-11 text-right text-sm tabular text-muted-foreground">{pct}%</span>
          </div>
        </SettingRow>
        <SettingRow label="Density" description="Compact reduces paddings everywhere, IDE-style.">
          <SegmentedControl
            aria-label="Density"
            value={a.density}
            onValueChange={(density) => appearanceSettings.set({ density })}
            options={[
              { value: 'comfortable', label: 'Comfortable' },
              { value: 'compact', label: 'Compact' },
            ]}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Layout" description="Everything hidden here stays reachable from the command palette.">
        <Toggle k="showMenuBar" label="Title bar" description="App menu, tabs and quick connect in one row. When off, every pane shows its own tabs." />
        <Toggle k="showRibbon" label="Toolbar" description="An extra row of tool buttons (the tools are also in the sidebar rail)." />
        <Toggle k="ribbonCompact" label="Compact toolbar" description="Show toolbar icons only." />
        <Toggle k="showSidebar" label="Sidebar" />
        <Toggle k="showStatusBar" label="Status bar" />
      </SettingsGroup>
    </SettingsPage>
  )
}
