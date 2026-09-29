/*
 * "Items already exist" question for uploads and copies: overwrite, skip, keep both (rename) — optionally remembered.
 */
import { useId, useState } from 'react'
import { CopyCheck } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { filesSettings } from '../settings'
import type { ConflictPolicy } from '../types'

export function ConflictDialog({
  names,
  destLabel,
  verb,
  onDone,
}: {
  names: string[]
  destLabel: string
  verb: 'upload' | 'copy' | 'move'
  onDone: (choice: ConflictPolicy | null) => void
}) {
  const [remember, setRemember] = useState(false)
  const id = useId()
  const finish = (c: ConflictPolicy | null) => {
    if (c && remember) filesSettings.set({ conflictPolicy: c })
    onDone(c)
  }
  const shown = names.slice(0, 6)
  return (
    <Dialog open onOpenChange={(o) => !o && finish(null)}>
      <DialogContent size="md" onOpenAutoFocus={(e) => e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>
            <CopyCheck className="size-4 text-warning" />
            {names.length === 1 ? `“${names[0]}” already exists` : `${names.length} items already exist`}
          </DialogTitle>
          <DialogDescription>
            The {verb === 'upload' ? 'upload' : verb === 'move' ? 'move' : 'copy'} target <span className="font-mono break-all">{destLabel}</span> already contains
            {names.length === 1 ? ' an item with this name.' : ' items with these names:'}
          </DialogDescription>
        </DialogHeader>
        {names.length > 1 && (
          <ul className="max-h-32 overflow-y-auto rounded-md border bg-muted/40 px-3 py-1.5 font-mono text-xs">
            {shown.map((n) => (
              <li key={n} className="truncate">
                {n}
              </li>
            ))}
            {names.length > shown.length && <li className="text-muted-foreground">…and {names.length - shown.length} more</li>}
          </ul>
        )}
        <div className="flex items-center gap-2">
          <Checkbox id={id} checked={remember} onCheckedChange={(v) => setRemember(v === true)} />
          <label htmlFor={id} className="text-sm select-none">
            Remember my choice (Settings → Files & SFTP)
          </label>
        </div>
        <DialogFooter className="flex-wrap">
          <Button variant="secondary" onClick={() => finish(null)}>
            Cancel
          </Button>
          <Button variant="secondary" onClick={() => finish('skip')}>
            Skip existing
          </Button>
          <Button variant="secondary" onClick={() => finish('rename')}>
            Keep both
          </Button>
          <Button variant="destructive" autoFocus onClick={() => finish('overwrite')}>
            Overwrite
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
