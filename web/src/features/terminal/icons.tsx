/*
 * Environment colouring (TERM-10): protocol icons tinted with the connection colour for the tab strip.
 */
import type { IconType } from '@/app/registry'

const cache = new WeakMap<object, Map<string, IconType>>()
const SAFE_COLOR = /^(#[0-9a-f]{3,8}|rgba?\([\d\s.,%]+\)|hsla?\([\d\s.,%deg]+\)|oklch\([\d\s.,%/]+\)|[a-z]{3,20})$/i

/** A stable component rendering `base` in `color` (cached per icon + colour, so tabs do not remount icons). */
export function tintedIcon(base: IconType, color: string): IconType | undefined {
  const c = color.trim()
  if (!SAFE_COLOR.test(c)) return undefined
  let byColor = cache.get(base as object)
  if (!byColor) {
    byColor = new Map()
    cache.set(base as object, byColor)
  }
  let icon = byColor.get(c)
  if (!icon) {
    const Base = base
    const Tinted = (props: { className?: string; size?: number | string; strokeWidth?: number | string }) => (
      <span className="inline-flex" style={{ color: c }}>
        <Base {...props} />
      </span>
    )
    Tinted.displayName = 'TintedIcon'
    icon = Tinted
    if (byColor.size > 64) byColor.clear()
    byColor.set(c, icon)
  }
  return icon
}
