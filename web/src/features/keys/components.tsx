/*
 * Small building blocks shared by the keys views and dialogs.
 */
import { useMemo, useState, type ReactNode } from 'react'
import { BadgeCheck, Copy, Download, KeyRound, Lock, LockOpen, ShieldAlert, ShieldCheck, TriangleAlert } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SimpleSelect } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { Tooltip } from '@/components/ui/tooltip'
import { useNow } from '@/lib/hooks'
import { cn, formatBytes, formatDateTime } from '@/lib/utils'
import type { CertificateInfo, StoredKey } from './types'
import { certExpiryText, certTone, copyWithToast, downloadText, keyTypeLabel, randomart, randomartTitle, shortFingerprint } from './util'

export function CopyButton({ text, label = 'Copy', what, size = 'xs' }: { text: string; label?: string; what?: string; size?: 'xs' | 'sm' }) {
  return <IconButton icon={Copy} label={label} size={size} onClick={() => void copyWithToast(text, what)} />
}

/** A fingerprint in monospace with copy; long values are shortened unless `full`. */
export function Fingerprint({ value, full, className }: { value: string; full?: boolean; className?: string }) {
  return (
    <span className={cn('inline-flex min-w-0 items-center gap-1', className)}>
      <Tooltip content={value}>
        <span className="truncate font-mono text-sm tabular-nums">{full ? value : shortFingerprint(value)}</span>
      </Tooltip>
      <CopyButton text={value} label="Copy fingerprint" what="Fingerprint copied" />
    </span>
  )
}

/** A labelled read-only value with copy (fingerprints in dialogs). */
export function CopyField({ label, value, mono = true }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="grid gap-1">
      <div className="text-xs font-medium text-muted-foreground">{label}</div>
      <div className="flex items-start gap-2 rounded-md border bg-muted/40 px-2 py-1.5">
        <span className={cn('min-w-0 flex-1 text-sm break-all select-all', mono && 'font-mono')}>{value}</span>
        <CopyButton text={value} label={`Copy ${label.toLowerCase()}`} />
      </div>
    </div>
  )
}

/** OpenSSH randomart of a SHA256 fingerprint. */
export function Randomart({ fingerprint, type, bits, className }: { fingerprint: string; type: string; bits: number; className?: string }) {
  const art = useMemo(() => randomart(fingerprint, randomartTitle(type, bits)), [fingerprint, type, bits])
  if (!art) return null
  return (
    <pre
      aria-label={`Randomart of ${fingerprint}`}
      className={cn('w-fit rounded-md border bg-muted/40 px-3 py-2 font-mono text-[12px] leading-[1.15] text-foreground/90 select-all', className)}
    >
      {art}
    </pre>
  )
}

/** Public key (authorized_keys line) with copy / save actions. */
export function PublicKeyBox({ value, filename, rows = 3 }: { value: string; filename?: string; rows?: number }) {
  return (
    <div className="grid gap-1.5">
      <Textarea mono readOnly rows={rows} value={value} className="resize-none text-xs break-all" onFocus={(e) => e.currentTarget.select()} aria-label="Public key" />
      <div className="flex flex-wrap gap-1.5">
        <Button size="xs" variant="secondary" onClick={() => void copyWithToast(value, 'Public key copied')}>
          <Copy /> Copy public key
        </Button>
        {filename && (
          <Button size="xs" variant="secondary" onClick={() => downloadText(value + '\n', filename)}>
            <Download /> Save .pub
          </Button>
        )}
      </div>
    </div>
  )
}

export function KeyTypeBadge({ type, bits }: { type: string; bits: number }) {
  return (
    <Badge variant="outline" className="font-mono">
      {keyTypeLabel(type, bits)}
    </Badge>
  )
}

