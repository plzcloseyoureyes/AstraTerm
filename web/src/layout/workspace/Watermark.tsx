import { SquareTerminal } from 'lucide-react'
import { tabKinds } from '@/app/registry'
import { ErrorBoundary } from '@/components/error-boundary'
import { EmptyState } from '@/components/ui/empty-state'
import { LazyBoundary } from '@/components/ui/spinner'

/** Shown when the workspace has no tabs: the Home view (if the home feature is present). */
export function Watermark() {
  const home = tabKinds.useItem('home')
  if (!home) {
    return (
      <div className="flex h-full w-full items-center justify-center bg-panel">
        <EmptyState icon={SquareTerminal} title="No open tabs" description="Use the ribbon or press Ctrl+K to get started." />
      </div>
    )
  }
  const Home = home.component
  return (
    <div className="relative h-full w-full overflow-hidden bg-panel">
      <ErrorBoundary label="Home">
        <LazyBoundary>
          <Home tabId="watermark" params={{ watermark: true }} />
        </LazyBoundary>
      </ErrorBoundary>
    </div>
  )
}
