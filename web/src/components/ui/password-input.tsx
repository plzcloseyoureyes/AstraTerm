import * as React from 'react'
import { Eye, EyeOff, WandSparkles } from 'lucide-react'
import { estimateStrength, generatePassword, type GenerateOptions } from '@/lib/password'
import { cn } from '@/lib/utils'
import { inputBase } from './input'
import { Tooltip } from './tooltip'

export interface PasswordInputProps extends Omit<React.ComponentProps<'input'>, 'type'> {
  /** Show a "generate" button that fills a random password (and reveals it). */
  generate?: boolean | GenerateOptions
  /** Called with generated passwords (useful to mirror into a confirm field). */
  onGenerate?: (password: string) => void
  inputSize?: 'sm' | 'md'
}

/** Password field with reveal toggle and optional CSPRNG generator. */
export function PasswordInput({ className, generate, onGenerate, inputSize = 'md', onChange, ref: externalRef, ...props }: PasswordInputProps) {
  const [visible, setVisible] = React.useState(false)
  const ref = React.useRef<HTMLInputElement>(null)
  const setRefs = React.useCallback(
    (el: HTMLInputElement | null) => {
      ref.current = el
      if (typeof externalRef === 'function') externalRef(el)
      else if (externalRef) (externalRef as React.RefObject<HTMLInputElement | null>).current = el
    },
    [externalRef],
  )

  const doGenerate = () => {
    const pw = generatePassword(typeof generate === 'object' ? generate : {})
    const el = ref.current
    if (el) {
      // Use the native setter so React's onChange fires for controlled inputs.
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set
      setter?.call(el, pw)
      el.dispatchEvent(new Event('input', { bubbles: true }))
    }
    setVisible(true)
    onGenerate?.(pw)
  }

  const buttons = generate ? 2 : 1
  return (
    <div className={cn('relative flex w-full items-center', className)}>
      <input
        ref={setRefs}
        type={visible ? 'text' : 'password'}
        data-slot="password-input"
        autoComplete={props.autoComplete ?? 'new-password'}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        className={cn(inputBase, inputSize === 'sm' ? 'h-7 text-sm' : 'h-8', buttons === 2 ? 'pr-14' : 'pr-8', visible && 'font-mono')}
        onChange={onChange}
        {...props}
      />
      <div className="absolute right-1 flex items-center gap-0.5">
        {generate && (
          <Tooltip content="Generate password">
            <button
              type="button"
              onClick={doGenerate}
              disabled={props.disabled || props.readOnly}
              className="flex size-6 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 outline-none disabled:opacity-40"
              aria-label="Generate password"
            >
              <WandSparkles className="size-3.5" />
            </button>
          </Tooltip>
        )}
        <Tooltip content={visible ? 'Hide' : 'Show'}>
          <button
            type="button"
            onClick={() => setVisible((v) => !v)}
            className="flex size-6 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 outline-none"
            aria-label={visible ? 'Hide password' : 'Show password'}
            aria-pressed={visible}
          >
            {visible ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
          </button>
        </Tooltip>
      </div>
    </div>
  )
}

const METER_COLORS = ['bg-destructive', 'bg-destructive', 'bg-warning', 'bg-success', 'bg-success']

/** 4-segment strength meter with label and first hint. */
export function PasswordStrengthMeter({ password, context, className }: { password: string; context?: string[]; className?: string }) {
  const s = React.useMemo(() => estimateStrength(password, context), [password, context])
  return (
    <div className={cn('grid gap-1', className)} aria-live="polite">
      <div className="flex gap-1" aria-hidden>
        {[1, 2, 3, 4].map((i) => (
          <div
            key={i}
            className={cn('h-1 flex-1 rounded-full transition-colors', password && s.score >= i ? METER_COLORS[s.score] : 'bg-muted')}
          />
        ))}
      </div>
      <div className="flex justify-between gap-2 text-xs text-muted-foreground">
        <span>{password ? s.hints[0] ?? 'Looks good' : 'Use a long, unique passphrase'}</span>
        {password && <span className="shrink-0 font-medium">{s.label}</span>}
      </div>
    </div>
  )
}
