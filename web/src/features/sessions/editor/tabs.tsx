/*
 * Generic editor tabs (SM-2): Terminal, Network, Automation and Bookmark. Like protocol editors they work on the draft
 * connection through {value, onChange}.
 */
import { lazy, useId, useMemo, useState } from 'react'
import { DndContext, KeyboardSensor, PointerSensor, closestCenter, useSensor, useSensors, type DragEndEvent } from '@dnd-kit/core'
import { SortableContext, arrayMove, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { ArrowDown, ArrowUp, FolderPlus, GripVertical, Plus, X } from 'lucide-react'
import { protocolIcon } from '@/app/protocols'
import type { Connection, Folder, TerminalOverrides } from '@/api/types'
import { Button } from '@/components/ui/button'
import { ColorSwatchPicker } from '@/components/ui/color-swatch-picker'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { SwitchField } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { Textarea } from '@/components/ui/textarea'
import { Tooltip } from '@/components/ui/tooltip'
import { LazyBoundary } from '@/components/ui/spinner'
import { cn, isPlainObject } from '@/lib/utils'
import { TerminalOverridesEditor } from '@/features/terminal/overrides'
import { useEditorEnv, useFieldError } from '../editors/context'
import type { ProtocolProfile } from '../editors/define'
import {
  ComboInput,
  ComboOption,
  ConnectionSelectOption,
  NumberOption,
  OptionSection,
  SecretInput,
  SelectOption,
  SwitchOption,
  TextOption,
  optBool,
  optStrings,
  withOptions,
  type ComboSuggestion,
} from '../editors/fields'
import { ConnectionIcon } from '../icons'
import { IconPicker } from '../IconPicker'
import { connectionTarget, flattenFolders } from '../model'
import { parseHostPort } from '../quickparse'
import { openFolderDialog } from '../dialogs/store'

const NotesPreview = lazy(() => import('../NotesPreview'))

interface TabProps {
  value: Connection
  onChange: (next: Connection) => void
}

// ---------------------------------------------------------------------------------------------------------------------
// Terminal
// ---------------------------------------------------------------------------------------------------------------------

const TERM_TYPES: ComboSuggestion[] = [
  { value: 'xterm-256color', description: 'default' },
  { value: 'xterm' },
  { value: 'xterm-direct', description: '24-bit colour' },
  { value: 'screen-256color' },
  { value: 'tmux-256color' },
  { value: 'linux' },
  { value: 'vt100' },
  { value: 'vt220' },
  { value: 'ansi' },
  { value: 'dumb' },
]

const ENCODINGS: ComboSuggestion[] = [
  ['utf-8', 'Unicode (default)'],
  ['iso-8859-1', 'Western (Latin-1)'],
  ['iso-8859-15', 'Western (Latin-9)'],
  ['windows-1252', 'Western (Windows)'],
  ['iso-8859-2', 'Central European'],
  ['windows-1250', 'Central European (Windows)'],
  ['windows-1251', 'Cyrillic (Windows)'],
  ['koi8-r', 'Cyrillic (KOI8-R)'],
  ['iso-8859-7', 'Greek'],
  ['iso-8859-8', 'Hebrew'],
  ['windows-1256', 'Arabic (Windows)'],
  ['cp437', 'DOS (US)'],
  ['cp850', 'DOS (Western)'],
  ['shift_jis', 'Japanese (Shift JIS)'],
  ['euc-jp', 'Japanese (EUC)'],
  ['euc-kr', 'Korean'],
  ['gb18030', 'Chinese (Simplified)'],
  ['big5', 'Chinese (Traditional)'],
].map(([value, description]) => ({ value, description }))

function terminalOf(value: Connection): Record<string, unknown> {
  const t = value.options?.terminal
  return isPlainObject(t) ? t : {}
}

function withTerminal(value: Connection, patch: Record<string, unknown>): Connection {
  const next: Record<string, unknown> = { ...terminalOf(value) }
  for (const [k, v] of Object.entries(patch)) {
    if (v === undefined || v === null || v === '') delete next[k]
    else next[k] = v
  }
  return withOptions(value, { terminal: next })
}

export function TerminalTab({ value, onChange }: TabProps) {
  const p = { value, onChange }
  const t = terminalOf(value)
  const blink = typeof t.cursorBlink === 'boolean' ? (t.cursorBlink ? 'on' : 'off') : 'default'
  return (
    <div className="grid gap-5">
      <OptionSection title="Emulation">
        <ComboOption {...p} name="term" label="Terminal type" mono placeholder="xterm-256color" suggestions={TERM_TYPES} hint="TERM sent to the remote host." />
        <ComboOption {...p} name="encoding" label="Character encoding" mono placeholder="utf-8" suggestions={ENCODINGS} />
        <SelectOption
          {...p}
          name="backspace"
          label="Backspace sends"
          defaultLabel="DEL (^?) — default"
          options={[
            { value: 'del', label: 'DEL (^?)' },
            { value: 'ctrl-h', label: 'Ctrl-H (^H)' },
          ]}
        />
      </OptionSection>

      <OptionSection title="Appearance" description="Overrides for this session only; empty fields follow Settings → Terminal." columns={1}>
        <TerminalOverridesEditor value={t as TerminalOverrides} onChange={(next) => onChange(withOptions(value, { terminal: next }))} />
        <Field label="Cursor blink" className="@lg:max-w-[calc(50%-0.5rem)]">
          <SimpleSelect
            value={blink}
            onValueChange={(v) => onChange(withTerminal(value, { cursorBlink: v === 'default' ? undefined : v === 'on' }))}
            options={[
              { value: 'default', label: 'Default (from settings)' },
              { value: 'on', label: 'Blink' },
              { value: 'off', label: 'Steady' },
            ]}
          />
        </Field>
      </OptionSection>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Network
// ---------------------------------------------------------------------------------------------------------------------

type ProxyType = 'none' | 'socks5' | 'socks5h' | 'socks4' | 'socks4a' | 'http'

function proxyOf(value: Connection): Record<string, unknown> {
  const p = value.options?.proxy
  return isPlainObject(p) ? (p as Record<string, unknown>) : {}
}

function withProxy(value: Connection, patch: Record<string, unknown>): Connection {
  const next: Record<string, unknown> = { ...proxyOf(value) }
  for (const [k, v] of Object.entries(patch)) {
    if (v === undefined || v === null || v === '') delete next[k]
    else next[k] = v
  }
  return withOptions(value, { proxy: next.type && next.type !== 'none' ? next : undefined })
}

function ProxySection({ value, onChange, sshFamily }: TabProps & { sshFamily: boolean }) {
  const proxy = proxyOf(value)
  const type = (typeof proxy.type === 'string' ? proxy.type : 'none') as ProxyType
  const off = type === 'none'
  const hostErr = useFieldError('options.proxy.host')
  const portErr = useFieldError('options.proxy.port')
  const typeId = useId()
  return (
    <OptionSection title="Proxy">
      <Field label="Proxy type" htmlFor={typeId}>
        <SimpleSelect
          id={typeId}
          value={type}
          onValueChange={(v) => {
            if (v === 'none') onChange(withOptions(value, { proxy: undefined }))
            else onChange(withProxy(value, { type: v, port: proxy.port ?? (v === 'http' ? 3128 : 1080) }))
          }}
          options={[
            { value: 'none', label: 'None — connect directly' },
            { value: 'socks5', label: 'SOCKS5' },
            { value: 'socks5h', label: 'SOCKS5 (DNS through the proxy)' },
            { value: 'socks4', label: 'SOCKS4' },
            { value: 'socks4a', label: 'SOCKS4a (DNS through the proxy)' },
            { value: 'http', label: 'HTTP CONNECT' },
          ]}
        />
      </Field>
      <div className="grid grid-cols-[1fr_7rem] gap-2">
        <Field label="Proxy host" error={hostErr}>
          <Input
            value={typeof proxy.host === 'string' ? proxy.host : ''}
            disabled={off}
            placeholder={off ? '—' : 'proxy.example.com'}
            spellCheck={false}
            autoComplete="off"
            className="font-mono"
            onChange={(e) => onChange(withProxy(value, { host: e.target.value }))}
          />
        </Field>
        <Field label="Port" error={portErr}>
          <NumberInput
            value={typeof proxy.port === 'number' ? proxy.port : null}
            disabled={off}
            min={1}
            max={65535}
            allowEmpty
            onChange={(n) => onChange(withProxy(value, { port: n }))}
          />
        </Field>
      </div>
      <Field label="Proxy user">
        <Input
          value={typeof proxy.username === 'string' ? proxy.username : ''}
          disabled={off}
          placeholder={off ? '—' : 'None'}
          spellCheck={false}
          autoComplete="off"
          onChange={(e) => onChange(withProxy(value, { username: e.target.value }))}
        />
      </Field>
      <Field label="Proxy password">
        {off ? <Input disabled placeholder="—" /> : <SecretInput value={value} onChange={onChange} secret="proxyPassword" placeholder="None" />}
      </Field>
      {sshFamily && (
        <TextOption
          value={value}
          onChange={onChange}
          name="proxyCommand"
          label="Proxy command"
          placeholder="None"
          mono
          className="@lg:col-span-2"
          hint="Local command used as the transport, like ssh ProxyCommand (%h host, %p port, %r user, %n name, %% percent). Desktop mode or administrators only."
        />
      )}
    </OptionSection>
  )
}

/** A saved connection id (20-char base32, see model.NewID) that no longer resolves, vs an ad-hoc host spec. */
const CONNECTION_ID_RE = /^[a-z2-7]{20}$/

function SortableHop({ id, index, conn, onRemove }: { id: string; index: number; conn?: Connection; onRemove: () => void }) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id })
  const adHoc = !conn && !CONNECTION_ID_RE.test(id)
  const name = conn?.name ?? (adHoc ? id : 'Unknown session (deleted?)')
  return (
    <li
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={cn('flex h-8 items-center gap-2 rounded-md border bg-card px-1.5', isDragging && 'relative z-10 shadow-popover', !conn && !adHoc && 'border-destructive/40')}
    >
      <button
        type="button"
        className="flex size-6 cursor-grab items-center justify-center rounded-sm text-muted-foreground hover:bg-accent active:cursor-grabbing"
        aria-label={`Reorder ${name}`}
        {...attributes}
        {...listeners}
      >
        <GripVertical className="size-3.5" />
      </button>
      <span className="w-4 text-right text-xs text-muted-foreground tabular-nums">{index + 1}</span>
      {conn ? <ConnectionIcon protocol={conn.protocol} icon={conn.icon} color={conn.color} className="size-3.5" /> : null}
      <span className={cn('truncate text-sm font-medium', adHoc && 'font-mono')}>{name}</span>
      {conn && <span className="truncate font-mono text-xs text-muted-foreground">{connectionTarget(conn)}</span>}
      {adHoc && <span className="shrink-0 text-xs text-muted-foreground">not saved</span>}
      <span className="flex-1" />
      <Button variant="ghost" size="icon-xs" aria-label={`Remove ${name}`} onClick={onRemove}>
        <X />
      </Button>
    </li>
  )
}

