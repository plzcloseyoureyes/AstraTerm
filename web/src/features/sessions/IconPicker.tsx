/*
 * Icon picker for sessions and folders: built-in icon grid with search, or a custom image (PNG/SVG/ICO/JPEG/GIF/WebP)
 * rasterised to a 64×64 PNG data URL so it stays small and travels with exports.
 */
import { useMemo, useRef, useState } from 'react'
import { Popover as PopoverPrimitive } from 'radix-ui'
import { ImagePlus, RotateCcw, Search } from 'lucide-react'
import type { IconType } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { cn, errorMessage } from '@/lib/utils'
import { EntityIcon, ICON_GROUPS, LUCIDE_PREFIX, MAX_ICON_CHARS, isCustomImageIcon } from './icons'

const ACCEPT = 'image/png,image/svg+xml,image/x-icon,image/vnd.microsoft.icon,image/jpeg,image/gif,image/webp,image/bmp,.ico,.svg'
const ICON_SIZE = 64

function loadImage(src: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image()
    img.onload = () => resolve(img)
    img.onerror = () => reject(new Error('The file is not a readable image'))
    img.src = src
  })
}

function readAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result ?? ''))
    r.onerror = () => reject(new Error('Could not read the file'))
    r.readAsDataURL(file)
  })
}

/** Convert an uploaded image into a small PNG data URL (falls back to the original bytes for tiny files). */
async function imageFileToIcon(file: File): Promise<string> {
  const okType = /^image\/(png|jpeg|gif|webp|bmp|svg\+xml|x-icon|vnd\.microsoft\.icon)$/.test(file.type) || /\.(ico|svg|png|jpe?g|gif|webp|bmp)$/i.test(file.name)
  if (!okType) throw new Error('Choose a PNG, SVG, ICO, JPEG, GIF or WebP image')
  if (file.size > 4 * 1024 * 1024) throw new Error('The image is larger than 4 MB')
  const url = URL.createObjectURL(file)
  try {
    const img = await loadImage(url)
    const canvas = document.createElement('canvas')
    canvas.width = ICON_SIZE
    canvas.height = ICON_SIZE
    const ctx = canvas.getContext('2d')
    if (!ctx) throw new Error('Canvas is not available')
    const w = img.naturalWidth || ICON_SIZE
    const h = img.naturalHeight || ICON_SIZE
    const scale = Math.min(ICON_SIZE / w, ICON_SIZE / h)
    const dw = Math.max(1, Math.round(w * scale))
    const dh = Math.max(1, Math.round(h * scale))
    ctx.imageSmoothingQuality = 'high'
    ctx.drawImage(img, Math.round((ICON_SIZE - dw) / 2), Math.round((ICON_SIZE - dh) / 2), dw, dh)
    const data = canvas.toDataURL('image/png')
    if (isCustomImageIcon(data)) return data
    throw new Error('The image could not be converted')
  } catch (err) {
    // Some SVGs taint the canvas; small originals can be stored as they are.
    if (file.size <= 40 * 1024) {
      const data = await readAsDataUrl(file)
      if (isCustomImageIcon(data)) return data
    }
    throw err instanceof Error ? err : new Error(errorMessage(err))
  } finally {
    URL.revokeObjectURL(url)
  }
}

export interface IconPickerProps {
  value: string | null | undefined
  onChange: (icon: string | null) => void
  /** Icon shown for "default" (protocol or folder icon). */
  fallback: IconType
  color?: string | null
  id?: string
  disabled?: boolean
}

