import { Monitor, Moon, RotateCcw, Sun } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ColorSwatchPicker } from '@/components/ui/color-swatch-picker'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { Slider } from '@/components/ui/slider'
import { Switch } from '@/components/ui/switch'
import { DESKTOP_APP, MIN_WINDOW_OPACITY, TRANSLUCENCY_SUPPORTED } from '@/lib/desktop'
import { terminalSettings } from '@/features/terminal/settings'
import { ACCENT_PRESETS, APP_THEMES, isDarkTheme, type AppTheme } from '@/lib/theme'
import { cn } from '@/lib/utils'
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
  const opacity = Math.round(a.windowOpacity * 100)
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
        <SettingRow label="App theme" description="Colours of the whole interface. Picking one also switches the terminal to its colour scheme." stacked>
          <AppThemePicker value={a.appTheme} />
        </SettingRow>
        <SettingRow label="Accent colour" description="Used for focus, selection and active tabs." stacked>
          <ColorSwatchPicker
            aria-label="Accent colour"
            value={a.accent}
            swatches={ACCENTS}
            onChange={(accent) => appearanceSettings.set({ accent: accent ?? 'blue' })}
          />
        </SettingRow>
        <SettingRow
          label="Hardware acceleration"
          description={`Uses the GPU to draw terminals and effects. Turn it off if text looks garbled or the app is slow on this computer.${DESKTOP_APP ? ' The app applies it fully after a restart.' : ''}`}
          htmlFor="appearance-gpu"
        >
          <Switch id="appearance-gpu" checked={a.hardwareAcceleration} onCheckedChange={(v) => appearanceSettings.set({ hardwareAcceleration: v })} />
        </SettingRow>
        {TRANSLUCENCY_SUPPORTED && a.hardwareAcceleration && (
          <SettingRow label="Window opacity" description="Below 100%, your desktop shows through the window, blurred.">
            <div className="flex w-64 items-center gap-3">
              <Slider
                aria-label="Window opacity"
                min={MIN_WINDOW_OPACITY * 100}
                max={100}
                step={5}
                value={[opacity]}
                onValueChange={([v]) => appearanceSettings.set({ windowOpacity: v / 100 })}
              />
              <span className="w-11 text-right text-sm tabular text-muted-foreground">{opacity}%</span>
            </div>
          </SettingRow>
        )}
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

function AppThemePicker({ value }: { value: string }) {
  const dark = isDarkTheme()
  const pick = (t: AppTheme) => {
    appearanceSettings.set({ appTheme: t.id })
    terminalSettings.set({ theme: t.terminal[0], lightTheme: t.terminal[1] })
  }
  return (
    <div role="radiogroup" aria-label="App theme" className="grid w-full max-w-3xl grid-cols-3 gap-2 sm:grid-cols-5">
      {APP_THEMES.map((t) => {
        // The theme's own surfaces, as index.css computes them (sidebar, panel, border) in the current mode.
        const surface = (l: number, c: number) => `oklch(${dark ? l + t.l : 1 - (1 - l) * 0.06} ${c * t.cs} ${t.h})`
        const selected = (value || 'default') === t.id
        return (
          <button
            key={t.id}
            type="button"
            role="radio"
            aria-checked={selected}
            onClick={() => pick(t)}
            className={cn(
              'flex flex-col gap-1.5 rounded-lg border p-1.5 text-left text-sm outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60',
              selected && 'border-primary ring-1 ring-primary',
            )}
          >
            <span className="flex h-10 overflow-hidden rounded-md border" style={{ borderColor: surface(0.29, 0.01) }} aria-hidden>
              <span className="w-1/4" style={{ background: surface(0.165, 0.009) }} />
              <span className="flex flex-1 flex-col justify-end gap-1 p-1.5" style={{ background: surface(0.195, 0.009) }}>
                <span className="h-1 w-3/5 rounded-full bg-primary" />
                <span className="h-1 w-2/5 rounded-full" style={{ background: surface(0.36, 0.012) }} />
              </span>
            </span>
            <span className="truncate px-0.5">{t.label}</span>
          </button>
        )
      })}
    </div>
  )
}