/** Searchable "add a saved connection" field (scales to thousands of sessions). */
function AddConnectionPicker({
  candidates,
  onPick,
  placeholder,
  onFreeText,
}: {
  candidates: readonly Connection[]
  onPick: (id: string) => void
  placeholder: string
  /** Typed text + Enter with no suggestion chosen; return true when it was accepted. */
  onFreeText?: (text: string) => boolean
}) {
  const [text, setText] = useState('')
  const suggestions = useMemo<ComboSuggestion[]>(
    () => candidates.map((c) => ({ value: c.id, label: c.name, description: connectionTarget(c) })),
    [candidates],
  )
  const ids = useMemo(() => new Set(candidates.map((c) => c.id)), [candidates])
  return (
    <ComboInput
      value={text}
      placeholder={placeholder}
      disabled={!candidates.length && !onFreeText}
      suggestions={suggestions}
      emptyText="No matching session"
      onBlur={() => setText('')}
      onEnter={(typed) => {
        if (!onFreeText || !typed.trim() || !onFreeText(typed.trim())) return false
        setText('')
        return true
      }}
      onValueChange={(v) => {
        if (ids.has(v)) {
          onPick(v)
          setText('')
        } else setText(v)
      }}
    />
  )
}

/** "[user@]host[:port]" accepted as an ad-hoc hop. */
function isJumpSpec(text: string): boolean {
  try {
    const at = text.lastIndexOf('@')
    if (at >= 0 && text.slice(0, at).includes(':')) return false // no inline passwords
    parseHostPort(at >= 0 ? text.slice(at + 1) : text)
    return true
  } catch {
    return false
  }
}

