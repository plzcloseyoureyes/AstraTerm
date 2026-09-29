/*
 * Frontend-only utilities (TOOL-15, CC-13): password/passphrase generator, subnet calculator, hash calculator,
 * base64/hex/URL codec, JWT decoder, JSON formatter, timestamp converter, chmod calculator and UUID generator.
 * Everything runs in the browser (no backend); heavy libs (hash-wasm) are dynamically imported on demand.
 */
import * as React from 'react'
import { RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox, CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordStrengthMeter } from '@/components/ui/password-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { Slider } from '@/components/ui/slider'
import { Textarea } from '@/components/ui/textarea'
import { generatePassword, randomInt } from '@/lib/password'
import { cn, copyText, formatBytes, formatRelativeTime } from '@/lib/utils'
import { CopyButton, FormGrid } from './components'
import { calcSubnet } from './subnet'
import { WORDLIST } from './wordlist'

function UtilLayout({ title, description, children }: { title: string; description?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="shrink-0 border-b px-4 py-3">
        <h2 className="text-md font-semibold">{title}</h2>
        {description && <p className="mt-0.5 text-sm text-muted-foreground text-balance">{description}</p>}
      </header>
      {/* A plain scroller: Radix ScrollArea's display:table viewport lets wide content overflow narrow tabs. */}
      <div className="min-h-0 flex-1 overflow-x-hidden overflow-y-auto">
        <div className="p-4">{children}</div>
      </div>
    </div>
  )
}

function ResultField({ label, value, mono = true }: { label: string; value: React.ReactNode; mono?: boolean }) {
  return (
    <div className="flex items-baseline gap-2 border-b border-border/50 py-1">
      <span className="w-40 shrink-0 text-sm text-muted-foreground">{label}</span>
      <span className={cn('min-w-0 flex-1 break-all', mono && 'font-mono text-sm')}>{value}</span>
    </div>
  )
}

// ---- Password / passphrase generator (CC-13) -------------------------------------------------------------------------

function PasswordGenPanel() {
  const [mode, setMode] = React.useState<'password' | 'passphrase'>('password')
  const [length, setLength] = React.useState(20)
  const [classes, setClasses] = React.useState({ lower: true, upper: true, digits: true, symbols: true })
  const [words, setWords] = React.useState(5)
  const [sep, setSep] = React.useState('-')
  const [capitalize, setCapitalize] = React.useState(true)
  const [addNumber, setAddNumber] = React.useState(true)
  const [value, setValue] = React.useState('')

  const genPassword = React.useCallback(() => setValue(generatePassword({ length, ...classes })), [length, classes])
  const genPassphrase = React.useCallback(() => {
    const picked: string[] = []
    for (let i = 0; i < words; i++) {
      let w = WORDLIST[randomInt(WORDLIST.length)]
      if (capitalize) w = w.charAt(0).toUpperCase() + w.slice(1)
      picked.push(w)
    }
    let phrase = picked.join(sep)
    if (addNumber) phrase += sep + randomInt(100)
    setValue(phrase)
  }, [words, sep, capitalize, addNumber])

  const generate = React.useCallback(() => (mode === 'password' ? genPassword() : genPassphrase()), [mode, genPassword, genPassphrase])
  React.useEffect(() => {
    generate()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode])

  const passphraseBits = Math.round(words * Math.log2(WORDLIST.length) + (addNumber ? Math.log2(100) : 0))

  return (
    <UtilLayout title="Password generator" description="Cryptographically-random passwords and passphrases (crypto.getRandomValues).">
      <SegmentedControl className="mb-4" value={mode} onValueChange={setMode} options={[{ value: 'password', label: 'Password' }, { value: 'passphrase', label: 'Passphrase' }]} />

      <div className="mb-3 flex items-center gap-2 rounded-lg border bg-card p-3">
        <code className="min-w-0 flex-1 truncate font-mono text-md" title={value}>{value || '—'}</code>
        <CopyButton value={value} label="Copy" size="sm" />
        <Button variant="secondary" size="sm" onClick={generate}>
          <RefreshCw className="size-3.5" /> Regenerate
        </Button>
      </div>

      {mode === 'password' ? (
        <div className="grid gap-4">
          <Field label={`Length: ${length}`}>
            <Slider value={[length]} min={8} max={128} step={1} onValueChange={([v]) => setLength(v)} onValueCommit={genPassword} />
          </Field>
          <div className="flex flex-wrap gap-4">
            {(['lower', 'upper', 'digits', 'symbols'] as const).map((k) => (
              <CheckboxField
                key={k}
                label={{ lower: 'Lowercase', upper: 'Uppercase', digits: 'Digits', symbols: 'Symbols' }[k]}
                checked={classes[k]}
                onCheckedChange={(v) => {
                  const next = { ...classes, [k]: !!v }
                  if (!next.lower && !next.upper && !next.digits && !next.symbols) return
                  setClasses(next)
                }}
              />
            ))}
          </div>
          <PasswordStrengthMeter password={value} />
        </div>
      ) : (
        <div className="grid gap-4">
          <FormGrid>
            <Field label={`Words: ${words}`}>
              <Slider value={[words]} min={3} max={10} step={1} onValueChange={([v]) => setWords(v)} onValueCommit={genPassphrase} />
            </Field>
            <Field label="Separator">
              <SimpleSelect value={sep} onValueChange={setSep} options={[{ value: '-', label: 'Hyphen -' }, { value: '.', label: 'Dot .' }, { value: '_', label: 'Underscore _' }, { value: ' ', label: 'Space' }, { value: '', label: 'None' }]} />
            </Field>
          </FormGrid>
          <div className="flex flex-wrap gap-4">
            <CheckboxField label="Capitalize words" checked={capitalize} onCheckedChange={(v) => setCapitalize(!!v)} />
            <CheckboxField label="Append a number" checked={addNumber} onCheckedChange={(v) => setAddNumber(!!v)} />
          </div>
          <p className="text-sm text-muted-foreground">
            ~{passphraseBits} bits of entropy from a {WORDLIST.length.toLocaleString()}-word list.
          </p>
        </div>
      )}
    </UtilLayout>
  )
}

