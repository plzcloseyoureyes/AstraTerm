/*
 * The singleton "keys" tab: SSH keys · Identities · Known hosts · Agent. The active sub-tab lives in the tab params
 * (persisted with the layout, and set by the keys.* commands).
 */
import { KeyRound, ShieldCheck, TerminalSquare, UserRound } from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { cn } from '@/lib/utils'
import { updateTabParams, useIsTabVisible } from '@/stores/workspace'
import { AgentPanel } from './AgentPanel'
import { useAgentStatus, useKeys } from './api'
import { IdentitiesPanel } from './IdentitiesPanel'
import { KeysPanel } from './KeysPanel'
import { KnownHostsPanel } from './KnownHostsPanel'
import type { KeysTab, KeysTabParams } from './types'

const TABS: KeysTab[] = ['keys', 'identities', 'knownHosts', 'agent']

export default function KeysView({ tabId, params }: TabProps<KeysTabParams>) {
  const tab: KeysTab = params?.tab && TABS.includes(params.tab) ? params.tab : 'keys'
  const visible = useIsTabVisible(tabId)
  const keys = useKeys()
  const agent = useAgentStatus(false)
  const running = !!agent.data?.running && agent.data.owner

  return (
    <div className="@container flex h-full min-h-0 flex-col bg-background">
      <Tabs value={tab} onValueChange={(v) => updateTabParams<KeysTabParams>(tabId, { tab: v as KeysTab })} className="flex min-h-0 flex-1 flex-col gap-0">
        <TabsList className="shrink-0 px-2" aria-label="Keys sections">
          <TabsTrigger value="keys">
            <KeyRound /> SSH keys
            {!!keys.data?.length && (
              <Badge variant="secondary" className="ml-0.5 px-1 tabular-nums">
                {keys.data.length}
              </Badge>
            )}
          </TabsTrigger>
          <TabsTrigger value="identities">
            <UserRound /> Identities
          </TabsTrigger>
          <TabsTrigger value="knownHosts">
            <ShieldCheck /> Known hosts
          </TabsTrigger>
          <TabsTrigger value="agent">
            <TerminalSquare /> Agent
            <span aria-label={running ? 'running' : 'stopped'} className={cn('size-1.5 rounded-full', running ? 'bg-success' : 'bg-muted-foreground/40')} />
          </TabsTrigger>
        </TabsList>
        <TabsContent value="keys" className="min-h-0">
          <KeysPanel />
        </TabsContent>
        <TabsContent value="identities" className="min-h-0">
          <IdentitiesPanel />
        </TabsContent>
        <TabsContent value="knownHosts" className="min-h-0">
          <KnownHostsPanel />
        </TabsContent>
        <TabsContent value="agent" className="min-h-0">
          <AgentPanel visible={visible && tab === 'agent'} />
        </TabsContent>
      </Tabs>
    </div>
  )
}
