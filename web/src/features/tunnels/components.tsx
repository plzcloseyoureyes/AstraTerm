/*
 * Small presentational pieces shared by the tunnel manager tab and its dialogs.
 */
import { useEffect, useMemo, useState, type KeyboardEvent } from 'react'
import { Command } from 'cmdk'
import {
  Bot,
  ChartLine,
  Check,
  ChevronsUpDown,
  Cloud,
  Container,
  Cpu,
  Database,
  Film,
  Folder,
  Gamepad2,
  GitBranch,
  Globe,
  HardDrive,
  House,
  Key,
  Lock,
  Mail,
  MessageSquare,
  Monitor,
  Network,
  Printer,
  Server,
  Shield,
  SquareTerminal,
  Webhook,
  X,
  type LucideIcon,
} from 'lucide-react'
import { RadioGroup as RadioGroupPrimitive } from 'radix-ui'
import { useConnections } from '@/api/connections'
import type { Connection } from '@/api/types'
import { protocolIcon } from '@/app/protocols'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, formatBytes, formatRate } from '@/lib/utils'
import { KINDS, TONE_DOT, TONE_LABEL, connectionLabel, isSSHConnection, statusText, statusTone } from './model'
import type { TunnelKind, TunnelStatusEx } from './types'

export function StatusDot({ status, className, withTooltip = true }: { status: TunnelStatusEx | undefined; className?: string; withTooltip?: boolean }) {
  const tone = statusTone(status)
  const dot = <span aria-hidden className={cn('inline-block size-2 shrink-0 rounded-full', TONE_DOT[tone], className)} />
  if (!withTooltip) return dot
  return (
    <Tooltip
      content={<span className="whitespace-pre-line">{`${TONE_LABEL[tone]}\n${statusText(status)}${status?.warning ? `\n${status.warning}` : ''}`}</span>}
      side="right"
    >
      <span className="inline-flex items-center" role="img" aria-label={TONE_LABEL[tone]}>
        {dot}
      </span>
    </Tooltip>
  )
}

/** Tunnel icons (options.icon), in picker order. */
export const TUNNEL_ICONS: Record<string, { icon: LucideIcon; label: string }> = {
  web: { icon: Globe, label: 'Web' },
  database: { icon: Database, label: 'Database' },
  docker: { icon: Container, label: 'Containers' },
  server: { icon: Server, label: 'Server' },
  desktop: { icon: Monitor, label: 'Remote desktop' },
  terminal: { icon: SquareTerminal, label: 'Terminal' },
  files: { icon: Folder, label: 'Files' },
  mail: { icon: Mail, label: 'Mail' },
  git: { icon: GitBranch, label: 'Git' },
  metrics: { icon: ChartLine, label: 'Monitoring' },
  api: { icon: Webhook, label: 'API' },
  cloud: { icon: Cloud, label: 'Cloud' },
  network: { icon: Network, label: 'Network' },
  storage: { icon: HardDrive, label: 'Storage' },
  lock: { icon: Lock, label: 'Secure service' },
  key: { icon: Key, label: 'Keys' },
  shield: { icon: Shield, label: 'Security' },
  cpu: { icon: Cpu, label: 'Hardware' },
  printer: { icon: Printer, label: 'Printer' },
  home: { icon: House, label: 'Home' },
  bot: { icon: Bot, label: 'Bot' },
  media: { icon: Film, label: 'Media' },
  chat: { icon: MessageSquare, label: 'Chat' },
  game: { icon: Gamepad2, label: 'Game' },
}

/** The icon component of a tunnel icon name (undefined for none / unknown names). */
export function tunnelIcon(name: string | undefined): LucideIcon | undefined {
  return name && Object.hasOwn(TUNNEL_ICONS, name) ? TUNNEL_ICONS[name].icon : undefined
}

/** A tunnel's icon, or nothing when it has none. */
export function TunnelIcon({ name, className }: { name: string | undefined; className?: string }) {
  const Icon = tunnelIcon(name)
  return Icon ? <Icon aria-hidden className={cn('size-3.5 shrink-0 text-muted-foreground', className)} /> : null
}