// ---- Subnet calculator (TOOL-8) --------------------------------------------------------------------------------------

function SubnetPanel() {
  const [input, setInput] = React.useState('192.168.1.0/24')
  const result = React.useMemo(() => calcSubnet(input), [input])
  const error = 'error' in result ? result.error : ''

  return (
    <UtilLayout title="Subnet calculator" description="IPv4 and IPv6 CIDR breakdown.">
      <Field label="Address / CIDR" error={error || undefined}>
        <Input value={input} onChange={(e) => setInput(e.target.value)} placeholder="192.168.1.0/24 or 2001:db8::/48" className="font-mono" aria-invalid={!!error} />
      </Field>
      {!('error' in result) && result.family === 4 && (
        <div className="mt-4">
          <ResultField label="CIDR" value={result.cidr} />
          <ResultField label="Netmask" value={result.netmask} />
          <ResultField label="Wildcard" value={result.wildcard} />
          <ResultField label="Network" value={result.network} />
          <ResultField label="Broadcast" value={result.broadcast} />
          <ResultField label="First host" value={result.firstHost} />
          <ResultField label="Last host" value={result.lastHost} />
          <ResultField label="Total addresses" value={result.hostCount} mono={false} />
          <ResultField label="Usable hosts" value={result.usableHosts} mono={false} />
          <ResultField label="Class" value={<span className="flex items-center gap-2">{result.ipClass}{result.isPrivate && <Badge variant="secondary">private</Badge>}</span>} mono={false} />
        </div>
      )}
      {!('error' in result) && result.family === 6 && (
        <div className="mt-4">
          <ResultField label="CIDR" value={result.cidr} />
          <ResultField label="Network" value={result.network} />
          <ResultField label="First address" value={result.firstAddress} />
          <ResultField label="Last address" value={result.lastAddress} />
          <ResultField label="Prefix" value={`/${result.prefix}`} />
          <ResultField label="Total addresses" value={result.addressCount} mono={false} />
        </div>
      )}
    </UtilLayout>
  )
}

// ---- Hash calculator -------------------------------------------------------------------------------------------------

const HASH_ALGOS = ['md5', 'sha1', 'sha256', 'sha512', 'blake3'] as const
type HashAlgo = (typeof HASH_ALGOS)[number]

