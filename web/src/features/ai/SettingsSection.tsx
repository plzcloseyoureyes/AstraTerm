/*
 * Settings → AI assistant: provider (Anthropic, OpenAI, Ollama, LM Studio, Gemini, any OpenAI-compatible server),
 * model, write-only API key, connection test, admin policy (server mode: enable, allowed models, personal providers,
 * rate limits), extra redaction patterns, and personal assistant preferences.
 */
import { useEffect, useMemo, useState } from 'react'
import {
  Bot,
  CheckCircle2,
  ChevronRight,
  Cpu,
  Gem,
  KeyRound,
  Plug,
  Server,
  ShieldCheck,
  Sparkles,
  Trash2,
  XCircle,
} from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { isApiError } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { QueryState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { cn, errorMessage } from '@/lib/utils'
import { aiKeys, describeAiError, putConfig, testConfig, useAiConfig, useAiModels } from './api'
import { aiSettings, type AiUiSettings } from './settings'
import { refreshAiStatus, useAiStatusStore } from './store'
import type { AiConfig, AiConfigView, AiModel } from './types'

interface Preset {
  id: string
  label: string
  blurb: string
  icon: typeof Bot
  provider: 'anthropic' | 'openai'
  baseUrl: string
  model: string
  keyRequired: boolean
  modelHint: string
}

const PRESETS: Preset[] = [
  { id: 'anthropic', label: 'Anthropic Claude', blurb: 'Best results — Sonnet, Opus, Haiku', icon: Sparkles, provider: 'anthropic', baseUrl: '', model: 'claude-sonnet-5', keyRequired: true, modelHint: 'claude-sonnet-5' },
  { id: 'openai', label: 'OpenAI', blurb: 'GPT models with your API key', icon: Bot, provider: 'openai', baseUrl: 'https://api.openai.com/v1', model: '', keyRequired: true, modelHint: 'a model id from your OpenAI account' },
  { id: 'ollama', label: 'Ollama', blurb: 'Local models, nothing leaves your network', icon: Cpu, provider: 'openai', baseUrl: 'http://localhost:11434/v1', model: '', keyRequired: false, modelHint: 'e.g. llama3.2, qwen2.5-coder' },
  { id: 'lmstudio', label: 'LM Studio', blurb: 'Local models via LM Studio’s server', icon: Server, provider: 'openai', baseUrl: 'http://localhost:1234/v1', model: '', keyRequired: false, modelHint: 'the model loaded in LM Studio' },
  { id: 'gemini', label: 'Google Gemini', blurb: 'Through Gemini’s OpenAI-compatible API', icon: Gem, provider: 'openai', baseUrl: 'https://generativelanguage.googleapis.com/v1beta/openai', model: '', keyRequired: true, modelHint: 'a Gemini model id' },
  { id: 'custom', label: 'Other server', blurb: 'vLLM, llama.cpp, LiteLLM… (OpenAI API)', icon: Plug, provider: 'openai', baseUrl: '', model: '', keyRequired: false, modelHint: 'model id' },
]

const ANTHROPIC_MODELS: AiModel[] = [
  { id: 'claude-sonnet-5', label: 'Claude Sonnet 5', hint: 'Fast and capable — recommended' },
  { id: 'claude-opus-5-5', label: 'Claude Opus 5.5', hint: 'Deepest reasoning' },
  { id: 'claude-fable-5-1', label: 'Claude Fable 5.1', hint: 'Most capable, slowest' },
  { id: 'claude-haiku-4-5-20251001', label: 'Claude Haiku 4.5', hint: 'Fastest, lowest cost' },
]

function presetOf(c: AiConfig): Preset | undefined {
  if (!c.provider) return undefined
  return PRESETS.find((p) => p.id === c.preset) ?? (c.provider === 'anthropic' ? PRESETS[0] : PRESETS[PRESETS.length - 1])
}

type Draft = AiConfig

function toDraft(v: AiConfigView | undefined): Draft {
  if (!v) return {}
  const { scope: _s, hasKey: _h, allowed: _a, ...rest } = v
  return rest
}

function StatusLine() {
  const status = useAiStatusStore((s) => s.status)
  if (!status) return null
  const ok = status.available && !status.locked
  const text = ok
    ? `Ready · ${status.model}${status.source === 'user' ? ' (personal)' : ''}`
    : status.reason === 'disabled'
      ? 'Turned off'
      : status.reason === 'locked'
        ? 'Vault locked — unlock to use the assistant'
        : 'Not set up'
  return (
    <Badge variant={ok ? 'success' : status.reason === 'locked' ? 'warning' : 'secondary'} className="h-6 gap-1.5 px-2 text-sm">
      {ok ? <CheckCircle2 /> : <Sparkles />} {text}
    </Badge>
  )
}

function PresetPicker({ value, onPick, disabled }: { value?: string; onPick: (p: Preset) => void; disabled?: boolean }) {
  return (
    <div className="grid grid-cols-1 gap-2 p-3 @md:grid-cols-2 @2xl:grid-cols-3" role="radiogroup" aria-label="Provider">
      {PRESETS.map((p) => {
        const active = value === p.id
        return (
          <button
            key={p.id}
            type="button"
            role="radio"
            aria-checked={active}
            disabled={disabled}
            onClick={() => onPick(p)}
            className={cn(
              'flex items-start gap-2.5 rounded-lg border p-2.5 text-left transition-[border-color,background-color,box-shadow] duration-150',
              'hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring/60 focus-visible:outline-none disabled:opacity-50',
              active ? 'border-primary/60 bg-primary/6 ring-1 ring-primary/30' : 'border-border',
            )}
          >
            <span className={cn('flex size-7 shrink-0 items-center justify-center rounded-md', active ? 'bg-primary/15 text-primary' : 'bg-muted text-muted-foreground')}>
              <p.icon className="size-4" />
            </span>
            <span className="min-w-0">
              <span className="block text-sm font-medium">{p.label}</span>
              <span className="block text-xs text-muted-foreground">{p.blurb}</span>
            </span>
          </button>
        )
      })}
    </div>
  )
}

function ProviderForm({ scope, view, isAdmin, mode }: { scope: 'global' | 'user'; view: AiConfigView; isAdmin: boolean; mode: 'desktop' | 'server' }) {
  const qc = useQueryClient()
  const [draft, setDraft] = useState<Draft>(() => toDraft(view))
  const [key, setKey] = useState<string | null>(null) // null = unchanged
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [test, setTest] = useState<{ ok: boolean; text: string; hint?: string } | null>(null)
  const [advanced, setAdvanced] = useState(false)
  useEffect(() => {
    setDraft(toDraft(view))
    setKey(null)
  }, [view])
  const preset = presetOf(draft)
  const set = (patch: Partial<Draft>) => {
    setDraft((d) => ({ ...d, ...patch }))
    setTest(null)
  }
  const dirty = key !== null || JSON.stringify(draft) !== JSON.stringify(toDraft(view))
  const models = useAiModels(scope, !!view.provider && (view.hasKey || view.provider === 'openai'))
  const modelList: AiModel[] = draft.provider === 'anthropic' ? (models.data?.source === 'provider' ? models.data.models : ANTHROPIC_MODELS) : (models.data?.models ?? [])
  const needsKey = preset?.keyRequired && !view.hasKey && !key
  const serverPolicy = scope === 'global' && isAdmin

  const pick = (p: Preset) => {
    setDraft((d) => ({
      ...d,
      provider: p.provider,
      preset: p.id,
      baseUrl: p.baseUrl,
      model: d.preset === p.id ? d.model : p.model,
    }))
    setTest(null)
  }

  const save = async () => {
    setSaving(true)
    try {
      await putConfig({ ...draft, scope, ...(key !== null ? { apiKey: key } : {}) })
      await qc.invalidateQueries({ queryKey: ['ai'] })
      const s = await refreshAiStatus()
      setKey(null)
      toast.success(s?.available ? 'AI assistant is ready' : 'AI settings saved', {
        description: s?.available ? 'Press Ctrl/⌘+I in a terminal, or open the assistant from the sidebar.' : undefined,
      })
    } catch (err) {
      toast.error('Could not save the AI settings', { description: errorMessage(err) })
    } finally {
      setSaving(false)
    }
  }

  const runTest = async () => {
    setTesting(true)
    setTest(null)
    try {
      const r = await testConfig({ ...draft, scope, ...(key ? { apiKey: key } : {}) })
      const reply = (r.reply ?? '').replace(/\s+/g, ' ').trim()
      setTest({ ok: true, text: `Connected in ${(r.latencyMs / 1000).toFixed(1)} s · ${r.model}${reply ? ` · “${reply.length > 60 ? `${reply.slice(0, 59)}…` : reply}”` : ''}` })
    } catch (err) {
      const d = isApiError(err) ? describeAiError(err.code, err.message) : { title: errorMessage(err) }
      setTest({ ok: false, text: d.title, hint: d.hint && d.hint !== d.title ? d.hint : isApiError(err) && err.message !== d.title ? err.message : undefined })
    } finally {
      setTesting(false)
    }
  }

  const remove = async () => {
    const ok = await confirm({
      title: 'Remove the AI provider?',
      description: 'The configuration and the stored API key are deleted. The assistant stops working until a provider is set up again.',
      confirmLabel: 'Remove',
      destructive: true,
    })
    if (!ok) return
    try {
      await putConfig({ scope, reset: true })
      await qc.invalidateQueries({ queryKey: ['ai'] })
      await refreshAiStatus()
      toast('AI provider removed')
    } catch (err) {
      toast.error('Could not remove the provider', { description: errorMessage(err) })
    }
  }

  return (
    <>
      <SettingsGroup
        title={scope === 'global' && mode === 'server' ? 'Provider (organisation)' : scope === 'user' ? 'Your provider' : 'Provider'}
        description={
          scope === 'global' && mode === 'server'
            ? 'Used by everyone on this server unless personal providers are allowed.'
            : 'Where requests go. Local servers (Ollama, LM Studio) keep everything on your network.'
        }
      >
        <div className="@container">
          <PresetPicker value={preset?.id} onPick={pick} />
        </div>
        {preset && (
          <>
            <SettingRow
              label="API key"
              htmlFor="ai-key"
              description={
                view.hasKey && key === null
                  ? 'Stored encrypted in the vault. It is never shown again.'
                  : preset.keyRequired
                    ? 'Required. Encrypted in the vault; only the AstraTerm server uses it.'
                    : 'Optional for local servers.'
              }
            >
              {view.hasKey && key === null ? (
                <div className="flex items-center gap-2">
                  <Badge variant="success" className="h-6 gap-1">
                    <KeyRound /> Key stored
                  </Badge>
                  <Button size="sm" variant="outline" onClick={() => setKey('')}>
                    Replace
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={async () => {
                      if (await confirm({ title: 'Delete the stored API key?', confirmLabel: 'Delete key', destructive: true })) {
                        try {
                          await putConfig({ ...toDraft(view), scope, apiKey: '' })
                          await qc.invalidateQueries({ queryKey: ['ai'] })
                          await refreshAiStatus()
                        } catch (err) {
                          toast.error('Could not delete the key', { description: errorMessage(err) })
                        }
                      }
                    }}
                  >
                    Delete
                  </Button>
                </div>
              ) : (
                <PasswordInput
                  id="ai-key"
                  className="w-72"
                  inputSize="sm"
                  autoComplete="off"
                  placeholder={preset.id === 'anthropic' ? 'sk-ant-…' : preset.keyRequired ? 'API key' : 'Leave empty if not needed'}
                  value={key ?? ''}
                  onChange={(e) => {
                    setKey(e.target.value)
                    setTest(null)
                  }}
                />
              )}
            </SettingRow>
            {(draft.provider !== 'anthropic' || advanced) && (
              <SettingRow label="Server URL" htmlFor="ai-url" description={draft.provider === 'anthropic' ? 'Only for gateways/proxies. Empty = api.anthropic.com.' : 'OpenAI-compatible base URL (ending in /v1 for most servers).'}>
                <Input
                  id="ai-url"
                  inputSize="sm"
                  className="w-72 font-mono"
                  spellCheck={false}
                  placeholder={draft.provider === 'anthropic' ? 'https://api.anthropic.com' : 'http://localhost:11434/v1'}
                  value={draft.baseUrl ?? ''}
                  onChange={(e) => set({ baseUrl: e.target.value })}
                />
              </SettingRow>
            )}
            <SettingRow
              label="Model"
              htmlFor="ai-model"
              description={models.data?.error ? `Model list unavailable: ${models.data.error}` : draft.provider === 'anthropic' ? 'Sonnet 5 is the best balance of speed and quality.' : `Name of the model (${preset.modelHint}).`}
            >
              {draft.provider === 'anthropic' && modelList.some((m) => m.id === (draft.model || 'claude-sonnet-5')) && !advanced ? (
                <SimpleSelect
                  id="ai-model"
                  size="sm"
                  className="w-72"
                  value={draft.model || 'claude-sonnet-5'}
                  onValueChange={(v) => set({ model: v })}
                  options={modelList.map((m) => ({
                    value: m.id,
                    label: (
                      <span className="truncate">
                        {m.label || m.id}
                        {m.hint && <span className="text-muted-foreground"> · {m.hint}</span>}
                      </span>
                    ),
                  }))}
                />
              ) : (
                <>
                  <Input
                    id="ai-model"
                    inputSize="sm"
                    className="w-72 font-mono"
                    spellCheck={false}
                    list="ai-model-list"
                    placeholder={preset.modelHint}
                    value={draft.model ?? ''}
                    onChange={(e) => set({ model: e.target.value })}
                  />
                  <datalist id="ai-model-list">
                    {modelList.map((m) => (
                      <option key={m.id} value={m.id}>
                        {m.label}
                      </option>
                    ))}
                  </datalist>
                </>
              )}
            </SettingRow>
            <button
              type="button"
              onClick={() => setAdvanced((a) => !a)}
              className="flex w-full items-center gap-1 px-4 py-2 text-left text-sm text-muted-foreground hover:text-foreground"
              aria-expanded={advanced}
            >
              <ChevronRight className={cn('size-3.5 transition-transform duration-150', advanced && 'rotate-90')} /> Advanced
            </button>
            {advanced && (
              <>
                <SettingRow label="Reasoning effort" htmlFor="ai-effort" description="Auto: quick for the command bar, balanced for chat. Higher is slower and costs more.">
                  <SimpleSelect
                    id="ai-effort"
                    size="sm"
                    className="w-40"
                    value={draft.effort || 'auto'}
                    onValueChange={(v) => set({ effort: v === 'auto' ? '' : (v as Draft['effort']) })}
                    options={[
                      { value: 'auto', label: 'Auto' },
                      { value: 'low', label: 'Low' },
                      { value: 'medium', label: 'Medium' },
                      { value: 'high', label: 'High' },
                    ]}
                  />
                </SettingRow>
                <SettingRow label="Max answer length" htmlFor="ai-max" description="Output token limit for chat answers (0 = default 16 000).">
                  <NumberInput id="ai-max" inputSize="sm" className="w-36" min={0} max={64000} step={1000} value={draft.maxTokens ?? 0} onChange={(v) => set({ maxTokens: v ?? 0 })} />
                </SettingRow>
              </>
            )}
          </>
        )}
        <div className="flex flex-wrap items-center gap-2 px-4 py-3">
          <Button size="sm" onClick={() => void save()} loading={saving} disabled={!dirty || !preset || (!!needsKey && preset.keyRequired && !view.hasKey && !key)}>
            Save
          </Button>
          <Button size="sm" variant="outline" onClick={() => void runTest()} loading={testing} disabled={!preset || (!!preset?.keyRequired && !view.hasKey && !key)}>
            <Plug /> Test connection
          </Button>
          {view.provider && (
            <Button size="sm" variant="ghost" className="ml-auto text-destructive hover:text-destructive" onClick={() => void remove()}>
              <Trash2 /> Remove
            </Button>
          )}
          {test && (
            <div className={cn('flex w-full items-start gap-1.5 text-sm animate-in fade-in-0 duration-150', test.ok ? 'text-success' : 'text-destructive')}>
              {test.ok ? <CheckCircle2 className="mt-0.5 size-3.5 shrink-0" /> : <XCircle className="mt-0.5 size-3.5 shrink-0" />}
              <span className="min-w-0 break-words">
                {test.text}
                {test.hint && <span className="text-muted-foreground"> — {test.hint}</span>}
              </span>
            </div>
          )}
        </div>
      </SettingsGroup>
      {serverPolicy && (mode === 'server' || !!view.provider) && <PolicyGroup draft={draft} set={set} mode={mode} dirty={dirty} save={save} saving={saving} />}
    </>
  )
}

function PolicyGroup({ draft, set, mode, dirty, save, saving }: { draft: Draft; set: (p: Partial<Draft>) => void; mode: 'desktop' | 'server'; dirty: boolean; save: () => Promise<void>; saving: boolean }) {
  return (
    <SettingsGroup
      title={mode === 'server' ? 'Access & limits' : 'Limits & privacy'}
      description={mode === 'server' ? 'Who may use the assistant on this server, and how much.' : 'Protects against runaway usage and extra secrets in your output.'}
    >
      {mode === 'server' && (
        <>
          <SettingRow label="Enable for users" htmlFor="ai-enabled" description="Off by default in server mode. Every request is audited (without its content).">
            <Switch id="ai-enabled" checked={!!draft.enabled} onCheckedChange={(c) => set({ enabled: c })} />
          </SettingRow>
          <SettingRow label="Users may choose the model" htmlFor="ai-usermodel">
            <Switch id="ai-usermodel" checked={draft.allowUserModel ?? true} onCheckedChange={(c) => set({ allowUserModel: c })} />
          </SettingRow>
          <SettingRow label="Allowed models" description="Empty = any model of the provider." stacked>
            <TagInput value={draft.models ?? []} onChange={(v) => set({ models: v })} placeholder="Add a model id and press Enter" aria-label="Allowed models" />
          </SettingRow>
          <SettingRow label="Personal providers" htmlFor="ai-userconfig" description="Let users bring their own provider and API key. Their endpoints go through the network policy.">
            <Switch id="ai-userconfig" checked={!!draft.allowUserConfig} onCheckedChange={(c) => set({ allowUserConfig: c })} />
          </SettingRow>
        </>
      )}
      {mode === 'desktop' && (
        <SettingRow label="Assistant enabled" htmlFor="ai-enabled-d" description="Turn the assistant off without removing its configuration.">
          <Switch id="ai-enabled-d" checked={draft.enabled ?? true} onCheckedChange={(c) => set({ enabled: c })} />
        </SettingRow>
      )}
      <SettingRow label="Requests per minute" htmlFor="ai-rpm" description="Per user. 0 = default (20), −1 = unlimited.">
        <NumberInput id="ai-rpm" inputSize="sm" className="w-32" min={-1} max={10000} value={draft.rateLimitPerMinute ?? 0} onChange={(v) => set({ rateLimitPerMinute: v ?? 0 })} />
      </SettingRow>
      <SettingRow label="Requests per day" htmlFor="ai-rpd" description="Per user. 0 = default (1000), −1 = unlimited.">
        <NumberInput id="ai-rpd" inputSize="sm" className="w-32" min={-1} max={1000000} value={draft.rateLimitPerDay ?? 0} onChange={(v) => set({ rateLimitPerDay: v ?? 0 })} />
      </SettingRow>
      <SettingRow
        label="Extra redaction patterns"
        description="Regular expressions (RE2) removed from everything sent, in addition to stored secrets, keys and common token formats."
        stacked
      >
        <TagInput value={draft.redactPatterns ?? []} onChange={(v) => set({ redactPatterns: v })} placeholder="e.g. corp-[0-9]{6}" aria-label="Redaction patterns" />
      </SettingRow>
      <div className="px-4 py-3">
        <Button size="sm" onClick={() => void save()} loading={saving} disabled={!dirty}>
          Save
        </Button>
      </div>
    </SettingsGroup>
  )
}

function PreferencesGroup() {
  const s = aiSettings.use()
  const toggle = (k: keyof AiUiSettings, label: string, description: string) => (
    <SettingRow label={label} description={description} htmlFor={`ai-${k}`}>
      <Switch id={`ai-${k}`} checked={!!s[k]} onCheckedChange={(c) => aiSettings.set({ [k]: c } as Partial<AiUiSettings>)} />
    </SettingRow>
  )
  return (
    <SettingsGroup title="In the terminal" description="How the assistant shows up while you work.">
      {toggle('hashTrigger', 'Commands from comments', 'Type “# what you want” at a prompt and press Enter to get a command suggestion.')}
      {toggle('offerOnError', 'Offer help on failed commands', 'Shows a small “Explain · Fix” chip next to a failed command’s output.')}
      {toggle('errorHeuristics', 'Recognise errors without shell integration', 'When the shell does not report exit codes (OSC 133), recognise common error messages instead.')}
      {toggle('autoAttachTerminal', 'Attach the active terminal to new chats', 'New chats start with the recent output of the active terminal as context.')}
      <SettingRow label="Terminal lines" htmlFor="ai-lines" description="How much recent output a terminal context includes.">
        <NumberInput id="ai-lines" inputSize="sm" className="w-28" min={10} max={1000} step={10} value={s.terminalLines} onChange={(v) => v != null && aiSettings.set({ terminalLines: v })} />
      </SettingRow>
    </SettingsGroup>
  )
}

function PersonalToggle() {
  const qc = useQueryClient()
  const cfg = useAiConfig('user')
  return (
    <SettingsGroup title="For you">
      {/* The switch appears with its real state (no on → off flip once the setting has loaded). */}
      <QueryState query={cfg} skeleton={<SkeletonRows rows={1} rowHeight={52} icon={false} />} errorTitle="Could not load your AI setting">
        {(view) => (
          <SettingRow label="Use the AI assistant" htmlFor="ai-me" description="Hide the assistant for your account only.">
            <Switch
              id="ai-me"
              checked={view.enabled !== false}
              onCheckedChange={async (c) => {
                try {
                  await putConfig({ ...toDraft(view), scope: 'user', enabled: c ? undefined : false })
                  await qc.invalidateQueries({ queryKey: aiKeys.config('user') })
                  await refreshAiStatus()
                } catch (err) {
                  toast.error('Could not save', { description: errorMessage(err) })
                }
              }}
            />
          </SettingRow>
        )}
      </QueryState>
    </SettingsGroup>
  )
}

export default function AiSettingsSection() {
  const status = useAiStatusStore((s) => s.status)
  const isAdmin = !!status?.canConfigureGlobal
  const mode = status?.mode ?? 'desktop'
  // Admins edit the organisation (global) provider; others their personal one when allowed.
  const scope: 'global' | 'user' = isAdmin ? 'global' : 'user'
  const canEdit = isAdmin || !!status?.canConfigure
  const cfg = useAiConfig(scope, canEdit)
  useEffect(() => {
    void refreshAiStatus()
  }, [])
  const managedNote = useMemo(() => {
    if (!status || canEdit) return null
    if (!status.enabled) return 'Your administrator has not enabled the AI assistant on this server.'
    return `Your administrator manages the provider${status.model ? ` (${status.model})` : ''}.`
  }, [status, canEdit])

  return (
    <SettingsPage
      title="AI assistant"
      description="Natural language to commands, error explanations and a sysadmin chat — optional, private by design."
      actions={<StatusLine />}
    >
      <div className="flex items-start gap-3 rounded-lg border border-success/25 bg-success/5 px-4 py-3 text-sm">
        <ShieldCheck className="mt-0.5 size-4 shrink-0 text-success" />
        <div className="grid gap-1 text-muted-foreground">
          <span className="font-medium text-foreground">Nothing is sent until you ask.</span>
          <span>
            A request contains your question and the context you attach (terminal output, selection, a file). Stored passwords, private
            keys and token-like strings are redacted on the server first; keys stay encrypted in the vault; commands never run without your
            confirmation.
          </span>
        </div>
      </div>
      {managedNote && <p className="text-sm text-muted-foreground">{managedNote}</p>}
      {canEdit && (
        <QueryState query={cfg} errorTitle="Could not load the AI settings">
          {(view) => <ProviderForm scope={scope} view={view} isAdmin={isAdmin} mode={mode} />}
        </QueryState>
      )}
      {!isAdmin && status && <PersonalToggle />}
      {status?.available && <PreferencesGroup />}
    </SettingsPage>
  )
}
