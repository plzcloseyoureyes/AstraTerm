/*
 * Remote desktop recordings (guacd sessions recorded by AstraTerm, GET /api/rdp/recordings): list with play, download
 * and delete, and a player built on Guacamole.SessionRecording (lazy, streamed). Opened by the command rdp.recordings.
 */
import { useEffect, useRef, useState } from 'react'
import type * as Guac from 'guacamole-common-js'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, Download, Film, Pause, Play, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Slider } from '@/components/ui/slider'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage, formatBytes, formatDateTime, formatDuration } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { deleteRecording, downloadUrl, recordingFileUrl, useRecordings } from './api'
import { loadGuacamole } from './guac'
import { closeRecordings, useRdpStore } from './store'
import type { RdpRecording } from './types'

export default function RecordingsDialog({ locked }: { locked: boolean }) {
  const open = useRdpStore((s) => s.recordingsOpen) && !locked
  const [playing, setPlaying] = useState<RdpRecording | null>(null)
  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        if (!v) {
          closeRecordings()
          setPlaying(null)
        }
      }}
    >
      <DialogContent size="2xl" className="max-h-[85vh]">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            {playing && <IconButton icon={ArrowLeft} label="Back to the recordings" size="xs" onClick={() => setPlaying(null)} />}
            {playing ? playing.title || 'Remote desktop recording' : 'Remote desktop recordings'}
          </DialogTitle>
          <DialogDescription>
            {playing
              ? `${formatDateTime(playing.startedAt)} · ${playing.width}×${playing.height}`
              : 'Sessions of connections with "Record sessions" through the guacd engine.'}
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="min-h-0">{playing ? <Player rec={playing} /> : <List onPlay={setPlaying} />}</DialogBody>
      </DialogContent>
    </Dialog>
  )
}