type Hasher = { init: () => unknown; update: (d: Uint8Array | string) => unknown; digest: (enc?: 'hex') => string }

async function createHashers(algos: readonly HashAlgo[]): Promise<Record<string, Hasher>> {
  const wasm = await import('hash-wasm')
  const make: Record<HashAlgo, () => Promise<Hasher>> = {
    md5: () => wasm.createMD5() as Promise<Hasher>,
    sha1: () => wasm.createSHA1() as Promise<Hasher>,
    sha256: () => wasm.createSHA256() as Promise<Hasher>,
    sha512: () => wasm.createSHA512() as Promise<Hasher>,
    blake3: () => wasm.createBLAKE3() as Promise<Hasher>,
  }
  const out: Record<string, Hasher> = {}
  for (const a of algos) {
    out[a] = await make[a]()
    out[a].init()
  }
  return out
}

function HashPanel() {
  const [text, setText] = React.useState('')
  const [algos, setAlgos] = React.useState<Record<HashAlgo, boolean>>({ md5: true, sha1: true, sha256: true, sha512: false, blake3: false })
  const [results, setResults] = React.useState<Record<string, string>>({})
  const [busy, setBusy] = React.useState(false)
  const [progress, setProgress] = React.useState<number | null>(null)
  const [file, setFile] = React.useState<File | null>(null)
  const [expected, setExpected] = React.useState('')
  const seq = React.useRef(0)

  const selected = React.useMemo(() => HASH_ALGOS.filter((a) => algos[a]), [algos])

  // Every computation gets a sequence number; results of a superseded one are dropped.
  const compute = React.useCallback(async (source: string | File, which: readonly HashAlgo[]) => {
    const my = ++seq.current
    if (which.length === 0) {
      setResults({})
      return
    }
    setBusy(true)
    setProgress(typeof source === 'string' ? null : 0)
    try {
      const hashers = await createHashers(which)
      if (typeof source === 'string') {
        for (const h of Object.values(hashers)) h.update(source)
      } else {
        const reader = source.stream().getReader()
        let done = 0
        for (;;) {
          const { value, done: end } = await reader.read()
          if (end) break
          if (my !== seq.current) {
            void reader.cancel()
            return
          }
          for (const h of Object.values(hashers)) h.update(value)
          done += value.byteLength
          setProgress(source.size ? done / source.size : 1)
        }
      }
      if (my !== seq.current) return
      const out: Record<string, string> = {}
      for (const [a, h] of Object.entries(hashers)) out[a] = h.digest('hex')
      setResults(out)
    } catch (err) {
      if (my === seq.current) toast.error('Hashing failed', { description: err instanceof Error ? err.message : String(err) })
    } finally {
      if (my === seq.current) {
        setBusy(false)
        setProgress(null)
      }
    }
  }, [])

  React.useEffect(() => {
    if (file) {
      void compute(file, selected)
      return
    }
    if (!text) {
      seq.current++
      setResults({})
      setBusy(false)
      return
    }
    const t = window.setTimeout(() => void compute(text, selected), 200)
    return () => window.clearTimeout(t)
  }, [text, file, selected, compute])

  React.useEffect(() => () => void seq.current++, [])

  const want = expected.trim().toLowerCase()
  const match = want ? Object.entries(results).find(([, v]) => v === want)?.[0] : undefined

  return (
    <UtilLayout title="Hash calculator" description="MD5, SHA-1/256/512 and BLAKE3 of text or a file of any size (computed locally in the browser with hash-wasm).">
      <div className="mb-3 flex flex-wrap gap-4">
        {HASH_ALGOS.map((a) => (
          <CheckboxField key={a} label={a.toUpperCase()} checked={algos[a]} onCheckedChange={(v) => setAlgos((s) => ({ ...s, [a]: !!v }))} />
        ))}
      </div>
      <Field label="Text">
        <Textarea
          mono
          rows={4}
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            setFile(null)
          }}
          placeholder="Type or paste text to hash…"
        />
      </Field>
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <label className="inline-flex cursor-pointer items-center gap-2 rounded-md border bg-secondary px-3 py-1.5 text-sm hover:bg-accent focus-within:ring-2 focus-within:ring-ring/50">
          <input
            type="file"
            className="sr-only"
            onChange={(e) => {
              const f = e.target.files?.[0]
              e.target.value = ''
              if (!f) return
              setText('')
              setFile(f)
            }}
          />
          Choose file…
        </label>
        {file && (
          <span className="text-sm text-muted-foreground">
            {file.name} ({formatBytes(file.size)})
          </span>
        )}
        {busy && <span className="text-sm text-muted-foreground tabular">Hashing…{progress != null ? ` ${Math.round(progress * 100)}%` : ''}</span>}
      </div>
      <div className="mt-4">
        {selected.map((a) => (
          <div key={a} className={cn('flex items-baseline gap-2 border-b border-border/50 py-1', match === a && 'bg-success/10')}>
            <span className="w-20 shrink-0 text-sm font-medium text-muted-foreground uppercase">{a}</span>
            <code className="min-w-0 flex-1 break-all font-mono text-sm select-text">{results[a] || '—'}</code>
            {results[a] && <CopyButton value={results[a]} label="" title={`Copy ${a}`} />}
          </div>
        ))}
      </div>
      <Field label="Compare with" hint="Paste an expected checksum to verify it" className="mt-4">
        <Input value={expected} onChange={(e) => setExpected(e.target.value)} placeholder="expected hash" className="font-mono" spellCheck={false} />
      </Field>
      {want && Object.keys(results).length > 0 && (
        <p className={cn('mt-1 text-sm', match ? 'text-success' : 'text-destructive')}>{match ? `Matches the ${match.toUpperCase()} hash.` : 'Does not match any computed hash.'}</p>
      )}
    </UtilLayout>
  )
}

