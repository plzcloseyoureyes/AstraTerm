import { useEffect } from 'react'
import './builtins'
import { installKeybindings } from '@/app/keybindings'
import { DialogHost } from '@/components/ui/dialog-host'
import { useIsMobile } from '@/lib/hooks'
import { cn } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { useUIStore } from '@/stores/ui'
import { getPopoutWindows, onWorkspaceWindow, useActiveTab } from '@/stores/workspace'
import { AboutDialog } from './AboutDialog'
import { CommandPalette } from './CommandPalette'
import { LockScreen, useAutoLock } from './LockScreen'
import { MenuBar } from './MenuBar'
import { OverlayHost } from './OverlayHost'
import { PromptHost } from './PromptHost'
import { Ribbon } from './Ribbon'
import { Sidebar } from './Sidebar'
import { StatusBar } from './StatusBar'
import { VaultUnlockDialog } from './VaultUnlockDialog'
import { Workspace } from './workspace/Workspace'

/**
 * The shell is a fixed-size application: its document must never scroll. body has overflow:hidden, so the user cannot
 * scroll back if a programmatic scroll (focus()/scrollIntoView() on an element briefly outside the viewport) ever
 * shifts the whole UI — snap it back. Layout keeps the document from overflowing in the first place (index.css).
 */
function pinDocumentScroll(win: Window): () => void {
  const onScroll = () => {
    if (win.scrollX !== 0 || win.scrollY !== 0) win.scrollTo(0, 0)
  }
  win.addEventListener('scroll', onScroll, { passive: true })
  onScroll()
  return () => win.removeEventListener('scroll', onScroll)
}

/**
 * Application shell: menu bar, ribbon, sidebar, dock workspace, status bar, plus global overlays (palette, prompts,
 * vault unlock, feature overlays from registerOverlay, lock screen) and the keyboard shortcut dispatcher.
 */
export function AppShell() {
  const a = appearanceSettings.use()
  const locked = useUIStore((s) => s.locked)
  const mobile = useIsMobile()
  useAutoLock()

  useEffect(() => {
    const offMain = installKeybindings(window)
    const offScroll = pinDocumentScroll(window)
    const offWin = onWorkspaceWindow((w) => {
      installKeybindings(w)
      pinDocumentScroll(w)
    })
    return () => {
      offMain()
      offScroll()
      offWin()
    }
  }, [])

  // Browser tab title follows the active dock tab.
  const activeTitle = useActiveTab()?.title
  useEffect(() => {
    document.title = activeTitle ? `${activeTitle} — Termstead` : 'Termstead'
  }, [activeTitle])
  useEffect(() => () => void (document.title = 'Termstead'), [])

  // Pop-out windows are outside this document: hide their content while the screen is locked (SEC-5).
  useEffect(() => {
    const wins = getPopoutWindows()
    for (const w of wins) {
      try {
        const body = w.document.body
        body.toggleAttribute('inert', locked)
        body.style.filter = locked ? 'blur(18px)' : ''
      } catch {
        /* window closed */
      }
    }
  }, [locked])

  return (
    <>
      <div
        className={cn('flex h-full w-full flex-col bg-background transition-[filter] duration-200', locked && 'pointer-events-none blur-xl select-none')}
        inert={locked}
        aria-hidden={locked || undefined}
      >
        {(a.showMenuBar || mobile) && <MenuBar />}
        {a.showRibbon && <Ribbon />}
        <div className="flex min-h-0 flex-1">
          {(a.showSidebar || mobile) && <Sidebar />}
          <main className="relative min-w-0 flex-1 overflow-clip" aria-label="Workspace">
            <Workspace />
          </main>
        </div>
        {a.showStatusBar && <StatusBar />}
      </div>
      {!locked && (
        <>
          <CommandPalette />
          <PromptHost />
          <VaultUnlockDialog />
          <DialogHost />
          <AboutDialog />
        </>
      )}
      <OverlayHost />
      <LockScreen />
    </>
  )
}