/** Ordered SSH jump chain (`jumpHosts`: connection ids or ad-hoc hosts, first hop first); drag to reorder. */
function JumpHostsEditor({ value, onChange }: TabProps) {
  const env = useEditorEnv()
  const error = useFieldError('options.jumpHosts')
  const ids = optStrings(value, 'jumpHosts')
  const idsKey = ids.join(',')
  const self = env.selfId
  const byId = useMemo(() => new Map(env.connections.map((c) => [c.id, c])), [env.connections])
  // Other SSH sessions not already in the chain and not hopping through this one (no loops).
  const candidates = useMemo(() => {
    const chosen = new Set(idsKey ? idsKey.split(',') : [])
    return env.connections.filter((c) => c.protocol === 'ssh' && c.id !== self && !chosen.has(c.id) && !(self && optStrings(c, 'jumpHosts').includes(self)))
  }, [env.connections, idsKey, self])
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const set = (next: string[]) => onChange(withOptions(value, { jumpHosts: next }))
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) return
    const from = ids.indexOf(String(active.id))
    const to = ids.indexOf(String(over.id))
    if (from >= 0 && to >= 0) set(arrayMove(ids, from, to))
  }
  return (
    <div className="grid gap-1.5 @lg:col-span-2">
      <span className="text-sm font-medium text-foreground/90">Jump hosts</span>
      {ids.length === 0 ? (
        <p className="text-sm text-muted-foreground">Direct connection. Add hops to reach the host through bastions (like ssh -J).</p>
      ) : (
        <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
          <SortableContext items={ids} strategy={verticalListSortingStrategy}>
            <ol className="grid gap-1" aria-label="Jump hosts, first hop first">
              {ids.map((id, i) => (
                <SortableHop key={id} id={id} index={i} conn={byId.get(id)} onRemove={() => set(ids.filter((x) => x !== id))} />
              ))}
            </ol>
          </SortableContext>
        </DndContext>
      )}
      {ids.length > 0 && <p className="text-sm text-muted-foreground">You → {ids.map((id) => byId.get(id)?.name ?? id).join(' → ')} → this host</p>}
      <AddConnectionPicker
        candidates={candidates}
        placeholder="Add a jump host: a saved SSH session, or user@host:port + Enter"
        onPick={(id) => set([...ids, id])}
        onFreeText={(text) => {
          // Ad-hoc hop (the backend dials "[user@]host[:port]" entries directly).
          if (ids.includes(text) || !isJumpSpec(text)) return false
          set([...ids, text])
          return true
        }}
      />
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}

