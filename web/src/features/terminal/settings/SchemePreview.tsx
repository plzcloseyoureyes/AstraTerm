/*
 * Live colour scheme previews (TERM-9): a realistic shell sample for the selected scheme and compact gallery cards.
 */
import type { CSSProperties, ReactNode } from 'react'
import { Check } from 'lucide-react'
import { cn } from '@/lib/utils'
import { normalizeScheme } from '../themes'
import { ANSI_KEYS, type AnsiKey, type TerminalScheme } from '../types'

export interface PreviewFont {
  family: string
  size: number
  lineHeight: number
  weight: string
  boldWeight: string
  letterSpacing: number
  cursorStyle: 'block' | 'underline' | 'bar'
}

function C({ s, k, bold, children }: { s: TerminalScheme; k: AnsiKey | 'foreground'; bold?: boolean; children: ReactNode }) {
  return <span style={{ color: s[k], fontWeight: bold ? 'var(--nx-preview-bold)' : undefined }}>{children}</span>
}

/** A sample terminal screen rendered with the scheme and font settings. */
export function SchemePreview({ scheme, font, opacity = 1, className }: { scheme: TerminalScheme; font: PreviewFont; opacity?: number; className?: string }) {
  const s = normalizeScheme(scheme)
  const style = {
    background: s.background,
    color: s.foreground,
    fontFamily: font.family,
    fontSize: `${font.size}px`,
    lineHeight: font.lineHeight,
    fontWeight: font.weight === 'normal' ? 400 : font.weight === 'bold' ? 700 : Number(font.weight) || 400,
    letterSpacing: `${font.letterSpacing}px`,
    opacity: opacity < 1 ? 0.35 + opacity * 0.65 : undefined,
    '--nx-preview-bold': font.boldWeight === 'normal' ? 400 : font.boldWeight === 'bold' ? 700 : Number(font.boldWeight) || 700,
  } as CSSProperties
  const cursor: CSSProperties =
    font.cursorStyle === 'block'
      ? { background: s.cursor, color: s.cursorAccent }
      : font.cursorStyle === 'bar'
        ? { boxShadow: `inset 2px 0 0 ${s.cursor}` }
        : { boxShadow: `inset 0 -2px 0 ${s.cursor}` }
  return (
    <div className={cn('overflow-hidden rounded-md border', className)} style={style} role="img" aria-label={`Preview of the ${s.name} colour scheme`}>
      <pre className="m-0 overflow-x-auto px-3 py-2.5 whitespace-pre" style={{ font: 'inherit', letterSpacing: 'inherit' }}>
        <C s={s} k="green" bold>
          admin@web-01
        </C>
        :<C s={s} k="blue" bold>
          ~/app
        </C>
        $ ls -l{'\n'}
        drwxr-xr-x 5 admin staff  160 Sep 27 10:12 <C s={s} k="blue" bold>
          src
        </C>
        {'\n'}
        -rwxr-xr-x 1 admin staff 4.2K Sep 27 09:58 <C s={s} k="green" bold>
          deploy.sh
        </C>
        {'\n'}
        lrwxr-xr-x 1 admin staff   14 Sep 27 09:41 <C s={s} k="cyan" bold>
          current
        </C>{' '}
        -&gt; releases/42{'\n'}
        -rw-r--r-- 1 admin staff  12M Sep 27 09:40 <C s={s} k="red" bold>
          backup.tar.gz
        </C>
        {'\n'}
        <C s={s} k="green" bold>
          admin@web-01
        </C>
        :<C s={s} k="blue" bold>
          ~/app
        </C>
        $ git status -s{'\n'}
        {' '}
        <C s={s} k="red">M</C> src/server.go{'\n'}
        <C s={s} k="green">A </C> src/metrics.go{'\n'}
        <C s={s} k="brightBlack">?? tmp/</C>
        {'\n'}
        <C s={s} k="yellow">WARN</C> disk usage at 81% <C s={s} k="brightRed" bold>
          ERROR
        </C>{' '}
        retry <C s={s} k="magenta">#3</C>{' '}
        <span style={{ background: s.selectionBackground, color: s.selectionForeground }}>selected text</span>
        {'\n'}
        <C s={s} k="green" bold>
          admin@web-01
        </C>
        :<C s={s} k="blue" bold>
          ~/app
        </C>
        $ <span style={cursor}> </span>
      </pre>
      <div className="grid grid-cols-8 gap-px px-3 pb-2.5" aria-hidden>
        {ANSI_KEYS.map((k) => (
          <span key={k} className="h-3 rounded-[2px]" style={{ background: s[k] }} title={k} />
        ))}
      </div>
    </div>
  )
}

/** Compact gallery card for one scheme. */
export function SchemeCard({ scheme, selected, onSelect, actions }: { scheme: TerminalScheme; selected: boolean; onSelect: () => void; actions?: ReactNode }) {
  const s = normalizeScheme(scheme)
  return (
    <div className={cn('group relative rounded-md border transition-shadow', selected ? 'border-primary ring-2 ring-primary/40' : 'hover:border-foreground/30')}>
      <button
        type="button"
        role="radio"
        aria-checked={selected}
        aria-label={s.name}
        onClick={onSelect}
        className="flex w-full flex-col gap-1.5 rounded-md p-1.5 text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
      >
        <div className="flex h-12 w-full flex-col justify-between rounded-sm px-2 py-1.5 font-mono text-[10px] leading-none" style={{ background: s.background, color: s.foreground }}>
          <span>
            <span style={{ color: s.green }}>$</span> ls <span style={{ color: s.blue }}>src</span> <span style={{ color: s.red }}>err</span>
          </span>
          <span className="flex gap-0.5">
            {ANSI_KEYS.slice(1, 8).map((k) => (
              <span key={k} className="h-1.5 flex-1 rounded-[1px]" style={{ background: s[k] }} />
            ))}
          </span>
        </div>
        <span className="flex items-center gap-1 truncate text-xs">
          {selected && <Check className="size-3 shrink-0 text-primary" />}
          <span className="truncate">{s.name}</span>
        </span>
      </button>
      {actions && <div className="absolute top-1 right-1 opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">{actions}</div>}
    </div>
  )
}
