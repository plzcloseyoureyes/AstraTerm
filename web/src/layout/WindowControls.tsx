/*
 * Minimize / maximize / close for the Windows desktop app, whose window has no system frame (lib/desktop.ts).
 * Styled like the system caption buttons; like those, they stay out of the Tab order (Alt+F4 and Win+arrows work).
 */
import { useEffect, useState } from 'react'
import { Copy, Minus, Square, X } from 'lucide-react'
import { windowCommand } from '@/lib/desktop'
import { cn } from '@/lib/utils'

const button = 'flex h-full w-11 items-center justify-center text-muted-foreground outline-none hover:bg-foreground/10 hover:text-foreground'

export function WindowControls() {
  const [maximized, setMaximized] = useState(false)

  useEffect(() => {
    const sync = () => void windowCommand<boolean>('is_maximized').then(setMaximized, () => {})
    sync()
    window.addEventListener('resize', sync)
    return () => window.removeEventListener('resize', sync)
  }, [])

  const run = (cmd: string) => () => void windowCommand(cmd).catch(() => {})
  return (
    <div className="ml-1.5 flex h-full shrink-0 self-stretch">
      <button type="button" tabIndex={-1} aria-label="Minimize" className={button} onClick={run('minimize')}>
        <Minus className="size-4" strokeWidth={1.5} />
      </button>
      <button type="button" tabIndex={-1} aria-label={maximized ? 'Restore' : 'Maximize'} className={button} onClick={run('toggle_maximize')}>
        {maximized ? <Copy className="size-3.5 -scale-x-100" strokeWidth={1.5} /> : <Square className="size-3.5" strokeWidth={1.5} />}
      </button>
      <button type="button" tabIndex={-1} aria-label="Close" className={cn(button, 'hover:bg-destructive hover:text-destructive-foreground')} onClick={run('close')}>
        <X className="size-4" strokeWidth={1.5} />
      </button>
    </div>
  )
}
