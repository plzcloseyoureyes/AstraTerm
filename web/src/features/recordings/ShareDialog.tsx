/*
 * "Share session" dialog (MU-18, command recordings.share {sessionId}): create read-only / interactive links with an
 * expiry, copy them (or show a QR code), watch who is connected and revoke links. Opened from the terminal and tab
 * context menus.
 */
import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { QRCodeSVG } from 'qrcode.react'
import { AlertTriangle, Check, Copy, Eye, Keyboard, Link2, Pause, Play, QrCode, Share2, Trash2, Users } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { SwitchField } from '@/components/ui/switch'
import { Tooltip } from '@/components/ui/tooltip'
import { events } from '@/lib/events'
import { useNow } from '@/lib/hooks'
import { cn, copyText, errorMessage, formatRelativeTime, plural } from '@/lib/utils'
import { absoluteShareUrl, createShare, recKeys, revokeSessionShares, revokeShare, setShareInput, usePolicy, useShares } from './api'
import { recordingsSettings } from './settings'
import { closeShareDialog, useRecordingsUI } from './store'
import type { ShareView } from './types'

const EXPIRY: { value: string; label: string; sec: number }[] = [
  { value: '900', label: '15 minutes', sec: 900 },
  { value: '3600', label: '1 hour', sec: 3600 },
  { value: '14400', label: '4 hours', sec: 14400 },
  { value: '28800', label: '8 hours', sec: 28800 },
  { value: '86400', label: '24 hours', sec: 86400 },
  { value: '259200', label: '3 days', sec: 259200 },
  { value: '604800', label: '7 days', sec: 604800 },
  { value: '2592000', label: '30 days', sec: 2592000 },
]

export function ShareDialog({ locked }: { locked: boolean }) {
  const sessionId = useRecordingsUI((s) => s.shareSessionId)
  const title = useRecordingsUI((s) => s.shareTitle)
  const open = !!sessionId && !locked
  return (
    <Dialog open={open} onOpenChange={(v) => !v && closeShareDialog()}>
      {/* Anchored near the top: creating and revoking links grows / shrinks the list without re-centring the dialog. */}
      <DialogContent size="lg" className="top-[10vh] max-h-[80vh] translate-y-0">
        {sessionId && <ShareDialogBody sessionId={sessionId} title={title} />}
      </DialogContent>
    </Dialog>
  )
}

