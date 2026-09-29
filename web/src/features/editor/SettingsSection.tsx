/* Settings → Editor (settings section "editor"). */
import { RotateCcw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { clearRecent } from './recent'
import { editorSettings, FONT_MAX, FONT_MIN } from './settings'
import type { EditorSettings } from './types'

export default function EditorSettingsSection() {
  const s = editorSettings.use()
  const set = (patch: Partial<EditorSettings>) => editorSettings.set(patch)
  return (
    <SettingsPage
      title="Editor"
      description="The built-in text editor (remote files, scratch documents), the hex editor and text diff. Press F1 in the editor for all of its commands."
      actions={
        <Button variant="secondary" size="sm" onClick={() => editorSettings.reset()}>
          <RotateCcw /> Reset to defaults
        </Button>
      }
    >
      <SettingsGroup title="Text">
        <SettingRow label="Font size" description="Per-tab zoom (Ctrl/⌘ + / −) is added on top." htmlFor="ed-font-size">
          <NumberInput id="ed-font-size" className="w-28" inputSize="sm" value={s.fontSize} min={FONT_MIN} max={FONT_MAX} unit="px" onChange={(v) => v !== null && set({ fontSize: v })} />
        </SettingRow>
        <SettingRow label="Font family" description="Any CSS font-family list. Empty uses the app's monospace font (JetBrains Mono)." htmlFor="ed-font-family">
          <Input
            // remount when the value changes elsewhere (reset, another tab) so the field shows it
            key={s.fontFamily}
            id="ed-font-family"
            inputSize="sm"
            className="w-64 font-mono"
            placeholder="JetBrains Mono"
            defaultValue={s.fontFamily}
            onBlur={(e) => set({ fontFamily: e.target.value.trim() })}
            onKeyDown={(e) => {
              if (e.key === 'Enter') e.currentTarget.blur()
            }}
          />
        </SettingRow>
        <SettingRow label="Word wrap" description="Wrap long lines by default (toggle per tab with Alt+Z)." htmlFor="ed-wrap">
          <Switch id="ed-wrap" checked={s.wordWrap} onCheckedChange={(v) => set({ wordWrap: v })} />
        </SettingRow>
        <SettingRow label="Show whitespace" description="Render spaces and tabs, and highlight trailing whitespace." htmlFor="ed-ws">
          <Switch id="ed-ws" checked={s.showWhitespace} onCheckedChange={(v) => set({ showWhitespace: v })} />
        </SettingRow>
        <SettingRow label="Indent guides" description="Thin vertical lines at every indentation level." htmlFor="ed-guides">
          <Switch id="ed-guides" checked={s.indentGuides} onCheckedChange={(v) => set({ indentGuides: v })} />
        </SettingRow>
        <SettingRow label="Bracket pair colours" description="Colour matching brackets by nesting level and highlight the active pair's guide." htmlFor="ed-brackets-color">
          <Switch id="ed-brackets-color" checked={s.bracketPairs} onCheckedChange={(v) => set({ bracketPairs: v })} />
        </SettingRow>
        <SettingRow label="Minimap" description="A code overview at the right edge (toggle per tab from the toolbar). Off for very large files." htmlFor="ed-minimap">
          <Switch id="ed-minimap" checked={s.minimap} onCheckedChange={(v) => set({ minimap: v })} />
        </SettingRow>
        <SettingRow label="Sticky scroll" description="Keep the lines of the enclosing blocks (functions, sections) at the top while scrolling." htmlFor="ed-sticky">
          <Switch id="ed-sticky" checked={s.stickyScroll} onCheckedChange={(v) => set({ stickyScroll: v })} />
        </SettingRow>
        <SettingRow label="Colour swatches" description="Preview CSS colours (#rrggbb, rgb(), hsl()) in style sheets, markup and config files; click one to pick another colour." htmlFor="ed-swatches">
          <Switch id="ed-swatches" checked={s.colorSwatches} onCheckedChange={(v) => set({ colorSwatches: v })} />
        </SettingRow>
        <SettingRow label="Line numbers" htmlFor="ed-ln">
          <Switch id="ed-ln" checked={s.lineNumbers} onCheckedChange={(v) => set({ lineNumbers: v })} />
        </SettingRow>
        <SettingRow label="Highlight the current line" htmlFor="ed-al">
          <Switch id="ed-al" checked={s.highlightActiveLine} onCheckedChange={(v) => set({ highlightActiveLine: v })} />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Editing">
        <SettingRow label="Indent with" htmlFor="ed-indent">
          <SegmentedControl
            aria-label="Indent with"
            size="sm"
            value={s.insertSpaces ? 'spaces' : 'tabs'}
            onValueChange={(v) => set({ insertSpaces: v === 'spaces' })}
            options={[
              { value: 'spaces', label: 'Spaces' },
              { value: 'tabs', label: 'Tabs' },
            ]}
          />
        </SettingRow>
        <SettingRow label="Tab size" description="Indent width in spaces, and how wide a tab character is displayed." htmlFor="ed-tab">
          <NumberInput id="ed-tab" className="w-24" inputSize="sm" value={s.tabSize} min={1} max={16} onChange={(v) => v !== null && set({ tabSize: v })} />
        </SettingRow>
        <SettingRow label="Detect indentation" description="Use the indentation style found in each file (tabs vs spaces and width)." htmlFor="ed-detect">
          <Switch id="ed-detect" checked={s.detectIndentation} onCheckedChange={(v) => set({ detectIndentation: v })} />
        </SettingRow>
        <SettingRow label="Auto-close brackets and quotes" htmlFor="ed-brackets">
          <Switch id="ed-brackets" checked={s.closeBrackets} onCheckedChange={(v) => set({ closeBrackets: v })} />
        </SettingRow>
        <SettingRow label="Autocomplete" description="Suggestions while typing: IntelliSense for JavaScript, TypeScript, JSON, CSS and HTML, words from the document for everything else." htmlFor="ed-ac">
          <Switch id="ed-ac" checked={s.autocomplete} onCheckedChange={(v) => set({ autocomplete: v })} />
        </SettingRow>
        <SettingRow
          label="Type-check JavaScript / TypeScript"
          description="Report type errors and unknown names, not only syntax errors. A single remote file has no project around it, so this can be noisy; missing imports are never reported."
          htmlFor="ed-semantic"
        >
          <Switch id="ed-semantic" checked={s.semanticValidation} onCheckedChange={(v) => set({ semanticValidation: v })} />
        </SettingRow>
        <SettingRow label="Vim key bindings"description="Modal editing (:w saves, :q closes the tab). Ctrl-key combinations go to Vim on Windows and Linux." htmlFor="ed-vim">
          <Switch id="ed-vim" checked={s.vimMode} onCheckedChange={(v) => set({ vimMode: v })} />
        </SettingRow>
        <SettingRow label="Line endings for new documents">
          <SegmentedControl
            aria-label="Line endings for new documents"
            size="sm"
            value={s.defaultEol}
            onValueChange={(v) => set({ defaultEol: v })}
            options={[
              { value: 'lf', label: 'LF (Unix)' },
              { value: 'crlf', label: 'CRLF (Windows)' },
            ]}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Remote files" description="Files opened from SFTP, FTP, S3 or the server's disk are uploaded when you save.">
        <SettingRow label="Automatic save" description="Save (upload) automatically after you stop typing, or when the editor loses focus." htmlFor="ed-autosave">
          <SimpleSelect
            id="ed-autosave"
            aria-label="Automatic save"
            size="sm"
            className="w-48"
            value={s.autosave}
            onValueChange={(autosave) => set({ autosave })}
            options={[
              { value: 'off', label: 'Off' },
              { value: 'afterDelay', label: 'After a delay' },
              { value: 'onFocusLost', label: 'When focus is lost' },
            ]}
          />
        </SettingRow>
        {s.autosave === 'afterDelay' && (
          <SettingRow label="Autosave delay" htmlFor="ed-autosave-delay">
            <NumberInput
              id="ed-autosave-delay"
              className="w-32"
              inputSize="sm"
              value={s.autosaveDelay}
              min={300}
              max={60_000}
              step={100}
              unit="ms"
              onChange={(v) => v !== null && set({ autosaveDelay: v })}
            />
          </SettingRow>
        )}
        <SettingRow label="Watch for changes on the server" description="Check the file every 5 seconds while its tab is focused and offer to reload or compare." htmlFor="ed-watch">
          <Switch id="ed-watch" checked={s.watchRemote} onCheckedChange={(v) => set({ watchRemote: v })} />
        </SettingRow>
        <SettingRow label="Large file warning" description="Ask before opening files larger than this." htmlFor="ed-large">
          <NumberInput id="ed-large" className="w-28" inputSize="sm" value={s.largeFileMiB} min={1} max={64} unit="MiB" onChange={(v) => v !== null && set({ largeFileMiB: v })} />
        </SettingRow>
        <SettingRow label="Recent files" description="The list under Tools → Recent files (stored in this browser).">
          <Button variant="secondary" size="sm" onClick={() => clearRecent()}>
            Clear
          </Button>
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
