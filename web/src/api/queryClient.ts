import { QueryClient, keepPreviousData } from '@tanstack/react-query'
import { isApiError } from './client'

/**
 * App-wide react-query client (also used outside React, e.g. by the events socket to patch caches).
 *
 * Loading-state rules (docs/UX.md "Loading states"):
 *   - `placeholderData: keepPreviousData` — when a query's key changes (another folder, page, filter) the previous data
 *     stays on screen until the new data arrives (`isPlaceholderData` is true meanwhile), so views never blank out or
 *     fall back to a skeleton on a refetch. A query whose old data would be misleading under a new key (another host's
 *     processes, another user's details) opts out with `placeholderData: undefined`.
 *   - `refetchOnWindowFocus: false` — focusing the window never refetches (no surprise refresh indicators); views that
 *     need live data poll or listen to events.
 */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      gcTime: 5 * 60_000,
      placeholderData: keepPreviousData,
      refetchOnWindowFocus: false,
      retry: (failureCount, err) => {
        // Never retry client errors (auth, validation, locked vault, not found).
        if (isApiError(err) && err.status >= 400 && err.status < 500) return false
        return failureCount < 2
      },
    },
    mutations: {
      retry: false,
    },
  },
})