// ---- Encode / decode -------------------------------------------------------------------------------------------------

function bytesToBinary(bytes: Uint8Array): string {
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  return bin
}
function toBase64(s: string, urlSafe = false): string {
  const b = btoa(bytesToBinary(new TextEncoder().encode(s)))
  return urlSafe ? b.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '') : b
}
function fromBase64(s: string): string {
  const clean = s.replace(/\s+/g, '').replace(/-/g, '+').replace(/_/g, '/')
  const padded = clean + '='.repeat((4 - (clean.length % 4)) % 4)
  const bin = atob(padded)
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
  return new TextDecoder().decode(bytes)
}
function toHex(s: string): string {
  return Array.from(new TextEncoder().encode(s))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
}
function fromHex(s: string): string {
  const clean = s.replace(/^0x/i, '').replace(/[\s:,-]/g, '')
  if (/[^0-9a-fA-F]/.test(clean)) throw new Error('Not a hexadecimal string')
  if (clean.length % 2 !== 0) throw new Error('Hex length must be even')
  const bytes = new Uint8Array(clean.length / 2)
  for (let i = 0; i < bytes.length; i++) bytes[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16)
  return new TextDecoder().decode(bytes)
}

function EncodePanel() {
  const [format, setFormat] = React.useState<'base64' | 'base64url' | 'hex' | 'url'>('base64')
  const [op, setOp] = React.useState<'encode' | 'decode'>('encode')
  const [input, setInput] = React.useState('')

  const output = React.useMemo(() => {
    if (!input) return { value: '', error: '' }
    try {
      if (format === 'base64' || format === 'base64url') return { value: op === 'encode' ? toBase64(input, format === 'base64url') : fromBase64(input), error: '' }
      if (format === 'hex') return { value: op === 'encode' ? toHex(input) : fromHex(input), error: '' }
      return { value: op === 'encode' ? encodeURIComponent(input) : decodeURIComponent(input), error: '' }
    } catch (err) {
      return { value: '', error: err instanceof Error ? err.message : 'Invalid input' }
    }
  }, [input, format, op])

  return (
    <UtilLayout title="Encode / decode" description="Base64, hexadecimal and URL encoding.">
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <SegmentedControl value={format} onValueChange={setFormat} options={[{ value: 'base64', label: 'Base64' }, { value: 'base64url', label: 'Base64url' }, { value: 'hex', label: 'Hex' }, { value: 'url', label: 'URL' }]} aria-label="Encoding" />
        <SegmentedControl value={op} onValueChange={setOp} options={[{ value: 'encode', label: 'Encode' }, { value: 'decode', label: 'Decode' }]} aria-label="Direction" />
        <Button variant="ghost" size="sm" disabled={!output.value} onClick={() => { setInput(output.value); setOp(op === 'encode' ? 'decode' : 'encode') }}>
          Swap
        </Button>
      </div>
      <Field label="Input">
        <Textarea mono rows={5} value={input} onChange={(e) => setInput(e.target.value)} placeholder="Text to transform…" />
      </Field>
      <div className="mt-3 flex items-center justify-between">
        <span className="text-sm font-medium text-foreground/90">Output</span>
        {output.value && <CopyButton value={output.value} label="Copy" size="xs" />}
      </div>
      {output.error ? (
        <p className="mt-1 text-sm text-destructive">{output.error}</p>
      ) : (
        <Textarea mono rows={5} readOnly value={output.value} className="mt-1" />
      )}
    </UtilLayout>
  )
}