/** Icon chooser (arrow keys move within the grid, like any radio group). */
export function IconPicker({ value, onChange, id }: { value: string; onChange: (name: string) => void; id?: string }) {
  const item = 'flex size-7 items-center justify-center rounded-md border border-transparent text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40 data-[state=checked]:border-primary data-[state=checked]:bg-primary/10 data-[state=checked]:text-primary'
  return (
    <RadioGroupPrimitive.Root id={id} aria-label="Icon" value={value || 'none'} onValueChange={(v) => onChange(v === 'none' ? '' : v)} className="flex flex-wrap gap-1">
      <RadioGroupPrimitive.Item value="none" aria-label="No icon" title="No icon" className={item}>
        <X className="size-3.5" />
      </RadioGroupPrimitive.Item>
      {Object.entries(TUNNEL_ICONS).map(([name, { icon: Icon, label }]) => (
        <RadioGroupPrimitive.Item key={name} value={name} aria-label={label} title={label} className={item}>
          <Icon className="size-3.5" />
        </RadioGroupPrimitive.Item>
      ))}
    </RadioGroupPrimitive.Root>
  )
}

const KIND_VARIANT: Record<TunnelKind, 'default' | 'info' | 'success' | 'warning'> = {
  local: 'default',
  remote: 'warning',
  dynamic: 'success',
  rdynamic: 'info',
}

export function KindBadge({ kind, className }: { kind: TunnelKind; className?: string }) {
  const k = KINDS[kind]
  return (
    <Badge variant={KIND_VARIANT[kind]} className={cn('font-mono', className)} title={`${k.label}: ${k.description}`}>
      {k.flag}
      <span className="font-sans">{k.short}</span>
    </Badge>
  )
}

export function Traffic({ status, compact }: { status: TunnelStatusEx | undefined; compact?: boolean }) {
  if (!status || (status.bytesIn === 0 && status.bytesOut === 0 && status.state === 'stopped')) {
    return <span className="text-muted-foreground/60">—</span>
  }
  const rate = status.rateIn > 0 || status.rateOut > 0
  return (
    <span className="block truncate tabular-nums whitespace-nowrap" title={`Received ${formatBytes(status.bytesIn)} · sent ${formatBytes(status.bytesOut)}`}>
      <span className="text-muted-foreground">↓</span>
      {formatBytes(status.bytesIn)} <span className="text-muted-foreground">↑</span>
      {formatBytes(status.bytesOut)}
      {rate && !compact && (
        <span className="ml-1.5 text-xs text-primary">
          {formatRate(status.rateIn)} / {formatRate(status.rateOut)}
        </span>
      )}
    </span>
  )
}

