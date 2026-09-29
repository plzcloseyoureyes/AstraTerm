import { useEffect, useState } from 'react'

/**
 * Restores the focus to the element that had it when the component first rendered (drawers, panels) on unmount.
 * The element is captured during the first render — before the component moves the focus into itself — and restored
 * synchronously, so a StrictMode re-mount (cleanup + effect again) ends with the focus inside the component.
 */
export function useRestoreFocus(): void {
  const [prev] = useState(() => (document.activeElement instanceof HTMLElement ? document.activeElement : null))
  useEffect(
    () => () => {
      if (prev && prev.isConnected && prev !== document.body) prev.focus({ preventScroll: true })
    },
    [prev],
  )
}
