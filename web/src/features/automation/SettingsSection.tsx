/*
 * Settings → Highlighting & triggers (section "automation"): keyword highlighting rule sets and custom rules,
 * per-connection overrides, trigger notifications, password prompts, the dangerous-command guard, compose, macros,
 * the button bar and (admins) scripts for everyone.
 */
import * as React from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useConnections } from '@/api/connections'
import { useAdminSettings, useUpdateAdminSettings } from '@/api/settings'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { LoadingState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { cn, errorMessage, isPlainObject, uid } from '@/lib/utils'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { cssColor, ColorSelect } from './components/pickers'
import { compileUserPattern, RULE_SETS, type RuleDef } from './highlight/rules'
import { automationSettings, DEFAULT_PRODUCTION_TAGS, type ComposeSendKey, type TypedGuardMode } from './settings'
import { playSound } from './sound'
import { openButtonEditor } from './store'
import { openAutomationTab } from './tab/open'
import type { HighlightRule } from './types'

/** Render a sample line with a set's rules (preview). */
function Preview({ text, rules }: { text: string; rules: RuleDef[] }) {
  const spans: { from: number; to: number; rule: RuleDef }[] = []
  const taken = new Uint8Array(text.length)
  for (const r of rules) {
    const re = new RegExp(r.re.source, r.re.flags.includes('g') ? r.re.flags : r.re.flags + 'g')
    let m: RegExpExecArray | null
    while ((m = re.exec(text))) {
      if (!m[0].length) {
        re.lastIndex++
        continue
      }
      const from = m.index
      const to = from + m[0].length
      let free = true
      for (let i = from; i < to; i++) if (taken[i]) free = false
      if (!free) continue
      taken.fill(1, from, to)
      spans.push({ from, to, rule: r })
    }
  }
  spans.sort((a, b) => a.from - b.from)
  const out: React.ReactNode[] = []
  let pos = 0
  for (const s of spans) {
    if (s.from > pos) out.push(text.slice(pos, s.from))
    out.push(
      <span key={s.from} style={{ color: cssColor(s.rule.color), background: cssColor(s.rule.background), textDecoration: s.rule.underline ? 'underline' : undefined }}>
        {text.slice(s.from, s.to)}
      </span>,
    )
    pos = s.to
  }
  if (pos < text.length) out.push(text.slice(pos))
  return <code className="block truncate rounded bg-[#1e1e1e] px-2 py-0.5 font-mono text-xs text-[#d4d4d4]">{out}</code>
}

function CustomRules() {
  const rules = automationSettings.useValue('highlightCustom')
  const set = (next: HighlightRule[]) => automationSettings.set({ highlightCustom: next })
  const update = (id: string, patch: Partial<HighlightRule>) => set(rules.map((r) => (r.id === id ? { ...r, ...patch } : r)))
  return (
    <div className="flex w-full flex-col gap-2">
      {rules.map((r) => {
        const valid = !r.pattern || !!compileUserPattern(r.pattern, r.caseSensitive)
        return (
          <div key={r.id} className="flex flex-wrap items-center gap-2 rounded-md border p-2">
            <Checkbox checked={r.enabled} onCheckedChange={(v) => update(r.id, { enabled: v === true })} aria-label={`Enable ${r.name || 'rule'}`} />
            <Input inputSize="sm" className="w-32" value={r.name} onChange={(e) => update(r.id, { name: e.target.value })} placeholder="Name" aria-label="Rule name" />
            <Input
              inputSize="sm"
              className={cn('min-w-40 flex-1 font-mono', !valid && 'border-destructive')}
              value={r.pattern}
              onChange={(e) => update(r.id, { pattern: e.target.value })}
              placeholder="regular expression"
              aria-label="Pattern"
              aria-invalid={!valid || undefined}
              spellCheck={false}
            />
            <label className="flex items-center gap-1 text-xs">
              <Checkbox checked={!!r.caseSensitive} onCheckedChange={(v) => update(r.id, { caseSensitive: v === true })} /> Aa
            </label>
            <ColorSelect value={r.color} onChange={(color) => update(r.id, { color })} allowNone noneLabel="Text: default" aria-label="Text colour" />
            <ColorSelect value={r.background} onChange={(background) => update(r.id, { background })} allowNone noneLabel="No background" aria-label="Background colour" />
            <label className="flex items-center gap-1 text-xs">
              <Checkbox checked={!!r.underline} onCheckedChange={(v) => update(r.id, { underline: v === true })} /> Underline
            </label>
            <IconButton icon={Trash2} label="Delete rule" size="xs" onClick={() => set(rules.filter((x) => x.id !== r.id))} />
          </div>
        )
      })}
      <Button
        size="sm"
        variant="secondary"
        className="self-start"
        onClick={() => set([...rules, { id: uid('hl'), name: '', pattern: '', color: 'brightMagenta', enabled: true }])}
      >
        <Plus /> Add rule
      </Button>
    </div>
  )
}

function ConnectionOverrides() {
  const overrides = automationSettings.useValue('highlightConnections')
  const { data: conns } = useConnections()
  const [adding, setAdding] = React.useState<string>()
  const entries = Object.entries(overrides)
  const name = (id: string) => conns?.find((c) => c.id === id)?.name ?? 'Deleted connection'
  const set = (next: Record<string, boolean>) => automationSettings.set({ highlightConnections: next })
  const candidates = (conns ?? []).filter((c) => !(c.id in overrides) && !['sftp', 'ftp', 's3', 'vnc', 'rdp', 'web'].includes(c.protocol))
  return (
    <div className="flex w-full flex-col gap-1.5">
      {entries.map(([id, on]) => (
        <div key={id} className="flex items-center gap-2">
          <span className="min-w-0 flex-1 truncate text-sm">{name(id)}</span>
          <SimpleSelect
            size="sm"
            className="w-36"
            value={on ? 'on' : 'off'}
            onValueChange={(v) => set({ ...overrides, [id]: v === 'on' })}
            options={[
              { value: 'on', label: 'Highlight' },
              { value: 'off', label: "Don't highlight" },
            ]}
            aria-label={`Highlighting for ${name(id)}`}
          />
          <IconButton
            icon={Trash2}
            label="Remove override"
            size="xs"
            onClick={() => {
              const next = { ...overrides }
              delete next[id]
              set(next)
            }}
          />
        </div>
      ))}
      <div className="flex items-center gap-2">
        <SimpleSelect size="sm" className="flex-1" value={adding} onValueChange={setAdding} placeholder="Choose a connection…" options={candidates.map((c) => ({ value: c.id, label: c.name }))} aria-label="Connection" />
        <Button
          size="sm"
          variant="secondary"
          disabled={!adding}
          onClick={() => {
            if (!adding) return
            set({ ...overrides, [adding]: !automationSettings.get().highlightEnabled })
            setAdding(undefined)
          }}
        >
          <Plus /> Add
        </Button>
      </div>
    </div>
  )
}

function GuardCustomRules() {
  const rules = automationSettings.useValue('guardCustom')
  const set = (next: typeof rules) => automationSettings.set({ guardCustom: next })
  return (
    <div className="flex w-full flex-col gap-1.5">
      {rules.map((r, i) => {
        const valid = !r.pattern || !!compileUserPattern(r.pattern)
        return (
          <div key={i} className="flex items-center gap-2">
            <Input
              inputSize="sm"
              className={cn('w-64 font-mono', !valid && 'border-destructive')}
              value={r.pattern}
              onChange={(e) => set(rules.map((x, j) => (j === i ? { ...x, pattern: e.target.value } : x)))}
              placeholder="deploy\s+prod"
              aria-label="Pattern"
              spellCheck={false}
            />
            <Input inputSize="sm" className="flex-1" value={r.message} onChange={(e) => set(rules.map((x, j) => (j === i ? { ...x, message: e.target.value } : x)))} placeholder="Why it is dangerous" aria-label="Message" />
            <IconButton icon={Trash2} label="Delete rule" size="xs" onClick={() => set(rules.filter((_, j) => j !== i))} />
          </div>
        )
      })}
      <Button size="sm" variant="secondary" className="self-start" onClick={() => set([...rules, { pattern: '', message: '' }])}>
        <Plus /> Add rule
      </Button>
    </div>
  )
}

function AdminScripts() {
  const { data, isPending } = useAdminSettings()
  const update = useUpdateAdminSettings()
  const section = isPlainObject(data?.automation) ? (data.automation as Record<string, unknown>) : {}
  const on = section.userScripts === true
  return (
    <SettingRow
      label="Allow scripts for every user"
      description="Scripts run inside the NexTerm server process. In server mode only administrators may run them unless this is on."
      htmlFor="auto-user-scripts"
    >
      {/* The switch mounts only once the stored value is known (in its final state, so it never flips on load); until
          then a box of the same size holds its place. */}
      <LoadingState busy={data === undefined && isPending} skeleton={<Skeleton className="h-[1.15rem] w-8 rounded-full" />} className="h-[1.15rem] w-8">
        <Switch
          id="auto-user-scripts"
          checked={on}
          disabled={update.isPending}
          onCheckedChange={(v) =>
            update.mutate({ automation: { userScripts: v } }, { onError: (err) => toast.error('Could not save', { description: errorMessage(err) }) })
          }
        />
      </LoadingState>
    </SettingRow>
  )
}

export default function AutomationSettingsSection() {
  const s = automationSettings.use()
  const isAdmin = useIsAdmin()
  const mode = useRunMode()
  const [perm, setPerm] = React.useState(typeof Notification !== 'undefined' ? Notification.permission : 'denied')
  const toggleSet = (id: string, on: boolean) => automationSettings.set({ highlightSets: on ? [...s.highlightSets.filter((x) => x !== id), id] : s.highlightSets.filter((x) => x !== id) })
  return (
    <SettingsPage title="Highlighting & triggers" description="Keyword highlighting, triggers, password prompts, the dangerous-command guard, compose, macros and the button bar.">
      <SettingsGroup title="Keyword highlighting" description="Colours keywords in the visible part of terminals without changing the output (copy, logs and recordings stay exact).">
        <SettingRow label="Highlight keywords" description="Default for every terminal; override per connection below or per tab (Terminal → Toggle keyword highlighting)." htmlFor="hl-on">
          <Switch id="hl-on" checked={s.highlightEnabled} onCheckedChange={(v) => automationSettings.set({ highlightEnabled: v })} />
        </SettingRow>
        <SettingRow label="Rule sets" stacked>
          <ul className="grid w-full gap-2 @2xl:grid-cols-2">
            {RULE_SETS.map((set) => {
              const id = `hl-set-${set.id}`
              return (
                <li key={set.id} className="flex flex-col gap-1 rounded-md border p-2">
                  <label htmlFor={id} className="flex items-center gap-2 text-sm font-medium">
                    <Checkbox id={id} checked={s.highlightSets.includes(set.id)} onCheckedChange={(v) => toggleSet(set.id, v === true)} />
                    {set.name}
                    <span className="truncate text-xs font-normal text-muted-foreground">{set.description}</span>
                  </label>
                  <Preview text={set.sample} rules={set.rules} />
                </li>
              )
            })}
          </ul>
        </SettingRow>
        <SettingRow label="Your rules" description="Regular expressions (JavaScript syntax) with colours; they win over the built-in sets." stacked>
          <CustomRules />
        </SettingRow>
        <SettingRow label="Apply the “highlight” actions of triggers" htmlFor="hl-triggers">
          <Switch id="hl-triggers" checked={s.highlightTriggers} onCheckedChange={(v) => automationSettings.set({ highlightTriggers: v })} />
        </SettingRow>
        <SettingRow label="Per connection" description="Turn highlighting on or off for particular saved sessions." stacked>
          <ConnectionOverrides />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Triggers" description="Regular expressions over the output that notify you, answer prompts, log lines or run snippets and scripts — evaluated by the server, also with no browser open.">
        <SettingRow label="Manage triggers">
          <Button size="sm" variant="secondary" onClick={() => openAutomationTab({ page: 'triggers' })}>
            Open triggers
          </Button>
        </SettingRow>
        <SettingRow label="Show notifications of triggers as toasts" htmlFor="tr-toasts">
          <Switch id="tr-toasts" checked={s.triggerToasts} onCheckedChange={(v) => automationSettings.set({ triggerToasts: v })} />
        </SettingRow>
        <SettingRow label="Play trigger sounds" htmlFor="tr-sounds">
          <Button size="xs" variant="ghost" onClick={() => playSound('chime')}>
            Test
          </Button>
          <Switch id="tr-sounds" checked={s.triggerSounds} onCheckedChange={(v) => automationSettings.set({ triggerSounds: v })} />
        </SettingRow>
        <SettingRow
          label="Desktop notifications"
          description={perm === 'denied' ? 'Blocked by the browser for this site.' : 'For triggers that ask for them, while NexTerm is in the background.'}
          htmlFor="tr-desktop"
        >
          {perm === 'default' && (
            <Button size="xs" variant="secondary" onClick={() => void Notification.requestPermission().then(setPerm)}>
              Allow
            </Button>
          )}
          <Switch id="tr-desktop" checked={s.triggerDesktop} onCheckedChange={(v) => automationSettings.set({ triggerDesktop: v })} />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Password prompts">
        <SettingRow
          label="Offer to send stored passwords"
          description="When a terminal stops at “Password:”, “[sudo] password for …” or a key passphrase prompt and the connection has that secret stored, a small button types it (it never reaches the browser)."
          htmlFor="pw-chip"
        >
          <Switch id="pw-chip" checked={s.passwordChip} onCheckedChange={(v) => automationSettings.set({ passwordChip: v })} />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Dangerous-command guard" description="Asks before rm -rf /, mkfs, dd to a device, shutdown, DROP DATABASE, reload… is sent to sessions. Enforced for snippets, macros, compose, batch runs and schedules; best effort, not a sandbox.">
        <SettingRow label="Check commands before they are sent" htmlFor="gd-on">
          <Switch id="gd-on" checked={s.guardEnabled} onCheckedChange={(v) => automationSettings.set({ guardEnabled: v })} />
        </SettingRow>
        <SettingRow label="Strict" description="Also ask for rm -rf on any path, DROP TABLE, firewall flushes, kubectl delete…" htmlFor="gd-strict">
          <Switch id="gd-strict" checked={s.guardStrict} disabled={!s.guardEnabled} onCheckedChange={(v) => automationSettings.set({ guardStrict: v })} />
        </SettingRow>
        <SettingRow label="Commands typed into one terminal" description="Input broadcast to several terminals is always checked. Also check Enter in a single terminal:">
          <SimpleSelect<TypedGuardMode>
            size="sm"
            className="w-60"
            disabled={!s.guardEnabled}
            value={s.guardTyped}
            onValueChange={(guardTyped) => automationSettings.set({ guardTyped })}
            options={[
              { value: 'production', label: 'On production-tagged hosts' },
              { value: 'always', label: 'Always' },
              { value: 'off', label: 'Never' },
            ]}
            aria-label="Check typed commands"
          />
        </SettingRow>
        <SettingRow label="Production tags" description="Connection tags that mark production hosts." stacked>
          <div className="flex w-full items-center gap-2">
            <TagInput value={s.productionTags} onChange={(productionTags) => automationSettings.set({ productionTags })} className="flex-1" />
            <Button size="xs" variant="ghost" onClick={() => automationSettings.set({ productionTags: DEFAULT_PRODUCTION_TAGS })}>
              Reset
            </Button>
          </div>
        </SettingRow>
        <SettingRow label="Your rules" description="Extra patterns that always ask (checked by the browser and the server)." stacked>
          <GuardCustomRules />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Compose & macros">
        <SettingRow label="Send key in the compose window">
          <SimpleSelect<ComposeSendKey>
            size="sm"
            className="w-60"
            value={s.composeSendKey}
            onValueChange={(composeSendKey) => automationSettings.set({ composeSendKey })}
            options={[
              { value: 'mod-enter', label: 'Ctrl/⌘+Enter (Enter = new line)' },
              { value: 'enter', label: 'Enter (Shift+Enter = new line)' },
            ]}
            aria-label="Send key"
          />
        </SettingRow>
        <SettingRow label="Default delay between lines" htmlFor="cmp-delay">
          <span className="w-28">
            <NumberInput id="cmp-delay" inputSize="sm" value={s.composeLineDelayMs} min={0} max={60000} step={50} unit="ms" onChange={(v) => automationSettings.set({ composeLineDelayMs: v ?? 0 })} />
          </span>
        </SettingRow>
        <SettingRow label="Wait for the prompt between lines by default" htmlFor="cmp-wait">
          <Switch id="cmp-wait" checked={s.composeWaitPrompt} onCheckedChange={(v) => automationSettings.set({ composeWaitPrompt: v })} />
        </SettingRow>
        <SettingRow label="Clear the compose window after sending" htmlFor="cmp-clear">
          <Switch id="cmp-clear" checked={s.composeClearAfterSend} onCheckedChange={(v) => automationSettings.set({ composeClearAfterSend: v })} />
        </SettingRow>
        <SettingRow label="Macro replay speed">
          <SimpleSelect
            size="sm"
            className="w-44"
            value={String(s.macroSpeed)}
            onValueChange={(v) => automationSettings.set({ macroSpeed: Number(v) })}
            options={[
              { value: '0.5', label: '0.5× (slower)' },
              { value: '1', label: '1× (as recorded)' },
              { value: '2', label: '2×' },
              { value: '5', label: '5×' },
              { value: '0', label: 'No delays' },
            ]}
            aria-label="Macro speed"
          />
        </SettingRow>
        <SettingRow label="Merge keystrokes typed within" description="While recording: one step per word instead of per key (0 = every key)." htmlFor="mac-merge">
          <span className="w-28">
            <NumberInput id="mac-merge" inputSize="sm" value={s.macroMergeMs} min={0} max={5000} step={50} unit="ms" onChange={(v) => automationSettings.set({ macroMergeMs: v ?? 0 })} />
          </span>
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Button bar">
        <SettingRow label="Show the button bar in the status bar" description={`${s.buttonBars.reduce((n, b) => n + b.buttons.length, 0)} buttons in ${s.buttonBars.length} bars.`} htmlFor="bb-on">
          <Button size="sm" variant="secondary" onClick={() => openButtonEditor()}>
            Edit buttons…
          </Button>
          <Switch id="bb-on" checked={s.buttonBarVisible} onCheckedChange={(v) => automationSettings.set({ buttonBarVisible: v })} />
        </SettingRow>
      </SettingsGroup>

      {isAdmin && (
        <SettingsGroup title="Administration" description={mode === 'server' ? 'Server mode.' : 'Desktop mode: every user may run scripts.'}>
          <AdminScripts />
        </SettingsGroup>
      )}
    </SettingsPage>
  )
}
