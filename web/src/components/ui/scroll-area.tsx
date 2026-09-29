import * as React from 'react'
import { ScrollArea as ScrollAreaPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'

export function ScrollArea({
  className,
  children,
  viewportRef,
  viewportClassName,
  orientation = 'vertical',
  ...props
}: React.ComponentProps<typeof ScrollAreaPrimitive.Root> & {
  viewportRef?: React.Ref<HTMLDivElement>
  viewportClassName?: string
  orientation?: 'vertical' | 'horizontal' | 'both'
}) {
  return (
    <ScrollAreaPrimitive.Root data-slot="scroll-area" className={cn('relative overflow-hidden', className)} {...props}>
      <ScrollAreaPrimitive.Viewport
        ref={viewportRef}
        data-slot="scroll-area-viewport"
        className={cn('size-full rounded-[inherit] outline-none focus-visible:ring-2 focus-visible:ring-ring/40', viewportClassName)}
      >
        {children}
      </ScrollAreaPrimitive.Viewport>
      {(orientation === 'vertical' || orientation === 'both') && <ScrollBar orientation="vertical" />}
      {(orientation === 'horizontal' || orientation === 'both') && <ScrollBar orientation="horizontal" />}
      <ScrollAreaPrimitive.Corner />
    </ScrollAreaPrimitive.Root>
  )
}

export function ScrollBar({ className, orientation = 'vertical', ...props }: React.ComponentProps<typeof ScrollAreaPrimitive.ScrollAreaScrollbar>) {
  return (
    <ScrollAreaPrimitive.ScrollAreaScrollbar
      data-slot="scroll-area-scrollbar"
      orientation={orientation}
      className={cn(
        'flex touch-none p-px transition-colors select-none',
        orientation === 'vertical' && 'h-full w-2.5 border-l border-l-transparent',
        orientation === 'horizontal' && 'h-2.5 flex-col border-t border-t-transparent',
        className,
      )}
      {...props}
    >
      <ScrollAreaPrimitive.ScrollAreaThumb className="relative flex-1 rounded-full bg-(--scrollbar-thumb) hover:bg-(--scrollbar-thumb-hover)" />
    </ScrollAreaPrimitive.ScrollAreaScrollbar>
  )
}
