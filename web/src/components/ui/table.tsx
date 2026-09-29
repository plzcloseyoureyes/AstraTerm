import * as React from 'react'
import { cn } from '@/lib/utils'

/** Simple styled table (for large data use @tanstack/react-table + react-virtual on top of these parts). */
export function Table({ className, containerClassName, ...props }: React.ComponentProps<'table'> & { containerClassName?: string }) {
  return (
    <div data-slot="table-container" className={cn('relative w-full overflow-auto', containerClassName)}>
      <table data-slot="table" className={cn('w-full caption-bottom border-collapse text-base', className)} {...props} />
    </div>
  )
}

export function TableHeader({ className, ...props }: React.ComponentProps<'thead'>) {
  return <thead data-slot="table-header" className={cn('sticky top-0 z-10 bg-panel [&_tr]:border-b', className)} {...props} />
}

export function TableBody({ className, ...props }: React.ComponentProps<'tbody'>) {
  return <tbody data-slot="table-body" className={cn('[&_tr:last-child]:border-0', className)} {...props} />
}

export function TableFooter({ className, ...props }: React.ComponentProps<'tfoot'>) {
  return <tfoot data-slot="table-footer" className={cn('border-t bg-muted/40 font-medium', className)} {...props} />
}

export function TableRow({ className, ...props }: React.ComponentProps<'tr'>) {
  return (
    <tr
      data-slot="table-row"
      className={cn('border-b border-border/70 transition-colors hover:bg-accent/40 data-[state=selected]:bg-primary/10', className)}
      {...props}
    />
  )
}

export function TableHead({ className, ...props }: React.ComponentProps<'th'>) {
  return (
    <th
      data-slot="table-head"
      className={cn('h-7 px-2.5 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground', className)}
      {...props}
    />
  )
}

export function TableCell({ className, ...props }: React.ComponentProps<'td'>) {
  return <td data-slot="table-cell" className={cn('h-8 px-2.5 align-middle', className)} {...props} />
}

export function TableCaption({ className, ...props }: React.ComponentProps<'caption'>) {
  return <caption data-slot="table-caption" className={cn('mt-3 text-sm text-muted-foreground', className)} {...props} />
}