interface Knock {
  port: number
  proto: 'tcp' | 'udp'
  /** Pause after this knock (backend extension, SPEC §9 B1). */
  delayMs?: number
}

function knocksOf(value: Connection): Knock[] {
  const v = value.options?.portKnock
  if (!Array.isArray(v)) return []
  return v
    .filter((k): k is Knock => !!k && typeof k === 'object' && typeof (k as Knock).port === 'number')
    .map((k) => ({ port: k.port, proto: k.proto === 'udp' ? 'udp' : 'tcp', ...(typeof k.delayMs === 'number' && k.delayMs > 0 ? { delayMs: k.delayMs } : {}) }))
}

/** Port-knocking sequence (`portKnock`), sent in order before connecting. */
function PortKnockEditor({ value, onChange }: TabProps) {
  const knocks = knocksOf(value)
  const error = useFieldError('options.portKnock')
  const [draftPort, setDraftPort] = useState<number | null>(null)
  const [draftProto, setDraftProto] = useState<'tcp' | 'udp'>('tcp')
  const set = (next: Knock[]) => onChange(withOptions(value, { portKnock: next }))
  const move = (i: number, d: -1 | 1) => {
    const j = i + d
    if (j < 0 || j >= knocks.length) return
    const next = knocks.slice()
    ;[next[i], next[j]] = [next[j], next[i]]
    set(next)
  }
  return (
    <div className="grid gap-1.5 @lg:col-span-2">
      <span className="text-sm font-medium text-foreground/90">Port knocking</span>
      {knocks.length === 0 && <p className="text-sm text-muted-foreground">No knock sequence.</p>}
      {knocks.map((k, i) => (
        <div key={i} className="flex items-center gap-1.5">
          <span className="w-4 text-right text-xs text-muted-foreground tabular-nums">{i + 1}</span>
          <NumberInput
            aria-label={`Knock ${i + 1} port`}
            value={k.port}
            min={1}
            max={65535}
            className="w-28"
            onChange={(n) => n !== null && set(knocks.map((x, j) => (j === i ? { ...x, port: n } : x)))}
          />
          <SimpleSelect
            aria-label={`Knock ${i + 1} protocol`}
            className="w-24"
            value={k.proto}
            onValueChange={(proto) => set(knocks.map((x, j) => (j === i ? { ...x, proto } : x)))}
            options={[
              { value: 'tcp', label: 'TCP' },
              { value: 'udp', label: 'UDP' },
            ]}
          />
          <NumberInput
            aria-label={`Knock ${i + 1} delay`}
            value={k.delayMs ?? null}
            min={0}
            max={60_000}
            allowEmpty
            unit="ms"
            placeholder="Delay"
            className="w-32"
            onChange={(n) =>
              set(
                knocks.map((x, j) => {
                  if (j !== i) return x
                  const { delayMs: _old, ...rest } = x
                  return n ? { ...rest, delayMs: n } : rest
                }),
              )
            }
          />
          <Button variant="ghost" size="icon-sm" aria-label="Move up" disabled={i === 0} onClick={() => move(i, -1)}>
            <ArrowUp />
          </Button>
          <Button variant="ghost" size="icon-sm" aria-label="Move down" disabled={i === knocks.length - 1} onClick={() => move(i, 1)}>
            <ArrowDown />
          </Button>
          <Button variant="ghost" size="icon-sm" aria-label="Remove knock" onClick={() => set(knocks.filter((_, j) => j !== i))}>
            <X />
          </Button>
        </div>
      ))}
      <div className="flex items-center gap-1.5">
        <span className="w-4" />
        <NumberInput aria-label="New knock port" value={draftPort} onChange={setDraftPort} min={1} max={65535} allowEmpty placeholder="Port" className="w-28" />
        <SimpleSelect
          aria-label="New knock protocol"
          className="w-24"
          value={draftProto}
          onValueChange={setDraftProto}
          options={[
            { value: 'tcp', label: 'TCP' },
            { value: 'udp', label: 'UDP' },
          ]}
        />
        <Button
          variant="secondary"
          size="sm"
          disabled={draftPort === null}
          onClick={() => {
            if (draftPort === null) return
            set([...knocks, { port: draftPort, proto: draftProto }])
            setDraftPort(null)
          }}
        >
          <Plus /> Add
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}

export function NetworkTab({ value, onChange, profile }: TabProps & { profile: ProtocolProfile }) {
  const p = { value, onChange }
  return (
    <div className="grid gap-5">
      {profile.jump && (
        <OptionSection title={profile.jump === 'chain' ? 'SSH jump hosts' : 'SSH gateway'}>
          {profile.jump === 'chain' ? (
            <JumpHostsEditor {...p} />
          ) : (
            <ConnectionSelectOption
              {...p}
              name="sshTunnelVia"
              label="Tunnel through SSH session"
              noneLabel="None — connect directly"
              filter={(c) => c.protocol === 'ssh'}
              hint="Reach the host through a saved SSH connection (port forwarding)."
            />
          )}
        </OptionSection>
      )}
      <ProxySection {...p} sshFamily={profile.jump === 'chain'} />
      <OptionSection title="Connection">
        <NumberOption {...p} name="connectTimeoutSec" label="Connect timeout" min={1} max={600} unit="s" placeholder="20" hint="Paused while you answer prompts." />
        <NumberOption {...p} name="keepAliveSec" label="Keepalive interval" min={0} max={3600} unit="s" placeholder="30" hint="0 disables keepalives." />
        <PortKnockEditor {...p} />
      </OptionSection>
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Automation
// ---------------------------------------------------------------------------------------------------------------------

export function AutomationTab({ value, onChange, profile }: TabProps & { profile: ProtocolProfile }) {
  const p = { value, onChange }
  const terminal = profile.kind === 'terminal'
  const startupErr = useFieldError('options.startupCommand')
  return (
    <div className="grid gap-5">
      {terminal && (
        <OptionSection title="On connect" columns={1}>
          <Field label="Startup commands" hint="Sent to the session after it connects, one command per line." error={startupErr}>
            <Textarea
              mono
              rows={4}
              value={typeof value.options?.startupCommand === 'string' ? value.options.startupCommand : ''}
              placeholder={'cd /var/log\ntail -f syslog'}
              spellCheck={false}
              onChange={(e) => onChange(withOptions(value, { startupCommand: e.target.value }))}
            />
          </Field>
        </OptionSection>
      )}
      <OptionSection title="Reliability" columns={1}>
        <SwitchOption {...p} name="autoReconnect" label="Reconnect automatically" hint="Retry with backoff after a network drop (never after an authentication failure)." />
      </OptionSection>
      {terminal && (
        <OptionSection title="Recording & logging" columns={1}>
          <SwitchOption {...p} name="record" label="Record session" hint="Save an asciicast recording you can replay later." />
          <SwitchOption
            {...p}
            name="recordInput"
            label="Include keyboard input in recordings"
            hint="Off by default: typed input can contain passwords."
            disabled={!optBool(value, 'record')}
          />
          <SwitchOption {...p} name="log" label="Text log" hint="Write the session output to a plain-text log." />
          <SwitchOption {...p} name="logTimestamps" label="Timestamp log lines" disabled={!optBool(value, 'log')} />
        </OptionSection>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Bookmark
// ---------------------------------------------------------------------------------------------------------------------

const NEW_FOLDER = '__new_folder__'
const ROOT = '__root__'

function NotesField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [mode, setMode] = useState<'write' | 'preview'>('write')
  const id = useId()
  return (
    <div className="grid gap-1.5 @lg:col-span-2">
      <div className="flex items-center justify-between gap-2">
        <label htmlFor={id} className="text-sm font-medium text-foreground/90">
          Notes
        </label>
        <SegmentedControl
          size="sm"
          value={mode}
          onValueChange={setMode}
          aria-label="Notes view"
          options={[
            { value: 'write', label: 'Write' },
            { value: 'preview', label: 'Preview' },
          ]}
        />
      </div>
      {mode === 'write' ? (
        <Textarea
          id={id}
          rows={6}
          value={value}
          placeholder="Markdown: runbooks, contacts, asset IDs…"
          onChange={(e) => onChange(e.target.value)}
          className="font-mono text-sm"
        />
      ) : (
        <div className="relative min-h-[8.5rem] rounded-md border bg-muted/20 px-3 py-2">
          <LazyBoundary className="bg-transparent">
            <NotesPreview text={value} />
          </LazyBoundary>
        </div>
      )}
    </div>
  )
}

export function BookmarkTab({
  value,
  onChange,
  folders,
  allTags,
  canShare,
}: TabProps & { folders: readonly Folder[]; allTags: string[]; canShare: boolean }) {
  const folderId = useId()
  const colorErr = useFieldError('color')
  const iconErr = useFieldError('icon')
  const tagsErr = useFieldError('tags')
  const flat = flattenFolders(folders)
  const current = value.folderId || ROOT
  const options = [
    { value: ROOT, label: 'Top level' },
    ...flat.map((f) => ({
      value: f.folder.id,
      label: (
        <span className="flex items-center" style={{ paddingLeft: f.depth * 12 }}>
          {f.folder.name}
        </span>
      ),
    })),
    { value: NEW_FOLDER, label: <span className="flex items-center gap-1.5 text-primary">New folder…</span> },
  ]
  if (value.folderId && !flat.some((f) => f.folder.id === value.folderId)) options.splice(1, 0, { value: value.folderId, label: 'Unknown folder' })

  return (
    <div className="grid gap-5">
      <OptionSection title="Organisation">
        <Field label="Folder" htmlFor={folderId}>
          {/* Own id so Field does not copy the select's id onto this wrapper. */}
          <div id={`${folderId}-row`} className="flex items-center gap-1.5">
            <SimpleSelect
              id={folderId}
              className="flex-1"
              value={current}
              options={options}
              onValueChange={async (v) => {
                if (v === NEW_FOLDER) {
                  const created = await openFolderDialog({ mode: 'create', parentId: value.folderId || null })
                  if (created) onChange({ ...value, folderId: created.id })
                  return
                }
                onChange({ ...value, folderId: v === ROOT ? null : v })
              }}
            />
            <Tooltip content="New folder">
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="New folder"
                onClick={async () => {
                  const created = await openFolderDialog({ mode: 'create', parentId: value.folderId || null })
                  if (created) onChange({ ...value, folderId: created.id })
                }}
              >
                <FolderPlus />
              </Button>
            </Tooltip>
          </div>
        </Field>
        <Field label="Tags" error={tagsErr}>
          <TagInput value={value.tags ?? []} onChange={(tags) => onChange({ ...value, tags })} suggestions={allTags} placeholder="Add tag…" />
        </Field>
        <div className="grid content-start gap-2 pt-1">
          <SwitchField label="Favorite" description="Pinned in the Favorites section." checked={!!value.favorite} onCheckedChange={(v) => onChange({ ...value, favorite: v })} />
          <SwitchField
            label="Shared with all users"
            description={canShare ? 'Everyone can see and open it; secrets stay on the server.' : 'Only administrators can share sessions.'}
            checked={!!value.shared}
            disabled={!canShare}
            onCheckedChange={(v) => onChange({ ...value, shared: v })}
          />
        </div>
      </OptionSection>
      <OptionSection title="Appearance">
        <Field label="Colour" hint="Tab and tree colour." error={colorErr}>
          <ColorSwatchPicker value={value.color} onChange={(c) => onChange({ ...value, color: c })} allowNone size="sm" aria-label="Session colour" />
        </Field>
        <Field label="Icon" error={iconErr}>
          <IconPicker value={value.icon} onChange={(icon) => onChange({ ...value, icon: icon ?? undefined })} fallback={protocolIcon(value.protocol)} color={value.color} />
        </Field>
      </OptionSection>
      <OptionSection title="Notes" columns={1}>
        <NotesField value={value.notes ?? ''} onChange={(notes) => onChange({ ...value, notes })} />
      </OptionSection>
    </div>
  )
}
