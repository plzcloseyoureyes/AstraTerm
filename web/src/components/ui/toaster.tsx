import { useEffect, useState } from 'react'
import { Toaster as Sonner } from 'sonner'
import { CircleAlert, CircleCheck, Info, LoaderCircle, TriangleAlert } from 'lucide-react'
import { isDarkTheme, onThemeChange } from '@/lib/theme'

/** App toaster (sonner) themed with the design tokens. Use `toast` from 'sonner' anywhere. */
export function Toaster() {
  const [dark, setDark] = useState(isDarkTheme())
  useEffect(() => onThemeChange(setDark), [])
  return (
    <Sonner
      theme={dark ? 'dark' : 'light'}
      position="bottom-right"
      closeButton
      visibleToasts={5}
      offset={{ bottom: 36, right: 16 }}
      icons={{
        success: <CircleCheck className="size-4 text-success" />,
        info: <Info className="size-4 text-info" />,
        warning: <TriangleAlert className="size-4 text-warning" />,
        error: <CircleAlert className="size-4 text-destructive" />,
        loading: <LoaderCircle className="size-4 animate-spin text-muted-foreground" />,
      }}
      toastOptions={{
        classNames: {
          toast: 'group bg-popover! text-popover-foreground! border-border! shadow-popover! rounded-lg! text-base! font-sans! gap-2.5! py-3!',
          title: 'font-medium!',
          description: 'text-muted-foreground! text-sm!',
          actionButton: 'bg-primary! text-primary-foreground! rounded-md! text-sm!',
          cancelButton: 'bg-secondary! text-secondary-foreground! rounded-md! text-sm!',
          closeButton: 'bg-popover! border-border! text-muted-foreground! hover:text-foreground!',
        },
      }}
    />
  )
}
