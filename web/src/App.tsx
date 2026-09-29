import { lazy, Suspense } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { queryClient } from '@/api/queryClient'
import { AuthGate } from '@/auth/AuthGate'
import { ErrorBoundary } from '@/components/error-boundary'
import { Toaster } from '@/components/ui/toaster'
import { TooltipProvider } from '@/components/ui/tooltip'

// Public live-session share links (/share/<token>, recording feature, SPEC §9 "recording"): no login, no app shell.
const ShareViewer = lazy(() => import('@/features/recordings/ShareViewer'))
const isShareLink = location.pathname.startsWith('/share/')

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <ErrorBoundary label="NexTerm">
          {isShareLink ? (
            <Suspense fallback={null}>
              <ShareViewer />
            </Suspense>
          ) : (
            <AuthGate />
          )}
        </ErrorBoundary>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>
  )
}
