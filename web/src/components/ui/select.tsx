import * as React from 'react'
import { Select as SelectPrimitive } from 'radix-ui'
import { Check, ChevronDown, ChevronUp } from 'lucide-react'
import { cn } from '@/lib/utils'
import { usePortalContainer } from './portal'

export const Select = SelectPrimitive.Root
export const SelectGroup = SelectPrimitive.Group
export const SelectValue = SelectPrimitive.Value

export function SelectTrigger({
  className,
  children,
  size = 'md',
  ...props
}: React.ComponentProps<typeof SelectPrimitive.Trigger> & { size?: 'sm' | 'md' }) {
  return (
    <SelectPrimitive.Trigger
      data-slot="select-trigger"
      className={cn(
        'flex w-full items-center justify-between gap-2 rounded-md border border-input bg-background/60 px-2.5 text-base shadow-xs dark:bg-input/25',
        'outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25',
        'data-[placeholder]:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive',
        '[&>span]:truncate [&_svg]:shrink-0',
        size === 'sm' ? 'h-7 text-sm' : 'h-8',
        className,
      )}
      {...props}
    >
      {children}
      <SelectPrimitive.Icon asChild>
        <ChevronDown className="size-3.5 opacity-60" />
      </SelectPrimitive.Icon>
    </SelectPrimitive.Trigger>
  )
}

export function SelectContent({
  className,
  children,
  position = 'popper',
  ...props
}: React.ComponentProps<typeof SelectPrimitive.Content>) {
  return (
    <SelectPrimitive.Portal container={usePortalContainer()}>
      <SelectPrimitive.Content
        data-slot="select-content"
        position={position}
        className={cn(
          'relative z-50 max-h-(--radix-select-content-available-height) min-w-[8rem] overflow-hidden rounded-md border bg-popover text-popover-foreground shadow-popover',
          'data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=open]:zoom-in-95',
          position === 'popper' && 'w-full min-w-(--radix-select-trigger-width) data-[side=bottom]:translate-y-1 data-[side=top]:-translate-y-1',
          className,
        )}
        {...props}
      >
        <SelectPrimitive.ScrollUpButton className="flex h-5 items-center justify-center text-muted-foreground">
          <ChevronUp className="size-3.5" />
        </SelectPrimitive.ScrollUpButton>
        <SelectPrimitive.Viewport className="p-1">{children}</SelectPrimitive.Viewport>
        <SelectPrimitive.ScrollDownButton className="flex h-5 items-center justify-center text-muted-foreground">
          <ChevronDown className="size-3.5" />
        </SelectPrimitive.ScrollDownButton>
      </SelectPrimitive.Content>
    </SelectPrimitive.Portal>
  )
}

export function SelectLabel({ className, ...props }: React.ComponentProps<typeof SelectPrimitive.Label>) {
  return <SelectPrimitive.Label className={cn('px-2 py-1 text-xs font-medium text-muted-foreground', className)} {...props} />
}

export function SelectItem({ className, children, ...props }: React.ComponentProps<typeof SelectPrimitive.Item>) {
  return (
    <SelectPrimitive.Item
      data-slot="select-item"
      className={cn(
        'relative flex w-full cursor-default items-center gap-2 rounded-sm py-1 pr-7 pl-2 text-base outline-none select-none',
        'focus:bg-accent focus:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50',
        "[&_svg:not([class*='size-'])]:size-4 [&_svg]:shrink-0",
        className,
      )}
      {...props}
    >
      <SelectPrimitive.ItemText>{children}</SelectPrimitive.ItemText>
      <span className="absolute right-2 flex size-3.5 items-center justify-center">
        <SelectPrimitive.ItemIndicator>
          <Check className="size-3.5" />
        </SelectPrimitive.ItemIndicator>
      </span>
    </SelectPrimitive.Item>
  )
}

export function SelectSeparator({ className, ...props }: React.ComponentProps<typeof SelectPrimitive.Separator>) {
  return <SelectPrimitive.Separator className={cn('-mx-1 my-1 h-px bg-border', className)} {...props} />
}

export interface SimpleSelectOption<T extends string> {
  value: T
  label: React.ReactNode
  disabled?: boolean
}

/** Convenience wrapper: `<SimpleSelect value onValueChange options=[{value,label}] />`. */
export function SimpleSelect<T extends string>({
  value,
  onValueChange,
  options,
  placeholder,
  size,
  className,
  disabled,
  id,
  'aria-label': ariaLabel,
}: {
  value: T | undefined
  onValueChange: (v: T) => void
  options: SimpleSelectOption<T>[]
  placeholder?: string
  size?: 'sm' | 'md'
  className?: string
  disabled?: boolean
  id?: string
  'aria-label'?: string
}) {
  return (
    <Select value={value} onValueChange={(v) => onValueChange(v as T)} disabled={disabled}>
      <SelectTrigger id={id} size={size} className={className} aria-label={ariaLabel}>
        <SelectValue placeholder={placeholder} />
      </SelectTrigger>
      <SelectContent>
        {options.map((o) => (
          <SelectItem key={o.value} value={o.value} disabled={o.disabled}>
            {o.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}
