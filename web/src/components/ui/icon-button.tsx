import * as React from 'react'
import type { IconType } from '@/app/registry'
import type { DelayedFlagOptions } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { Button, type ButtonProps } from './button'
import { BusyIcon } from './spinner'
import { Tooltip } from './tooltip'

export interface IconButtonProps extends Omit<ButtonProps, 'children' | 'size'> {
  icon: IconType
  /** Accessible label, also used as tooltip. */
  label: string
  /** Keybinding shown in the tooltip (tinykeys syntax). */
  shortcut?: string
  size?: 'xs' | 'sm' | 'md'
  tooltipSide?: 'top' | 'right' | 'bottom' | 'left'
  /** Toggle state (renders pressed styling + aria-pressed). */
  active?: boolean
  iconClassName?: string
  /** A slow user-initiated operation is running: the icon spins (after 300 ms, for ≥ 600 ms). Not for polling. */
  busy?: boolean
  /** Timing of the busy spin: a DELAY_PRESETS entry (default EXPLICIT_WAIT; NAVIGATION for view refreshes). */
  busyTiming?: DelayedFlagOptions
}

const sizeMap = { xs: 'icon-xs', sm: 'icon-sm', md: 'icon' } as const

/** Icon-only button with tooltip + aria-label. */
export function IconButton({
  icon: Icon,
  label,
  shortcut,
  size = 'sm',
  variant = 'ghost',
  tooltipSide,
  active,
  className,
  iconClassName,
  busy,
  busyTiming,
  ...props
}: IconButtonProps) {
  return (
    <Tooltip content={label} shortcut={shortcut} side={tooltipSide}>
      <Button
        variant={variant}
        size={sizeMap[size]}
        aria-label={label}
        aria-pressed={active === undefined ? undefined : active}
        className={cn('text-muted-foreground hover:text-foreground', active && 'bg-accent text-foreground', className)}
        {...props}
      >
        {busy === undefined ? <Icon className={iconClassName} /> : <BusyIcon icon={Icon} busy={busy} timing={busyTiming} className={iconClassName} />}
      </Button>
    </Tooltip>
  )
}
