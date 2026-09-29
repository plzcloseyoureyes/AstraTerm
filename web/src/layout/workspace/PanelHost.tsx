import { useEffect, useState } from 'react'
import type { IDockviewPanelProps } from 'dockview-react'
import { PackageX } from 'lucide-react'
import { tabKinds } from '@/app/registry'
import { ErrorBoundary } from '@/components/error-boundary'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { PortalScope, useOwnerBody } from '@/components/ui/portal'
import { LazyBoundary, LoadingPane } from '@/components/ui/spinner'
import { closeTab, useWorkspaceStore, type PanelParams } from '@/stores/workspace'

/**
 * Generic dock panel: resolves the tab kind from the registry and renders its component. Tabs restored before their
 * feature registered show a placeholder and switch over as soon as the kind registers.
 */
export function PanelHost(props: IDockviewPanelProps<PanelParams>) {
  const kind = props.params?.kind
  const def = tabKinds.useItem(kind)
  const tabId = props.api.id
  const focusedPane = useFocusedPane(props.api)

  // Overlays opened from this panel portal into the document it lives in (pop-out windows included).
  const [host, setHost] = useState<HTMLDivElement | null>(null)
  const portalBody = useOwnerBody(host)

  if (!def) return <MissingKind kind={kind ?? 'unknown'} tabId={tabId} />
  const Component = def.component
  return (
    <div ref={setHost} className="nx-panel @container relative h-full w-full overflow-hidden bg-panel text-panel-foreground" data-tab-kind={kind} data-tab-id={tabId}>
      {focusedPane && <span className="pointer-events-none absolute inset-x-0 top-0 z-10 h-0.5 bg-primary/70" aria-hidden />}
      <PortalScope container={portalBody}>
        <ErrorBoundary label={def.title(props.params.params) || kind} resetKey={props.params.params}>
          <LazyBoundary>
            <Component tabId={tabId} params={props.params.params} />
          </LazyBoundary>
        </ErrorBoundary>
      </PortalScope>
    </div>
  )
}

/** With several panes and tabs in the title bar (no group headers), the focused pane gets a thin accent line on top. */
function useFocusedPane(api: IDockviewPanelProps['api']): boolean {
  const split = useWorkspaceStore((s) => s.panes.length > 1)
  const [active, setActive] = useState(api.isGroupActive)
  useEffect(() => {
    setActive(api.isGroupActive)
    const d = api.onDidActiveGroupChange((e) => setActive(e.isActive))
    return () => d.dispose()
  }, [api])
  return split && active && api.group.header.hidden
}

function MissingKind({ kind, tabId }: { kind: string; tabId: string }) {
  // Give lazily-registered features a moment before declaring the kind unavailable.
  const [waited, setWaited] = useState(false)
  useEffect(() => {
    const t = setTimeout(() => setWaited(true), 2500)
    return () => clearTimeout(t)
  }, [])
  if (!waited) return <LoadingPane />
  return (
    <EmptyState
      icon={PackageX}
      title="This tab is not available"
      description={`No feature provides the "${kind}" tab type in this build.`}
      action={
        <Button size="sm" variant="secondary" onClick={() => void closeTab(tabId, { force: true })}>
          Close tab
        </Button>
      }
    />
  )
}