function ShareDialogBody({ sessionId, title }: { sessionId: string; title?: string }) {
  const qc = useQueryClient()
  const defaults = recordingsSettings.get()
  const policy = usePolicy().data
  const shares = useShares(sessionId)
  const [mode, setMode] = useState<'read' | 'write'>(defaults.shareMode)
  const [expiry, setExpiry] = useState(String(defaults.shareExpiresInSec))
  const [requireLogin, setRequireLogin] = useState(defaults.shareRequireLogin)
  const [inputPaused, setInputPaused] = useState(defaults.shareInputPaused)
  const [label, setLabel] = useState('')
  const [creating, setCreating] = useState(false)
  const [fresh, setFresh] = useState<string | null>(null)

  const maxSec = (policy?.shareMaxHours ?? 168) * 3600
  const options = useMemo(() => EXPIRY.filter((o) => o.sec <= maxSec), [maxSec])
  const writeAllowed = policy?.shareWriteEnabled !== false
  const disabled = policy?.shareEnabled === false
  const effectiveMode = writeAllowed ? mode : 'read'

  useEffect(() => {
    if (!options.some((o) => o.value === expiry) && options.length) setExpiry(options[options.length - 1].value)
  }, [options, expiry])

  // Viewer joins / leaves and revocations from other windows refresh the list.
  useEffect(
    () =>
      events.onAny((ev) => {
        const e = ev as unknown as { type?: string; sessionId?: string }
        if (e.type === 'share.changed' && e.sessionId === sessionId) void qc.invalidateQueries({ queryKey: recKeys.shares(sessionId) })
      }),
    [qc, sessionId],
  )

  const create = async () => {
    setCreating(true)
    try {
      const v = await createShare(sessionId, {
        mode: effectiveMode,
        expiresInSec: Number(expiry),
        requireLogin,
        label: label.trim() || undefined,
        inputPaused: effectiveMode === 'write' ? inputPaused : undefined,
      })
      recordingsSettings.set({
        shareMode: effectiveMode,
        shareExpiresInSec: Number(expiry),
        shareRequireLogin: requireLogin,
        ...(effectiveMode === 'write' ? { shareInputPaused: inputPaused } : {}),
      })
      setFresh(v.id)
      setLabel('')
      qc.setQueryData<ShareView[]>(recKeys.shares(sessionId), (old) => [...(old ?? []), v])
      if (v.url && (await copyText(absoluteShareUrl(v.url)))) toast.success('Share link copied to the clipboard')
    } catch (err) {
      toast.error('Could not create the share link', { description: errorMessage(err) })
    } finally {
      setCreating(false)
    }
  }

  const list = shares.data ?? []
  const viewers = list.reduce((n, s) => n + s.viewers.length, 0)

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          <Share2 className="size-4 text-muted-foreground" />
          Share {title ? `“${title}”` : 'session'}
        </DialogTitle>
        <DialogDescription>People with a link watch this terminal live in their browser — no Termstead account needed unless you require one.</DialogDescription>
      </DialogHeader>
      <DialogBody className="flex flex-col gap-4">
        {disabled ? (
          <div className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm">Session sharing is disabled by your administrator.</div>
        ) : (
          <section className="grid gap-3 rounded-lg border bg-muted/30 p-3" aria-label="New share link">
            <div className="flex flex-wrap items-center gap-3">
              <SegmentedControl
                aria-label="Access"
                value={effectiveMode}
                onValueChange={setMode}
                options={[
                  { value: 'read', label: 'View only', icon: Eye },
                  { value: 'write', label: 'Interactive', icon: Keyboard, disabled: !writeAllowed, title: writeAllowed ? undefined : 'Disabled by the administrator' },
                ]}
              />
              <div className="flex items-center gap-2">
                <span className="text-sm text-muted-foreground">Expires in</span>
                <SimpleSelect size="sm" value={expiry} onValueChange={setExpiry} options={options} aria-label="Expiry" className="w-32" />
              </div>
            </div>
            {effectiveMode === 'write' && (
              <div className="grid gap-2 rounded-md border border-warning/40 bg-warning/10 px-2.5 py-2 text-sm animate-in fade-in-0">
                <div className="flex items-start gap-2">
                  <AlertTriangle className="mt-0.5 size-3.5 shrink-0 text-warning" />
                  Anyone with this link can type into your session, with your permissions. Share it only with people you trust
                  {policy?.commandAudit ? ' — their commands are recorded in the audit log as typed by a guest.' : '.'}
                </div>
                <SwitchField
                  checked={inputPaused}
                  onCheckedChange={setInputPaused}
                  label="Guests watch until I allow input"
                  description="Turn guest input on or off at any time from the link below."
                  className="items-center gap-2 pl-5.5"
                />
              </div>
            )}
            <div className="flex flex-wrap items-center gap-3">
              <Input
                inputSize="sm"
                value={label}
                onChange={(e) => setLabel(e.target.value)}
                placeholder="Label (optional), e.g. “for Dana”"
                maxLength={80}
                className="min-w-48 flex-1"
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void create()
                }}
              />
              <SwitchField checked={requireLogin} onCheckedChange={setRequireLogin} label="Viewers must sign in" className="items-center gap-2" />
            </div>
            <div className="flex justify-end">
              <Button size="sm" onClick={() => void create()} loading={creating}>
                <Link2 /> Create link & copy
              </Button>
            </div>
          </section>
        )}

        <section className="grid gap-1.5" aria-label="Active links">
          <div className="flex items-center justify-between">
            <h3 className="text-sm font-medium text-muted-foreground">
              Active links{list.length ? ` · ${list.length}` : ''}
              {viewers > 0 && <span className="ml-2 text-foreground">{plural(viewers, 'viewer')} connected</span>}
            </h3>
            {list.length > 1 && (
              <Button
                size="xs"
                variant="ghost"
                className="text-destructive"
                onClick={async () => {
                  try {
                    await revokeSessionShares(sessionId)
                    qc.setQueryData(recKeys.shares(sessionId), [])
                    toast.success('All links revoked')
                  } catch (err) {
                    toast.error('Could not revoke the links', { description: errorMessage(err) })
                  }
                }}
              >
                Revoke all
              </Button>
            )}
          </div>
          {list.length === 0 ? (
            <p className="rounded-md border border-dashed px-3 py-4 text-center text-sm text-muted-foreground">
              {shares.isLoading ? ' ' : 'No active links. Links stop working when you revoke them, when they expire or when the session closes.'}
            </p>
          ) : (
            <ul className="grid gap-1.5">
              {list.map((s) => (
                <ShareRow key={s.id} share={s} highlight={s.id === fresh} sessionId={sessionId} />
              ))}
            </ul>
          )}
        </section>
      </DialogBody>
      <DialogFooter>
        <Button variant="secondary" onClick={closeShareDialog}>
          Done
        </Button>
      </DialogFooter>
    </>
  )
}

