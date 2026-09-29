import { Suspense } from 'react'
import { overlays } from '@/app/registry'
import { ErrorBoundary } from '@/components/error-boundary'
import { Button } from '@/components/ui/button'
import { useUIStore } from '@/stores/ui'

/**
 * Mounts every registered overlay (registerOverlay): feature dialogs, drawers and floating panels that must exist
 * regardless of which chrome (sidebar, status bar, tabs) is visible. Each overlay is isolated by its own error boundary
 * so one crashing feature cannot take the shell down. Overlays are unmounted while the screen is locked unless they
 * opt into staying mounted (they then receive `locked` and must hide themselves).
 */
export function OverlayHost() {
  const items = overlays.useList()
  const locked = useUIStore((s) => s.locked)
  return (
    <>
      {items.map((o) => {
        if (locked && !o.keepMountedWhileLocked) return null
        const Component = o.component
        return (
          <ErrorBoundary
            key={o.id}
            label={`Overlay "${o.id}"`}
            fallback={(error, reset) => (
              <div
                role="alert"
                className="fixed right-3 bottom-10 z-[60] flex max-w-sm items-center gap-3 rounded-md border border-destructive/60 bg-background px-3 py-2 text-xs shadow-lg"
              >
                <span className="min-w-0 flex-1 truncate" title={error.message}>
                  <span className="font-medium text-destructive">{o.id}</span> crashed: {error.message}
                </span>
                <Button size="xs" variant="outline" onClick={reset}>
                  Retry
                </Button>
              </div>
            )}
          >
            <Suspense fallback={null}>
              <Component locked={locked} />
            </Suspense>
          </ErrorBoundary>
        )
      })}
    </>
  )
}