export function PassphraseBadge({ k }: { k: Pick<StoredKey, 'hasPassphrase' | 'passphraseSaved'> }) {
  if (!k.hasPassphrase) {
    return (
      <Tooltip content="The stored private key has no passphrase (it is still encrypted by the AstraTerm vault)">
        <Badge variant="secondary">
          <LockOpen /> None
        </Badge>
      </Tooltip>
    )
  }
  return (
    <Tooltip content={k.passphraseSaved ? 'Passphrase-protected; the passphrase is remembered in the vault' : 'Passphrase-protected; asked for when the key is used'}>
      <Badge variant="info">
        <Lock /> {k.passphraseSaved ? 'Passphrase' : 'Asks'}
      </Badge>
    </Tooltip>
  )
}

export function CertBadge({ info }: { info?: CertificateInfo }) {
  const now = useNow(60_000)
  if (!info) return null
  const tone = certTone(info, now)
  return (
    <Tooltip content={`OpenSSH certificate · ${certExpiryText(info, now)}`}>
      <Badge variant={tone}>
        {tone === 'success' ? <BadgeCheck /> : <TriangleAlert />} Cert
      </Badge>
    </Tooltip>
  )
}

const EXTENSION_LABELS: Record<string, string> = {
  'permit-pty': 'Terminal (PTY)',
  'permit-port-forwarding': 'Port forwarding',
  'permit-agent-forwarding': 'Agent forwarding',
  'permit-X11-forwarding': 'X11 forwarding',
  'permit-user-rc': '~/.ssh/rc',
  'no-touch-required': 'No touch required (FIDO)',
}

const USER_PERMISSIONS = ['permit-pty', 'permit-port-forwarding', 'permit-agent-forwarding', 'permit-X11-forwarding', 'permit-user-rc']

