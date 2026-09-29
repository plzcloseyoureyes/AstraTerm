import * as React from 'react'
import { cn } from '@/lib/utils'
import { inputBase } from './input'

export interface TextareaProps extends React.ComponentProps<'textarea'> {
  /** Use the monospace font (scripts, keys, notes with code). */
  mono?: boolean
}

export function Textarea({ className, mono, ...props }: TextareaProps) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(inputBase, 'min-h-16 py-1.5 leading-relaxed', mono && 'font-mono text-sm', className)}
      {...props}
    />
  )
}