/** Searchable picker of the SSH connections the user can see. */
export function ConnectionPicker({
  value,
  onChange,
  id,
  invalid,
  disabled,
  'aria-describedby': describedBy,
  'aria-invalid': ariaInvalid,
}: {
  value: string
  onChange: (id: string, conn: Connection) => void
  id?: string
  invalid?: boolean
  disabled?: boolean
  /** Set by <Field> (error / hint wiring). */
  'aria-describedby'?: string
  'aria-invalid'?: boolean
}) {
  const { data, isLoading } = useConnections()
  const [open, setOpen] = useState(false)
  const conns = useMemo(() => (data ?? []).filter(isSSHConnection).sort((a, b) => a.name.localeCompare(b.name)), [data])
  const current = conns.find((c) => c.id === value) ?? data?.find((c) => c.id === value)
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          id={id}
          type="button"
          role="combobox"
          aria-expanded={open}
          aria-invalid={invalid || ariaInvalid || undefined}
          aria-describedby={describedBy}
          disabled={disabled}
          className={cn(
            'flex h-8 w-full min-w-0 items-center gap-2 rounded-md border border-input bg-background/60 px-2.5 text-left text-base shadow-xs dark:bg-input/25',
            'outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25 disabled:opacity-50',
            'aria-invalid:border-destructive',
          )}
        >
          <Server className="size-3.5 shrink-0 text-muted-foreground" />
          <span className={cn('min-w-0 flex-1 truncate', !current && 'text-muted-foreground')}>
            {current ? connectionLabel(current) : isLoading ? 'Loading sessions…' : 'Choose an SSH session…'}
          </span>
          <ChevronsUpDown className="size-3.5 shrink-0 opacity-60" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--radix-popover-trigger-width) min-w-72 p-0">
        <Command
          filter={(itemValue, search) => (itemValue.toLowerCase().includes(search.toLowerCase().trim()) ? 1 : 0)}
          className="flex max-h-72 flex-col"
        >
          <Command.Input
            autoFocus
            placeholder="Search SSH sessions…"
            className="h-8 w-full border-b bg-transparent px-2.5 text-base outline-none placeholder:text-muted-foreground/70"
          />
          <Command.List className="min-h-0 overflow-y-auto p-1">
            <Command.Empty className="px-2 py-3 text-center text-sm text-muted-foreground">
              {conns.length ? 'No matching session' : 'No saved SSH session yet'}
            </Command.Empty>
            {conns.map((c) => {
              const Icon = protocolIcon(c.protocol)
              return (
                <Command.Item
                  key={c.id}
                  value={`${c.name} ${c.username}@${c.host} ${c.id}`}
                  onSelect={() => {
                    onChange(c.id, c)
                    setOpen(false)
                  }}
                  className="flex h-8 cursor-default items-center gap-2 rounded-sm px-2 text-base data-[selected=true]:bg-accent data-[selected=true]:text-accent-foreground"
                >
                  <Icon className="size-3.5 shrink-0 text-muted-foreground" />
                  <span className="truncate">{c.name}</span>
                  <span className="ml-auto truncate pl-2 font-mono text-xs text-muted-foreground">
                    {c.username ? `${c.username}@` : ''}
                    {c.host}
                    {c.port && c.port !== 22 ? `:${c.port}` : ''}
                  </span>
                  {c.id === value && <Check className="size-3.5 shrink-0 text-primary" />}
                </Command.Item>
              )
            })}
          </Command.List>
        </Command>
      </PopoverContent>
    </Popover>
  )
}

/**
 * Port field that reports every keystroke (the flow diagram updates live, and Enter never submits a stale value);
 * ↑/↓ step by 1 (Shift: 10). Range errors are reported by the form validation.
 */
export function PortInput({
  value,
  onChange,
  placeholder,
  id,
  min = 0,
  'aria-invalid': ariaInvalid,
  'aria-describedby': describedBy,
  inputSize,
}: {
  value: number | null
  onChange: (v: number | null) => void
  placeholder?: string
  id?: string
  min?: number
  'aria-invalid'?: boolean
  'aria-describedby'?: string
  inputSize?: 'sm' | 'md'
}) {
  const [text, setText] = useState(value == null ? '' : String(value))
  // Follow outside changes (presets, "Use 8081") without fighting the user's typing.
  useEffect(() => {
    const cur = text === '' ? null : Number(text)
    if (cur !== value) setText(value == null ? '' : String(value))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value])
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return
    e.preventDefault()
    const step = (e.key === 'ArrowUp' ? 1 : -1) * (e.shiftKey ? 10 : 1)
    const next = Math.min(65535, Math.max(min, (value ?? (e.key === 'ArrowUp' ? min - 1 : min)) + step))
    setText(String(next))
    onChange(next)
  }
  return (
    <Input
      id={id}
      inputMode="numeric"
      autoComplete="off"
      inputSize={inputSize}
      className="font-mono tabular-nums"
      placeholder={placeholder}
      value={text}
      aria-invalid={ariaInvalid}
      aria-describedby={describedBy}
      onKeyDown={onKeyDown}
      onChange={(e) => {
        const t = e.target.value.replace(/\D/g, '').slice(0, 5)
        setText(t)
        onChange(t === '' ? null : Number(t))
      }}
    />
  )
}