// ---- JWT decoder -----------------------------------------------------------------------------------------------------

function b64urlToJson(s: string, part: string): unknown {
  let json: string
  try {
    json = fromBase64(s)
  } catch {
    throw new Error(`The ${part} is not valid base64url`)
  }
  try {
    return JSON.parse(json)
  } catch {
    throw new Error(`The ${part} is not JSON`)
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === 'object' && !Array.isArray(v)
}

function JwtPanel() {
  const [token, setToken] = React.useState('')
  const decoded = React.useMemo(() => {
    const t = token.trim().replace(/^Bearer\s+/i, '')
    if (!t) return null
    const parts = t.split('.')
    if (parts.length === 5) return { error: 'This is an encrypted JWE token; its payload cannot be decoded without the key' }
    if (parts.length < 2) return { error: 'A JWT has at least two dot-separated parts' }
    try {
      const header = b64urlToJson(parts[0], 'header')
      const payload = b64urlToJson(parts[1], 'payload')
      if (!isRecord(header) || !isRecord(payload)) return { error: 'The header and payload must be JSON objects' }
      return { header, payload, signature: parts[2] || '' }
    } catch (err) {
      return { error: err instanceof Error ? err.message : 'Invalid token' }
    }
  }, [token])

  const claimTime = (v: unknown) => (typeof v === 'number' ? `${new Date(v * 1000).toLocaleString()} (${formatRelativeTime(v * 1000)})` : '')

  return (
    <UtilLayout title="JWT decoder" description="Decode a JSON Web Token's header and claims (signature is not verified).">
      <Field label="Token">
        <Textarea mono rows={4} value={token} onChange={(e) => setToken(e.target.value)} placeholder="eyJhbGci…" />
      </Field>
      {decoded && 'error' in decoded && <p className="mt-2 text-sm text-destructive">{decoded.error}</p>}
      {decoded && !('error' in decoded) && (
        <div className="mt-4 grid gap-4">
          <div>
            <div className="mb-1 text-sm font-medium text-muted-foreground">Header</div>
            <pre className="overflow-x-auto rounded-md border bg-muted/50 p-2 font-mono text-sm">{JSON.stringify(decoded.header, null, 2)}</pre>
          </div>
          <div>
            <div className="mb-1 text-sm font-medium text-muted-foreground">Payload</div>
            <pre className="overflow-x-auto rounded-md border bg-muted/50 p-2 font-mono text-sm">{JSON.stringify(decoded.payload, null, 2)}</pre>
          </div>
          {(decoded.payload.exp != null || decoded.payload.iat != null || decoded.payload.nbf != null) && (
            <div className="text-sm">
              {decoded.payload.iat != null && <ResultField label="Issued at" value={claimTime(decoded.payload.iat)} mono={false} />}
              {decoded.payload.nbf != null && <ResultField label="Not before" value={claimTime(decoded.payload.nbf)} mono={false} />}
              {decoded.payload.exp != null && (
                <ResultField
                  label="Expires"
                  value={<span className={cn(typeof decoded.payload.exp === 'number' && decoded.payload.exp * 1000 < Date.now() && 'text-destructive')}>{claimTime(decoded.payload.exp)}</span>}
                  mono={false}
                />
              )}
            </div>
          )}
        </div>
      )}
    </UtilLayout>
  )
}

// ---- JSON formatter --------------------------------------------------------------------------------------------------

function sortKeys(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortKeys)
  if (v && typeof v === 'object') {
    const out: Record<string, unknown> = {}
    for (const k of Object.keys(v as Record<string, unknown>).sort()) out[k] = sortKeys((v as Record<string, unknown>)[k])
    return out
  }
  return v
}

