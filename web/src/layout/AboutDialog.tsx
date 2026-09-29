import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { BrandMark } from '@/components/brand-mark'
import { Badge } from '@/components/ui/badge'
import { useAuthState } from '@/stores/auth'
import { setAboutOpen, useUIStore } from '@/stores/ui'

export function AboutDialog() {
  const open = useUIStore((s) => s.aboutOpen)
  const st = useAuthState()
  const features = st?.features ?? {}
  return (
    <Dialog open={open} onOpenChange={setAboutOpen}>
      <DialogContent size="sm">
        <DialogHeader className="items-center text-center">
          <BrandMark className="mb-1 size-12" />
          <DialogTitle className="text-lg">AstraTerm</DialogTitle>
          <DialogDescription>An organized remote-management workspace: SSH, SFTP, RDP, VNC, tunnels and more.</DialogDescription>
        </DialogHeader>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-base">
          <dt className="text-muted-foreground">Version</dt>
          <dd className="font-mono">{st?.version || 'dev'}</dd>
          <dt className="text-muted-foreground">Mode</dt>
          <dd>{st?.mode === 'server' ? 'Server (multi-user)' : 'Desktop'}</dd>
          <dt className="text-muted-foreground">Integrations</dt>
          <dd className="flex flex-wrap gap-1">
            {Object.entries(features).map(([k, v]) => (
              <Badge key={k} variant={v ? 'success' : 'outline'}>
                {k}
              </Badge>
            ))}
            {!Object.keys(features).length && <span className="text-muted-foreground">—</span>}
          </dd>
        </dl>
      </DialogContent>
    </Dialog>
  )
}