/** Certificate details with a live expiry countdown (SSH-5). */
export function CertificateDetails({ info }: { info: CertificateInfo }) {
  const now = useNow(30_000)
  const tone = certTone(info, now)
  const Icon = tone === 'destructive' ? ShieldAlert : ShieldCheck
  const rows: [string, ReactNode][] = [
    ['Type', info.type === 'host' ? 'Host certificate' : 'User certificate'],
    ['Key ID', info.keyId || <span className="text-muted-foreground">(none)</span>],
    [
      'Principals',
      info.principals.length ? (
        <span className="flex flex-wrap gap-1">
          {info.principals.map((p) => (
            <Badge key={p} variant="secondary" className="font-mono">
              {p}
            </Badge>
          ))}
        </span>
      ) : (
        <span className="text-warning">Any {info.type === 'host' ? 'host' : 'user'}</span>
      ),
    ],
    ['Valid from', info.validAfter ? formatDateTime(info.validAfter) : 'Always'],
    ['Valid until', info.validBefore ? formatDateTime(info.validBefore) : 'Forever'],
    ['Serial', <span className="font-mono">{info.serial}</span>],
    ['Signed by', <Fingerprint value={info.caFingerprint} />],
    ['CA key', <span className="font-mono">{info.caKeyType}</span>],
    ['Signature', <span className="font-mono">{info.signatureType}</span>],
  ]
  const critical = Object.entries(info.criticalOptions)
  return (
    <div className="grid gap-3">
      <div
        className={cn(
          'flex items-center gap-2 rounded-md border px-3 py-2 text-sm',
          tone === 'success' && 'border-success/40 bg-success/8 text-success',
          tone === 'warning' && 'border-warning/40 bg-warning/10 text-warning',
          tone === 'destructive' && 'border-destructive/40 bg-destructive/8 text-destructive',
        )}
        role="status"
      >
        <Icon className="size-4 shrink-0" />
        <span className="font-medium">{info.status === 'valid' ? 'Valid' : info.status === 'expired' ? 'Expired' : 'Not yet valid'}</span>
        <span className="text-foreground/80">· {certExpiryText(info, now)}</span>
      </div>
      <dl className="grid grid-cols-[auto_1fr] items-center gap-x-4 gap-y-1.5 text-sm">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="min-w-0">{v}</dd>
          </div>
        ))}
      </dl>
      {info.type === 'user' && (
        <div className="grid gap-1.5">
          <div className="text-xs font-medium text-muted-foreground">Permissions</div>
          <div className="flex flex-wrap gap-1">
            {USER_PERMISSIONS.map((p) => {
              const on = info.extensions.includes(p)
              return (
                <Badge key={p} variant={on ? 'success' : 'outline'} className={cn(!on && 'text-muted-foreground line-through')}>
                  {EXTENSION_LABELS[p]}
                </Badge>
              )
            })}
            {info.extensions
              .filter((e) => !USER_PERMISSIONS.includes(e))
              .map((e) => (
                <Badge key={e} variant="secondary">
                  {EXTENSION_LABELS[e] ?? e}
                </Badge>
              ))}
          </div>
        </div>
      )}
      {critical.length > 0 && (
        <div className="grid gap-1.5">
          <div className="text-xs font-medium text-muted-foreground">Restrictions (critical options)</div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
            {critical.map(([k, v]) => (
              <div key={k} className="contents">
                <dt className="font-mono text-muted-foreground">{k}</dt>
                <dd className="font-mono break-all">{v || '—'}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </div>
  )
}

/** Select one of the stored keys ("" = none). */
export function KeySelect({
  keys,
  value,
  onChange,
  noneLabel = 'None',
  id,
  disabled,
  filter,
}: {
  keys: StoredKey[] | undefined
  value: string
  onChange: (id: string) => void
  noneLabel?: string
  id?: string
  disabled?: boolean
  filter?: (k: StoredKey) => boolean
}) {
  const NONE = '__none__'
  const options = [
    ...(noneLabel ? [{ value: NONE, label: noneLabel }] : []),
    ...(keys ?? []).filter((k) => !filter || filter(k)).map((k) => ({ value: k.id, label: `${k.name} — ${keyTypeLabel(k.type, k.bits)}` })),
  ]
  if (value && !options.some((o) => o.value === value)) options.push({ value, label: 'Unknown key (deleted?)' })
  return (
    <SimpleSelect
      id={id}
      value={value || (noneLabel ? NONE : undefined)}
      onValueChange={(v) => onChange(v === NONE ? '' : v)}
      options={options}
      placeholder="Select a key"
      disabled={disabled}
      aria-label="SSH key"
    />
  )
}

/** Text input + "Load file…" for keys and certificates (text is read client-side, never uploaded as a file). */
export function KeyTextInput({
  value,
  onChange,
  placeholder,
  rows = 7,
  accept,
  id,
  onFileName,
  maxBytes = 256 * 1024,
  fileLabel = 'key',
}: {
  value: string
  onChange: (text: string) => void
  placeholder?: string
  rows?: number
  accept?: string
  id?: string
  onFileName?: (name: string) => void
  /** Largest file "Load file…" accepts. */
  maxBytes?: number
  /** What the file is, for the size error ("key", "known_hosts file"). */
  fileLabel?: string
}) {
  const [error, setError] = useState<string | null>(null)
  return (
    <div className="grid gap-1.5">
      <Textarea
        id={id}
        mono
        rows={rows}
        value={value}
        spellCheck={false}
        autoComplete="off"
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="text-xs"
      />
      <div className="flex items-center gap-2">
        <label className="inline-flex">
          <Input
            type="file"
            accept={accept}
            className="sr-only"
            onChange={async (e) => {
              const f = e.target.files?.[0]
              e.target.value = ''
              if (!f) return
              if (f.size > maxBytes) {
                setError(`The file is too large to be a ${fileLabel} (over ${formatBytes(maxBytes)})`)
                return
              }
              setError(null)
              onChange(await f.text())
              onFileName?.(f.name)
            }}
          />
          <span className="inline-flex h-6 cursor-pointer items-center gap-1.5 rounded-md border border-border/60 bg-secondary px-2 text-xs font-medium text-secondary-foreground hover:bg-accent focus-within:ring-2 focus-within:ring-ring/60">
            <KeyRound className="size-3.5" /> Load file…
          </span>
        </label>
        {error && <span className="text-sm text-destructive">{error}</span>}
      </div>
    </div>
  )
}

/** Small section heading inside dialogs / panels. */
export function SectionLabel({ children, className }: { children: ReactNode; className?: string }) {
  return <h3 className={cn('text-xs font-semibold tracking-wide text-muted-foreground uppercase', className)}>{children}</h3>
}
