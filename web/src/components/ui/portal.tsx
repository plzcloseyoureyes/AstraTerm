/*
 * Portal target for overlays (menus, popovers, selects, tooltips, dialogs) rendered from inside a dock panel. A panel
 * moved into a dockview pop-out window lives in another document: its overlays must be portalled into THAT document's
 * body, or they open in the main window. The workspace wraps every panel in <PortalScope>; the components in
 * components/ui read the container with usePortalContainer(). Outside a scope (or in the main window) it is undefined,
 * i.e. Radix's default document.body.
 */
import { createContext, useContext, useLayoutEffect, useState, type ReactNode } from 'react'

const PortalContainerContext = createContext<HTMLElement | undefined>(undefined)

/** The body of the document the current panel lives in (undefined = the main window). */
export function usePortalContainer(): HTMLElement | undefined {
  return useContext(PortalContainerContext)
}

/**
 * Tracks the document an element lives in (pass it through a callback ref / state). Re-checked on mount and on the first interaction after a move (pointer,
 * focus, keys), so the container is right before any overlay opens.
 */
export function useOwnerBody(el: HTMLElement | null): HTMLElement | undefined {
  const [body, setBody] = useState<HTMLElement | undefined>(undefined)
  useLayoutEffect(() => {
    if (!el) return
    const sync = () => {
      const b = el.ownerDocument.body
      const next = b === document.body ? undefined : b
      setBody((cur) => (cur === next ? cur : next))
    }
    sync()
    const types = ['pointerdown', 'pointerover', 'focusin', 'keydown', 'contextmenu'] as const
    for (const t of types) el.addEventListener(t, sync, true)
    return () => {
      for (const t of types) el.removeEventListener(t, sync, true)
    }
  }, [el])
  return body
}

/** Provides the portal container of `container` (see useOwnerBody) to everything below. */
export function PortalScope({ container, children }: { container: HTMLElement | undefined; children: ReactNode }) {
  return <PortalContainerContext.Provider value={container}>{children}</PortalContainerContext.Provider>
}