function JsonPanel() {
  const [input, setInput] = React.useState('')
  const [output, setOutput] = React.useState('')
  const [error, setError] = React.useState('')

  const transform = (kind: 'pretty' | 'minify' | 'sort') => {
    if (!input.trim()) return
    try {
      const parsed = JSON.parse(input)
      if (kind === 'minify') setOutput(JSON.stringify(parsed))
      else if (kind === 'sort') setOutput(JSON.stringify(sortKeys(parsed), null, 2))
      else setOutput(JSON.stringify(parsed, null, 2))
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid JSON')
      setOutput('')
    }
  }

  return (
    <UtilLayout title="JSON formatter" description="Pretty-print, minify or sort keys, with error reporting.">
      <div className="mb-2 flex flex-wrap gap-2">
        <Button size="sm" onClick={() => transform('pretty')}>Format</Button>
        <Button size="sm" variant="secondary" onClick={() => transform('minify')}>Minify</Button>
        <Button size="sm" variant="secondary" onClick={() => transform('sort')}>Sort keys</Button>
        {output && <CopyButton value={output} label="Copy" size="sm" />}
      </div>
      <Field label="Input">
        <Textarea mono rows={6} value={input} onChange={(e) => setInput(e.target.value)} placeholder='{"hello": "world"}' aria-invalid={!!error} />
      </Field>
      {error && <p className="mt-1 text-sm text-destructive">{error}</p>}
      {output && (
        <div className="mt-3">
          <span className="text-sm font-medium text-foreground/90">Output</span>
          <pre className="mt-1 overflow-x-auto rounded-md border bg-muted/50 p-2 font-mono text-sm">{output}</pre>
        </div>
      )}
    </UtilLayout>
  )
}

// ---- Timestamp converter ---------------------------------------------------------------------------------------------

function TimestampPanel() {
  const [input, setInput] = React.useState('')
  const parsed = React.useMemo(() => {
    const t = input.trim()
    if (!t) return null
    let dateMs: number | null = null
    let unit = ''
    if (/^-?\d+(\.\d+)?$/.test(t)) {
      const digits = t.replace(/^-/, '').split('.')[0].length
      const n = Number(t)
      // Unit heuristic by magnitude: seconds (≤ 11 digits), ms (12–14), µs (15–17), ns (18+).
      if (digits >= 18) [dateMs, unit] = [n / 1e6, 'nanoseconds']
      else if (digits >= 15) [dateMs, unit] = [n / 1e3, 'microseconds']
      else if (digits >= 12) [dateMs, unit] = [n, 'milliseconds']
      else [dateMs, unit] = [n * 1000, 'seconds']
    } else {
      const d = Date.parse(t)
      if (!Number.isNaN(d)) dateMs = d
    }
    if (dateMs === null || !Number.isFinite(dateMs) || Math.abs(dateMs) > 8.64e15) return { error: 'Unrecognized timestamp' }
    const d = new Date(dateMs)
    return {
      unit,
      unixSec: Math.floor(dateMs / 1000),
      unixMs: dateMs,
      iso: d.toISOString(),
      local: d.toLocaleString(),
      utc: d.toUTCString(),
      relative: formatRelativeTime(dateMs),
    }
  }, [input])

  return (
    <UtilLayout title="Timestamp converter" description="Convert between Unix time and human-readable dates.">
      <div className="flex items-end gap-2">
        <Field label="Unix timestamp or date" className="flex-1">
          <Input value={input} onChange={(e) => setInput(e.target.value)} placeholder="1700000000, 1700000000000 or 2023-11-14T22:13:20Z" className="font-mono" />
        </Field>
        <Button variant="secondary" size="sm" onClick={() => setInput(String(Math.floor(Date.now() / 1000)))}>Now</Button>
      </div>
      {parsed && 'error' in parsed && <p className="mt-2 text-sm text-destructive">{parsed.error}</p>}
      {parsed && !('error' in parsed) && (
        <div className="mt-4">
          {parsed.unit && <ResultField label="Interpreted as" value={`Unix ${parsed.unit}`} mono={false} />}
          <ResultField label="Unix (seconds)" value={<span className="flex items-center gap-2">{parsed.unixSec}<CopyButton value={String(parsed.unixSec)} label="" size="xs" /></span>} />
          <ResultField label="Unix (millis)" value={parsed.unixMs} />
          <ResultField label="ISO 8601" value={parsed.iso} />
          <ResultField label="Local" value={parsed.local} mono={false} />
          <ResultField label="UTC" value={parsed.utc} mono={false} />
          <ResultField label="Relative" value={parsed.relative} mono={false} />
        </div>
      )}
    </UtilLayout>
  )
}

