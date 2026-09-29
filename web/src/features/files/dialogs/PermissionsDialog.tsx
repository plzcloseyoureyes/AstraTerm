/*
 * Permissions (FILE-4): rwx grid ↔ octal (with setuid / setgid / sticky), recursive application (optionally "execute
 * for folders only", capital X), and owner / group (names or numeric ids) when the file system supports chown.
 * Several items with different modes show mixed (indeterminate) boxes; untouched mixed bits keep each item's value.
 */
import { useId, useMemo, useState } from 'react'
import { Shield } from 'lucide-react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { cn, errorMessage } from '@/lib/utils'
import { invalidateDir } from '../actions'
import { fsApi } from '../api'
import { describeSelection, isDirLike, modeToOctal, octalToMode, permBits, permString, S_ISGID, S_ISUID, S_ISVTX } from '../format'
import { withFs } from '../fsHandles'
import { dirname } from '../paths'
import type { FsContext } from '../types'

type Tri = boolean | 'mixed'

const WHO = [
  { label: 'Owner', shift: 6 },
  { label: 'Group', shift: 3 },
  { label: 'Others', shift: 0 },
] as const
const WHAT = [
  { label: 'Read', bit: 4 },
  { label: 'Write', bit: 2 },
  { label: 'Execute', bit: 1 },
] as const
const SPECIAL = [
  { label: 'Set user ID', bit: S_ISUID, hint: 'setuid: run as the file owner' },
  { label: 'Set group ID', bit: S_ISGID, hint: 'setgid: run as the group / new files inherit the group' },
  { label: 'Sticky', bit: S_ISVTX, hint: 'only owners may delete entries (e.g. /tmp)' },
] as const

const ALL_BITS = [...WHO.flatMap((w) => WHAT.map((x) => x.bit << w.shift)), ...SPECIAL.map((s) => s.bit)]

