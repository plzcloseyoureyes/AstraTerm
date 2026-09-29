import * as React from 'react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { X } from 'lucide-react'
import { cn } from '@/lib/utils'
import { usePortalContainer } from './portal'

export const Dialog = DialogPrimitive.Root
export const DialogTrigger = DialogPrimitive.Trigger
export const DialogPortal = DialogPrimitive.Portal
export const DialogClose = DialogPrimitive.Close

export function DialogOverlay({ className, ...props }: React.ComponentProps<typeof DialogPrimitive.Overlay>) {
  return (
    <DialogPrimitive.Overlay
      data-slot="dialog-overlay"
      className={cn(
        'fixed inset-0 z-50 bg-overlay backdrop-blur-[2px]',
        'data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0',
        className,
      )}
      {...props}
    />
  )
}

const sizes = {
  sm: 'max-w-sm',
  md: 'max-w-md',
  lg: 'max-w-lg',
  xl: 'max-w-2xl',
  '2xl': 'max-w-4xl',
  full: 'max-w-[min(96vw,1400px)] h-[90vh]',
} as const

export interface DialogContentProps extends React.ComponentProps<typeof DialogPrimitive.Content> {
  size?: keyof typeof sizes
  /** Hide the top-right close button. */
  hideClose?: boolean
  overlayClassName?: string
  /** 'top': anchored near the top of the window instead of centred — for dialogs whose content grows or shrinks
   *  (results, generated links), so the top edge and the controls under it stay put. */
  anchor?: 'center' | 'top'
}

export function DialogContent({ className, children, size = 'md', hideClose, overlayClassName, anchor = 'center', ...props }: DialogContentProps) {
  return (
    <DialogPortal container={usePortalContainer()}>
      <DialogOverlay className={overlayClassName} />
      <DialogPrimitive.Content
        data-slot="dialog-content"
        className={cn(
          'fixed left-[50%] z-50 flex w-[calc(100%-2rem)] translate-x-[-50%] flex-col gap-4',
          anchor === 'top' ? 'top-[max(1rem,10vh)] max-h-[calc(100dvh-max(1rem,10vh)-1rem)]' : 'top-[50%] max-h-[calc(100dvh-2rem)] translate-y-[-50%]',
          'rounded-lg border bg-popover p-5 text-popover-foreground shadow-popover outline-none',
          'data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0',
          'data-[state=closed]:zoom-out-[0.98] data-[state=open]:zoom-in-[0.98] duration-150',
          sizes[size],
          className,
        )}
        {...props}
      >
        {children}
        {!hideClose && (
          <DialogPrimitive.Close
            className="absolute top-3 right-3 rounded-sm p-1 text-muted-foreground opacity-80 outline-none transition hover:bg-accent hover:text-foreground hover:opacity-100 focus-visible:ring-2 focus-visible:ring-ring/50 disabled:pointer-events-none"
            aria-label="Close"
          >
            <X className="size-4" />
          </DialogPrimitive.Close>
        )}
      </DialogPrimitive.Content>
    </DialogPortal>
  )
}

export function DialogHeader({ className, ...props }: React.ComponentProps<'div'>) {
  return <div data-slot="dialog-header" className={cn('flex flex-col gap-1.5 pr-6', className)} {...props} />
}

export function DialogBody({ className, ...props }: React.ComponentProps<'div'>) {
  return <div data-slot="dialog-body" className={cn('-mx-5 min-h-0 flex-1 overflow-y-auto px-5', className)} {...props} />
}

export function DialogFooter({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      data-slot="dialog-footer"
      className={cn('flex flex-col-reverse gap-2 sm:flex-row sm:items-center sm:justify-end', className)}
      {...props}
    />
  )
}

export function DialogTitle({ className, ...props }: React.ComponentProps<typeof DialogPrimitive.Title>) {
  return (
    <DialogPrimitive.Title
      data-slot="dialog-title"
      className={cn('flex items-center gap-2 text-md font-semibold leading-tight', className)}
      {...props}
    />
  )
}

export function DialogDescription({ className, ...props }: React.ComponentProps<typeof DialogPrimitive.Description>) {
  return <DialogPrimitive.Description data-slot="dialog-description" className={cn('text-base text-muted-foreground', className)} {...props} />
}
