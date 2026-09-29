import { useEffect } from 'react'

/** Auto-grow a textarea up to maxPx. */
export function useAutoGrow(ref: React.RefObject<HTMLTextAreaElement | null>, value: string, maxPx = 200): void {
  useEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(maxPx, el.scrollHeight)}px`
    el.style.overflowY = el.scrollHeight > maxPx ? 'auto' : 'hidden'
  }, [ref, value, maxPx])
}