export function PermissionsDialog({ ctx, entries, onClose }: { ctx: FsContext; entries: FileEntry[]; onClose: () => void }) {
  const initial = useMemo(() => {
    const out: Record<number, Tri> = {}
    for (const bit of ALL_BITS) {
      const on = entries.filter((e) => (permBits(e.mode) & bit) !== 0).length
      out[bit] = on === 0 ? false : on === entries.length ? true : 'mixed'
    }
    return out
  }, [entries])
  const [bits, setBits] = useState<Record<number, Tri>>(initial)
  const [recursive, setRecursive] = useState(false)
  const [dirsOnlyX, setDirsOnlyX] = useState(true)
  const owners = new Set(entries.map((e) => String(e.owner ?? e.uid ?? '')))
  const groups = new Set(entries.map((e) => String(e.group ?? e.gid ?? '')))
  const [owner, setOwner] = useState(owners.size === 1 ? [...owners][0] : '')
  const [group, setGroup] = useState(groups.size === 1 ? [...groups][0] : '')
  const [busy, setBusy] = useState(false)
  const ids = useId()
  const hasDir = entries.some((e) => e.type === 'dir')
  const caps = ctx.handle.capabilities
  const mixed = Object.values(bits).some((v) => v === 'mixed')
  const mode = ALL_BITS.reduce((m, bit) => (bits[bit] === true ? m | bit : m), 0)
  const [octalText, setOctalText] = useState(mixed ? '' : modeToOctal(mode))
  const [octalError, setOctalError] = useState(false)

  const setBit = (bit: number, v: boolean) => {
    const next = { ...bits, [bit]: v }
    setBits(next)
    if (!Object.values(next).some((x) => x === 'mixed')) setOctalText(modeToOctal(ALL_BITS.reduce((m, b) => (next[b] === true ? m | b : m), 0)))
  }
  const onOctal = (text: string) => {
    setOctalText(text)
    const m = octalToMode(text)
    setOctalError(m === null && text.trim() !== '')
    if (m === null) return
    const next: Record<number, Tri> = {}
    for (const b of ALL_BITS) next[b] = (m & b) !== 0
    setBits(next)
  }

  const ownerChanged = caps.chown && ((owner.trim() && !(owners.size === 1 && owners.has(owner.trim()))) || (group.trim() && !(groups.size === 1 && groups.has(group.trim()))))

  /** Symbolic mode for recursive application with "X" (execute only for folders / already executable files). */
  const symbolic = () => {
    const part = (shift: number, who: string) => {
      let s = ''
      if (bits[4 << shift] === true) s += 'r'
      if (bits[2 << shift] === true) s += 'w'
      if (bits[1 << shift] === true) s += dirsOnlyX ? 'X' : 'x'
      if (shift === 6 && bits[S_ISUID] === true) s += 's'
      if (shift === 3 && bits[S_ISGID] === true) s += 's'
      if (shift === 0 && bits[S_ISVTX] === true) s += 't'
      return `${who}=${s}`
    }
    return [part(6, 'u'), part(3, 'g'), part(0, 'o')].join(',')
  }

  const apply = async () => {
    if (octalError) return
    setBusy(true)
    try {
      const changedBits = ALL_BITS.filter((b) => bits[b] !== initial[b] && bits[b] !== 'mixed')
      const modeChanged = changedBits.length > 0 || (recursive && !mixed)
      if (modeChanged && caps.chmod) {
        if (recursive && hasDir) {
          if (mixed) throw new Error('Resolve the mixed boxes before applying recursively')
          const m = dirsOnlyX && entries.some(isDirLike) ? symbolic() : mode
          await withFs(ctx.key, (id) =>
            fsApi.chmod(
              id,
              entries.map((e) => e.path),
              m,
              true,
            ),
          )
        } else {
          // Keep each item's mixed bits: group items by their resulting mode.
          const groupsByMode = new Map<number, string[]>()
          for (const e of entries) {
            let m = permBits(e.mode)
            for (const b of ALL_BITS) {
              if (bits[b] === true) m |= b
              else if (bits[b] === false) m &= ~b
            }
            const list = groupsByMode.get(m) ?? []
            list.push(e.path)
            groupsByMode.set(m, list)
          }
          for (const [m, paths] of groupsByMode) await withFs(ctx.key, (id) => fsApi.chmod(id, paths, m, false))
        }
      }
      if (ownerChanged) {
        const o = owner.trim()
        const g = group.trim()
        const who: { uid?: number; gid?: number; owner?: string; group?: string } = {}
        if (o && !(owners.size === 1 && owners.has(o))) {
          if (/^\d+$/.test(o)) who.uid = Number(o)
          else who.owner = o
        }
        if (g && !(groups.size === 1 && groups.has(g))) {
          if (/^\d+$/.test(g)) who.gid = Number(g)
          else who.group = g
        }
        await withFs(ctx.key, (id) =>
          fsApi.chown(
            id,
            entries.map((e) => e.path),
            who,
            recursive && hasDir,
          ),
        )
      }
      toast.success(`Updated ${describeSelection(entries)}`)
      onClose()
    } catch (err) {
      toast.error('Could not change the permissions', { description: errorMessage(err) })
    } finally {
      for (const d of new Set(entries.map((e) => dirname(e.path)))) invalidateDir(ctx, d)
      setBusy(false)
    }
  }

  const preview = mixed ? '' : permString(mode, entries.length === 1 ? entries[0].type : 'file')
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="md">
        <DialogHeader>
          <DialogTitle>
            <Shield className="size-4 text-muted-foreground" /> Permissions of {describeSelection(entries)}
          </DialogTitle>
          <DialogDescription className="truncate">{entries.length === 1 ? entries[0].path : dirname(entries[0].path)}</DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-4">
          <div role="grid" aria-label="Permission bits" className="grid grid-cols-[6rem_repeat(3,minmax(0,1fr))] items-center gap-y-1.5 rounded-md border p-3">
            <span />
            {WHAT.map((w) => (
              <span key={w.label} role="columnheader" className="text-center text-xs font-medium text-muted-foreground">
                {w.label}
              </span>
            ))}
            {WHO.map((who) => (
              <div key={who.label} role="row" className="contents">
                <span role="rowheader" className="text-sm">
                  {who.label}
                </span>
                {WHAT.map((what) => {
                  const bit = what.bit << who.shift
                  const v = bits[bit]
                  return (
                    <span key={what.label} role="gridcell" className="flex justify-center">
                      <Checkbox
                        aria-label={`${who.label} ${what.label.toLowerCase()}`}
                        checked={v === 'mixed' ? 'indeterminate' : v}
                        disabled={!caps.chmod}
                        onCheckedChange={(c) => setBit(bit, c === true)}
                      />
                    </span>
                  )
                })}
              </div>
            ))}
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-2">
            {SPECIAL.map((s) => {
              const v = bits[s.bit]
              return (
                <label key={s.label} className="flex items-center gap-1.5 text-sm" title={s.hint}>
                  <Checkbox checked={v === 'mixed' ? 'indeterminate' : v} disabled={!caps.chmod} onCheckedChange={(c) => setBit(s.bit, c === true)} />
                  {s.label}
                </label>
              )
            })}
          </div>
          <div className="flex items-end gap-3">
            <div className="grid gap-1">
              <label htmlFor={`${ids}-octal`} className="text-sm font-medium">
                Octal
              </label>
              <Input
                id={`${ids}-octal`}
                value={octalText}
                placeholder={mixed ? 'mixed' : '0755'}
                onChange={(e) => onOctal(e.target.value)}
                className={cn('w-24 font-mono', octalError && 'border-destructive')}
                aria-invalid={octalError || undefined}
                disabled={!caps.chmod}
                inputMode="numeric"
                maxLength={4}
              />
            </div>
            <div className="pb-1.5 font-mono text-sm text-muted-foreground" aria-label="Symbolic permissions">
              {preview}
            </div>
          </div>
          {hasDir && (
            <div className="grid gap-1.5 rounded-md border bg-muted/30 p-2.5">
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={recursive} onCheckedChange={(v) => setRecursive(v === true)} />
                Apply to everything inside the folder{entries.filter((e) => e.type === 'dir').length > 1 ? 's' : ''}
              </label>
              {recursive && (
                <label className="flex items-center gap-2 pl-6 text-sm text-muted-foreground">
                  <Checkbox checked={dirsOnlyX} onCheckedChange={(v) => setDirsOnlyX(v === true)} />
                  Execute only for folders (and files that are already executable)
                </label>
              )}
            </div>
          )}
          {caps.chown && (
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-1">
                <label htmlFor={`${ids}-owner`} className="text-sm font-medium">
                  Owner
                </label>
                <Input id={`${ids}-owner`} value={owner} placeholder={owners.size > 1 ? 'mixed' : 'user or uid'} onChange={(e) => setOwner(e.target.value)} className="font-mono" />
              </div>
              <div className="grid gap-1">
                <label htmlFor={`${ids}-group`} className="text-sm font-medium">
                  Group
                </label>
                <Input id={`${ids}-group`} value={group} placeholder={groups.size > 1 ? 'mixed' : 'group or gid'} onChange={(e) => setGroup(e.target.value)} className="font-mono" />
              </div>
            </div>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => void apply()} loading={busy} disabled={octalError || (recursive && mixed)}>
            Apply
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
