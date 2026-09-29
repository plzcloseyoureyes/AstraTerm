import * as React from 'react'
import { Tabs as TabsPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'

export function Tabs({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Root>) {
  return <TabsPrimitive.Root data-slot="tabs" className={cn('flex flex-col gap-2', className)} {...props} />
}

/** `variant="underline"` renders IDE-style tabs, `"pill"` a segmented background. */
export function TabsList({
  className,
  variant = 'underline',
  ...props
}: React.ComponentProps<typeof TabsPrimitive.List> & { variant?: 'underline' | 'pill' }) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      data-variant={variant}
      className={cn(
        'group/tabs inline-flex items-center text-muted-foreground',
        variant === 'underline' ? 'h-8 w-full justify-start gap-1 border-b' : 'h-8 w-fit rounded-md bg-muted p-0.5',
        className,
      )}
      {...props}
    />
  )
}

export function TabsTrigger({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      data-slot="tabs-trigger"
      className={cn(
        'relative inline-flex h-full items-center justify-center gap-1.5 px-2.5 text-sm font-medium whitespace-nowrap outline-none transition-colors',
        'hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40 disabled:pointer-events-none disabled:opacity-50',
        "[&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-3.5",
        // underline variant
        'group-data-[variant=underline]/tabs:data-[state=active]:text-foreground',
        'group-data-[variant=underline]/tabs:after:absolute group-data-[variant=underline]/tabs:after:inset-x-1 group-data-[variant=underline]/tabs:after:-bottom-px group-data-[variant=underline]/tabs:after:h-0.5 group-data-[variant=underline]/tabs:after:rounded-full',
        'group-data-[variant=underline]/tabs:data-[state=active]:after:bg-primary',
        // pill variant
        'group-data-[variant=pill]/tabs:rounded-sm group-data-[variant=pill]/tabs:data-[state=active]:bg-background group-data-[variant=pill]/tabs:data-[state=active]:text-foreground group-data-[variant=pill]/tabs:data-[state=active]:shadow-xs',
        className,
      )}
      {...props}
    />
  )
}

export function TabsContent({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content data-slot="tabs-content" className={cn('flex-1 outline-none', className)} {...props} />
}