// ---- chmod calculator ------------------------------------------------------------------------------------------------

const PERM_BITS = [
  { key: 'ur', mask: 0o400 }, { key: 'uw', mask: 0o200 }, { key: 'ux', mask: 0o100 },
  { key: 'gr', mask: 0o040 }, { key: 'gw', mask: 0o020 }, { key: 'gx', mask: 0o010 },
  { key: 'or', mask: 0o004 }, { key: 'ow', mask: 0o002 }, { key: 'ox', mask: 0o001 },
]

function modeToSymbolic(mode: number): string {
  const rwx = (m: number) => (m & 4 ? 'r' : '-') + (m & 2 ? 'w' : '-') + (m & 1 ? 'x' : '-')
  let s = rwx((mode >> 6) & 7) + rwx((mode >> 3) & 7) + rwx(mode & 7)
  // Apply special bits to the symbolic string.
  if (mode & 0o4000) s = s.slice(0, 2) + (s[2] === 'x' ? 's' : 'S') + s.slice(3)
  if (mode & 0o2000) s = s.slice(0, 5) + (s[5] === 'x' ? 's' : 'S') + s.slice(6)
  if (mode & 0o1000) s = s.slice(0, 8) + (s[8] === 'x' ? 't' : 'T')
  return s
}

function ChmodPanel() {
  const [mode, setMode] = React.useState(0o644)
  const octal = (mode & 0o7777).toString(8).padStart(mode & 0o7000 ? 4 : 3, '0')
  // The octal field keeps its own text while typing (e.g. clearing it to type 755) and follows checkbox changes.
  const [octalText, setOctalText] = React.useState(octal)
  React.useEffect(() => setOctalText(octal), [octal])

  const toggle = (mask: number) => setMode((m) => m ^ mask)
  const setOctal = (v: string) => {
    const clean = v.replace(/[^0-7]/g, '').slice(0, 4)
    setOctalText(clean)
    if (clean.length >= 3) setMode(parseInt(clean, 8) & 0o7777)
  }

  const rows: { label: string; bits: [string, string, string] }[] = [
    { label: 'Owner', bits: ['ur', 'uw', 'ux'] },
    { label: 'Group', bits: ['gr', 'gw', 'gx'] },
    { label: 'Others', bits: ['or', 'ow', 'ox'] },
  ]
  const maskOf = (key: string) => PERM_BITS.find((b) => b.key === key)!.mask

  return (
    <UtilLayout title="chmod calculator" description="Convert between octal, symbolic and permission bits.">
      <FormGrid>
        <Field label="Octal">
          <Input value={octalText} onChange={(e) => setOctal(e.target.value)} onBlur={() => setOctalText(octal)} className="font-mono" maxLength={4} inputMode="numeric" aria-label="Octal mode" />
        </Field>
        <Field label="Symbolic">
          <Input readOnly value={modeToSymbolic(mode)} className="font-mono" />
        </Field>
        <Field label="chmod command">
          <Input readOnly value={`chmod ${octal} file`} className="font-mono" />
        </Field>
      </FormGrid>
      <table className="mt-4 w-full max-w-md text-sm">
        <thead>
          <tr className="text-left text-xs text-muted-foreground">
            <th className="py-1" />
            <th className="py-1 text-center">Read</th>
            <th className="py-1 text-center">Write</th>
            <th className="py-1 text-center">Execute</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.label} className="border-t border-border/50">
              <td className="py-1.5 font-medium">{r.label}</td>
              {r.bits.map((key) => (
                <td key={key} className="py-1.5 text-center">
                  <Checkbox checked={(mode & maskOf(key)) !== 0} onCheckedChange={() => toggle(maskOf(key))} aria-label={key} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <div className="mt-3 flex flex-wrap gap-4">
        <CheckboxField label="setuid (4000)" checked={(mode & 0o4000) !== 0} onCheckedChange={() => toggle(0o4000)} />
        <CheckboxField label="setgid (2000)" checked={(mode & 0o2000) !== 0} onCheckedChange={() => toggle(0o2000)} />
        <CheckboxField label="sticky (1000)" checked={(mode & 0o1000) !== 0} onCheckedChange={() => toggle(0o1000)} />
      </div>
    </UtilLayout>
  )
}

// ---- UUID generator --------------------------------------------------------------------------------------------------

function uuidV4(): string {
  const b = new Uint8Array(16)
  crypto.getRandomValues(b)
  b[6] = (b[6] & 0x0f) | 0x40
  b[8] = (b[8] & 0x3f) | 0x80
  return formatUuid(b)
}

function uuidV7(): string {
  const b = new Uint8Array(16)
  crypto.getRandomValues(b)
  const ts = Date.now()
  b[0] = (ts / 2 ** 40) & 0xff
  b[1] = (ts / 2 ** 32) & 0xff
  b[2] = (ts / 2 ** 24) & 0xff
  b[3] = (ts / 2 ** 16) & 0xff
  b[4] = (ts / 2 ** 8) & 0xff
  b[5] = ts & 0xff
  b[6] = (b[6] & 0x0f) | 0x70
  b[8] = (b[8] & 0x3f) | 0x80
  return formatUuid(b)
}

function formatUuid(b: Uint8Array): string {
  const hex = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

function UuidPanel() {
  const [version, setVersion] = React.useState<'v4' | 'v7' | 'nil'>('v4')
  const [count, setCount] = React.useState(5)
  const [list, setList] = React.useState<string[]>([])

  const generate = React.useCallback(() => {
    const gen = version === 'v4' ? uuidV4 : version === 'v7' ? uuidV7 : () => '00000000-0000-0000-0000-000000000000'
    setList(Array.from({ length: count }, gen))
  }, [version, count])

  React.useEffect(() => {
    generate()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [version, count])

  return (
    <UtilLayout title="UUID generator" description="Generate version 4 (random) or version 7 (time-ordered) UUIDs.">
      <div className="flex flex-wrap items-end gap-3">
        <Field label="Version">
          <SegmentedControl value={version} onValueChange={setVersion} options={[{ value: 'v4', label: 'v4' }, { value: 'v7', label: 'v7' }, { value: 'nil', label: 'nil' }]} />
        </Field>
        <Field label="Count">
          <NumberInput value={count} onChange={(v) => setCount(v ?? 1)} min={1} max={100} />
        </Field>
        <Button variant="secondary" size="sm" onClick={generate}>
          <RefreshCw className="size-3.5" /> Regenerate
        </Button>
        <Button variant="ghost" size="sm" onClick={() => void copyText(list.join('\n')).then((ok) => ok && toast.success('Copied all'))}>Copy all</Button>
      </div>
      <div className="mt-4 flex flex-col gap-1">
        {list.map((u, i) => (
          <div key={i} className="flex items-center gap-2 border-b border-border/50 py-1">
            <code className="min-w-0 flex-1 font-mono text-sm">{u}</code>
            <CopyButton value={u} label="" size="xs" />
          </div>
        ))}
      </div>
    </UtilLayout>
  )
}

export const UTILITY_PANELS: Record<string, React.ComponentType> = {
  password: PasswordGenPanel,
  subnet: SubnetPanel,
  hash: HashPanel,
  encode: EncodePanel,
  jwt: JwtPanel,
  json: JsonPanel,
  timestamp: TimestampPanel,
  chmod: ChmodPanel,
  uuid: UuidPanel,
}