/** Button + in-place popover (not portalled, so it scrolls inside modal dialogs). */
export function IconPicker({ value, onChange, fallback, color, id, disabled }: IconPickerProps) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const fileRef = useRef<HTMLInputElement>(null)

  const groups = useMemo(() => {
    const q = query.trim().toLowerCase()
    return ICON_GROUPS.map((g) => ({
      label: g.label,
      icons: Object.entries(g.icons).filter(([name]) => !q || name.toLowerCase().includes(q)),
    })).filter((g) => g.icons.length > 0)
  }, [query])

  const pick = (v: string | null) => {
    onChange(v)
    setOpen(false)
  }

  const onFile = async (file: File | undefined) => {
    if (!file) return
    setError(null)
    setBusy(true)
    try {
      const data = await imageFileToIcon(file)
      if (data.length > MAX_ICON_CHARS) throw new Error('The image is too large')
      pick(data)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
      if (fileRef.current) fileRef.current.value = ''
    }
  }

  const custom = isCustomImageIcon(value)
  const selectedName = value?.startsWith(LUCIDE_PREFIX) ? value.slice(LUCIDE_PREFIX.length) : null

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={setOpen}>
      <div className="flex items-center gap-2">
        <PopoverPrimitive.Trigger asChild>
          <Button id={id} variant="secondary" size="sm" disabled={disabled} aria-label="Choose icon" className="gap-2">
            <EntityIcon icon={value} fallback={fallback} color={color} />
            {custom ? 'Custom image' : selectedName ?? 'Default'}
          </Button>
        </PopoverPrimitive.Trigger>
        {value && (
          <Button variant="ghost" size="sm" disabled={disabled} onClick={() => onChange(null)}>
            <RotateCcw /> Default
          </Button>
        )}
      </div>
      <PopoverPrimitive.Content
        align="start"
        sideOffset={4}
        className="z-50 grid w-[min(22rem,calc(100vw-2rem))] gap-2 rounded-md border bg-popover p-2 text-popover-foreground shadow-popover outline-none"
        onOpenAutoFocus={(e) => e.preventDefault()}
      >
        <Input
          inputSize="sm"
          leading={<Search />}
          placeholder="Search icons"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="Search icons"
        />
        <div className="max-h-64 overflow-y-auto overscroll-contain pr-1" role="listbox" aria-label="Icons">
          <button
            type="button"
            role="option"
            aria-selected={!value}
            onClick={() => pick(null)}
            className={cn('mb-1 flex h-8 w-full items-center gap-2 rounded-sm px-2 text-sm hover:bg-accent', !value && 'bg-accent')}
          >
            <EntityIcon icon={null} fallback={fallback} color={color} /> Default icon
          </button>
          {groups.map((g) => (
            <div key={g.label} className="grid gap-1 pb-2">
              <div className="px-1 text-2xs font-medium tracking-wide text-muted-foreground uppercase">{g.label}</div>
              <div className="grid grid-cols-8 gap-0.5">
                {g.icons.map(([name, Icon]) => (
                  <button
                    key={name}
                    type="button"
                    role="option"
                    aria-selected={selectedName === name}
                    aria-label={name}
                    title={name}
                    onClick={() => pick(`${LUCIDE_PREFIX}${name}`)}
                    className={cn(
                      'flex size-8 items-center justify-center rounded-sm text-foreground/80 outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
                      selectedName === name && 'bg-primary/15 text-primary',
                    )}
                  >
                    <Icon className="size-4" style={color ? { color } : undefined} />
                  </button>
                ))}
              </div>
            </div>
          ))}
          {groups.length === 0 && <p className="px-2 py-3 text-center text-sm text-muted-foreground">No icon matches “{query}”.</p>}
        </div>
        <div className="flex items-center justify-between gap-2 border-t pt-2">
          <span className="text-xs text-muted-foreground">PNG, SVG, ICO… stored as 64×64</span>
          <Button variant="secondary" size="xs" loading={busy} onClick={() => fileRef.current?.click()}>
            <ImagePlus /> Upload image
          </Button>
          <input ref={fileRef} type="file" accept={ACCEPT} className="hidden" onChange={(e) => void onFile(e.target.files?.[0])} />
        </div>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
      </PopoverPrimitive.Content>
    </PopoverPrimitive.Root>
  )
}
