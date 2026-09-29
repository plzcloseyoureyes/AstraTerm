import { useId } from 'react'

/** Path data of the AstraTerm mark on a 64-unit grid (packaging/icons/astraterm.svg is the 1024-unit master). */
export const MARK = {
  chevron: 'M14.6 18.1L26.6 30.1L14.6 42.1',
  star: 'M41.6 27.6Q43.6 36.1 52.1 38.1Q43.6 40.1 41.6 48.6Q39.6 40.1 31.1 38.1Q39.6 36.1 41.6 27.6Z',
}

/** The AstraTerm logo: the terminal prompt with a star for its cursor, on the brand gradient. */
export function BrandMark({ className }: { className?: string }) {
  const id = useId()
  return (
    <svg viewBox="0 0 64 64" className={className} aria-hidden>
      <defs>
        <linearGradient id={id} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#7c6cff" />
          <stop offset="1" stopColor="#2459e0" />
        </linearGradient>
      </defs>
      <rect width="64" height="64" rx="14" fill={`url(#${CSS.escape(id)})`} />
      <path d={MARK.chevron} fill="none" stroke="#fff" strokeWidth="5.5" strokeLinecap="round" strokeLinejoin="round" />
      <path d={MARK.star} fill="#fff" />
    </svg>
  )
}
