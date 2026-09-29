/*
 * Preview (FILE-12): images in a lightbox (zoom / pan / pinch, previous / next among the folder's images), PDF in the
 * browser's viewer, audio / video, text (the first 256 KiB, monospace) and Markdown (rendered or source) with "Open in
 * editor". Anything else offers a download.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, Download, Eye, FileQuestionMark, Maximize, Pencil, X, ZoomIn, ZoomOut } from 'lucide-react'
import { TransformComponent, TransformWrapper, type ReactZoomPanPinchRef } from 'react-zoom-pan-pinch'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { FileEntry } from '@/api/types'
import { useCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { IconButton } from '@/components/ui/icon-button'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { cn, errorMessage, formatBytes } from '@/lib/utils'
import { Spinner } from '@/components/ui/spinner'
import { editEntry } from '../actions'
import { fsApi } from '../api'
import { downloadEntries, inlineUrl, liveFsId } from '../download'
import { previewKind } from '../format'
import { withFs } from '../fsHandles'
import { FileIcon } from '../icons'
import { dirname } from '../paths'
import type { FsContext } from '../types'

const TEXT_LIMIT = 256 * 1024
const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

export default function PreviewDialog({ ctx, entry: initial, siblings, onClose }: { ctx: FsContext; entry: FileEntry; siblings: FileEntry[]; onClose: () => void }) {
  const [entry, setEntry] = useState(initial)
  const kind = previewKind(entry)
  const images = useMemo(() => siblings.filter((e) => previewKind(e) === 'image').sort((a, b) => collator.compare(a.name, b.name)), [siblings])
  const index = kind === 'image' ? images.findIndex((e) => e.path === entry.path) : -1
  const canStep = kind === 'image' && images.length > 1 && index >= 0
  const step = (d: number) => {
    if (!canStep) return
    setEntry(images[(index + d + images.length) % images.length])
  }
  const editor = useCommand('editor.open')
  const contentRef = useRef<HTMLDivElement>(null)
  const zoomRef = useRef<ReactZoomPanPinchRef | null>(null)
  // Media elements load straight from the download URL: make sure its handle is alive first (idle / replaced).
  const live = useQuery({ queryKey: ['fs', ctx.key, 'live', ctx.handle.id], queryFn: () => liveFsId(ctx), staleTime: 30_000, retry: false })
  const fsId = live.data

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        ref={contentRef}
        size="full"
        hideClose
        className="gap-0 overflow-hidden p-0"
        // Focus the viewer itself (not the first toolbar button, whose tooltip would take the first Escape).
        onOpenAutoFocus={(e) => {
          e.preventDefault()
          contentRef.current?.focus()
        }}
        onKeyDown={(e) => {
          if (kind !== 'image' || e.ctrlKey || e.metaKey || e.altKey) return
          const z = zoomRef.current
          if (e.key === 'ArrowRight' || e.key === 'PageDown' || e.key === ' ') {
            e.preventDefault()
            step(1)
          } else if (e.key === 'ArrowLeft' || e.key === 'PageUp') {
            e.preventDefault()
            step(-1)
          } else if ((e.key === '+' || e.key === '=') && z) {
            e.preventDefault()
            z.zoomIn()
          } else if ((e.key === '-' || e.key === '_') && z) {
            e.preventDefault()
            z.zoomOut()
          } else if (e.key === '0' && z) {
            e.preventDefault()
            z.resetTransform()
          }
        }}
      >
        <div className="flex h-10 shrink-0 items-center gap-2 border-b pr-1.5 pl-3">
          <FileIcon entry={entry} />
          <DialogTitle className="min-w-0 truncate text-sm">{entry.name}</DialogTitle>
          <DialogDescription className="shrink-0 text-xs">
            {formatBytes(entry.size)}
            {canStep && ` · ${index + 1} of ${images.length}`}
          </DialogDescription>
          <span className="flex-1" />
          {canStep && (
            <>
              <IconButton icon={ChevronLeft} label="Previous image" shortcut="ArrowLeft" size="xs" onClick={() => step(-1)} />
              <IconButton icon={ChevronRight} label="Next image" shortcut="ArrowRight" size="xs" onClick={() => step(1)} />
            </>
          )}
          {(kind === 'text' || kind === 'markdown') && editor.enabled && (
            <Button
              size="xs"
              variant="secondary"
              onClick={() => {
                onClose()
                void editEntry(ctx, entry)
              }}
            >
              <Pencil /> Open in editor
            </Button>
          )}
          <IconButton icon={Download} label="Download" size="xs" onClick={() => downloadEntries(ctx, [entry], dirname(entry.path))} />
          <IconButton icon={X} label="Close" size="xs" onClick={onClose} />
        </div>
        <div className="relative min-h-0 flex-1 bg-muted/30">
          {!fsId && kind && kind !== 'text' && kind !== 'markdown' && (
            <div className="flex size-full items-center justify-center">
              <Spinner className="size-6" />
            </div>
          )}
          {fsId && kind === 'image' && <ImageView key={entry.path} src={inlineUrl(fsId, entry.path)} alt={entry.name} zoomRef={zoomRef} />}
          {fsId && kind === 'pdf' && <iframe title={entry.name} src={inlineUrl(fsId, entry.path)} className="size-full border-0 bg-white" />}
          {fsId && kind === 'video' && (
            <div className="flex size-full items-center justify-center p-4">
              <video src={inlineUrl(fsId, entry.path)} controls className="max-h-full max-w-full rounded-md bg-black" />
            </div>
          )}
          {fsId && kind === 'audio' && (
            <div className="flex size-full items-center justify-center p-4">
              <audio src={inlineUrl(fsId, entry.path)} controls className="w-full max-w-lg" />
            </div>
          )}
          {(kind === 'text' || kind === 'markdown') && <TextView ctx={ctx} entry={entry} markdown={kind === 'markdown'} />}
          {!kind && (
            <div className="flex size-full flex-col items-center justify-center gap-3 p-6 text-center">
              <FileQuestionMark className="size-8 text-muted-foreground" />
              <div className="text-sm text-muted-foreground">No preview for this type of file.</div>
              <Button size="sm" onClick={() => downloadEntries(ctx, [entry], dirname(entry.path))}>
                <Download /> Download
              </Button>
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}

function ImageView({ src, alt, zoomRef }: { src: string; alt: string; zoomRef: React.RefObject<ReactZoomPanPinchRef | null> }) {
  const [state, setState] = useState<'loading' | 'ok' | 'error'>('loading')
  return (
    <TransformWrapper
      ref={zoomRef}
      minScale={0.1}
      maxScale={24}
      centerOnInit
      limitToBounds={false}
      doubleClick={{ mode: 'toggle', step: 1.5 }}
      wheel={{ step: 0.12 }}
      // Arrow keys switch images (the dialog handles +/-/0 zoom).
      keyboard={{ disabled: true }}
    >
      {({ zoomIn, zoomOut, resetTransform, centerView }) => (
        <>
          <div className="absolute right-3 bottom-3 z-10 flex items-center gap-0.5 rounded-md border bg-popover/90 p-0.5 shadow-popover backdrop-blur">
            <IconButton icon={ZoomOut} label="Zoom out" size="xs" onClick={() => void zoomOut()} />
            <IconButton icon={ZoomIn} label="Zoom in" size="xs" onClick={() => void zoomIn()} />
            <IconButton icon={Maximize} label="Fit" size="xs" onClick={() => void resetTransform()} />
            <IconButton icon={Eye} label="Actual size" size="xs" onClick={() => void centerView(1)} />
          </div>
          {state === 'loading' && (
            <div className="absolute inset-0 flex items-center justify-center">
              <Spinner className="size-6" />
            </div>
          )}
          {state === 'error' && <div className="absolute inset-0 flex items-center justify-center text-sm text-destructive">The image could not be loaded.</div>}
          <TransformComponent wrapperClass="!size-full" contentClass="!size-full flex items-center justify-center">
            <img
              src={src}
              alt={alt}
              draggable={false}
              onLoad={() => setState('ok')}
              onError={() => setState('error')}
              className={cn('max-h-full max-w-full object-contain [image-rendering:auto]', state !== 'ok' && 'opacity-0')}
              style={{ backgroundImage: 'repeating-conic-gradient(#8882 0% 25%, transparent 0% 50%)', backgroundSize: '16px 16px' }}
            />
          </TransformComponent>
        </>
      )}
    </TransformWrapper>
  )
}

function looksBinary(bytes: Uint8Array): boolean {
  const n = Math.min(bytes.length, 8192)
  let odd = 0
  for (let i = 0; i < n; i++) {
    const b = bytes[i]
    if (b === 0) return true
    if (b < 7 || (b > 13 && b < 32 && b !== 27)) odd++
  }
  return n > 0 && odd / n > 0.1
}

function TextView({ ctx, entry, markdown }: { ctx: FsContext; entry: FileEntry; markdown: boolean }) {
  const q = useQuery({
    queryKey: ['fs', ctx.handle.id, 'head', entry.path, entry.mtime],
    placeholderData: undefined, // never show another file's text under this key
    queryFn: ({ signal }) => withFs(ctx.key, (id) => fsApi.head(id, entry.path, TEXT_LIMIT, signal)),
    retry: false,
    staleTime: 30_000,
  })
  const [mode, setMode] = useState<'rendered' | 'source'>(markdown ? 'rendered' : 'source')
  useEffect(() => setMode(markdown ? 'rendered' : 'source'), [markdown, entry.path])
  const text = useMemo(() => (q.data && !looksBinary(q.data.bytes) ? new TextDecoder('utf-8', { fatal: false }).decode(q.data.bytes) : null), [q.data])

  if (q.isPending) {
    return (
      <div className="flex size-full items-center justify-center">
        <Spinner className="size-6" />
      </div>
    )
  }
  if (q.isError) return <div className="flex size-full items-center justify-center p-6 text-sm text-destructive">{errorMessage(q.error)}</div>
  if (text === null) {
    return (
      <div className="flex size-full flex-col items-center justify-center gap-3 p-6 text-center text-sm text-muted-foreground">
        This looks like a binary file.
        <Button size="sm" onClick={() => downloadEntries(ctx, [entry], dirname(entry.path))}>
          <Download /> Download
        </Button>
      </div>
    )
  }
  return (
    <div className="flex size-full flex-col">
      {(markdown || q.data?.truncated) && (
        <div className="flex h-8 shrink-0 items-center gap-2 border-b bg-panel px-3 text-xs text-muted-foreground">
          {markdown && (
            <SegmentedControl
              size="sm"
              value={mode}
              onValueChange={setMode}
              options={[
                { value: 'rendered', label: 'Rendered' },
                { value: 'source', label: 'Source' },
              ]}
              aria-label="Markdown view"
            />
          )}
          {q.data?.truncated && <span>Showing the first {formatBytes(TEXT_LIMIT)}{q.data.total ? ` of ${formatBytes(q.data.total)}` : ''}.</span>}
        </div>
      )}
      <div className="min-h-0 flex-1 overflow-auto bg-panel">
        {mode === 'rendered' ? (
          <article className="nx-markdown mx-auto max-w-3xl px-6 py-5 text-base leading-relaxed [&_a]:text-primary [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:pl-3 [&_blockquote]:text-muted-foreground [&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_code]:font-mono [&_code]:text-sm [&_h1]:mt-4 [&_h1]:mb-2 [&_h1]:text-xl [&_h1]:font-semibold [&_h2]:mt-4 [&_h2]:mb-2 [&_h2]:text-lg [&_h2]:font-semibold [&_h3]:mt-3 [&_h3]:mb-1.5 [&_h3]:font-semibold [&_li]:ml-5 [&_ol]:list-decimal [&_p]:my-2 [&_pre]:my-2 [&_pre]:overflow-auto [&_pre]:rounded-md [&_pre]:bg-muted [&_pre]:p-3 [&_table]:my-2 [&_td]:border [&_td]:px-2 [&_th]:border [&_th]:px-2 [&_ul]:list-disc">
            <ReactMarkdown
              remarkPlugins={[remarkGfm]}
              components={{ a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" /> }}
            >
              {text}
            </ReactMarkdown>
          </article>
        ) : (
          <pre className="min-w-max p-3 font-mono text-xs leading-5 whitespace-pre text-foreground">{text}</pre>
        )}
      </div>
    </div>
  )
}
