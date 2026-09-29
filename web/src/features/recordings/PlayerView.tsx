/*
 * Tab kind "player" {recordingId | sessionId + minutes, startAt?, line?}: plays an asciicast recording or an instant
 * replay of a live session with asciinema-player (lazy, its own control bar off), or shows a text log (LogViewer). Our
 * transport: scrubber with command markers, ±5 s, speed and pause shortening; seek from markers and search results,
 * keyboard (Space, ←/→, [ / ]), fullscreen, copy the screen at the current time, downloads (cast v3 / v2 / transcript).
 */
import { lazy, useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore, type KeyboardEvent } from 'react'
import type { Player } from 'asciinema-player'
import {
  Clapperboard,
  ClipboardCopy,
  Download,
  FileText,
  Flag,
  Maximize,
  Minimize,
  Pause,
  Play,
  RefreshCw,
  Rewind,
  FastForward,
  Search,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/api/client'
import type { TabProps } from '@/app/registry'
import { runCommand, isCommandEnabled } from '@/app/commands'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LazyBoundary, LoadingPane } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { isDarkTheme, onThemeChange } from '@/lib/theme'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, copyText, errorMessage, formatBytes, formatDateTime, formatDuration } from '@/lib/utils'
import { setTabTitle, useIsTabVisible } from '@/stores/workspace'
import { effectiveTerminalSettings, terminalSettings } from '@/features/terminal/settings'
import { resolveScheme } from '@/features/terminal/themes'
import { downloadUrl, recordingFileUrl, replayUrl, useRecording } from './api'
import { castMarkers, idleTimeline, parseCast, screenAt, transcript, type ParsedCast } from './cast'
import { clock } from './clock'
import { recordingsSettings } from './settings'
import { Timeline, type TimelineMarker } from './Timeline'
import type { PlayerTabParams, RecordingItem } from './types'

const LogViewer = lazy(() => import('./LogViewer'))

const SPEEDS = ['0.5', '1', '1.5', '2', '4', '8', '16']
const IDLE = [
  { value: '0', label: 'Keep as recorded' },
  { value: '0.5', label: 'Shorten to 0.5 s' },
  { value: '1', label: 'Shorten to 1 s' },
  { value: '2', label: 'Shorten to 2 s' },
  { value: '5', label: 'Shorten to 5 s' },
]

function useUiDark(): boolean {
  return useSyncExternalStore(
    (cb) => onThemeChange(() => cb()),
    isDarkTheme,
    isDarkTheme,
  )
}