/** m:ss (h:mm:ss) player clock. */
function clock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = String(s % 60).padStart(2, '0')
  return h ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`
}

function duration(r: RdpRecording): string {
  return r.endedAt ? formatDuration(new Date(r.endedAt).getTime() - new Date(r.startedAt).getTime()) : 'recording…'
}

function List({ onPlay }: { onPlay: (r: RdpRecording) => void }) {
  const isAdmin = useIsAdmin()
  const [all, setAll] = useState(false)
  const qc = useQueryClient()
  const { data, isLoading, error } = useRecordings(all && isAdmin)
  const showSpinner = useDelayedFlag(isLoading)

  const remove = async (r: RdpRecording) => {
    const ok = await confirm({
      title: 'Delete this recording?',
      description: `${r.title || 'The recording'} (${formatBytes(r.size)}) is removed permanently.`,
      confirmLabel: 'Delete',
      destructive: true,
    })
    if (!ok) return
    try {
      await deleteRecording(r.id)
      await qc.invalidateQueries({ queryKey: ['rdp', 'recordings'] })
      toast.success('Recording deleted')
    } catch (err) {
      toast.error('Could not delete the recording', { description: errorMessage(err) })
    }
  }

  return (
    <div className="grid gap-2">
      {isAdmin && (
        <label className="flex items-center justify-end gap-2 text-sm text-muted-foreground">
          All users
          <Switch checked={all} onCheckedChange={setAll} aria-label="Show every user's recordings" />
        </label>
      )}
      {isLoading ? (
        <div className="flex h-24 items-center justify-center">{showSpinner && <Spinner immediate label="Loading recordings" />}</div>
      ) : error ? (
        <EmptyState size="sm" title="Recordings could not be loaded" description={errorMessage(error)} />
      ) : !data?.length ? (
        <EmptyState
          size="sm"
          icon={Film}
          title="No recordings yet"
          description='Turn on "Record sessions" for a remote desktop connection and connect with the guacd engine.'
        />
      ) : (
        <ul className="divide-y rounded-md border" aria-label="Recordings">
          {data.map((r) => (
            <li key={r.id} className="flex items-center gap-3 px-3 py-2" onDoubleClick={() => r.endedAt && onPlay(r)}>
              <div className="grid min-w-0 flex-1 gap-0.5">
                <span className="truncate text-base">{r.title || 'Remote desktop'}</span>
                <span className="text-sm text-muted-foreground tabular-nums">
                  {formatDateTime(r.startedAt)} · {duration(r)} · {formatBytes(r.size)}
                </span>
              </div>
              <Button size="sm" variant="secondary" disabled={!r.endedAt} onClick={() => onPlay(r)}>
                <Play /> Play
              </Button>
              <IconButton icon={Download} label="Download (.guac)" onClick={() => downloadUrl(recordingFileUrl(r.id))} />
              <IconButton icon={Trash2} label="Delete" disabled={!r.endedAt} onClick={() => void remove(r)} />
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function Player({ rec }: { rec: RdpRecording }) {
  const stageRef = useRef<HTMLDivElement>(null)
  const recRef = useRef<Guac.SessionRecording | null>(null)
  const [state, setState] = useState<{ loading: boolean; error?: string; playing: boolean; position: number; duration: number }>({
    loading: true,
    playing: false,
    position: 0,
    duration: 0,
  })
  const showSpinner = useDelayedFlag(state.loading)

  useEffect(() => {
    let disposed = false
    let recording: Guac.SessionRecording | null = null
    let timer: ReturnType<typeof setInterval> | undefined
    const fit = () => {
      const stage = stageRef.current
      const d = recording?.getDisplay()
      if (!stage || !d || !d.getWidth()) return
      d.scale(Math.min(stage.clientWidth / d.getWidth(), stage.clientHeight / d.getHeight(), 1))
    }
    const ro = new ResizeObserver(fit)
    ;(async () => {
      try {
        const G = await loadGuacamole()
        if (disposed) return
        // Streamed through a StaticHTTPTunnel: in guacamole-common-js 1.5.0 the Blob source of SessionRecording is
        // broken (its internal blob is never assigned: "Cannot read properties of undefined (reading 'size')").
        recording = new G.SessionRecording(new G.StaticHTTPTunnel(recordingFileUrl(rec.id)))
        recRef.current = recording
        const display = recording.getDisplay()
        display.onresize = fit
        stageRef.current?.replaceChildren(display.getElement())
        if (stageRef.current) ro.observe(stageRef.current)
        const update = (patch: Partial<typeof state>) => !disposed && setState((s) => ({ ...s, ...patch }))
        recording.onload = () => update({ loading: false, duration: recording!.getDuration() })
        recording.onerror = (msg) =>
          update({
            loading: false,
            error: /gone|not found/i.test(msg) ? 'The recording file no longer exists.' : `The recording could not be read${msg ? ` (${msg})` : ''}.`,
          })
        recording.onplay = () => update({ playing: true })
        recording.onpause = () => update({ playing: false, position: recording!.getPosition() })
        recording.onprogress = (d) => update({ duration: d })
        recording.connect()
        timer = setInterval(() => {
          if (recording?.isPlaying()) update({ position: recording.getPosition() })
        }, 250)
      } catch (err) {
        if (!disposed) setState((s) => ({ ...s, loading: false, error: errorMessage(err) }))
      }
    })()
    return () => {
      disposed = true
      clearInterval(timer)
      ro.disconnect()
      try {
        recording?.pause()
        recording?.disconnect()
      } catch {
        /* ignore */
      }
      recRef.current = null
    }
  }, [rec.id])

  const toggle = () => {
    const r = recRef.current
    if (!r) return
    if (r.isPlaying()) r.pause()
    else r.play()
  }

  return (
    <div className="grid gap-3">
      <div
        ref={stageRef}
        className="relative flex h-[55vh] items-center justify-center overflow-hidden rounded-md border bg-remote-backdrop"
      />
      {showSpinner && (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner immediate className="size-3.5" /> Loading the recording…
        </div>
      )}
      {state.error ? (
        <p className="text-sm text-destructive">{state.error}</p>
      ) : (
        <div className="flex items-center gap-3">
          <Button size="sm" onClick={toggle} disabled={state.loading} aria-label={state.playing ? 'Pause' : 'Play'}>
            {state.playing ? <Pause /> : <Play />} {state.playing ? 'Pause' : 'Play'}
          </Button>
          <Slider
            aria-label="Position"
            min={0}
            max={Math.max(1, state.duration)}
            step={100}
            value={[Math.min(state.position, state.duration)]}
            disabled={state.loading || !state.duration}
            onValueChange={([v]) => setState((s) => ({ ...s, position: v }))}
            onValueCommit={([v]) => recRef.current?.seek(v, () => setState((s) => ({ ...s, position: v })))}
          />
          <span className="w-28 shrink-0 text-right font-mono text-xs text-muted-foreground tabular-nums">
            {clock(state.position)} / {clock(state.duration)}
          </span>
        </div>
      )}
    </div>
  )
}
