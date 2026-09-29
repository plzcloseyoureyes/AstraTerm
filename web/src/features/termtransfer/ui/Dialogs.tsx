/*
 * Small dialogs of the term-transfer feature: existing names in a drop destination, and text the automation guard
 * flagged as dangerous before a paced "send file".
 */
import { useId, useState } from 'react'
import { FolderInput, ShieldAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { useTransferStore, type ConflictRequest, type DangerRequest } from '../store'

export function ConflictDialog() {
  const req = useTransferStore((s) => s.conflict)
  return (
    <Dialog open={!!req} onOpenChange={(o) => !o && req?.resolve(null, false)}>
      {req && <ConflictBody key={req.id} req={req} />}
    </Dialog>
  )
}

function ConflictBody({ req }: { req: ConflictRequest }) {
  const [remember, setRemember] = useState(false)
  const id = useId()
  const n = req.names.length
  return (
    <DialogContent size="md" hideClose>
      <DialogHeader>
        <DialogTitle>
          <FolderInput className="size-4 text-muted-foreground" />
          {n === 1 ? `“${req.names[0]}” already exists` : `${n} items already exist`}
        </DialogTitle>
        <DialogDescription>
          in <span className="font-mono">{req.dest}</span>
        </DialogDescription>
      </DialogHeader>
      {n > 1 && (
        <DialogBody>
          <ul className="max-h-40 overflow-y-auto rounded-md border bg-muted/30 px-3 py-2 font-mono text-sm">
            {req.names.slice(0, 200).map((name) => (
              <li key={name} className="truncate">
                {name}
              </li>
            ))}
            {n > 200 && <li className="text-muted-foreground">… and {n - 200} more</li>}
          </ul>
        </DialogBody>
      )}
      <div className="flex items-center gap-2">
        <Checkbox id={id} checked={remember} onCheckedChange={(v) => setRemember(v === true)} />
        <label htmlFor={id} className="text-base select-none">
          Always do this for drops on a terminal
        </label>
      </div>
      <DialogFooter>
        <Button variant="ghost" onClick={() => req.resolve(null, false)}>
          Cancel
        </Button>
        <Button variant="secondary" onClick={() => req.resolve('skip', remember)}>
          Skip
        </Button>
        <Button variant="secondary" onClick={() => req.resolve('rename', remember)}>
          Keep both
        </Button>
        <Button variant="destructive" onClick={() => req.resolve('overwrite', remember)} autoFocus>
          Replace
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}

export function DangerDialog() {
  const req = useTransferStore((s) => s.danger)
  return (
    <Dialog open={!!req} onOpenChange={(o) => !o && req?.resolve(false)}>
      {req && <DangerBody key={req.id} req={req} />}
    </Dialog>
  )
}

function DangerBody({ req }: { req: DangerRequest }) {
  return (
    <DialogContent size="lg" hideClose>
      <DialogHeader>
        <DialogTitle>
          <ShieldAlert className="size-4 text-destructive" />
          {req.title}
        </DialogTitle>
        <DialogDescription>The file contains commands the dangerous-command guard flags. Review them before they are typed into the session.</DialogDescription>
      </DialogHeader>
      <DialogBody>
        <ul className="grid max-h-60 gap-2 overflow-y-auto">
          {req.matches.map((m, i) => (
            <li key={i} className="rounded-md border bg-muted/30 px-3 py-2">
              <div className="text-sm font-medium">{m.message || m.rule || 'Flagged command'}</div>
              {m.line && <pre className="mt-1 overflow-x-auto font-mono text-xs whitespace-pre-wrap break-all text-muted-foreground">{m.line}</pre>}
            </li>
          ))}
        </ul>
      </DialogBody>
      <DialogFooter>
        <Button variant="secondary" onClick={() => req.resolve(false)} autoFocus>
          Don’t send
        </Button>
        <Button variant="destructive" onClick={() => req.resolve(true)}>
          Send anyway
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}