export default function PlayerView({ tabId, params }: TabProps<PlayerTabParams>) {
  const rec = useRecording(params.recordingId)
  const item = rec.data
  const gate = useLoadingGate(rec.isPending && rec.fetchStatus !== 'idle')
  useEffect(() => {
    if (item?.title) setTabTitle(tabId, item.title)
  }, [tabId, item?.title])

  if (params.sessionId && !params.recordingId) {
    return <CastPlayer tabId={tabId} source={{ kind: 'replay', sessionId: params.sessionId, minutes: params.minutes ?? 0 }} title={params.title} startAt={params.startAt} />
  }
  if (!params.recordingId) return <EmptyState icon={Clapperboard} title="Nothing to play" description="Open a recording from the Recordings tab." />
  if (rec.isError) {
    return (
      <EmptyState
        icon={Clapperboard}
        title="Recording unavailable"
        description={errorMessage(rec.error)}
        action={
          <Button size="sm" variant="secondary" onClick={() => void rec.refetch()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  }
  if (gate.hold || !item) return <LoadingPane active={gate.show} immediate label="Loading the recording" />
  if (item.kind === 'log') {
    return (
      <div className="relative h-full">
        <LazyBoundary label="Loading the log viewer" className="bg-background">
          <LogViewer item={item} line={params.line} />
        </LazyBoundary>
      </div>
    )
  }
  return <CastPlayer tabId={tabId} source={{ kind: 'recording', item }} title={item.title} startAt={params.startAt} />
}

type Source = { kind: 'recording'; item: RecordingItem } | { kind: 'replay'; sessionId: string; minutes: number }

let themeSeq = 0

/** Registers an asciinema-player theme class from the current terminal colour scheme; returns its name. */
function useSchemeTheme(): string {
  const dark = useUiDark()
  const [name] = useState(() => `astraterm-${++themeSeq}`)
  useEffect(() => {
    const s = resolveScheme(effectiveTerminalSettings(terminalSettings.get()), dark)
    const colors = [s.black, s.red, s.green, s.yellow, s.blue, s.magenta, s.cyan, s.white, s.brightBlack, s.brightRed, s.brightGreen, s.brightYellow, s.brightBlue, s.brightMagenta, s.brightCyan, s.brightWhite]
    const el = document.createElement('style')
    el.textContent = `div.ap-player.asciinema-player-theme-${name}{--term-color-foreground:${s.foreground};--term-color-background:${s.background};${colors
      .map((c, i) => `--term-color-${i}:${c};`)
      .join('')}}`
    document.head.appendChild(el)
    return () => el.remove()
  }, [dark, name])
  return name
}

/** Runs `done` once the asciinema player inside `layer` has painted its terminal (or after ~0.5 s). */
function whenPainted(layer: HTMLElement, done: () => void): () => void {
  let frames = 0
  let id = 0
  const tick = () => {
    if ((layer.querySelector('.ap-term') && !layer.querySelector('.ap-overlay-loading')) || ++frames > 30) done()
    else id = requestAnimationFrame(tick)
  }
  id = requestAnimationFrame(tick)
  return () => cancelAnimationFrame(id)
}

function CastPlayer({ tabId, source, title, startAt }: { tabId: string; source: Source; title?: string; startAt?: number }) {
  const prefs = recordingsSettings.use()
  const visible = useIsTabVisible(tabId)
  const theme = useSchemeTheme()
  const [text, setText] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [speed, setSpeed] = useState(String(prefs.speed))
  const [idle, setIdle] = useState(String(prefs.idleTimeLimit))
  const [playing, setPlaying] = useState(false)
  /** The playhead in recorded time, sampled while visible (shown in player time: see `current`). Kept in recorded time
   *  so a new idle limit re-maps it in the same render as the new duration — the playhead never jumps for a frame. */
  const [rawNow, setRawNow] = useState(0)
  const [panel, setPanel] = useState<'markers' | 'search' | null>(null)
  const [fullscreen, setFullscreen] = useState(false)
  const hostRef = useRef<HTMLDivElement>(null)
  const rootRef = useRef<HTMLDivElement>(null)
  const playerRef = useRef<Player | null>(null)
  /** Players replaced by a newer one: disposed once the newer one has painted (or on unmount). */
  const stale = useRef<(() => void)[]>([])
  /** Position across player re-creations, in recorded time (independent of the idle limit). */
  const pos = useRef<{ raw: number; play: boolean; first: boolean }>({ raw: 0, play: prefs.autoPlay, first: true })

  const url = source.kind === 'recording' ? recordingFileUrl(source.item.id) : replayUrl(source.sessionId, source.minutes)
  const live = source.kind === 'recording' ? source.item.live : true

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const t = await api.get<string>(url, { as: 'text' })
      if (!parseCast(t)) throw new Error('This file is not an asciicast recording.')
      setText(t)
      setError(null)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [url])

  useEffect(() => {
    void load()
  }, [load])

  const idleLimit = Number(idle) > 0 ? Number(idle) : 0
  const parsed = useMemo<ParsedCast | null>(() => (text ? parseCast(text) : null), [text])
  const timeline = useMemo(() => (parsed ? idleTimeline(parsed, idleLimit) : null), [parsed, idleLimit])
  const markers = useMemo(
    () => (parsed && timeline ? castMarkers(parsed).map((m) => ({ time: timeline.toPlayer(m.time), label: m.label })) : []),
    [parsed, timeline],
  )
  const duration = timeline?.duration ?? 0
  const current = timeline ? timeline.toPlayer(rawNow) : 0
  /** The timeline the live player was created with (its clock is in that timeline's player time). */
  const playerTimeline = useRef(timeline)

  // (Re)create the player whenever the data or the playback options change, keeping the position. The new player
  // renders into a fresh layer above the old one; the old one goes once the new one has painted (no blank frame).
  useEffect(() => {
    const host = hostRef.current
    if (!host || !text || !parsed || !timeline) return
    let disposed = false
    let player: Player | null = null
    let layer: HTMLDivElement | null = null
    let cancelPaint = () => {}
    void (async () => {
      const [mod] = await Promise.all([import('asciinema-player'), import('asciinema-player/dist/bundle/asciinema-player.css')])
      if (disposed) return
      let start = pos.current.raw
      if (pos.current.first) {
        pos.current.first = false
        if (startAt != null) start = startAt < 0 ? Math.max(0, parsed.duration + startAt) : startAt
      }
      start = Math.max(0, Math.min(start, parsed.duration))
      layer = document.createElement('div')
      layer.className = 'absolute inset-0'
      host.appendChild(layer)
      const p = mod.create({ data: text }, layer, {
        autoPlay: pos.current.play,
        startAt: start,
        // Paused: show the frame at the position (not a blank screen behind a start button).
        preload: true,
        poster: pos.current.play ? undefined : `npt:${timeline.toPlayer(start)}`,
        speed: Number(speed) || 1,
        idleTimeLimit: idleLimit || undefined,
        fit: 'both',
        controls: false,
        cursorMode: 'steady', // nothing blinks
        theme,
        pauseOnMarkers: prefs.pauseOnMarkers,
        terminalFontFamily: effectiveTerminalSettings(terminalSettings.get()).fontFamily,
      })
      player = p
      playerRef.current = p
      p.addEventListener('play', () => setPlaying(true))
      p.addEventListener('playing', () => setPlaying(true))
      p.addEventListener('pause', () => setPlaying(false))
      p.addEventListener('ended', () => setPlaying(false))
      setPlaying(pos.current.play)
      playerTimeline.current = timeline
      setRawNow(start)
      cancelPaint = whenPainted(layer, () => stale.current.splice(0).forEach((dispose) => dispose()))
    })()
    return () => {
      disposed = true
      cancelPaint()
      if (playerRef.current === player) playerRef.current = null
      if (!player || !layer) return
      try {
        pos.current.raw = timeline.toRaw(player.getCurrentTime() ?? 0)
      } catch {
        /* ignore */
      }
      const [p, l] = [player, layer]
      void p.pause()
      stale.current.push(() => {
        p.dispose()
        l.remove()
      })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text, parsed, timeline, speed, theme, prefs.pauseOnMarkers])

  // Declared after the player effect: on unmount its cleanup has queued the last player by now.
  useEffect(() => () => stale.current.splice(0).forEach((dispose) => dispose()), [])

  /** Seeks in flight: the clock must not sample the old position meanwhile (the playhead would jump back and forth). */
  const seeking = useRef(0)

  // Clock: sample the position while visible.
  useEffect(() => {
    if (!visible) return
    const t = setInterval(() => {
      const now = seeking.current ? null : playerRef.current?.getCurrentTime()
      if (now == null || !playerTimeline.current) return
      const raw = playerTimeline.current.toRaw(now)
      setRawNow((old) => (Math.abs(old - raw) >= 0.2 ? raw : old))
    }, 200)
    return () => clearInterval(t)
  }, [visible])

  // Pause when the tab is hidden.
  useEffect(() => {
    if (!visible && playerRef.current && playing) void playerRef.current.pause()
  }, [visible, playing])

  useEffect(() => {
    const on = () => setFullscreen(document.fullscreenElement === rootRef.current)
    document.addEventListener('fullscreenchange', on)
    return () => document.removeEventListener('fullscreenchange', on)
  }, [])

  /** Seek to a player time. */
  const seek = useCallback(
    (t: number) => {
      const p = playerRef.current
      if (!p) return
      const to = Math.min(Math.max(t, 0), duration)
      if (timeline) setRawNow(timeline.toRaw(to))
      seeking.current++
      void p.seek(to).finally(() => seeking.current--)
    },
    [duration, timeline],
  )

  const togglePlay = () => {
    const p = playerRef.current
    if (!p) return
    pos.current.play = !playing
    void (playing ? p.pause() : p.play())
  }


  const copyScreen = async (withScrollback: boolean) => {
    if (!parsed) return
    try {
      const txt = await screenAt(parsed, rawNow, withScrollback)
      if (await copyText(txt)) toast.success(withScrollback ? 'Screen and history copied' : `Screen at ${clock(current)} copied`)
      else toast.error('The browser refused clipboard access')
    } catch (err) {
      toast.error('Could not render the screen', { description: errorMessage(err) })
    }
  }

  const openScreenAsText = async () => {
    if (!parsed) return
    const txt = await screenAt(parsed, rawNow, true)
    if (!(await runCommand('editor.openText', { title: `${title || 'recording'} @ ${clock(current)}.txt`, content: txt }))) {
      await copyText(txt)
      toast.success('Copied (no text editor available)')
    }
  }

  const toggleFullscreen = async () => {
    try {
      if (document.fullscreenElement) await document.exitFullscreen()
      else await rootRef.current?.requestFullscreen({ navigationUI: 'hide' })
    } catch {
      /* denied */
    }
  }

  /** Player shortcuts while focus is anywhere in the tab except a field or button (the player handles its own). */
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.altKey || e.metaKey || e.ctrlKey || !parsed) return
    if (e.target instanceof Element && e.target.closest('input, textarea, button, [role=slider], [role=listbox], [role=menu]')) return
    const next = (dir: 1 | -1) => (dir > 0 ? markers.find((m) => m.time > current + 0.25) : [...markers].reverse().find((m) => m.time < current - 0.25))
    if (e.key === ' ' || e.key === 'k') togglePlay()
    else if (e.key === 'ArrowLeft') seek(current - (e.shiftKey ? 30 : 5))
    else if (e.key === 'ArrowRight') seek(current + (e.shiftKey ? 30 : 5))
    else if (e.key === '[' || e.key === ']') {
      const m = next(e.key === ']' ? 1 : -1)
      if (m) seek(m.time)
    } else return
    e.preventDefault()
  }

  if (error && !text) {
    return (
      <EmptyState
        icon={Clapperboard}
        title="Cannot load the recording"
        description={error}
        action={
          <Button size="sm" variant="secondary" onClick={() => void load()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  }

  const rec = source.kind === 'recording' ? source.item : null
  const togglePanel = (p: 'markers' | 'search') => setPanel((cur) => (cur === p ? null : p))
  return (
    <div ref={rootRef} className="flex h-full min-h-0 flex-col bg-background" onKeyDown={onKeyDown}>
      <div className="flex h-9 shrink-0 items-center gap-1 border-b bg-toolbar pr-1.5 pl-3">
        <div className="min-w-0 flex-1 truncate text-sm font-medium">
          {source.kind === 'replay' ? (
            <>
              Instant replay{source.minutes ? ` · last ${source.minutes} min` : ''}
              {title ? <span className="font-normal text-muted-foreground"> · {title}</span> : null}
            </>
          ) : (
            <>
              {rec?.title || 'Recording'}
              <span className="ml-2 font-normal text-muted-foreground">
                {formatDateTime(rec?.startedAt)} · {formatBytes(rec?.size)}
              </span>
            </>
          )}
        </div>
        {live && (
          <Badge variant="destructive" className="mr-1">
            <span className="size-1.5 rounded-full bg-current" /> {source.kind === 'replay' ? 'Live session' : 'Recording'}
          </Badge>
        )}
        {live && <IconButton icon={RefreshCw} label="Load newest output" busy={loading && !!text} onClick={() => void load()} />}
        <IconButton icon={Flag} label={`Commands & markers (${markers.length})`} active={panel === 'markers'} onClick={() => togglePanel('markers')} disabled={!parsed} />
        <IconButton icon={Search} label="Search output" active={panel === 'search'} onClick={() => togglePanel('search')} disabled={!parsed} />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label="Copy" className="text-muted-foreground hover:text-foreground" disabled={!parsed}>
              <ClipboardCopy />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => void copyScreen(false)}>Copy the screen at {clock(current)}</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => void copyScreen(true)}>Copy screen + history up to {clock(current)}</DropdownMenuItem>
            {isCommandEnabled('editor.openText') && <DropdownMenuItem onSelect={() => void openScreenAsText()}>Open as text…</DropdownMenuItem>}
          </DropdownMenuContent>
        </DropdownMenu>
        {rec && (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" aria-label="Download" className="text-muted-foreground hover:text-foreground">
                <Download />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuLabel>Download</DropdownMenuLabel>
              <DropdownMenuItem onSelect={() => downloadUrl(recordingFileUrl(rec.id, 'v3', true))}>asciicast v3 (.cast)</DropdownMenuItem>
              <DropdownMenuItem onSelect={() => downloadUrl(recordingFileUrl(rec.id, 'v2', true))}>asciicast v2 (older players)</DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem onSelect={() => downloadUrl(recordingFileUrl(rec.id, 'txt', true))}>
                <FileText /> Plain-text transcript
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        <IconButton icon={fullscreen ? Minimize : Maximize} label={fullscreen ? 'Exit fullscreen' : 'Fullscreen'} onClick={() => void toggleFullscreen()} />
      </div>
      <div className="flex min-h-0 flex-1">
        <div className="relative min-w-0 flex-1 p-2">
          <div ref={hostRef} className="nx-player relative h-full w-full" />
          <LoadingPane active={loading && !text} label="Loading the recording" className="pointer-events-none absolute inset-0" />
        </div>
        {panel && parsed && timeline && (
          <aside className="flex w-72 shrink-0 flex-col border-l bg-sidebar animate-in slide-in-from-right-2 fade-in-0 duration-150">
            <div className="flex h-8 items-center justify-between border-b px-2 text-sm font-medium">
              {panel === 'markers' ? 'Commands & markers' : 'Search output'}
              <IconButton icon={X} label="Close panel" size="xs" onClick={() => setPanel(null)} />
            </div>
            {panel === 'markers' ? (
              <MarkerList markers={markers} current={current} onSeek={seek} />
            ) : (
              <OutputSearch parsed={parsed} current={current} toPlayer={timeline.toPlayer} onSeek={seek} />
            )}
          </aside>
        )}
      </div>
      <Transport
        disabled={!parsed}
        playing={playing}
        current={current}
        duration={duration}
        markers={markers}
        speed={speed}
        idle={idle}
        onToggle={togglePlay}
        onSeek={seek}
        onSpeed={setSpeed}
        onIdle={setIdle}
      />
      {/* asciinema's own loading overlay would flash (the tab shows one calm LoadingPane instead), and its start button
          duplicates the transport's Play. */}
      <style>{`.nx-player .ap-wrapper{height:100%;width:100%;background:transparent}.nx-player .ap-player{border-radius:6px}.nx-player .ap-overlay-loading,.nx-player .ap-overlay-start{display:none}`}</style>
    </div>
  )
}

/** Bottom transport: play/pause, ±5 s, the scrubber between the two clocks, speed and pause shortening. */
function Transport({
  disabled,
  playing,
  current,
  duration,
  markers,
  speed,
  idle,
  onToggle,
  onSeek,
  onSpeed,
  onIdle,
}: {
  disabled: boolean
  playing: boolean
  current: number
  duration: number
  markers: TimelineMarker[]
  speed: string
  idle: string
  onToggle: () => void
  onSeek: (t: number) => void
  onSpeed: (v: string) => void
  onIdle: (v: string) => void
}) {
  return (
    <div className="flex h-10 shrink-0 items-center gap-1 border-t bg-toolbar px-1.5">
      <IconButton icon={playing ? Pause : Play} label={playing ? 'Pause' : 'Play'} shortcut="Space" onClick={onToggle} disabled={disabled} />
      <IconButton icon={Rewind} label="Back 5 seconds" shortcut="ArrowLeft" onClick={() => onSeek(current - 5)} disabled={disabled} />
      <IconButton icon={FastForward} label="Forward 5 seconds" shortcut="ArrowRight" onClick={() => onSeek(current + 5)} disabled={disabled} />
      <span className="w-12 shrink-0 text-right text-xs text-muted-foreground tabular-nums">{clock(current)}</span>
      <Timeline className="mx-2 min-w-24 flex-1" duration={duration} current={current} markers={markers} onSeek={onSeek} disabled={disabled} />
      <span className="w-12 shrink-0 text-xs text-muted-foreground tabular-nums">{clock(duration)}</span>
      <DropdownMenu>
        <Tooltip content="Speed and pauses">
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="xs" className="min-w-11 text-muted-foreground tabular-nums hover:text-foreground" disabled={disabled}>
              {speed}×
            </Button>
          </DropdownMenuTrigger>
        </Tooltip>
        <DropdownMenuContent align="end" side="top">
          <DropdownMenuLabel>Speed</DropdownMenuLabel>
          <DropdownMenuRadioGroup value={speed} onValueChange={onSpeed}>
            {SPEEDS.map((s) => (
              <DropdownMenuRadioItem key={s} value={s}>
                {s}×
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
          <DropdownMenuSeparator />
          <DropdownMenuLabel>Long pauses</DropdownMenuLabel>
          <DropdownMenuRadioGroup value={idle} onValueChange={onIdle}>
            {IDLE.map((o) => (
              <DropdownMenuRadioItem key={o.value} value={o.value}>
                {o.label}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}

function MarkerList({ markers, current, onSeek }: { markers: { time: number; label: string }[]; current: number; onSeek: (t: number) => void }) {
  if (!markers.length) {
    return <p className="p-3 text-sm text-muted-foreground">No markers. Shells with shell integration (OSC 133) mark every prompt; instant replays list the recognized commands.</p>
  }
  let active = -1
  for (let i = 0; i < markers.length; i++) if (markers[i].time <= current + 0.05) active = i
  return (
    <ul className="min-h-0 flex-1 overflow-y-auto py-1" role="listbox" aria-label="Markers">
      {markers.map((m, i) => (
        <li key={i}>
          <button
            type="button"
            role="option"
            aria-selected={i === active}
            onClick={() => onSeek(m.time)}
            className={cn(
              'flex w-full items-baseline gap-2 px-2 py-1 text-left text-sm transition-colors hover:bg-accent focus-visible:bg-accent focus-visible:outline-none',
              i === active && 'bg-primary/10',
            )}
          >
            <span className="w-12 shrink-0 text-xs text-muted-foreground tabular-nums">{clock(m.time)}</span>
            <span className="min-w-0 truncate font-mono text-[12px]">{m.label || `Prompt ${i + 1}`}</span>
          </button>
        </li>
      ))}
    </ul>
  )
}

function OutputSearch({
  parsed,
  current,
  toPlayer,
  onSeek,
}: {
  parsed: ParsedCast
  current: number
  toPlayer: (raw: number) => number
  onSeek: (t: number) => void
}) {
  const [q, setQ] = useState('')
  const lines = useMemo(() => transcript(parsed).map((l) => ({ ...l, time: toPlayer(l.time) })), [parsed, toPlayer])
  const hits = useMemo(() => {
    const s = q.trim().toLowerCase()
    if (!s) return []
    const r: { time: number; text: string }[] = []
    for (const l of lines) {
      if (l.text.toLowerCase().includes(s)) {
        r.push(l)
        if (r.length >= 500) break
      }
    }
    return r
  }, [q, lines])
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="border-b p-2">
        <Input
          inputSize="sm"
          autoFocus
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Find in output…"
          leading={<Search />}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && hits.length) {
              const next = hits.find((h) => h.time > current + 0.05) ?? hits[0]
              onSeek(next.time)
            }
          }}
        />
        {q.trim() && (
          <div className="mt-1 text-xs text-muted-foreground tabular-nums">
            {hits.length ? `${hits.length}${hits.length >= 500 ? '+' : ''} matches · Enter jumps to the next` : 'No matches'}
          </div>
        )}
      </div>
      <ul className="min-h-0 flex-1 overflow-y-auto py-1">
        {hits.map((h, i) => (
          <li key={i}>
            <button
              type="button"
              onClick={() => onSeek(h.time)}
              className="flex w-full items-baseline gap-2 px-2 py-1 text-left text-sm hover:bg-accent focus-visible:bg-accent focus-visible:outline-none"
            >
              <span className="w-12 shrink-0 text-xs text-muted-foreground tabular-nums">{clock(h.time)}</span>
              <span className="min-w-0 truncate font-mono text-[12px]">{h.text}</span>
            </button>
          </li>
        ))}
      </ul>
      <div className="border-t px-2 py-1 text-xs text-muted-foreground">{formatDuration(parsed.duration * 1000)} of output · {lines.length} lines</div>
    </div>
  )
}