function ShareRow({ share, highlight, sessionId }: { share: ShareView; highlight: boolean; sessionId: string }) {
  const qc = useQueryClient()
  const now = useNow(30_000)
  const [copied, setCopied] = useState(false)
  const [revoking, setRevoking] = useState(false)
  const [toggling, setToggling] = useState(false)

  const toggleInput = async () => {
    const paused = !share.inputPaused
    setToggling(true)
    // Optimistic: the switch flips at once; the server confirms (or the list is refetched on error).
    qc.setQueryData<ShareView[]>(recKeys.shares(sessionId), (old) => old?.map((s) => (s.id === share.id ? { ...s, inputPaused: paused } : s)))
    try {
      const v = await setShareInput(share.id, paused)
      qc.setQueryData<ShareView[]>(recKeys.shares(sessionId), (old) => old?.map((s) => (s.id === share.id ? v : s)))
    } catch (err) {
      void qc.invalidateQueries({ queryKey: recKeys.shares(sessionId) })
      toast.error(paused ? 'Could not pause guest input' : 'Could not allow guest input', { description: errorMessage(err) })
    } finally {
      setToggling(false)
    }
  }
  const url = share.url ? absoluteShareUrl(share.url) : ''
  const expiresIn = Date.parse(share.expiresAt) - now

  const copy = async () => {
    if (!url) return
    if (await copyText(url)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } else toast.error('The browser refused clipboard access')
  }

  return (
    <li
      className={cn(
        'grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1.5 rounded-md border px-2.5 py-1.5 transition-colors duration-200',
        highlight ? 'border-primary/50 bg-primary/5' : 'bg-card',
      )}
    >
      <Badge variant={share.mode === 'write' ? 'warning' : 'secondary'}>
        {share.mode === 'write' ? <Keyboard /> : <Eye />}
        {share.mode === 'write' ? 'Interactive' : 'View only'}
      </Badge>
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm">
          {share.label || <span className="text-muted-foreground">Link created {formatRelativeTime(share.createdAt, now)}</span>}
        </div>
        <div className="flex items-center gap-2 text-xs text-muted-foreground tabular-nums">
          <span>{expiresIn > 0 ? `expires ${formatRelativeTime(share.expiresAt, now)}` : 'expired'}</span>
          {share.requireLogin && <span>· sign-in required</span>}
          {share.mode === 'write' && share.inputPaused && <span className="text-warning">· guest input paused</span>}
          {share.uses > 0 && <span>· opened {plural(share.uses, 'time')}</span>}
        </div>
      </div>
      <div className="flex items-center gap-0.5">
        {share.viewers.length > 0 && (
          <Tooltip
            content={
              <div className="grid gap-0.5">
                {share.viewers.map((v) => (
                  <div key={v.id}>
                    {v.username ?? 'Anonymous'} · {v.ip} · since {new Date(v.since).toLocaleTimeString()}
                  </div>
                ))}
              </div>
            }
          >
            <span className="flex items-center gap-1 rounded-sm bg-success/15 px-1.5 py-0.5 text-xs font-medium text-success tabular-nums">
              <Users className="size-3" />
              {share.viewers.length}
            </span>
          </Tooltip>
        )}
        {share.mode === 'write' && (
          <IconButton
            icon={share.inputPaused ? Play : Pause}
            label={share.inputPaused ? 'Allow guest input' : 'Pause guest input'}
            size="xs"
            disabled={toggling} // optimistic: the icon and the row flip at once, no spinner needed
            onClick={() => void toggleInput()}
          />
        )}
        {url && (
          <>
            <IconButton icon={copied ? Check : Copy} label={copied ? 'Copied' : 'Copy link'} size="xs" onClick={() => void copy()} />
            <Popover>
              <PopoverTrigger asChild>
                <Button variant="ghost" size="icon-xs" aria-label="Show QR code" className="text-muted-foreground hover:text-foreground">
                  <QrCode />
                </Button>
              </PopoverTrigger>
              <PopoverContent className="w-auto p-3">
                <div className="rounded-md bg-white p-2">
                  <QRCodeSVG value={url} size={176} level="M" />
                </div>
                <p className="mt-2 max-w-44 text-center text-xs text-muted-foreground">Scan to open the shared session on another device.</p>
              </PopoverContent>
            </Popover>
          </>
        )}
        <IconButton
          icon={Trash2}
          label="Revoke link"
          size="xs"
          busy={revoking}
          disabled={revoking}
          className="hover:text-destructive"
          onClick={async () => {
            setRevoking(true)
            try {
              await revokeShare(share.id)
              qc.setQueryData<ShareView[]>(recKeys.shares(sessionId), (old) => old?.filter((s) => s.id !== share.id))
              toast.success('Link revoked', { description: share.viewers.length ? 'Its viewers were disconnected.' : undefined })
            } catch (err) {
              toast.error('Could not revoke the link', { description: errorMessage(err) })
            } finally {
              setRevoking(false)
            }
          }}
        />
      </div>
      {/* The link just created: shown in full, so what was copied is never a mystery. */}
      {highlight && url && (
        <Input inputSize="sm" readOnly value={url} aria-label="Share link" className="col-span-3 font-mono text-xs" onFocus={(e) => e.currentTarget.select()} />
      )}
    </li>
  )
}
