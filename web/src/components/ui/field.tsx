import * as React from 'react'
import { cn } from '@/lib/utils'

interface FieldContextValue {
  id: string
  hintId: string
  errorId: string
  invalid: boolean
  describedBy?: string
}

const FieldContext = React.createContext<FieldContextValue | null>(null)

/** Ids + aria wiring for custom controls inside <Field> (spread onto the control). */
export function useFieldControl(): { id?: string; 'aria-describedby'?: string; 'aria-invalid'?: true } {
  const ctx = React.useContext(FieldContext)
  if (!ctx) return {}
  return { id: ctx.id, 'aria-describedby': ctx.describedBy, 'aria-invalid': ctx.invalid ? true : undefined }
}

export interface FieldProps {
  label?: React.ReactNode
  hint?: React.ReactNode
  error?: React.ReactNode
  required?: boolean
  /** Explicit id for the control (otherwise generated). */
  htmlFor?: string
  className?: string
  /** Label on the left, control on the right (settings rows). */
  orientation?: 'vertical' | 'horizontal'
  /** Extra element aligned right of the label (e.g. "Generate" link). */
  labelAside?: React.ReactNode
  children: React.ReactNode
}

/**
 * Form field wrapper: label, control, hint and error with proper aria wiring. The single child control receives
 * `id`, `aria-describedby` and `aria-invalid` automatically (or use useFieldControl() in custom controls).
 */
export function Field({ label, hint, error, required, htmlFor, className, orientation = 'vertical', labelAside, children }: FieldProps) {
  const autoId = React.useId()
  const id = htmlFor ?? autoId
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  const invalid = !!error
  const describedBy = [hint ? hintId : null, error ? errorId : null].filter(Boolean).join(' ') || undefined
  const ctx = React.useMemo(() => ({ id, hintId, errorId, invalid, describedBy }), [id, hintId, errorId, invalid, describedBy])

  let control = children
  if (React.isValidElement(children) && React.Children.count(children) === 1) {
    const el = children as React.ReactElement<Record<string, unknown>>
    control = React.cloneElement(el, {
      id: (el.props.id as string | undefined) ?? id,
      'aria-describedby': (el.props['aria-describedby'] as string | undefined) ?? describedBy,
      'aria-invalid': (el.props['aria-invalid'] as boolean | undefined) ?? (invalid || undefined),
    })
  }

  const labelEl = label ? (
    <div className="flex items-center justify-between gap-2">
      <label htmlFor={id} className="text-sm font-medium text-foreground/90 select-none">
        {label}
        {required && (
          <span className="ml-0.5 text-destructive" aria-hidden>
            *
          </span>
        )}
      </label>
      {labelAside}
    </div>
  ) : null

  const messages = (
    <>
      {hint && !error && (
        <p id={hintId} className="text-sm text-muted-foreground">
          {hint}
        </p>
      )}
      {error && (
        <p id={errorId} role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </>
  )

  return (
    <FieldContext.Provider value={ctx}>
      {orientation === 'horizontal' ? (
        <div className={cn('grid grid-cols-1 items-start gap-x-6 gap-y-1.5 sm:grid-cols-[minmax(10rem,16rem)_1fr]', className)}>
          <div className="grid gap-1 pt-1.5">{labelEl}</div>
          <div className="grid gap-1.5">
            {control}
            {messages}
          </div>
        </div>
      ) : (
        <div className={cn('grid content-start gap-1.5', className)}>
          {labelEl}
          {control}
          {messages}
        </div>
      )}
    </FieldContext.Provider>
  )
}

/** Titled group of fields (settings pages, dialogs). */
export function FieldSet({
  title,
  description,
  children,
  className,
  actions,
}: {
  title?: React.ReactNode
  description?: React.ReactNode
  children: React.ReactNode
  className?: string
  actions?: React.ReactNode
}) {
  return (
    <section className={cn('grid gap-4', className)}>
      {(title || description) && (
        <header className="flex items-end justify-between gap-4 border-b pb-2">
          <div className="grid gap-0.5">
            {title && <h3 className="text-md font-semibold">{title}</h3>}
            {description && <p className="text-sm text-muted-foreground">{description}</p>}
          </div>
          {actions}
        </header>
      )}
      {children}
    </section>
  )
}
