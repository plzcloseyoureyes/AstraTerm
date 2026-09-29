/*
 * Settings → Terminal (settings.terminal): colour schemes (gallery, custom schemes, import / export), font, cursor,
 * behaviour, mouse & clipboard, paste safety, keyboard, bell & notifications, end-of-session behaviour.
 */
import { useMemo, useRef, useState } from 'react'
import { Copy, Download, Ellipsis, FileUp, Keyboard, Pencil, Plus, RotateCcw, Search, Trash2, Volume2 } from 'lucide-react'
import { parseKeybinding } from 'tinykeys'
import { toast } from 'sonner'
import { runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { Slider } from '@/components/ui/slider'
import { Switch } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { isDarkTheme } from '@/lib/theme'
import { isMac } from '@/lib/utils'
import { clearClipboardHistory, useClipboardHistory } from '../clipboard'
import { cssFamily, fontOptions, queryInstalledMonospaceFonts } from '../fonts'
import { notificationPermission, playBell, requestNotificationPermission } from '../notify'
import { downloadText, parseSchemeFile, SchemeImportError, schemeToItermColors, schemeToJson } from '../schemeIO'
import { BUNDLED_FONT_FAMILY, LIMITS, terminalSettings, TERMINAL_DEFAULTS, type TerminalSettings } from '../settings'
import { BUILTIN_SCHEMES, CUSTOM_PREFIX, findScheme, listSchemes, newSchemeId, normalizeScheme } from '../themes'
import type { TerminalScheme } from '../types'
import { SchemeEditorDialog } from './SchemeEditorDialog'
import { SchemeCard, SchemePreview, type PreviewFont } from './SchemePreview'

type Keys<T, V> = { [K in keyof T]: T[K] extends V ? K : never }[keyof T]

function Toggle({ k, label, description, onEnable }: { k: Keys<TerminalSettings, boolean>; label: string; description?: string; onEnable?: () => void }) {
  const value = terminalSettings.useValue(k)
  const id = `term-${k}`
  return (
    <SettingRow label={label} description={description} htmlFor={id}>
      <Switch
        id={id}
        checked={value}
        onCheckedChange={(v) => {
          terminalSettings.set({ [k]: v } as Partial<TerminalSettings>)
          if (v) onEnable?.()
        }}
      />
    </SettingRow>
  )
}

function NumberRow({
  k,
  label,
  description,
  unit,
  step = 1,
  integer = true,
}: {
  k: keyof typeof LIMITS & Keys<TerminalSettings, number>
  label: string
  description?: string
  unit?: string
  step?: number
  integer?: boolean
}) {
  const value = terminalSettings.useValue(k)
  const [min, max] = LIMITS[k]
  const id = `term-${k}`
  return (
    <SettingRow label={label} description={description} htmlFor={id}>
      <NumberInput
        id={id}
        className="w-32"
        inputSize="sm"
        value={value}
        min={min}
        max={max}
        step={step}
        integer={integer}
        unit={unit}
        onChange={(v) => v !== null && terminalSettings.set({ [k]: v } as Partial<TerminalSettings>)}
      />
    </SettingRow>
  )
}

function SelectRow<K extends Keys<TerminalSettings, string>>({
  k,
  label,
  description,
  options,
  width = 'w-48',
}: {
  k: K
  label: string
  description?: string
  options: { value: TerminalSettings[K] & string; label: string }[]
  width?: string
}) {
  const value = terminalSettings.useValue(k) as TerminalSettings[K] & string
  return (
    <SettingRow label={label} description={description}>
      <SimpleSelect aria-label={label} size="sm" className={width} value={value} onValueChange={(v) => terminalSettings.set({ [k]: v } as Partial<TerminalSettings>)} options={options} />
    </SettingRow>
  )
}

const WEIGHTS: { value: TerminalSettings['fontWeight']; label: string }[] = [
  { value: '100', label: 'Thin (100)' },
  { value: '200', label: 'Extra light (200)' },
  { value: '300', label: 'Light (300)' },
  { value: 'normal', label: 'Regular (400)' },
  { value: '500', label: 'Medium (500)' },
  { value: '600', label: 'Semibold (600)' },
  { value: 'bold', label: 'Bold (700)' },
  { value: '800', label: 'Extra bold (800)' },
  { value: '900', label: 'Black (900)' },
]

function validBinding(raw: string): string {
  const b = raw.trim()
  if (!b) return ''
  try {
    const seq = parseKeybinding(b)
    return seq.length === 1 ? b : ''
  } catch {
    return ''
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Colour schemes
// ---------------------------------------------------------------------------------------------------------------------

function ColourSchemes({ s, font }: { s: TerminalSettings; font: PreviewFont }) {
  const [slot, setSlot] = useState<'dark' | 'light'>(() => (isDarkTheme() ? 'dark' : 'light'))
  const [editing, setEditing] = useState<{ scheme: TerminalScheme; replaceId?: string } | null>(null)
  const [query, setQuery] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)
  const editingLight = s.matchAppTheme && slot === 'light'
  const selectedId = editingLight ? s.lightTheme : s.theme
  const selected = findScheme(selectedId, s.customSchemes, !editingLight)
  const all = useMemo(() => listSchemes(s.customSchemes), [s.customSchemes])
  const q = query.trim().toLowerCase()
  const shown = q ? all.filter((x) => x.name.toLowerCase().includes(q)) : all

  const select = (id: string) => terminalSettings.set(editingLight ? { lightTheme: id } : { theme: id })

  const saveCustom = (scheme: TerminalScheme, replaceId?: string) => {
    const rawId = replaceId?.startsWith(CUSTOM_PREFIX) ? replaceId.slice(CUSTOM_PREFIX.length) : undefined
    const id = rawId ?? newSchemeId()
    const entry = { ...normalizeScheme(scheme), id }
    terminalSettings.set((prev) => ({
      customSchemes: rawId ? prev.customSchemes.map((c) => (c.id === rawId ? entry : c)) : [...prev.customSchemes, entry],
    }))
    select(CUSTOM_PREFIX + id)
    setEditing(null)
    toast.success(rawId ? 'Scheme updated' : 'Scheme created')
  }

  const remove = async (scheme: TerminalScheme) => {
    if (!(await confirm({ title: `Delete "${scheme.name}"?`, description: 'Terminals using it switch to the default scheme.', confirmLabel: 'Delete', destructive: true }))) return
    const raw = scheme.id.slice(CUSTOM_PREFIX.length)
    terminalSettings.set((prev) => ({
      customSchemes: prev.customSchemes.filter((c) => c.id !== raw),
      theme: prev.theme === scheme.id ? TERMINAL_DEFAULTS.theme : prev.theme,
      lightTheme: prev.lightTheme === scheme.id ? TERMINAL_DEFAULTS.lightTheme : prev.lightTheme,
    }))
  }

  const importFiles = async (files: FileList | null) => {
    if (!files?.length) return
    const added: TerminalScheme[] = []
    for (const f of Array.from(files)) {
      if (f.size > 2 * 1024 * 1024) {
        toast.error(`${f.name} is too large`)
        continue
      }
      try {
        added.push(...parseSchemeFile(await f.text(), f.name))
      } catch (err) {
        toast.error(`Could not import ${f.name}`, { description: err instanceof SchemeImportError ? err.message : 'Unreadable file.' })
      }
    }
    if (!added.length) return
    terminalSettings.set((prev) => ({ customSchemes: [...prev.customSchemes, ...added] }))
    select(CUSTOM_PREFIX + added[added.length - 1].id)
    toast.success(added.length === 1 ? `Imported "${added[0].name}"` : `Imported ${added.length} schemes`)
  }

  const actions = (scheme: TerminalScheme) => {
    const custom = scheme.id.startsWith(CUSTOM_PREFIX)
    return (
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button size="icon-xs" variant="secondary" aria-label={`Actions for ${scheme.name}`} className="shadow-xs">
            <Ellipsis />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          {custom ? (
            <DropdownMenuItem onSelect={() => setEditing({ scheme, replaceId: scheme.id })}>
              <Pencil /> Edit
            </DropdownMenuItem>
          ) : null}
          <DropdownMenuItem onSelect={() => setEditing({ scheme: { ...scheme, name: `${scheme.name} (custom)` } })}>
            <Copy /> {custom ? 'Duplicate' : 'Customize a copy'}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => downloadText(schemeToJson(scheme), `${scheme.name}.json`)}>
            <Download /> Export JSON
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => downloadText(schemeToItermColors(scheme), `${scheme.name}.itermcolors`, 'application/xml')}>
            <Download /> Export .itermcolors
          </DropdownMenuItem>
          {custom ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onSelect={() => void remove(scheme)}>
                <Trash2 /> Delete
              </DropdownMenuItem>
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
    )
  }

  return (
    <SettingsGroup title="Colour scheme" description={`${BUILTIN_SCHEMES.length} built-in schemes plus your own. Connections can override the scheme in their Terminal settings.`}>
      <SettingRow label="Match the app theme" description="Use one scheme with the dark UI theme and another with the light one." htmlFor="term-matchAppTheme">
        <Switch id="term-matchAppTheme" checked={s.matchAppTheme} onCheckedChange={(v) => terminalSettings.set({ matchAppTheme: v })} />
      </SettingRow>
      <div className="grid gap-3 px-4 py-3">
        <div className="flex flex-wrap items-center gap-2">
          {s.matchAppTheme && (
            <SegmentedControl
              size="sm"
              aria-label="Scheme being chosen"
              value={slot}
              onValueChange={setSlot}
              options={[
                { value: 'dark', label: 'Dark UI' },
                { value: 'light', label: 'Light UI' },
              ]}
            />
          )}
          <Input inputSize="sm" className="w-48" leading={<Search />} placeholder="Filter schemes" value={query} onChange={(e) => setQuery(e.target.value)} aria-label="Filter schemes" />
          <div className="ml-auto flex items-center gap-1.5">
            <Button size="sm" variant="secondary" onClick={() => setEditing({ scheme: { ...selected, name: 'My scheme' } })}>
              <Plus /> New scheme
            </Button>
            <Button size="sm" variant="secondary" onClick={() => fileRef.current?.click()}>
              <FileUp /> Import…
            </Button>
            <input
              ref={fileRef}
              type="file"
              accept=".json,.itermcolors,.xml,application/json,application/xml"
              multiple
              hidden
              onChange={(e) => {
                void importFiles(e.target.files)
                e.target.value = ''
              }}
            />
          </div>
        </div>
        <div role="radiogroup" aria-label="Colour schemes" className="grid grid-cols-2 gap-2 @md:grid-cols-3 @2xl:grid-cols-4">
          {shown.map((scheme) => (
            <SchemeCard key={scheme.id} scheme={scheme} selected={scheme.id === selected.id} onSelect={() => select(scheme.id)} actions={actions(scheme)} />
          ))}
          {!shown.length && <p className="col-span-full py-4 text-center text-sm text-muted-foreground">No scheme matches “{query}”.</p>}
        </div>
        <SchemePreview scheme={selected} font={font} opacity={s.backgroundOpacity} />
      </div>
      <SettingRow label="Minimum contrast ratio" description="Adjusts text colours that are too close to their background (1 = off, 4.5 = WCAG AA).">
        <div className="flex w-64 items-center gap-3">
          <Slider aria-label="Minimum contrast ratio" min={1} max={21} step={0.5} value={[s.minimumContrastRatio]} onValueChange={([v]) => terminalSettings.set({ minimumContrastRatio: v })} />
          <span className="w-10 text-right text-sm text-muted-foreground tabular">{s.minimumContrastRatio.toFixed(1)}</span>
        </div>
      </SettingRow>
      <Toggle k="drawBoldTextInBrightColors" label="Bold text in bright colours" description="Render bold text with the bright variant of its colour (classic terminal behaviour)." />
      <SettingRow label="Background opacity" description="Lets the application background show through. Applies to newly opened terminals when switching from fully opaque.">
        <div className="flex w-64 items-center gap-3">
          <Slider
            aria-label="Background opacity"
            min={30}
            max={100}
            step={5}
            value={[Math.round(s.backgroundOpacity * 100)]}
            onValueChange={([v]) => terminalSettings.set({ backgroundOpacity: v / 100 })}
          />
          <span className="w-10 text-right text-sm text-muted-foreground tabular">{Math.round(s.backgroundOpacity * 100)}%</span>
        </div>
      </SettingRow>
      {editing && (
        <SchemeEditorDialog scheme={editing.scheme} font={font} onClose={() => setEditing(null)} onSave={(scheme) => saveCustom(scheme, editing.replaceId)} />
      )}
    </SettingsGroup>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Font
// ---------------------------------------------------------------------------------------------------------------------

function FontSettings({ s }: { s: TerminalSettings }) {
  const [extra, setExtra] = useState<string[]>([])
  const options = useMemo(() => {
    const base = fontOptions()
    const known = new Set(base.map((o) => o.label))
    for (const name of extra) if (!known.has(name)) base.splice(base.length - 1, 0, { label: name, family: `${cssFamily(name)}, monospace` })
    return base
  }, [extra])
  const current = options.find((o) => o.family === s.fontFamily)
  const [custom, setCustom] = useState(!current)
  return (
    <SettingsGroup title="Font" description="JetBrains Mono is bundled and works offline. Installed fonts are detected automatically.">
      <SettingRow label="Font family" description={custom ? 'Any CSS font-family list, e.g. "Fira Code", monospace.' : undefined} stacked={custom}>
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {custom ? (
            <Input
              inputSize="sm"
              className="min-w-64 flex-1 font-mono"
              defaultValue={s.fontFamily}
              aria-label="Custom font family"
              onBlur={(e) => {
                const v = e.target.value.trim()
                terminalSettings.set({ fontFamily: v || BUNDLED_FONT_FAMILY })
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') (e.target as HTMLInputElement).blur()
              }}
            />
          ) : (
            <SimpleSelect
              aria-label="Font family"
              size="sm"
              className="w-60"
              value={current?.family}
              onValueChange={(family) => terminalSettings.set({ fontFamily: family })}
              options={options.map((o) => ({ value: o.family, label: o.label }))}
            />
          )}
          <Button size="sm" variant="ghost" onClick={() => setCustom((c) => !c)}>
            {custom ? 'Choose from list' : 'Custom…'}
          </Button>
          {'queryLocalFonts' in window && (
            <Button
              size="sm"
              variant="ghost"
              onClick={async () => {
                const fonts = await queryInstalledMonospaceFonts()
                if (!fonts) toast.error('Font access was denied')
                else {
                  setExtra(fonts)
                  toast.success(`${fonts.length} monospace fonts found`)
                }
              }}
            >
              <Search /> Detect installed fonts
            </Button>
          )}
        </div>
      </SettingRow>
      <NumberRow k="fontSize" label="Font size" unit="px" description="Per-tab zoom: Ctrl/⌘ + / − / 0 or Ctrl + mouse wheel." />
      <SelectRow k="fontWeight" label="Font weight" options={WEIGHTS} />
      <SelectRow k="fontWeightBold" label="Bold font weight" options={WEIGHTS} />
      <SettingRow label="Line height">
        <div className="flex w-64 items-center gap-3">
          <Slider aria-label="Line height" min={100} max={200} step={5} value={[Math.round(s.lineHeight * 100)]} onValueChange={([v]) => terminalSettings.set({ lineHeight: v / 100 })} />
          <span className="w-10 text-right text-sm text-muted-foreground tabular">{s.lineHeight.toFixed(2)}</span>
        </div>
      </SettingRow>
      <NumberRow k="letterSpacing" label="Letter spacing" unit="px" />
    </SettingsGroup>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Section
// ---------------------------------------------------------------------------------------------------------------------

export default function TerminalSettingsSection() {
  const s = terminalSettings.use()
  const historyCount = useClipboardHistory((st) => st.entries.length)
  const [perm, setPerm] = useState(notificationPermission())
  const font: PreviewFont = {
    family: s.fontFamily,
    size: s.fontSize,
    lineHeight: s.lineHeight,
    weight: s.fontWeight,
    boldWeight: s.fontWeightBold,
    letterSpacing: s.letterSpacing,
    cursorStyle: s.cursorStyle,
  }
  const askPermission = () => {
    void requestNotificationPermission().then(() => setPerm(notificationPermission()))
  }

  return (
    <SettingsPage
      title="Terminal"
      description="Appearance and behaviour of terminal tabs. Connections can override fonts, colours and cursor in their own settings."
      actions={
        <Button
          variant="ghost"
          size="sm"
          onClick={async () => {
            if (await confirm({ title: 'Reset terminal settings?', description: 'Custom colour schemes are kept.', confirmLabel: 'Reset' })) {
              terminalSettings.replace({ customSchemes: s.customSchemes })
            }
          }}
        >
          <RotateCcw /> Reset to defaults
        </Button>
      }
    >
      <ColourSchemes s={s} font={font} />
      <FontSettings s={s} />

      <SettingsGroup title="Cursor">
        <SettingRow label="Cursor style">
          <SegmentedControl
            size="sm"
            aria-label="Cursor style"
            value={s.cursorStyle}
            onValueChange={(cursorStyle) => terminalSettings.set({ cursorStyle })}
            options={[
              { value: 'block', label: 'Block' },
              { value: 'underline', label: 'Underline' },
              { value: 'bar', label: 'Bar' },
            ]}
          />
        </SettingRow>
        <Toggle k="cursorBlink" label="Blinking cursor" />
        <SelectRow
          k="cursorInactiveStyle"
          label="Cursor when unfocused"
          options={[
            { value: 'outline', label: 'Hollow outline' },
            { value: 'block', label: 'Block' },
            { value: 'bar', label: 'Bar' },
            { value: 'underline', label: 'Underline' },
            { value: 'none', label: 'Hidden' },
          ]}
        />
        {s.cursorStyle === 'bar' && <NumberRow k="cursorWidth" label="Bar width" unit="px" />}
      </SettingsGroup>

      <SettingsGroup title="Display & scrolling">
        <NumberRow k="scrollback" label="Scrollback lines" description={s.scrollback > 100_000 ? 'Large scrollbacks use a lot of memory per terminal.' : 'Lines kept above the screen in each terminal. The server keeps its own buffer for re-attaching.'} />
        <SelectRow
          k="titleMode"
          label="Tab title"
          description="Applications set the terminal title with OSC 0/2 escape sequences."
          width="w-56"
          options={[
            { value: 'osc', label: 'Terminal title (fallback: session name)' },
            { value: 'session', label: 'Session name only' },
            { value: 'both', label: 'Session name — terminal title' },
          ]}
        />
        <NumberRow k="padding" label="Inner padding" unit="px" />
        <SelectRow
          k="renderer"
          label="Renderer"
          description="WebGL is fastest; the DOM renderer is used automatically when WebGL is unavailable."
          options={[
            { value: 'auto', label: 'WebGL (automatic fallback)' },
            { value: 'dom', label: 'DOM' },
          ]}
        />
        <SelectRow
          k="unicodeVersion"
          label="Unicode width tables"
          description="Version 11 renders emoji and CJK characters with the correct width."
          options={[
            { value: '11', label: 'Unicode 11' },
            { value: '6', label: 'Unicode 6 (legacy)' },
          ]}
        />
        <Toggle k="images" label="Inline images" description="Display Sixel and iTerm2 inline images (img2sixel, chafa, yazi…)." />
        <NumberRow k="smoothScrollDuration" label="Smooth scrolling" unit="ms" description="0 disables smooth scrolling." />
        <NumberRow k="scrollSensitivity" label="Scroll speed" step={0.5} integer={false} />
        <NumberRow k="fastScrollSensitivity" label="Fast scroll speed (Alt + wheel)" integer={false} />
        <Toggle k="scrollOnUserInput" label="Scroll to bottom on input" />
        <Toggle k="screenReaderMode" label="Screen reader mode" description="Exposes terminal output to assistive technologies (slower)." />
      </SettingsGroup>

      <SettingsGroup title="Mouse & clipboard">
        <SelectRow
          k="rightClickAction"
          label="Right click"
          description="With paste modes, Shift + right click still opens the menu."
          width="w-56"
          options={[
            { value: 'menu', label: 'Open the context menu' },
            { value: 'paste', label: 'Paste' },
            { value: 'copy-or-paste', label: 'Copy selection, else paste' },
          ]}
        />
        <Toggle k="copyOnSelect" label="Copy on select" description="Selecting text copies it to the clipboard." />
        <Toggle k="middleClickPaste" label="Middle click pastes" />
        <Toggle k="rightClickSelectsWord" label="Right click selects the word" />
        <SettingRow label="Word separators" description="Characters that end a word when double-clicking to select." htmlFor="term-wordSeparator">
          <Input
            id="term-wordSeparator"
            inputSize="sm"
            className="w-56 font-mono"
            defaultValue={s.wordSeparator}
            onBlur={(e) => terminalSettings.set({ wordSeparator: e.target.value })}
          />
        </SettingRow>
        <Toggle k="trimCopiedWhitespace" label="Trim trailing spaces when copying" />
        <Toggle k="altClickMovesCursor" label="Alt + click moves the cursor" description="Moves the shell cursor to the clicked position." />
        <SelectRow
          k="linkModifier"
          label="Open links"
          width="w-56"
          options={[
            { value: 'mod', label: isMac ? 'With ⌘ + click' : 'With Ctrl + click' },
            { value: 'none', label: 'With a plain click' },
          ]}
        />
        <SelectRow
          k="osc52"
          label="Remote clipboard access (OSC 52)"
          description="Lets tmux, vim or helix on the server copy to your clipboard. Reads always ask first."
          width="w-56"
          options={[
            { value: 'off', label: 'Off' },
            { value: 'write', label: 'Allow copy (recommended)' },
            { value: 'read-write', label: 'Allow copy and ask to read' },
          ]}
        />
        <SettingRow label="Clipboard history" description={`Recent copies, in memory only (${historyCount} kept). Paste them from the context menu or command palette.`} htmlFor="term-clipboardHistory">
          <Button size="sm" variant="ghost" disabled={!historyCount} onClick={() => clearClipboardHistory()}>
            <Trash2 /> Clear
          </Button>
          <Switch id="term-clipboardHistory" checked={s.clipboardHistory} onCheckedChange={(v) => terminalSettings.set({ clipboardHistory: v })} />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Paste safety" description="Pasted text is checked for hidden characters, escape-sequence injection and destructive commands.">
        <Toggle k="pasteConfirmDangerous" label="Confirm suspicious pastes" description="Hidden characters, curl | sh, rm -rf, homographs…" />
        <Toggle k="pasteConfirmMultiline" label="Confirm multi-line pastes" />
        <Toggle k="pasteSkipConfirmBracketed" label="…unless the application uses bracketed paste" description="Shells with bracketed paste do not run pasted lines until you press Enter." />
        <Toggle k="bracketedPaste" label="Bracketed paste" description="Honour the application's bracketed paste mode (recommended)." />
        <NumberRow k="pasteLineDelayMs" label="Line-by-line paste delay" unit="ms" description="Delay between lines for “Paste line by line” (slow devices, network gear)." />
      </SettingsGroup>

      <SettingsGroup title="Keyboard">
        {isMac && <Toggle k="macOptionIsMeta" label="Use Option as Meta" description="Option + key sends ESC + key instead of a special character." />}
        <Toggle k="altArrowWordJump" label="Alt + ←/→ jumps words" description="Sends ESC b / ESC f like macOS Terminal." />
        <Toggle k="ctrlZoom" label="Ctrl / ⌘ + = − 0 zoom the terminal" description="Also Ctrl + mouse wheel. When off, these keys reach the browser / shell." />
        <Toggle k="altKeysToTerminal" label="Send Alt + key to the shell" description="App shortcuts using Alt are ignored while a terminal is focused (for Emacs / readline Meta keys)." />
        <SettingRow label="Keys always sent to the shell" description="Shortcuts that should reach the terminal instead of Termstead, e.g. Alt+w or Control+Shift+x." stacked>
          <TagInput
            aria-label="Keys always sent to the shell"
            className="w-full"
            value={s.passthroughKeys}
            placeholder="Alt+w"
            normalize={validBinding}
            onChange={(passthroughKeys) => terminalSettings.set({ passthroughKeys })}
          />
        </SettingRow>
        <SettingRow label="Terminal shortcuts" description="Copy, paste, find, zoom and more can be rebound.">
          <Button size="sm" variant="secondary" onClick={() => void runCommand('settings.open', { section: 'keyboard' })}>
            <Keyboard /> Keyboard shortcuts…
          </Button>
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup
        title="Bell & notifications"
        description={perm === 'denied' ? 'Desktop notifications are blocked by the browser; Termstead falls back to in-app notifications.' : undefined}
      >
        <SettingRow label="Bell">
          <div className="flex items-center gap-2">
            <SegmentedControl
              size="sm"
              aria-label="Bell"
              value={s.bell}
              onValueChange={(bell) => terminalSettings.set({ bell })}
              options={[
                { value: 'none', label: 'Off' },
                { value: 'visual', label: 'Flash' },
                { value: 'sound', label: 'Sound' },
                { value: 'both', label: 'Both' },
              ]}
            />
            <Button size="icon-sm" variant="ghost" aria-label="Test the bell sound" title="Test sound" onClick={() => playBell()}>
              <Volume2 />
            </Button>
          </div>
        </SettingRow>
        <Toggle k="bellNotify" label="Notify on bell in background tabs" onEnable={askPermission} />
        <Toggle k="notifyLongCommands" label="Notify when a long command finishes" description="Needs shell integration (OSC 133) in the remote shell." onEnable={askPermission} />
        {s.notifyLongCommands && <NumberRow k="longCommandSeconds" label="Long command threshold" unit="s" />}
        <NumberRow k="silenceSeconds" label="Silence threshold" unit="s" description="Used by “Notify after silence” (terminal context menu → Monitor)." />
        <Toggle k="osc9Notifications" label="Application notifications (OSC 9 / 777)" onEnable={askPermission} />
        {perm === 'default' && (
          <SettingRow label="Desktop notifications" description="Allow Termstead to show system notifications.">
            <Button size="sm" variant="secondary" onClick={askPermission}>
              Allow notifications
            </Button>
          </SettingRow>
        )}
      </SettingsGroup>

      <SettingsGroup title="Conveniences">
        <Toggle k="showToolbar" label="Terminal toolbar" description="Find, split, broadcast, pause and logging buttons shown while hovering a terminal." />
        <Toggle k="hidePointerWhileTyping" label="Hide the mouse pointer while typing" />
        <Toggle k="focusFollowsMouse" label="Focus follows the mouse" description="Moving the mouse into a terminal pane gives it the keyboard." />
      </SettingsGroup>

      <SettingsGroup title="When a session ends">
        <Toggle k="showEndPrompt" label="Show the end-of-session prompt" description="R = reconnect, D = duplicate, S = save output, Q / Enter = close." />
        <SelectRow
          k="closeOnExit"
          label="Close the tab automatically"
          options={[
            { value: 'never', label: 'Never' },
            { value: 'clean', label: 'When it exits cleanly (code 0)' },
            { value: 'always', label: 'Whenever it exits' },
          ]}
          width="w-56"
        />
      </SettingsGroup>
    </SettingsPage>
  )
}
