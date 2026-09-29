/* "Web proxies" dialog: the user's live proxies (web pages and Xpra apps) with their route, activity and a close button. */
import { AppWindow, Globe, Network, Plus, X } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { errorMessage, formatRelativeTime } from '@/lib/utils'
import { findTabs, focusTab } from '@/stores/workspace'
import { closeProxy, proxyEntry, useProxies } from './api'
import { displayUrl, viaText } from './model'
import { openProxyTab } from './open'
import { openWebDialog } from './store'
import type { ProxyInfo, WebTabParams } from './types'

async function show(p: ProxyInfo, done: () => void) {
  const tab = findTabs((t) => t.kind === 'web' && (t.params as WebTabParams)?.proxyId === p.id)[0]
  if (tab) {
    focusTab(tab.id)
    done()
    return
  }
  try {
    const info = await proxyEntry(p.id)
    openProxyTab(info, p.spec)
    done()
  } catch (err) {
    toast.error('Could not open the proxy', { description: errorMessage(err) })
  }
}

export function ManagerDialog({ onClose }: { onClose: () => void }) {
  const q = useProxies()
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <DialogHeader>
          <DialogTitle>
            <Network className="size-4 text-primary" /> Web proxies
          </DialogTitle>
          <DialogDescription>Web pages and X11 applications Termstead currently proxies for you. Idle proxies close by themselves.</DialogDescription>
        </DialogHeader>
        <DialogBody className="min-h-40">
          <QueryState
            query={q}
            skeleton={<SkeletonRows rows={3} rowHeight={52} />}
            errorTitle="Could not load the proxies"
            isEmpty={(list) => !list.length}
            empty={
              <EmptyState
                size="sm"
                icon={Globe}
                title="No open web proxies"
                description="Open a web page reachable from Termstead or from one of your SSH servers."
                action={
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => {
                      onClose()
                      openWebDialog()
                    }}
                  >
                    <Plus /> Open web page
                  </Button>
                }
              />
            }
          >
            {(list) => (
              <ul className="grid gap-1" aria-label="Open proxies">
                {[...list]
                  .sort((a, b) => b.lastUsedAt.localeCompare(a.lastUsedAt))
                  .map((p) => (
                    <ProxyRow key={p.id} p={p} onShow={() => void show(p, onClose)} />
                  ))}
              </ul>
            )}
          </QueryState>
        </DialogBody>
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ProxyRow({ p, onShow }: { p: ProxyInfo; onShow: () => void }) {
  const Icon = p.kind === 'xpra' ? AppWindow : Globe
  return (
    <li className="group flex items-center gap-3 rounded-md border px-3 py-2 transition-colors duration-150 hover:bg-accent/30">
      <Icon className="size-4 shrink-0 text-muted-foreground" />
      <button type="button" className="grid min-w-0 flex-1 text-left" onClick={onShow}>
        <span className="truncate font-medium">{p.title}</span>
        <span className="truncate font-mono text-xs text-muted-foreground">
          {p.kind === 'xpra' ? (p.extra?.command ?? '') : displayUrl(p.target)} · {viaText(p.via)}
        </span>
      </button>
      {p.active > 0 ? (
        <Badge variant="success">active</Badge>
      ) : (
        <span className="text-xs whitespace-nowrap text-muted-foreground tabular-nums">used {formatRelativeTime(p.lastUsedAt)}</span>
      )}
      <IconButton
        icon={X}
        label={p.kind === 'xpra' ? 'Stop application' : 'Close proxy'}
        onClick={() => closeProxy(p.id).catch((err) => toast.error('Could not close the proxy', { description: errorMessage(err) }))}
      />
    </li>
  )
}
