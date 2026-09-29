/*
 * The "automation" tab (singleton): scripts, batch runs, scheduled tasks, triggers, logon actions and the run history.
 */
import * as React from 'react'
import { CalendarClock, Code2, History, Layers, LogIn, Zap } from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { ErrorBoundary } from '@/components/error-boundary'
import { LazyBoundary } from '@/components/ui/spinner'
import { cn } from '@/lib/utils'
import { updateTabParams } from '@/stores/workspace'
import { PAGE_TITLES, type AutomationPage, type AutomationTabParams } from './open'

const ScriptsPage = React.lazy(() => import('./ScriptsPage'))
const BatchPage = React.lazy(() => import('./BatchPage'))
const SchedulesPage = React.lazy(() => import('./SchedulesPage'))
const TriggersPage = React.lazy(() => import('./TriggersPage'))
const LogonPage = React.lazy(() => import('./LogonPage'))
const HistoryPage = React.lazy(() => import('./HistoryPage'))

const NAV: { page: AutomationPage; icon: typeof Code2 }[] = [
  { page: 'scripts', icon: Code2 },
  { page: 'batch', icon: Layers },
  { page: 'schedules', icon: CalendarClock },
  { page: 'triggers', icon: Zap },
  { page: 'logon', icon: LogIn },
  { page: 'history', icon: History },
]

export default function AutomationTab({ tabId, params }: TabProps<AutomationTabParams>) {
  const [page, setPage] = React.useState<AutomationPage>(params?.page ?? 'scripts')
  const [historyRef, setHistoryRef] = React.useState<string | undefined>()
  React.useEffect(() => {
    if (params?.page) setPage(params.page)
  }, [params?.page, params?.nonce])
  const go = (p: AutomationPage) => {
    setPage(p)
    updateTabParams<AutomationTabParams>(tabId, { page: p })
  }
  return (
    <div className="flex h-full min-h-0 bg-background">
      <nav className="flex w-44 shrink-0 flex-col gap-0.5 border-r bg-sidebar p-2 @max-2xl:w-12 @max-2xl:px-1" aria-label="Automation">
        {NAV.map(({ page: p, icon: Icon }) => (
          <button
            key={p}
            type="button"
            aria-current={page === p ? 'page' : undefined}
            onClick={() => go(p)}
            title={PAGE_TITLES[p]}
            className={cn(
              'flex items-center gap-2 rounded-md px-2 py-1.5 text-left text-base text-muted-foreground outline-none hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
              page === p && 'bg-sidebar-accent font-medium text-foreground',
            )}
          >
            <Icon className="size-4 shrink-0" aria-hidden />
            <span className="truncate @max-2xl:sr-only">{PAGE_TITLES[p]}</span>
          </button>
        ))}
      </nav>
      <main className="relative min-w-0 flex-1">
        <ErrorBoundary label={PAGE_TITLES[page]} resetKey={page}>
          <LazyBoundary className="bg-background">
            {page === 'scripts' && <ScriptsPage scriptId={params?.scriptId} runId={params?.runId} />}
            {page === 'batch' && <BatchPage key={params?.nonce} connectionIds={params?.connectionIds} />}
            {page === 'schedules' && (
              <SchedulesPage
                onShowRuns={(refId) => {
                  setHistoryRef(refId)
                  go('history')
                }}
              />
            )}
            {page === 'triggers' && <TriggersPage />}
            {page === 'logon' && <LogonPage connectionId={params?.connectionId} />}
            {page === 'history' && <HistoryPage runId={params?.page === 'history' ? params?.runId : undefined} refId={historyRef} />}
          </LazyBoundary>
        </ErrorBoundary>
      </main>
    </div>
  )
}
